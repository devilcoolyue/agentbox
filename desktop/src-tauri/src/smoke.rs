//! Compiled only into explicit smoke builds. No production server or keyring writes.
use futures_util::{SinkExt, StreamExt};
use serde_json::json;
use std::collections::HashSet;
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Arc, Mutex,
};
use tauri::{
    plugin::{Builder, TauriPlugin},
    Manager, State, Wry,
};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio_tungstenite::tungstenite::{
    handshake::server::{Request, Response},
    Message,
};

pub(crate) struct Fixture {
    pub(crate) sync: Option<super::smoke_sync::SyncFixture>,
    compat: Option<CompatibilityFixture>,
    server: String,
    project_mode: bool,
    terminal_inputs: Arc<Mutex<HashSet<String>>>,
    report: std::path::PathBuf,
    attachment: Arc<AtomicBool>,
    directory_upload: Arc<AtomicBool>,
    input: Arc<AtomicBool>,
    resize: Arc<AtomicBool>,
}

// Only a test build can receive disposable credentials from the isolated
// compatibility runner. This never changes the normal login or picker path.
#[derive(serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct CompatibilityFixture {
    server: String,
    username: String,
    password: String,
    session: String,
}
impl CompatibilityFixture {
    fn from_environment() -> Result<Option<Self>, Box<dyn std::error::Error>> {
        let Some(raw) = std::env::var_os("AGENTBOX_SMOKE_COMPAT") else {
            return Ok(None);
        };
        let config: Self = serde_json::from_str(&raw.to_string_lossy())?;
        let url = url::Url::parse(&config.server)?;
        if url.scheme() != "http"
            || url.host_str() != Some("127.0.0.1")
            || url.port().is_none()
            || url.path() != "/"
            || !url.username().is_empty()
            || url.password().is_some()
            || url.query().is_some()
            || url.fragment().is_some()
            || config.username != "boxadmin"
            || config.password.is_empty()
            || config.password.len() > 256
            || crate::remote::valid_session_id(&config.session).is_err()
        {
            return Err("compatibility smoke requires an isolated loopback fixture".into());
        }
        Ok(Some(config))
    }
}

#[tauri::command]
pub(crate) async fn smoke_config(
    app: tauri::AppHandle,
    fixture: State<'_, Fixture>,
) -> std::result::Result<serde_json::Value, String> {
    let diagnostics = activate_smoke_window(&app).await?;
    use std::io::Write;
    let mut log = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(fixture.report.with_extension("window.jsonl"))
        .map_err(|_| "cannot write window diagnostics")?;
    writeln!(log, "{diagnostics}").map_err(|_| "cannot write window diagnostics")?;
    Ok(
        json!({"server":fixture.server,"projects":fixture.project_mode,"sync":fixture.sync.is_some(),
        "compat":fixture.compat.is_some(),"username":fixture.compat.as_ref().map(|f| f.username.as_str()),
        "password":fixture.compat.as_ref().map(|f| f.password.as_str()),"window":diagnostics}),
    )
}

// This changes only the explicit test application's own window. It neither
// changes WebKit visibility reporting nor touches another running application.
// Awaiting the main-thread callback establishes that activation was attempted,
// while the renderer must still observe real visibility and animation frames.
async fn activate_smoke_window(app: &tauri::AppHandle) -> Result<serde_json::Value, String> {
    #[cfg(target_os = "macos")]
    {
        app.set_activation_policy(tauri::ActivationPolicy::Regular)
            .map_err(|_| "cannot set smoke activation policy")?;
        app.show().map_err(|_| "cannot show smoke application")?;
    }
    let window = app
        .get_webview_window("main")
        .ok_or("smoke window missing")?;
    window
        .unminimize()
        .map_err(|_| "cannot unminimize smoke window")?;
    window.center().map_err(|_| "cannot center smoke window")?;
    window
        .set_always_on_top(true)
        .map_err(|_| "cannot raise smoke window")?;
    window.show().map_err(|_| "cannot show smoke window")?;
    window
        .set_focus()
        .map_err(|_| "cannot focus smoke window")?;
    let (sent, received) = tokio::sync::oneshot::channel();
    let main_window = window.clone();
    window
        .run_on_main_thread(move || {
            let diagnostics = smoke_window_diagnostics(&main_window);
            let _ = sent.send(diagnostics);
        })
        .map_err(|_| "cannot dispatch smoke activation to main thread")?;
    tokio::time::timeout(std::time::Duration::from_secs(3), received)
        .await
        .map_err(|_| "smoke main thread did not respond")?
        .map_err(|_| "smoke activation callback closed")?
}

