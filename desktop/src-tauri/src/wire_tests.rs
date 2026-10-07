//! Synthetic wire fixtures: these tests do not start Docker or call a provider.
use crate::{
    remote::{normalize_server, Remote},
    terminal::{Command, Event, Terminal},
};
use futures_util::{SinkExt, StreamExt};
use std::{sync::Arc, time::Duration};
use tauri::ipc::{Channel, InvokeResponseBody};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
    sync::mpsc,
    time::timeout,
};
use tokio_tungstenite::{
    accept_hdr_async,
    tungstenite::{
        handshake::server::{Request, Response},
        protocol::{frame::coding::CloseCode, CloseFrame},
        Message,
    },
};

#[tokio::test]
async fn structured_login_error_preserves_safe_identifiers_and_session_stop_reason() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base =
        normalize_server(&format!("http://{}", listener.local_addr().unwrap()), false).unwrap();
    let server = tokio::spawn(async move {
        for (status, body) in [
            (
                "401 Unauthorized",
                r#"{"code":"invalid_credentials","operation_id":"0123456789abcdef0123456789abcdef","retryable":false,"error":"secret body","hint":"private path"}"#,
            ),
            (
                "200 OK",
                r#"[{"id":"s1","name":"fixture","agent":"claude","status":"stopped","stop_reason":"idle"}]"#,
            ),
        ] {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut headers = vec![];
            while !headers.ends_with(b"\r\n\r\n") {
                headers.push(stream.read_u8().await.unwrap());
                assert!(headers.len() < 8192);
            }
            let headers = String::from_utf8(headers).unwrap();
            let length: usize = headers
                .lines()
                .find_map(|line| {
                    line.to_lowercase()
                        .strip_prefix("content-length: ")
                        .map(str::to_owned)
                })
                .unwrap_or("0".into())
                .parse()
                .unwrap();
            let mut request_body = vec![0; length];
            stream.read_exact(&mut request_body).await.unwrap();
            stream.write_all(format!("HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).as_bytes()).await.unwrap();
        }
    });
    let error = Remote::login(base.clone(), "fixture", "synthetic")
        .await
        .err()
        .unwrap();
    let json = serde_json::to_string(&error).unwrap();
    assert_eq!(error.code.as_deref(), Some("invalid_credentials"));
    assert_eq!(
        error.operation_id.as_deref(),
        Some("0123456789abcdef0123456789abcdef")
    );
    assert_eq!(error.retryable, Some(false));
    assert!(!json.contains("secret") && !json.contains("private"));
    let remote = Remote::new(base, "synthetic".into()).unwrap();
    assert_eq!(remote.sessions().await.unwrap()[0].stop_reason, "idle");
    server.await.unwrap();
}

#[tokio::test]
async fn legacy_login_identity_capabilities_sessions_and_logout() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = normalize_server(
        &format!("http://{}/box/", listener.local_addr().unwrap()),
        false,
    )
    .unwrap();
    let server = tokio::spawn(async move {
        for (route, body, content_type) in [
            (
                "POST /box/api/login ",
                r#"{"token":"synthetic-only"}"#,
                "application/json",
            ),
            (
                "GET /box/api/me ",
                r#"{"user":"fixture","role":"user"}"#,
                "application/json",
            ),
            (
                "GET /box/api/clients/capabilities ",
                "<!doctype html><html></html>",
                "text/html",
            ),
            (
                "GET /box/api/sessions ",
                r#"[{"id":"s1","name":"fixture","agent":"claude","status":"stopped"}]"#,
                "application/json",
            ),
            (
                "POST /box/api/logout ",
                r#"{"ok":true}"#,
                "application/json",
            ),
        ] {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut bytes = vec![];
            let headers = loop {
                let byte = stream.read_u8().await.unwrap();
                bytes.push(byte);
                assert!(bytes.len() < 8192);
                if bytes.ends_with(b"\r\n\r\n") {
                    break String::from_utf8(bytes).unwrap();
                }
            };
            assert!(headers.starts_with(route), "{headers}");
            assert!(!headers.contains("?token="));
            let length: usize = headers
                .lines()
                .find_map(|line| {
                    line.to_lowercase()
                        .strip_prefix("content-length: ")
                        .map(str::to_owned)
                })
                .unwrap_or("0".into())
                .parse()
                .unwrap();
            let mut body_in = vec![0; length];
            stream.read_exact(&mut body_in).await.unwrap();
            if route.contains("login") {
                let login: serde_json::Value = serde_json::from_slice(&body_in).unwrap();
                assert_eq!(login["username"], "fixture");
                assert_eq!(login["password"], "fake-password");
                assert!(!headers.to_lowercase().contains("authorization:"));
            } else {
                assert!(headers
                    .to_lowercase()
                    .contains("authorization: bearer synthetic-only\r\n"));
            }
            let response = format!("HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
            stream.write_all(response.as_bytes()).await.unwrap();
        }
    });
    let remote = Remote::login(base, "fixture", "fake-password")
        .await
        .unwrap();
    assert_eq!(remote.identity().await.unwrap().user, "fixture");
    assert!(remote.capabilities().await.unwrap().is_none());
    assert_eq!(remote.sessions().await.unwrap()[0].id, "s1");
    remote.logout().await.unwrap();
    server.await.unwrap();
}

#[tokio::test]
async fn authentication_rejections_identify_credentials_pairing_and_session_expiry() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base =
        normalize_server(&format!("http://{}", listener.local_addr().unwrap()), false).unwrap();
    let server = tokio::spawn(async move {
        for (route, status, body) in [
            (
                "POST /api/login ",
                "401 Unauthorized",
                r#"{"error":"do-not-forward-password-or-proxy-details"}"#,
            ),
            (
                "POST /api/clients/pair/redeem ",
                "401 Unauthorized",
                r#"{"error":"do-not-forward-pairing-details"}"#,
            ),
            (
                "POST /api/login ",
                "200 OK",
                r#"{"token":"synthetic-only"}"#,
            ),
            (
                "GET /api/me ",
                "401 Unauthorized",
                r#"{"error":"invalid token"}"#,
            ),
        ] {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut bytes = vec![];
            let headers = loop {
                bytes.push(stream.read_u8().await.unwrap());
                assert!(bytes.len() < 8192);
                if bytes.ends_with(b"\r\n\r\n") {
                    break String::from_utf8(bytes).unwrap();
                }
            };
            assert!(headers.starts_with(route), "{headers}");
            let headers = headers.to_lowercase();
            let length: usize = headers
                .lines()
                .find_map(|line| line.strip_prefix("content-length: "))
                .unwrap_or("0")
                .parse()
                .unwrap();
            let mut body_in = vec![0; length];
            stream.read_exact(&mut body_in).await.unwrap();
            if route.starts_with("POST") {
                assert!(!headers.contains("authorization:"));
                let request: serde_json::Value = serde_json::from_slice(&body_in).unwrap();
                if route == "POST /api/login " {
                    assert_eq!(request["username"], "fixture");
                    assert_eq!(request["password"], "synthetic-password");
                } else {
                    assert_eq!(request["code"], "abcdefghijklmnopqrstuv");
                }
            } else {
                assert!(headers.contains("authorization: bearer synthetic-only\r\n"));
            }
            let response = format!("HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
            stream.write_all(response.as_bytes()).await.unwrap();
        }
    });
    let error = Remote::login(base.clone(), "fixture", "synthetic-password")
        .await
        .err()
        .expect("invalid credentials must fail");
    assert_eq!(error.kind, "invalid_credentials");
    assert_eq!(
        error.message,
        "账号或密码错误，请使用与网页版相同的账号密码"
    );
    let error = Remote::redeem(base.clone(), "abcdefghijklmnopqrstuv")
        .await
        .err()
        .expect("invalid pairing code must fail");
    assert_eq!(error.kind, "invalid_pair");
    assert_eq!(error.message, "配对码无效或已过期，请重新生成");
    let remote = Remote::login(base, "fixture", "synthetic-password")
        .await
        .unwrap();
    let error = remote
        .identity()
        .await
        .err()
        .expect("expired session must fail");
    assert_eq!(error.kind, "unauthorized");
    assert_eq!(error.message, "登录无效或已过期，请重新登录");
    timeout(Duration::from_secs(5), server)
        .await
        .unwrap()
        .unwrap();
}

#[tokio::test]
#[allow(clippy::result_large_err)] // tungstenite's callback signature is external.
async fn native_websocket_headers_binary_resize_ack_and_policy_close() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base =
        normalize_server(&format!("http://{}", listener.local_addr().unwrap()), false).unwrap();
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut socket = accept_hdr_async(stream, |request: &Request, response: Response| {
            assert_eq!(request.uri().to_string(), "/api/sessions/s1/term");
            assert_eq!(request.headers()["authorization"], "Bearer synthetic-only");
            assert!(!request.headers().contains_key("origin"));
            Ok(response)
        })
        .await
        .unwrap();
        assert_eq!(
            socket.next().await.unwrap().unwrap(),
            Message::Binary(vec![3, 0xe4, 0xb8, 0xad].into())
        );
        let resize = socket.next().await.unwrap().unwrap().into_text().unwrap();
        let resize: serde_json::Value = serde_json::from_str(&resize).unwrap();
        assert_eq!(resize["type"], "resize");
        assert_eq!(resize["cols"], 120);
        for _ in 0..3 {
            socket
                .send(Message::Binary(vec![65; 32 * 1024].into()))
                .await
                .unwrap();
        }
        socket.send(Message::Ping(vec![7].into())).await.unwrap();
        assert_eq!(
            socket.next().await.unwrap().unwrap(),
            Message::Pong(vec![7].into())
        );
        socket
            .send(Message::Close(Some(CloseFrame {
                code: CloseCode::Library(4004),
                reason: "account revoked".into(),
            })))
            .await
            .unwrap();
    });
    let (tx, mut rx) = mpsc::unbounded_channel::<serde_json::Value>();
    let events = Channel::<Event>::new(move |body| {
        if let InvokeResponseBody::Json(json) = body {
            tx.send(serde_json::from_str(&json).unwrap()).unwrap();
        }
        Ok(())
    });
    let terminal = Terminal::open(
        Arc::new(Remote::new(base, "synthetic-only".into()).unwrap()),
        "s1",
        None,
        events,
    )
    .await
    .unwrap();
    terminal
        .send(Command::Input(vec![3, 0xe4, 0xb8, 0xad]))
        .unwrap();
    terminal.send(Command::Resize(120, 40)).unwrap();
    for sequence in 1..=3 {
        let event = timeout(Duration::from_secs(3), rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(event["type"], "data");
        assert_eq!(event["sequence"], sequence);
        assert_eq!(event["bytes"].as_array().unwrap().len(), 32 * 1024);
        // An arbitrary/stale ACK cannot release the next output batch.
        terminal.send(Command::Ack(99)).unwrap();
        assert!(timeout(Duration::from_millis(40), rx.recv()).await.is_err());
        terminal.send(Command::Ack(sequence)).unwrap();
    }
    let closed = timeout(Duration::from_secs(3), rx.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(closed["code"], 4004);
    server.await.unwrap();
}

#[tokio::test]
async fn attachments_preserve_bytes_and_keep_credentials_native() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = normalize_server(
        &format!("http://{}/box/", listener.local_addr().unwrap()),
        false,
    )
    .unwrap();
    let server = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        let mut bytes = Vec::new();
        loop {
            bytes.push(stream.read_u8().await.unwrap());
            if bytes.ends_with(b"\r\n\r\n") {
                break;
            }
            assert!(bytes.len() < 8192);
        }
        let headers = String::from_utf8(bytes).unwrap();
        assert!(headers.starts_with("POST /box/api/sessions/s1/images HTTP/1.1\r\n"));
        assert!(headers
            .to_lowercase()
            .contains("authorization: bearer attachment-test\r\n"));
        assert!(!headers.contains("?token="));
        let length: usize = headers
            .lines()
            .find_map(|line| {
                line.to_lowercase()
                    .strip_prefix("content-length: ")
                    .map(str::to_owned)
            })
            .unwrap()
            .parse()
            .unwrap();
        let mut body = vec![0; length];
        stream.read_exact(&mut body).await.unwrap();
        assert!(body
            .windows(7)
            .any(|bytes| bytes == b"a\r\n\x00\xe4\xb8\xad"));
        assert!(body
            .windows(18)
            .any(|bytes| bytes == b"filename=\"raw.bin\""));
        let result =
            r#"{"path":"/shared/.file/2026-test.bin","name":"2026-test.bin","orig":"raw.bin"}"#;
        stream.write_all(format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{result}",result.len()).as_bytes()).await.unwrap();
    });
    let remote = Remote::new(base, "attachment-test".into()).unwrap();
    let result = remote
        .upload_attachment(
            "s1",
            "raw.bin",
            "application/octet-stream",
            b"a\r\n\x00\xe4\xb8\xad".to_vec(),
        )
        .await
        .unwrap();
    assert_eq!(result.path, "/shared/.file/2026-test.bin");
    server.await.unwrap();
}

