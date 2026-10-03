//! Explicit offline installed-package probe for release validation. It never
//! reads credentials, connects to a server or mutates user preferences.
use serde_json::json;
use std::{io::Write, path::Path};

pub fn run(output: &Path) -> std::result::Result<(), String> {
    let executable = std::env::current_exe().map_err(|_| "cannot locate executable")?;
    let parent = executable.parent().ok_or("missing application directory")?;
    let sidecar = parent.join(if cfg!(windows) {
        "abox-sync.exe"
    } else {
        "abox-sync"
    });
    let resources = if cfg!(target_os = "macos") {
        parent.parent().ok_or("missing bundle")?.join("Resources")
    } else {
        parent.to_path_buf()
    };
    if !resources.join("third-party/NOTICES.txt").is_file()
        || !resources.join("third-party/inventory.json").is_file()
    {
        return Err("missing bundled license notices".into());
    }
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|_| "cannot create diagnostic runtime")?;
    runtime.block_on(async {
        let mut worker = crate::sidecar::Sidecar::start(&sidecar)
            .await
            .map_err(|_| "bundled sidecar failed to start")?;
        let result = worker.ping().await;
        worker.stop().await;
        result.map_err(|_| "bundled sidecar protocol failed")
    })?;
    let report = json!({"ok":true,"version":env!("CARGO_PKG_VERSION"),"os":std::env::consts::OS,"arch":std::env::consts::ARCH,"sidecar":true,"licenses":true,"development_tools_required":false});
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options
        .open(output)
        .map_err(|_| "report exists or cannot be created")?;
    file.write_all(serde_json::to_string_pretty(&report).unwrap().as_bytes())
        .map_err(|_| "cannot save diagnostic report")?;
    Ok(())
}
