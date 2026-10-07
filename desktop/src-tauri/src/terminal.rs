use crate::remote::{response_problem, valid_session_id, Error, Remote, Result};
use futures_util::{SinkExt, StreamExt};
use serde::Serialize;
use std::{sync::Arc, time::Duration};
use tauri::ipc::Channel;
use tokio::{
    sync::mpsc,
    task::JoinHandle,
    time::{timeout, Instant},
};
use tokio_tungstenite::tungstenite::{
    client::IntoClientRequest, protocol::WebSocketConfig, Message,
};

pub enum Command {
    Input(Vec<u8>),
    Resize(u16, u16),
    Ack(u64),
}

#[derive(Clone, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Event {
    Data { sequence: u64, bytes: Vec<u8> },
    Closed { code: u16, message: String },
}

pub struct Terminal {
    pub tx: mpsc::Sender<Command>,
    task: JoinHandle<()>,
}

impl Drop for Terminal {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Terminal {
    pub async fn open(
        remote: Arc<Remote>,
        session: &str,
        terminal: Option<&str>,
        events: Channel<Event>,
    ) -> Result<Self> {
        valid_session_id(session)?;
        let path = if let Some(terminal) = terminal {
            valid_session_id(terminal)?;
            format!("api/sessions/{session}/client-terminals/{terminal}/stream")
        } else {
            format!("api/sessions/{session}/term")
        };
        let mut url = remote.endpoint(&path);
        let scheme = if url.scheme() == "https" { "wss" } else { "ws" };
        url.set_scheme(scheme)
            .map_err(|_| Error::new("address", "无效的终端地址"))?;
        let mut request = url.as_str().into_client_request().map_err(Error::network)?;
        request.headers_mut().insert(
            "Authorization",
            remote
                .authorization()
                .parse()
                .map_err(|_| Error::new("protocol", "登录令牌无效"))?,
        );
        // Native client sends no Origin. The existing server's browser origin check is unchanged.
        let config = WebSocketConfig::default()
            .max_message_size(Some(64 * 1024))
            .max_frame_size(Some(64 * 1024));
        let (mut socket, _) = timeout(
            Duration::from_secs(70),
            tokio_tungstenite::connect_async_with_config(request, Some(config), true),
        )
        .await
        .map_err(Error::network)?
        .map_err(|err| {
            if let tokio_tungstenite::tungstenite::Error::Http(response) = &err {
                return response_problem(
                    response.status(),
                    response
                        .headers()
                        .get("content-type")
                        .and_then(|v| v.to_str().ok())
                        .unwrap_or(""),
                    response.body().as_deref().unwrap_or(&[]),
                    response
                        .headers()
                        .get("x-agentbox-operation-id")
                        .and_then(|v| v.to_str().ok()),
                );
            }
            Error::network(err)
        })?;
        let (tx, mut rx) = mpsc::channel::<Command>(32);
        let task = tokio::spawn(async move {
            let mut sequence = 0u64;
            let mut pending = None;
            let mut last_receive = Instant::now();
            let mut tick = tokio::time::interval(Duration::from_secs(1));
            let (code, message) = loop {
                let send = tokio::select! {
                    command = rx.recv() => match command {
                        Some(Command::Input(bytes)) => Some(Message::Binary(bytes.into())),
                        Some(Command::Resize(cols, rows)) => Some(Message::Text(serde_json::json!({"type":"resize","cols":cols,"rows":rows}).to_string().into())),
                        Some(Command::Ack(ack)) => { if pending.is_some_and(|(seq, _)| seq == ack) { pending = None; } None },
                        None => break (1000, "连接已关闭".to_string()),
                    },
                    incoming = socket.next(), if pending.is_none() => {
                        last_receive = Instant::now();
                        match incoming {
                            Some(Ok(Message::Binary(bytes))) => {
                                sequence += 1;
                                if events.send(Event::Data { sequence, bytes: bytes.to_vec() }).is_err() { break (1000, "窗口已关闭".into()); }
                                pending = Some((sequence, Instant::now()));
                                None
                            },
                            Some(Ok(Message::Ping(bytes))) => Some(Message::Pong(bytes)),
                            Some(Ok(Message::Close(frame))) => {
                                let code = frame.as_ref().map(|f| u16::from(f.code)).unwrap_or(1000);
                                let message = frame.map(|f| f.reason.to_string()).unwrap_or_else(|| "终端已关闭".into());
                                break (code, message);
                            },
                            Some(Ok(_)) => None,
                            _ => break (1006, "连接中断".into()),
                        }
                    },
                    _ = tick.tick() => {
                        if pending.is_some_and(|(_, at): (u64, Instant)| at.elapsed() > Duration::from_secs(15)) {
                            break (4008, "终端显示未响应，请重新连接".into());
                        }
                        if last_receive.elapsed() > Duration::from_secs(75) { break (1006, "连接超时".into()); }
                        None
                    },
                };
                if let Some(message) = send {
                    if !matches!(
                        timeout(Duration::from_secs(5), socket.send(message)).await,
                        Ok(Ok(()))
                    ) {
                        break (1006, "终端发送失败".into());
                    }
                }
            };
            let _ = events.send(Event::Closed { code, message });
        });
        Ok(Self { tx, task })
    }

    pub fn send(&self, command: Command) -> Result<()> {
        self.tx
            .try_send(command)
            .map_err(|_| Error::new("backpressure", "终端输入繁忙或已断开，请稍后重试"))
    }
}
