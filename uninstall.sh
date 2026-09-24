#!/usr/bin/env bash
# Standalone removal for the default release layout (including v0.1.0).
set -euo pipefail
main() {
  command -v python3 >/dev/null || { echo 'Python 3.9+ is required' >&2; return 1; }
  python3 - "$@" <<'PY'
import argparse
import datetime
import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

APP = Path('/opt/agentbox')
CONFIG = Path('/etc/agentbox')
DATA = Path('/var/lib/agentbox')
CACHE = Path('/var/cache/agentbox')
UNIT = Path('/etc/systemd/system/agentbox.service')
BACKUPS = Path('/var/backups/agentbox-uninstall')


def run(*args):
    return subprocess.check_output([str(a) for a in args], text=True).strip()


def validate():
    for path in (APP, CONFIG, DATA, CACHE, UNIT, BACKUPS):
        if path.is_symlink() or path.resolve() != path:
            raise ValueError('Refusing symlinked installation path: ' + str(path))
    if not UNIT.is_file():
        raise ValueError('No release installation unit found; no files changed')
    text = UNIT.read_text()
    if ('WorkingDirectory=' + str(APP) + '\n' not in text or
            'ExecStart=' + str(APP) + '/current/agentbox -config ' + str(CONFIG / 'config.json') + '\n' not in text):
        raise ValueError('Service belongs to a different installation; refusing removal')
    fragment = run('systemctl', 'show', 'agentbox.service', '--property=FragmentPath', '--value')
    if fragment != str(UNIT):
        raise ValueError('Loaded service does not match the release unit')
    if run('systemctl', 'show', 'agentbox.service', '--property=DropInPaths', '--value'):
        raise ValueError('Service has custom overrides; review them before uninstalling')
    config_file = CONFIG / 'config.json'
    if config_file.is_symlink():
        raise ValueError('Refusing symlinked config')
    cfg = json.loads(config_file.read_text())
    for key, expected in [('data_dir', DATA), ('cache_dir', CACHE)]:
        if Path(cfg.get(key, '')).resolve() != expected:
            raise ValueError('Custom ' + key + '; this script only removes the default release layout')
    return cfg


def containers():
    # Refuse unrelated containers using these files instead of deleting their data.
    ids = run('docker', 'ps', '-aq').split()
    if not ids:
        return []
    selected = []
    roots = (APP, CONFIG, DATA, CACHE)
    for item in json.loads(run('docker', 'inspect', *ids)):
        mounts = [Path(m['Source']).resolve() for m in item.get('Mounts', []) if m.get('Type') == 'bind']
        touches = any(m == root or root in m.parents or m in root.parents for root in roots for m in mounts)
        if not touches:
            continue
        if (not item.get('Config', {}).get('Labels', {}).get('agentbox.session') or
                not mounts or not all(m == DATA or DATA in m.parents for m in mounts)):
            raise ValueError('Unrelated or custom-mounted container uses installation paths: ' + item['Id'][:12])
        selected.append(item['Id'])
    return selected


def main():
    parser = argparse.ArgumentParser(description='Uninstall the default agentbox release installation. By default retain all files in a private backup directory, allowing a fresh install.')
    parser.add_argument('--yes', action='store_true', help='confirm stopping agentbox and removing its workspace containers')
    parser.add_argument('--purge', action='store_true', help='permanently delete configuration, credentials, workspaces and releases instead of retaining them')
    parser.add_argument('--dry-run', action='store_true', help='validate and print the plan without changing anything')
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('run with sudo bash')
    # Same lock as the online installer; no concurrent install/uninstall.
    with open('/run/lock/agentbox-install.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        for key in ('DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH'):
            os.environ.pop(key, None)
        os.environ['DOCKER_HOST'] = 'unix:///var/run/docker.sock'
        validate()
        selected = containers()
        print('Stop and disable agentbox.service; remove its %d workspace containers.' % len(selected))
        print(('PERMANENTLY DELETE: ' if args.purge else 'Move to a private backup: ') + ', '.join(map(str, (APP, CONFIG, DATA, CACHE))))
        print('Docker, images, other containers and firewall rules are retained.')
        if args.dry_run:
            return
        if not args.yes:
            with open('/dev/tty', 'r+') as tty:
                tty.write('Type uninstall to continue: '); tty.flush()
                if tty.readline().strip() != 'uninstall':
                    raise ValueError('Cancelled; no changes made')
        backup = None
        if not args.purge:
            BACKUPS.mkdir(parents=True, exist_ok=True, mode=0o700)
            backup = Path(tempfile.mkdtemp(prefix=datetime.datetime.now().strftime('%Y%m%d-%H%M%S-'), dir=BACKUPS))
            # Default retention uses renames: no full copy or partial deletion on low disk.
            if any(p.exists() and p.stat().st_dev != backup.stat().st_dev for p in (APP, CONFIG, DATA, CACHE)):
                backup.rmdir()
                raise ValueError('Backup is on another filesystem; make a verified backup and use --purge explicitly')
        run('systemctl', 'stop', 'agentbox.service')
        selected = containers()  # Recheck after stopping new workspace creation.
        if selected:
            run('docker', 'rm', '-f', *selected)
        run('systemctl', 'disable', 'agentbox.service')
        if backup:
            shutil.copy2(UNIT, backup / 'agentbox.service')
        UNIT.unlink()
        run('systemctl', 'daemon-reload')
        subprocess.run(['systemctl', 'reset-failed', 'agentbox.service'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for name, path in [('app', APP), ('config', CONFIG), ('data', DATA), ('cache', CACHE)]:
            if path.exists():
                if backup:
                    os.rename(path, backup / name)
                else:
                    shutil.rmtree(path)
        print('agentbox uninstalled. Fresh installation is now possible.')
        if backup:
            print('Retained files (root only): ' + str(backup))


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        raise SystemExit('Uninstall stopped: ' + str(error))
PY
}
main "$@"
