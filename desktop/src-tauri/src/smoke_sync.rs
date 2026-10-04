//! Native-only paths for the opt-in Go server/WebView integration fixture.
//! This module and its picker substitution do not exist in normal builds.
use serde::Deserialize;
use std::{
    path::PathBuf,
    sync::atomic::{AtomicBool, Ordering},
};
use tauri::State;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct SyncFixture {
    pub server: String,
    pub local: PathBuf,
    pub state: PathBuf,
    pub export: PathBuf,
    #[serde(skip)]
    overlap: AtomicBool,
}
impl SyncFixture {
    pub fn from_environment() -> Result<Option<Self>, Box<dyn std::error::Error>> {
        let Some(raw) = std::env::var_os("AGENTBOX_SMOKE_SYNC") else {
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
        {
            return Err("sync smoke requires a loopback fixture".into());
        }
        for path in [&config.local, &config.state, &config.export] {
            if !path.is_absolute() || !path.is_dir() {
                return Err("missing sync fixture directory".into());
            }
        }
        Ok(Some(config))
    }
    pub fn selected(&self, export: bool) -> PathBuf {
        if export && !self.overlap.load(Ordering::SeqCst) {
            self.export.clone()
        } else {
            self.local.clone()
        }
    }
}

#[tauri::command]
pub(crate) async fn smoke_sync_action(
    fixture: State<'_, super::smoke::Fixture>,
    action: String,
) -> Result<serde_json::Value, String> {
    let sync = fixture.sync.as_ref().ok_or("not a sync fixture")?;
    match action.as_str() {
        "export_overlap" => {
            sync.overlap.store(true, Ordering::SeqCst);
            return Ok(serde_json::Value::Null);
        }
        "export_safe" => {
            sync.overlap.store(false, Ordering::SeqCst);
            return Ok(serde_json::Value::Null);
        }
        "stale"
        | "lost"
        | "review_stale"
        | "review_restore"
        | "download"
        | "partial"
        | "block"
        | "blocked"
        | "verify"
        | "verify_remote_cleanup"
        | "orphan_seed"
        | "abandon_lost"
        | "history_seed"
        | "choices"
        | "continuous"
        | "continuous_done"
        | "continuous_conflict"
        | "continuous_restore" => (),
        _ => return Err("invalid sync fixture action".into()),
    }
    reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(std::time::Duration::from_secs(10))
        .build()
        .map_err(|_| "fixture client")?
        .post(format!("{}__smoke/{action}", sync.server))
        .bearer_auth("client-fixture-alice")
        .send()
        .await
        .map_err(|_| "fixture request")?
        .error_for_status()
        .map_err(|_| "fixture rejected action")?
        .json()
        .await
        .map_err(|_| "fixture response".into())
}
