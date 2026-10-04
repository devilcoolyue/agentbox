#!/usr/bin/env python3
"""Prepare/verify desktop-only release assets; never publishes or changes latest.

Requires actual updater signatures for all three architectures. Production OS
signing/notarization is checked by the packaging workflow, independently.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
from datetime import datetime, timezone

TARGETS = {'darwin-aarch64': '.app.tar.gz', 'darwin-x86_64': '.app.tar.gz', 'windows-x86_64': '.exe'}
REPO = 'devilcoolyue/agentbox'


def prepare(directory, version, assets, notes=''):
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?', version):
        raise ValueError('invalid desktop semver')
    if set(assets) != set(TARGETS):
        raise ValueError('all three supported architectures are required')
    platforms = {}
    checksums = []
    names = set()
    for target, suffix in TARGETS.items():
        name = assets[target]
        if Path(name).name != name or not name.endswith(suffix) or name in names:
            raise ValueError('invalid or colliding updater filename')
        names.add(name)
        path = directory / name
        signature_path = directory / (name + '.sig')
        if path.is_symlink() or signature_path.is_symlink() or not path.is_file() or not signature_path.is_file():
            raise ValueError('missing or linked asset/signature: ' + name)
        signature = signature_path.read_text(encoding='utf-8').strip()
        if not signature or len(signature) > 4096 or not path.stat().st_size:
            raise ValueError('empty/invalid updater asset or signature')
        # Signatures are verified cryptographically by the candidate gate and
        # by Tauri before installation. This function only assembles the feed.
        platforms[target] = {'signature': signature, 'url': f'https://github.com/{REPO}/releases/download/desktop-v{version}/{name}'}
        for file in (path, signature_path):
            with file.open('rb') as stream:
                digest = hashlib.file_digest(stream, 'sha256').hexdigest()
            checksums.append(f'{digest}  {file.name}')
    manifest = {'version': version, 'notes': notes, 'pub_date': datetime.now(timezone.utc).isoformat(), 'platforms': platforms}
    release = {'tag_name': 'desktop-v' + version, 'name': 'Agentbox Desktop ' + version, 'draft': True, 'prerelease': '-' in version, 'make_latest': 'false', 'body': notes}
    channel = {'tag_name': 'desktop-stable', 'name': 'Agentbox Desktop update channel', 'draft': True, 'make_latest': 'false'}
    return manifest, release, channel, '\n'.join(checksums) + '\n'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--directory', type=Path, required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--assets', type=Path, required=True, help='JSON mapping target to unique asset basename')
    parser.add_argument('--notes', type=Path)
    args = parser.parse_args()
    result = prepare(args.directory, args.version, json.loads(args.assets.read_text(encoding='utf-8')), args.notes.read_text(encoding='utf-8') if args.notes else '')
    for name, value in zip(['latest.json', 'release-request.json', 'channel-request.json'], result[:3]):
        (args.directory / name).write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')
    (args.directory / 'SHA256SUMS').write_text(result[3])
    print('Prepared desktop feed and draft API payloads with make_latest=false; nothing published')


if __name__ == '__main__':
    main()
