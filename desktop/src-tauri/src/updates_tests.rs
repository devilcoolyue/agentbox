//! Real loopback HTTP and minisign verification with the production download
//! function. Only the Tauri application runtime is mocked; no installer runs.
use super::*;
use serde_json::json;
use tauri::test::{mock_builder, mock_context, noop_assets, MockRuntime};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

const PAYLOAD: &[u8] = include_bytes!("../testdata/updater/payload.bin");
const KEY: &str = include_str!("../testdata/updater/public-key.txt");
const SIGNATURE: &str = include_str!("../testdata/updater/versioned.sig");
const LEGACY: &str = include_str!("../testdata/updater/legacy.sig");

struct Fixture {
    endpoint: url::Url,
    task: tokio::task::JoinHandle<()>,
}
impl Drop for Fixture {
    fn drop(&mut self) {
        self.task.abort();
    }
}
impl Fixture {
    async fn start(version: &str, signature: &str) -> Self {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let base = format!("http://{}", listener.local_addr().unwrap());
        let endpoint = format!("{base}/latest.json").parse().unwrap();
        let manifest =
            json!({"version":version,"url":format!("{base}/valid"),"signature":signature.trim()})
                .to_string();
        let task = tokio::spawn(async move {
            while let Ok((mut stream, _)) = listener.accept().await {
                let mut request = Vec::new();
                while !request.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    if stream.read_exact(&mut byte).await.is_err() || request.len() > 8192 {
                        return;
                    }
                    request.push(byte[0]);
                }
                let request = String::from_utf8(request).unwrap();
                assert!(!request.to_ascii_lowercase().contains("authorization:"));
                let path = request.split_whitespace().nth(1).unwrap();
                let mut body = PAYLOAD.to_vec();
                let mut length = body.len();
                if path == "/latest.json" {
                    body = manifest.as_bytes().to_vec();
                    length = body.len();
                } else if path == "/corrupt" {
                    body[0] ^= 1;
                } else if path == "/interrupted" {
                    body.truncate(7); // Advertised complete length; connection closes early.
                } else if path == "/oversize" {
                    length = DOWNLOAD_LIMIT + 1;
                }
                stream.write_all(format!("HTTP/1.1 200 OK\r\nContent-Length: {length}\r\nConnection: close\r\n\r\n").as_bytes()).await.unwrap();
                if path == "/stall" {
                    // An open body that never completes; retry remains possible.
                    let mut closed = [0];
                    let _ = stream.read(&mut closed).await;
                } else {
                    let _ = stream.write_all(&body).await;
                }
            }
        });
        Self { endpoint, task }
    }

    async fn candidate(&self, app: &tauri::App<MockRuntime>) -> tauri_plugin_updater::Update {
        app.updater_builder()
            .endpoints(vec![self.endpoint.clone()])
            .unwrap()
            .no_proxy()
            .timeout(Duration::from_secs(3))
            .build()
            .unwrap()
            .check()
            .await
            .unwrap()
            .unwrap()
    }
}

fn app() -> tauri::App<MockRuntime> {
    let config: serde_json::Value =
        serde_json::from_str(include_str!("../tauri.conf.json")).unwrap();
    let mut plugin = config["plugins"]["updater"].clone();
    // HTTP is enabled only in this test runtime, on an ephemeral loopback port.
    plugin["dangerousInsecureTransportProtocol"] = json!(true);
    plugin["pubkey"] = json!(KEY.trim());
    let mut context = mock_context(noop_assets());
    context
        .config_mut()
        .plugins
        .0
        .insert("updater".into(), plugin);
    mock_builder()
        .plugin(tauri_plugin_updater::Builder::new().build())
        .build(context)
        .unwrap()
}

#[tokio::test]
async fn real_signature_accepts_bytes_and_rejects_tampering() {
    let app = app();
    let fixture = Fixture::start("9.0.0", SIGNATURE).await;
    let mut update = fixture.candidate(&app).await;
    assert_eq!(
        verified_download(&update, DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT)
            .await
            .unwrap(),
        PAYLOAD
    );
    update.download_url.set_path("/corrupt");
    assert!(matches!(
        update.download(|_, _| {}, || {}).await,
        Err(tauri_plugin_updater::Error::Minisign(_))
    ));
    assert_eq!(
        verified_download(&update, DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT)
            .await
            .unwrap_err()
            .kind,
        "update"
    );
}