fn smoke_window_diagnostics(window: &tauri::WebviewWindow) -> Result<serde_json::Value, String> {
    let diagnostics = json!({
        "pid": std::process::id(),
        "os": std::env::consts::OS,
        "visible": window.is_visible().map_err(|_| "cannot read smoke visibility")?,
        "minimized": window.is_minimized().map_err(|_| "cannot read smoke minimization")?,
        "focused": window.is_focused().map_err(|_| "cannot read smoke focus")?,
        "monitor_present": window.current_monitor().map_err(|_| "cannot read smoke monitor")?.is_some(),
        "inner_size": window.inner_size().map_err(|_| "cannot read smoke size")?,
        "outer_position": window.outer_position().map_err(|_| "cannot read smoke position")?,
    });
    #[cfg(target_os = "macos")]
    let diagnostics = {
        let mut diagnostics = diagnostics;
        use objc2::MainThreadMarker;
        use objc2_app_kit::{NSApplication, NSWindow, NSWindowCollectionBehavior};
        let marker = MainThreadMarker::new().ok_or("smoke activation is not on main thread")?;
        let app = NSApplication::sharedApplication(marker);
        let pointer = window.ns_window().map_err(|_| "smoke NSWindow missing")?;
        // SAFETY: Tauri owns the retained NSWindow, and this callback runs on
        // its main thread while the WebviewWindow handle is alive.
        let native =
            unsafe { (pointer as *const NSWindow).as_ref() }.ok_or("smoke NSWindow is null")?;
        native.setCollectionBehavior(
            native.collectionBehavior() | NSWindowCollectionBehavior::MoveToActiveSpace,
        );
        app.unhide(None);
        native.makeKeyAndOrderFront(None);
        native.orderFrontRegardless();
        #[allow(deprecated)]
        // Supports the desktop minimum macOS 12, before cooperative activation APIs.
        app.activateIgnoringOtherApps(true);
        diagnostics["appkit"] = json!({
            "active": app.isActive(), "hidden": app.isHidden(),
            "application_occlusion": app.occlusionState().0,
            "window_occlusion": native.occlusionState().0,
            "on_active_space": native.isOnActiveSpace(), "key_window": native.isKeyWindow(),
            "window_visible": native.isVisible(), "window_minimized": native.isMiniaturized(),
        });
        diagnostics
    };
    Ok(diagnostics)
}

// Only fixed diagnostic stages supplied by the explicit smoke frontend; no
// user input, credentials, or terminal contents are written into this trace.
#[tauri::command]
pub(crate) fn smoke_stage(
    fixture: State<'_, Fixture>,
    stage: String,
) -> std::result::Result<(), String> {
    use std::io::Write;
    let allowed = [
        "config",
        "window_visible",
        "login_form",
        "application_zoom",
        "login_submitted",
        "workspace",
        "project_created",
        "first_terminal",
        "second_terminal",
        "terminal_connected",
        "terminal_buffer_echo",
        "terminal_focused",
        "terminal_echo",
        "terminal_context_menu",
        "finishing",
        "attachment_uploaded",
        "directory_uploaded",
        "directory_selected",
        "directory_previewed",
        "sync_bound",
        "sync_stale_rejected",
        "sync_applied",
        "sync_progress",
        "sync_archived",
        "sync_rebound",
        "sync_history_pages",
        "sync_choices",
        "sync_continuous",
        "sync_abandoned",
        "sync_history_cleanup",
        "sync_recovery_discarded",
        "sync_remote_cleaned",
        "orphan_recovery_managed",
        "sync_finished",
        "sync_replanned",
        "sync_exported",
        "sync_canceled",
        "sync_logged_out",
    ];
    if !allowed.contains(&stage.as_str()) {
        return Err("invalid smoke stage".into());
    }
    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(fixture.report.with_extension("stages"))
        .map_err(|_| "stage file")?;
    writeln!(file, "{stage}").map_err(|_| "stage write".into())
}

