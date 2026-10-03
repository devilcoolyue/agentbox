//! Files may enter native storage only through an OS picker, clipboard or drop.
//! The renderer gets expiring single-use tickets, never a path-reading command.
use crate::remote::{Error, Remote, Result};
use crate::Desktop;
use clipboard_rs::{Clipboard, ClipboardContext, ContentFormat};
use serde::Serialize;
use std::{
    collections::HashMap,
    fs::File,
    io::Read,
    path::PathBuf,
    sync::Arc,
    time::{Duration, Instant},
};
use tauri::{Emitter, Manager, State};

#[path = "attachments/clipboard_image.rs"]
mod clipboard_image;

pub const MAX_BYTES: u64 = 19 * 1024 * 1024;
const MAX_FILES: usize = 32;
const MAX_BATCH_BYTES: u64 = 64 * 1024 * 1024;

pub enum Source {
    File(File, std::fs::Metadata),
    Image(Vec<u8>),
}
pub struct Grant {
    remote: Arc<Remote>,
    source: Source,
    pub name: String,
    expires: Instant,
}
#[derive(Default)]
pub struct Grants {
    next: u64,
    items: HashMap<String, Grant>,
}
#[derive(Clone, Serialize, serde::Deserialize)]
pub struct SelectedFile {
    pub ticket: String,
    pub name: String,
    pub size: u64,
}
#[derive(Serialize)]
pub struct ClipboardResult {
    pub text: Option<String>,
    pub files: Vec<SelectedFile>,
}
#[derive(Clone, Serialize)]
struct DroppedFiles {
    files: Vec<SelectedFile>,
    x: f64,
    y: f64,
}

fn local_error() -> Error {
    Error::new("file", "无法读取文件；仅支持本机普通文件，每个最多 19 MiB")
}

fn open_selected(path: PathBuf) -> Result<(Source, String, u64)> {
    // Resolve native user selection once, then hold the opened handle for the
    // entire ticket lifetime. The filename is display/multipart metadata only.
    let before = std::fs::symlink_metadata(&path).map_err(|_| local_error())?;
    if !before.is_file() || before.file_type().is_symlink() || before.len() > MAX_BYTES {
        return Err(local_error());
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        if before.file_attributes() & 0x400 != 0 {
            return Err(local_error());
        }
    }
    let file = File::open(&path).map_err(|_| local_error())?;
    let meta = file.metadata().map_err(|_| local_error())?;
    if !meta.is_file()
        || meta.len() != before.len()
        || meta.modified().ok() != before.modified().ok()
    {
        return Err(local_error());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        if meta.ino() != before.ino() || meta.dev() != before.dev() || meta.nlink() != 1 {
            return Err(local_error());
        }
    }
    #[cfg(windows)]
    {
        use std::os::windows::io::AsRawHandle;
        use windows_sys::Win32::Storage::FileSystem::{
            GetFileInformationByHandle, BY_HANDLE_FILE_INFORMATION,
        };
        let mut information: BY_HANDLE_FILE_INFORMATION = unsafe { std::mem::zeroed() };
        if unsafe { GetFileInformationByHandle(file.as_raw_handle(), &mut information) } == 0
            || information.nNumberOfLinks != 1
            || information.dwFileAttributes & 0x400 != 0
        {
            return Err(local_error());
        }
    }
    let name = path
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or_else(local_error)?
        .to_owned();
    let size = meta.len();
    Ok((Source::File(file, meta), name, size))
}

