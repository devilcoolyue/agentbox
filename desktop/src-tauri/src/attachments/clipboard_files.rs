//! Bound native file-list representations before allocating path collections.
use crate::remote::{Error, Result};
use std::path::PathBuf;

const MAX_PATH_UNITS: usize = 32_768;
const MAX_LIST_BYTES: usize = 2 * 1024 * 1024;

fn invalid() -> Error {
    Error::new("clipboard", "剪贴板文件列表无效，请重新复制或选择文件")
}
fn limit() -> Error {
    Error::new("limit", "剪贴板文件列表过长，每次最多选择 32 个文件")
}

#[cfg(any(windows, test))]
fn dropfiles(bytes: &[u8], ansi: impl Fn(&[u8]) -> Result<String>) -> Result<Vec<String>> {
    if bytes.len() > MAX_LIST_BYTES {
        return Err(limit());
    }
    if bytes.len() < 20 {
        return Err(invalid());
    }
    let start = u32::from_le_bytes(bytes[0..4].try_into().map_err(|_| invalid())?) as usize;
    let wide = u32::from_le_bytes(bytes[16..20].try_into().map_err(|_| invalid())?) != 0;
    let unit = if wide { 2 } else { 1 };
    if start < 20 || start >= bytes.len() || wide && !start.is_multiple_of(2) {
        return Err(invalid());
    }
    let mut paths = Vec::new();
    let mut position = start;
    loop {
        let begin = position;
        loop {
            let character = bytes.get(position..position + unit).ok_or_else(invalid)?;
            if character.iter().all(|value| *value == 0) {
                break;
            }
            position += unit;
            if position - begin > MAX_PATH_UNITS * 2 {
                return Err(limit());
            }
        }
        if position == begin {
            return if paths.is_empty() {
                Err(invalid())
            } else {
                Ok(paths)
            };
        }
        if paths.len() >= super::MAX_FILES {
            return Err(limit());
        }
        let path = if wide {
            let units: Vec<u16> = bytes[begin..position]
                .as_chunks::<2>()
                .0
                .iter()
                .map(|pair| u16::from_le_bytes([pair[0], pair[1]]))
                .collect();
            // Refuse malformed UTF-16 rather than opening a replacement-char
            // filename that the clipboard owner never actually selected.
            String::from_utf16(&units).map_err(|_| invalid())?
        } else {
            ansi(&bytes[begin..position])?
        };
        if path.encode_utf16().count() > MAX_PATH_UNITS {
            return Err(limit());
        }
        paths.push(path);
        position += unit;
    }
}

#[cfg(any(target_os = "macos", test))]
fn file_url_path(value: &str) -> Result<PathBuf> {
    if value.len() > MAX_PATH_UNITS * 4 {
        return Err(limit());
    }
    if value.bytes().any(|byte| byte.is_ascii_control()) {
        return Err(invalid());
    }
    if !value
        .get(..7)
        .is_some_and(|prefix| prefix.eq_ignore_ascii_case("file://"))
    {
        return Err(invalid());
    }
    let url = url::Url::parse(value).map_err(|_| invalid())?;
    if url.scheme() != "file"
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err(invalid());
    }
    let path = url.to_file_path().map_err(|_| invalid())?;
    if !path.is_absolute() || path.as_os_str().is_empty() || value.contains('\0') {
        return Err(invalid());
    }
    // Percent-encoded NUL is invalid too; no truncating C path conversion.
    #[cfg(unix)]
    {
        use std::os::unix::ffi::OsStrExt;
        if path.as_os_str().as_bytes().contains(&0) {
            return Err(invalid());
        }
    }
    #[cfg(windows)]
    {
        use std::os::windows::ffi::OsStrExt;
        if path.as_os_str().encode_wide().any(|unit| unit == 0) {
            return Err(invalid());
        }
    }
    Ok(path)
}