#[tokio::test]
async fn signed_version_is_required_and_must_match_manifest() {
    let app = app();
    for (signature, version, missing) in [(LEGACY, "9.0.0", true), (SIGNATURE, "9.0.1", false)] {
        let fixture = Fixture::start(version, signature).await;
        let update = fixture.candidate(&app).await;
        let error = update.download(|_, _| {}, || {}).await.unwrap_err();
        assert!(
            if missing {
                matches!(error, tauri_plugin_updater::Error::MissingSignedVersion)
            } else {
                matches!(
                    error,
                    tauri_plugin_updater::Error::SignedVersionMismatch { .. }
                )
            },
            "unexpected rejection: {error}"
        );
        assert!(verified_download(&update, DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT)
            .await
            .is_err());
    }
}

#[tokio::test]
async fn interrupted_oversized_and_stalled_downloads_cannot_return_install_bytes() {
    let app = app();
    let fixture = Fixture::start("9.0.0", SIGNATURE).await;
    let mut update = fixture.candidate(&app).await;
    for (path, limit, timeout, kind) in [
        ("/interrupted", DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT, "update"),
        ("/oversize", DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT, "limit"),
        ("/valid", 8, DOWNLOAD_TIMEOUT, "limit"),
        (
            "/stall",
            DOWNLOAD_LIMIT,
            Duration::from_millis(100),
            "timeout",
        ),
    ] {
        update.download_url.set_path(path);
        assert_eq!(
            verified_download(&update, limit, timeout)
                .await
                .unwrap_err()
                .kind,
            kind,
            "{path}"
        );
        update.download_url.set_path("/valid");
        assert_eq!(
            verified_download(&update, DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT)
                .await
                .unwrap(),
            PAYLOAD,
            "retry after {path}"
        );
    }
}

#[test]
fn download_asset_must_belong_to_announced_release() {
    let base = "https://github.com/devilcoolyue/agentbox/releases/download/";
    for (suffix, allowed) in [
        ("desktop-v9.0.0/Agentbox.app.tar.gz", true),
        ("desktop-v9.0.1/Agentbox.app.tar.gz", false),
        ("desktop-v9.0.0-evil/Agentbox.app.tar.gz", false),
        ("desktop-v9.0.0/", false),
        ("desktop-v9.0.0/sub/app", false),
        ("desktop-v9.0.0/%2fapp", false),
        ("desktop-v9.0.0/app?token=x", false),
        ("desktop-v9.0.0/app#fragment", false),
    ] {
        assert_eq!(
            allowed_download(&format!("{base}{suffix}").parse().unwrap(), "9.0.0"),
            allowed,
            "{suffix}"
        );
    }
    for host in ["github.com.evil.test", "github.com:444", "user@github.com"] {
        let url =
            format!("https://{host}/devilcoolyue/agentbox/releases/download/desktop-v9.0.0/app");
        assert!(!allowed_download(&url.parse().unwrap(), "9.0.0"));
    }
}

#[tokio::test]
async fn update_gate_excludes_inspection_selection_and_every_transfer_engine() {
    let desktop = Desktop::default();
    for mutex in [
        &desktop.sync_work,
        &desktop.attachment_work,
        &desktop.attachment_selection,
    ] {
        let held = mutex.lock().await;
        assert_eq!(IdleForUpdate::acquire(&desktop).err().unwrap().kind, "busy");
        drop(held);
    }
    desktop.inspection.lock().await.running = true;
    assert_eq!(IdleForUpdate::acquire(&desktop).err().unwrap().kind, "busy");
    desktop.inspection.lock().await.running = false;
    let background = desktop.sidecar.lock().await;
    assert_eq!(IdleForUpdate::acquire(&desktop).err().unwrap().kind, "busy");
    drop(background);
    let idle = IdleForUpdate::acquire(&desktop).unwrap();
    assert!(desktop.sync_work.try_lock().is_err());
    assert!(desktop.attachment_work.try_lock().is_err());
    assert!(desktop.attachment_selection.try_lock().is_err());
    assert!(desktop.inspection.try_lock().is_err());
    assert!(desktop.sidecar.try_lock().is_err());
    drop(idle);
    assert!(desktop.inspection.try_lock().is_ok());
}

