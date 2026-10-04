use super::*;
use flate2::{write::GzEncoder, Compression};
use std::os::unix::fs::PermissionsExt;

const ID: &str = "io.github.agentbox.synthetic-updater-test";
fn bundle(parent: &Path, name: &str, version: &str, marker: &str) -> PathBuf {
    let root = parent.join(name);
    fs::create_dir_all(root.join("Contents/MacOS")).unwrap();
    fs::create_dir_all(root.join("Contents/Resources/third-party")).unwrap();
    let mut fields = plist::Dictionary::new();
    for (key, value) in [
        ("CFBundleIdentifier", ID),
        ("CFBundleShortVersionString", version),
        ("CFBundleExecutable", EXECUTABLE),
    ] {
        fields.insert(key.into(), plist::Value::String(value.into()));
    }
    plist::Value::Dictionary(fields)
        .to_file_xml(root.join("Contents/Info.plist"))
        .unwrap();
    let cpu = if cfg!(target_arch = "aarch64") {
        0x0100000c_u32
    } else {
        0x01000007_u32
    };
    let mut binary = vec![0xcf, 0xfa, 0xed, 0xfe];
    binary.extend(cpu.to_le_bytes());
    binary.extend(marker.as_bytes());
    for executable in [EXECUTABLE, "abox-sync"] {
        let file = root.join("Contents/MacOS").join(executable);
        fs::write(&file, &binary).unwrap();
        fs::set_permissions(file, fs::Permissions::from_mode(0o755)).unwrap();
    }
    fs::write(
        root.join("Contents/Resources/third-party/NOTICES.txt"),
        marker,
    )
    .unwrap();
    fs::write(
        root.join("Contents/Resources/third-party/inventory.json"),
        b"[]",
    )
    .unwrap();
    root
}
fn archive(version: &str, marker: &str) -> Vec<u8> {
    let source = tempfile::tempdir().unwrap();
    let app = bundle(source.path(), "Agentbox.app", version, marker);
    let compressed = GzEncoder::new(Vec::new(), Compression::fast());
    let mut tar = tar::Builder::new(compressed);
    tar.append_dir_all("Agentbox.app", &app).unwrap();
    tar.into_inner().unwrap().finish().unwrap()
}
fn marker(bundle: &Path) -> String {
    fs::read_to_string(bundle.join("Contents/Resources/third-party/NOTICES.txt")).unwrap()
}
fn recoveries(parent: &Path) -> Vec<PathBuf> {
    fs::read_dir(parent)
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .filter(|path| {
            path.file_name()
                .unwrap()
                .to_string_lossy()
                .starts_with(STAGING_PREFIX)
        })
        .collect()
}

#[test]
fn complete_atomic_bundle_exchange_retains_old_app_until_next_explicit_update() {
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox user's 中文.app", "1.0.0", "old");
    let old_id = identity(&target).unwrap();
    let recovery = install_at(
        &archive("2.0.0", "new"),
        &target,
        ID,
        "1.0.0",
        "2.0.0",
        |_| Ok(()),
    )
    .unwrap();
    assert_eq!(marker(&target), "new");
    assert_eq!(fs::metadata(&recovery).unwrap().mode() & 0o777, 0o700);
    assert_eq!(marker(&recovery.join("Agentbox.app")), "old");
    assert_eq!(identity(&recovery.join("Agentbox.app")).unwrap(), old_id);
    validate_bundle(&target, ID, "2.0.0").unwrap();
    // An old running binary cannot silently prune the previous app merely
    // because a new bundle has appeared at the same launch path.
    assert!(cleanup_previous(&target, ID, "1.0.0").is_err());
    assert!(recovery.exists());
    cleanup_previous(&target, ID, "2.0.0").unwrap();
    assert!(!recovery.exists());
    assert_eq!(marker(&target), "new");
}

