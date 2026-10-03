fn main() {
    println!("cargo:rerun-if-env-changed=AGENTBOX_UPDATER_PUBLIC_KEY");
    tauri_build::build()
}
