#!/usr/bin/env python3
"""Probe installed packages, optionally replacing an actual older package.

Only temporary directories and a synthetic loopback peer are used. On Windows,
NSIS also changes per-user registry entries, so a disposable hosted CI user is
required. GUI preferences, credentials and the updater installation path are not
exercised; sync state is explicitly passed to the bundled private sidecar.
"""
import argparse
from contextlib import closing, contextmanager
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import queue
import re
import shutil
import sqlite3
import stat
import subprocess
import tempfile
import threading
import time

from webview2_support import (UNINSTALL_KEY, bounded_file, install_command, pe_machine,
                              registry_values, write_report)

SERVER_ID = 'a' * 64
TOKEN = 'isolated-install-probe'
USER = 'install-probe'
CONTENT = '升级 retention\r\n\0'.encode()


def semver(value):
    match = re.fullmatch(r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?', value)
    if not match:
        raise ValueError('Invalid diagnostic version')
    pre = match[4]
    parts = []
    if pre:
        for part in pre.split('.'):
            if not part or part.isdigit() and len(part) > 1 and part.startswith('0'):
                raise ValueError('Invalid prerelease version')
            parts.append((0, int(part)) if part.isdigit() else (1, part))
    return tuple(map(int, match.group(1, 2, 3))) + (not bool(pre), tuple(parts))


def runtime_environment():
    runtime_path = os.environ.get('SystemRoot', r'C:\Windows') + r'\System32' if os.name == 'nt' else '/usr/bin:/bin:/usr/sbin:/sbin'
    # The synthetic loopback peer must not pass through a configured proxy.
    environment = {key: value for key, value in os.environ.items() if key.lower() not in ('http_proxy', 'https_proxy', 'all_proxy')}
    return {**environment, 'PATH': runtime_path, 'NO_PROXY': '127.0.0.1'}


def check_windows_isolation(disposable):
    if os.name == 'nt' and not (disposable and os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted'):
        raise ValueError('NSIS probe requires --disposable-windows-user on a GitHub-hosted disposable runner; temporary install paths do not isolate registry entries')


def app_registry():
    return registry_values(UNINSTALL_KEY, ['DisplayVersion', 'InstallLocation', 'UninstallString'])


def package_executable(package):
    """Read the exact candidate's PE bytes without executing it or trusting a build directory."""
    specification = importlib.util.spec_from_file_location('nsis_payload_audit', Path(__file__).with_name('test-webview2-package.py'))
    audit = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(audit)
    seven_zip = shutil.which('7z') or str(Path(os.environ['ProgramFiles']) / '7-Zip/7z.exe')
    package_identity = bounded_file(package)
    listing = subprocess.run([seven_zip, 'l', '-slt', '-sccUTF-8', str(package)], capture_output=True,
                             text=True, encoding='utf-8', timeout=45, check=True).stdout
    if len(listing) > 16 * 1024 * 1024:
        raise ValueError('NSIS payload listing exceeded limit')
    with tempfile.TemporaryDirectory(prefix='agentbox-payload-identity-') as temporary:
        executable = Path(temporary) / 'agentbox-desktop.exe'
        audit.extract(seven_zip, package, audit.archive_entry(listing, executable.name), executable)
        if pe_machine(executable) != 0x8664:
            raise ValueError('Installed probe requires the actual Windows x64 payload')
        result = bounded_file(executable)
    if bounded_file(package) != package_identity:
        raise ValueError('Installer changed while verifying its payload')
    return package_identity, result


def normal_path(path, directory=False):
    metadata = path.lstat()
    if stat.S_ISLNK(metadata.st_mode) or getattr(metadata, 'st_file_attributes', 0) & 0x400:
        raise ValueError('Installer fixture path became a link or reparse point')
    if (directory and not stat.S_ISDIR(metadata.st_mode)) or (not directory and not stat.S_ISREG(metadata.st_mode)):
        raise ValueError('Installer fixture has an unexpected file type')
    return metadata.st_dev, metadata.st_ino


def registry_path(value):
    # Tauri writes quoted InstallLocation and UninstallString values. Permit
    # exactly a path, never command arguments from a registry string.
    if value.startswith('"') and value.endswith('"'):
        value = value[1:-1]
    if not value or any(character in value for character in ('"', '\r', '\n', '\0')):
        raise ValueError('Invalid installer registry path')
    path = Path(value)
    if not path.is_absolute():
        raise ValueError('Installer registry path is not absolute')
    return path.resolve()


class WindowsInstallGuard:
    def __init__(self, installed):
        self.before = app_registry()
        if any(row['exists'] for row in self.before):
            raise ValueError('Agentbox is already registered; do not replace a pre-existing application')
        if any(installed.iterdir()) or installed.resolve() != installed:
            raise ValueError('Require an empty, owned installer fixture directory')
        self.directory = installed
        self.identity = normal_path(installed, directory=True)
        self.payloads = []
        self.report = {'registry_before': self.before, 'cleanup': 'not_run'}

    def prepare(self, package):
        package_identity, executable = package_executable(package)
        self.payloads.append(executable)
        if bounded_file(package) != package_identity:
            raise ValueError('Verified installer changed before launch')

    def missing(self):
        self.report['registry_after_attempt'] = app_registry()
        self.report['unexpected_locations_followed'] = False
        return ValueError('Installed desktop executable missing; observed installer registry: ' +
                          json.dumps(self.report['registry_after_attempt'], ensure_ascii=False))

    def verify_executable(self):
        if self.directory.resolve() != self.directory or normal_path(self.directory, directory=True) != self.identity:
            raise ValueError('Owned installation directory changed')
        executable = self.directory / 'agentbox-desktop.exe'
        normal_path(executable)
        if bounded_file(executable) not in self.payloads:
            raise ValueError('Installed executable does not match a verified candidate payload')

    def cleanup(self):
        current = app_registry()
        self.report['registry_after_attempt'] = current
        self.report['unexpected_locations_followed'] = False
        executable = self.directory / 'agentbox-desktop.exe'
        uninstaller = self.directory / 'uninstall.exe'
        if not executable.exists() and current == self.before:
            self.report['cleanup'] = 'not_installed'
            return
        try:
            self.verify_executable()
            normal_path(uninstaller)
            for row in current:
                if row['exists'] and (row['hive'] != 'HKCU' or
                        registry_path(row['values'].get('InstallLocation', '')) != self.directory or
                        registry_path(row['values'].get('UninstallString', '')) != uninstaller):
                    raise ValueError('Unexpected registered installation; location recorded but not followed')
            # This path is fixed inside our previously empty owned directory.
            # Never execute a registry command or follow a fallback location.
            subprocess.run([str(uninstaller), '/S'], shell=False, check=True, timeout=180)
            for _ in range(50):
                if not executable.exists() and app_registry() == self.before:
                    self.report['cleanup'] = 'passed'
                    self.report['registry_after_cleanup'] = app_registry()
                    return
                time.sleep(0.2)
            raise ValueError('Owned installation files or registry remain after uninstall')
        except Exception as error:
            self.report['cleanup'] = 'failed'
            self.report['cleanup_error'] = str(error)
            raise


def install(package, installed, guard=None):
    if os.name == 'nt':
        if guard is None:
            raise ValueError('Windows installation requires an owned disposable fixture guard')
        guard.prepare(package)
        subprocess.run(install_command(package, installed), shell=False, check=True, timeout=180)
        executable = installed / 'agentbox-desktop.exe'
    else:
        if package.suffix != '.app' or not package.is_dir():
            raise ValueError('macOS probe expects a bundled .app')
        # This tests complete bundle replacement, not Tauri's installer plugin.
        staging = installed / 'next.app'
        shutil.copytree(package, staging, symlinks=True)
        destination = installed / 'Agentbox.app'
        if destination.exists():
            shutil.rmtree(destination)
        staging.rename(destination)
        executable = destination / 'Contents/MacOS/agentbox-desktop'
    if not executable.is_file():
        raise guard.missing() if guard is not None else ValueError('Installed desktop executable missing')
    if guard is not None:
        guard.verify_executable()
    return executable


def diagnostics(executable, directory, label):
    reports = []
    for attempt in range(2):
        report = directory / f'{label}-{attempt}.json'
        subprocess.run([str(executable), '--diagnostics-json', str(report)], env=runtime_environment(), check=True, timeout=30)
        value = json.loads(report.read_text(encoding='utf-8'))
        if not all(value.get(key) for key in ('ok', 'sidecar', 'licenses')):
            raise ValueError('Installed probe failed')
        semver(value['version'])
        reports.append(value)
    if reports[0] != reports[1]:
        raise ValueError('Cold and repeat diagnostic reports differ')
    return reports[0]


@contextmanager
def fixture_peer():
    rules = hashlib.sha256(b'agentbox-ignore-v1\n').hexdigest()
    manifest = {'version': 1, 'rules_hash': rules, 'entries': {'保留.txt': {'kind': 'file', 'hash': hashlib.sha256(CONTENT).hexdigest(), 'size': len(CONTENT)}}}
    digest = hashlib.sha256(json.dumps(manifest, ensure_ascii=False, separators=(',', ':')).encode()).hexdigest()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def response(self, value, status=200):
            data = json.dumps(value).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.send_header('X-Agentbox-Server-ID', SERVER_ID)
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.headers.get('Authorization') != 'Bearer ' + TOKEN:
                return self.response({}, 401)
            if self.path == '/api/clients/capabilities':
                return self.response({'protocol_version': 1, 'server_id': SERVER_ID, 'user': USER, 'features': {'sync': 1}})
            if self.path == '/api/sessions/workspace/sync/manifest?project=project':
                return self.response({'project': 'project', 'project_revision': 1, 'project_path': '.', 'manifest': manifest, 'digest': digest})
            self.response({}, 404)

        def do_POST(self):
            if self.headers.get('Authorization') != 'Bearer ' + TOKEN or self.path != '/api/sessions/workspace/sync/lease':
                return self.response({}, 403)
            length = int(self.headers.get('Content-Length', '0'))
            if length < 1 or length > 4096:
                return self.response({}, 400)
            data = json.loads(self.rfile.read(length))
            if data['action'] == 'release':
                return self.response({'ok': True})
            self.response({'workspace': 'workspace', 'project': 'project', 'device': data['device'], 'token': 'fixture-token', 'generation': 'fixture-generation', 'path': '.', 'expires_at': (datetime.now(timezone.utc) + timedelta(seconds=30)).isoformat()})

    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f'http://127.0.0.1:{server.server_port}'
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def sidecar_command(sidecar, state, server, kind, **fields):
    process = subprocess.Popen([str(sidecar), '--desktop'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, encoding='utf-8', env=runtime_environment())
    replies = queue.Queue()

    def read():
        try:
            for line in process.stdout:
                if len(line) > 4 << 20:
                    raise ValueError('Sidecar response too large')
                replies.put(json.loads(line))
        except Exception as error:
            replies.put(error)
        finally:
            replies.put(EOFError('Sidecar exited before replying'))

    reader = threading.Thread(target=read, daemon=True)
    reader.start()

    def receive():
        value = replies.get(timeout=30)
        if isinstance(value, Exception):
            raise value
        return value

    try:
        if receive().get('type') != 'ready':
            raise ValueError('Missing sidecar handshake')
        request = {'version': 1, 'id': 'upgrade-probe', 'type': kind, 'server': server, 'token': TOKEN, 'user': USER, 'state_dir': str(state), **fields}
        process.stdin.write(json.dumps(request) + '\n')
        process.stdin.flush()
        value = receive()
        if value.get('id') != request['id'] or value.get('type') == 'error':
            raise ValueError(f'Sidecar rejected {kind}: {value.get("error", "protocol")}')
        process.stdin.close()
        process.wait(timeout=10)
        if process.returncode:
            raise ValueError('Sidecar did not exit cleanly')
        return value
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=10)
        if not process.stdin.closed:
            process.stdin.close()
        reader.join(timeout=5)
        process.stdout.close()


def state_snapshot(state):
    with closing(sqlite3.connect(state / 'sync.db')) as database, database:
        if database.execute('PRAGMA integrity_check').fetchone() != ('ok',):
            raise ValueError('Sync state failed SQLite integrity check')
        return {'schema': database.execute('PRAGMA user_version').fetchone()[0], **{name: database.execute(f'SELECT * FROM {name} ORDER BY 1,2').fetchall() for name in ('metadata', 'bindings', 'batches')}}


def seed_state(sidecar, directory, server):
    state = directory / '独立 sync state'
    local = directory / '本地 project'
    state.mkdir(mode=0o700)
    local.mkdir()
    (local / '保留.txt').write_bytes(CONTENT)
    saved = sidecar_command(sidecar, state, server, 'sync_bind', directory=str(local), binding={'workspace': 'workspace', 'project': 'project', 'project_path': '.'})['binding']
    preview = sidecar_command(sidecar, state, server, 'sync_preview', binding_id=saved['id'], direction='automatic')['preview']
    if preview['plan'].get('operations') or preview['plan'].get('conflicts'):
        raise ValueError('Fixture unexpectedly requires a file mutation')
    sidecar_command(sidecar, state, server, 'sync_apply', binding_id=saved['id'], preview=preview, confirmation=preview['plan']['digest'])
    before = state_snapshot(state)
    if len(before['bindings']) != 1 or len(before['batches']) != 1 or not json.loads(before['bindings'][0][2]).get('baseline'):
        raise ValueError('Old sidecar did not create a baseline and completed history')
    return state, local, saved['id'], before


def verify_retention(sidecar, state, local, binding_id, before, server):
    for _ in range(2):
        result = sidecar_command(sidecar, state, server, 'sync_list')
        if [binding['id'] for binding in result.get('bindings', [])] != [binding_id]:
            raise ValueError('New sidecar did not retain the binding')
        preview = sidecar_command(sidecar, state, server, 'sync_preview', binding_id=binding_id, direction='automatic')['preview']
        if not preview['baseline_current'] or preview['plan'].get('operations') or preview['plan'].get('conflicts'):
            raise ValueError('New sidecar lost the baseline')
    after = state_snapshot(state)
    if after['schema'] < before['schema'] or any(before[key] != after[key] for key in ('metadata', 'bindings', 'batches')):
        raise ValueError('Upgrade changed device, binding, baseline or history')
    if (local / '保留.txt').read_bytes() != CONTENT:
        raise ValueError('Upgrade changed local project bytes')
    return {'status': 'passed', 'schema_before': before['schema'], 'schema_after': after['schema'], 'schema_migrated': before['schema'] != after['schema'], 'device_binding_baseline_history': True, 'restart_readback': True, 'real_user_profile': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('package', type=Path)
    parser.add_argument('--previous', type=Path, help='Actual older .app or NSIS installer; omitted upgrades are reported skipped')
    parser.add_argument('--expected-version')
    parser.add_argument('--disposable-windows-user', action='store_true')
    parser.add_argument('--report', type=Path)
    args = parser.parse_args()
    report = {'installed_probe': 'failed'}
    guard = None
    try:
        check_windows_isolation(args.disposable_windows_user)
        package = args.package.resolve()
        with tempfile.TemporaryDirectory(prefix='agentbox-install-') as temporary, fixture_peer() as server:
            directory = Path(temporary).resolve()
            installed = directory / 'Agentbox 安装测试'
            installed.mkdir()
            # Snapshot once: --previous then current are both this fixture's
            # owned installations, never a pre-existing user application.
            if os.name == 'nt':
                guard = WindowsInstallGuard(installed)
            try:
                old = None
                if args.previous:
                    executable = install(args.previous.resolve(), installed, guard)
                    old = diagnostics(executable, directory, 'previous')
                    seeded = seed_state(executable.with_name('abox-sync.exe' if os.name == 'nt' else 'abox-sync'), directory, server)
                executable = install(package, installed, guard)
                current = diagnostics(executable, directory, 'current')
                if args.expected_version and current['version'] != args.expected_version:
                    raise ValueError('Installed version does not match reviewed candidate')
                upgrade = {'status': 'skipped', 'reason': 'No actual previous package supplied'}
                if old:
                    if (old['os'], old['arch']) != (current['os'], current['arch']) or semver(old['version']) >= semver(current['version']):
                        raise ValueError('Upgrade requires an older version of the same OS and architecture')
                    state, local, binding_id, before = seeded
                    retention = verify_retention(executable.with_name('abox-sync.exe' if os.name == 'nt' else 'abox-sync'), state, local, binding_id, before, server)
                    upgrade = {'status': 'passed', 'previous_version': old['version'], 'current_version': current['version'], 'installation': 'nsis' if os.name == 'nt' else 'bundle_replacement', 'sync_state': retention}
                report.update(installed_probe='passed', unicode_path=True, cold_and_repeat=True,
                              version=current['version'], platform=current['os'], arch=current['arch'], upgrade=upgrade,
                              gui_ime_tested=False, gui_settings_retention_tested=False, keyring_retention_tested=False,
                              updater_installation_tested=False, interrupted_installation_tested=False, os_signature_verified=False)
            except Exception as error:
                report['error'] = str(error)
                raise
            finally:
                if guard is not None:
                    guard.cleanup()
    except Exception as error:
        report['installed_probe'] = 'failed'
        report.setdefault('error', str(error))
    finally:
        if guard is not None:
            report['windows_installation'] = guard.report
        if args.report:
            write_report(args.report, report)
        print(json.dumps(report, indent=2))
    if report['installed_probe'] != 'passed':
        raise SystemExit(1)


if __name__ == '__main__':
    main()
