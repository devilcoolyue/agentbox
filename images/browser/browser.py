#!/usr/bin/env python3
"""Workspace desktop. No published port, privileged process, or CDP endpoint.
Control and VNC relay run as the workspace UID; that UID is the trust boundary.
"""
import base64
import fcntl
import json
import os
from pathlib import Path
import select
import signal
import socket
import socketserver
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit, unquote

RUNTIME = Path('/tmp/agentbox-browser-' + str(os.getuid()))
CONTROL = str(RUNTIME / 'control.sock')
PROFILE = Path.home() / '.agentbox-browser'
VNC_PORT = 5909
PROXY_PORT = 18089
MAX_REQUEST = 300000


def request(value, timeout=3):
    with socket.socket(socket.AF_UNIX) as s:
        s.settimeout(timeout)
        s.connect(CONTROL)
        s.sendall(json.dumps(value).encode() + b'\n')
        with s.makefile('rb') as f:
            return json.loads(f.readline(MAX_REQUEST))


def safe_url(value):
    if not isinstance(value, str) or len(value) > 8192 or any(ord(c) < 32 for c in value):
        raise ValueError('地址格式无效')
    p = urlsplit(value)
    if p.scheme not in ('http', 'https') or not p.hostname or p.username or p.password:
        raise ValueError('仅支持不含用户名密码的 HTTP / HTTPS 地址')
    return value


def pump(a, b):
    # A bounded idle timeout also collects abandoned CONNECT requests.
    while True:
        ready, _, _ = select.select([a, b], [], [], 90)
        if not ready:
            return
        for source in ready:
            data = source.recv(65536)
            if not data:
                return
            (b if source is a else a).sendall(data)


class Proxy(socketserver.StreamRequestHandler):
    """Local adapter: Chrome cannot put Basic proxy credentials in --proxy-server.
    Forward through the configured bridge only; never retry directly.
    """
    def handle(self):
        self.connection.settimeout(30)
        try:
            first = self.rfile.readline(MAX_REQUEST)
            if not first.endswith(b'\r\n'):
                return
            headers, size = [], len(first)
            upgrade = False
            while True:
                line = self.rfile.readline(MAX_REQUEST)
                size += len(line)
                if size > MAX_REQUEST or not line.endswith(b'\r\n'):
                    return
                if line == b'\r\n':
                    break
                if line.split(b':', 1)[0].lower() == b'upgrade':
                    upgrade = True
                if line.split(b':', 1)[0].lower() not in (b'proxy-authorization', b'proxy-connection', b'connection'):
                    headers.append(line)
            upstream = self.server.upstream
            with socket.create_connection((upstream.hostname, upstream.port or 80), timeout=20) as out:
                auth = base64.b64encode((unquote(upstream.username or '') + ':' + unquote(upstream.password or '')).encode())
                out.sendall(first + b''.join(headers) + b'Proxy-Authorization: Basic ' + auth +
                            (b'\r\nConnection: Upgrade\r\n\r\n' if upgrade else b'\r\nConnection: close\r\n\r\n'))
                # Unbuffered rfile prevents consuming pipelined tunnel/body bytes.
                pump(self.connection, out)
        except (OSError, ValueError):
            try:
                self.connection.sendall(b'HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n')
            except OSError:
                pass

    rbufsize = 0


class ProxyServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def browser_proxy_flags():
    # Chrome applies resolver rules to the proxy address too, even when it is
    # an IPv4 literal. Keep the loopback adapter resolvable while blocking
    # local resolution of destination names (the upstream proxy resolves them).
    return ['--proxy-server=http://127.0.0.1:' + str(PROXY_PORT),
            '--proxy-bypass-list=<-loopback>',
            '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE localhost, EXCLUDE 127.0.0.1']


