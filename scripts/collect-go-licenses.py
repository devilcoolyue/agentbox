#!/usr/bin/env python3
"""Record licenses for modules linked into the distributed binaries (all targets)."""
import hashlib
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parent.parent
os.chdir(ROOT)
modules = {}
for goos, goarch, package in [
    ('linux', 'amd64', './cmd/agentbox'), ('linux', 'arm64', './cmd/agentbox'),
    ('linux', 'amd64', './cmd/abox-link'), ('linux', 'arm64', './cmd/abox-link'),
    ('darwin', 'amd64', './cmd/abox-link'), ('darwin', 'arm64', './cmd/abox-link'),
    ('windows', 'amd64', './cmd/abox-link'),
]:
    env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0')
    raw = subprocess.check_output(['go', 'list', '-deps', '-json', package], env=env, text=True)
    decoder = json.JSONDecoder()
    while raw.strip():
        item, end = decoder.raw_decode(raw.lstrip())
        raw = raw.lstrip()[end:]
        module = item.get('Module', {})
        if module and not module.get('Main'):
            if module.get('Replace'):
                raise SystemExit('Review replaced dependency before release: ' + module['Path'])
            modules[module['Path']] = module

out = ROOT / 'third_party' / 'go'
out.mkdir(parents=True, exist_ok=True)
manifest = []
for name, module in sorted(modules.items()):
    source = Path(module['Dir'])
    # Include nested license/notice files as dependencies may vendor other code.
    candidates = sorted(p for p in source.rglob('*') if p.is_file() and
                        p.name.lower().split('.')[0] in ('license', 'licence', 'copying', 'notice', 'copyright'))
    if not candidates:
        raise SystemExit('No license found; manual review required: ' + name)
    entry = {'module': name, 'version': module['Version'], 'source': 'https://' + name, 'files': []}
    for p in candidates:
        dest = out / (name.replace('/', '_') + '@' + module['Version']) / p.relative_to(source)
        dest.parent.mkdir(parents=True, exist_ok=True)
        raw = p.read_bytes()
        dest.write_bytes(raw)
        entry['files'].append({'path': dest.relative_to(ROOT).as_posix(), 'sha256': hashlib.sha256(raw).hexdigest()})
    manifest.append(entry)
(ROOT / 'third_party' / 'go-modules.json').write_text(json.dumps(manifest, indent=2) + '\n')
# The standard library is also linked into release executables.
goroot = Path(subprocess.check_output(['go', 'env', 'GOROOT'], text=True).strip())
(ROOT / 'third_party' / 'licenses' / 'Go-LICENSE').write_bytes((goroot / 'LICENSE').read_bytes())
print('Collected licenses for', len(manifest), 'linked modules and Go standard library')