impl Grants {
    fn insert(
        &mut self,
        remote: Arc<Remote>,
        sources: Vec<(Source, String, u64)>,
    ) -> Result<Vec<SelectedFile>> {
        self.items
            .retain(|_, item| item.expires > Instant::now() && Arc::ptr_eq(&item.remote, &remote));
        if sources.len() + self.items.len() > MAX_FILES {
            return Err(Error::new(
                "limit",
                "最多选择 32 个附件；请先上传、取消或等待旧选择过期",
            ));
        }
        let used: u64 = self
            .items
            .values()
            .map(|g| match &g.source {
                Source::File(_, m) => m.len(),
                Source::Image(b) => b.len() as u64,
            })
            .sum();
        if sources.iter().map(|s| s.2).sum::<u64>() + used > MAX_BATCH_BYTES {
            return Err(Error::new("limit", "所选附件总计不得超过 64 MiB"));
        }
        let mut result = Vec::new();
        for (source, name, size) in sources {
            if size > MAX_BYTES {
                return Err(local_error());
            }
            self.next += 1;
            let ticket = self.next.to_string();
            result.push(SelectedFile {
                ticket: ticket.clone(),
                name: name.clone(),
                size,
            });
            self.items.insert(
                ticket,
                Grant {
                    remote: remote.clone(),
                    source,
                    name,
                    expires: Instant::now() + Duration::from_secs(120),
                },
            );
        }
        Ok(result)
    }
    pub fn clear(&mut self) {
        self.items.clear();
    }
    pub fn take(&mut self, ticket: &str, remote: &Arc<Remote>) -> Result<Grant> {
        let grant = self
            .items
            .remove(ticket)
            .ok_or_else(|| Error::new("expired", "附件选择已过期，请重新选择"))?;
        if grant.expires <= Instant::now() || !Arc::ptr_eq(&grant.remote, remote) {
            return Err(Error::new("expired", "附件选择或登录已变化"));
        }
        Ok(grant)
    }
}
impl Grant {
    pub fn read(self) -> Result<(String, Vec<u8>)> {
        match self.source {
            Source::Image(bytes) => Ok((self.name, bytes)),
            Source::File(mut file, before) => {
                let mut bytes = Vec::with_capacity(before.len() as usize);
                file.by_ref()
                    .take(MAX_BYTES + 1)
                    .read_to_end(&mut bytes)
                    .map_err(|_| local_error())?;
                let after = file.metadata().map_err(|_| local_error())?;
                if bytes.len() as u64 != before.len()
                    || after.len() != before.len()
                    || after.modified().ok() != before.modified().ok()
                {
                    return Err(Error::new("changed", "附件读取期间发生变化，请重新选择"));
                }
                Ok((self.name, bytes))
            }
        }
    }
}

async fn current(state: &Desktop) -> Result<Arc<Remote>> {
    let data = state.inner.lock().await;
    if data.disconnecting {
        return Err(Error::new("state", "正在退出登录"));
    }
    data.remote
        .clone()
        .ok_or_else(|| Error::new("unauthorized", "请先登录"))
}
async fn register(
    state: &Desktop,
    remote: Arc<Remote>,
    sources: Vec<(Source, String, u64)>,
) -> Result<Vec<SelectedFile>> {
    let mut data = state.inner.lock().await;
    if data.disconnecting
        || !data
            .remote
            .as_ref()
            .is_some_and(|r| Arc::ptr_eq(r, &remote))
    {
        return Err(Error::new("state", "登录已变化"));
    }
    data.attachments.insert(remote, sources)
}
fn open_many(paths: Vec<PathBuf>) -> Result<Vec<(Source, String, u64)>> {
    if paths.len() > MAX_FILES {
        return Err(Error::new("limit", "每次最多选择 32 个文件"));
    }
    paths.into_iter().map(open_selected).collect()
}
#[tauri::command]
pub async fn choose_attachments(
    app: tauri::AppHandle,
    state: State<'_, Desktop>,
) -> Result<Vec<SelectedFile>> {
    let _selection = state
        .attachment_selection
        .try_lock()
        .map_err(|_| Error::new("busy", "文件选择正在进行"))?;
    let remote = current(&state).await?;
    #[cfg(feature = "desktop-smoke")]
    let paths = vec![crate::smoke::upload_selection(&app).map_err(|_| local_error())?];
    #[cfg(not(feature = "desktop-smoke"))]
    let paths = {
        let _ = app;
        let Some(files) = rfd::AsyncFileDialog::new()
            .set_title("选择附件（每个最多 19 MiB）")
            .pick_files()
            .await
        else {
            return Ok(Vec::new());
        };
        files.into_iter().map(|f| f.path().to_path_buf()).collect()
    };
    let sources = tauri::async_runtime::spawn_blocking(move || open_many(paths))
        .await
        .map_err(Error::network)??;
    register(&state, remote, sources).await
}
#[tauri::command]
pub async fn read_attachment_clipboard(state: State<'_, Desktop>) -> Result<ClipboardResult> {
    let _selection = state
        .attachment_selection
        .try_lock()
        .map_err(|_| Error::new("busy", "剪贴板读取正在进行"))?;
    let remote = current(&state).await?;
    let (text, sources) = tauri::async_runtime::spawn_blocking(|| {
        let clipboard =
            ClipboardContext::new().map_err(|_| Error::new("clipboard", "无法读取系统剪贴板"))?;
        if clipboard.has(ContentFormat::Files) {
            let files = clipboard
                .get_files()
                .map_err(|_| Error::new("clipboard", "无法读取剪贴板文件列表"))?;
            return Ok((
                None,
                open_many(files.into_iter().map(PathBuf::from).collect())?,
            ));
        }
        if let Some(bytes) = clipboard_image::read()? {
            let size = bytes.len() as u64;
            return Ok((
                None,
                vec![(Source::Image(bytes), "screenshot.png".into(), size)],
            ));
        }
        let text = clipboard
            .get_text()
            .map_err(|_| Error::new("clipboard", "剪贴板没有文本、图片或文件"))?;
        if text.len() > 256 * 1024 {
            return Err(Error::new("limit", "剪贴板文本超过 256 KiB"));
        }
        Ok((Some(text), Vec::new()))
    })
    .await
    .map_err(Error::network)??;
    Ok(ClipboardResult {
        text,
        files: register(&state, remote, sources).await?,
    })
}
#[tauri::command]
pub async fn release_attachments(state: State<'_, Desktop>, tickets: Vec<String>) -> Result<()> {
    if tickets.len() > MAX_FILES {
        return Err(Error::new("limit", "附件选择无效"));
    }
    let mut data = state.inner.lock().await;
    for ticket in tickets {
        data.attachments.items.remove(&ticket);
    }
    Ok(())
}

