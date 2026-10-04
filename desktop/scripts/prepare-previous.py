#!/usr/bin/env python3
"""Download one frozen desktop release package for the isolated upgrade probe.

Only this repository's desktop-v<semver> releases are accepted. SHA256SUMS is
required and checked, but does not replace OS/updater signature verification.
No release, tag or workflow is changed.
"""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
from urllib.parse import urlsplit

REPOSITORY = 'devilcoolyue/agentbox'
TARGETS = {'aarch64-apple-darwin': 'darwin-aarch64', 'x86_64-apple-darwin': 'darwin-x86_64', 'x86_64-pc-windows-msvc': 'windows-x86_64'}
LIMIT = 512 << 20


def validate_tag(tag):
    if not re.fullmatch(r'desktop-v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?', tag):
        raise ValueError('Previous release must be an exact desktop-v<semver> tag')


def select_asset(manifest, tag, target):
    validate_tag(tag)
    if manifest.get('version') != tag.removeprefix('desktop-v'):
        raise ValueError('Previous manifest version differs from tag')
    item = manifest['platforms'][TARGETS[target]]
    address = urlsplit(item['url'])
    name = PurePosixPath(address.path).name
    suffix = '.exe' if target.endswith('windows-msvc') else '.app.tar.gz'
    expected = f'https://github.com/{REPOSITORY}/releases/download/{tag}/{name}'
    if not re.fullmatch(r'[A-Za-z0-9_.-]+', name) or not name.endswith(suffix) or item['url'] != expected:
        raise ValueError('Previous package must belong to the exact repository, tag and platform')
    return name


def verify_checksum(path, checksums):
    entries = {}
    for line in checksums.splitlines():
        match = re.fullmatch(r'([0-9a-f]{64})  ([A-Za-z0-9_.-]+)', line)
        if not match or match[2] in entries:
            raise ValueError('Invalid or duplicate checksum entry')
        entries[match[2]] = match[1]
    with path.open('rb') as stream:
        actual = hashlib.file_digest(stream, 'sha256').hexdigest()
    if actual != entries.get(path.name):
        raise ValueError('Previous package SHA256 mismatch or missing checksum')
    return actual


def extract_app(archive, directory):
    # Preflight the entire bounded member list before any extraction. data_filter
    # additionally rejects links escaping the temporary destination directory.
    with tarfile.open(archive, 'r:gz') as bundle:
        total = 0
        count = 0
        for member in bundle:
            count += 1
            total += member.size
            path = PurePosixPath(member.name)
            if count > 20000 or total > 1 << 30 or path.is_absolute() or '..' in path.parts or '\\' in member.name or not path.parts or not path.parts[0].endswith('.app') or not (member.isfile() or member.isdir() or member.issym() or member.islnk()):
                raise ValueError('Unsafe or oversized previous macOS bundle')
        bundle.extractall(directory, filter='data')
    apps = list(directory.glob('*.app'))
    if len(apps) != 1 or apps[0].is_symlink() or not (apps[0] / 'Contents/MacOS/agentbox-desktop').is_file():
        raise ValueError('Previous archive must contain exactly one desktop app')
    return apps[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--target', choices=TARGETS, required=True)
    parser.add_argument('--directory', type=Path, required=True)
    args = parser.parse_args()
    validate_tag(args.tag)
    directory = args.directory.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    metadata = json.loads(subprocess.check_output(['gh', 'api', f'repos/{REPOSITORY}/releases/tags/{args.tag}'], timeout=60))
    if metadata.get('tag_name') != args.tag or metadata.get('draft'):
        raise ValueError('Previous release is missing or draft')
    assets = metadata['assets']

    def download(name, limit):
        matches = [asset for asset in assets if asset['name'] == name]
        if len(matches) != 1 or not 0 < matches[0]['size'] <= limit:
            raise ValueError('Missing, duplicate or oversized previous asset: ' + name)
        subprocess.run(['gh', 'release', 'download', args.tag, '--repo', REPOSITORY, '--pattern', name, '--dir', str(directory)], check=True, timeout=180)
        path = directory / name
        if not path.is_file() or path.is_symlink() or path.stat().st_size != matches[0]['size']:
            raise ValueError('Downloaded asset size mismatch')
        return path

    manifest = json.loads(download('latest.json', 2 << 20).read_text(encoding='utf-8'))
    checksums = download('SHA256SUMS', 64 << 10).read_text(encoding='utf-8')
    archive = download(select_asset(manifest, args.tag, args.target), LIMIT)
    digest = verify_checksum(archive, checksums)
    package = archive if args.target.endswith('windows-msvc') else extract_app(archive, directory / 'extracted')
    provenance = {'repository': REPOSITORY, 'tag': args.tag, 'target': args.target, 'asset': archive.name, 'sha256': digest, 'package': str(package), 'os_signature_verified': False}
    (directory / 'previous-package.json').write_text(json.dumps(provenance, indent=2) + '\n')
    print(json.dumps(provenance, indent=2))


if __name__ == '__main__':
    main()