#[test]
fn staged_and_post_exchange_io_failures_keep_a_complete_old_application() {
    for point in [
        "after_extract",
        "before_exchange",
        "after_exchange",
        "after_sync",
    ] {
        for code in [libc::ENOSPC, libc::EACCES, libc::EIO] {
            let root = tempfile::tempdir().unwrap();
            let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
            let before = identity(&target).unwrap();
            let result = install_at(
                &archive("2.0.0", "new"),
                &target,
                ID,
                "1.0.0",
                "2.0.0",
                |stage| {
                    if stage == point {
                        Err(io::Error::from_raw_os_error(code))
                    } else {
                        Ok(())
                    }
                },
            );
            assert!(result.is_err(), "{point}/{code}");
            assert_eq!(identity(&target).unwrap(), before, "{point}/{code}");
            assert_eq!(marker(&target), "old", "{point}/{code}");
            assert!(recoveries(root.path()).is_empty(), "{point}/{code}");
        }
    }
}

#[test]
fn failed_atomic_rollback_retains_both_complete_bundles_and_receipt() {
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
    let result = install_at(
        &archive("2.0.0", "new"),
        &target,
        ID,
        "1.0.0",
        "2.0.0",
        |stage| match stage {
            "after_exchange" => Err(io::Error::from_raw_os_error(libc::ENOSPC)),
            "before_rollback" => Err(io::Error::from_raw_os_error(libc::EACCES)),
            _ => Ok(()),
        },
    );
    assert!(result.is_err());
    assert_eq!(marker(&target), "new");
    let recovery = recoveries(root.path());
    assert_eq!(recovery.len(), 1);
    assert_eq!(marker(&recovery[0].join("Agentbox.app")), "old");
    let receipt: Receipt =
        serde_json::from_slice(&fs::read(recovery[0].join("receipt.json")).unwrap()).unwrap();
    assert_eq!(Some(identity(&target).unwrap()), receipt.after);
    assert_eq!(
        identity(&recovery[0].join("Agentbox.app")).unwrap(),
        receipt.before
    );
}

#[test]
fn real_exchange_of_missing_source_and_changed_target_never_removes_user_bundle() {
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
    assert!(exchange(&target, &root.path().join("missing.app")).is_err());
    assert_eq!(marker(&target), "old");
    let moved = root.path().join("manually-moved.app");
    let result = install_at(
        &archive("2.0.0", "new"),
        &target,
        ID,
        "1.0.0",
        "2.0.0",
        |stage| {
            if stage == "before_exchange" {
                fs::rename(&target, &moved)?;
                bundle(root.path(), "Agentbox.app", "1.0.0", "different app");
            }
            Ok(())
        },
    );
    assert!(result.is_err());
    assert_eq!(marker(&target), "different app");
    assert_eq!(marker(&moved), "old");
}

#[test]
fn identity_version_architecture_and_untrusted_archive_entries_are_rejected() {
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
    for (bytes, identifier, current, version) in [
        (archive("2.0.0", "wrong version"), ID, "1.0.0", "3.0.0"),
        (
            archive("2.0.0", "wrong app"),
            "another.application",
            "1.0.0",
            "2.0.0",
        ),
        (b"invalid gzip".to_vec(), ID, "1.0.0", "2.0.0"),
    ] {
        assert!(install_at(&bytes, &target, identifier, current, version, |_| Ok(())).is_err());
        assert_eq!(marker(&target), "old");
    }
    let fake = bundle(root.path(), "WrongCPU.app", "2.0.0", "candidate");
    fs::write(
        fake.join("Contents/MacOS").join(EXECUTABLE),
        b"wrong cpu executable",
    )
    .unwrap();
    assert!(validate_bundle(&fake, ID, "2.0.0").is_err());
    for (name, target) in [
        ("Agentbox.app/Contents/link", "/etc/passwd"),
        ("Agentbox.app/link", "../outside"),
    ] {
        assert!(validate_link(Path::new(name), Path::new(target)).is_err());
    }
    assert!(validate_link(
        Path::new("Agentbox.app/Contents/Frameworks/Foo"),
        Path::new("../Versions/A")
    )
    .is_ok());
    let compressed = GzEncoder::new(Vec::new(), Compression::fast());
    let mut tar = tar::Builder::new(compressed);
    let mut header = tar::Header::new_gnu();
    header.set_entry_type(tar::EntryType::Symlink);
    header.set_size(0);
    header.set_mode(0o777);
    header.set_link_name("/etc").unwrap();
    header.set_cksum();
    tar.append_data(&mut header, "Agentbox.app/Contents", io::empty())
        .unwrap();
    let malicious = tar.into_inner().unwrap().finish().unwrap();
    assert!(install_at(&malicious, &target, ID, "1.0.0", "2.0.0", |_| Ok(())).is_err());
    assert_eq!(marker(&target), "old");
}

