use futures_util::StreamExt;
use reqwest::{Client, Method, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::time::Duration;
use url::Url;

const MAX_JSON: usize = 4 * 1024 * 1024;

#[derive(Debug, Clone, Serialize)]
pub struct Error {
    pub kind: String,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub code: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub operation_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub retryable: Option<bool>,
}

impl Error {
    pub fn new(kind: &str, message: &str) -> Self {
        Self {
            kind: kind.into(),
            message: message.into(),
            code: None,
            operation_id: None,
            retryable: None,
        }
    }
    pub fn network(_: impl std::fmt::Display) -> Self {
        // Library errors can contain URLs or request details. Never forward them to the UI/logs.
        Self::new("network", "连接失败，请检查服务器地址、网络及 TLS 证书")
    }
}

pub type Result<T> = std::result::Result<T, Error>;

pub fn normalize_server(raw: &str, allow_http: bool) -> Result<Url> {
    let mut url =
        Url::parse(raw.trim()).map_err(|_| Error::new("address", "请输入完整的服务器地址"))?;
    if !matches!(url.scheme(), "http" | "https")
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(Error::new(
            "address",
            "地址仅支持 HTTP(S)，不能包含凭证、查询参数或片段",
        ));
    }
    let local = matches!(url.host_str(), Some("localhost" | "127.0.0.1" | "[::1]"));
    if url.scheme() == "http" && !local && !allow_http {
        return Err(Error::new(
            "insecure",
            "远程 HTTP 会明文传输密码和终端内容，请使用 HTTPS 或明确允许 HTTP",
        ));
    }
    if url.path().contains('%') || url.path().contains('\\') {
        return Err(Error::new("address", "服务器路径不能包含转义字符"));
    }
    let path = format!("{}/", url.path().trim_end_matches('/'));
    url.set_path(&path);
    Ok(url)
}

#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct Features {
    pub pairing: u32,
    pub project_terminals: u32,
    pub sync: u32,
    #[serde(default)]
    pub sync_recovery_inspect: u32,
    #[serde(default)]
    pub sync_recovery_gc: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Capabilities {
    pub protocol_version: u32,
    #[serde(default)]
    pub server_id: String,
    pub features: Features,
}

#[derive(Clone, Serialize)]
pub struct Connection {
    pub server: String,
    pub user: String,
    pub role: String,
    pub capabilities: Option<Capabilities>,
}

#[derive(Deserialize)]
pub struct Identity {
    pub user: String,
    pub role: String,
}

#[derive(Serialize, Deserialize)]
pub struct Session {
    pub id: String,
    pub name: String,
    pub agent: String,
    pub status: String,
    #[serde(default)]
    pub stop_reason: String,
    #[serde(default)]
    pub account_label: String,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct UploadResult {
    pub path: String,
    pub name: String,
    #[serde(default)]
    pub orig: String,
}

// Intentionally not Debug/Serialize: the bearer token stays in the native process.
pub struct Remote {
    pub base: Url,
    client: Client,
    token: String,
}

impl Remote {
    pub fn new(base: Url, token: String) -> Result<Self> {
        let client = Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(10))
            .timeout(Duration::from_secs(20))
            .build()
            .map_err(Error::network)?;
        Ok(Self {
            base,
            client,
            token,
        })
    }

    pub fn endpoint(&self, path: &str) -> Url {
        self.base.join(path).expect("only fixed native API paths")
    }

    pub fn authorization(&self) -> String {
        format!("Bearer {}", self.token)
    }
    pub fn token(&self) -> &str {
        &self.token
    }

