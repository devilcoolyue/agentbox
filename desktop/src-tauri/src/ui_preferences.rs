use crate::remote::{Error, Result};
use std::sync::atomic::{AtomicU16, Ordering};
use tauri::{State, WebviewWindow};

pub struct ZoomState(AtomicU16);

impl Default for ZoomState {
    fn default() -> Self {
        Self(AtomicU16::new(100))
    }
}

impl ZoomState {
    pub fn factor(&self) -> f64 {
        f64::from(self.0.load(Ordering::Relaxed)) / 100.0
    }
}

fn allowed(percent: u16) -> bool {
    matches!(percent, 80 | 90 | 100 | 110 | 125 | 150 | 175 | 200)
}

// A bundled renderer may change only its own main WebView to an approved size.
// No generic window target, script, system command or filesystem API is exposed.
#[tauri::command]
pub fn desktop_set_zoom(
    window: WebviewWindow,
    state: State<'_, ZoomState>,
    percent: u16,
) -> Result<()> {
    if window.label() != "main" || !allowed(percent) {
        return Err(Error::new("request", "界面缩放比例无效"));
    }
    window
        .set_zoom(f64::from(percent) / 100.0)
        .map_err(|_| Error::new("zoom", "无法调整界面缩放，请重试"))?;
    state.0.store(percent, Ordering::Relaxed);
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_supported_zoom_levels_are_allowed() {
        for percent in [80, 90, 100, 110, 125, 150, 175, 200] {
            assert!(allowed(percent));
        }
        for percent in [0, 1, 79, 81, 201, u16::MAX] {
            assert!(!allowed(percent));
        }
        assert_eq!(ZoomState::default().factor(), 1.0);
    }
}