#[tokio::test]
async fn manual_download_encodes_path_and_refuses_truncation() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base =
        normalize_server(&format!("http://{}", listener.local_addr().unwrap()), false).unwrap();
    let server = tokio::spawn(async move {
        for length in [7, 100] {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut bytes = Vec::new();
            loop {
                bytes.push(stream.read_u8().await.unwrap());
                if bytes.ends_with(b"\r\n\r\n") {
                    break;
                }
            }
            let headers = String::from_utf8(bytes).unwrap();
            assert!(headers.starts_with(
                "GET /api/sessions/s1/file?path=a%3Ftoken%3Db&scope=workspace&dl=1 HTTP/1.1"
            ));
            assert!(headers
                .to_lowercase()
                .contains("authorization: bearer download-test\r\n"));
            stream
                .write_all(
                    format!(
                        "HTTP/1.1 200 OK\r\nContent-Length: {length}\r\nConnection: close\r\n\r\n"
                    )
                    .as_bytes(),
                )
                .await
                .unwrap();
            stream.write_all(b"a\r\n\x00\xe4\xb8\xad").await.unwrap();
        }
    });
    let remote = Remote::new(base, "download-test".into()).unwrap();
    let path = crate::files::route("s1", "a?token=b", "workspace", "file").unwrap();
    let mut progress = Vec::new();
    let bytes = remote
        .download_bytes(&path, |n, total| progress.push((n, total)))
        .await
        .unwrap();
    assert_eq!(bytes, b"a\r\n\x00\xe4\xb8\xad");
    assert_eq!(progress.last(), Some(&(7, Some(7))));
    assert!(remote.download_bytes(&path, |_, _| {}).await.is_err());
    server.await.unwrap();
}

