//! Install already signature-verified macOS bytes without a missing-app window.
//! The destination comes only from the running executable. A complete old bundle
//! is retained until a later explicit update validates and retires that receipt.
use crate::remote::{Error, Result};
use flate2::read::GzDecoder;
use serde::{Deserialize, Serialize};
use std::{
    ffi::CString,
    fs::{self, File, OpenOptions},
    io::{self, Read, Write},
    os::unix::{ffi::OsStrExt, fs::MetadataExt, fs::OpenOptionsExt, fs::PermissionsExt},
    path::{Component, Path, PathBuf},
};

const STAGING_PREFIX: &str = ".agentbox-update-";
const MAX_UNPACKED: u64 = 1024 * 1024 * 1024;
const MAX_ENTRIES: usize = 50_000;
const EXECUTABLE: &str = "agentbox-desktop";

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Serialize)]
struct Identity {
    device: u64,
    inode: u64,
}
fn identity(path: &Path) -> io::Result<Identity> {
    let metadata = fs::symlink_metadata(path)?;
    if !metadata.is_dir() || metadata.file_type().is_symlink() {
        return Err(invalid("application directory is not a normal directory"));
    }
    Ok(Identity {
        device: metadata.dev(),
        inode: metadata.ino(),
    })
}
fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}

#[derive(Deserialize, Serialize)]
struct Receipt {
    target: String,
    identifier: String,
    before_version: String,
    after_version: String,
    bundle: String,
    staging: Identity,
    before: Identity,
    after: Option<Identity>,
}

fn write_receipt(directory: &Path, receipt: &Receipt) -> io::Result<()> {
    if identity(directory)? != receipt.staging {
        return Err(invalid("update staging directory changed"));
    }
    let next = directory.join("receipt.next");
    let mut journal = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&next)?;
    journal.write_all(
        &serde_json::to_vec(receipt).map_err(|_| invalid("cannot encode update receipt"))?,
    )?;
    journal.sync_all()?;
    fs::rename(next, directory.join("receipt.json"))?;
    File::open(directory)?.sync_all()
}

pub(super) fn install(bytes: &[u8], version: &str, identifier: &str, current: &str) -> Result<()> {
    let executable = std::env::current_exe().map_err(|_| install_error())?;
    let macos = executable.parent().ok_or_else(install_error)?;
    let contents = macos.parent().ok_or_else(install_error)?;
    let bundle = contents.parent().ok_or_else(install_error)?;
    if executable.file_name().and_then(|name| name.to_str()) != Some(EXECUTABLE)
        || macos.file_name().and_then(|name| name.to_str()) != Some("MacOS")
        || contents.file_name().and_then(|name| name.to_str()) != Some("Contents")
        || bundle.extension().and_then(|name| name.to_str()) != Some("app")
    {
        return Err(Error::new(
            "update_install",
            "请先将完整应用安装到可写目录，再检查更新",
        ));
    }
    install_at(bytes, bundle, identifier, current, version, |_| Ok(()))
        .map(|_| ())
        .map_err(|_| install_error())
}
fn install_error() -> Error {
    Error::new(
        "update_install",
        "更新安装未完成，原应用或完整旧版副本已保留；请检查磁盘空间与应用目录权限后重新检查更新",
    )
}

fn safe_name(value: &str) -> bool {
    let mut parts = Path::new(value).components();
    matches!(parts.next(), Some(Component::Normal(_))) && parts.next().is_none()
}
fn validate_link(path: &Path, target: &Path) -> io::Result<()> {
    let mut depth = path
        .parent()
        .ok_or_else(|| invalid("missing link parent"))?
        .components()
        .count();
    for part in target.components() {
        match part {
            Component::Normal(_) => depth += 1,
            Component::CurDir => (),
            Component::ParentDir if depth > 1 => depth -= 1,
            _ => return Err(invalid("bundle link escapes application")),
        }
    }
    Ok(())
}

