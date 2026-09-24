#!/usr/bin/env python3
"""Versioned Linux installation and offline migration. No downloads or source builds."""
import argparse
import contextlib
import fcntl
import hashlib
import stat
import tarfile
import datetime
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import tempfile
import time
import urllib.request


def run(*args, capture=False):
    return subprocess.run([str(x) for x in args], check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def read(path):
    return json.loads(Path(path).read_text())


def absolute(path):
    p = Path(path).absolute()
    if any(c.isspace() for c in str(p)) or any(c in str(p) for c in '%"\\'):
        raise ValueError('deployment paths must not contain whitespace, %, quotes or backslashes')
    return p


def resolve(base, path):
    return (base / path).resolve()


def disjoint(paths):
    paths = [p.resolve() for p in paths]
    for i, a in enumerate(paths):
        for b in paths[i + 1:]:
            if a == b or a in b.parents or b in a.parents:
                raise ValueError('source and destination directories must not overlap')


def atomic_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix='.agentbox-')
    try:
        with os.fdopen(fd, 'w') as f:
            json.dump(value, f, indent=2); f.write('\n'); f.flush(); os.fsync(f.fileno())
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp): os.unlink(tmp)


@contextlib.contextmanager
def locked(path):
    with open(path, 'a') as f:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


def unit(app, config):
    return f'''[Unit]
Description=agentbox
After=network-online.target docker.service
Wants=network-online.target docker.service
StartLimitIntervalSec=60
StartLimitBurst=5
[Service]
Type=simple
WorkingDirectory={app}
ExecStart={app}/current/agentbox -config {config}
Restart=on-failure
RestartSec=3
TimeoutStopSec=90
UMask=0077
StandardOutput=journal
StandardError=journal
[Install]
WantedBy=multi-user.target
'''


def restore_context(path):
    # copytree/copy2 preserve security.selinux xattrs from the download's /tmp
    # directory. systemd cannot execute user_tmp_t files on enforcing hosts.
    # Apply the host policy at the final installation path, never to user data.
    if not Path('/sys/fs/selinux/enforce').exists():
        return
    restorecon = shutil.which('restorecon')
    if not restorecon:
        raise RuntimeError('SELinux is enabled but restorecon is missing; install policycoreutils and retry')
    run(restorecon, '-R', path)


def stage(package, app):
    meta = read(package / 'build.json')
    version = meta['version']
    if meta.get('program') != 'agentbox' or meta.get('os') != 'linux' or not re.fullmatch(r'v\d+\.\d+\.\d+(?:-[\w.-]+)?', version):
        raise ValueError('expected an unpacked Linux agentbox release')
    if any(p.is_symlink() for p in package.rglob('*')):
        raise ValueError('release must not contain symlinks')
    releases = app / 'releases'; releases.mkdir(parents=True, exist_ok=True)
    target = releases / version
    if target.exists(): raise ValueError('version already installed; versions are immutable')
    with tempfile.TemporaryDirectory(dir=releases, prefix='.stage-') as tmp:
        dest = Path(tmp) / version
        shutil.copytree(package, dest)
        run(dest / 'agentbox', '--version')
        os.rename(dest, target)
        try:
            restore_context(target)
        except Exception:
            # Keep failed staging retryable; no current link points here yet.
            os.rename(target, dest)
            raise
    return version


def switch(app, version):
    if not re.fullmatch(r'v\d+\.\d+\.\d+(?:-[\w.-]+)?', version): raise ValueError('invalid version')
    target = app / 'releases' / version
    if not (target / 'agentbox').is_file(): raise ValueError('version is not installed')
    link = app / '.current-new'
    if link.exists() or link.is_symlink(): raise ValueError('stale switch marker; inspect before retry')
    os.symlink('releases/' + version, link)
    os.replace(link, app / 'current')


def schema(data):
    db = data / 'state.db'
    if not db.exists(): return 0
    with contextlib.closing(sqlite3.connect(db.as_uri() + '?mode=ro', uri=True)) as conn:
        return conn.execute('pragma user_version').fetchone()[0]


def compatible(binary, config, data):
    info = json.loads(run(binary, 'check-config', '--config', config, capture=True))
    if info.get('compatibility_epoch') != 1 or schema(data) > info['schema_version']:
        raise ValueError('incompatible database; restore a compatible backup to a NEW directory first')


