#!/usr/bin/env python3
"""Release-to-release upgrade and rollback drill on synthetic data.

Installs two real Linux release packages into an isolated temporary layout with
each package's own deploy/release.py: activate the previous release, populate
synthetic users/workspaces/ledger, upgrade with release.activate, then roll back
by restoring the compatible pre-upgrade system backup to a NEW directory, carrying
over the workspace files, and activating the previous release again.

systemctl is simulated (the binary runs as a child process) and a stub Docker
Engine API reports no containers, so no session containers, real systemd, models,
providers or production data are involved. Workspace directories are chowned to
1000:1000 by the product, so run as root or UID 1000.
"""
import argparse
import contextlib
import datetime
import hashlib
import http.server
import importlib.util
import json
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from unittest.mock import patch
import urllib.request

ADMIN_PASSWORD = 'rollback-drill-synthetic-admin-password'
ALICE_PASSWORD = 'rollback-drill-synthetic-alice-password'
COMPARED_TABLES = ('users', 'quotas', 'credit_ledger', 'usage_events')
SESSION_KEYS = ('id', 'user', 'name', 'agent', 'account_id', 'created_at')


class DockerStub(http.server.ThreadingHTTPServer):
    """Minimal loopback Engine API: answers ping/version, reports no containers or images."""
    daemon_threads = True

    def __init__(self):
        self.requests = []
        super().__init__(('127.0.0.1', 0), DockerHandler)


class DockerHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, status, body):
        data = json.dumps(body).encode() if body is not None else b''
        self.send_response(status)
        self.send_header('API-Version', '1.45')
        self.send_header('OSType', 'linux')
        self.send_header('Content-Type', 'application/json' if body is not None else 'text/plain')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(data)

    def handle_any(self):
        path = self.path.split('?')[0]
        self.server.requests.append(self.command + ' ' + path)
        tail = path.split('/', 2)[-1] if path.startswith('/v1.') else path.lstrip('/')
        if tail == '_ping':
            self.send_response(200)
            for key, value in (('API-Version', '1.45'), ('OSType', 'linux'), ('Content-Length', '2')):
                self.send_header(key, value)
            self.end_headers()
            if self.command != 'HEAD':
                self.wfile.write(b'OK')
        elif tail == 'version':
            self.reply(200, {'ApiVersion': '1.45', 'Version': 'rollback-drill-stub', 'Os': 'linux', 'Arch': 'amd64'})
        elif tail in ('containers/json', 'images/json', 'networks'):
            self.reply(200, [])
        else:
            self.reply(404, {'message': 'rollback drill stub: no such object'})

    do_GET = do_HEAD = do_POST = do_DELETE = do_PUT = handle_any


