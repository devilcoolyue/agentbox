//! Confirm one native-selected source and a specific legacy server directory.
//! Legacy upload has no compare-and-swap: the last list check narrows but cannot
//! eliminate races with editors, and archive merge can partially succeed.
use crate::{
    attachments::SelectedFile,
    files::{route, DownloadProgress, Entry},
    remote::{Error, Remote, Result},
    Desktop,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    sync::Arc,
    time::{Duration, Instant},
};
use tauri::{ipc::Channel, State};
use tokio::sync::oneshot;

pub struct PendingUpload {
    remote: Arc<Remote>,
    session: String,
    path: String,
    scope: String,
    name: String,
    bytes: Vec<u8>,
    listing: String,
    confirmation: String,
    expires: Instant,
}
#[derive(Serialize)]
pub struct UploadPreview {
    pub confirmation: String,
    pub session: String,
    pub path: String,
    pub scope: String,
    pub name: String,
    pub size: usize,
    pub replaces: bool,
    pub archive: bool,
}
#[derive(Deserialize)]
pub struct PreviewRequest {
    session: String,
    path: String,
    scope: String,
    file: SelectedFile,
}
#[derive(Deserialize)]
pub struct ApplyRequest {
    task: String,
    confirmation: String,
}
#[derive(Serialize, Deserialize)]
pub struct UploadSummary {
    pub files: u64,
    pub mode: String,
}
fn archive(name: &str) -> bool {
    let name = name.to_lowercase();
    [".zip", ".tar.gz", ".tgz", ".tar"]
        .iter()
        .any(|suffix| name.ends_with(suffix))
}
fn digest_listing(mut entries: Vec<Entry>) -> Result<String> {
    if entries.len() > 10000 {
        return Err(Error::new("limit", "目标目录超过预览限制"));
    }
    entries.sort_by(|a, b| a.name.cmp(&b.name));
    let raw = serde_json::to_vec(&entries).map_err(|_| Error::new("protocol", "目标目录无效"))?;
    Ok(format!("{:x}", Sha256::digest(raw)))
}
async fn list(remote: &Remote, session: &str, path: &str, scope: &str) -> Result<Vec<Entry>> {
    remote
        .api(
            reqwest::Method::GET,
            &route(session, path, scope, "files")?,
            None,
        )
        .await
}
#[tauri::command]
pub async fn preview_file_upload(
    state: State<'_, Desktop>,
    request: PreviewRequest,
) -> Result<UploadPreview> {
    let _guard = state
        .attachment_work
        .try_lock()
        .map_err(|_| Error::new("busy", "已有文件正在传输"))?;
    route(&request.session, &request.path, &request.scope, "upload")?;
    let (remote, grant) = {
        let mut data = state.inner.lock().await;
        if data.disconnecting {
            return Err(Error::new("state", "正在断开连接"));
        }
        let remote = data
            .remote
            .clone()
            .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
        data.file_upload = None;
        let grant = data.attachments.take(&request.file.ticket, &remote)?;
        (remote, grant)
    };
    // Snapshot bytes under the originally opened handle. Renderer name/size
    // are never used as filesystem authority or multipart metadata.
    let (name, bytes) = tauri::async_runtime::spawn_blocking(move || grant.read())
        .await
        .map_err(Error::network)??;
    let entries = list(&remote, &request.session, &request.path, &request.scope).await?;
    let replaces = entries.iter().any(|e| e.name == name);
    let listing = digest_listing(entries)?;
    let confirmation = format!(
        "{:x}",
        Sha256::digest(
            serde_json::to_vec(&(
                &request.session,
                &request.path,
                &request.scope,
                &name,
                &listing,
                format!("{:x}", Sha256::digest(&bytes))
            ))
            .map_err(|_| Error::new("request", "上传预览无效"))?
        )
    );
    let preview = UploadPreview {
        confirmation: confirmation.clone(),
        session: request.session.clone(),
        path: request.path.clone(),
        scope: request.scope.clone(),
        archive: archive(&name),
        name: name.clone(),
        size: bytes.len(),
        replaces,
    };
    let mut data = state.inner.lock().await;
    if data.disconnecting
        || !data
            .remote
            .as_ref()
            .is_some_and(|r| Arc::ptr_eq(r, &remote))
    {
        return Err(Error::new("state", "登录已变化"));
    }
    data.file_upload = Some(PendingUpload {
        remote,
        session: request.session,
        path: request.path,
        scope: request.scope,
        name,
        bytes,
        listing,
        confirmation,
        expires: Instant::now() + Duration::from_secs(120),
    });
    Ok(preview)
}
#[tauri::command]
pub async fn apply_file_upload(
    state: State<'_, Desktop>,
    request: ApplyRequest,
    events: Channel<DownloadProgress>,
) -> Result<UploadSummary> {
    if request.task.is_empty() || request.task.len() > 128 {
        return Err(Error::new("request", "上传任务无效"));
    }
    let _sync = state
        .sync_work
        .try_lock()
        .map_err(|_| Error::new("busy", "请先停止文件同步"))?;
    let _guard = state
        .attachment_work
        .try_lock()
        .map_err(|_| Error::new("busy", "已有文件正在传输"))?;
    let (cancel, canceled) = oneshot::channel();
    let pending = {
        let mut data = state.inner.lock().await;
        if data.disconnecting {
            return Err(Error::new("state", "正在断开连接"));
        }
        let pending = data
            .file_upload
            .take()
            .ok_or_else(|| Error::new("stale", "请重新选择文件并预览"))?;
        if pending.confirmation != request.confirmation
            || pending.expires <= Instant::now()
            || !data
                .remote
                .as_ref()
                .is_some_and(|r| Arc::ptr_eq(r, &pending.remote))
        {
            return Err(Error::new("stale", "上传预览或登录已变化，请重新选择"));
        }
        data.attachment_cancel = Some((request.task, cancel));
        pending
    };
    let work = async {
        let current = list(
            &pending.remote,
            &pending.session,
            &pending.path,
            &pending.scope,
        )
        .await?;
        if digest_listing(current)? != pending.listing {
            return Err(Error::new("stale", "目标目录已变化，请重新选择文件并预览"));
        }
        let result: UploadSummary = pending
            .remote
            .upload_multipart(
                &route(&pending.session, &pending.path, &pending.scope, "upload")?,
                &pending.name,
                "application/octet-stream",
                pending.bytes,
                move |bytes, total| {
                    let _ = events.send(DownloadProgress {
                        bytes,
                        total: Some(total),
                    });
                },
            )
            .await?;
        if !matches!(result.mode.as_str(), "file" | "archive") || result.files > 100000 {
            return Err(Error::new(
                "protocol",
                "上传结果无效，请检查目标目录后再操作",
            ));
        }
        Ok(result)
    };
    let result = tokio::select! {result=work=>result,_=canceled=>Err(Error::new("canceled","上传已取消，目标可能已部分更新，请先检查目录；不会自动重试"))};
    state.inner.lock().await.attachment_cancel = None;
    result
}
#[tauri::command]
pub async fn discard_file_upload(state: State<'_, Desktop>, confirmation: String) -> Result<()> {
    let mut data = state.inner.lock().await;
    if data
        .file_upload
        .as_ref()
        .is_some_and(|p| p.confirmation == confirmation)
    {
        data.file_upload = None;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn entry(name: &str, size: u64) -> Entry {
        Entry {
            name: name.into(),
            size,
            is_dir: false,
            mode: "-rw-r--r--".into(),
            mtime: "2026-01-01".into(),
        }
    }
    #[test]
    fn preview_detects_metadata_changes_but_not_list_order() {
        let before = digest_listing(vec![entry("a", 1), entry("b", 2)]).unwrap();
        assert_eq!(
            before,
            digest_listing(vec![entry("b", 2), entry("a", 1)]).unwrap()
        );
        assert_ne!(
            before,
            digest_listing(vec![entry("a", 3), entry("b", 2)]).unwrap()
        );
    }
    #[test]
    fn archives_are_explicitly_marked_for_merge_warning() {
        for name in ["code.zip", "code.tar.gz", "code.tgz", "code.tar"] {
            assert!(archive(name));
        }
        for name in ["code.gz", "code.txt", "tar"] {
            assert!(!archive(name));
        }
    }
}