fn extract(bytes: &[u8], directory: &Path) -> io::Result<PathBuf> {
    let decoder = GzDecoder::new(bytes).take(MAX_UNPACKED + 1);
    let mut archive = tar::Archive::new(decoder);
    let mut root_name: Option<PathBuf> = None;
    let mut declared = 0u64;
    for (index, entry) in archive.entries()?.enumerate() {
        if index >= MAX_ENTRIES {
            return Err(invalid("too many update entries"));
        }
        let mut entry = entry?;
        let path = entry.path()?.into_owned();
        if path.components().count() > 64 {
            return Err(invalid("update entry is too deeply nested"));
        }
        let mut components = path.components();
        let first = match components.next() {
            Some(Component::Normal(value)) => PathBuf::from(value),
            _ => return Err(invalid("invalid update root")),
        };
        if first.extension().and_then(|value| value.to_str()) != Some("app")
            || components.any(|value| !matches!(value, Component::Normal(_)))
        {
            return Err(invalid("invalid bundle entry path"));
        }
        if root_name.as_ref().is_some_and(|root| root != &first) {
            return Err(invalid("multiple application roots"));
        }
        root_name = Some(first);
        let kind = entry.header().entry_type();
        if !kind.is_file() && !kind.is_dir() && !kind.is_symlink() {
            return Err(invalid("unsupported update entry type"));
        }
        if entry.header().mode()? & 0o6000 != 0 {
            return Err(invalid("privileged update entry mode"));
        }
        if kind.is_symlink() {
            let target = entry
                .link_name()?
                .ok_or_else(|| invalid("missing bundle link target"))?;
            validate_link(&path, &target)?;
        }
        declared = declared
            .checked_add(entry.size())
            .ok_or_else(|| invalid("update size overflow"))?;
        if declared > MAX_UNPACKED {
            return Err(invalid("expanded update exceeds limit"));
        }
        // tar's containment checks also prevent a later archive entry from
        // following an earlier symlink outside this private staging directory.
        if !entry.unpack_in(directory)? {
            return Err(invalid("update entry was not extracted"));
        }
    }
    let mut tail = archive.into_inner();
    io::copy(&mut tail, &mut io::sink())?; // consume/validate gzip CRC and trailing data
    if tail.limit() == 0 {
        return Err(invalid("expanded archive exceeds limit"));
    }
    Ok(directory.join(root_name.ok_or_else(|| invalid("empty update archive"))?))
}

fn validate_bundle(bundle: &Path, identifier: &str, version: &str) -> io::Result<()> {
    identity(bundle)?;
    let canonical = fs::canonicalize(bundle)?;
    validate_links(&canonical, &canonical)?;
    let contents = bundle.join("Contents");
    identity(&contents)?;
    identity(&contents.join("MacOS"))?;
    let path = contents.join("Info.plist");
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(&path)?;
    if !file.metadata()?.is_file() || file.metadata()?.len() > 1 << 20 {
        return Err(invalid("invalid bundle property list"));
    }
    let value =
        plist::Value::from_reader(file).map_err(|_| invalid("invalid bundle property list"))?;
    let fields = value
        .as_dictionary()
        .ok_or_else(|| invalid("missing bundle metadata"))?;
    let field = |name| fields.get(name).and_then(plist::Value::as_string);
    if field("CFBundleIdentifier") != Some(identifier)
        || field("CFBundleShortVersionString") != Some(version)
        || field("CFBundleExecutable") != Some(EXECUTABLE)
    {
        return Err(invalid(
            "bundle identity or version differs from verified update",
        ));
    }
    let binary = contents.join("MacOS").join(EXECUTABLE);
    let mut file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(binary)?;
    let metadata = file.metadata()?;
    if !metadata.is_file() || metadata.nlink() != 1 || metadata.mode() & 0o111 == 0 {
        return Err(invalid(
            "bundle executable is not a private executable file",
        ));
    }
    // Packages are architecture-specific in this desktop release channel.
    let mut header = [0u8; 8];
    file.read_exact(&mut header)?;
    let cpu = if cfg!(target_arch = "aarch64") {
        0x0100000c_u32
    } else {
        0x01000007_u32
    };
    if header[..4] != [0xcf, 0xfa, 0xed, 0xfe] || header[4..] != cpu.to_le_bytes() {
        return Err(invalid(
            "update executable architecture differs from this client",
        ));
    }
    let resources = contents.join("Resources/third-party");
    for name in ["NOTICES.txt", "inventory.json"] {
        let file = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(resources.join(name))?;
        if !file.metadata()?.is_file() {
            return Err(invalid("missing bundled license resource"));
        }
    }
    let sidecar = contents.join("MacOS/abox-sync");
    let file = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(sidecar)?;
    if !file.metadata()?.is_file() || file.metadata()?.mode() & 0o111 == 0 {
        return Err(invalid("missing bundled background executable"));
    }
    Ok(())
}