def load_release(package):
    spec = importlib.util.spec_from_file_location('release_' + package.name.replace('.', '_'), package / 'deploy/release.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def unpack(source, destination):
    source = Path(source).resolve()
    if source.is_dir():
        return source
    with tarfile.open(source) as archive:
        for member in archive.getmembers():
            if member.name.startswith('/') or '..' in Path(member.name).parts or not (member.isfile() or member.isdir()):
                raise SystemExit('unexpected archive member: ' + member.name)
        archive.extractall(destination)
    roots = [p for p in destination.iterdir() if p.is_dir()]
    if len(roots) != 1:
        raise SystemExit('expected one top-level directory in ' + str(source))
    return roots[0]


def sha256(path):
    digest = hashlib.sha256()
    with open(path, 'rb') as f:
        while chunk := f.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


class Drill:
    def __init__(self, base, old, new, files, file_bytes):
        self.base, self.old, self.new = base, old, new
        self.files, self.file_bytes = files, file_bytes
        self.app, self.units, self.backups = base / 'app', base / 'units', base / 'backups'
        self.config = base / 'etc/config.json'
        self.port = free_port()
        self.url = f'http://127.0.0.1:{self.port}'
        self.process = self.docker = self.log = None
        self.logs = base / 'logs'
        self.steps, self.checks = [], []
        self.old_meta = json.loads((old / 'build.json').read_text())
        self.new_meta = json.loads((new / 'build.json').read_text())

    # --- simulated service -------------------------------------------------
    def systemctl(self, action):
        if action == 'stop' and self.process is not None:
            self.process.terminate()
            self.process.wait(timeout=60)
            self.process = None
            self.log.close()
        elif action == 'start':
            if self.process is not None:
                raise RuntimeError('service already running')
            self.logs.mkdir(exist_ok=True)
            self.log = open(self.logs / f'service-{len(list(self.logs.iterdir())):02d}.log', 'w')
            self.process = subprocess.Popen([str(self.app / 'current/agentbox'), '-config', str(self.config)],
                                            stdout=self.log, stderr=subprocess.STDOUT, env=self.env)
        elif action == 'is-active' and (self.process is None or self.process.poll() is not None):
            raise subprocess.CalledProcessError(3, 'systemctl is-active')

    def patched(self, module):
        actual = module.run

        def run(*argv, capture=False):
            if argv[0] == 'systemctl':
                if argv[1] in ('stop', 'start', 'is-active'):
                    self.systemctl(argv[1])
                return ''
            return actual(*argv, capture=capture)
        return patch.object(module, 'run', side_effect=run)

    def install(self, module, package):
        # Mirrors release.py install without the root/systemd parts: validate the
        # config with the package binary, stage immutably, own the unit, create data.
        with self.patched(module):
            module.run(package / 'agentbox', 'check-config', '--config', self.config)
            version = module.stage(package, self.app)
        self.units.joinpath('agentbox.service').write_text(module.unit(self.app, self.config))
        cfg = json.loads(self.config.read_text())
        module.resolve(self.config.parent, cfg.get('data_dir', 'data')).mkdir(parents=True, exist_ok=True, mode=0o700)
        return version

    def activate(self, module, version):
        with self.patched(module):
            module.activate(self.app, version, self.config, self.backups, self.units)

    # --- helpers -----------------------------------------------------------
    def step(self, name, fn, *args, **kwargs):
        started = time.monotonic()
        try:
            return fn(*args, **kwargs)
        finally:
            self.steps.append({'step': name, 'seconds': round(time.monotonic() - started, 3)})
            print(f'  {name}: {self.steps[-1]["seconds"]}s', flush=True)

    def check(self, name, condition, detail=''):
        self.checks.append({'check': name, 'passed': bool(condition), 'detail': detail})
        print(('  ok   ' if condition else '  FAIL ') + name + (f' ({detail})' if detail else ''), flush=True)
        if not condition:
            raise AssertionError(name + (': ' + detail if detail else ''))

    def api(self, method, path, token=None, body=None, raw=None, content_type=None):
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        headers = {'Origin': self.url}
        if data is not None:
            headers['Content-Type'] = content_type or 'application/json'
        if token:
            headers['Authorization'] = 'Bearer ' + token
        request = urllib.request.Request(self.url + path, data=data, method=method, headers=headers)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=30) as response:
            payload = response.read()
            return json.loads(payload) if payload and response.headers.get_content_type() == 'application/json' else payload

    def login(self, user, password):
        return self.api('POST', '/api/login', body={'username': user, 'password': password})['token']

    def upload(self, token, session, name, content):
        boundary = 'rollback-drill-' + os.urandom(8).hex()
        body = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{name}"\r\n'
                'Content-Type: text/plain\r\n\r\n').encode() + content + f'\r\n--{boundary}--\r\n'.encode()
        return self.api('POST', f'/api/sessions/{session}/upload', token, raw=body,
                        content_type='multipart/form-data; boundary=' + boundary)

    def read_file(self, token, session, name):
        return self.api('GET', f'/api/sessions/{session}/file?path={name}', token)

    def data_dir(self):
        cfg = json.loads(self.config.read_text())
        return (self.config.parent / cfg['data_dir']).resolve()

    def snapshot(self, data):
        uri = (data / 'state.db').as_uri() + '?mode=ro'
        with contextlib.closing(sqlite3.connect(uri, uri=True)) as conn:
            result = {'schema': conn.execute('pragma user_version').fetchone()[0], 'tables': {}}
            for table in COMPARED_TABLES:
                rows = sorted(json.dumps(r, default=str) for r in conn.execute(f'select * from {table}'))
                result['tables'][table] = {'rows': len(rows), 'sha256': hashlib.sha256('\n'.join(rows).encode()).hexdigest()}
            conn.row_factory = sqlite3.Row
            sessions = sorted(json.dumps({k: row[k] for k in SESSION_KEYS}) for row in conn.execute('select * from sessions'))
            result['sessions'] = {'rows': len(sessions), 'sha256': hashlib.sha256('\n'.join(sessions).encode()).hexdigest()}
            return result

    def tree_bytes(self, path):
        return sum(p.stat().st_size for p in path.rglob('*') if p.is_file() and not p.is_symlink())

    # --- scenario ----------------------------------------------------------
    def run(self):
        self.base.mkdir(parents=True, exist_ok=True)
        (self.base / 'etc').mkdir()
        self.units.mkdir()
        creds = self.base / 'credentials/fixture'
        creds.mkdir(parents=True)
        (creds / 'auth.json').write_text('{"synthetic":true}\n')
        self.docker = DockerStub()
        threading.Thread(target=self.docker.serve_forever, daemon=True).start()
        self.env = {k: v for k, v in os.environ.items() if not k.startswith(('AGENTBOX_', 'DOCKER_'))}
        self.env['DOCKER_HOST'] = 'tcp://127.0.0.1:%d' % self.docker.server_address[1]
        os.environ['DOCKER_HOST'] = self.env['DOCKER_HOST']
        cfg = {'listen': f'127.0.0.1:{self.port}', 'auth_token': ADMIN_PASSWORD,
               'data_dir': str(self.base / 'state'), 'cache_dir': str(self.base / 'cache'),
               'agent_image': 'agentbox-agent:rollback-drill-not-pulled', 'timezone': 'UTC',
               'tunnel': {'enabled': False}, 'proxy_bridge': {'bind': f'127.0.0.1:{free_port()}'},
               'accounts': [{'id': 'fixture', 'type': 'codex', 'label': 'Synthetic', 'credentials_dir': str(creds)}]}
        self.config.write_text(json.dumps(cfg, indent=2))
        self.config.chmod(0o600)
        old_release, new_release = load_release(self.old), load_release(self.new)
        old_v, new_v = self.old_meta['version'], self.new_meta['version']

        print(f'Phase A: install and run {old_v}', flush=True)
        self.step('install ' + old_v, self.install, old_release, self.old)
        self.step('first activate ' + old_v, self.activate, old_release, old_v)
        admin = self.login('boxadmin', ADMIN_PASSWORD)
        self.api('POST', '/api/users', admin, {'username': 'alice', 'password': ALICE_PASSWORD})
        self.api('PUT', '/api/users/alice/quota', admin, {'metered': True, 'enforced': True})
        self.api('POST', '/api/users/alice/credits', admin, {'micro_usd': 5_000_000, 'ref': 'rollback-drill-before', 'note': 'synthetic'})
        alice = self.login('alice', ALICE_PASSWORD)
        sessions = [self.api('POST', '/api/sessions', alice, {'name': f'drill-{i}', 'agent': 'codex', 'account_id': 'fixture'})['id'] for i in range(2)]
        self.upload(alice, sessions[0], 'before.txt', b'written before upgrade\n')
        data = self.data_dir()
        for session in sessions:
            root = data / 'users/alice/sessions' / session
            for i in range(self.files):
                (root / 'workspace' / f'synthetic-{i:04d}.bin').write_bytes(os.urandom(self.file_bytes))
            (root / 'chats').mkdir(exist_ok=True)
            (root / 'chats/t1.jsonl').write_text('{"kind":"user","text":"synthetic history"}\n')
        before = self.snapshot(data)
        self.check('previous release created schema', before['schema'] > 0, f'schema {before["schema"]}')
        quota = self.api('GET', '/api/users/alice/quota', admin)

        print(f'Phase B: upgrade {old_v} -> {new_v} with {old_v} release.py', flush=True)
        self.step('install ' + new_v, self.install, old_release, self.new)
        self.step('activate ' + new_v + ' (check, stop, system backup, switch, start, health)', self.activate, old_release, new_v)
        compatible = sorted(self.backups.glob(f'before-{new_v}-*.tar.gz'))
        self.check('activation created a pre-upgrade backup', len(compatible) == 1, compatible[-1].name if compatible else 'missing')
        upgraded = self.snapshot(data)
        self.check('upgrade migrated the database', upgraded['schema'] > before['schema'], f'{before["schema"]} -> {upgraded["schema"]}')
        alice = self.login('alice', ALICE_PASSWORD)
        listed = self.api('GET', '/api/sessions', alice)
        self.check('upgraded service lists existing workspaces', sorted(s['id'] for s in listed) == sorted(sessions))
        self.check('upgraded service reads pre-upgrade file', self.read_file(alice, sessions[0], 'before.txt') == b'written before upgrade\n')
        after_session = self.api('POST', '/api/sessions', alice, {'name': 'after-upgrade', 'agent': 'codex', 'account_id': 'fixture'})['id']
        self.upload(alice, sessions[0], 'after-upgrade.txt', b'written after upgrade\n')
        admin = self.login('boxadmin', ADMIN_PASSWORD)
        self.api('POST', '/api/users/alice/credits', admin, {'micro_usd': 2_000_000, 'ref': 'rollback-drill-after', 'note': 'synthetic'})
        users_bytes = self.tree_bytes(data / 'users')

        print(f'Phase C: roll back {new_v} -> {old_v} with {new_v} release.py', flush=True)
        try:
            self.activate(new_release, old_v)
        except ValueError as error:
            refused = str(error)
        else:
            refused = ''
        self.check('direct activate of the previous release is refused', 'incompatible database' in refused, refused)
        self.check('refusal keeps the upgraded release current', (self.app / 'current').resolve().name == new_v)
        self.check('refusal keeps the service running', self.process is not None and self.process.poll() is None)
        rollback_started = time.monotonic()
        self.step('stop service', self.systemctl, 'stop')
        preserved = self.backups / f'after-upgrade-{new_v}.tar.gz'
        self.step('preserve upgraded state (system backup with ' + new_v + ')', subprocess.run,
                  [str(self.new / 'agentbox'), 'backup', '--config', str(self.config), '--output', str(preserved)], check=True, stdout=subprocess.DEVNULL)
        # Old binaries must refuse the migrated database instead of opening it.
        probe = self.base / 'probe-upgraded'
        subprocess.run([str(self.new / 'agentbox'), 'restore', '--to', str(probe), str(preserved)], check=True, stdout=subprocess.DEVNULL)
        probe_cfg = json.loads((probe / 'config.json').read_text())
        probe_cfg['listen'] = f'127.0.0.1:{free_port()}'
        (probe / 'config.json').write_text(json.dumps(probe_cfg))
        opened = subprocess.run([str(self.old / 'agentbox'), '-config', str(probe / 'config.json')], env=self.env,
                                capture_output=True, text=True, timeout=60)
        self.check(f'{old_v} refuses the upgraded database', opened.returncode != 0 and 'schema' in (opened.stdout + opened.stderr).lower(),
                   (opened.stdout + opened.stderr).strip().splitlines()[-1][:200] if (opened.stdout + opened.stderr).strip() else f'exit {opened.returncode}')
        self.check('refused database left unmodified', self.snapshot(probe / 'data')['schema'] == upgraded['schema'])
        self.step('verify compatible backup with ' + old_v, subprocess.run,
                  [str(self.old / 'agentbox'), 'backup-verify', str(compatible[-1])], check=True, stdout=subprocess.DEVNULL)
        restored = self.base / f'state-rollback-{old_v}'
        self.step('restore compatible backup to a new directory with ' + old_v, subprocess.run,
                  [str(self.old / 'agentbox'), 'restore', '--to', str(restored), str(compatible[-1])], check=True, stdout=subprocess.DEVNULL)
        self.step('move workspace files into the restored data directory', self.move_users, data, restored / 'data')
        self.step('point the service config at the restored instance', self.rewrite_config, restored, new_v)
        self.step(f'activate {old_v} (check, stop, system backup, switch, start, health)', self.activate, new_release, old_v)
        rollback_seconds = round(time.monotonic() - rollback_started, 3)
        self.steps.append({'step': 'rollback total (stop -> previous release healthy)', 'seconds': rollback_seconds})
        print(f'  rollback total: {rollback_seconds}s', flush=True)

        restored_data = self.data_dir()
        after = self.snapshot(restored_data)
        self.check('previous release is current', (self.app / 'current').resolve().name == old_v)
        self.check('restored database keeps the previous schema', after['schema'] == before['schema'], str(after['schema']))
        self.check('users, quotas, ledger and usage match the pre-upgrade state', after['tables'] == before['tables'])
        self.check('pre-upgrade workspaces match', after['sessions'] == before['sessions'])
        alice = self.login('alice', ALICE_PASSWORD)
        listed = sorted(s['id'] for s in self.api('GET', '/api/sessions', alice))
        self.check('workspace created after upgrade is not in the restored database', listed == sorted(sessions))
        self.check('its files are kept as an unlisted directory', (restored_data / 'users/alice/sessions' / after_session).is_dir())
        self.check('pre-upgrade file readable', self.read_file(alice, sessions[0], 'before.txt') == b'written before upgrade\n')
        self.check('post-upgrade file carried over', self.read_file(alice, sessions[0], 'after-upgrade.txt') == b'written after upgrade\n')
        admin = self.login('boxadmin', ADMIN_PASSWORD)
        balance = self.api('GET', '/api/users/alice/quota', admin)['quota']['balance_micro_usd']
        self.check('balance equals the pre-upgrade balance (post-upgrade grant lost)',
                   balance == quota['quota']['balance_micro_usd'] == 5_000_000, str(balance))
        verified = json.loads(subprocess.check_output([str(self.new / 'agentbox'), 'backup-verify', str(preserved)]))
        self.check('upgraded state preserved for later inspection', verified.get('valid') is True, preserved.name)
        return {'rollback_seconds': rollback_seconds, 'users_tree_bytes': users_bytes,
                'workspaces_before': len(sessions), 'files_per_workspace': self.files + (1 if self.files else 0),
                'snapshots': {'before': before, 'upgraded': upgraded, 'after_rollback': after}}

    def move_users(self, data, restored_data):
        # A system backup holds only user templates/MCP files; the live tree is
        # newer. Rename within one filesystem: no copy, no extra disk.
        if os.stat(data).st_dev != os.stat(restored_data).st_dev:
            raise RuntimeError('restore directory must be on the data filesystem to move users/')
        if (restored_data / 'users').exists():
            os.rename(restored_data / 'users', restored_data / 'users.from-backup')
        os.rename(data / 'users', restored_data / 'users')

    def rewrite_config(self, restored, new_v):
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
        shutil.copy2(self.config, self.config.with_name(f'config.json.{new_v}-{stamp}'))
        cfg = json.loads((restored / 'config.json').read_text())
        cfg['data_dir'] = str((restored / cfg['data_dir']).resolve())
        if cfg.get('cache_dir'):
            cfg['cache_dir'] = str((restored / cfg['cache_dir']).resolve())
        for account in cfg.get('accounts', []):
            if account.get('credentials_dir') and not os.path.isabs(account['credentials_dir']):
                account['credentials_dir'] = str((restored / account['credentials_dir']).resolve())
        temp = self.config.with_suffix('.tmp')
        temp.write_text(json.dumps(cfg, indent=2))
        temp.chmod(0o600)
        os.replace(temp, self.config)
        subprocess.run([str(self.old / 'agentbox'), 'check-config', '--config', str(self.config)], check=True, stdout=subprocess.DEVNULL)

    def close(self):
        with contextlib.suppress(Exception):
            self.systemctl('stop')
        with contextlib.suppress(Exception):
            self.docker.shutdown()
            self.docker.server_close()


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--old', required=True, help='previous release: Linux package .tar.gz or unpacked directory')
    p.add_argument('--new', required=True, help='candidate release: Linux package .tar.gz or unpacked directory')
    p.add_argument('--files-per-workspace', type=int, default=20)
    p.add_argument('--file-bytes', type=int, default=4096)
    p.add_argument('--report', type=Path, help='write the JSON report here (must not exist)')
    p.add_argument('--keep', action='store_true', help='keep the temporary layout for inspection')
    a = p.parse_args()
    if sys.platform != 'linux':
        raise SystemExit('Linux release packages only')
    if os.geteuid() not in (0, 1000):
        raise SystemExit('run as root or UID 1000: the product chowns workspace directories to 1000:1000')
    if a.report and a.report.exists():
        raise SystemExit('report already exists; choose a new path')
    temp = Path(tempfile.mkdtemp(prefix='agentbox-rollback-drill-'))
    started = time.monotonic()
    drill = None
    status, error = 'failed', None
    try:
        old, new = unpack(a.old, temp / 'old'), unpack(a.new, temp / 'new')
        drill = Drill(temp / 'run', old, new, a.files_per_workspace, a.file_bytes)
        print(f'Rollback drill: {drill.old_meta["version"]} <-> {drill.new_meta["version"]} in {temp}', flush=True)
        result = drill.run()
        status = 'passed'
    except Exception as e:
        error = f'{type(e).__name__}: {e}'
        print('FAILED: ' + error, flush=True)
        result = {}
        logs = sorted(drill.logs.glob('service-*.log')) if drill else []
        if logs:
            result['service_log_tail'] = logs[-1].read_text(errors='replace').splitlines()[-40:]
    finally:
        if drill is not None:
            drill.close()
    report = {'format_version': 1, 'status': status, 'error': error,
              'finished_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'elapsed_seconds': round(time.monotonic() - started, 3),
              'scope': 'Real release binaries and release.py; simulated systemctl; stub Docker API without containers; synthetic data only.',
              'host': {'platform': sys.platform, 'uid': os.geteuid(), 'machine': os.uname().machine, 'cpus': os.cpu_count()},
              'packages': {} if drill is None else {
                  'old': drill.old_meta | {'agentbox_sha256': sha256(drill.old / 'agentbox')},
                  'new': drill.new_meta | {'agentbox_sha256': sha256(drill.new / 'agentbox')}},
              'steps': [] if drill is None else drill.steps, 'checks': [] if drill is None else drill.checks,
              'docker_stub_requests': sorted(set(drill.docker.requests)) if drill and drill.docker else [], **result}
    if a.report:
        a.report.parent.mkdir(parents=True, exist_ok=True)
        with open(a.report, 'x') as f:
            json.dump(report, f, ensure_ascii=False, indent=2)
        print('Report: ' + str(a.report))
    if a.keep:
        print('Kept: ' + str(temp))
    else:
        shutil.rmtree(temp, ignore_errors=True)
    return 0 if status == 'passed' else 1


if __name__ == '__main__':
    sys.exit(main())
