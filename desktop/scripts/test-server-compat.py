#!/usr/bin/env python3
"""Frozen server/client compatibility against disposable Linux Docker fixtures.

Builds an immutable git archive (default eb845db, server schema 9) and the current
server. Requires a prebuilt local agent image with bash and tmux. No provider
calls or production data are used. Only the trusted test server has Docker socket
access; workspace containers have network=none. A full backup uses a metadata-
preserving copy of the stopped fixture in an isolated maintenance container.

The browser checks exercise the old HTTP/WebSocket contract and asset URLs, not
an interactive old browser. --desktop-smoke additionally runs an explicit native
desktop-smoke build against the actual frozen server before upgrading it.
"""

import argparse
import base64
import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import re
import secrets
import socket
import sqlite3
import struct
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile


ROOT = Path(__file__).resolve().parents[2]
BASELINE = 'eb845db59e45ed042b1af9df44dae96f104bf43b'


def run(*args, **kwargs):
    return subprocess.check_output(args, text=True, timeout=90, **kwargs).strip()


def docker(*args):
    return run('docker', *args)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


class HTTP:
    def __init__(self, base):
        self.base = base
        self.token = ''
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(self, path, body=None, method=None, raw=False, content_type=None):
        headers = {'Authorization': 'Bearer ' + self.token}
        if isinstance(body, bytes):
            data = body
        elif body is not None:
            data = json.dumps(body).encode()
            content_type = 'application/json'
        else:
            data = None
        if content_type:
            headers['Content-Type'] = content_type
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        with self.opener.open(req, timeout=65) as response:
            data = response.read()
            if not raw and response.headers.get_content_type() == 'application/json':
                return json.loads(data)
            return data

    def ready(self):
        deadline = time.monotonic() + 30
        while True:
            try:
                self.request('/api/ping')
                return
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.2)


