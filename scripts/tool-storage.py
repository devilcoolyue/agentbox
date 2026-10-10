#!/usr/bin/env python3
"""Publish verified, immutable Go/browser tools for agentbox (operator-only)."""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import posixpath
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import urllib.request

MARKER = '.agentbox-tool.json'
VERSION = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+')
PLATFORM = re.compile(r'linux-(amd64|arm64)-[a-z0-9]+-[a-z0-9.]+')

def run(*args, **kwargs):
    return subprocess.run(list(map(str, args)), check=True, text=True, **kwargs)

def trusted_dir(path, mode=0o700):
    missing = []
    current = path
    while not current.exists() and not current.is_symlink():
        missing.append(current); current = current.parent
    for directory in reversed(missing):
        directory.mkdir(mode=mode)
        directory.chmod(mode)
    st = path.lstat()
    if not stat.S_ISDIR(st.st_mode) or st.st_uid != os.geteuid() or st.st_mode & 0o022:
        raise ValueError('directory must be operator-owned and not group/world writable: ' + str(path))

def init(root):
    if not root.is_absolute() or str(root) == '/': raise ValueError('root must be an absolute dedicated directory')
    trusted_dir(root)
    for name, mode in [('public', 0o755), ('private', 0o700), ('staging', 0o700)]:
        trusted_dir(root / name, mode)
        (root / name).chmod(mode)

