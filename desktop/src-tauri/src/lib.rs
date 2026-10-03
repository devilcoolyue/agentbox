mod attachments;
pub mod diagnostics;
mod file_upload;
mod files;
mod projects;
mod remote;
mod sidecar;
#[cfg(feature = "desktop-smoke")]
mod smoke;
#[cfg(feature = "desktop-smoke")]
mod smoke_sync;
mod sync_commands;
mod sync_progress;
mod terminal;
mod ui_preferences;
mod updates;
#[cfg(test)]
mod wire_tests;

use remote::{Connection, Error, Remote, Result, Session};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::sync::{
    atomic::{AtomicU64, Ordering},
    Arc,
};
use tauri::{ipc::Channel, Manager, State};
use terminal::{Command, Event, Terminal};
use tokio::sync::{oneshot, Mutex};

#[derive(Default)]
struct StateData {
    remote: Option<Arc<Remote>>,
    connection: Option<Connection>,
    terminals: HashMap<u64, Terminal>,
    pending: HashMap<u64, oneshot::Sender<()>>,
    sync_cancel: Option<oneshot::Sender<()>>,
    sync_progress: Option<Arc<sync_progress::Relay>>,
    disconnecting: bool,
    attachments: attachments::Grants,
    file_upload: Option<file_upload::PendingUpload>,
    attachment_cancel: Option<(String, oneshot::Sender<()>)>,
}

#[derive(Default)]
struct Desktop {
    inner: Mutex<StateData>,
    next_terminal: AtomicU64,
    next_sync: AtomicU64,
    sidecar: Mutex<Option<sidecar::Sidecar>>,
    inspection: Mutex<InspectionState>,
    sync_work: Mutex<()>,
    attachment_work: Mutex<()>,
    attachment_selection: Mutex<()>,
}

#[derive(Default)]
struct InspectionState {
    running: bool,
    cancel: Option<oneshot::Sender<()>>,
}

#[tauri::command]
async fn inspect_local(state: State<'_, Desktop>) -> Result<serde_json::Value> {
    let (cancel, canceled) = oneshot::channel();
    {
        let mut active = state.inspection.lock().await;
        if active.running {
            return Err(Error::new("inspection_busy", "已有目录检查正在进行"));
        }
        active.running = true;
        active.cancel = Some(cancel);
    }
    let task = async {
        let selected = rfd::AsyncFileDialog::new()
            .set_title("选择要检查的本地项目目录")
            .pick_folder()
            .await;
        let Some(selected) = selected else {
            return Ok(serde_json::Value::Null);
        };
        let exe = std::env::current_exe().map_err(Error::network)?;
        let name = if cfg!(windows) {
            "abox-sync.exe"
        } else {
            "abox-sync"
        };
        let parent = exe
            .parent()
            .ok_or_else(|| Error::new("sidecar_path", "无法定位应用目录"))?;
        let mut worker = sidecar::Sidecar::start(&parent.join(name)).await?;
        let result = tokio::select! {
            result=worker.inspect(selected.path())=>result,
            _=canceled=>Err(Error::new("inspection_canceled","检查已取消")),
        };
        worker.stop().await;
        result
    }
    .await;
    *state.inspection.lock().await = InspectionState::default();
    task
}

#[tauri::command]
async fn cancel_inspection(state: State<'_, Desktop>) -> Result<()> {
    if let Some(cancel) = state.inspection.lock().await.cancel.take() {
        let _ = cancel.send(());
    }
    Ok(())
}

#[tauri::command]
async fn backend_status(state: State<'_, Desktop>) -> Result<String> {
    let mut background = state.sidecar.lock().await;
    if background.is_none() {
        let exe =
            std::env::current_exe().map_err(|_| Error::new("sidecar_path", "无法定位后台程序"))?;
        let directory = exe
            .parent()
            .ok_or_else(|| Error::new("sidecar_path", "无法定位应用目录"))?;
        let name = if cfg!(windows) {
            "abox-sync.exe"
        } else {
            "abox-sync"
        };
        *background = Some(sidecar::Sidecar::start(&directory.join(name)).await?);
    }
    match background.as_mut().expect("started above").ping().await {
        Ok(()) => Ok("后台已就绪；同步功能尚未启用".into()),
        Err(error) => {
            *background = None;
            Err(error)
        }
    }
}