#[test]
fn foreign_or_replaced_recovery_directories_are_never_automatically_removed() {
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
    let recovery = install_at(
        &archive("2.0.0", "new"),
        &target,
        ID,
        "1.0.0",
        "2.0.0",
        |_| Ok(()),
    )
    .unwrap();
    fs::rename(
        recovery.join("Agentbox.app"),
        root.path().join("kept-old.app"),
    )
    .unwrap();
    bundle(&recovery, "Agentbox.app", "1.0.0", "foreign replacement");
    assert!(cleanup_previous(&target, ID, "2.0.0").is_err());
    assert_eq!(
        marker(&recovery.join("Agentbox.app")),
        "foreign replacement"
    );
    assert_eq!(marker(&root.path().join("kept-old.app")), "old");
}

#[test]
fn link_chains_cannot_escape_even_when_individual_targets_are_lexically_internal() {
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
    fs::create_dir_all(target.join("dir")).unwrap();
    fs::create_dir_all(target.join("outside")).unwrap();
    fs::create_dir_all(root.path().join("foreign")).unwrap();
    std::os::unix::fs::symlink("../outside", target.join("dir/alias")).unwrap();
    std::os::unix::fs::symlink("dir/alias/../../foreign", target.join("link")).unwrap();
    validate_link(
        Path::new("Agentbox.app/link"),
        Path::new("dir/alias/../../foreign"),
    )
    .unwrap();
    assert!(validate_bundle(&target, ID, "1.0.0").is_err());
    assert_eq!(marker(&target), "old");
}

#[test]
fn real_unwritable_install_parent_keeps_the_complete_current_application() {
    assert_ne!(
        unsafe { libc::geteuid() },
        0,
        "permission probe must run as an unprivileged user"
    );
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
    let bytes = archive("2.0.0", "new");
    fs::set_permissions(root.path(), fs::Permissions::from_mode(0o555)).unwrap();
    let result = install_at(&bytes, &target, ID, "1.0.0", "2.0.0", |_| Ok(()));
    fs::set_permissions(root.path(), fs::Permissions::from_mode(0o700)).unwrap();
    assert_eq!(result.unwrap_err().kind(), io::ErrorKind::PermissionDenied);
    assert_eq!(marker(&target), "old");
}

