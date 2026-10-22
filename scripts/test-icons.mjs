// Icon slot registry: every icon the page references is a registered slot, the
// registry is well formed and translated, and the vendored sets only contain the
// shapes the renderer accepts. Pure file checks; no browser or server.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { ICON_SLOTS, ICON_GROUPS } from '../internal/web/static/js/icon-slots.js';
import { BUILTIN_ICONS } from '../internal/web/static/js/icon-builtin.js';
import enStatic from '../internal/web/static/js/locales/en-static.js';
import enDynamic from '../internal/web/static/js/locales/en-dynamic.js';
import twStatic from '../internal/web/static/js/locales/zh-TW-static.js';
import twDynamic from '../internal/web/static/js/locales/zh-TW-dynamic.js';

const root = new URL('../', import.meta.url);
const read = p => readFile(new URL(p, root), 'utf8');
const ids = Object.keys(ICON_SLOTS);

async function tsFiles(dir) {
  const out = [];
  for (const e of await readdir(new URL(dir, root), { withFileTypes: true })) {
    const p = dir + e.name;
    if (e.isDirectory()) out.push(...await tsFiles(p + '/'));
    else if (p.endsWith('.ts')) out.push(p);
  }
  return out;
}

test('every data-icon in markup and TS templates names a registered slot', async () => {
  const sources = [['internal/web/static/index.html', await read('internal/web/static/index.html')]];
  for (const f of await tsFiles('web/src/')) sources.push([f, await read(f)]);
  let seen = 0;
  for (const [file, text] of sources) {
    for (const m of text.matchAll(/data-icon="([^"$]*)"/g)) {
      seen++;
      assert.ok(Object.hasOwn(ICON_SLOTS, m[1]), `${file}: unknown icon slot "${m[1]}"`);
    }
  }
  assert.ok(seen > 250, 'expected the page to reference icon slots');
});

test('slots have one default source, existing bases and no cycles', () => {
  for (const id of ids) {
    const def = ICON_SLOTS[id];
    assert.ok(Object.hasOwn(ICON_GROUPS, def.group), `${id}: unknown group ${def.group}`);
    assert.ok(!!def.icon !== !!def.base, `${id}: needs exactly one of icon/base`);
    assert.match(id, /^[a-z][a-z0-9-]*(\.[a-z0-9-]+)*$/, `${id}: slot id must match the server pattern`);
    if (def.icon) assert.ok(BUILTIN_ICONS[def.icon], `${id}: default ${def.icon} missing from icon-builtin.ts`);
    const seen = new Set([id]);
    for (let cur = def; cur.base; cur = ICON_SLOTS[cur.base]) {
      assert.ok(Object.hasOwn(ICON_SLOTS, cur.base), `${id}: unknown base ${cur.base}`);
      assert.ok(!seen.has(cur.base), `${id}: base cycle`);
      seen.add(cur.base);
    }
  }
});

test('slot and group labels are translated', () => {
  for (const key of [...Object.values(ICON_GROUPS), ...ids.map(id => ICON_SLOTS[id].label)]) {
    assert.ok(Object.hasOwn(enDynamic, key) || Object.hasOwn(enStatic, key), `Missing English: ${key}`);
    assert.ok(Object.hasOwn(twDynamic, key) || Object.hasOwn(twStatic, key), `Missing Traditional: ${key}`);
  }
});

test('generated default shapes match the registry and vendored sets', () => {
  execFileSync(process.execPath, ['scripts/gen-icons.mjs', '--check'], { cwd: root, stdio: 'pipe' });
});

test('vendored sets only use shapes the renderer draws', async () => {
  const allowed = {
    path: ['d'], circle: ['cx', 'cy', 'r'], ellipse: ['cx', 'cy', 'rx', 'ry'],
    rect: ['x', 'y', 'width', 'height', 'rx', 'ry'], line: ['x1', 'y1', 'x2', 'y2'], polyline: ['points'], polygon: ['points'],
  };
  const paint = { fill: ['currentColor', 'none'], stroke: ['currentColor', 'none'], opacity: ['.5'] };
  for (const [set, style] of [['tabler', 'stroke'], ['tabler-filled', 'fill'], ['lucide', 'stroke']]) {
    const doc = JSON.parse(await read(`internal/web/static/vendor/icons/${set}.json`));
    assert.equal(doc.prefix, set);
    assert.equal(doc.style, style);
    assert.ok(Object.keys(doc.icons).length > 1000, `${set}: suspiciously small`);
    for (const [name, nodes] of Object.entries(doc.icons)) {
      assert.ok(Array.isArray(nodes) && nodes.length, `${set}:${name} empty`);
      for (const node of nodes) {
        if (typeof node === 'string') continue;
        const [tag, attrs] = node;
        assert.ok(allowed[tag], `${set}:${name} <${tag}>`);
        for (const [k, v] of Object.entries(attrs)) {
          assert.ok(allowed[tag].includes(k) || paint[k]?.includes(v), `${set}:${name} ${k}="${v}"`);
        }
      }
    }
  }
});
