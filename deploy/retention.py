#!/usr/bin/env python3
"""Preview or remove retired release packages under the shared deployment lock."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import shutil

VERSION = re.compile(r'v\d+\.\d+\.\d+(?:-[\w.-]+)?')

def references():
    refs = []
    for proc in Path('/proc').iterdir():
        if not proc.name.isdigit():
            continue
        try:
            links = [proc / 'exe', proc / 'cwd', *list((proc / 'fd').iterdir())]
            for link in links:
                try:
                    value = os.readlink(link).removesuffix(' (deleted)')
                    if value.startswith('/'):
                        refs.append(Path(value))
                except FileNotFoundError:
                    pass
            for arg in (proc / 'cmdline').read_bytes().split(b'\x00'):
                if arg.startswith(b'/'):
                    refs.append(Path(os.fsdecode(arg)))
            for line in (proc / 'maps').read_text().splitlines():
                fields = line.split(None, 5)
                if len(fields) == 6 and fields[5].startswith('/'):
                    refs.append(Path(fields[5].removesuffix(' (deleted)')))
        except (FileNotFoundError, ProcessLookupError):
            pass
        # A permission failure deliberately aborts, rather than assuming unused.
    return refs

def plan(app, keep, protected=(), refs=()):
    if keep == 0:
        return []
    if keep < 2:
        raise ValueError('keep must be zero or at least two')
    if any(p.is_symlink() for p in (app, *app.parents)):
        raise ValueError('app ancestry must not be symlinked')
    releases = app / 'releases'
    if releases.is_symlink():
        raise ValueError('release root must not be symlinked')
    current = (app / 'current').resolve(strict=True)
    if current.parent != releases:
        raise ValueError('current is outside release root')
    packages = []
    for p in releases.iterdir():
        if p.is_symlink() or not p.is_dir() or not VERSION.fullmatch(p.name):
            continue
        if p.stat().st_uid != os.geteuid() or p.stat().st_mode & 0o022:
            continue
        if not (p / 'agentbox').is_file() or (p / 'agentbox').is_symlink():
            continue
        packages.append(p)
    packages.sort(key=lambda p: p.stat().st_mtime, reverse=True)
    held = {current, *packages[:keep]}
    for name in protected:
        if not VERSION.fullmatch(name):
            raise ValueError('invalid protected version')
        held.add(releases / name)
    for p in app.iterdir():
        if p.is_symlink():
            held.add(p.resolve())
    return [p for p in packages if p not in held and not any(r == p or p in r.parents for r in refs)]

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--app', type=Path, default=Path('/opt/agentbox'))
    parser.add_argument('--config', type=Path, default=Path('/etc/agentbox/config.json'))
    parser.add_argument('--protect', action='append', default=[])
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    app = args.app.absolute()
    # Validate before opening the deployment lock through a possibly wrong root.
    if any(p.is_symlink() for p in (app, *app.parents)):
        raise ValueError('app ancestry must not be symlinked')
    policy = json.loads(args.config.read_text()).get('storage_policy', {})
    keep = policy.get('release_keep', 0)
    with (app / '.deploy.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        targets = plan(app, keep, args.protect, references())
        print(json.dumps({'apply': args.apply, 'keep': keep, 'targets': [str(p) for p in targets]}), flush=True)
        if args.apply:
            if not shutil.rmtree.avoids_symlink_attacks:
                raise RuntimeError('safe recursive deletion unavailable')
            for p in targets:
                # Recheck process references immediately before each deletion.
                if p not in plan(app, keep, args.protect, references()):
                    continue
                shutil.rmtree(p)
                print('removed', p.name, flush=True)

if __name__ == '__main__':
    main()