CLIENT_NAMES = tuple('abox-link-' + system + '-' + arch + ('.exe' if system == 'windows' else '')
                     for system, arch in [('linux', 'amd64'), ('linux', 'arm64'),
                                          ('darwin', 'amd64'), ('darwin', 'arm64'), ('windows', 'amd64')])


def install_clients(package, data):
    source = package / 'clients'
    if not source.exists():
        return  # Releases before v0.1.1 did not bundle clients.
    for name in CLIENT_NAMES:
        if not (source / name).is_file() or (source / name).is_symlink():
            raise ValueError('Incomplete bundled clients: ' + name)
    target = data / 'abox-link'
    if target.is_symlink():
        raise ValueError('client directory must not be a symlink')
    target.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=data, prefix='.clients-') as tmp:
        for name in CLIENT_NAMES:
            shutil.copyfile(source / name, Path(tmp) / name)
            (Path(tmp) / name).chmod(0o755)
        for name in CLIENT_NAMES:
            os.replace(Path(tmp) / name, target / name)


def activate(app, version, config, backup_dir, unit_dir):
    # Only manage a unit created for this layout. Never hijack a repository install.
    if (unit_dir / 'agentbox.service').read_text() != unit(app, config):
        raise ValueError('unit belongs to another installation; migrate/install explicitly first')
    if not re.fullmatch(r'v\d+\.\d+\.\d+(?:-[\w.-]+)?', version): raise ValueError('invalid version')
    binary = app / 'releases' / version / 'agentbox'
    # Also repairs versions staged by older installers, before stopping service.
    restore_context(binary.parent)
    cfg = read(config); data = resolve(config.parent, cfg.get('data_dir', 'data'))
    compatible(binary, config, data)
    run('systemctl', 'stop', 'agentbox.service')
    # Inspect again after stop, then back up with the currently installed binary.
    # No automatic rollback: the new binary might already have migrated SQLite.
    compatible(binary, config, data)
    old = app / 'current'
    if old.is_symlink() and (data / 'state.db').exists():
        backup_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        archive = backup_dir / ('before-' + version + '-' + str(time.time_ns()) + '.tar.gz')
        run(old / 'agentbox', 'backup', '--config', config, '--output', archive)
        run(binary, 'backup-verify', archive)
    with locked(data / 'agentbox.lock'):
        compatible(binary, config, data)
        switch(app, version)
    install_clients(binary.parent, data)
    run('systemctl', 'start', 'agentbox.service')
    listen = cfg.get('listen', '127.0.0.1:8080')
    host, port = listen.rsplit(':', 1)
    if host in ('', '0.0.0.0'): host = '127.0.0.1'
    if host == '[::]': host = '[::1]'
    for _ in range(20):
        try:
            with urllib.request.build_opener(urllib.request.ProxyHandler({})).open('http://' + host + ':' + port + '/api/ping', timeout=2) as res:
                if res.status in (200, 204):
                    run('systemctl', 'is-active', '--quiet', 'agentbox.service')
                    print('Active:', version); return
        except (OSError, subprocess.CalledProcessError): pass
        time.sleep(1)
    raise RuntimeError('health check failed; inspect journalctl -u agentbox. No automatic database rollback performed')


def mounted_containers(roots):
    ids = run('docker', 'ps', '-aq', capture=True).split()
    if not ids: return []
    records = json.loads(run('docker', 'inspect', *ids, capture=True))
    result = []
    for c in records:
        for m in c.get('Mounts', []):
            src = Path(m.get('Source', '/__none__')).resolve()
            if any(src == r or src in r.parents or r in src.parents for r in roots):
                result.append(c); break
    return result


def check_reflink(source, destination_parent):
    # Probe the actual filesystems before stopping the service. Never fall back
    # to a full copy on a low-space host.
    with tempfile.TemporaryDirectory(dir=source, prefix='.reflink-probe-') as src:
        with tempfile.TemporaryDirectory(dir=destination_parent, prefix='.reflink-probe-') as dst:
            f = Path(src) / 'fixture'; f.write_bytes(b'agentbox-reflink-test' * 4096)
            run('cp', '--reflink=always', f, Path(dst) / 'copy')


