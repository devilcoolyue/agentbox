#!/usr/bin/env python3
"""Build release archives locally. Publication is a separate, manual operation."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parent.parent
os.chdir(ROOT)
p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--version', required=True)
p.add_argument('--output', required=True, help='new output directory (refuses existing paths)')
p.add_argument('--allow-dirty', action='store_true', help='local candidate only; never used by release CI')
a = p.parse_args()
if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?', a.version):
    p.error('version must be vMAJOR.MINOR.PATCH[-prerelease]')
out = Path(a.output).resolve()
if out.exists():
    p.error('output already exists')
if not (ROOT / 'LICENSE').is_file():
    p.error('project owner must choose LICENSE before creating distributable packages')
dirty = bool(subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=normal'], text=True).strip())
if dirty and not a.allow_dirty:
    p.error('release requires a clean checkout')
revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
if dirty:
    revision += '+dirty'
built = subprocess.check_output(['git', 'show', '-s', '--format=%cI', 'HEAD'], text=True).strip()
subprocess.run(['npm', 'ci'], check=True)
subprocess.run(['npm', 'run', 'check'], check=True)
subprocess.run(['npm', 'run', 'build'], check=True)
if not a.allow_dirty:
    subprocess.run(['git', 'diff', '--exit-code', '--', 'internal/web/static/js'], check=True)
subprocess.run(['python3', 'scripts/verify-third-party.py'], check=True)
flags = f'-s -w -X agentbox/internal/buildinfo.Version={a.version} -X agentbox/internal/buildinfo.Revision={revision} -X agentbox/internal/buildinfo.BuiltAt={built}'
def copy_tracked_tree(name, destination, required=()):
    # Never package ignored files, credentials or a developer's extra files.
    files = subprocess.check_output(['git', 'ls-files', '-z', name]).decode().split('\0')
    # Explicit resources are also required in --allow-dirty local candidates,
    # where newly added default configuration may not be tracked yet.
    for entry in sorted(set(files) | set(required)):
        if not entry:
            continue
        source = ROOT / entry
        if source.is_symlink():
            raise SystemExit('Review symlink before packaging: ' + entry)
        target = destination / Path(entry).relative_to(name)
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)

# Build in private staging and publish only after every platform succeeds.
out.parent.mkdir(parents=True, exist_ok=True)
with tempfile.TemporaryDirectory(prefix='agentbox-release-', dir=out.parent) as tmp:
    stage = Path(tmp)
    products = stage / 'artifacts'
    products.mkdir()
    records = []
    clients = stage / 'clients'; clients.mkdir()
    targets = [('abox-link', system, arch) for system, arch in
                [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'), ('darwin', 'arm64'), ('windows', 'amd64')]]
    targets += [('agentbox', 'linux', arch) for arch in ['amd64', 'arm64']]
    for program, system, arch in targets:
        name = f'{program}_{a.version}_{system}_{arch}'
        folder = stage / name
        folder.mkdir()
        binary = folder / (program + ('.exe' if system == 'windows' else ''))
        env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED='0')
        subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-ldflags', flags,
                        '-o', str(binary), './cmd/' + program], check=True, env=env)
        if program == 'abox-link':
            shutil.copy2(binary, clients / (f'abox-link-{system}-{arch}' + ('.exe' if system == 'windows' else '')))
        for doc in ['LICENSE', 'NOTICE']:
            shutil.copy2(ROOT / doc, folder / doc)
        # Binary packages contain operator instructions; architecture, audit,
        # and development documents remain available in the source repository.
        shutil.copy2(ROOT / 'deploy/downloads/README.md', folder / 'README.md')
        copy_tracked_tree('third_party', folder / 'third_party')
        if program == 'agentbox':
            shutil.copy2(ROOT / 'install.sh', folder / 'install.sh')
            shutil.copy2(ROOT / 'uninstall.sh', folder / 'uninstall.sh')
            shutil.copytree(clients, folder / 'clients')
            shutil.copy2(ROOT / 'config.example.json', folder / 'config.example.json')
            copy_tracked_tree('images', folder / 'images', required=(
                'images/agent/bashrc', 'images/agent/vimrc'))
            (folder / 'scripts').mkdir()
            for script in ['build-image.sh', 'build-browser-image.sh', 'backup.sh']:
                shutil.copy2(ROOT / 'scripts' / script, folder / 'scripts' / script)
            (folder / 'deploy').mkdir()
            shutil.copy2(ROOT / 'deploy/downloads/README.md', folder / 'deploy/README.md')
            for deploy_file in ['release.py', 'bootstrap.py', 'update.py']:
                shutil.copy2(ROOT / 'deploy' / deploy_file, folder / 'deploy' / deploy_file)
        metadata = {'program': program, 'version': a.version, 'revision': revision,
                    'built_at': built, 'os': system, 'arch': arch,
                    'go': subprocess.check_output(['go', 'version'], text=True).strip()}
        (folder / 'build.json').write_text(json.dumps(metadata, indent=2) + '\n')
        if system == 'windows':
            archive = products / (name + '.zip')
            with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as z:
                for f in sorted(folder.rglob('*')):
                    if f.is_file():
                        z.write(f, f.relative_to(stage))
        else:
            archive = products / (name + '.tar.gz')
            with tarfile.open(archive, 'w:gz') as tar:
                tar.add(folder, arcname=name)
        records.append(metadata | {'archive': archive.name})
        print('Built', archive.name, flush=True)
    (products / 'release.json').write_text(json.dumps(records, indent=2) + '\n')
    sums = [hashlib.sha256(f.read_bytes()).hexdigest() + '  ' + f.name for f in sorted(products.iterdir())]
    (products / 'SHA256SUMS').write_text('\n'.join(sums) + '\n')
    # Refuse an output directory created by another process while building.
    out.mkdir()
    for artifact in products.iterdir():
        shutil.move(artifact, out / artifact.name)
print('Release candidate:', out)