fn credential(server: &str, user: &str) -> Result<keyring::Entry> {
    let identity = serde_json::to_vec(&(server, user)).expect("string pair");
    let name = format!("{:x}", Sha256::digest(identity));
    keyring::Entry::new("io.github.devilcoolyue.agentbox.desktop", &name)
        .map_err(|_| Error::new("credential", "无法访问系统凭证库；不会回退为明文保存"))
}

#[tauri::command]
async fn connect(
    state: State<'_, Desktop>,
    server: String,
    username: String,
    password: Option<String>,
    allow_http: bool,
    remember: bool,
) -> Result<Connection> {
    let mut data = state.inner.lock().await;
    if data.remote.is_some() {
        return Err(Error::new("state", "请先退出当前连接"));
    }
    let base = remote::normalize_server(&server, allow_http)?;
    let username = username.trim();
    if username.is_empty() || username.len() > 256 {
        return Err(Error::new("request", "请输入用户名"));
    }
    let saved = credential(base.as_str(), username)?;
    let fresh = password.is_some();
    let remote = if let Some(password) = password {
        Remote::login(base, username, &password).await?
    } else {
        let token = saved
            .get_password()
            .map_err(|_| Error::new("credential", "无法读取已保存登录，请输入密码重新登录"))?;
        Remote::new(base, token)?
    };
    install_connection(&mut data, remote, Some(username), fresh && remember, fresh).await
}

async fn install_connection(
    data: &mut StateData,
    remote: Remote,
    expected_user: Option<&str>,
    save: bool,
    fresh: bool,
) -> Result<Connection> {
    let verified = async {
        let me = remote.identity().await?;
        if me.user.is_empty() || expected_user.is_some_and(|user| me.user != user) {
            return Err(Error::new("identity", "服务器返回的用户身份不匹配"));
        }
        let capabilities = remote.capabilities().await?;
        Ok(Connection {
            server: remote.base.to_string(),
            user: me.user,
            role: me.role,
            capabilities,
        })
    }
    .await;
    let connection = match verified {
        Ok(connection) => connection,
        Err(err) => {
            if fresh {
                let _ = remote.logout().await;
            }
            return Err(err);
        }
    };
    if save
        && credential(&connection.server, &connection.user)
            .and_then(|entry| {
                entry
                    .set_password(remote.token())
                    .map_err(|_| Error::new("credential", "凭证保存失败"))
            })
            .is_err()
    {
        let _ = remote.logout().await;
        return Err(Error::new(
            "credential",
            "保存系统凭证失败，已取消本次登录；可关闭记住登录后重试",
        ));
    }
    data.disconnecting = false;
    data.remote = Some(Arc::new(remote));
    data.connection = Some(connection.clone());
    Ok(connection)
}

#[tauri::command]
async fn connect_pair(
    state: State<'_, Desktop>,
    server: String,
    code: String,
    allow_http: bool,
    remember: bool,
) -> Result<Connection> {
    let mut data = state.inner.lock().await;
    if data.remote.is_some() {
        return Err(Error::new("state", "请先退出当前连接"));
    }
    let remote =
        Remote::redeem(remote::normalize_server(&server, allow_http)?, code.trim()).await?;
    install_connection(&mut data, remote, None, remember, true).await
}

#[tauri::command]
async fn issue_pair(state: State<'_, Desktop>) -> Result<serde_json::Value> {
    let data = state.inner.lock().await;
    if data
        .connection
        .as_ref()
        .and_then(|c| c.capabilities.as_ref())
        .is_none_or(|c| c.features.pairing != 1)
    {
        return Err(Error::new("unsupported", "此服务端尚不支持客户端配对"));
    }
    let remote = data
        .remote
        .clone()
        .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
    drop(data);
    remote
        .api(reqwest::Method::POST, "api/clients/pair", None)
        .await
}