    async fn request(
        &self,
        method: Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<(StatusCode, String, Vec<u8>)> {
        let mut request = self.client.request(method, self.endpoint(path));
        if !self.token.is_empty() {
            request = request.bearer_auth(&self.token);
        }
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request.send().await.map_err(Error::network)?;
        let status = response.status();
        if !(status.is_success()
            || path == "api/clients/capabilities" && status == StatusCode::NOT_FOUND
            || path == "api/logout" && status == StatusCode::UNAUTHORIZED)
        {
            return Err(http_error(response).await);
        }
        let content_type = response
            .headers()
            .get("content-type")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_lowercase();
        let mut stream = response.bytes_stream();
        let mut bytes = Vec::new();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(Error::network)?;
            if bytes.len() + chunk.len() > MAX_JSON {
                return Err(Error::new("protocol", "服务器响应超出大小限制"));
            }
            bytes.extend_from_slice(&chunk);
        }
        Ok((status, content_type, bytes))
    }

    pub async fn login(base: Url, username: &str, password: &str) -> Result<Self> {
        Self::authenticate(
            base,
            "api/login",
            serde_json::json!({"username":username,"password":password}),
        )
        .await
        .map_err(|mut error| {
            if error.kind == "unauthorized" && error.code.is_none() {
                error.kind = "invalid_credentials".into();
                error.message = "账号或密码错误，请使用与网页版相同的账号密码".into();
            }
            error
        })
    }

    pub async fn redeem(base: Url, code: &str) -> Result<Self> {
        if code.len() != 22
            || !code
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        {
            return Err(Error::new("request", "配对码格式无效"));
        }
        Self::authenticate(
            base,
            "api/clients/pair/redeem",
            serde_json::json!({"code":code}),
        )
        .await
        .map_err(|mut error| {
            if error.kind == "unauthorized" && error.code.is_none() {
                error.kind = "invalid_pair".into();
                error.message = "配对码无效或已过期，请重新生成".into();
            }
            error
        })
    }

    async fn authenticate(base: Url, path: &str, body: Value) -> Result<Self> {
        let mut remote = Self::new(base, String::new())?;
        let (status, content_type, bytes) = remote.request(Method::POST, path, Some(body)).await?;
        check_status(status)?;
        require_json(&content_type)?;
        #[derive(Deserialize)]
        struct Login {
            token: String,
        }
        let response: Login = parse(&bytes)?;
        if response.token.is_empty()
            || response.token.len() > 4096
            || response.token.contains(['\r', '\n'])
        {
            return Err(Error::new("protocol", "登录响应缺少有效令牌"));
        }
        remote.token = response.token;
        Ok(remote)
    }

    pub async fn identity(&self) -> Result<Identity> {
        self.get("api/me").await
    }
    pub async fn sessions(&self) -> Result<Vec<Session>> {
        self.get("api/sessions").await
    }

    async fn get<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T> {
        self.api(Method::GET, path, None).await
    }

    pub(crate) async fn api<T: serde::de::DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<T> {
        let (status, content_type, bytes) = self.request(method, path, body).await?;
        check_status(status)?;
        require_json(&content_type)?;
        parse(&bytes)
    }

    pub async fn capabilities(&self) -> Result<Option<Capabilities>> {
        let (status, content_type, bytes) = self
            .request(Method::GET, "api/clients/capabilities", None)
            .await?;
        classify_capabilities(status, &content_type, &bytes)
    }

    pub async fn logout(&self) -> Result<()> {
        let (status, _, _) = self.request(Method::POST, "api/logout", None).await?;
        if status == StatusCode::UNAUTHORIZED {
            return Ok(());
        }
        check_status(status)
    }

