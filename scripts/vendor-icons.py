#!/usr/bin/env python3
"""Vendor the Tabler and Lucide icon sets as compact JSON for the icon manager.

Downloads the pinned npm tarballs, checks their integrity, reads the upstream
node JSON (no SVG parsing) and writes internal/web/static/vendor/icons/*.json.
Only whitelisted shape tags/attributes survive, so the browser can build the
icons with createElementNS and never needs innerHTML. Also refreshes the
license copies and the matching third_party/vendor.json entries.

  python3 scripts/vendor-icons.py            # download, write, update manifest
  python3 scripts/vendor-icons.py --check    # regenerate in memory, compare bytes
  python3 scripts/vendor-icons.py --from DIR # use DIR/<tarball name> instead of downloading
"""
import argparse
import base64
import hashlib
import io
import json
import sys
import tarfile
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / 'internal/web/static/vendor/icons'
LICENSES = ROOT / 'third_party/licenses'

PACKAGES = [
    {
        'name': '@tabler/icons', 'version': '3.49.0', 'license': 'MIT',
        'source': 'https://registry.npmjs.org/@tabler/icons/-/icons-3.49.0.tgz',
        'integrity': 'sha512-l+GnSkFETx0U26icxv1VUFnxxiTqXfCcVRiRAnyMwibupM2S2L/hSaZ9gmSgz01rkrsH/93NvLsxykqdanc6lw==',
        'license_file': 'tabler-icons-LICENSE',
        'sets': [
            {'prefix': 'tabler', 'style': 'stroke', 'nodes': 'package/tabler-nodes-outline.json', 'tags': 'tabler-outline'},
            {'prefix': 'tabler-filled', 'style': 'fill', 'nodes': 'package/tabler-nodes-filled.json', 'tags': 'tabler-filled'},
        ],
    },
    {
        'name': 'lucide-static', 'version': '1.61.0', 'license': 'ISC',
        'source': 'https://registry.npmjs.org/lucide-static/-/lucide-static-1.61.0.tgz',
        'integrity': 'sha512-yLAiQB3MWUJEstsEDAwenSA3m7vr1tdLvgnC16Q/XMxRRma0DWwMs8M2bWlvgShrIs1m2mWMJ+0AiahqQO4GrA==',
        'license_file': 'lucide-LICENSE',
        'sets': [
            {'prefix': 'lucide', 'style': 'stroke', 'nodes': 'package/icon-nodes.json', 'tags': 'lucide'},
        ],
    },
]

# Shape vocabulary the frontend renderer accepts (web/src/icons.ts keeps the same list).
TAGS = {
    'path': {'d'},
    'circle': {'cx', 'cy', 'r'},
    'ellipse': {'cx', 'cy', 'rx', 'ry'},
    'rect': {'x', 'y', 'width', 'height', 'rx', 'ry'},
    'line': {'x1', 'y1', 'x2', 'y2'},
    'polyline': {'points'},
    'polygon': {'points'},
}
PAINT = {'fill': {'currentColor', 'none'}, 'stroke': {'currentColor', 'none'}, 'opacity': {'.5'}}
# Tabler draws a transparent 24x24 box to keep the artboard; it renders nothing.
ARTBOARD = {'d': 'M0 0h24v24H0z', 'fill': 'none', 'stroke': 'none'}


def fetch(pkg, cache):
    name = pkg['source'].rsplit('/', 1)[1]
    if cache:
        raw = (Path(cache) / name).read_bytes()
    else:
        with urllib.request.urlopen(pkg['source'], timeout=120) as r:
            raw = r.read()
    algo, want = pkg['integrity'].split('-', 1)
    if base64.b64encode(hashlib.new(algo, raw).digest()).decode() != want:
        raise SystemExit('Integrity mismatch: ' + pkg['source'])
    return raw


def member(tar, path):
    f = tar.extractfile(path)
    if f is None:
        raise SystemExit('Missing in tarball: ' + path)
    return f.read()