#[test]
#[ignore = "requires test-update-disk-full.py to provision an owned bounded disk image"]
fn real_disk_full_keeps_installed_bundle() {
    let root = PathBuf::from(
        std::env::var_os("AGENTBOX_UPDATE_FULL_ROOT")
            .expect("explicit bounded test volume required"),
    );
    assert_eq!(fs::canonicalize(&root).unwrap(), root);
    let entries = fs::read_dir(&root)
        .unwrap()
        .map(|entry| entry.unwrap().file_name())
        .collect::<Vec<_>>();
    assert_eq!(
        entries,
        [std::ffi::OsString::from(".agentbox-update-space-fixture")]
    );
    assert_eq!(
        fs::read(root.join(".agentbox-update-space-fixture")).unwrap(),
        b"owned-updater-enospc-volume-v1"
    );
    assert_ne!(
        fs::metadata(&root).unwrap().dev(),
        fs::metadata(std::env::temp_dir()).unwrap().dev(),
        "never fill the host temporary filesystem"
    );
    let encoded = CString::new(root.as_os_str().as_bytes()).unwrap();
    let mut volume: libc::statfs = unsafe { std::mem::zeroed() };
    assert_eq!(unsafe { libc::statfs(encoded.as_ptr(), &mut volume) }, 0);
    let capacity = (volume.f_bsize as u64)
        .checked_mul(volume.f_blocks)
        .unwrap();
    assert!(
        (8 * 1024 * 1024..=128 * 1024 * 1024).contains(&capacity),
        "only an owned 8–128 MiB fixture is allowed"
    );
    let target = bundle(&root, "Agentbox.app", "1.0.0", "old");
    let before = identity(&target).unwrap();
    let bytes = archive("2.0.0", &"new candidate".repeat(2 * 1024 * 1024));
    let mut filler = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(root.join("space-fixture-filler"))
        .unwrap();
    let chunk = [0xA5_u8; 64 * 1024];
    loop {
        if let Err(error) = filler.write_all(&chunk).and_then(|_| filler.sync_all()) {
            assert_eq!(error.raw_os_error(), Some(libc::ENOSPC));
            break;
        }
    }
    // Leave enough metadata/staging space to reach a real extraction write,
    // while the 25 MiB candidate cannot fit. No fake filesystem error is used.
    filler
        .set_len(filler.metadata().unwrap().len().saturating_sub(1024 * 1024))
        .unwrap();
    filler.sync_all().unwrap();
    let error = install_at(&bytes, &target, ID, "1.0.0", "2.0.0", |_| Ok(())).unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::StorageFull, "{error}");
    assert_eq!(identity(&target).unwrap(), before);
    assert_eq!(marker(&target), "old");
    println!("real_enospc_preserved_complete_old_bundle=true");
}

#[test]
fn interrupted_extraction_is_cleaned_only_with_original_app_and_owned_staging_identity() {
    let root = tempfile::tempdir().unwrap();
    let target = bundle(root.path(), "Agentbox.app", "1.0.0", "old");
    let staging = tempfile::Builder::new()
        .prefix(STAGING_PREFIX)
        .permissions(fs::Permissions::from_mode(0o700))
        .tempdir_in(root.path())
        .unwrap();
    let receipt = Receipt {
        target: "Agentbox.app".into(),
        identifier: ID.into(),
        before_version: "1.0.0".into(),
        after_version: "2.0.0".into(),
        bundle: String::new(),
        staging: identity(staging.path()).unwrap(),
        before: identity(&target).unwrap(),
        after: None,
    };
    write_receipt(staging.path(), &receipt).unwrap();
    fs::write(
        staging.path().join("incomplete-payload"),
        b"partially extracted bundle",
    )
    .unwrap();
    let path = staging.keep();
    cleanup_previous(&target, ID, "1.0.0").unwrap();
    assert!(!path.exists());
    assert_eq!(marker(&target), "old");
}

#[derive(serde::Deserialize, serde::Serialize)]
struct KillProbe {
    nonce: String,
    parent: u32,
    checkpoint: String,
}

struct OwnedKillChild {
    process: std::process::Child,
    reaped: bool,
}
impl Drop for OwnedKillChild {
    fn drop(&mut self) {
        // Child::kill/wait retain process ownership; never signal a PID read
        // from the filesystem or leave a timeout/panic child running.
        if !self.reaped {
            let _ = self.process.kill();
            let _ = self.process.wait();
        }
    }
}