#[tauri::command]
async fn list_sessions(state: State<'_, Desktop>) -> Result<Vec<Session>> {
    let remote = state
        .inner
        .lock()
        .await
        .remote
        .clone()
        .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
    remote.sessions().await
}

#[tauri::command]
async fn disconnect(state: State<'_, Desktop>) -> Result<()> {
    {
        let mut data = state.inner.lock().await;
        data.disconnecting = true;
        data.attachments.clear();
        data.file_upload = None;
        if let Some((_, cancel)) = data.attachment_cancel.take() {
            let _ = cancel.send(());
        }
        if let Some(progress) = data.sync_progress.take() {
            progress.close();
        }
        if let Some(cancel) = data.sync_cancel.take() {
            let _ = cancel.send(());
        }
    }
    // Wait for the canceled sidecar to exit before revoking credentials; hold
    // this mutex through logout so another sync command cannot start meanwhile.
    let _sync_guard = state.sync_work.lock().await;
    let _attachment_guard = state.attachment_work.lock().await;
    let mut data = state.inner.lock().await;
    data.terminals.clear();
    data.pending.clear();
    if let Some(remote) = data.remote.as_ref() {
        // Failed revocation remains retryable; don't claim that an offline logout revoked the token.
        remote.logout().await?;
    }
    if let Some(connection) = data.connection.as_ref() {
        let entry = credential(&connection.server, &connection.user)?;
        match entry.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => (),
            Err(_) => {
                return Err(Error::new(
                    "credential",
                    "令牌已撤销，但系统凭证删除失败，请重试退出",
                ))
            }
        }
    }
    data.remote = None;
    data.connection = None;
    data.disconnecting = false;
    Ok(())
}

#[tauri::command]
async fn terminal_open(
    state: State<'_, Desktop>,
    session: String,
    terminal: Option<String>,
    events: Channel<Event>,
) -> Result<u64> {
    let mut data = state.inner.lock().await;
    if data.disconnecting {
        return Err(Error::new("state", "正在断开连接或安装更新"));
    }
    let remote = data
        .remote
        .clone()
        .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
    if terminal.is_some()
        && data
            .connection
            .as_ref()
            .and_then(|c| c.capabilities.as_ref())
            .is_none_or(|c| c.features.project_terminals != 1)
    {
        return Err(Error::new("unsupported", "服务端不支持项目独立终端"));
    }
    if data.terminals.len() + data.pending.len() >= 32 {
        return Err(Error::new("limit", "同时打开的终端过多，请先关闭标签"));
    }
    let id = state.next_terminal.fetch_add(1, Ordering::Relaxed) + 1;
    let (cancel, canceled) = oneshot::channel();
    data.pending.insert(id, cancel);
    drop(data);
    let opened = tokio::select! {
        opened=Terminal::open(remote.clone(), &session, terminal.as_deref(), events)=>opened,
        _=canceled=>return Err(Error::new("state","终端连接已取消")),
    };
    let mut data = state.inner.lock().await;
    if data.pending.remove(&id).is_none()
        || !data
            .remote
            .as_ref()
            .is_some_and(|current| Arc::ptr_eq(current, &remote))
    {
        return Err(Error::new("state", "登录已更改，终端连接已取消"));
    }
    data.terminals.insert(id, opened?);
    Ok(id)
}

#[tauri::command]
async fn terminal_close(state: State<'_, Desktop>, id: u64) -> Result<()> {
    let mut data = state.inner.lock().await;
    data.terminals.remove(&id);
    data.pending.remove(&id);
    Ok(())
}

fn terminal_for(data: &StateData, id: u64) -> Result<&Terminal> {
    data.terminals
        .get(&id)
        .ok_or_else(|| Error::new("state", "终端已关闭"))
}