def verify_cloned_users(archive, directory):
    # Compare the clone with the already-verified archive, not just the live
    # source: this also detects source changes between backup and clone.
    manifest = None
    with tarfile.open(archive, 'r|gz') as tar:
        for entry in tar:
            if entry.name == 'manifest.json':
                if entry.size > 256 << 20: raise ValueError('manifest too large')
                manifest = json.load(tar.extractfile(entry))
    if manifest is None or manifest['mode'] != 'full': raise ValueError('full manifest required')
    expected = {}
    for entry in manifest['entries']:
        name = entry['name']
        if name == 'data/users': expected['.'] = entry
        elif name.startswith('data/users/'): expected[name[len('data/users/'):]] = entry
    actual = {'.'} if directory.exists() else set()
    if directory.exists():
        for parent, dirs, files in os.walk(directory, followlinks=False):
            actual.update(str((Path(parent) / name).relative_to(directory)) for name in dirs + files)
    if actual != set(expected): raise ValueError('cloned user tree differs from full backup inventory')
    for name, entry in expected.items():
        file = directory / name
        st = file.lstat()
        kind = 53 if stat.S_ISDIR(st.st_mode) else 50 if stat.S_ISLNK(st.st_mode) else 48 if stat.S_ISREG(st.st_mode) else -1
        if kind != entry['type'] or (stat.S_IMODE(st.st_mode) & 0o777) != entry['mode'] or st.st_uid != entry['uid'] or st.st_gid != entry['gid']:
            raise ValueError('cloned user tree metadata mismatch')
        stamp = entry['mtime']
        parsed = datetime.datetime.fromisoformat(re.sub(r'\.\d+', '', stamp).replace('Z', '+00:00'))
        nanos = int(parsed.replace(microsecond=0).timestamp()) * 1000000000
        fraction = re.search(r'\.(\d+)', stamp)
        if fraction: nanos += int(fraction.group(1).ljust(9, '0')[:9])
        if st.st_mtime_ns != nanos: raise ValueError('cloned user tree timestamp mismatch')
        if kind == 50 and os.readlink(file) != entry['link']: raise ValueError('cloned symlink mismatch')
        if kind == 48:
            with open(file, 'rb') as stream: digest = hashlib.file_digest(stream, 'sha256').hexdigest() if hasattr(hashlib, 'file_digest') else file_digest(stream)
            if st.st_size != entry['size'] or digest != entry['sha256']: raise ValueError('cloned user file differs from full backup')


def file_digest(stream):
    digest = hashlib.sha256()
    for block in iter(lambda: stream.read(1024 * 1024), b''): digest.update(block)
    return digest.hexdigest()


def tree_inventory(directory):
    inventory = {}
    def record(path):
        st = path.lstat()
        row = {'mode': st.st_mode, 'uid': st.st_uid, 'gid': st.st_gid, 'mtime_ns': st.st_mtime_ns}
        if stat.S_ISLNK(st.st_mode): row['link'] = os.readlink(path)
        elif stat.S_ISREG(st.st_mode):
            row['size'] = st.st_size
            with open(path, 'rb') as stream: row['sha256'] = file_digest(stream)
            after = path.lstat()
            if (after.st_size, after.st_mtime_ns, after.st_ino) != (st.st_size, st.st_mtime_ns, st.st_ino):
                raise ValueError('snapshot source changed during hashing')
        elif not stat.S_ISDIR(st.st_mode): raise ValueError('unsupported special file in snapshot')
        inventory[str(path.relative_to(directory))] = row
        if len(inventory) > 1000000: raise ValueError('snapshot exceeds one million entries')
    if directory.exists():
        record(directory)
        for parent, dirs, files in os.walk(directory, followlinks=False):
            for name in dirs + files: record(Path(parent) / name)
    return inventory


def verify_snapshot(directory):
    manifest = read(directory / 'snapshot-manifest.json')
    if manifest.get('format_version') != 1 or manifest.get('mode') != 'full-reflink':
        raise ValueError('unsupported snapshot manifest')
    # The root mtime changes when writing the manifest; compare every child.
    actual = tree_inventory(directory)
    actual.pop('.', None); actual.pop('snapshot-manifest.json', None)
    if actual != manifest['entries']: raise ValueError('snapshot contents or metadata differ from manifest')