def compact(name, nodes):
    """[tag, attrs] -> "d" for a bare path, otherwise [tag, attrs] with whitelisted keys only."""
    out = []
    for tag, attrs in nodes:
        if tag == 'path' and attrs == ARTBOARD:
            continue
        allowed = TAGS.get(tag)
        if allowed is None:
            raise SystemExit(f'{name}: unsupported element <{tag}>')
        clean = {}
        for k, v in attrs.items():
            if k in allowed:
                clean[k] = str(v)
            elif k in PAINT and str(v) in PAINT[k]:
                clean[k] = str(v)
            else:
                raise SystemExit(f'{name}: unsupported attribute {k}="{v}" on <{tag}>')
        out.append(clean['d'] if tag == 'path' and list(clean) == ['d'] else [tag, clean])
    if not out:
        raise SystemExit(f'{name}: empty icon')
    return out


def tag_index(tar, kind):
    if kind.startswith('tabler'):
        meta = json.loads(member(tar, 'package/icons.json'))
        style = 'filled' if kind == 'tabler-filled' else 'outline'
        return {k: ' '.join(str(t) for t in [v.get('category', '')] + v.get('tags', [])).strip().lower()
                for k, v in meta.items() if style in v.get('styles', {})}
    meta = json.loads(member(tar, 'package/tags.json'))
    return {k: ' '.join(str(t) for t in v).lower() for k, v in meta.items()}


def build(cache):
    files, licenses = {}, {}
    for pkg in PACKAGES:
        with tarfile.open(fileobj=io.BytesIO(fetch(pkg, cache)), mode='r:gz') as tar:
            licenses[pkg['license_file']] = member(tar, 'package/LICENSE')
            for s in pkg['sets']:
                nodes = json.loads(member(tar, s['nodes']))
                tags = tag_index(tar, s['tags'])
                icons = {k: compact(f"{s['prefix']}:{k}", v) for k, v in sorted(nodes.items())}
                doc = {
                    'prefix': s['prefix'], 'style': s['style'], 'package': pkg['name'],
                    'version': pkg['version'], 'license': pkg['license'],
                    'icons': icons,
                    'tags': {k: tags[k] for k in icons if tags.get(k)},
                }
                files[s['prefix'] + '.json'] = (json.dumps(doc, ensure_ascii=False, separators=(',', ':')) + '\n').encode()
    return files, licenses


def manifest_entries(files):
    entries = []
    for pkg in PACKAGES:
        entries.append({
            'name': pkg['name'], 'version': pkg['version'], 'source': pkg['source'],
            'integrity': pkg['integrity'], 'license': pkg['license'],
            'modifications': 'Repacked the upstream icon node JSON into compact per-set JSON: dropped the transparent '
                             'artboard path, kept only whitelisted shape tags/attributes, merged search tags. Shapes unchanged.',
            'license_files': ['third_party/licenses/' + pkg['license_file']],
            'files': [{'path': str((OUT / (s['prefix'] + '.json')).relative_to(ROOT)),
                       'sha256': hashlib.sha256(files[s['prefix'] + '.json']).hexdigest()} for s in pkg['sets']],
        })
    return entries


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--check', action='store_true')
    ap.add_argument('--from', dest='cache')
    args = ap.parse_args()
    files, licenses = build(args.cache)
    if args.check:
        stale = [n for n, b in files.items() if (OUT / n).read_bytes() != b]
        stale += [n for n, b in licenses.items() if (LICENSES / n).read_bytes() != b]
        if stale:
            raise SystemExit('Vendored icons are stale: ' + ', '.join(stale))
        print('Vendored icons match upstream')
        return
    OUT.mkdir(parents=True, exist_ok=True)
    for n, b in files.items():
        (OUT / n).write_bytes(b)
    for n, b in licenses.items():
        (LICENSES / n).write_bytes(b)
    vendor_path = ROOT / 'third_party/vendor.json'
    names = {p['name'] for p in PACKAGES}
    vendor = [x for x in json.loads(vendor_path.read_text()) if x['name'] not in names] + manifest_entries(files)
    vendor_path.write_text(json.dumps(vendor, ensure_ascii=False, indent=2) + '\n')
    for n, b in files.items():
        print(f'{n}: {len(json.loads(b)["icons"])} icons, {len(b)} bytes')


if __name__ == '__main__':
    sys.exit(main())