#[tauri::command]
async fn terminal_input(state: State<'_, Desktop>, id: u64, bytes: Vec<u8>) -> Result<()> {
    if bytes.len() > 16 * 1024 {
        return Err(Error::new("limit", "单次终端输入超过限制"));
    }
    terminal_for(&*state.inner.lock().await, id)?.send(Command::Input(bytes))
}

#[tauri::command]
async fn terminal_resize(state: State<'_, Desktop>, id: u64, cols: u16, rows: u16) -> Result<()> {
    if cols == 0 || rows == 0 || cols > 1000 || rows > 1000 {
        return Err(Error::new("limit", "终端尺寸无效"));
    }
    terminal_for(&*state.inner.lock().await, id)?.send(Command::Resize(cols, rows))
}

#[tauri::command]
async fn terminal_ack(state: State<'_, Desktop>, id: u64, sequence: u64) -> Result<()> {
    terminal_for(&*state.inner.lock().await, id)?.send(Command::Ack(sequence))
}

#[derive(serde::Deserialize)]
struct AttachmentRequest {
    task: String,
    session: String,
    filename: String,
    mime: String,
    bytes: Option<Vec<u8>>,
    ticket: Option<String>,
}

#[tauri::command]
async fn upload_attachment(
    state: State<'_, Desktop>,
    request: AttachmentRequest,
    events: Channel<files::DownloadProgress>,
) -> Result<remote::UploadResult> {
    let AttachmentRequest {
        task,
        session,
        filename,
        mime,
        bytes,
        ticket,
    } = request;
    if task.is_empty() || task.len() > 128 {
        return Err(Error::new("request", "上传任务无效"));
    }
    let _guard = state
        .attachment_work
        .try_lock()
        .map_err(|_| Error::new("busy", "已有附件正在传输"))?;
    let (cancel, canceled) = oneshot::channel();
    let remote = {
        let mut data = state.inner.lock().await;
        if data.disconnecting {
            return Err(Error::new("state", "正在退出登录"));
        }
        let remote = data
            .remote
            .clone()
            .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
        data.attachment_cancel = Some((task, cancel));
        remote
    };
    let work = async {
        let (filename, bytes) = if let Some(ticket) = ticket {
            let grant = state
                .inner
                .lock()
                .await
                .attachments
                .take(&ticket, &remote)?;
            tauri::async_runtime::spawn_blocking(move || grant.read())
                .await
                .map_err(Error::network)??
        } else {
            (
                filename,
                bytes.ok_or_else(|| Error::new("request", "附件内容缺失"))?,
            )
        };
        remote
            .upload_attachment_progress(&session, &filename, &mime, bytes, move |bytes, total| {
                let _ = events.send(files::DownloadProgress {
                    bytes,
                    total: Some(total),
                });
            })
            .await
    };
    let result = tokio::select! {
        result=work=>result,
        _=canceled=>Err(Error::new("canceled", "上传已取消；服务器可能已收到文件，请先检查再重试")),
    };
    state.inner.lock().await.attachment_cancel = None;
    result
}

#[tauri::command]
async fn cancel_attachment(state: State<'_, Desktop>, task: String) -> Result<()> {
    let mut data = state.inner.lock().await;
    if data
        .attachment_cancel
        .as_ref()
        .is_some_and(|(active, _)| active == &task)
    {
        if let Some((_, cancel)) = data.attachment_cancel.take() {
            let _ = cancel.send(());
        }
    }
    Ok(())
}