class WebSocket:
    """Small bounded client for the existing browser terminal/chat wire protocol."""

    def __init__(self, http, path):
        url = urllib.parse.urlsplit(http.base)
        self.socket = socket.create_connection((url.hostname, url.port), timeout=10)
        self.buffer = b''
        key = base64.b64encode(secrets.token_bytes(16)).decode()
        self.socket.sendall((f'GET {path} HTTP/1.1\r\nHost: {url.netloc}\r\n'
                             f'Authorization: Bearer {http.token}\r\nUpgrade: websocket\r\n'
                             'Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n'
                             f'Sec-WebSocket-Key: {key}\r\n\r\n').encode())
        while b'\r\n\r\n' not in self.buffer:
            chunk = self.socket.recv(4096)
            if not chunk:
                self.socket.close()
                raise EOFError('WebSocket closed during upgrade')
            self.buffer += chunk
            require(len(self.buffer) < 65536, 'oversized WebSocket upgrade')
        header, self.buffer = self.buffer.split(b'\r\n\r\n', 1)
        require(header.startswith(b'HTTP/1.1 101 '), 'WebSocket upgrade failed')
        accept = base64.b64encode(hashlib.sha1((key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
        require(b'sec-websocket-accept: ' + accept.lower() in header.lower(), 'invalid WebSocket accept')

    def read(self, size):
        while len(self.buffer) < size:
            chunk = self.socket.recv(max(4096, size - len(self.buffer)))
            if not chunk:
                raise EOFError('WebSocket closed')
            self.buffer += chunk
        result, self.buffer = self.buffer[:size], self.buffer[size:]
        return result

    def send(self, opcode, payload):
        require(len(payload) < 65536, 'test frame too large')
        size = len(payload)
        header = bytes([0x80 | opcode, 0x80 | size]) if size < 126 else bytes([0x80 | opcode, 0xFE]) + struct.pack('!H', size)
        mask = secrets.token_bytes(4)
        self.socket.sendall(header + mask + bytes(value ^ mask[i % 4] for i, value in enumerate(payload)))

    def frame(self):
        first, size = self.read(2)
        require(not size & 0x80, 'server WebSocket frame masked')
        if size == 126:
            size = struct.unpack('!H', self.read(2))[0]
        elif size == 127:
            size = struct.unpack('!Q', self.read(8))[0]
        require(size <= 4 << 20, 'oversized WebSocket frame')
        return first & 0x0F, self.read(size)

    def terminal_echo(self):
        marker = ('compat-' + uuid.uuid4().hex).encode()
        # Octal input cannot satisfy the expected output merely through PTY echo.
        command = "printf '" + ''.join('\\%03o' % b for b in marker) + "\\n'\r"
        self.send(1, json.dumps({'type': 'resize', 'cols': 100, 'rows': 30}).encode())
        self.send(2, command.encode())
        output = b''
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            opcode, payload = self.frame()
            if opcode == 9:
                self.send(10, payload)
            elif opcode == 8:
                raise AssertionError('terminal closed before shell output')
            elif opcode == 2:
                output = (output + payload)[-(1 << 20):]
                if marker in output:
                    return
        raise AssertionError('terminal shell output timed out')

    def close(self):
        self.socket.close()


class Fixture:
    def __init__(self, image, temporary):
        self.image = image
        self.temporary = temporary
        self.volume = 'agentbox-compat-' + uuid.uuid4().hex
        self.containers = []
        self.sessions = set()
        self.server = None
        self.password = secrets.token_urlsafe(32)
        self.http = None
        docker('volume', 'create', self.volume)
        self.data = docker('volume', 'inspect', '--format', '{{.Mountpoint}}', self.volume)
        self.config = temporary / 'fixture.json'
        self.config.write_text(json.dumps({
            'listen': '0.0.0.0:8180', 'auth_token': self.password, 'data_dir': self.data,
            'timezone': 'UTC', 'agent_image': image,
            'container': {'network': 'none', 'memory_mb': 512, 'cpus': 1, 'pids_limit': 128},
            'tunnel': {'enabled': True, 'transparent': False, 'proxy_bind': '127.0.0.1:1080'},
            'accounts': [{'id': 'synthetic', 'type': 'codex', 'label': 'Synthetic compatibility account'}],
        }))
        self.config.chmod(0o600)

    def launch(self, binary):
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
        self.server = docker('create', '--mount', f'type=volume,src={self.volume},dst={self.data}',
                             '--mount', 'type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock',
                             '-p', f'127.0.0.1:{port}:8180', '--entrypoint', '/opt/agentbox', 'alpine:3',
                             '--config', '/tmp/config.json')
        self.containers.append(self.server)
        docker('cp', str(binary), self.server + ':/opt/agentbox')
        docker('cp', str(self.config), self.server + ':/tmp/config.json')
        docker('start', self.server)
        port = json.loads(docker('inspect', self.server))[0]['NetworkSettings']['Ports']['8180/tcp'][0]['HostPort']
        token = self.http.token if self.http else ''
        self.http = HTTP('http://127.0.0.1:' + port)
        self.http.token = token
        self.http.ready()

    def stop(self):
        docker('stop', '--time', '15', self.server)
        state = json.loads(docker('inspect', self.server))[0]['State']
        require(state['ExitCode'] == 0, 'server did not exit gracefully')

    def database(self, name):
        # Only called after server shutdown/checkpoint; never copy a live WAL DB.
        target = self.temporary / (name + '.db')
        docker('cp', self.server + ':' + self.data + '/state.db', str(target))
        return sqlite3.connect('file:' + str(target) + '?mode=ro', uri=True)

    def cleanup(self):
        for container in [*self.sessions, *reversed(self.containers)]:
            subprocess.run(['docker', 'rm', '-f', container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
        subprocess.run(['docker', 'volume', 'rm', self.volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)


def unicode_file_scope_contract(fixture, session, phase, scope):
    """Encoded paths and multipart names must survive both legacy file scopes."""
    http = fixture.http
    path = '/api/sessions/' + session
    name = '报告 +50% #&.bin'

    def directory(stage):
        return '兼容目录 +50% #& (' + stage + ')'

    def contents(stage):
        return ('中文文件 ' + scope + ' ' + stage + '\r\n').encode() + b'\0\xff'

    if phase == 'after upgrade':
        previous = urllib.parse.urlencode({'scope': scope, 'path': directory('before upgrade') + '/' + name, 'dl': '1'})
        require(http.request(path + '/file?' + previous, raw=True) == contents('before upgrade'),
                scope + ' Unicode file bytes did not survive upgrade')
    folder = directory(phase)
    created = http.request(path + '/files/mkdir', {'scope': scope, 'dir': '', 'name': folder}, 'POST')
    require(created['path'] == folder, scope + ' Unicode directory name changed')
    query = urllib.parse.urlencode({'scope': scope, 'path': folder})
    boundary = 'compat-' + uuid.uuid4().hex
    payload = contents(phase)
    upload = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{name}"\r\n'
              'Content-Type: application/octet-stream\r\n\r\n').encode() + payload + f'\r\n--{boundary}--\r\n'.encode()
    http.request(path + '/upload?' + query, upload, 'POST', content_type='multipart/form-data; boundary=' + boundary)
    entries = http.request(path + '/files?' + query)
    require(len(entries) == 1 and entries[0]['name'] == name and entries[0]['size'] == len(payload),
            scope + ' Unicode file listing changed')
    relative = folder + '/' + name
    query = urllib.parse.urlencode({'scope': scope, 'path': relative, 'dl': '1'})
    require(http.request(path + '/file?' + query, raw=True) == payload, scope + ' Unicode download bytes changed')
    with zipfile.ZipFile(io.BytesIO(http.request(path + '/archive?' + urllib.parse.urlencode({'scope': scope}), raw=True))) as archive:
        require(archive.read(relative) == payload, scope + ' Unicode archive path or bytes changed')
    container_path = ('/shared/' if scope == 'shared' else '/workspace/') + relative
    # Read as the actual unprivileged agent, not through the root HTTP process.
    container = 'agentbox-' + session
    require(subprocess.check_output(['docker', 'exec', '--user', '1000:1000', container, 'cat', container_path], timeout=15) == payload,
            scope + ' Unicode upload not readable by agent')
    require(docker('exec', container, 'stat', '-c', '%u:%g', container_path) == '1000:1000',
            scope + ' Unicode upload ownership changed')


def legacy_contract(fixture, session, phase):
    http = fixture.http
    path = '/api/sessions/' + session
    require(http.request('/api/me')['role'] == 'admin', 'old token lost')
    require(any(item['id'] == session for item in http.request('/api/sessions')), 'session missing')
    require(any(item['id'] == 'synthetic' for item in http.request('/api/accounts')), 'account binding lost')
    require(http.request(path)['account_id'] == 'synthetic', 'session account changed')
    contents = ('旧版网页兼容 ' + phase + '\r\n').encode()
    http.request(path + '/file?path=compat.txt', contents, 'PUT', content_type='text/plain;charset=utf-8')
    require(http.request(path + '/file?path=compat.txt', raw=True) == contents, 'legacy file save/download changed')
    require(any(item['name'] == 'compat.txt' for item in http.request(path + '/files?path=')), 'legacy file listing changed')
    payload = b'legacy-upload\r\n\0\xff'
    boundary = 'compat-' + uuid.uuid4().hex
    upload = (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="legacy.bin"\r\n'
              'Content-Type: application/octet-stream\r\n\r\n').encode() + payload + f'\r\n--{boundary}--\r\n'.encode()
    http.request(path + '/upload?path=', upload, 'POST', content_type='multipart/form-data; boundary=' + boundary)
    require(http.request(path + '/file?path=legacy.bin&dl=1', raw=True) == payload, 'legacy multipart bytes changed')
    with zipfile.ZipFile(io.BytesIO(http.request(path + '/archive', raw=True))) as archive:
        require(archive.read('compat.txt') == contents and archive.read('legacy.bin') == payload, 'workspace archive changed')
    for scope in ('workspace', 'shared'):
        unicode_file_scope_contract(fixture, session, phase, scope)
    thread = http.request(path + '/chat/threads', {}, 'POST')['id']
    require(http.request(path + '/chat/threads')['active'] == thread, 'legacy thread selection changed')
    ws = WebSocket(http, path + '/term')
    try:
        ws.terminal_echo()
    finally:
        ws.close()
    return contents


def native_smoke(binary, fixture, session, temporary):
    path = '/api/sessions/' + session
    # Fixture-only layout: tmux's status row would otherwise make the shell's
    # stty rows one less than the WebView terminal. Keep strict resize checks.
    docker('exec', '--user', '1000:1000', 'agentbox-' + session, 'tmux', 'set-option', '-g', 'status', 'off')
    fixture.http.request(path + '/file?path=directory-upload.bin', b'existing directory fixture', 'PUT')
    report = temporary / 'native-report.json'
    compat = json.dumps({'server': fixture.http.base + '/', 'username': 'boxadmin',
                         'password': fixture.password, 'session': session})
    env = dict(os.environ, AGENTBOX_SMOKE_COMPAT=compat, AGENTBOX_SMOKE_REPORT=str(report))
    for key in ('AGENTBOX_SMOKE_SYNC', 'AGENTBOX_SMOKE_PROJECTS'):
        env.pop(key, None)
    def diagnostic():
        stages = report.with_suffix('.stages')
        values = stages.read_text()[-4096:].splitlines() if stages.exists() else []
        safe = {'stages': [value for value in values if re.fullmatch('[a-z_]+', value)]}
        if report.exists() and report.stat().st_size <= 65536:
            try:
                value = json.loads(report.read_text())
                for field in ('message', 'compat_error'):
                    if isinstance(value.get(field), str):
                        safe[field] = value[field].replace(fixture.password, '<redacted>').replace(fixture.http.token, '<redacted>')[:512]
            except (ValueError, OSError):
                pass
        return json.dumps(safe, ensure_ascii=False)
    log = temporary / 'native.log'
    try:
        with log.open('w') as output:
            subprocess.run([str(binary.resolve())], env=env, stdout=output, stderr=output, check=True, timeout=90)
    except subprocess.SubprocessError as error:
        raise RuntimeError('native compatibility smoke failed: ' + diagnostic()) from error
    result = json.loads(report.read_text())
    require(result.get('ok') and result.get('compat_mode') and result.get('legacy_readback'),
            'native frozen-server smoke failed: ' + diagnostic())
    require(fixture.http.request(path + '/file?path=directory-upload.bin', raw=True) == b'directory-upload\r\n\0',
            'native directory upload bytes changed')
    attachment = result.get('attachment_path', '')
    require(attachment.startswith('/shared/.file/'), 'unexpected native attachment path')
    name = attachment.removeprefix('/shared/.file/')
    require(bool(name) and name not in ('.', '..') and '/' not in name and '\\' not in name,
            'unsafe native attachment name')
    entries = fixture.http.request(path + '/files?scope=shared&path=.file')
    require(sum(item['name'] == name for item in entries) == 1, 'native attachment missing or duplicated')
    query = urllib.parse.urlencode({'scope': 'shared', 'path': '.file/' + name})
    require(fixture.http.request(path + '/file?' + query, raw=True) == b'attachment\r\n\0', 'native attachment bytes changed')
    for source, expected in ((attachment, b'attachment\r\n\0'), ('/workspace/directory-upload.bin', b'directory-upload\r\n\0')):
        actual = subprocess.check_output(['docker', 'exec', 'agentbox-' + session, 'cat', source], timeout=15)
        require(actual == expected, 'container native upload bytes changed')
    return result


def full_backup(fixture, binary, old_binary, session, contents, project, terminal):
    # A container mounting the volume itself must not be exempted from the real
    # full-backup mount safety check. Copy the fully stopped fixture instead.
    maintenance = docker('run', '-d', '--network', 'none', '--mount',
                         'type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock',
                         '--entrypoint', 'sleep', 'alpine:3', 'infinity')
    fixture.containers.append(maintenance)
    docker('exec', maintenance, 'mkdir', '-p', '/tmp/source/data')
    source = subprocess.Popen(['docker', 'cp', fixture.server + ':' + fixture.data + '/.', '-'], stdout=subprocess.PIPE)
    try:
        subprocess.run(['docker', 'cp', '-a', '-', maintenance + ':/tmp/source/data'], stdin=source.stdout, check=True, timeout=60)
        source.stdout.close()
        require(source.wait(timeout=60) == 0, 'offline fixture copy failed')
    finally:
        if source.poll() is None:
            source.kill()
            source.wait()
    config = json.loads(fixture.config.read_text())
    config['data_dir'] = '/tmp/source/data'
    path = fixture.temporary / 'backup-config.json'
    path.write_text(json.dumps(config))
    path.chmod(0o600)
    docker('cp', str(path), maintenance + ':/tmp/source/config.json')
    docker('cp', str(binary), maintenance + ':/opt/agentbox')
    docker('cp', str(old_binary), maintenance + ':/opt/old-agentbox')
    backup = json.loads(docker('exec', maintenance, '/opt/agentbox', 'backup', '--config', '/tmp/source/config.json',
                              '--full', '--output', '/tmp/full.tar.gz'))
    require(backup['mode'] == 'full', 'full backup mode missing')
    verified = json.loads(docker('exec', maintenance, '/opt/agentbox', 'backup-verify', '/tmp/full.tar.gz'))
    require(verified['valid'], 'backup verification failed')
    docker('exec', maintenance, '/opt/agentbox', 'restore', '--to', '/tmp/restored', '/tmp/full.tar.gz')
    archive_path = fixture.temporary / 'full.tar.gz'
    docker('cp', maintenance + ':/tmp/full.tar.gz', str(archive_path))
    with tarfile.open(archive_path) as archive:
        prefix = 'data/users/boxadmin/sessions/' + session + '/workspace/'
        entry = archive.getmember(prefix + 'compat.txt')
        require((entry.uid, entry.gid) == (1000, 1000), 'backup file ownership changed')
        require(archive.extractfile(entry).read() == contents, 'backup workspace bytes changed')
        require(not any(item.name == 'data/client-instance-id' for item in archive), 'backup copied instance identity')
    restored_file = '/tmp/restored/data/users/boxadmin/sessions/' + session + '/workspace/compat.txt'
    require(subprocess.check_output(['docker', 'exec', maintenance, 'cat', restored_file], timeout=15) == contents,
            'restored workspace bytes changed')
    require(docker('exec', maintenance, 'stat', '-c', '%u:%g', restored_file) == '1000:1000',
            'restored file ownership changed')
    restored = fixture.temporary / 'restored.db'
    docker('cp', maintenance + ':/tmp/restored/data/state.db', str(restored))
    with sqlite3.connect('file:' + str(restored) + '?mode=ro', uri=True) as database:
        require(database.execute('PRAGMA integrity_check').fetchone()[0] == 'ok', 'restored database corrupt')
        require(database.execute('PRAGMA user_version').fetchone()[0] == 10, 'restored schema changed')
        require(database.execute('SELECT id FROM client_projects').fetchall() == [(project,)], 'restored project lost')
        require(database.execute('SELECT id FROM client_terminals').fetchall() == [(terminal,)], 'restored terminal lost')
        require(database.execute('SELECT account_id FROM sessions WHERE id=?', (session,)).fetchone() == ('synthetic',),
                'restored session account changed')
    # A schema 9 binary must reject upgraded data instead of silently reopening.
    result = subprocess.run(['docker', 'exec', maintenance, '/opt/old-agentbox', '--config', '/tmp/restored/config.json'],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
    require(result.returncode != 0 and b'unsupported database schema version 10 (maximum 9)' in result.stderr,
            'frozen binary did not reject future schema')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True, help='already-built local agent image (never pulled)')
    parser.add_argument('--baseline', default=BASELINE, help='immutable server/client git revision (schema 9)')
    parser.add_argument('--new-server', type=Path, help='already-built current Linux server; otherwise builds current checkout')
    parser.add_argument('--desktop-smoke', type=Path, help='explicit desktop-smoke bundle executable')
    parser.add_argument('--browser-smoke', action='store_true', help='run actual Chromium with frozen web assets against the new server')
    parser.add_argument('--report', type=Path, help='write a credential-free JSON evidence report')
    args = parser.parse_args()
    docker('image', 'inspect', args.image)
    docker('image', 'inspect', 'alpine:3')
    arch = {'aarch64': 'arm64', 'arm64': 'arm64', 'x86_64': 'amd64', 'amd64': 'amd64'}[docker('info', '--format', '{{.Architecture}}')]
    revision = run('git', 'rev-parse', '--verify', args.baseline + '^{commit}', cwd=ROOT)
    evidence = {'baseline': revision, 'linux_arch': arch, 'checks': [], 'desktop_native': 'not_requested', 'old_browser': 'http_contract_only',
                'limitations': ['old browser HTTP/WebSocket contract, not interactive browser UI',
                                'full backup uses a metadata-preserving copy of the stopped fixture',
                                'no Windows/Intel execution or installer acceptance']}
    fixture = None
    link = None
    target = None
    try:
        with tempfile.TemporaryDirectory(prefix='agentbox-compat-') as directory:
            temporary = Path(directory)
            frozen = temporary / 'frozen'
            frozen.mkdir()
            archive_path = temporary / 'source.tar'
            subprocess.run(['git', 'archive', '--format=tar', '--output=' + str(archive_path), revision], cwd=ROOT, check=True)
            with tarfile.open(archive_path) as archive:
                archive.extractall(frozen, filter='data')
            print('Building frozen server/client and current Linux server…', flush=True)
            linux_env = dict(os.environ, GOOS='linux', GOARCH=arch, CGO_ENABLED='0')
            old_binary = temporary / 'old-agentbox'
            old_link = temporary / 'old-abox-link'
            subprocess.run(['go', 'build', '-trimpath', '-o', str(old_binary), './cmd/agentbox'], cwd=frozen, env=linux_env, check=True)
            native_env = dict(os.environ, CGO_ENABLED='0')
            for key in ('GOOS', 'GOARCH'):
                native_env.pop(key, None)
            subprocess.run(['go', 'build', '-trimpath', '-o', str(old_link), './cmd/abox-link'], cwd=frozen, env=native_env, check=True)
            binary = args.new_server.resolve() if args.new_server else temporary / 'agentbox'
            if not args.new_server:
                subprocess.run(['go', 'build', '-trimpath', '-o', str(binary), './cmd/agentbox'], cwd=ROOT, env=linux_env, check=True)
            evidence['binary_sha256'] = {name: hashlib.sha256(path.read_bytes()).hexdigest()
                                         for name, path in (('frozen_server', old_binary), ('frozen_link', old_link), ('current_server', binary))}
            fixture = Fixture(args.image, temporary)
            fixture.launch(old_binary)
            fixture.http.token = fixture.http.request('/api/login', {'username': 'boxadmin', 'password': fixture.password})['token']
            old_assets = set(re.findall(r'(?:src|href)="(/_v/[^\"]+)"', fixture.http.request('/', raw=True).decode()))
            require(old_assets, 'frozen browser assets not found')
            session = fixture.http.request('/api/sessions', {'name': 'frozen compatibility', 'agent': 'codex', 'account_id': 'synthetic'})['id']
            fixture.sessions.add('agentbox-' + session)
            started = fixture.http.request('/api/sessions/' + session + '/start', {}, 'POST')
            cid = started['container_id']
            fixture.sessions.add(cid)
            inspect = json.loads(docker('inspect', cid))[0]
            require(inspect['HostConfig']['NetworkMode'] == 'none', 'workspace networking enabled')
            require(all(item['Source'].startswith(fixture.data + '/') for item in inspect['Mounts']), 'non-fixture workspace mount')
            legacy_contract(fixture, session, 'before upgrade')
            if args.desktop_smoke:
                native_smoke(args.desktop_smoke, fixture, session, temporary)
                evidence['desktop_native'] = 'passed_against_frozen_server'
                print('Native desktop: frozen server capability fallback, shell/resize and upload bytes passed.', flush=True)
            docker('exec', '--user', '1000:1000', cid, 'tmux', 'new-session', '-d', '-s', 'compat-survivor', 'sleep 300')
            fixture.stop()
            with fixture.database('old') as database:
                old_schema = database.execute('PRAGMA user_version').fetchone()[0]
                require(old_schema == 9, 'expected real baseline schema 9')
                require(not database.execute("SELECT name FROM sqlite_master WHERE name='client_projects'").fetchall(), 'desktop table exists before migration')
            evidence['old_schema'] = old_schema
            evidence['checks'].append('frozen_server_legacy_http_files_archive_threads_terminal')
            evidence['checks'].append('frozen_server_unicode_encoded_paths_multipart_archive_workspace_shared_agent_uid')
            print('Frozen server: schema 9, legacy API and real terminal passed; upgrading…', flush=True)
            fixture.launch(binary)
            migrated = fixture.http.request('/api/sessions/' + session)
            require(migrated['status'] == 'running', 'upgrade did not reconcile running container')
            for field in ('id', 'user', 'agent', 'account_id', 'default_model', 'created_at', 'container_id'):
                require(migrated.get(field) == started.get(field), 'migration changed session ' + field)
            docker('exec', '--user', '1000:1000', cid, 'tmux', 'has-session', '-t', 'compat-survivor')
            capabilities = fixture.http.request('/api/clients/capabilities')
            require(capabilities['features']['sync'] == 0, 'sync became enabled')
            for asset in old_assets:
                require(fixture.http.request(asset, raw=True), 'frozen browser asset URL missing')
            contents = legacy_contract(fixture, session, 'after upgrade')
            evidence['checks'].append('upgraded_server_unicode_files_survive_and_workspace_shared_contract_unchanged')
            if args.browser_smoke:
                browser_input = json.dumps({'server': fixture.http.base + '/', 'username': 'boxadmin',
                                            'password': fixture.password, 'session': session,
                                            'static_root': str(frozen / 'internal/web/static')})
                subprocess.run(['node', str(ROOT / 'desktop/scripts/test-legacy-browser.mjs')], input=browser_input,
                               text=True, check=True, timeout=120)
                evidence['old_browser'] = 'frozen_assets_in_chromium_against_current_server'
                evidence['limitations'].remove('old browser HTTP/WebSocket contract, not interactive browser UI')
            evidence['checks'].append('old_token_account_workspace_tmux_and_old_asset_urls_after_upgrade')
            evidence['old_asset_urls'] = len(old_assets)

            marker = ('tunnel-' + uuid.uuid4().hex).encode()

            class Target(http.server.BaseHTTPRequestHandler):
                def do_GET(self):
                    self.send_response(200)
                    self.send_header('Content-Length', str(len(marker)))
                    self.end_headers()
                    self.wfile.write(marker)

                def log_message(self, *_):
                    pass

            target = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Target)
            threading.Thread(target=target.serve_forever, daemon=True).start()
            target_address = '127.0.0.1:' + str(target.server_port)
            with (temporary / 'link.log').open('w') as log:
                link = subprocess.Popen([str(old_link), '--server', fixture.http.base, '--user', 'boxadmin',
                                         '--transparent=false', '--map', '19090=' + target_address],
                                        env=dict(os.environ, ABOX_PASSWORD=fixture.password), stdout=log, stderr=log)

            def tunnel_roundtrip():
                deadline = time.monotonic() + 25
                while not fixture.http.request('/api/tunnel/status')['connected']:
                    require(link.poll() is None, 'frozen abox-link exited')
                    require(time.monotonic() < deadline, 'frozen abox-link connection timed out')
                    time.sleep(0.2)
                received = docker('exec', fixture.server, 'wget', '-qO-', '-T', '10', 'http://127.0.0.1:19090/')
                require(received.encode() == marker, 'frozen abox-link tunnel bytes changed')

            tunnel_roundtrip()
            # A stop/start must preserve its published port for the old link's
            # unchanged reconnect configuration. Docker start reuses that port.
            chat = WebSocket(fixture.http, '/api/sessions/' + session + '/chat')
            fixture.stop()
            try:
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    opcode, _ = chat.frame()
                    if opcode == 8:
                        break
                else:
                    raise AssertionError('chat WebSocket did not close on shutdown')
            except EOFError:
                pass
            finally:
                chat.close()
            with fixture.database('upgraded') as database:
                schema = database.execute('PRAGMA user_version').fetchone()[0]
                require(schema == 10, 'upgraded database is not schema 10')
                require(database.execute('SELECT COUNT(*) FROM client_projects').fetchone()[0] == 0, 'migration auto-created projects')
                require(database.execute('SELECT COUNT(*) FROM client_terminals').fetchone()[0] == 0, 'migration auto-created terminals')
            evidence['new_schema'] = schema
            docker('start', fixture.server)
            fixture.http.ready()
            tunnel_roundtrip()
            require(fixture.http.request('/api/clients/capabilities')['server_id'] == capabilities['server_id'], 'restart changed server identity')
            require(fixture.http.request('/api/sessions/' + session)['status'] == 'running', 'restart lost session state')
            docker('exec', '--user', '1000:1000', cid, 'tmux', 'has-session', '-t', 'compat-survivor')
            evidence['checks'].append('schema_9_to_10_and_graceful_restart_chat_closed_tmux_preserved')
            evidence['checks'].append('frozen_abox_link_real_tcp_bytes_and_reconnect')
            print('Upgrade/restart: schema 10, old token/API, tmux and frozen abox-link passed.', flush=True)
            link.terminate()
            link.wait(timeout=15)
            link = None
            path = '/api/sessions/' + session
            project = fixture.http.request(path + '/client-projects', {'name': 'compat project', 'path': '.', 'arguments': []})['id']
            terminal = fixture.http.request(path + '/client-terminals', {'project_id': project, 'kind': 'shell'})['id']
            fixture.http.request(path + '/stop', {}, 'POST')
            fixture.stop()
            full_backup(fixture, binary, old_binary, session, contents, project, terminal)
            evidence['checks'].append('offline_full_backup_verify_restore_uid_account_and_desktop_metadata')
            evidence['checks'].append('frozen_schema_9_server_refuses_schema_10_rollback')
            docker('start', fixture.server)
            fixture.http.ready()
            fixture.http.request(path + '?purge=1', method='DELETE')
            require(not fixture.http.request('/api/sessions'), 'purged session still visible')
            fixture.stop()
            with fixture.database('purged') as database:
                for table in ('sessions', 'client_projects', 'client_terminals'):
                    require(database.execute('SELECT COUNT(*) FROM ' + table).fetchone()[0] == 0, 'purge left ' + table)
            # Exact fixture path only; never search or remove daemon data broadly.
            check = subprocess.run(['docker', 'cp', fixture.server + ':' + fixture.data + '/users/boxadmin/sessions/' + session,
                                    str(temporary / 'unexpected-purged-session')], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            require(check.returncode != 0, 'purge left session files')
            evidence['checks'].append('purge_removes_only_fixture_session_files_and_desktop_metadata')
            evidence['ok'] = True
            print(json.dumps(evidence, ensure_ascii=False, indent=2))
            if args.report:
                args.report.write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + '\n')
    finally:
        if link is not None and link.poll() is None:
            link.terminate()
            try:
                link.wait(timeout=10)
            except subprocess.TimeoutExpired:
                link.kill()
                link.wait()
        if target:
            target.shutdown()
            target.server_close()
        if fixture:
            fixture.cleanup()


if __name__ == '__main__':
    main()
