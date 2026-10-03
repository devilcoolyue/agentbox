//! Desktop-only update channel. Never query repository releases/latest, which
//! is reserved for the Linux server. No Agentbox credentials reach this client.
use crate::remote::{Error, Result};
use crate::Desktop;
use serde::Serialize;
use std::time::Duration;
use tauri::{AppHandle, State};
use tauri_plugin_updater::UpdaterExt;
use tokio::sync::Mutex;

pub const PUBLIC_KEY: Option<&str> = option_env!("AGENTBOX_UPDATER_PUBLIC_KEY");
const ENDPOINT: &str =
    "https://github.com/devilcoolyue/agentbox/releases/download/desktop-stable/latest.json";
const DOWNLOAD_LIMIT: usize = 512 * 1024 * 1024;
const DOWNLOAD_TIMEOUT: Duration = Duration::from_secs(120);

fn allowed_download(url: &url::Url, version: &str) -> bool {
    let prefix = format!("/devilcoolyue/agentbox/releases/download/desktop-v{version}/");
    url.scheme() == "https"
        && url.host_str() == Some("github.com")
        && url.port_or_known_default() == Some(443)
        && url.username().is_empty()
        && url.password().is_none()
        && url.query().is_none()
        && url.fragment().is_none()
        && url
            .path()
            .strip_prefix(&prefix)
            .is_some_and(|asset| !asset.is_empty() && !asset.contains('/') && !asset.contains('%'))
}

// Verification happens inside the pinned plugin's download. Only verified,
// bounded, complete bytes may reach the terminal-detach/install boundary.
async fn verified_download(
    update: &tauri_plugin_updater::Update,
    limit: usize,
    timeout: Duration,
) -> Result<Vec<u8>> {
    let (too_large, exceeded) = tokio::sync::oneshot::channel();
    let mut too_large = Some(too_large);
    let mut received = 0usize;
    let download = update.download(
        |chunk, total| {
            received = received.saturating_add(chunk);
            if received > limit || total.is_some_and(|total| total > limit as u64) {
                if let Some(signal) = too_large.take() {
                    let _ = signal.send(());
                }
            }
        },
        || {},
    );
    tokio::select! {
        biased;
        _=exceeded=>Err(Error::new("limit", "桌面更新包超过容量限制")),
        result=tokio::time::timeout(timeout, download)=> {
            let bytes = result.map_err(|_| Error::new("timeout", "桌面更新下载超时，请重新检查更新"))?
                .map_err(update_error)?;
            // A short stream may finish before select observes its limit signal.
            if bytes.len() > limit || received > limit {
                return Err(Error::new("limit", "桌面更新包超过容量限制"));
            }
            Ok(bytes)
        },
    }
}
#[derive(Default)]
pub struct UpdateState {
    pub candidate: Mutex<Option<tauri_plugin_updater::Update>>,
    pub work: Mutex<()>,
}
#[derive(Serialize)]
pub struct UpdateInfo {
    configured: bool,
    current: String,
    version: Option<String>,
    notes: Option<String>,
}
fn configured() -> bool {
    PUBLIC_KEY.is_some_and(|key| !key.trim().is_empty())
}
fn update_error(_: impl std::fmt::Display) -> Error {
    Error::new(
        "update",
        "桌面更新检查、签名验证或安装失败；请稍后重试或使用正式安装包",
    )
}
#[tauri::command]
pub async fn desktop_update_check(
    app: AppHandle,
    state: State<'_, UpdateState>,
) -> Result<UpdateInfo> {
    let _guard = state
        .work
        .try_lock()
        .map_err(|_| Error::new("busy", "更新正在进行"))?;
    let current = app.package_info().version.to_string();
    // A failed recheck must not leave an earlier candidate installable.
    *state.candidate.lock().await = None;
    if !configured() {
        return Ok(UpdateInfo {
            configured: false,
            current,
            version: None,
            notes: None,
        });
    }
    let updater = app
        .updater_builder()
        .pubkey(PUBLIC_KEY.unwrap())
        .endpoints(vec![ENDPOINT.parse().map_err(update_error)?])
        .map_err(update_error)?
        .timeout(DOWNLOAD_TIMEOUT)
        .build()
        .map_err(update_error)?;
    let candidate = updater.check().await.map_err(update_error)?;
    if let Some(update) = &candidate {
        if !allowed_download(&update.download_url, &update.version) {
            return Err(Error::new("update", "桌面更新来源不符合发布通道"));
        }
    }
    let result = UpdateInfo {
        configured: true,
        current,
        version: candidate.as_ref().map(|u| u.version.clone()),
        notes: candidate.as_ref().and_then(|u| u.body.clone()),
    };
    *state.candidate.lock().await = candidate;
    Ok(result)
}
#[tauri::command]
pub async fn desktop_update_install(
    app: AppHandle,
    state: State<'_, UpdateState>,
    desktop: State<'_, Desktop>,
    version: String,
) -> Result<()> {
    let _guard = state
        .work
        .try_lock()
        .map_err(|_| Error::new("busy", "更新正在进行"))?;
    // Require idle transfer engines; keep credentials so the updated application
    // can restore the same login. The UI explicitly confirms terminal detach.
    let _sync = desktop
        .sync_work
        .try_lock()
        .map_err(|_| Error::new("busy", "请先停止同步"))?;
    let _files = desktop
        .attachment_work
        .try_lock()
        .map_err(|_| Error::new("busy", "请先停止文件传输"))?;
    let mut candidate = state.candidate.lock().await;
    let update = candidate
        .as_ref()
        .ok_or_else(|| Error::new("update", "请先检查桌面更新"))?;
    if update.version != version {
        return Err(Error::new("update", "更新版本已变化，请重新检查"));
    }
    let bytes = verified_download(update, DOWNLOAD_LIMIT, DOWNLOAD_TIMEOUT).await?;
    {
        let mut data = desktop.inner.lock().await;
        if data.disconnecting {
            return Err(Error::new("state", "正在退出登录"));
        }
        data.disconnecting = true;
        data.terminals.clear();
        data.pending.clear();
        data.attachments.clear();
        data.file_upload = None;
    }
    if let Err(error) = update.install(bytes) {
        desktop.inner.lock().await.disconnecting = false;
        return Err(update_error(error));
    }
    *candidate = None;
    app.restart();
}

#[cfg(test)]
#[path = "updates_tests.rs"]
mod tests;