#[tokio::test]
async fn consumed_install_candidate_requires_a_fresh_check_after_download_failure() {
    let app = app();
    let fixture = Fixture::start("9.0.0", SIGNATURE).await;
    let mut update = fixture.candidate(&app).await;
    update.download_url.set_path("/corrupt");
    let state = UpdateState::default();
    *state.candidate.lock().await = Some(update);
    assert!(take_candidate(&state, "9.0.1").await.is_err());
    let update = take_candidate(&state, "9.0.0").await.unwrap();
    assert!(verified_download(&update, DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT)
        .await
        .is_err());
    assert!(state.candidate.lock().await.is_none());
    assert!(take_candidate(&state, "9.0.0").await.is_err());
}

#[tokio::test]
async fn failed_installer_preserves_authenticated_terminal_and_releases_admission() {
    use crate::{
        remote::Remote,
        terminal::{Command, Event, Terminal},
    };
    use futures_util::StreamExt;
    use std::sync::Arc;
    use tauri::ipc::Channel;
    use tokio_tungstenite::tungstenite::Message;

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base: url::Url = format!("http://{}", listener.local_addr().unwrap())
        .parse()
        .unwrap();
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut socket = tokio_tungstenite::accept_async(stream).await.unwrap();
        assert_eq!(
            socket.next().await.unwrap().unwrap(),
            Message::Binary(b"after failed installer".to_vec().into())
        );
    });
    let remote = Arc::new(Remote::new(base, "synthetic-updater-test".into()).unwrap());
    let terminal = Terminal::open(
        remote.clone(),
        "s1",
        None,
        Channel::<Event>::new(|_| Ok(())),
    )
    .await
    .unwrap();
    let desktop = Desktop::default();
    let (cancel, canceled) = tokio::sync::oneshot::channel();
    {
        let mut data = desktop.inner.lock().await;
        data.remote = Some(remote.clone());
        data.terminals.insert(1, terminal);
        data.pending.insert(2, cancel);
    }
    let mut idle = IdleForUpdate::acquire(&desktop).unwrap();
    let error = install_verified(&desktop, &mut idle, || {
        Err(Error::new("fixture_install", "synthetic install failure"))
    })
    .await
    .unwrap_err();
    assert_eq!(error.kind, "fixture_install");
    assert!(canceled.await.is_err());
    let data = desktop.inner.lock().await;
    assert!(!data.disconnecting);
    assert!(Arc::ptr_eq(data.remote.as_ref().unwrap(), &remote));
    assert_eq!(data.terminals.len(), 1);
    data.terminals
        .get(&1)
        .unwrap()
        .send(Command::Input(b"after failed installer".to_vec()))
        .unwrap();
    drop(data);
    tokio::time::timeout(Duration::from_secs(3), server)
        .await
        .unwrap()
        .unwrap();
    drop(idle);
    assert!(IdleForUpdate::acquire(&desktop).is_ok());
}

#[cfg(unix)]
#[tokio::test]
async fn health_sidecar_exits_before_installer_callback() {
    use std::os::unix::fs::PermissionsExt;
    let directory = tempfile::tempdir().unwrap();
    let script = directory.path().join("synthetic-sidecar");
    std::fs::write(&script, b"#!/bin/sh\nprintf '%s\\n' '{\"version\":1,\"type\":\"ready\",\"capabilities\":[]}'\nwhile IFS= read -r line; do :; done\nprintf stopped > \"$0.stopped\"\n").unwrap();
    std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
    let desktop = Desktop::default();
    *desktop.sidecar.lock().await = Some(Sidecar::start(&script).await.unwrap());
    let mut idle = IdleForUpdate::acquire(&desktop).unwrap();
    install_verified(&desktop, &mut idle, || {
        assert_eq!(
            std::fs::read(directory.path().join("synthetic-sidecar.stopped")).unwrap(),
            b"stopped"
        );
        Err(Error::new("fixture_install", "do not install anything"))
    })
    .await
    .unwrap_err();
    assert!(idle.background.is_none());
    assert!(!desktop.inner.lock().await.disconnecting);
}