#[tokio::test]
async fn directory_upload_uses_exact_scope_and_never_clears_directory() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base =
        normalize_server(&format!("http://{}", listener.local_addr().unwrap()), false).unwrap();
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut header = Vec::new();
        loop {
            header.push(socket.read_u8().await.unwrap());
            if header.ends_with(b"\r\n\r\n") {
                break;
            }
            assert!(header.len() < 8192);
        }
        let header = String::from_utf8(header).unwrap();
        assert!(header.starts_with("POST /api/sessions/s1/upload?path=project%2F%E4%B8%AD%E6%96%87&scope=workspace&dl=1 HTTP/1.1"));
        assert!(header
            .to_lowercase()
            .contains("authorization: bearer directory-test\r\n"));
        let length: usize = header
            .lines()
            .find_map(|line| {
                line.to_lowercase()
                    .strip_prefix("content-length: ")
                    .map(str::to_owned)
            })
            .unwrap()
            .parse()
            .unwrap();
        let mut body = vec![0; length];
        socket.read_exact(&mut body).await.unwrap();
        assert!(!body
            .windows(b"name=\"clear\"".len())
            .any(|part| part == b"name=\"clear\""));
        assert!(body
            .windows(b"raw\r\n\0".len())
            .any(|part| part == b"raw\r\n\0"));
        let result = r#"{"mode":"file","files":1}"#;
        socket.write_all(format!("HTTP/1.1 200 OK\r\nContent-Length: {}\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n{result}",result.len()).as_bytes()).await.unwrap();
    });
    let remote = Remote::new(base, "directory-test".into()).unwrap();
    let route = crate::files::route("s1", "project/中文", "workspace", "upload").unwrap();
    let result: crate::file_upload::UploadSummary = remote
        .upload_multipart(
            &route,
            "raw.bin",
            "application/octet-stream",
            b"raw\r\n\0".to_vec(),
            |_, _| {},
        )
        .await
        .unwrap();
    assert_eq!(result.mode, "file");
    assert_eq!(result.files, 1);
    server.await.unwrap();
}

