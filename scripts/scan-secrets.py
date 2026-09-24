#!/usr/bin/env python3
"""Scan all locally fetched refs, tracked files, and unpacked release candidates.

Reports are redacted. Fetch remote branches/tags before auditing. No network
upload or credential validity checks are performed by this script.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parent.parent
os.chdir(ROOT)
p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--output', required=True, help='new private report directory outside the repository')
p.add_argument('--artifacts', type=Path)
a = p.parse_args()
out = Path(a.output).resolve()
if out == ROOT or ROOT in out.parents:
    p.error('reports must be outside the repository')
os.umask(0o077)
out.mkdir(parents=True, exist_ok=False)
scanner = shutil.which('gitleaks')
if not scanner:
    candidate = Path(subprocess.check_output(['go', 'env', 'GOPATH'], text=True).strip()) / 'bin/gitleaks'
    if candidate.is_file():
        scanner = str(candidate)
if not scanner:
    p.error('install gitleaks v8.24.3 first')
results = []
def scan(label, args):
    report = out / (label + '.json')
    command = [scanner, *args, '--no-banner', '--redact=100', '--config', str(ROOT / '.gitleaks.toml'),
               '--report-format=json', '--report-path', str(report)]
    result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    (out / (label + '.log')).write_text(result.stdout)
    count = len(json.loads(report.read_text())) if report.exists() else None
    results.append({'scope': label, 'exit': result.returncode, 'findings': count})
    print(label, 'exit=' + str(result.returncode), 'findings=' + str(count), flush=True)
scan('history', ['git', '--log-opts=--all --full-history', str(ROOT)])
with tempfile.TemporaryDirectory(prefix='agentbox-secret-scan-') as tmp:
    stage = Path(tmp)
    tracked = stage / 'tracked'
    tracked.mkdir()
    for name in subprocess.check_output(['git', 'ls-files', '-z']).decode().split('\0'):
        if not name:
            continue
        src = ROOT / name
        if src.is_symlink():
            raise SystemExit('Review tracked symlink manually: ' + name)
        if src.is_file():
            dest = tracked / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(src, dest)
    scan('tracked', ['dir', str(tracked)])
    if a.artifacts:
        extracted = stage / 'artifacts'
        extracted.mkdir()
        for archive in sorted(a.artifacts.resolve().iterdir()):
            dest = extracted / archive.name
            if archive.name.endswith('.tar.gz'):
                with tarfile.open(archive) as t:
                    t.extractall(dest, filter='data')
            elif archive.suffix == '.zip':
                with zipfile.ZipFile(archive) as z:
                    for name in z.namelist():
                        target = (dest / name).resolve()
                        if dest.resolve() not in target.parents:
                            raise SystemExit('Unsafe archive member')
                    z.extractall(dest)
            elif archive.is_file():
                shutil.copyfile(archive, dest)
        # Gitleaks skips binary content. Also scan printable strings from the
        # exact release executables, including embedded frontend/config data.
        binaries = [f for f in extracted.rglob('*') if f.is_file() and (f.name in
                    ('agentbox', 'abox-link', 'abox-link.exe') or f.name.startswith('abox-link-'))]
        for binary in binaries:
            with binary.with_name(binary.name + '.strings.txt').open('wb') as output:
                subprocess.run(['strings', str(binary)], stdout=output, check=True)
        scan('artifacts', ['dir', str(extracted)])
refs = subprocess.check_output(['git', 'for-each-ref', '--format=%(refname) %(objectname)'], text=True)
(out / 'refs.txt').write_text(refs)
(out / 'summary.json').write_text(json.dumps(results, indent=2) + '\n')
if any(r['exit'] != 0 for r in results):
    raise SystemExit('Audit requires review; redacted reports: ' + str(out))
print('No unreviewed scanner findings. This is not proof that all sensitive information is absent.')