pub fn run() {
    let builder = tauri::Builder::default()
        .plugin(tauri_plugin_updater::Builder::new().build())
        .manage(ui_preferences::ZoomState::default())
        .manage(updates::UpdateState::default());
    #[cfg(feature = "desktop-smoke")]
    let builder = builder.plugin(smoke::plugin());
    let builder = builder
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::DragDrop(tauri::DragDropEvent::Drop {
                paths,
                position,
                ..
            }) = event
            {
                attachments::dropped(window, paths.clone(), position.x, position.y);
            }
        })
        .plugin(tauri_plugin_single_instance::init(|app, _, _| {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.set_focus();
            }
        }))
        .manage(Desktop::default())
        .invoke_handler(tauri::generate_handler![
            ui_preferences::desktop_set_zoom,
            updates::desktop_update_check,
            updates::desktop_update_install,
            file_upload::preview_file_upload,
            file_upload::apply_file_upload,
            file_upload::discard_file_upload,
            files::list_files,
            files::download_file,
            projects::list_projects,
            projects::save_project,
            projects::delete_project,
            projects::list_terminals,
            projects::create_terminal,
            projects::end_terminal,
            inspect_local,
            cancel_inspection,
            sync_commands::sync_bind,
            sync_commands::sync_archive,
            sync_commands::sync_abandon_review,
            sync_commands::sync_abandon,
            sync_commands::sync_preview,
            sync_commands::sync_apply,
            sync_commands::sync_list,
            sync_commands::sync_cancel,
            sync_commands::sync_progress_ack,
            sync_commands::sync_review,
            sync_commands::sync_resolve,
            sync_commands::sync_history,
            sync_commands::sync_history_cleanup,
            sync_commands::sync_recovery_discard,
            sync_commands::sync_remote_cleanup_review,
            sync_commands::sync_remote_cleanup,
            sync_commands::sync_orphan_list,
            sync_commands::sync_orphan_review,
            sync_commands::sync_orphan_export,
            sync_commands::sync_orphan_retire,
            sync_commands::sync_export,
            backend_status,
            connect,
            connect_pair,
            issue_pair,
            disconnect,
            list_sessions,
            terminal_open,
            terminal_close,
            terminal_input,
            terminal_resize,
            terminal_ack,
            attachments::choose_attachments,
            attachments::read_attachment_clipboard,
            attachments::release_attachments,
            upload_attachment,
            cancel_attachment
        ]);
    #[cfg(feature = "desktop-smoke")]
    let builder = builder.invoke_handler(tauri::generate_handler![
        ui_preferences::desktop_set_zoom,
        updates::desktop_update_check,
        updates::desktop_update_install,
        file_upload::preview_file_upload,
        file_upload::apply_file_upload,
        file_upload::discard_file_upload,
        files::list_files,
        files::download_file,
        projects::list_projects,
        projects::save_project,
        projects::delete_project,
        projects::list_terminals,
        projects::create_terminal,
        projects::end_terminal,
        inspect_local,
        cancel_inspection,
        sync_commands::sync_bind,
        sync_commands::sync_archive,
        sync_commands::sync_abandon_review,
        sync_commands::sync_abandon,
        sync_commands::sync_preview,
        sync_commands::sync_apply,
        sync_commands::sync_list,
        sync_commands::sync_cancel,
        sync_commands::sync_progress_ack,
        sync_commands::sync_review,
        sync_commands::sync_resolve,
        sync_commands::sync_history,
        sync_commands::sync_history_cleanup,
        sync_commands::sync_recovery_discard,
        sync_commands::sync_remote_cleanup_review,
        sync_commands::sync_remote_cleanup,
        sync_commands::sync_orphan_list,
        sync_commands::sync_orphan_review,
        sync_commands::sync_orphan_export,
        sync_commands::sync_orphan_retire,
        sync_commands::sync_export,
        backend_status,
        connect,
        connect_pair,
        issue_pair,
        disconnect,
        list_sessions,
        terminal_open,
        terminal_close,
        terminal_input,
        terminal_resize,
        terminal_ack,
        attachments::choose_attachments,
        attachments::read_attachment_clipboard,
        attachments::release_attachments,
        upload_attachment,
        cancel_attachment,
        smoke::smoke_config,
        smoke::smoke_stage,
        smoke::smoke_finish,
        smoke_sync::smoke_sync_action
    ]);
    builder
        .run(tauri::generate_context!())
        .expect("failed to run Agentbox desktop");
}