def create_snapshot(binary, source, old_data, target):
    # Self-contained recovery directory: SQLite/config/creds via verified system
    # backup, all users via independent copy-on-write inodes. Never a hardlink.
    system = target.with_name(target.name + '.system.tar.gz')
    run(binary, 'backup', '--config', source, '--output', system)
    run(binary, 'backup-verify', system)
    run(binary, 'restore', '--to', target, system)
    users = old_data / 'users'
    if users.exists():
        expected = tree_inventory(users)
        clone = target / 'cloned-users'
        run('cp', '-a', '--reflink=always', users, clone)
        if tree_inventory(clone) != expected: raise ValueError('snapshot users differ from source')
        templates = target / 'data' / 'users'
        if templates.exists(): shutil.rmtree(templates)
        os.rename(clone, templates)
    inventory = tree_inventory(target); inventory.pop('.', None)
    atomic_json(target / 'snapshot-manifest.json', {'format_version': 1, 'mode': 'full-reflink', 'entries': inventory})
    verify_snapshot(target)


def migrate(args):
    source, config, data, cache = map(absolute, [args.source_config, args.config, args.data, args.cache])
    binary = absolute(args.binary); archive = absolute(args.backup)
    cfg = read(source); old_data = resolve(source.parent, cfg.get('data_dir', 'data'))
    roots = [old_data] + [resolve(source.parent, x['credentials_dir']) for x in cfg.get('accounts', []) if x.get('credentials_dir')]
    disjoint([data, cache, config.parent])
    for dest in (data, cache, config.parent):
        for root in roots: disjoint([root, dest])
    if getattr(args, 'snapshot_backup', False) and not getattr(args, 'reflink', False): raise ValueError('--snapshot-backup requires --reflink')
    if data.exists() or config.exists() or archive.exists(): raise ValueError('data/config/backup destinations must be new')
    containers = mounted_containers(roots)
    print(json.dumps({'source_data': str(old_data), 'destination_data': str(data),
                      'containers_to_stop_and_remove': [c['Id'][:12] for c in containers],
                      'apply': args.apply}, indent=2))
    if not args.apply: return
    if getattr(args, 'reflink', False): check_reflink(old_data, data.parent)
    # Backup before downtime as well as the authoritative full backup after stop.
    archive.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    pre = archive.with_name(archive.name + '.preflight.tar.gz')
    run(binary, 'backup', '--config', source, '--output', pre)
    run(binary, 'backup-verify', pre)
    run('systemctl', 'stop', 'agentbox.service')
    if read(source) != cfg: raise ValueError('configuration changed during shutdown; inspect and retry before migration')
    for c in containers:
        if c.get('State', {}).get('Running'): run('docker', 'stop', c['Id'])
    snapshot_backup = getattr(args, 'snapshot_backup', False)
    restore_archive = archive
    if not snapshot_backup:
        run(binary, 'backup', '--full', '--config', source, '--output', archive)
        run(binary, 'backup-verify', archive)
        if getattr(args, 'reflink', False):
            restore_archive = archive.with_name(archive.name + '.system.tar.gz')
            run(binary, 'backup', '--config', source, '--output', restore_archive)
            run(binary, 'backup-verify', restore_archive)
    # Hold the service lock throughout restore/publication. The verified full
    # archive is the only source; never copy changing database/WAL files directly.
    with locked(old_data / 'agentbox.lock'):
        if any(c.get('State', {}).get('Running') for c in mounted_containers(roots)):
            raise ValueError('a source container restarted; stop it and retry with new destinations')
        data.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=data.parent, prefix='.migration-') as tmp:
            restored = Path(tmp) / 'restore'
            if snapshot_backup:
                create_snapshot(binary, source, old_data, archive)
                run('cp', '-a', '--reflink=always', archive, restored)
                verify_snapshot(restored)
            else:
                run(binary, 'restore', '--to', restored, restore_archive)
                if getattr(args, 'reflink', False):
                    cloned = Path(tmp) / 'users'
                    if (old_data / 'users').exists(): run('cp', '-a', '--reflink=always', old_data / 'users', cloned)
                    verify_cloned_users(archive, cloned)
                    if cloned.exists():
                        templates = restored / 'data' / 'users'
                        if templates.exists(): shutil.rmtree(templates)
                        os.rename(cloned, templates)
            updated = read(restored / 'config.json')
            # Credential roots may be outside data in the backup layout.
            creds = restored / 'data' / 'creds'
            creds.mkdir(exist_ok=True)
            moved = {}
            for account in updated.get('accounts', []):
                if not account.get('credentials_dir'): continue
                src = resolve(restored, account['credentials_dir'])
                if src not in moved:
                    dest = creds / ('pool-' + str(len(moved)))
                    while dest.exists(): dest = dest.with_name(dest.name + '-copy')
                    # cp -a preserves numeric ownership, mode, link times and mtime.
                    run('cp', '-a', src, dest)
                    moved[src] = data / 'creds' / dest.name
                account['credentials_dir'] = str(moved[src])
            updated['data_dir'] = str(data); updated['cache_dir'] = str(cache)
            atomic_json(restored / 'candidate.json', updated)
            run(binary, 'check-config', '--config', restored / 'candidate.json')
            # Remove only source-related stopped containers: bind paths cannot
            # follow moved data. Never remove Docker volumes or unrelated images.
            for c in mounted_containers(roots):
                if c.get('State', {}).get('Running'): raise ValueError('source container restarted')
                run('docker', 'rm', c['Id'])
            db = restored / 'data' / 'state.db'
            with sqlite3.connect(db) as conn:
                conn.execute("update sessions set container_id='', status='stopped'")
            os.rename(restored / 'data', data)
            cache.mkdir(parents=True, exist_ok=True, mode=0o700)
            if config.exists(): raise ValueError('config destination appeared during migration')
            atomic_json(config, updated)
    print('Migrated offline. Old files and backups retained. Install the release unit, then activate and verify sessions/history/credentials.')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--app', default='/opt/agentbox')
    p.add_argument('--config', default='/etc/agentbox/config.json')
    p.add_argument('--unit-dir', default='/etc/systemd/system')
    sub = p.add_subparsers(dest='command', required=True)
    s = sub.add_parser('install'); s.add_argument('--package', required=True)
    s = sub.add_parser('activate'); s.add_argument('--version', required=True); s.add_argument('--backup-dir', default='/var/lib/agentbox/backups')
    s = sub.add_parser('migrate'); s.add_argument('--source-config', required=True); s.add_argument('--binary', required=True); s.add_argument('--backup', required=True)
    s.add_argument('--snapshot-backup', action='store_true', help='create a verified self-contained full reflink recovery directory instead of a full tarball')
    s.add_argument('--data', default='/var/lib/agentbox'); s.add_argument('--cache', default='/var/cache/agentbox'); s.add_argument('--apply', action='store_true'); s.add_argument('--reflink', action='store_true', help='require copy-on-write cloning of users; verify every entry against full backup')
    a = p.parse_args()
    if os.geteuid() != 0: p.error('run as root on the target Linux host')
    app, config, units = map(absolute, [a.app, a.config, a.unit_dir])
    if a.command == 'migrate' and not a.apply: migrate(a); return
    app.mkdir(parents=True, exist_ok=True)
    with locked(app / '.deploy.lock'):
        if a.command == 'migrate': migrate(a); return
        if a.command == 'install':
            if not config.is_file(): p.error('create config.json first; see deploy/README.md')
            target_unit = units / 'agentbox.service'
            if target_unit.exists() and target_unit.read_text() != unit(app, config):
                p.error('existing service uses another layout; migrate, then retain/rename its unit before installing')
            package = absolute(a.package)
            run(package / 'agentbox', 'check-config', '--config', config)
            version = stage(package, app)
            units.mkdir(parents=True, exist_ok=True)
            target_unit.write_text(unit(app, config))
            run('systemctl', 'daemon-reload'); run('systemctl', 'enable', 'agentbox.service')
            cfg = read(config)
            resolve(config.parent, cfg.get('data_dir', 'data')).mkdir(parents=True, exist_ok=True, mode=0o700)
            print('Installed', version, '; activate explicitly after configuring the pinned agent image.')
        elif a.command == 'activate':
            activate(app, a.version, config, absolute(a.backup_dir), units)


if __name__ == '__main__':
    try: main()
    except (ValueError, OSError, RuntimeError, subprocess.CalledProcessError) as e:
        raise SystemExit(str(e))