    pub async fn download_bytes(
        &self,
        path: &str,
        mut progress: impl FnMut(u64, Option<u64>),
    ) -> Result<Vec<u8>> {
        const LIMIT: u64 = 64 * 1024 * 1024;
        let response = self
            .client
            .get(self.endpoint(path))
            .bearer_auth(&self.token)
            .header("Accept-Encoding", "identity")
            .send()
            .await
            .map_err(Error::network)?;
        if !response.status().is_success() {
            return Err(http_error(response).await);
        }
        if response.status() != StatusCode::OK {
            return Err(Error::new("protocol", "下载响应无效"));
        }
        let total = response.content_length();
        if total.is_some_and(|size| size > LIMIT) {
            return Err(Error::new("limit", "手动下载每个文件最多 64 MiB"));
        }
        let mut stream = response.bytes_stream();
        let mut bytes = Vec::new();
        let mut last = 0;
        progress(0, total);
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(Error::network)?;
            if bytes.len() as u64 + chunk.len() as u64 > LIMIT {
                return Err(Error::new("limit", "文件超过下载限制"));
            }
            bytes.extend_from_slice(&chunk);
            if bytes.len() - last >= 1024 * 1024 {
                last = bytes.len();
                progress(last as u64, total);
            }
        }
        if total.is_some_and(|size| size != bytes.len() as u64) {
            return Err(Error::new("protocol", "下载内容不完整"));
        }
        progress(bytes.len() as u64, total);
        Ok(bytes)
    }

    #[cfg(test)]
    pub async fn upload_attachment(
        &self,
        session: &str,
        filename: &str,
        mime: &str,
        bytes: Vec<u8>,
    ) -> Result<UploadResult> {
        self.upload_attachment_progress(session, filename, mime, bytes, |_, _| {})
            .await
    }

    pub async fn upload_attachment_progress(
        &self,
        session: &str,
        filename: &str,
        mime: &str,
        bytes: Vec<u8>,
        progress: impl FnMut(u64, u64) + Send + 'static,
    ) -> Result<UploadResult> {
        valid_session_id(session)?;
        let result: UploadResult = self
            .upload_multipart(
                &format!("api/sessions/{session}/images"),
                filename,
                mime,
                bytes,
                progress,
            )
            .await?;
        if !attachment_path_valid(&result.path) {
            return Err(Error::new("protocol", "服务器返回的附件路径无效"));
        }
        Ok(result)
    }

    pub(crate) async fn upload_multipart<T: serde::de::DeserializeOwned>(
        &self,
        route: &str,
        filename: &str,
        mime: &str,
        bytes: Vec<u8>,
        mut progress: impl FnMut(u64, u64) + Send + 'static,
    ) -> Result<T> {
        if filename.is_empty()
            || filename.len() > 255
            || filename.contains(['/', '\\', '\0', '\r', '\n', '"'])
            || filename == "."
            || filename == ".."
        {
            return Err(Error::new("request", "文件名无效"));
        }
        if bytes.len() > 19 * 1024 * 1024 {
            return Err(Error::new(
                "limit",
                "附件超过 19 MiB 限制（服务器上限包含上传封装）",
            ));
        }
        let mime = mime.split(';').next().unwrap_or("").trim();
        if mime.is_empty() || mime.len() > 128 || !mime.bytes().all(|b| b.is_ascii_graphic()) {
            return Err(Error::new("request", "文件类型无效"));
        }
        if !mime
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"!#$&^_.+-/".contains(&byte))
        {
            return Err(Error::new("request", "文件类型无效"));
        }
        // Keep the dependency graph small and the upload body bounded by
        // constructing the one-field multipart body directly. Filename and
        // MIME are validated above, so they cannot inject a second part.
        let boundary = format!(
            "agentbox-{:x}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_err(|_| Error::new("request", "无法生成上传边界"))?
                .as_nanos()
        );
        let mut body = Vec::with_capacity(bytes.len() + 256);
        body.extend_from_slice(format!("--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{filename}\"\r\nContent-Type: {mime}\r\n\r\n").as_bytes());
        body.extend_from_slice(&bytes);
        body.extend_from_slice(format!("\r\n--{boundary}--\r\n").as_bytes());
        let header_bytes = body.len() - bytes.len() - format!("\r\n--{boundary}--\r\n").len();
        let content_bytes = bytes.len();
        let body_length = body.len();
        progress(0, content_bytes as u64);
        let stream = futures_util::stream::unfold(
            (body, 0usize, 0usize, progress),
            move |(body, offset, reported, mut progress)| async move {
                if offset >= body.len() {
                    return None;
                }
                let end = (offset + 64 * 1024).min(body.len());
                let chunk = body[offset..end].to_vec();
                let read = end.saturating_sub(header_bytes).min(content_bytes);
                let mut reported = reported;
                if read.saturating_sub(reported) >= 1024 * 1024 || end == body.len() {
                    progress(read as u64, content_bytes as u64);
                    reported = read;
                }
                Some((
                    Ok::<_, std::io::Error>(chunk),
                    (body, end, reported, progress),
                ))
            },
        );
        let response = self
            .client
            .post(self.endpoint(route))
            .bearer_auth(&self.token)
            .header(
                "Content-Type",
                format!("multipart/form-data; boundary={boundary}"),
            )
            .header("Content-Length", body_length)
            .body(reqwest::Body::wrap_stream(stream))
            .send()
            .await
            .map_err(Error::network)?;
        let status = response.status();
        let content_type = response
            .headers()
            .get("content-type")
            .and_then(|value| value.to_str().ok())
            .unwrap_or("")
            .to_lowercase();
        if !status.is_success() {
            return Err(http_error(response).await);
        }
        if status != StatusCode::OK {
            return Err(Error::new("protocol", "附件响应状态无效"));
        }
        require_json(&content_type)?;
        let mut stream = response.bytes_stream();
        let mut bytes = Vec::new();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(Error::network)?;
            if bytes.len() + chunk.len() > 64 * 1024 {
                return Err(Error::new("protocol", "上传响应超出大小限制"));
            }
            bytes.extend_from_slice(&chunk);
        }
        parse(&bytes)
    }
}

