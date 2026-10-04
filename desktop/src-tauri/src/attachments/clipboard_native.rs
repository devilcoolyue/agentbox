//! Native snapshot boundaries shared by file and image representations.
//! Tests use private pasteboards or their own HGLOBAL; never the user clipboard.
use crate::remote::{Error, Result};

pub(super) fn changed() -> Error {
    Error::new("changed", "剪贴板已变化，请重新粘贴")
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) struct Revision(u64);

#[cfg(target_os = "macos")]
pub(super) fn revision() -> Revision {
    objc2::rc::autoreleasepool(|_| {
        Revision(objc2_app_kit::NSPasteboard::generalPasteboard().changeCount() as u64)
    })
}
#[cfg(windows)]
pub(super) fn revision() -> Revision {
    Revision(unsafe {
        windows_sys::Win32::System::DataExchange::GetClipboardSequenceNumber() as u64
    })
}
#[cfg(not(any(windows, target_os = "macos")))]
pub(super) fn revision() -> Revision {
    Revision(0) // No native desktop support is claimed on other platforms.
}

pub(super) fn ensure_current(expected: Revision) -> Result<()> {
    if revision() != expected {
        return Err(changed());
    }
    Ok(())
}

#[cfg(windows)]
pub(super) struct ClipboardGuard;

#[cfg(windows)]
impl ClipboardGuard {
    pub(super) fn open() -> Result<Self> {
        if unsafe { windows_sys::Win32::System::DataExchange::OpenClipboard(std::ptr::null_mut()) }
            == 0
        {
            return Err(Error::new("clipboard", "系统剪贴板正忙，请重试"));
        }
        Ok(Self)
    }
}

#[cfg(windows)]
impl Drop for ClipboardGuard {
    fn drop(&mut self) {
        unsafe { windows_sys::Win32::System::DataExchange::CloseClipboard() };
    }
}

/// The handle must remain owned by the caller or pinned by an open clipboard
/// for this entire call. The callback cannot retain the borrowed byte slice.
#[cfg(windows)]
pub(super) unsafe fn with_global_bytes<T>(
    handle: windows_sys::Win32::Foundation::HGLOBAL,
    maximum: usize,
    consume: impl FnOnce(&[u8]) -> Result<T>,
) -> Result<T> {
    use windows_sys::Win32::System::Memory::{GlobalLock, GlobalSize, GlobalUnlock};
    if handle.is_null() {
        return Err(Error::new("clipboard", "剪贴板数据无效"));
    }
    let size = unsafe { GlobalSize(handle) };
    if size == 0 {
        return Err(Error::new("clipboard", "剪贴板数据为空"));
    }
    if size > maximum {
        return Err(Error::new("limit", "剪贴板内容过大，请分批粘贴"));
    }
    let pointer = unsafe { GlobalLock(handle) };
    if pointer.is_null() {
        return Err(Error::new("clipboard", "无法读取剪贴板数据"));
    }
    struct Locked(windows_sys::Win32::Foundation::HGLOBAL);
    impl Drop for Locked {
        fn drop(&mut self) {
            unsafe { GlobalUnlock(self.0) };
        }
    }
    let _locked = Locked(handle);
    // Size is bounded before borrowing or copying any native memory.
    let bytes = unsafe { std::slice::from_raw_parts(pointer.cast::<u8>(), size) };
    consume(bytes)
}

#[cfg(all(test, windows))]
pub(super) mod fixture {
    use windows_sys::Win32::{
        Foundation::{GlobalFree, HGLOBAL},
        System::Memory::{GlobalAlloc, GlobalLock, GlobalUnlock, GMEM_MOVEABLE, GMEM_ZEROINIT},
    };
    pub(crate) struct GlobalData(pub(crate) HGLOBAL);
    impl GlobalData {
        pub(crate) fn new(bytes: &[u8]) -> Self {
            let handle = unsafe { GlobalAlloc(GMEM_MOVEABLE | GMEM_ZEROINIT, bytes.len()) };
            assert!(!handle.is_null());
            let pointer = unsafe { GlobalLock(handle) };
            assert!(!pointer.is_null());
            unsafe {
                std::ptr::copy_nonoverlapping(bytes.as_ptr(), pointer.cast::<u8>(), bytes.len());
                GlobalUnlock(handle);
            }
            Self(handle)
        }
    }
    impl Drop for GlobalData {
        fn drop(&mut self) {
            unsafe { GlobalFree(self.0) };
        }
    }
}

#[cfg(all(test, target_os = "macos"))]
pub(super) mod fixture {
    use objc2::rc::Retained;
    use objc2_app_kit::NSPasteboard;
    pub(crate) struct PrivatePasteboard(pub(crate) Retained<NSPasteboard>);
    impl PrivatePasteboard {
        pub(crate) fn new() -> Self {
            Self(NSPasteboard::pasteboardWithUniqueName())
        }
    }
    impl Drop for PrivatePasteboard {
        fn drop(&mut self) {
            self.0.clearContents();
        }
    }
}
