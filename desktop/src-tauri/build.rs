fn main() {
    println!("cargo:rerun-if-env-changed=AGENTBOX_UPDATER_PUBLIC_KEY");
    let mut attributes = tauri_build::Attributes::new();
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows")
        && std::env::var("CARGO_CFG_TARGET_ENV").as_deref() == Ok("msvc")
    {
        // Tauri's resource compiler links its manifest into package binaries,
        // but Rust's lib-test executable also imports Common Controls v6 APIs.
        // Embed the same activation context in every linked executable, keeping
        // the resource compiler responsible for icons and version metadata.
        println!("cargo:rerun-if-changed=windows/app.manifest.xml");
        let manifest = std::path::PathBuf::from(std::env::var_os("CARGO_MANIFEST_DIR").unwrap())
            .join("windows/app.manifest.xml");
        println!("cargo:rustc-link-arg=/MANIFEST:EMBED");
        println!("cargo:rustc-link-arg=/MANIFESTINPUT:{}", manifest.display());
        attributes = attributes
            .windows_attributes(tauri_build::WindowsAttributes::new_without_app_manifest());
    }
    tauri_build::try_build(attributes).expect("failed to build desktop application resources");
}