#[test]
#[ignore = "private exact subprocess entry; requires a nonce and synthetic fixture"]
fn update_sigkill_child_entry() {
    let nonce = std::env::var("AGENTBOX_UPDATE_KILL_NONCE").expect("private test nonce required");
    assert!(
        nonce.len() == 32
            && nonce
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
    );
    let root = PathBuf::from(
        std::env::var_os("AGENTBOX_UPDATE_KILL_ROOT").expect("owned test directory required"),
    );
    assert_eq!(fs::canonicalize(&root).unwrap(), root);
    assert_eq!(
        root.parent().unwrap(),
        fs::canonicalize(std::env::temp_dir()).unwrap()
    );
    assert!(root
        .file_name()
        .unwrap()
        .to_string_lossy()
        .starts_with("agentbox-update-kill-"));
    let metadata = fs::symlink_metadata(&root).unwrap();
    assert!(metadata.is_dir() && !metadata.file_type().is_symlink());
    assert_eq!(metadata.uid(), unsafe { libc::geteuid() });
    assert_eq!(metadata.mode() & 0o777, 0o700);
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(root.join("control.json"))
        .unwrap();
    assert!(file.metadata().unwrap().is_file() && file.metadata().unwrap().len() <= 4096);
    let control: KillProbe = serde_json::from_reader(file).unwrap();
    assert_eq!(control.nonce, nonce);
    assert_eq!(control.parent, unsafe { libc::getppid() } as u32);
    assert!(matches!(
        control.checkpoint.as_str(),
        "after_extract" | "after_exchange"
    ));
    let target = root.join("Agentbox.app");
    validate_bundle(&target, ID, "1.0.0").unwrap();
    assert_eq!(marker(&target), "sigkill-old");
    let bytes = fs::read(root.join("candidate.tar.gz")).unwrap();
    assert!(
        bytes.len() < 1024 * 1024,
        "only the tiny synthetic archive is accepted"
    );
    install_at(&bytes, &target, ID, "1.0.0", "2.0.0", |checkpoint| {
        if checkpoint == control.checkpoint {
            let mut ready = OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(root.join("ready"))?;
            ready.write_all(format!("{}:{}", nonce, checkpoint).as_bytes())?;
            ready.sync_all()?;
            loop {
                // If the parent itself disappears, this private test process
                // must not outlive the runner even without its Drop cleanup.
                assert_eq!(control.parent, unsafe { libc::getppid() } as u32);
                std::thread::park_timeout(std::time::Duration::from_millis(50));
            }
        }
        Ok(())
    })
    .unwrap();
    panic!("child never reached its requested update checkpoint");
}