// All request bodies and tokens in these fixtures are synthetic. One accepted
// connection per operation also verifies that error handling never retries it.
async fn error_peer(
    body: Vec<u8>,
    content_type: &str,
    framing: &str,
) -> (Remote, tokio::task::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base =
        normalize_server(&format!("http://{}", listener.local_addr().unwrap()), false).unwrap();
    let content_type = content_type.to_owned();
    let framing = framing.to_owned();
    let server = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        let mut header = vec![];
        while !header.ends_with(b"\r\n\r\n") {
            header.push(stream.read_u8().await.unwrap());
            assert!(header.len() < 16384);
        }
        let header = String::from_utf8(header).unwrap().to_lowercase();
        assert!(!header.contains("?token="));
        let length: usize = header
            .lines()
            .find_map(|l| l.strip_prefix("content-length: "))
            .unwrap_or("0")
            .parse()
            .unwrap();
        let mut incoming = vec![0; length];
        stream.read_exact(&mut incoming).await.unwrap();
        let framing_header = if framing == "chunked" {
            "Transfer-Encoding: chunked".to_owned()
        } else {
            format!(
                "Content-Length: {}",
                if framing == "truncated" || framing == "stalled" {
                    body.len() + 100
                } else {
                    body.len()
                }
            )
        };
        let response=format!("HTTP/1.1 403 Forbidden\r\nContent-Type: {content_type}\r\nX-Agentbox-Operation-ID: 0123456789abcdef0123456789abcdef\r\n{framing_header}\r\nConnection: close\r\n\r\n");
        stream.write_all(response.as_bytes()).await.unwrap();
        if framing == "stalled" {
            tokio::time::sleep(Duration::from_secs(30)).await;
            return;
        }
        if framing == "chunked" {
            let _ = stream
                .write_all(format!("{:x}\r\n", body.len()).as_bytes())
                .await;
            let _ = stream.write_all(&body).await;
            let _ = stream.write_all(b"\r\n0\r\n\r\n").await;
        } else {
            let _ = stream.write_all(&body).await;
        }
    });
    (
        Remote::new(base, "synthetic-error-token".into()).unwrap(),
        server,
    )
}

