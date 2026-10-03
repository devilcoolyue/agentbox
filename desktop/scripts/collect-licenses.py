#!/usr/bin/env python3
"""Generate a reproducible superset of notices for the locked desktop build.

Includes build-time crates and production npm packages; Go
notices come from the repository's hash-verified inventory. No guessed licenses.
"""
import hashlib
import json
from pathlib import Path
import subprocess

DESKTOP = Path(__file__).resolve().parents[1]
ROOT = DESKTOP.parent
OUT = DESKTOP / 'third-party'


def license_files(directory):
    result = []
    for path in sorted(directory.iterdir()):
        if path.is_file() and path.name.lower().startswith(('license', 'licence', 'copying', 'notice', 'copyright', 'unlicense')):
            result.append(path)
    licenses = directory / 'licenses'
    if licenses.is_dir():
        result.extend(sorted(p for p in licenses.rglob('*') if p.is_file()))
    return result


def main():
    records, texts = [], []
    metadata = json.loads(subprocess.check_output(['cargo', 'metadata', '--locked', '--format-version', '1', '--manifest-path', str(DESKTOP / 'src-tauri/Cargo.toml')], text=True))
    linked = set()
    for target in ('aarch64-apple-darwin','x86_64-apple-darwin','x86_64-pc-windows-msvc'):
        graph = json.loads(subprocess.check_output(['cargo','metadata','--locked','--format-version','1','--filter-platform',target,'--manifest-path',str(DESKTOP / 'src-tauri/Cargo.toml')],text=True))['resolve']
        nodes = {node['id']:node for node in graph['nodes']}
        pending=[graph['root']]
        visited=set()
        while pending:
            identifier=pending.pop()
            if identifier in visited: continue
            visited.add(identifier)
            for dep in nodes[identifier]['deps']:
                if any(kind['kind'] != 'dev' for kind in dep['dep_kinds']): pending.append(dep['pkg'])
        linked.update(visited)
    packages = []
    for package in metadata['packages']:
        if package.get('source') and package['id'] in linked:
            directory = Path(package['manifest_path']).parent
            paths = license_files(directory)
            if package.get('license_file'):
                path = directory / package['license_file']
                if path not in paths:
                    paths.append(path)
            if not paths:
                pinned = ROOT / 'third_party' / 'desktop' / (package['name'] + '@' + package['version'])
                paths = license_files(pinned) if pinned.is_dir() else []
                if paths:
                    provenance=json.loads((pinned/'source.json').read_text())
                    for entry in provenance['files']:
                        if hashlib.sha256((pinned/entry['file']).read_bytes()).hexdigest()!=entry['sha256']:
                            raise SystemExit('Pinned license hash mismatch: '+str(pinned))
            packages.append(('cargo', package['name'], package['version'], package.get('license'), paths))
    lock = json.loads((DESKTOP / 'package-lock.json').read_text())
    for relative, item in lock['packages'].items():
        if not relative or item.get('dev'):
            continue
        directory = DESKTOP / relative
        if not directory.is_dir():
            # Optional binaries for another platform are not in this installer.
            if item.get('optional'):
                continue
            raise SystemExit('Missing installed npm package: ' + relative)
        meta = json.loads((directory / 'package.json').read_text())
        paths=license_files(directory)
        if not paths:
            parent_name = 'esbuild' if meta['name'].startswith('@esbuild/') else 'rollup' if meta['name'].startswith('@rollup/rollup-') else '@tauri-apps/cli' if meta['name'].startswith('@tauri-apps/cli-') else None
            if parent_name:
                parent=DESKTOP/'node_modules'/parent_name
                parent_meta=json.loads((parent/'package.json').read_text())
                if parent_meta['version']!=meta['version']:raise SystemExit('Platform license version differs: '+meta['name'])
                paths=license_files(parent)
            else:
                pinned=ROOT/'third_party/desktop'/(meta['name'].replace('/','_')+'@'+meta['version'])
                if pinned.is_dir():paths=license_files(pinned)
        packages.append(('npm', meta['name'], meta['version'], meta.get('license'), paths))
    for item in json.loads((ROOT / 'third_party/go-modules.json').read_text()):
        paths = [ROOT / file['path'] for file in item['files']]
        packages.append(('go', item['module'], item['version'], item.get('license', 'See license text'), paths))
    for ecosystem, name, version, expression, paths in sorted(packages, key=lambda p: (p[0], p[1], p[2])):
        if not expression or not paths:
            raise SystemExit(f'Missing license declaration/text: {ecosystem}:{name}@{version}')
        record = {'ecosystem': ecosystem, 'name': name, 'version': version, 'license': expression, 'texts': []}
        for path in paths:
            raw = path.read_bytes()
            if not raw.strip():
                raise SystemExit('Empty license: ' + str(path))
            record['texts'].append({'name': path.name, 'sha256': hashlib.sha256(raw).hexdigest()})
            texts.append(f'\n===== {ecosystem}: {name}@{version} / {path.name} =====\n' + raw.decode('utf-8', errors='replace'))
        records.append(record)
    OUT.mkdir(exist_ok=True)
    (OUT / 'inventory.json').write_text(json.dumps(records, ensure_ascii=False, indent=2) + '\n', encoding='utf-8', newline='\n')
    # Disable platform newline conversion: some upstream texts already contain
    # CRLF. Windows must not turn them into CRCRLF or alter pinned license bytes.
    (OUT / 'NOTICES.txt').write_text('Agentbox Desktop third-party notices (includes build dependencies)\n' + '\n'.join(texts), encoding='utf-8', newline='\n')
    print(f'Collected license texts for {len(records)} dependencies')


if __name__ == '__main__':
    main()