#[tauri::command]
pub(crate) async fn smoke_finish(
    app: tauri::AppHandle,
    fixture: State<'_, Fixture>,
    ok: bool,
    message: String,
    attachment_path: Option<String>,
) -> std::result::Result<(), String> {
    let backend = super::backend_status(app.state()).await;
    let inspection = async {
        let directory = fixture.report.parent().ok_or("missing fixture directory")?;
        let directory = directory.join("inspect");
        std::fs::create_dir_all(&directory).map_err(|_| "fixture directory")?;
        std::fs::write(
            directory.join("fixture-local.txt"),
            b"synthetic local bytes",
        )
        .map_err(|_| "fixture write")?;
        let directory = directory.canonicalize().map_err(|_| "fixture path")?;
        let state = app.state::<super::Desktop>();
        let mut worker = state.sidecar.lock().await;
        let inspected = worker
            .as_mut()
            .ok_or("missing sidecar")?
            .inspect(&directory)
            .await
            .map_err(|_| "inspection failed")?;
        if inspected["files"] != 1 {
            return Err("wrong scan count");
        };
        Ok::<_, &str>(())
    }
    .await;
    let compatibility = async {
        let Some(config) = fixture.compat.as_ref() else {
            return Ok::<_, String>(false);
        };
        let state = app.state::<super::Desktop>();
        let (remote, connection) = {
            let data = state.inner.lock().await;
            (data.remote.clone(), data.connection.clone())
        };
        let connection = connection.ok_or("compatibility login missing")?;
        if connection.capabilities.is_some() || connection.user != config.username {
            return Err("frozen legacy server did not use capability fallback".into());
        }
        let remote = remote.ok_or("compatibility transport missing")?;
        let path = attachment_path
            .as_deref()
            .ok_or("attachment result missing")?;
        let relative = path.strip_prefix("/shared/").ok_or("attachment scope")?;
        for (path, scope, expected) in [
            (relative, "shared", b"attachment\r\n\0".as_slice()),
            (
                "directory-upload.bin",
                "workspace",
                b"directory-upload\r\n\0".as_slice(),
            ),
        ] {
            let route = super::files::route(&config.session, path, scope, "file")
                .map_err(|_| "compatibility file path")?;
            let bytes = remote
                .download_bytes(&route, |_, _| {})
                .await
                .map_err(|_| "compatibility readback failed")?;
            if bytes != expected {
                return Err("compatibility readback bytes differ".into());
            }
        }
        Ok(true)
    }
    .await;
    let ok = ok
        && backend.is_ok()
        && inspection.is_ok()
        && compatibility.is_ok()
        && (fixture.sync.is_some()
            || fixture.compat.is_some()
            || (fixture.directory_upload.load(Ordering::SeqCst)
                && fixture.attachment.load(Ordering::SeqCst)
                && fixture.input.load(Ordering::SeqCst)
                && fixture.resize.load(Ordering::SeqCst)
                && (!fixture.project_mode || fixture.terminal_inputs.lock().unwrap().len() == 2)));
    let backend = backend.unwrap_or_else(|e| e.message);
    let report = json!({"ok":ok,"compat_mode":fixture.compat.is_some(),"legacy_readback":compatibility.as_ref().is_ok_and(|v| *v),"compat_error":compatibility.err(),"attachment_path":attachment_path,"directory_upload":fixture.directory_upload.load(Ordering::SeqCst),"attachment_upload":fixture.attachment.load(Ordering::SeqCst),"message":message,"local_inspection":inspection.is_ok(),"project_mode":fixture.project_mode,"sync_mode":fixture.sync.is_some(),"independent_inputs":fixture.terminal_inputs.lock().unwrap().len(),"terminal_input":fixture.input.load(Ordering::SeqCst),"terminal_resize":fixture.resize.load(Ordering::SeqCst),"backend":backend,"os":std::env::consts::OS,"arch":std::env::consts::ARCH});
    std::fs::write(&fixture.report, serde_json::to_vec_pretty(&report).unwrap())
        .map_err(|_| "cannot write smoke report")?;
    app.exit(if ok { 0 } else { 1 });
    Ok(())
}