fn validate_links(root: &Path, directory: &Path) -> io::Result<()> {
    for entry in fs::read_dir(directory)? {
        let entry = entry?;
        let kind = entry.file_type()?;
        if kind.is_symlink() {
            // Resolve complete link chains after extraction. Lexically internal
            // targets can still escape after another link shortens an ancestor.
            if !fs::canonicalize(entry.path())?.starts_with(root) {
                return Err(invalid("resolved bundle link escapes application"));
            }
        } else if kind.is_dir() {
            validate_links(root, &entry.path())?;
        }
    }
    Ok(())
}

fn sync_tree(directory: &Path) -> io::Result<()> {
    for entry in fs::read_dir(directory)? {
        let entry = entry?;
        let kind = entry.file_type()?;
        if kind.is_dir() {
            sync_tree(&entry.path())?;
        } else if kind.is_file() {
            File::open(entry.path())?.sync_all()?;
        } else if !kind.is_symlink() {
            return Err(invalid("unexpected staged file type"));
        }
    }
    File::open(directory)?.sync_all()
}
fn exchange(left: &Path, right: &Path) -> io::Result<()> {
    let left = CString::new(left.as_os_str().as_bytes())
        .map_err(|_| invalid("invalid application path"))?;
    let right =
        CString::new(right.as_os_str().as_bytes()).map_err(|_| invalid("invalid staged path"))?;
    // macOS RENAME_SWAP exchanges two existing directory entries atomically.
    // Unsupported filesystems fail without moving/deleting either application.
    let code = unsafe {
        libc::renameatx_np(
            libc::AT_FDCWD,
            left.as_ptr(),
            libc::AT_FDCWD,
            right.as_ptr(),
            libc::RENAME_SWAP,
        )
    };
    if code == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

fn cleanup_previous(target: &Path, identifier: &str, current: &str) -> io::Result<()> {
    let parent = target
        .parent()
        .ok_or_else(|| invalid("missing application parent"))?;
    let current_id = identity(target)?;
    for entry in fs::read_dir(parent)? {
        let entry = entry?;
        if !entry
            .file_name()
            .to_string_lossy()
            .starts_with(STAGING_PREFIX)
        {
            continue;
        }
        let metadata = entry.metadata()?;
        if !entry.file_type()?.is_dir()
            || metadata.uid() != unsafe { libc::geteuid() }
            || metadata.mode() & 0o077 != 0
        {
            continue;
        }
        let file = match OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW)
            .open(entry.path().join("receipt.json"))
        {
            Ok(file) if file.metadata()?.is_file() && file.metadata()?.len() <= 8192 => file,
            _ => continue,
        };
        let receipt: Receipt = match serde_json::from_reader(file) {
            Ok(value) => value,
            Err(_) => continue,
        };
        if receipt.target != target.file_name().unwrap().to_string_lossy()
            || receipt.identifier != identifier
        {
            continue;
        }
        if identity(&entry.path())? != receipt.staging {
            return Err(invalid("previous update directory changed"));
        }
        let Some(after) = receipt.after else {
            if !receipt.bundle.is_empty()
                || current_id != receipt.before
                || current != receipt.before_version
            {
                return Err(invalid("unfinished update requires review"));
            }
            // Preparation is journaled before extraction starts. No exchange
            // can occur until the completed receipt has been synced in its place.
            fs::remove_dir_all(entry.path())?;
            File::open(parent)?.sync_all()?;
            continue;
        };
        if !safe_name(&receipt.bundle) {
            return Err(invalid("invalid previous update receipt"));
        }
        let previous = identity(&entry.path().join(&receipt.bundle))?;
        let committed =
            current_id == after && previous == receipt.before && current == receipt.after_version;
        let uncommitted =
            current_id == receipt.before && previous == after && current == receipt.before_version;
        if !committed && !uncommitted {
            return Err(invalid("previous application recovery requires review"));
        }
        // These are exactly the two directories from our prior atomic exchange.
        // Never use a receipt's path as a source/destination for an app restore.
        fs::remove_dir_all(entry.path())?;
        File::open(parent)?.sync_all()?;
    }
    Ok(())
}

