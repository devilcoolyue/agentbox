//! Legacy-compatible file browsing and explicitly selected downloads.
use crate::{
    remote::{Error, Result},
    Desktop,
};
use serde::{Deserialize, Serialize};
use std::io::Write;
use tauri::{ipc::Channel, State};
use tokio::sync::oneshot;

#[derive(Serialize, Deserialize)]
pub struct Entry {
    pub name: String,
    pub is_dir: bool,
    pub size: u64,
    pub mode: String,
    pub mtime: String,
}
#[derive(Clone, Serialize)]
pub struct DownloadProgress {
    pub bytes: u64,
    pub total: Option<u64>,
}

pub fn route(session: &str, path: &str, scope: &str, kind: &str) -> Result<String> {
    crate::remote::valid_session_id(session)?;
    if !matches!(scope, "workspace" | "shared")
        || path.len() > 4096
        || path.starts_with('/')
        || path.contains(['\\', '\0'])
        || path.split('/').any(|p| matches!(p, "." | ".."))
    {
        return Err(Error::new("request", "文件路径或范围无效"));
    }
    let query = url::form_urlencoded::Serializer::new(String::new())
        .append_pair("path", path)
        .append_pair("scope", scope)
        .append_pair("dl", "1")
        .finish();
    Ok(format!("api/sessions/{session}/{kind}?{query}"))
}
#[tauri::command]
pub async fn list_files(
    state: State<'_, Desktop>,
    session: String,
    path: String,
    scope: String,
) -> Result<Vec<Entry>> {
    let route = route(&session, &path, &scope, "files")?;
    let remote = state
        .inner
        .lock()
        .await
        .remote
        .clone()
        .ok_or_else(|| Error::new("unauthorized", "请先登录"))?;
    let entries: Vec<Entry> = remote.api(reqwest::Method::GET, &route, None).await?;
    if entries.len() > 10000
        || entries.iter().any(|e| {
            e.name.is_empty()
                || matches!(e.name.as_str(), "." | "..")
                || e.name.contains(['/', '\\', '\0'])
        })
    {
        return Err(Error::new("protocol", "文件列表超限或包含无效名称"));
    }
    Ok(entries)
}
#[tauri::command]
pub async fn download_file(
    state: State<'_, Desktop>,
    task: String,
    session: String,
    path: String,
    scope: String,
    events: Channel<DownloadProgress>,
) -> Result<bool> {
    let route = route(&session, &path, &scope, "file")?;
    if task.is_empty() || task.len() > 128 || path.is_empty() {
        return Err(Error::new("request", "下载请求无效"));
    }
    let _guard = state
        .attachment_work
        .try_lock()
        .map_err(|_| Error::new("busy", "已有文件正在传输"))?;
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
        let name = path.rsplit('/').next().unwrap_or("download");
        let Some(destination) = rfd::AsyncFileDialog::new()
            .set_title("保存文件（不覆盖已有文件）")
            .set_file_name(name)
            .save_file()
            .await
        else {
            return Ok(false);
        };
        if destination
            .path()
            .try_exists()
            .map_err(|_| Error::new("file", "无法检查保存位置"))?
        {
            return Err(Error::new("exists", "文件已存在，请选择新名称"));
        }
        let bytes = remote
            .download_bytes(&route, |bytes, total| {
                let _ = events.send(DownloadProgress { bytes, total });
            })
            .await?;
        // Publication is a short final step after the entire bounded response
        // arrived. Never open/truncate the destination or follow its symlink.
        let parent = destination
            .path()
            .parent()
            .ok_or_else(|| Error::new("file", "保存路径无效"))?;
        let mut temporary = tempfile::NamedTempFile::new_in(parent)
            .map_err(|_| Error::new("file", "无法创建下载临时文件"))?;
        temporary
            .write_all(&bytes)
            .map_err(|_| Error::new("file", "下载保存失败，请检查磁盘空间"))?;
        temporary
            .as_file()
            .sync_all()
            .map_err(|_| Error::new("file", "下载文件同步失败"))?;
        temporary
            .persist_noclobber(destination.path())
            .map_err(|_| Error::new("file", "无法保存下载；已有文件不会被覆盖"))?;
        Ok(true)
    };
    let result =
        tokio::select! {result=work=>result,_=canceled=>Err(Error::new("canceled","下载已取消"))};
    state.inner.lock().await.attachment_cancel = None;
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn paths_cannot_escape_or_inject_queries() {
        for path in ["../outside", "/etc/passwd", "a/./b", "a/../b", "C:\\file"] {
            assert!(route("s1", path, "workspace", "file").is_err());
        }
        assert!(route("s1", "file", "bogus", "file").is_err());
        assert_eq!(
            route("s1", "中文 a?scope=shared", "workspace", "file").unwrap(),
            "api/sessions/s1/file?path=%E4%B8%AD%E6%96%87+a%3Fscope%3Dshared&scope=workspace&dl=1"
        );
    }
}