fn require_json(content_type: &str) -> Result<()> {
    if content_type.split(';').next().unwrap_or("").trim() != "application/json" {
        return Err(Error::new(
            "protocol",
            "服务器返回了非 JSON 响应，请检查地址和反向代理",
        ));
    }
    Ok(())
}

fn parse<T: serde::de::DeserializeOwned>(bytes: &[u8]) -> Result<T> {
    serde_json::from_slice(bytes).map_err(|_| Error::new("protocol", "服务器响应格式不兼容"))
}

// Error bodies are metadata, never downloads. Retain the known HTTP status and
// safe header reference if the peer truncates, stalls, or sends an oversized body.
pub(crate) const MAX_PROBLEM: usize = 16 * 1024;
async fn http_error(response: reqwest::Response) -> Error {
    let status = response.status();
    let content_type = response
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_lowercase();
    let reference = response
        .headers()
        .get("x-agentbox-operation-id")
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned);
    let fallback = || response_problem(status, "", &[], reference.as_deref());
    if content_type.split(';').next().unwrap_or("").trim() != "application/json"
        || response
            .content_length()
            .is_some_and(|n| n > MAX_PROBLEM as u64)
    {
        return fallback();
    }
    let read = async {
        let mut bytes = Vec::new();
        let mut stream = response.bytes_stream();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.ok()?;
            if bytes.len() + chunk.len() > MAX_PROBLEM {
                return None;
            }
            bytes.extend_from_slice(&chunk);
        }
        Some(bytes)
    };
    match tokio::time::timeout(Duration::from_secs(2), read).await {
        Ok(Some(bytes)) => response_problem(status, &content_type, &bytes, reference.as_deref()),
        _ => fallback(),
    }
}