fn install_at(
    bytes: &[u8],
    target: &Path,
    identifier: &str,
    current: &str,
    version: &str,
    mut checkpoint: impl FnMut(&str) -> io::Result<()>,
) -> io::Result<PathBuf> {
    let target = fs::canonicalize(target)?;
    validate_bundle(&target, identifier, current)?;
    cleanup_previous(&target, identifier, current)?;
    let parent = target
        .parent()
        .ok_or_else(|| invalid("missing application parent"))?;
    let before = identity(&target)?;
    let staging = tempfile::Builder::new()
        .prefix(STAGING_PREFIX)
        .permissions(fs::Permissions::from_mode(0o700))
        .tempdir_in(parent)?;
    let mut receipt = Receipt {
        target: target.file_name().unwrap().to_string_lossy().into_owned(),
        identifier: identifier.into(),
        before_version: current.into(),
        after_version: version.into(),
        bundle: String::new(),
        staging: identity(staging.path())?,
        before,
        after: None,
    };
    write_receipt(staging.path(), &receipt)?;
    File::open(parent)?.sync_all()?;
    let prepared = extract(bytes, staging.path())?;
    validate_bundle(&prepared, identifier, version)?;
    checkpoint("after_extract")?;
    sync_tree(&prepared)?;
    let after = identity(&prepared)?;
    receipt.bundle = prepared.file_name().unwrap().to_string_lossy().into_owned();
    receipt.after = Some(after);
    write_receipt(staging.path(), &receipt)?;
    File::open(parent)?.sync_all()?;
    checkpoint("before_exchange")?;
    if identity(&target)? != before || identity(&prepared)? != after {
        return Err(invalid("application changed before update"));
    }
    // Once exchange might occur, no RAII temp-directory cleanup may delete the
    // old app. A crash leaves either a complete old or complete new live bundle.
    let recovery = staging.keep();
    if let Err(error) = exchange(&target, &prepared) {
        if identity(&target).ok() == Some(before) && identity(&prepared).ok() == Some(after) {
            let _ = fs::remove_dir_all(&recovery);
        }
        return Err(error);
    }
    let committed = checkpoint("after_exchange")
        .and_then(|_| File::open(parent)?.sync_all())
        .and_then(|_| File::open(&recovery)?.sync_all())
        .and_then(|_| checkpoint("after_sync"));
    if let Err(error) = committed {
        // Roll back by the same atomic operation. If rollback itself fails, both
        // full bundles and the identifying receipt remain available on disk.
        if checkpoint("before_rollback").is_ok()
            && identity(&target).ok() == Some(after)
            && identity(&prepared).ok() == Some(before)
            && exchange(&target, &prepared).is_ok()
        {
            let _ = File::open(parent).and_then(|directory| directory.sync_all());
            let _ = fs::remove_dir_all(&recovery);
        }
        return Err(error);
    }
    if identity(&target)? != after || identity(&prepared)? != before {
        return Err(invalid("application exchange identity mismatch"));
    }
    Ok(recovery)
}

#[cfg(test)]
#[path = "updates_macos_tests.rs"]
mod tests;
