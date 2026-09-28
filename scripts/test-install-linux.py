#!/usr/bin/env python3
"""Exercise the complete installer in disposable Linux; downloads/systemctl/build are simulated.

Uses the real server binary, local Docker API, SQLite and HTTP login. No model
calls, real accounts, host data mounts, package installs or workspace builds.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tarfile
import urllib.request


def inside():
    root = Path('/tmp/install-test')
    fixture = root / 'fixture'; fixture.mkdir()
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}[platform.machine()]
    name = f'agentbox_v0.0.1_linux_{arch}'
    package = fixture / name; package.mkdir()
    for relative in ('agentbox', 'config.example.json', 'deploy/release.py', 'deploy/bootstrap.py',
                     'scripts/build-image.sh', 'images/agent/versions.env', 'images/agent/Dockerfile',
                     'images/agent/tmux.conf', 'images/agent/bashrc', 'images/agent/vimrc'):
        target = package / relative; target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(root / relative, target)
    (package / 'build.json').write_text(json.dumps(dict(program='agentbox', os='linux', arch=arch,
                                                      version='v0.0.1', revision='synthetic-fixture')))
    (package / 'clients').mkdir()
    for client in ('linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64', 'windows-amd64.exe'):
        (package / 'clients' / ('abox-link-' + client)).write_bytes(b'synthetic client')
    archive = fixture / (name + '.tar.gz')
    with tarfile.open(archive, 'w:gz') as tar: tar.add(package, arcname=name)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    (fixture / 'SHA256SUMS').write_text('0' * 64 + '  ' + archive.name + '\n')
    (fixture / 'latest.json').write_text('{"tag_name":"v0.0.1"}')
    mocks = root / 'bin'; mocks.mkdir()
    commands = {
        'curl': '''import shutil,sys
from pathlib import Path
url=next(arg for arg in sys.argv if arg.startswith('https://'))
assert url.startswith(('https://api.github.com/repos/devilcoolyue/agentbox/releases/', 'https://github.com/devilcoolyue/agentbox/releases/')), url
name='latest.json' if url.endswith('/latest') else url.rsplit('/',1)[1]
shutil.copyfile(Path('/tmp/install-test/fixture')/name,sys.argv[sys.argv.index('-o')+1])
''',
        'docker': '''import json,sys
if sys.argv[1:4]==['network','inspect','bridge']:
 print(json.dumps([{'IPAM':{'Config':[{'Gateway':'172.17.0.1'}]}}]))
elif sys.argv[1]=='ps': pass
elif sys.argv[1] not in ('info','build'): raise SystemExit('unexpected Docker CLI operation')
''',
        'systemctl': '''import os,signal,subprocess,sys,time,socket
from pathlib import Path
args=sys.argv[1:];pid=Path('/tmp/install-test/server.pid')
if args[0]=='show':
 if '--property=DropInPaths' not in args and Path('/etc/systemd/system/agentbox.service').exists(): print('/etc/systemd/system/agentbox.service')
elif args[0]=='start':
 with open('/tmp/install-test/server.log','ab') as log:
  proc=subprocess.Popen(['/opt/agentbox/current/agentbox','--config','/etc/agentbox/config.json'],stdout=log,stderr=log,start_new_session=True)
 pid.write_text(str(proc.pid))
elif args[0]=='stop':
 if pid.exists():
  os.kill(int(pid.read_text()),signal.SIGTERM);pid.unlink()
  for _ in range(100):
   with socket.socket() as probe:
    if probe.connect_ex(('127.0.0.1',18189)) != 0: break
   time.sleep(.05)
elif args[0]=='is-active':
 os.kill(int(pid.read_text()),0)
elif args[0] not in ('enable','disable','daemon-reload','reset-failed'): raise SystemExit('unexpected systemctl operation')
''',
    }
    for name, body in commands.items():
        executable = mocks / name; executable.write_text('#!/usr/bin/env python3\n' + body); executable.chmod(0o755)
    Path('/run/systemd/system').mkdir(parents=True, exist_ok=True)
    Path('/run/lock').mkdir(parents=True, exist_ok=True)
    # Prevent apt from running: dependencies must already be in the test image.
    apt = mocks / 'apt-get'; apt.write_text('#!/bin/sh\necho "unexpected apt call" >&2\nexit 1\n'); apt.chmod(0o755)
    env = dict(os.environ, PATH=str(mocks) + ':' + os.environ['PATH'])
    command = ['bash', str(root / 'install.sh'), '--listen', '127.0.0.1:18189']
    original_staging = set(Path('/tmp').glob('tmp.*'))
    result = subprocess.run(command, env=env, capture_output=True, text=True)
    assert result.returncode != 0 and 'checksum mismatch' in result.stderr, result.stderr
    assert not Path('/etc/agentbox').exists()
    assert set(Path('/tmp').glob('tmp.*')) == original_staging, 'failed download staging was not cleaned: ' + result.stderr
    (fixture / 'SHA256SUMS').write_text(digest + '  ' + archive.name + '\n')
    result = subprocess.run(command, env=env, capture_output=True, text=True)
    if result.returncode:
        print(result.stderr)
        log = root / 'server.log'
        if log.exists(): print(log.read_text())
        raise AssertionError('installer failed')
    config_path = Path('/etc/agentbox/config.json')
    original = config_path.read_bytes(); config = json.loads(original)
    assert config_path.stat().st_mode & 0o777 == 0o600
    assert Path('/opt/agentbox/current').resolve().name == 'v0.0.1'
    assert config['auth_token'] in result.stdout and '已安装并启动' in result.stdout
    req = urllib.request.Request('http://127.0.0.1:18189/api/login',
        data=json.dumps({'username':'boxadmin','password':config['auth_token']}).encode(),
        headers={'Content-Type':'application/json'})
    with urllib.request.urlopen(req, timeout=5) as response:
        token = json.load(response)['token']
    with urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:18189/api/tunnel/clients', headers={'Authorization':'Bearer '+token}), timeout=5) as response:
        clients = json.load(response)
        assert len(clients) == 5, clients
    assert all((Path('/var/lib/agentbox/abox-link') / c['name']).read_bytes() == b'synthetic client' for c in clients)
    assert Path('/var/lib/agentbox/state.db').is_file()
    again = subprocess.run(command, env=env, capture_output=True, text=True)
    assert again.returncode != 0 and 'Existing installation' in again.stderr
    assert config_path.read_bytes() == original
    assert set(Path('/tmp').glob('tmp.*')) == original_staging, 'successful download staging was not cleaned'
    result = subprocess.run(['bash', str(root / 'uninstall.sh'), '--yes'], env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert not config_path.exists() and not Path('/opt/agentbox').exists()
    retained = list(Path('/var/backups/agentbox-uninstall').glob('*/config/config.json'))
    assert len(retained) == 1 and retained[0].read_bytes() == original
    result = subprocess.run(command, env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    result = subprocess.run(['bash', str(root / 'uninstall.sh'), '--yes', '--purge'], env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert retained[0].read_bytes() == original and not Path('/var/lib/agentbox').exists()
    print('Linux installer: checksum rejection/cleanup, download/extract/install/activate, real HTTP login, private config and repeat-install refusal passed; systemd/downloads/image build simulated')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inside', action='store_true')
    parser.add_argument('--binary', type=Path)
    parser.add_argument('--image', default='agentbox-agent:claude-2.1.280-codex-0.145.0')
    args = parser.parse_args()
    if args.inside:
        inside(); return
    if not args.binary: parser.error('--binary is required')
    root = Path(__file__).resolve().parent.parent
    container = subprocess.check_output(['docker', 'run', '-d', '--user', '0:0', '--network', 'none',
        '--mount', 'type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock', args.image, 'sleep', 'infinity'], text=True).strip()
    try:
        subprocess.run(['docker', 'exec', container, 'mkdir', '-p', '/tmp/install-test'], check=True)
        import tempfile
        with tempfile.TemporaryDirectory(prefix='agentbox-install-fixture-') as tmp:
            staging = Path(tmp)
            for relative in ('install.sh', 'uninstall.sh', 'config.example.json', 'deploy/bootstrap.py', 'deploy/release.py',
                             'scripts/build-image.sh', 'images/agent/versions.env', 'images/agent/Dockerfile',
                             'images/agent/tmux.conf', 'images/agent/bashrc', 'images/agent/vimrc'):
                dest = staging / relative; dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(root / relative, dest)
            shutil.copy2(args.binary, staging / 'agentbox')
            shutil.copy2(__file__, staging / 'test.py')
            subprocess.run(['docker', 'cp', str(staging) + '/.', container + ':/tmp/install-test'], check=True)
        subprocess.run(['docker', 'exec', container, 'python3', '/tmp/install-test/test.py', '--inside'], check=True)
    finally:
        subprocess.run(['docker', 'rm', '-f', container], check=True, stdout=subprocess.DEVNULL)


if __name__ == '__main__': main()