pub fn dropped(window: &tauri::Window, paths: Vec<PathBuf>, x: f64, y: f64) {
    let app = window.app_handle().clone();
    // The native drop point uses physical pixels; DOM bounds use zoomed CSS pixels.
    let scale = window.scale_factor().unwrap_or(1.0)
        * app.state::<crate::ui_preferences::ZoomState>().factor();
    tauri::async_runtime::spawn(async move {
        let state = app.state::<Desktop>();
        let Ok(_selection) = state.attachment_selection.try_lock() else {
            return;
        };
        let Ok(remote) = current(&state).await else {
            return;
        };
        let sources = tauri::async_runtime::spawn_blocking(move || open_many(paths)).await;
        let result = match sources {
            Ok(Ok(sources)) => register(&state, remote, sources).await,
            Ok(Err(error)) => Err(error),
            Err(_) => Err(local_error()),
        };
        match result {
            Ok(files) => {
                let _ = app.emit_to(
                    "main",
                    "attachment-drop",
                    DroppedFiles {
                        files,
                        x: x / scale,
                        y: y / scale,
                    },
                );
            }
            Err(error) => {
                let _ = app.emit_to("main", "attachment-drop-error", error);
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    fn remote() -> Arc<Remote> {
        Arc::new(
            Remote::new(
                crate::remote::normalize_server("http://localhost", false).unwrap(),
                "test-token".into(),
            )
            .unwrap(),
        )
    }
    #[test]
    fn tickets_are_single_use_and_tied_to_connection() {
        let mut grants = Grants::default();
        let owner = remote();
        let files = grants
            .insert(
                owner.clone(),
                vec![(Source::Image(vec![1, 2]), "a.png".into(), 2)],
            )
            .unwrap();
        assert!(grants.take(&files[0].ticket, &remote()).is_err());
        assert!(grants.take(&files[0].ticket, &owner).is_err());
        let files = grants
            .insert(
                owner.clone(),
                vec![(Source::Image(vec![3]), "b.png".into(), 1)],
            )
            .unwrap();
        assert_eq!(
            grants
                .take(&files[0].ticket, &owner)
                .unwrap()
                .read()
                .unwrap()
                .1,
            vec![3]
        );
        assert!(grants.take(&files[0].ticket, &owner).is_err());
    }
    #[test]
    fn capacity_and_expiry_do_not_authorize_extra_sources() {
        let mut grants = Grants::default();
        let owner = remote();
        let files = grants
            .insert(
                owner.clone(),
                vec![(Source::Image(vec![1]), "a.png".into(), 1)],
            )
            .unwrap();
        grants.items.get_mut(&files[0].ticket).unwrap().expires =
            Instant::now() - Duration::from_secs(1);
        assert!(grants.take(&files[0].ticket, &owner).is_err());
        let sources = (0..33)
            .map(|_| (Source::Image(vec![1]), "b.png".into(), 1))
            .collect();
        assert!(grants.insert(owner, sources).is_err());
        assert!(grants.items.is_empty());
    }
}
