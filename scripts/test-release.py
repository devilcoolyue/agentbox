#!/usr/bin/env python3
"""Verify every archive/checksum; exercise packaged Linux binaries without Go/Node."""
import hashlib
import json
import os
from pathlib import Path
import platform
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import zipfile

out = Path(sys.argv[1]).resolve()
records = json.loads((out / 'release.json').read_text())
expected = {('agentbox', 'linux', x) for x in ['amd64', 'arm64']}
expected |= {('abox-link', s, a) for s, a in [('linux','amd64'),('linux','arm64'),('darwin','amd64'),('darwin','arm64'),('windows','amd64')]}
if {(r['program'],r['os'],r['arch']) for r in records} != expected or len(records) != len(expected):
    raise SystemExit('Missing/duplicate release platform')
names = [r['archive'] for r in records]
if len(set(names)) != len(names):
    raise SystemExit('Duplicate archive names across platforms')
for r in records:
    suffix = '.zip' if r['os'] == 'windows' else '.tar.gz'
    expected_name = f"{r['program']}_{r['version']}_{r['os']}_{r['arch']}{suffix}"
    if r['archive'] != expected_name:
        raise SystemExit('Archive name does not match platform: ' + r['archive'])
checks = {}
for line in (out / 'SHA256SUMS').read_text().splitlines():
    digest, name = line.split('  ', 1)
    if Path(name).name != name or name in checks:
        raise SystemExit('Invalid checksum entry')
    if hashlib.sha256((out / name).read_bytes()).hexdigest() != digest:
        raise SystemExit('Checksum mismatch: ' + name)
    checks[name] = digest
if set(checks) != {r['archive'] for r in records} | {'release.json'}:
    raise SystemExit('Checksum inventory mismatch')
arch = {'aarch64':'arm64','arm64':'arm64','x86_64':'amd64','AMD64':'amd64'}[platform.machine()]
if platform.system() != 'Linux':
    info = subprocess.check_output(['docker','info','--format','{{.Architecture}}'],text=True).strip()
    arch = {'aarch64':'arm64','arm64':'arm64','x86_64':'amd64'}[info]
with tempfile.TemporaryDirectory(prefix='agentbox-release-test-') as tmp:
    stage=Path(tmp)
    for r in records:
        dest=stage/r['archive'];dest.mkdir()
        archive=out/r['archive']
        if archive.suffix=='.zip':
            with zipfile.ZipFile(archive) as z:
                if any(Path(n).is_absolute() or '..' in Path(n).parts for n in z.namelist()):raise SystemExit('Unsafe ZIP')
                z.extractall(dest)
        else:
            with tarfile.open(archive) as t:t.extractall(dest,filter='data')
        roots=list(dest.iterdir())
        if len(roots)!=1:raise SystemExit('Unexpected package root')
        folder=roots[0]
        allowed = {'agentbox', 'abox-link', 'abox-link.exe', 'LICENSE', 'NOTICE',
                   'README.md', 'build.json', 'third_party'}
        if r['program']=='agentbox':
            allowed |= {'install.sh', 'uninstall.sh', 'clients', 'config.example.json', 'images', 'scripts', 'deploy'}
        if {path.name for path in folder.iterdir()} - allowed:
            raise SystemExit('Unexpected files in public binary package')
        if any(path.suffix in ('.go', '.ts') or path.name in ('AGENTS.md', '.git') for path in folder.rglob('*')):
            raise SystemExit('Unexpected project sources in binary package')
        meta=json.loads((folder/'build.json').read_text())
        for key in ['version','revision','os','arch']:
            if meta[key]!=r[key]:raise SystemExit('Metadata mismatch')
        for path in ['LICENSE','NOTICE','third_party/README.md','third_party/vendor.json','third_party/go-modules.json']:
            if not (folder/path).is_file():raise SystemExit('Missing license inventory')
        if r['program']=='agentbox':
            for path in ['images/agent/Dockerfile', 'images/agent/tmux.conf', 'images/agent/bashrc', 'images/agent/vimrc', 'images/browser/Dockerfile', 'images/browser/browser.py', 'scripts/build-browser-image.sh']:
                if not (folder/path).is_file():raise SystemExit('Missing terminal image resource: '+path)
            for path in ['uninstall.sh', 'clients/abox-link-linux-amd64', 'clients/abox-link-linux-arm64', 'clients/abox-link-darwin-amd64', 'clients/abox-link-darwin-arm64', 'clients/abox-link-windows-amd64.exe', 'install.sh','deploy/bootstrap.py','deploy/release.py','deploy/update.py','scripts/build-image.sh','images/agent/versions.env']:
                if not (folder/path).is_file():raise SystemExit('Missing installer resource: '+path)
        if r['os']!='linux' or r['arch']!=arch:continue
        binary=r['program']
        # A clean no-network Alpine container demonstrates no Go/Node/glibc dependency.
        cid=subprocess.check_output(['docker','create','--network','none','--entrypoint','sleep','alpine:3','infinity'],text=True).strip()
        try:
            subprocess.run(['docker','cp',str(folder)+'/.',cid+':/opt'],check=True,stdout=subprocess.DEVNULL)
            subprocess.run(['docker','start',cid],check=True,stdout=subprocess.DEVNULL)
            result=subprocess.check_output(['docker','exec',cid,'/opt/'+binary,'--version'],text=True)
            if r['version'] not in result or r['revision'] not in result:raise SystemExit('Version metadata missing')
            if binary=='agentbox':
                config=stage/'fixture.json'
                config.write_text(json.dumps({'auth_token':'synthetic-release-test-token','data_dir':'/tmp/data','accounts':[]}))
                subprocess.run(['docker','exec',cid,'mkdir','-p','/tmp/data'],check=True)
                dbpath=stage/'state.db'
                with sqlite3.connect(dbpath) as db:
                    db.execute('CREATE TABLE IF NOT EXISTS release_fixture (value TEXT)')
                    db.execute('INSERT INTO release_fixture VALUES (?)', ('synthetic',))
                subprocess.run(['docker','cp',str(dbpath),cid+':/tmp/data/state.db'],check=True,stdout=subprocess.DEVNULL)
                subprocess.run(['docker','cp',str(config),cid+':/tmp/config.json'],check=True,stdout=subprocess.DEVNULL)
                subprocess.run(['docker','exec',cid,'/opt/agentbox','backup','--config','/tmp/config.json','--output','/tmp/system.tar.gz'],check=True)
                subprocess.run(['docker','exec',cid,'/opt/agentbox','backup-verify','/tmp/system.tar.gz'],check=True)
                subprocess.run(['docker','exec',cid,'/opt/agentbox','restore','--to','/tmp/restored','/tmp/system.tar.gz'],check=True)
                restored=stage/'restored.db'
                subprocess.run(['docker','cp',cid+':/tmp/restored/data/state.db',str(restored)],check=True,stdout=subprocess.DEVNULL)
                with sqlite3.connect(restored) as db:
                    if db.execute('SELECT value FROM release_fixture').fetchone() != ('synthetic',):
                        raise SystemExit('Restored data differs')
        finally:
            subprocess.run(['docker','rm','-f',cid],check=True,stdout=subprocess.DEVNULL)
print('All archives/checksums verified; native Linux packages ran in clean no-network containers')