#[cfg(target_os = "macos")]
fn pasteboard_files(
    board: &objc2_app_kit::NSPasteboard,
    revision: isize,
) -> Result<Option<Vec<PathBuf>>> {
    use objc2_app_kit::NSPasteboardTypeFileURL;
    use objc2_foundation::NSRange;
    if board.changeCount() != revision {
        return Err(super::clipboard_native::changed());
    }
    let kind = unsafe { NSPasteboardTypeFileURL };
    let has_files = board
        .types()
        .is_some_and(|types| types.iter().any(|value| value.isEqualToString(kind)));
    if !has_files {
        if board.changeCount() != revision {
            return Err(super::clipboard_native::changed());
        }
        return Ok(None);
    }
    // AppKit may materialize promised objects before exposing their count.
    // Bound the count before any Rust path buffers or per-item conversions.
    let items = board.pasteboardItems().ok_or_else(invalid)?;
    if items.len() > super::MAX_FILES {
        return Err(limit());
    }
    let mut paths = Vec::new();
    let mut represented = 0usize;
    for item in items.iter() {
        if board.changeCount() != revision {
            return Err(super::clipboard_native::changed());
        }
        let Some(value) = item.stringForType(kind) else {
            continue;
        };
        let count = value.length();
        if count == 0 {
            return Err(invalid());
        }
        if count > MAX_PATH_UNITS {
            return Err(limit());
        }
        represented = represented.checked_add(count * 2).ok_or_else(limit)?;
        if represented > MAX_LIST_BYTES {
            return Err(limit());
        }
        let mut units = vec![0u16; count];
        // stringForType returns a retained immutable NSString snapshot. Read a
        // bounded UTF-16 range so invalid Unicode cannot be silently replaced.
        unsafe {
            value.getCharacters_range(
                std::ptr::NonNull::new(units.as_mut_ptr()).ok_or_else(invalid)?,
                NSRange::new(0, count),
            );
        }
        let value = String::from_utf16(&units).map_err(|_| invalid())?;
        paths.push(file_url_path(&value)?);
    }
    if board.changeCount() != revision {
        return Err(super::clipboard_native::changed());
    }
    if paths.is_empty() {
        return Err(invalid());
    }
    Ok(Some(paths))
}

#[cfg(target_os = "macos")]
pub(super) fn read() -> Result<Option<Vec<PathBuf>>> {
    objc2::rc::autoreleasepool(|_| {
        let board = objc2_app_kit::NSPasteboard::generalPasteboard();
        pasteboard_files(&board, board.changeCount())
    })
}

#[cfg(windows)]
fn ansi_path(bytes: &[u8]) -> Result<String> {
    use windows_sys::Win32::Globalization::{MultiByteToWideChar, CP_ACP, MB_ERR_INVALID_CHARS};
    let length = unsafe {
        MultiByteToWideChar(
            CP_ACP,
            MB_ERR_INVALID_CHARS,
            bytes.as_ptr(),
            bytes.len() as i32,
            std::ptr::null_mut(),
            0,
        )
    };
    if length <= 0 {
        return Err(invalid());
    }
    if length as usize > MAX_PATH_UNITS {
        return Err(limit());
    }
    let mut wide = vec![0; length as usize];
    if unsafe {
        MultiByteToWideChar(
            CP_ACP,
            MB_ERR_INVALID_CHARS,
            bytes.as_ptr(),
            bytes.len() as i32,
            wide.as_mut_ptr(),
            length,
        )
    } != length
    {
        return Err(invalid());
    }
    String::from_utf16(&wide).map_err(|_| invalid())
}

#[cfg(windows)]
unsafe fn global_files(handle: windows_sys::Win32::Foundation::HGLOBAL) -> Result<Vec<PathBuf>> {
    let names = unsafe {
        super::clipboard_native::with_global_bytes(handle, MAX_LIST_BYTES, |bytes| {
            dropfiles(bytes, ansi_path)
        })
    }?;
    names
        .into_iter()
        .map(|name| {
            let path = PathBuf::from(name);
            if path.is_absolute() {
                Ok(path)
            } else {
                Err(invalid())
            }
        })
        .collect()
}

#[cfg(windows)]
pub(super) fn read() -> Result<Option<Vec<PathBuf>>> {
    use windows_sys::Win32::System::DataExchange::{GetClipboardData, IsClipboardFormatAvailable};
    let _clipboard = super::clipboard_native::ClipboardGuard::open()?;
    let revision = super::clipboard_native::revision();
    if unsafe { IsClipboardFormatAvailable(15) } == 0 {
        // CF_HDROP.
        super::clipboard_native::ensure_current(revision)?;
        return Ok(None);
    }
    let paths = unsafe { global_files(GetClipboardData(15)) }?;
    super::clipboard_native::ensure_current(revision)?;
    Ok(Some(paths))
}

#[cfg(not(any(windows, target_os = "macos")))]
pub(super) fn read() -> Result<Option<Vec<PathBuf>>> {
    use clipboard_rs::{Clipboard, ClipboardContext, ContentFormat};
    let clipboard = ClipboardContext::new().map_err(|_| invalid())?;
    if clipboard.has(ContentFormat::Files) {
        return Err(Error::new(
            "clipboard",
            "此系统暂不支持文件粘贴，请选择文件",
        ));
    }
    Ok(None)
}