pub(crate) fn response_problem(
    status: StatusCode,
    content_type: &str,
    bytes: &[u8],
    reference: Option<&str>,
) -> Error {
    let mut error = response_error(
        status,
        content_type,
        if bytes.len() <= MAX_PROBLEM {
            bytes
        } else {
            &[]
        },
    );
    if error.operation_id.is_none() {
        error.operation_id = reference
            .filter(|s| valid_operation_id(s))
            .map(str::to_owned);
    }
    error
}
fn valid_operation_id(value: &str) -> bool {
    value.len() == 32
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

// Forward only machine identifiers, never server/proxy error text, hints or paths.
fn response_error(status: StatusCode, content_type: &str, bytes: &[u8]) -> Error {
    let mut error = check_status(status)
        .err()
        .unwrap_or_else(|| Error::new("protocol", "服务器响应格式不兼容"));
    if !content_type
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .eq_ignore_ascii_case("application/json")
    {
        return error;
    }
    let Ok(value) = serde_json::from_slice::<Value>(bytes) else {
        return error;
    };
    error.code = value
        .get("code")
        .and_then(Value::as_str)
        .filter(|s| {
            !s.is_empty()
                && s.len() <= 64
                && s.bytes()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_')
        })
        .map(str::to_owned);
    error.operation_id = value
        .get("operation_id")
        .and_then(Value::as_str)
        .filter(|s| valid_operation_id(s))
        .map(str::to_owned);
    if error.code.is_some() {
        error.retryable = value.get("retryable").and_then(Value::as_bool);
    }
    error
}

pub fn check_status(status: StatusCode) -> Result<()> {
    let (kind, message) = match status.as_u16() {
        200..=299 => return Ok(()),
        401 => ("unauthorized", "登录无效或已过期，请重新登录"),
        403 => ("forbidden", "没有此操作的权限"),
        404 => ("missing", "工作空间或接口不存在"),
        409 => (
            "conflict",
            "资源已更改、达到数量上限或仍有终端，请刷新后重试",
        ),
        429 => ("rate_limit", "请求过于频繁，请稍后重试"),
        300..=399 => ("redirect", "服务器返回重定向，请直接填写最终地址"),
        500..=599 => ("server", "服务端暂时不可用，请稍后重试"),
        _ => ("request", "服务器拒绝了请求"),
    };
    Err(Error::new(kind, message))
}

fn classify_capabilities(
    status: StatusCode,
    content_type: &str,
    bytes: &[u8],
) -> Result<Option<Capabilities>> {
    if status == StatusCode::NOT_FOUND {
        return Ok(None);
    }
    check_status(status)?;
    // The frozen eb845db server routes unknown GETs to the SPA HTML handler.
    if content_type.split(';').next().unwrap_or("").trim() == "text/html" {
        return Ok(None);
    }
    require_json(content_type)?;
    let caps: Capabilities = parse(bytes)?;
    if caps.protocol_version != 1 {
        return Err(Error::new("version", "客户端协议版本不兼容，请升级客户端"));
    }
    Ok(Some(caps))
}

pub fn valid_session_id(id: &str) -> Result<()> {
    if id.is_empty()
        || id.len() > 128
        || !id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
    {
        return Err(Error::new("request", "无效的工作空间 ID"));
    }
    Ok(())
}

fn attachment_path_valid(path: &str) -> bool {
    let Some(name) = path
        .strip_prefix("/shared/.images/")
        .or_else(|| path.strip_prefix("/shared/.file/"))
    else {
        return false;
    };
    !name.is_empty()
        && name != "."
        && name != ".."
        && name.len() <= 255
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"._-".contains(&b))
}

#[cfg(test)]
mod tests {
    #[test]
    fn native_http_fallback_catalog_matches_renderer_allowlist() {
        let catalog: serde_json::Value =
            serde_json::from_str(include_str!("../../src/native-problems.json")).unwrap();
        for status in [400, 401, 403, 404, 409, 429, 302, 503] {
            let error =
                super::check_status(reqwest::StatusCode::from_u16(status).unwrap()).unwrap_err();
            assert_eq!(catalog[&error.kind].as_str(), Some(error.message.as_str()));
        }
        let error = super::Error::network("never forward raw detail");
        assert_eq!(catalog[&error.kind].as_str(), Some(error.message.as_str()));
    }