@contextlib.contextmanager
def locked(root):
    init(root)
    fd = os.open(root / '.publish.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as f:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield

def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as f:
        for data in iter(lambda: f.read(1024 * 1024), b''): h.update(data)
    return h.hexdigest()

def inventory(root):
    out = []
    for path in sorted(root.rglob('*')):
        rel = path.relative_to(root).as_posix()
        if rel == MARKER: continue
        st = path.lstat()
        if stat.S_ISLNK(st.st_mode):
            target = os.readlink(path)
            resolved = posixpath.normpath(str(PurePosixPath(rel).parent / target))
            if target.startswith('/') or resolved == '..' or resolved.startswith('../'):
                raise ValueError('external tool symlink: ' + rel)
            out.append([rel, 'link', target])
        elif stat.S_ISDIR(st.st_mode): out.append([rel, 'dir'])
        elif stat.S_ISREG(st.st_mode): out.append([rel, 'file', bool(st.st_mode & 0o111), st.st_size, sha(path)])
        else: raise ValueError('special file in tool distribution: ' + rel)
    return out

def tree_hash(root):
    return hashlib.sha256(json.dumps(inventory(root), separators=(',', ':')).encode()).hexdigest()

def publish(source, target, metadata):
    digest = tree_hash(source)
    if target.exists() or target.is_symlink():
        if target.is_symlink() or not (target / MARKER).is_file(): raise ValueError('unmanaged target exists')
        old = json.loads((target / MARKER).read_text())
        if old.get('tree_sha256') != digest or tree_hash(target) != digest:
            raise ValueError('immutable tool version differs: ' + str(target))
        return False
    trusted_dir(target.parent, 0o755)
    for path in source.rglob('*'):
        if path.is_symlink(): continue
        if path.is_dir(): path.chmod(0o755)
        else:
            path.chmod(0o755 if path.stat().st_mode & 0o111 else 0o644)
            with path.open('rb') as f: os.fsync(f.fileno())
    source.chmod(0o755)
    marker = source / MARKER
    with marker.open('w') as f:
        json.dump(dict(metadata, format=1, tree_sha256=digest), f, indent=2)
        f.write('\n'); f.flush(); os.fsync(f.fileno())
    marker.chmod(0o644)
    os.rename(source, target)  # serialized publisher; never replaces a version
    fd = os.open(target.parent, os.O_RDONLY | os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
    return True

def download(url, target, expected, limit=2 << 30):
    h = hashlib.sha256(); size = 0
    with urllib.request.urlopen(url, timeout=60) as response, target.open('xb') as f:
        while chunk := response.read(1024 * 1024):
            size += len(chunk)
            if size > limit: raise ValueError('download exceeds limit')
            h.update(chunk); f.write(chunk)
    if h.hexdigest() != expected: raise ValueError('download SHA256 mismatch')

def extract_go(archive, dest):
    links, seen, size = [], set(), 0
    with tarfile.open(archive) as tar:
        for n, member in enumerate(tar):
            path = PurePosixPath(member.name)
            if n >= 300000 or path.is_absolute() or '..' in path.parts or not path.parts or path.parts[0] != 'go':
                raise ValueError('invalid Go archive path or entry count')
            if member.name in seen: raise ValueError('duplicate archive path')
            seen.add(member.name)
            target = dest.joinpath(*path.parts)
            if member.isdir(): target.mkdir(parents=True, exist_ok=True); continue
            target.parent.mkdir(parents=True, exist_ok=True)
            if member.issym():
                resolved = posixpath.normpath(str(path.parent / member.linkname))
                if member.linkname.startswith('/') or not resolved.startswith('go/'):
                    raise ValueError('external archive symlink')
                links.append((target, member.linkname)); continue
            if not member.isfile(): raise ValueError('unsupported archive member')
            size += member.size
            if size > 2 << 30: raise ValueError('expanded Go archive too large')
            with tar.extractfile(member) as src, target.open('xb') as dst: shutil.copyfileobj(src, dst)
            target.chmod(0o755 if member.mode & 0o111 else 0o644)
    for target, link in links: target.symlink_to(link)
    return dest / 'go'

def install_go(args, root):
    if not re.fullmatch(r'go1\.[0-9]+\.[0-9]+', args.version): raise ValueError('exact Go version required')
    if shutil.disk_usage(root).free < 2 << 30: raise ValueError('at least 2 GiB free space is required')
    with tempfile.TemporaryDirectory(prefix='go-', dir=root / 'staging') as td:
        stage = Path(td); archive = stage / 'go.tar.gz'
        if args.archive:
            if not args.sha256 or not re.fullmatch('[a-f0-9]{64}', args.sha256): raise ValueError('--archive requires --sha256')
            if sha(Path(args.archive)) != args.sha256: raise ValueError('archive SHA256 mismatch')
            shutil.copyfile(args.archive, archive); digest = args.sha256
        else:
            with urllib.request.urlopen('https://go.dev/dl/?mode=json&include=all', timeout=60) as response:
                catalog = json.loads(response.read(16 << 20))
            entry = next((f for v in catalog if v['version'] == args.version for f in v['files']
                          if f['os'] == 'linux' and f['arch'] == args.arch and f['kind'] == 'archive'), None)
            if not entry: raise ValueError('Go distribution not found')
            filename = args.version + '.linux-' + args.arch + '.tar.gz'
            if entry['filename'] != filename: raise ValueError('unexpected Go distribution filename')
            digest = entry['sha256']; download('https://dl.google.com/go/' + filename, archive, digest)
        source = extract_go(archive, stage)
        if (source / 'VERSION').read_text().splitlines()[0] != args.version: raise ValueError('Go VERSION mismatch')
        binary = source / 'bin/go'
        if not binary.is_file(): raise ValueError('Go binary missing')
        with binary.open('rb') as f: header = f.read(20)
        machine = {'amd64':62, 'arm64':183}[args.arch]
        if len(header)!=20 or header[:6]!=b'\x7fELF\x02\x01' or int.from_bytes(header[18:20],'little')!=machine:
            raise ValueError('Go binary does not match the selected Linux architecture')
        target = root / 'public/go' / ('linux-' + args.arch) / args.version
        publish(source, target, {'kind': 'go', 'version': args.version, 'platform': 'linux-' + args.arch, 'archive_sha256': digest})
        print('Published', target)

PLATFORM_JS = r"""const fs=require('fs'),os=require('os');let d={};for(const l of fs.readFileSync('/etc/os-release','utf8').split('\n')){const m=l.match(/^([A-Z_]+)=(.*)$/);if(m)d[m[1]]=m[2].replace(/^"|"$/g,'');}fs.writeFileSync('/tmp/tool-platform', 'linux-'+({x64:'amd64',arm64:'arm64'}[os.arch()])+'-'+d.ID+'-'+d.VERSION_ID);"""

def install_browser(args, root):
    if not VERSION.fullmatch(args.version): raise ValueError('exact npm package version required')
    info = json.loads(subprocess.check_output(['docker', 'image', 'inspect', args.image]))[0]
    if info.get('Os') != 'linux' or info['Config'].get('Volumes') or info['Config'].get('OnBuild'):
        raise ValueError('a local Linux image without VOLUME/ONBUILD is required')
    if shutil.disk_usage(root).free < 4 << 30: raise ValueError('at least 4 GiB free space is required')
    kind = args.command.removeprefix('install-')
    # No host mount, Docker socket, credentials, user directory or CLI provider.
    # The image is pinned before create; only npm's official package is installed.
    command = 'set -eu\nnode -e "$ABOX_PLATFORM_JS"\n'
    command += f'npm install --ignore-scripts --no-audit --no-fund --registry=https://registry.npmjs.org --prefix /tmp/tool-package {kind}@{args.version}\n'
    if kind == 'playwright': command += 'node /tmp/tool-package/node_modules/playwright/cli.js install ' + ' '.join(args.browsers) + '\n'
    else: command += 'node /tmp/tool-package/node_modules/puppeteer/install.mjs\n'
    env = {v.split('=', 1)[0]: '' for v in info['Config'].get('Env', [])}
    env.update(PATH='/usr/local/bin:/usr/bin:/bin', HOME='/tmp', NODE_TLS_REJECT_UNAUTHORIZED='1',
               PLAYWRIGHT_BROWSERS_PATH='/tmp/tool-publish', PUPPETEER_CACHE_DIR='/tmp/tool-publish',
               ABOX_PLATFORM_JS=PLATFORM_JS)
    create = ['docker', 'create', '--user', '0:0', '--network', 'bridge', '--cap-drop', 'ALL',
              '--security-opt', 'no-new-privileges', '--memory', '2g', '--pids-limit', '512', '--no-healthcheck',
              '--entrypoint', '/bin/sh', '--label', 'agentbox.tool-publisher=1']
    for k, v in env.items(): create.extend(['--env', k + '=' + v])
    cid = subprocess.check_output(create + [info['Id'], '-c', command], text=True).strip()
    try:
        run('docker', 'start', '-a', cid, timeout=1200)
        if subprocess.check_output(['docker', 'inspect', '-f', '{{.State.ExitCode}}', cid], text=True).strip() != '0':
            raise ValueError('browser download failed')
        with tempfile.TemporaryDirectory(prefix=kind+'-', dir=root / 'staging') as td:
            stage = Path(td)
            run('docker', 'cp', cid + ':/tmp/tool-platform', stage / 'platform')
            platform = (stage / 'platform').read_text().strip()
            if not PLATFORM.fullmatch(platform): raise ValueError('unsupported browser platform')
            run('docker', 'cp', cid + ':/tmp/tool-publish', stage / 'browsers')
            run('docker', 'cp', cid + ':/tmp/tool-package/package-lock.json', stage / 'package-lock.json')
            lock_hash = sha(stage / 'package-lock.json')
            base = root / 'public' / kind / platform
            entries = list((stage / 'browsers').iterdir())
            published = 0
            for entry in entries:
                if kind == 'playwright':
                    if not re.fullmatch(r'[a-z][a-z0-9_]*-[0-9]+', entry.name): continue
                    published += 1
                    publish(entry, base / entry.name, {'kind':kind,'package_version':args.version,'platform':platform,'npm_lock_sha256':lock_hash})
                elif entry.name in ('chrome', 'chrome-headless-shell', 'chromium', 'firefox'):
                    for version in entry.iterdir():
                        if not re.fullmatch(r'[a-z0-9]+-[0-9.]+', version.name): raise ValueError('invalid browser revision')
                        published += 1
                        publish(version, base / entry.name / version.name, {'kind':kind,'package_version':args.version,'platform':platform,'npm_lock_sha256':lock_hash})
            if not published: raise ValueError('no browser revisions were downloaded')
            print('Published', kind, args.version, platform)
    finally:
        run('docker', 'rm', '-f', cid, timeout=30)  # cleanup failure is a failure

def verify(root):
    count = 0
    for manifest in (root / 'public').rglob(MARKER):
        meta = json.loads(manifest.read_text())
        if tree_hash(manifest.parent) != meta['tree_sha256']: raise ValueError('tool content changed: ' + str(manifest.parent))
        count += 1
    print('Verified immutable tool versions:', count)

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--root', required=True, type=Path)
    sub = p.add_subparsers(dest='command', required=True)
    sub.add_parser('init'); sub.add_parser('verify')
    go = sub.add_parser('install-go'); go.add_argument('--version', required=True); go.add_argument('--arch', choices=['amd64','arm64'], required=True)
    go.add_argument('--archive'); go.add_argument('--sha256')
    for kind in ('playwright', 'puppeteer'):
        b = sub.add_parser('install-'+kind); b.add_argument('--version', required=True); b.add_argument('--image', required=True)
        if kind == 'playwright': b.add_argument('--browsers', nargs='+', choices=['chromium','firefox','webkit'], default=['chromium'])
    args = p.parse_args()
    with locked(args.root):
        if args.command == 'install-go': install_go(args, args.root)
        elif args.command.startswith('install-'): install_browser(args, args.root)
        elif args.command == 'verify': verify(args.root)
        else: print('Initialized', args.root)

if __name__ == '__main__': main()