#[tokio::test]
async fn structured_errors_cover_upload_download_api_and_terminal_handshake() {
    let body=br#"{"code":"quota_exhausted","operation_id":"invalid","retryable":false,"error":"secret prompt /private/path","hint":"secret token"}"#.to_vec();
    for operation in ["upload", "download", "api", "terminal"] {
        let (remote, server) = error_peer(body.clone(), "application/json", "fixed").await;
        let error = match operation {
            "upload" => remote
                .upload_attachment("s1", "fixture.txt", "text/plain", b"synthetic".to_vec())
                .await
                .err()
                .unwrap(),
            "download" => remote
                .download_bytes("api/sessions/s1/file", |_, _| {
                    panic!("error must not report downloaded bytes")
                })
                .await
                .err()
                .unwrap(),
            "api" => remote.sessions().await.err().unwrap(),
            _ => Terminal::open(Arc::new(remote), "s1", None, Channel::new(|_| Ok(())))
                .await
                .err()
                .unwrap(),
        };
        assert_eq!(error.kind, "forbidden", "{operation}");
        assert_eq!(
            error.code.as_deref(),
            Some("quota_exhausted"),
            "{operation}"
        );
        assert_eq!(
            error.operation_id.as_deref(),
            Some("0123456789abcdef0123456789abcdef"),
            "{operation}"
        );
        assert_eq!(error.retryable, Some(false));
        let rendered = serde_json::to_string(&error).unwrap();
        assert!(!rendered.contains("secret") && !rendered.contains("/private/"));
        server.await.unwrap();
    }
}

#[tokio::test]
async fn error_body_bounds_and_deadlines_keep_status_and_safe_reference() {
    for (body, kind, framing) in [
        (
            b"<html>private proxy details</html>".to_vec(),
            "text/html",
            "fixed",
        ),
        (vec![b'x'; 20000], "application/json", "fixed"),
        (vec![b'x'; 20000], "application/json", "chunked"),
        (
            br#"{"code":"quota_exhausted"}"#.to_vec(),
            "application/json",
            "truncated",
        ),
        (vec![], "application/json", "stalled"),
    ] {
        let (remote, server) = error_peer(body, kind, framing).await;
        let error = timeout(
            Duration::from_secs(5),
            remote.download_bytes("api/sessions/s1/file", |_, _| {}),
        )
        .await
        .expect("error metadata hung the download")
        .err()
        .unwrap();
        assert_eq!(error.kind, "forbidden");
        assert!(error.code.is_none());
        assert_eq!(
            error.operation_id.as_deref(),
            Some("0123456789abcdef0123456789abcdef")
        );
        assert!(!error.message.contains("private"));
        if framing == "stalled" {
            server.abort();
        } else {
            server.await.unwrap();
        }
    }
}
