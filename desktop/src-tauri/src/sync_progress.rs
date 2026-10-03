//! Bounded, observational progress. One unacknowledged WebView event and one
//! latest snapshot per task; a stalled renderer never blocks file operations.
use serde::{Deserialize, Serialize};
use std::sync::Mutex;
use tauri::ipc::Channel;

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Progress {
    pub sequence: u64,
    pub stage: String,
    pub path: String,
    pub operation: String,
    pub completed: u32,
    pub total: u32,
    pub bytes: u64,
    pub total_bytes: u64,
    pub file_bytes: u64,
    pub file_total: u64,
    pub scanned: u32,
    pub scanned_bytes: u64,
}
impl Progress {
    pub fn valid(&self) -> bool {
        let relative = self.path.is_empty()
            || (!self.path.starts_with('/')
                && !self.path.contains(['\\', '\0', '\r', '\n'])
                && self
                    .path
                    .split('/')
                    .all(|part| !part.is_empty() && part != "." && part != ".."));
        matches!(
            self.stage.as_str(),
            "authenticating"
                | "local_scan"
                | "remote_scan"
                | "planning"
                | "lease"
                | "applying"
                | "committing"
                | "reviewing"
                | "history"
                | "exporting"
        ) && matches!(
            self.operation.as_str(),
            "" | "upload"
                | "download"
                | "delete_local"
                | "delete_remote"
                | "mkdir_local"
                | "mkdir_remote"
                | "rmdir_local"
                | "rmdir_remote"
        ) && self.sequence > 0
            && self.sequence <= 9_007_199_254_740_991
            && self.path.len() <= 4096
            && relative
            && self.completed <= self.total
            && self.total <= 200_000
            && self.bytes <= self.total_bytes
            && self.total_bytes <= 4 * 1024 * 1024 * 1024
            && self.file_bytes <= self.file_total
            && self.file_total <= 64 * 1024 * 1024
            && self.scanned <= 100_000
            && self.scanned_bytes <= 2 * 1024 * 1024 * 1024
    }
}
#[derive(Clone, Serialize)]
pub struct ProgressEvent {
    pub task: String,
    #[serde(flatten)]
    pub progress: Progress,
}
#[derive(Default)]
struct Mailbox {
    pending: Option<u64>,
    latest: Option<Progress>,
    closed: bool,
}
pub struct Relay {
    pub id: String,
    mailbox: Mutex<Mailbox>,
    send: Box<dyn Fn(ProgressEvent) -> bool + Send + Sync>,
}
impl Relay {
    pub fn new(id: String, events: Channel<ProgressEvent>) -> Self {
        Self {
            id,
            mailbox: Mutex::new(Mailbox::default()),
            send: Box::new(move |event| events.send(event).is_ok()),
        }
    }
    fn flush(&self, state: &mut Mailbox) {
        if state.closed || state.pending.is_some() {
            return;
        }
        if let Some(progress) = state.latest.take() {
            state.pending = Some(progress.sequence);
            if !(self.send)(ProgressEvent {
                task: self.id.clone(),
                progress,
            }) {
                state.closed = true;
            }
        }
    }
    pub fn offer(&self, progress: Progress) {
        let mut state = self.mailbox.lock().unwrap();
        if !state.closed {
            state.latest = Some(progress);
            self.flush(&mut state);
        }
    }
    pub fn ack(&self, sequence: u64) {
        let mut state = self.mailbox.lock().unwrap();
        if state.pending == Some(sequence) {
            state.pending = None;
            self.flush(&mut state);
        }
    }
    pub fn close(&self) {
        let mut state = self.mailbox.lock().unwrap();
        state.closed = true;
        state.latest = None;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    fn value(sequence: u64) -> Progress {
        serde_json::from_value(serde_json::json!({"sequence":sequence,"stage":"applying","path":"中文/file","operation":"upload","completed":0,"total":1,"bytes":5,"total_bytes":10,"file_bytes":5,"file_total":10,"scanned":0,"scanned_bytes":0})).unwrap()
    }
    #[test]
    fn stalled_renderer_keeps_only_one_pending_and_latest() {
        let sent = Arc::new(Mutex::new(Vec::new()));
        let captured = sent.clone();
        let relay = Relay {
            id: "task".into(),
            mailbox: Mutex::new(Mailbox::default()),
            send: Box::new(move |event| {
                captured.lock().unwrap().push(event.progress.sequence);
                true
            }),
        };
        for i in 1..=100_000 {
            relay.offer(value(i));
        }
        assert_eq!(*sent.lock().unwrap(), vec![1]);
        relay.ack(2);
        assert_eq!(*sent.lock().unwrap(), vec![1]);
        relay.ack(1);
        assert_eq!(*sent.lock().unwrap(), vec![1, 100_000]);
        relay.offer(value(100_001));
        relay.close();
        relay.ack(100_000);
        assert_eq!(*sent.lock().unwrap(), vec![1, 100_000]);
        assert!(relay.mailbox.lock().unwrap().latest.is_none());
    }
    #[test]
    fn progress_cannot_forward_credentials_paths_or_invalid_totals() {
        let valid = value(1);
        assert!(valid.valid());
        for path in [
            "/private/project",
            "../secret",
            "C:\\secret",
            "a//b",
            "a\nsecret",
        ] {
            let mut p = valid.clone();
            p.path = path.into();
            assert!(!p.valid());
        }
        let mut p = valid.clone();
        p.completed = 2;
        assert!(!p.valid());
        let mut p = valid.clone();
        p.bytes = 11;
        assert!(!p.valid());
        let mut raw = serde_json::to_value(valid).unwrap();
        raw["token"] = serde_json::json!("never forward");
        assert!(serde_json::from_value::<Progress>(raw).is_err());
    }
}
