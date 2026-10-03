#!/usr/bin/env python3
"""Verify vendored bytes and the license inventory before packaging."""
import hashlib
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parent.parent
for manifest in ['vendor.json', 'go-modules.json']:
    for item in json.loads((ROOT / 'third_party' / manifest).read_text()):
        for entry in item['files']:
            raw = (ROOT / entry['path']).read_bytes()
            if hashlib.sha256(raw).hexdigest() != entry['sha256']:
                raise SystemExit('Third-party hash mismatch: ' + entry['path'])
        for license in item.get('license_files', []):
            if not (ROOT / license).read_text().strip():
                raise SystemExit('Empty license: ' + license)
# Reject stale module versions, including dependencies no longer selected.
selected = dict(line.split() for line in subprocess.check_output(
    ['go', 'list', '-m', '-f', '{{if not .Main}}{{.Path}} {{.Version}}{{end}}', 'all'],
    cwd=ROOT, text=True).splitlines() if line.strip())
for item in json.loads((ROOT / 'third_party' / 'go-modules.json').read_text()):
    if selected.get(item['module']) != item['version']:
        raise SystemExit('Refresh Go license inventory: ' + item['module'])

# A new linked module must not disappear from the notices merely because all old
# entries still match. Check the union of supported build targets.
linked = set()
for system, arch, package in [
    ('linux','amd64','agentbox'), ('linux','arm64','agentbox'),
    ('linux','amd64','abox-link'), ('linux','arm64','abox-link'),
    ('darwin','amd64','abox-link'), ('darwin','arm64','abox-link'), ('windows','amd64','abox-link'),
    ('darwin','amd64','abox-sync'), ('darwin','arm64','abox-sync'), ('windows','amd64','abox-sync')]:
    env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED='0')
    lines = subprocess.check_output(['go','list','-deps','-f',
        '{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}{{end}}{{end}}','./cmd/'+package],
        cwd=ROOT, env=env, text=True).splitlines()
    linked.update(line for line in lines if line)
recorded = {x['module'] for x in json.loads((ROOT/'third_party/go-modules.json').read_text())}
if linked != recorded:
    raise SystemExit('Linked module/license inventory differs: '+str(linked ^ recorded))
print('Third-party inventory and hashes verified')