#[allow(clippy::result_large_err)] // tungstenite's callback requires its HTTP response error type.
pub fn plugin() -> TauriPlugin<Wry> {
    Builder::new("smoke").setup(|app, _| {
        let report = std::env::var_os("AGENTBOX_SMOKE_REPORT").ok_or("AGENTBOX_SMOKE_REPORT is required for smoke builds")?;
        // LaunchServices owns a bundled app process, so the runner needs this
        // exact fixture PID for timeout cleanup; it still verifies its executable.
        std::fs::write(std::path::Path::new(&report).with_extension("pid"), std::process::id().to_string())?;
        if let Some(compat) = CompatibilityFixture::from_environment()? {
            if std::env::var_os("AGENTBOX_SMOKE_SYNC").is_some() {
                return Err("conflicting smoke fixtures".into());
            }
            app.manage(Fixture { server: compat.server.clone(), compat: Some(compat), sync: None, project_mode: false,
                terminal_inputs: Arc::new(Mutex::new(HashSet::new())), report: report.into(),
                directory_upload:Arc::new(AtomicBool::new(false)),attachment: Arc::new(AtomicBool::new(false)), input: Arc::new(AtomicBool::new(false)), resize: Arc::new(AtomicBool::new(false)) });
            return Ok(());
        }
        if let Some(sync) = super::smoke_sync::SyncFixture::from_environment()? {
            app.manage(Fixture { server: sync.server.clone(), compat: None, sync: Some(sync), project_mode: true,
                terminal_inputs: Arc::new(Mutex::new(HashSet::new())), report: report.into(),
                directory_upload:Arc::new(AtomicBool::new(false)),attachment: Arc::new(AtomicBool::new(false)), input: Arc::new(AtomicBool::new(false)), resize: Arc::new(AtomicBool::new(false)) });
            return Ok(());
        }
        let listener = std::net::TcpListener::bind("127.0.0.1:0")?;
        listener.set_nonblocking(true)?;
        let server = format!("http://{}", listener.local_addr()?);
        let directory_upload=Arc::new(AtomicBool::new(false));
        let attachment=Arc::new(AtomicBool::new(false));
        let input = Arc::new(AtomicBool::new(false)); let resize = Arc::new(AtomicBool::new(false));
        let project_mode=std::env::var("AGENTBOX_SMOKE_MODE").as_deref()==Ok("projects");
        let terminal_inputs=Arc::new(Mutex::new(HashSet::new()));
        let projects=Arc::new(Mutex::new(Vec::<serde_json::Value>::new()));
        let terminals=Arc::new(Mutex::new(Vec::<serde_json::Value>::new()));
        app.manage(Fixture { server, compat: None, sync: None, project_mode, terminal_inputs:terminal_inputs.clone(), report: report.into(), directory_upload:directory_upload.clone(),attachment:attachment.clone(),input: input.clone(), resize: resize.clone() });
        tauri::async_runtime::spawn(async move {
            let listener = tokio::net::TcpListener::from_std(listener).unwrap();
            while let Ok((mut stream, _)) = listener.accept().await {
                let directory_upload=directory_upload.clone();
                let attachment=attachment.clone();
                let input = input.clone(); let resize = resize.clone();
                let terminal_inputs=terminal_inputs.clone();let projects=projects.clone();let terminals=terminals.clone();
                tauri::async_runtime::spawn(async move {
                    // Peek keeps the HTTP upgrade bytes available to tungstenite.
                    let mut peek = vec![0u8; 4096];
                    let count = stream.peek(&mut peek).await.unwrap();
                    let line=String::from_utf8_lossy(&peek[..count]).lines().next().unwrap_or("").to_owned();
                    if line.starts_with("GET /api/sessions/smoke/term ") || (project_mode&&line.starts_with("GET /api/sessions/smoke/client-terminals/")&&line.contains("/stream ")) {
                        let terminal_path=line.split_whitespace().nth(1).unwrap_or("").to_owned();
                        let socket = tokio_tungstenite::accept_hdr_async(stream, |request: &Request, response: Response| {
                            assert_eq!(request.headers()["authorization"], "Bearer synthetic-smoke");
                            assert!(!request.headers().contains_key("origin")); Ok(response)
                        }).await;
                        let Ok(mut socket) = socket else { return; };
                        let _ = socket.send(Message::Binary(b"Agentbox synthetic terminal\r\n".to_vec().into())).await;
                        while let Some(Ok(message)) = socket.next().await {
                            match message {
                                Message::Binary(bytes) => {
                                    if bytes.as_ref() == "echo 中文 ✓\r".as_bytes() { input.store(true, Ordering::SeqCst);terminal_inputs.lock().unwrap().insert(terminal_path.clone()); }
                                    if socket.send(Message::Binary(bytes)).await.is_err() { break; }
                                },
                                Message::Text(text) => {
                                    if let Ok(value) = serde_json::from_str::<serde_json::Value>(&text) {
                                        if value["type"] == "resize" && value["cols"].as_u64().unwrap_or(0) > 0 && value["rows"].as_u64().unwrap_or(0) > 0 { resize.store(true, Ordering::SeqCst); }
                                    }
                                },
                                Message::Close(_) => break,
                                _ => (),
                            }
                        }
                        return;
                    }
                    let mut header = Vec::new();
                    while !header.ends_with(b"\r\n\r\n") {
                        if header.len() > 8192 { return; }
                        let Ok(byte) = stream.read_u8().await else { return; }; header.push(byte);
                    }
                    let header = String::from_utf8_lossy(&header);
                    let length: usize = header.lines().find_map(|line| line.to_lowercase().strip_prefix("content-length: ").map(str::to_owned)).unwrap_or("0".into()).parse().unwrap();
                    if length > 8192 { return; }
                    let mut body = vec![0; length]; if stream.read_exact(&mut body).await.is_err() { return; }
                    let route = header.lines().next().unwrap_or("");
                    let (kind, body) = if route.starts_with("POST /api/login ") { ("application/json", r#"{"token":"synthetic-smoke"}"#.to_owned()) }
                    else if !header.to_lowercase().contains("authorization: bearer synthetic-smoke\r\n") { return; }
                    else if route.starts_with("GET /api/me ") { ("application/json", r#"{"user":"smoke","role":"user"}"#.to_owned()) }
                    else if route.starts_with("GET /api/sessions ") { ("application/json", r#"[{"id":"smoke","name":"Synthetic workspace","agent":"claude","status":"running"}]"#.to_owned()) }
                    else if route.starts_with("GET /api/sessions/smoke/files?") {
                        ("application/json",r#"[{"name":"directory-upload.bin","is_dir":false,"size":1,"mode":"-rw-r--r--","mtime":"2026-01-01"}]"#.into())
                    }
                    else if route.starts_with("POST /api/sessions/smoke/upload?path=&scope=workspace&dl=1 ") {
                        if !body.windows(b"directory-upload\r\n\0".len()).any(|part|part==b"directory-upload\r\n\0") || body.windows(b"name=\"clear\"".len()).any(|part|part==b"name=\"clear\""){return;}
                        directory_upload.store(true,Ordering::SeqCst);
                        ("application/json",r#"{"mode":"file","files":1}"#.into())
                    }
                    else if route.starts_with("POST /api/sessions/smoke/images ") {
                        if !body.windows(b"attachment\r\n\0".len()).any(|part|part==b"attachment\r\n\0") {return;}
                        attachment.store(true,Ordering::SeqCst);
                        ("application/json",r#"{"path":"/shared/.file/fixture.bin","name":"fixture.bin","orig":"fixture.bin"}"#.into())
                    }
                    else if route.starts_with("POST /api/logout ") { ("application/json", r#"{"ok":true}"#.to_owned()) }
                    else if project_mode&&route.starts_with("GET /api/clients/capabilities ") { ("application/json", json!({"protocol_version":1,"features":{"pairing":0,"project_terminals":1,"sync":0}}).to_string()) }
                    else if project_mode&&route.starts_with("GET /api/sessions/smoke/client-projects ") { ("application/json",serde_json::to_string(&*projects.lock().unwrap()).unwrap()) }
                    else if project_mode&&route.starts_with("GET /api/sessions/smoke/client-terminals ") { ("application/json",serde_json::to_string(&*terminals.lock().unwrap()).unwrap()) }
                    else if project_mode&&route.starts_with("POST /api/sessions/smoke/client-projects ") {
                        let request:serde_json::Value=serde_json::from_slice(&body).unwrap();
                        let project=json!({"id":"123456abcdef","session_id":"smoke","name":request["name"],"path":request["path"],"arguments":request["arguments"],"revision":1,"created_at":"fixture"});
                        projects.lock().unwrap().push(project.clone());("application/json",project.to_string())
                    }
                    else if project_mode&&route.starts_with("POST /api/sessions/smoke/client-terminals ") {
                        let request:serde_json::Value=serde_json::from_slice(&body).unwrap();
                        let mut terminals=terminals.lock().unwrap();
                        let terminal=json!({"id":format!("{:012x}",terminals.len()+1),"session_id":"smoke","project_id":request["project_id"],"kind":request["kind"],"state":"open","arguments":[],"created_at":"fixture"});
                        terminals.push(terminal.clone());("application/json",terminal.to_string())
                    }
                    else { ("text/html", "<!doctype html><html>old server</html>".to_owned()) };
                    let response = format!("HTTP/1.1 200 OK\r\nContent-Type: {kind}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
                    let _ = stream.write_all(response.as_bytes()).await;
                });
            }
        });
        Ok(())
    }).build()
}

pub(crate) fn upload_selection(
    app: &tauri::AppHandle,
) -> std::result::Result<std::path::PathBuf, String> {
    let fixture = app.state::<Fixture>();
    let directory = fixture.report.parent().ok_or("missing smoke directory")?;
    let path = directory.join("directory-upload.bin");
    std::fs::write(&path, b"directory-upload\r\n\0").map_err(|_| "smoke source write")?;
    path.canonicalize().map_err(|_| "smoke source path".into())
}