#[cfg(test)]
mod tests {
    use super::*;
    fn wide_drop(paths: &[&str]) -> Vec<u8> {
        let mut bytes = vec![0; 20];
        bytes[..4].copy_from_slice(&20u32.to_le_bytes());
        bytes[16..20].copy_from_slice(&1u32.to_le_bytes());
        for path in paths {
            for unit in path.encode_utf16().chain(Some(0)) {
                bytes.extend_from_slice(&unit.to_le_bytes());
            }
        }
        bytes.extend_from_slice(&[0, 0]);
        bytes
    }
    fn unused_ansi(_: &[u8]) -> Result<String> {
        panic!("wide format invoked ANSI decoding")
    }

    fn verify_native_paths_reach_single_use_ticket(paths: Vec<PathBuf>, expected: &[u8]) {
        use std::sync::Arc;
        let sources = super::super::open_many(paths).unwrap();
        let remote = Arc::new(
            crate::remote::Remote::new(
                crate::remote::normalize_server("http://localhost", false).unwrap(),
                "synthetic-clipboard-owner".into(),
            )
            .unwrap(),
        );
        let mut grants = super::super::Grants::default();
        let files = grants.insert(remote.clone(), sources).unwrap();
        assert_eq!(files.len(), 1);
        assert_eq!(files[0].name, "中文 😀 #.txt");
        let (_, bytes) = grants
            .take(&files[0].ticket, &remote)
            .unwrap()
            .read()
            .unwrap();
        assert_eq!(bytes, expected);
        assert!(grants.take(&files[0].ticket, &remote).is_err());
    }

    #[test]
    fn dropfiles_preserves_unicode_spaces_and_order() {
        let expected = [r"C:\文件\设计 😀.png", r"C:\work\name #%.txt"];
        assert_eq!(
            dropfiles(&wide_drop(&expected), unused_ansi).unwrap(),
            expected
        );
    }
    #[test]
    fn dropfiles_rejects_excess_count_lengths_and_malformed_representations() {
        assert_eq!(
            dropfiles(&wide_drop(&[r"C:\a"; 33]), unused_ansi)
                .unwrap_err()
                .kind,
            "limit"
        );
        assert_eq!(
            dropfiles(&wide_drop(&[&"x".repeat(MAX_PATH_UNITS + 1)]), unused_ansi)
                .unwrap_err()
                .kind,
            "limit"
        );
        assert_eq!(
            dropfiles(&vec![0; MAX_LIST_BYTES + 1], unused_ansi)
                .unwrap_err()
                .kind,
            "limit"
        );
        let mut missing_end = wide_drop(&[r"C:\a"]);
        missing_end.truncate(missing_end.len() - 2);
        assert!(dropfiles(&missing_end, unused_ansi).is_err());
        let mut bad_offset = wide_drop(&[r"C:\a"]);
        bad_offset[..4].copy_from_slice(&u32::MAX.to_le_bytes());
        assert!(dropfiles(&bad_offset, unused_ansi).is_err());
        let mut bad_unicode = wide_drop(&[r"C:\a"]);
        bad_unicode[20..22].copy_from_slice(&0xd800u16.to_le_bytes());
        assert!(dropfiles(&bad_unicode, unused_ansi).is_err());
        assert!(dropfiles(&wide_drop(&[]), unused_ansi).is_err());
    }
    #[test]
    fn file_urls_do_not_confuse_encoding_authority_or_shell_text() {
        #[cfg(windows)]
        let (url, expected) = (
            "file:///C:/work/%E4%B8%AD%E6%96%87%20%F0%9F%98%80%23.txt",
            PathBuf::from("C:\\work\\中文 😀#.txt"),
        );
        #[cfg(not(windows))]
        let (url, expected) = (
            "file:///tmp/%E4%B8%AD%E6%96%87%20%F0%9F%98%80%23.txt",
            PathBuf::from("/tmp/中文 😀#.txt"),
        );
        assert_eq!(file_url_path(url).unwrap(), expected);
        for invalid_url in [
            "https://example.com/a",
            "file:relative",
            "file:///tmp/a%00b",
            "file:///tmp/a?secret",
            "file:///tmp/a#fragment",
            "file:///tmp/a\nb",
        ] {
            assert!(file_url_path(invalid_url).is_err(), "{invalid_url}");
        }
        #[cfg(not(windows))]
        assert!(file_url_path("file://remote-host/share/file").is_err());
    }

