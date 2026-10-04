#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    let args: Vec<_> = std::env::args_os().collect();
    if args.len() == 3 && args[1] == "--diagnostics-json" {
        if let Err(error) = agentbox_desktop::diagnostics::run(std::path::Path::new(&args[2])) {
            eprintln!("{error}");
            std::process::exit(1);
        }
        return;
    }
    agentbox_desktop::run();
}
