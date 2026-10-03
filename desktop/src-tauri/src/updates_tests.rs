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
