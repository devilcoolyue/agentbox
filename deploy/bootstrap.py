#!/usr/bin/env python3
"""First installation from a checksum-verified release; reuse release.py activation."""
import argparse
from dataclasses import dataclass
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import secrets
import socket
import subprocess
import tempfile

import release


@dataclass
class Layout:
    app: Path = Path('/opt/agentbox')
    config: Path = Path('/etc/agentbox/config.json')
    data: Path = Path('/var/lib/agentbox')
    cache: Path = Path('/var/cache/agentbox')
    units: Path = Path('/etc/systemd/system')


def fresh(layout):
    for path in (layout.app, layout.config.parent, layout.data, layout.cache,
                 layout.units / 'agentbox.service'):
        if path.exists() or path.is_symlink():
            raise ValueError(f'Existing installation path: {path}; use release.py to upgrade/resume. No files overwritten.')
    if release.run('systemctl', 'show', 'agentbox.service', '--property=FragmentPath', '--value', capture=True).strip():
        raise ValueError('agentbox.service already exists; migrate the existing installation explicitly')


def listen_address(value):
    host, port = value.rsplit(':', 1)
    address = ipaddress.ip_address(host.strip('[]'))
    if str(address) != host and f'[{address}]' != host:
        raise ValueError('listen must use an IP address and port')
    if not port.isascii() or not port.isdigit() or not 1 <= int(port) <= 65535:
        raise ValueError('listen port must be between 1 and 65535')
    if address.version == 6 and not host.startswith('['):
        raise ValueError('IPv6 listen addresses require brackets, e.g. [::]:8180')
    return address, int(port)


def available(value):
    address, port = listen_address(value)
    family = socket.AF_INET6 if address.version == 6 else socket.AF_INET
    with socket.socket(family, socket.SOCK_STREAM) as listener:
        listener.bind((str(address), port))


def install(package, listen, layout):
    fresh(layout)
    available(listen)
    meta = release.read(package / 'build.json')
    version = meta.get('version', '')
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}.get(platform.machine())
    if (meta.get('program') != 'agentbox' or meta.get('os') != 'linux' or meta.get('arch') != arch
            or not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?', version)
            or '+dirty' in meta.get('revision', '')):
        raise ValueError('Expected a clean Linux release matching this machine')
    for relative in ('agentbox', 'deploy/release.py', 'scripts/build-image.sh', 'images/agent/versions.env'):
        if not (package / relative).is_file():
            raise ValueError('Incomplete release: ' + relative)

    records = json.loads(release.run('docker', 'network', 'inspect', 'bridge', capture=True))
    gateways = [entry.get('Gateway', '') for entry in records[0]['IPAM']['Config']]
    gateway = next((ip for ip in gateways if ip and ipaddress.ip_address(ip).version == 4), None)
    if not gateway:
        raise ValueError('Local Docker bridge needs an IPv4 gateway')
    # Use a per-release tag, leaving all existing workspace images untouched.
    image = 'agentbox-agent:' + version
    password = secrets.token_urlsafe(32)
    config = release.read(package / 'config.example.json')
    config.update(listen=listen, auth_token=password, data_dir=str(layout.data), cache_dir=str(layout.cache),
                  agent_image=image, accounts=[], proxies=[],
                  tunnel={'enabled': False, 'proxy_bind': gateway + ':1080'},
                  proxy_bridge={'bind': gateway + ':1081'})
    config['container']['network'] = 'bridge'
    with tempfile.TemporaryDirectory(prefix='agentbox-config-') as tmp:
        candidate = Path(tmp) / 'config.json'
        release.atomic_json(candidate, config)
        release.run(package / 'agentbox', 'check-config', '--config', candidate)

    print('Building the pinned Claude/Codex workspace image (first install may take several minutes)...', flush=True)
    subprocess.run(['bash', str(package / 'scripts/build-image.sh')], check=True,
                   env=dict(os.environ, AGENTBOX_IMAGE=image, AGENTBOX_VERSIONED_IMAGE=image,
                            AGENTBOX_CLAUDE_VERSION='', AGENTBOX_CODEX_VERSION='', AGENTBOX_BASE_IMAGE=''))
    # Nothing in the deployment layout is written until downloads/build/preflight succeed.
    fresh(layout)
    available(listen)
    layout.config.parent.mkdir(parents=True, mode=0o700)
    release.atomic_json(layout.config, config)
    layout.cache.mkdir(parents=True, mode=0o700)
    command = ['python3', str(package / 'deploy/release.py'), '--app', str(layout.app),
               '--config', str(layout.config), '--unit-dir', str(layout.units)]
    release.run(*command, 'install', '--package', package)
    # Activate using the installed copy so error recovery never relies on a temp path.
    command[1] = str(layout.app / 'releases' / version / 'deploy/release.py')
    try:
        release.run(*command, 'activate', '--version', version, '--backup-dir', layout.data / 'backups')
    except subprocess.CalledProcessError:
        print('Activation failed. Configuration and installed files are retained.')
        print('Inspect: journalctl -u agentbox --no-pager -n 50')
        print(f'Retry: python3 {command[1]} activate --version {version}')
        raise
    address, port = listen_address(listen)
    host = '<服务器IP>' if address.is_unspecified else (f'[{address}]' if address.version == 6 else str(address))
    print(f'\nagentbox {version} 已安装并启动（开机自启）。')
    print(f'控制台：http://{host}:{port}')
    print('管理员：boxadmin')
    print('初始密码：' + password)
    print(f'配置：{layout.config}（仅 root 可读）；数据：{layout.data}')
    print('登录后在「系统设置 → 账号池」添加 Claude/Codex 账号，再创建工作空间。')
    if address.is_unspecified:
        print(f'远程访问需在防火墙/安全组放行 TCP {port}；公网长期使用请配置 HTTPS 反向代理。')
    print('状态：systemctl status agentbox    日志：journalctl -u agentbox -f')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--package', required=True, type=Path)
    parser.add_argument('--listen', default='0.0.0.0:8180')
    args = parser.parse_args()
    if platform.system() != 'Linux' or os.geteuid() != 0 or not Path('/run/systemd/system').is_dir():
        parser.error('run as root on the target systemd Linux server')
    # A CLI context/remote DOCKER_HOST must not build images on a different daemon.
    for key in ('DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH'):
        os.environ.pop(key, None)
    os.environ['DOCKER_HOST'] = 'unix:///var/run/docker.sock'
    with release.locked('/run/lock/agentbox-install.lock'):
        install(args.package.resolve(), args.listen, Layout())


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, KeyError, IndexError, subprocess.CalledProcessError) as error:
        raise SystemExit('agentbox installation failed: ' + str(error))