    #[cfg(target_os = "macos")]
    fn set_files(board: &objc2_app_kit::NSPasteboard, files: &[&str]) {
        use objc2::runtime::ProtocolObject;
        use objc2_app_kit::{NSPasteboardItem, NSPasteboardTypeFileURL, NSPasteboardWriting};
        use objc2_foundation::{NSArray, NSString};
        board.clearContents();
        let objects: Vec<_> = files
            .iter()
            .map(|value| {
                let item = NSPasteboardItem::new();
                assert!(item.setString_forType(&NSString::from_str(value), unsafe {
                    NSPasteboardTypeFileURL
                }));
                ProtocolObject::<dyn NSPasteboardWriting>::from_retained(item)
            })
            .collect();
        assert!(board.writeObjects(&NSArray::from_retained_slice(&objects)));
    }
    #[cfg(target_os = "macos")]
    #[test]
    fn private_macos_pasteboard_file_contract() {
        objc2::rc::autoreleasepool(|_| {
            let private = super::super::clipboard_native::fixture::PrivatePasteboard::new();
            let board = &private.0;
            assert!(pasteboard_files(board, board.changeCount())
                .unwrap()
                .is_none());
            let directory = tempfile::tempdir().unwrap();
            let path = directory.path().join("中文 😀 #.txt");
            let original = b"native clipboard file\r\n\0";
            std::fs::write(&path, original).unwrap();
            let file_url = url::Url::from_file_path(&path).unwrap().to_string();
            set_files(board, &[&file_url]);
            verify_native_paths_reach_single_use_ticket(
                pasteboard_files(board, board.changeCount())
                    .unwrap()
                    .unwrap(),
                original,
            );
            set_files(
                board,
                &[
                    "file:///tmp/%E4%B8%AD%E6%96%87%20%F0%9F%98%80%23.txt",
                    "file:///tmp/second.txt",
                ],
            );
            let revision = board.changeCount();
            assert_eq!(
                pasteboard_files(board, revision).unwrap().unwrap(),
                [
                    PathBuf::from("/tmp/中文 😀#.txt"),
                    PathBuf::from("/tmp/second.txt")
                ]
            );
            set_files(board, &["file:///tmp/changed.txt"]);
            assert_eq!(
                pasteboard_files(board, revision).unwrap_err().kind,
                "changed"
            );
            set_files(board, &["file:///tmp/a"; 33]);
            assert_eq!(
                pasteboard_files(board, board.changeCount())
                    .unwrap_err()
                    .kind,
                "limit"
            );
            set_files(
                board,
                &[&format!("file:///tmp/{}", "a".repeat(MAX_PATH_UNITS))],
            );
            assert_eq!(
                pasteboard_files(board, board.changeCount())
                    .unwrap_err()
                    .kind,
                "limit"
            );
        });
    }
    #[cfg(windows)]
    #[test]
    fn windows_owned_hglobal_file_contract_without_system_clipboard() {
        use super::super::clipboard_native::fixture::GlobalData;
        use windows_sys::Win32::System::Memory::GlobalFlags;
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("中文 😀 #.txt");
        let original = b"native clipboard file\r\n\0";
        std::fs::write(&path, original).unwrap();
        let actual = GlobalData::new(&wide_drop(&[path.to_str().unwrap()]));
        verify_native_paths_reach_single_use_ticket(
            unsafe { global_files(actual.0) }.unwrap(),
            original,
        );
        let raw = wide_drop(&[r"C:\文件\设计 😀.png"]);
        let data = GlobalData::new(&raw);
        assert_eq!(
            unsafe { global_files(data.0) }.unwrap(),
            [PathBuf::from(r"C:\文件\设计 😀.png")]
        );
        assert_eq!(unsafe { GlobalFlags(data.0) } & 0xff, 0);
        let excessive = GlobalData::new(&wide_drop(&[r"C:\a"; 33]));
        assert_eq!(
            unsafe { global_files(excessive.0) }.unwrap_err().kind,
            "limit"
        );
        assert_eq!(unsafe { GlobalFlags(excessive.0) } & 0xff, 0);
        let relative = GlobalData::new(&wide_drop(&["relative.txt"]));
        assert!(unsafe { global_files(relative.0) }.is_err());
        let mut ansi = vec![0; 20];
        ansi[..4].copy_from_slice(&20u32.to_le_bytes());
        ansi.extend_from_slice(b"C:\\ASCII space.txt\0\0");
        assert_eq!(
            unsafe { global_files(GlobalData::new(&ansi).0) }.unwrap(),
            [PathBuf::from(r"C:\ASCII space.txt")]
        );
    }
}