    #[test]
    fn structured_error_contract_redacts_raw_text_and_rejects_invalid_identifiers() {
        let fixture: serde_json::Value = serde_json::from_str(include_str!(
            "../../../internal/server/testdata/problems-v1.json"
        ))
        .unwrap();
        for row in fixture["errors"].as_array().unwrap() {
            let mut body = row.clone();
            body["error"] = serde_json::json!("synthetic-secret");
            body["hint"] = serde_json::json!("/private/operator/path");
            body["operation_id"] = serde_json::json!("0123456789abcdef0123456789abcdef");
            let status =
                reqwest::StatusCode::from_u16(row["http_status"].as_u64().unwrap() as u16).unwrap();
            let error = super::response_error(
                status,
                "application/json; charset=utf-8",
                &serde_json::to_vec(&body).unwrap(),
            );
            assert_eq!(error.code.as_deref(), row["code"].as_str());
            assert_eq!(error.retryable, row["retryable"].as_bool());
            let raw = serde_json::to_string(&error).unwrap();
            assert!(!raw.contains("synthetic-secret") && !raw.contains("/private/"));
        }
        for body in [
            r#"{"code":"invalid\ncode","operation_id":"private","retryable":true}"#,
            r#"{"code":5,"operation_id":null}"#,
            "<html>private</html>",
        ] {
            let error = super::response_error(
                reqwest::StatusCode::FORBIDDEN,
                "application/json",
                body.as_bytes(),
            );
            assert!(
                error.code.is_none() && error.operation_id.is_none() && error.retryable.is_none()
            );
        }
        let old: super::Session = serde_json::from_str(
            r#"{"id":"s1","name":"fixture","agent":"codex","status":"stopped"}"#,
        )
        .unwrap();
        assert!(old.stop_reason.is_empty());
    }
    use super::*;

    #[test]
    fn attachment_paths_never_inject_terminal_control_or_shell_text() {
        for path in ["/shared/.file/a.bin", "/shared/.images/2026-screenshot.png"] {
            assert!(super::attachment_path_valid(path));
        }
        for path in [
            "/etc/passwd",
            "/shared/.file/../a",
            "/shared/.file/$(pwd)",
            "/shared/.file/a\nwhoami",
            "/shared/.file/a\u{1b}[31m",
            "/shared/.images/",
        ] {
            assert!(!super::attachment_path_valid(path));
        }
    }
    #[test]
    fn capability_fallback_does_not_hide_failures() {
        assert!(
            classify_capabilities(StatusCode::NOT_FOUND, "text/plain", b"")
                .unwrap()
                .is_none()
        );
        assert!(
            classify_capabilities(StatusCode::OK, "text/html; charset=utf-8", b"<html>")
                .unwrap()
                .is_none()
        );
        for status in [
            StatusCode::UNAUTHORIZED,
            StatusCode::FORBIDDEN,
            StatusCode::BAD_GATEWAY,
            StatusCode::FOUND,
        ] {
            assert!(classify_capabilities(status, "text/html", b"").is_err());
        }
        for body in [b"{}".as_slice(), b"{\"protocol_version\":2,\"features\":{\"pairing\":0,\"project_terminals\":0,\"sync\":0}}"] {
            assert!(classify_capabilities(StatusCode::OK, "application/json", body).is_err());
        }
        assert!(classify_capabilities(StatusCode::OK, "application/json", b"{\"protocol_version\":1,\"features\":{\"pairing\":0,\"project_terminals\":0,\"sync\":0}}").unwrap().is_some());
    }

    #[test]
    fn addresses_and_ids_cannot_change_native_route_or_leak_credentials() {
        assert_eq!(
            normalize_server("https://EXAMPLE.com/box", false)
                .unwrap()
                .as_str(),
            "https://example.com/box/"
        );
        for value in [
            "file:///tmp/a",
            "https://u:p@example.com",
            "https://example.com/?token=a",
            "https://example.com/#x",
            "https://example.com/%2f",
            "http://example.com",
        ] {
            assert!(normalize_server(value, false).is_err(), "{value}");
        }
        assert!(normalize_server("http://127.0.0.1:8080", false).is_ok());
        assert!(normalize_server("http://example.com", true).is_ok());
        for value in ["", "../me", "x?token=a", "x/y", "x#y"] {
            assert!(valid_session_id(value).is_err());
        }
    }
}