def serve():
    os.umask(0o077)
    PROFILE.mkdir(mode=0o700, parents=True, exist_ok=True)
    download = Path('/workspace/Downloads')
    download.mkdir(exist_ok=True)
    # Chrome uses XDG's download directory on first launch; subsequent user
    # preferences belong to Chrome and are deliberately left alone.
    config = PROFILE / 'xdg'
    config.mkdir(exist_ok=True)
    (config / 'user-dirs.dirs').write_text('XDG_DOWNLOAD_DIR="/workspace/Downloads"\n')
    env = {**os.environ, 'XDG_CONFIG_HOME': str(config)}
    children = []
    chrome = None
    proxy = None
    control = socket.socket(socket.AF_UNIX)
    stopping = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stopping.set())
    signal.signal(signal.SIGINT, lambda *_: stopping.set())
    def launch(argv, **kwargs):
        p = subprocess.Popen(argv, env=env, stdin=subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             start_new_session=True, **kwargs)
        children.append(p)
        return p
    try:
        # Allocate an unused X display, avoiding stale :99 locks after a crash.
        readfd, writefd = os.pipe()
        xvfb = subprocess.Popen(['Xvfb', '-displayfd', str(writefd), '-screen', '0', '1440x900x24', '-nolisten', 'tcp'],
                                pass_fds=(writefd,), stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL, start_new_session=True)
        children.append(xvfb)
        os.close(writefd)
        with os.fdopen(readfd, 'rb') as display:
            if not select.select([display], [], [], 10)[0]:
                raise RuntimeError('显示服务启动超时')
            number = display.readline(16).decode().strip()
        if not number.isdigit():
            raise RuntimeError('显示服务启动失败')
        env['DISPLAY'] = ':' + number
        launch(['openbox'])
        flags = ['--user-data-dir=' + str(PROFILE / 'profile'), '--no-first-run',
                 '--no-default-browser-check', '--restore-last-session',
                 '--disable-session-crashed-bubble', '--password-store=basic',
                 '--disable-quic', '--force-webrtc-ip-handling-policy=disable_non_proxied_udp',
                 '--window-size=1440,860', '--start-maximized']
        upstream = os.environ.get('AGENTBOX_BROWSER_PROXY', '')
        if upstream:
            parsed = urlsplit(upstream)
            if parsed.scheme != 'http' or not parsed.hostname:
                raise RuntimeError('浏览器代理配置无效')
            proxy = ProxyServer(('127.0.0.1', PROXY_PORT), Proxy)
            proxy.upstream = parsed
            threading.Thread(target=proxy.serve_forever, daemon=True).start()
            flags += browser_proxy_flags()
        # No browser instance exists here (start is serialized). Chrome's old
        # hostname/PID lock may survive container recreation; only remove those
        # three singleton markers, never profile contents.
        for name in ('SingletonLock', 'SingletonSocket', 'SingletonCookie'):
            (PROFILE / 'profile' / name).unlink(missing_ok=True)
        command = ['/usr/local/bin/agentbox-browser', *flags]
        chrome = launch([*command, 'about:blank'])
        vnc = launch(['x11vnc', '-display', env['DISPLAY'], '-rfbport', str(VNC_PORT),
                      '-localhost', '-nopw', '-forever', '-shared', '-xkb',
                      '-wait', '20', '-defer', '20', '-quiet'])
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if chrome.poll() is not None or vnc.poll() is not None:
                raise RuntimeError('浏览器启动失败，请检查镜像及用户命名空间沙箱支持')
            try:
                with socket.create_connection(('127.0.0.1', VNC_PORT), timeout=3) as test:
                    if test.recv(12).startswith(b'RFB '):
                        break
            except OSError:
                pass
            time.sleep(.1)
        else:
            raise RuntimeError('远程桌面启动超时')
        CONTROL_PATH = Path(CONTROL)
        CONTROL_PATH.unlink(missing_ok=True)
        control.bind(CONTROL)
        control.listen(8)
        control.settimeout(.5)
        browser = 'Chromium' if os.uname().machine in ('aarch64', 'arm64') else 'Google Chrome'
        info = {'running': True, 'browser': browser, 'proxy': bool(upstream),
                'network_key': os.environ.get('AGENTBOX_BROWSER_NETWORK_KEY', '')}
        while not stopping.is_set() and all(p.poll() is None for p in children):
            try:
                conn, _ = control.accept()
            except socket.timeout:
                continue
            with conn:
                conn.settimeout(3)
                try:
                    with conn.makefile('rb') as f:
                        data = json.loads(f.readline(MAX_REQUEST))
                    response = info
                    if data['action'] == 'clipboard':
                        if data.get('write'):
                            text = data.get('text', '')
                            if not isinstance(text, str) or len(text.encode()) > 65536:
                                raise ValueError('剪贴板内容过长')
                            subprocess.run(['xclip', '-selection', 'clipboard', '-in'],
                                           input=text.encode(), env=env, stdout=subprocess.DEVNULL,
                                           stderr=subprocess.DEVNULL, timeout=3, check=True)
                            response = {'text': text}
                        else:
                            proc = subprocess.Popen(['xclip', '-selection', 'clipboard', '-out'],
                                                    env=env, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
                            timer = threading.Timer(3, proc.kill)
                            timer.start()
                            try:
                                raw = proc.stdout.read(65537)
                                proc.kill()
                                proc.wait()
                                if len(raw) > 65536:
                                    raise ValueError('剪贴板内容过长')
                                response = {'text': raw.decode('utf-8', errors='replace')}
                            finally:
                                timer.cancel()
                                proc.stdout.close()
                    elif data['action'] == 'stop':
                        stopping.set()
                    elif data['action'] == 'open':
                        url = safe_url(data['url'])
                        # Chrome hands the URL to the existing instance and exits.
                        p = subprocess.run([*command, '--new-tab', url], env=env,
                                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                           stderr=subprocess.DEVNULL, timeout=5)
                        if p.returncode:
                            raise RuntimeError('打开网页失败')
                    elif data['action'] != 'status':
                        raise ValueError('未知操作')
                    conn.sendall(json.dumps(response).encode() + b'\n')
                except (ValueError, KeyError, RuntimeError, OSError, subprocess.SubprocessError):
                    conn.sendall(b'{"error":"Browser command failed"}\n')
    except Exception as exc:
        (RUNTIME / 'error').write_text(str(exc))
    finally:
        control.close()
        # Close Chrome windows through the window manager before terminating X
        # or the browser's network process: killing the entire process group
        # immediately can discard recently written cookies.
        if chrome is not None and chrome.poll() is None:
            try:
                windows = subprocess.run(['wmctrl', '-lp'], env=env, capture_output=True,
                                         text=True, timeout=3, check=True).stdout
                for line in windows.splitlines():
                    columns = line.split(None, 4)
                    if len(columns) >= 3 and columns[2] == str(chrome.pid):
                        subprocess.run(['wmctrl', '-ic', columns[0]], env=env,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3)
                chrome.wait(timeout=8)
            except (OSError, subprocess.SubprocessError):
                if chrome.poll() is None:
                    chrome.terminate()
                    try:
                        chrome.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        pass
        for p in reversed(children):
            if p.poll() is None:
                try:
                    os.killpg(p.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
        for p in children:
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait()
        if proxy:
            proxy.shutdown()
            proxy.server_close()
        Path(CONTROL).unlink(missing_ok=True)


def main():
    action = sys.argv[1]
    if action == 'relay':
        # Raw RFB on Docker's attached stdin/stdout, no public VNC listener.
        with socket.create_connection(('127.0.0.1', VNC_PORT), timeout=10) as sock:
            sock.settimeout(None)
            while True:
                ready, _, _ = select.select([sock, sys.stdin.buffer], [], [], 90)
                if not ready:
                    continue
                if sock in ready:
                    data = sock.recv(65536)
                    if not data:
                        return
                    sys.stdout.buffer.write(data)
                    sys.stdout.buffer.flush()
                if sys.stdin.buffer in ready:
                    data = os.read(0, 65536)
                    if not data:
                        return
                    sock.sendall(data)
        return
    RUNTIME.mkdir(mode=0o700, exist_ok=True)
    if action == 'serve':
        serve()
        return
    data = json.load(sys.stdin)
    with (RUNTIME / 'lock').open('w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            info = request({'action': 'status'})
        except (OSError, ValueError):
            info = {'running': False}
        if action == 'clipboard':
            if not info['running']:
                raise RuntimeError('浏览器未运行')
            data['action'] = 'clipboard'
            print(json.dumps(request(data, timeout=8), ensure_ascii=False))
            return
        if action == 'status':
            print(json.dumps(info))
            return
        if action == 'stop':
            if info['running']:
                request({'action': 'stop'})
                for _ in range(200):
                    if not Path(CONTROL).exists():
                        break
                    time.sleep(.1)
                else:
                    raise RuntimeError('浏览器尚未完全停止，请稍后重试')
            print('{"running":false}')
            return
        if action != 'start':
            raise ValueError('未知操作')
        url = safe_url(data['url']) if data.get('url') else ''
        key = os.environ.get('AGENTBOX_BROWSER_NETWORK_KEY', '')
        if info['running'] and info.get('network_key') != key:
            request({'action': 'stop'})
            for _ in range(150):
                if not Path(CONTROL).exists():
                    break
                time.sleep(.1)
            else:
                raise RuntimeError('浏览器网络切换超时，请停止工作空间后重试')
            info = {'running': False}
        if not info['running']:
            (RUNTIME / 'error').unlink(missing_ok=True)
            # Only the browser proxy is passed in, never model/API credentials.
            subprocess.Popen([sys.executable, '-I', __file__, 'serve'], env=os.environ,
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, start_new_session=True)
            for _ in range(250):
                if (RUNTIME / 'error').exists():
                    raise RuntimeError((RUNTIME / 'error').read_text())
                try:
                    info = request({'action': 'status'})
                    break
                except (OSError, ValueError):
                    time.sleep(.1)
            else:
                raise RuntimeError('浏览器启动超时')
        if url:
            result = request({'action': 'open', 'url': url}, timeout=8)
            if result.get('error'):
                raise RuntimeError('打开网页失败')
        print(json.dumps(info))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print(json.dumps({'error': str(exc)}))
        sys.exit(1)