fn assert_kernel_kill_recovery(checkpoint: &str) {
    use std::os::unix::process::ExitStatusExt;
    use std::process::{Command, Stdio};
    use std::time::{Duration, Instant};

    let temporary = tempfile::Builder::new()
        .prefix("agentbox-update-kill-")
        .permissions(fs::Permissions::from_mode(0o700))
        .tempdir()
        .unwrap();
    let root = fs::canonicalize(temporary.path()).unwrap();
    let target = bundle(&root, "Agentbox.app", "1.0.0", "sigkill-old");
    sync_tree(&target).unwrap();
    let original = identity(&target).unwrap();
    let mut entropy = [0u8; 16];
    File::open("/dev/urandom")
        .unwrap()
        .read_exact(&mut entropy)
        .unwrap();
    let nonce = entropy
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let control = KillProbe {
        nonce: nonce.clone(),
        parent: std::process::id(),
        checkpoint: checkpoint.into(),
    };
    let mut config = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(root.join("control.json"))
        .unwrap();
    config
        .write_all(&serde_json::to_vec(&control).unwrap())
        .unwrap();
    config.sync_all().unwrap();
    fs::write(
        root.join("candidate.tar.gz"),
        archive("2.0.0", "sigkill-new"),
    )
    .unwrap();
    let log = File::create(root.join("child.log")).unwrap();
    let child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "updates::macos::tests::update_sigkill_child_entry",
            "--ignored",
            "--nocapture",
            "--test-threads=1",
        ])
        .env("AGENTBOX_UPDATE_KILL_NONCE", &nonce)
        .env("AGENTBOX_UPDATE_KILL_ROOT", &root)
        .stdin(Stdio::null())
        .stdout(log.try_clone().unwrap())
        .stderr(log)
        .spawn()
        .unwrap();
    let mut owned = OwnedKillChild {
        process: child,
        reaped: false,
    };
    let pid = owned.process.id();
    assert_ne!(pid, std::process::id());
    let deadline = Instant::now() + Duration::from_secs(20);
    let expected = format!("{nonce}:{checkpoint}");
    loop {
        if fs::read(root.join("ready")).ok().as_deref() == Some(expected.as_bytes()) {
            break;
        }
        if let Some(status) = owned.process.try_wait().unwrap() {
            owned.reaped = true;
            let output = fs::read_to_string(root.join("child.log")).unwrap_or_default();
            panic!("update child exited before checkpoint {checkpoint}: {status}\n{output}");
        }
        assert!(
            Instant::now() < deadline,
            "update child did not reach {checkpoint}"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
    assert!(owned.process.try_wait().unwrap().is_none());
    owned.process.kill().unwrap();
    let status = owned.process.wait().unwrap();
    owned.reaped = true;
    assert_eq!(
        status.signal(),
        Some(libc::SIGKILL),
        "not a kernel SIGKILL: {status}"
    );
    drop(owned);

    let pending = recoveries(&root);
    assert_eq!(
        pending.len(),
        1,
        "killed transaction must retain its owned recovery directory"
    );
    let receipt: Receipt =
        serde_json::from_slice(&fs::read(pending[0].join("receipt.json")).unwrap()).unwrap();
    assert_eq!(identity(&pending[0]).unwrap(), receipt.staging);
    assert_eq!(receipt.before, original);
    let (current, expected_backup) = if checkpoint == "after_extract" {
        assert!(receipt.after.is_none());
        assert_eq!(identity(&target).unwrap(), original);
        assert_eq!(marker(&target), "sigkill-old");
        validate_bundle(&target, ID, "1.0.0").unwrap();
        validate_bundle(&pending[0].join("Agentbox.app"), ID, "2.0.0").unwrap();
        ("1.0.0", "sigkill-old")
    } else {
        assert_eq!(Some(identity(&target).unwrap()), receipt.after);
        assert_eq!(
            identity(&pending[0].join(&receipt.bundle)).unwrap(),
            original
        );
        assert_eq!(marker(&target), "sigkill-new");
        assert_eq!(marker(&pending[0].join(&receipt.bundle)), "sigkill-old");
        validate_bundle(&target, ID, "2.0.0").unwrap();
        validate_bundle(&pending[0].join(&receipt.bundle), ID, "1.0.0").unwrap();
        ("2.0.0", "sigkill-new")
    };
    let recovery = install_at(
        &archive("3.0.0", "sigkill-next"),
        &target,
        ID,
        current,
        "3.0.0",
        |_| Ok(()),
    )
    .unwrap();
    assert_eq!(marker(&target), "sigkill-next");
    validate_bundle(&target, ID, "3.0.0").unwrap();
    assert_eq!(marker(&recovery.join("Agentbox.app")), expected_backup);
    assert_eq!(recoveries(&root), vec![recovery.clone()]);
    let next_receipt: Receipt =
        serde_json::from_slice(&fs::read(recovery.join("receipt.json")).unwrap()).unwrap();
    assert_eq!(next_receipt.before_version, current);
    assert_eq!(next_receipt.after_version, "3.0.0");
    assert_ne!(next_receipt.staging, receipt.staging);
    println!("macos_update_kernel_sigkill={checkpoint}; pid={pid}; signal=9; next_explicit_update=passed");
}

#[test]
fn kernel_sigkill_after_extract_preserves_live_app_and_next_update_recovers() {
    assert_kernel_kill_recovery("after_extract");
}

#[test]
fn kernel_sigkill_after_exchange_preserves_both_apps_and_next_update_recovers() {
    assert_kernel_kill_recovery("after_exchange");
}
