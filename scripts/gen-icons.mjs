#!/usr/bin/env node
// Generate web/src/icon-builtin.ts: the shapes of every default icon referenced in
// web/src/icon-slots.ts, copied out of the vendored icon sets. The page renders
// these without fetching anything; the full sets load only in the icon picker.
//   node scripts/gen-icons.mjs          write the file
//   node scripts/gen-icons.mjs --check  fail when the committed file is stale
import { readFile, writeFile } from 'node:fs/promises';

const root = new URL('../', import.meta.url);
const SETS = ['tabler', 'tabler-filled', 'lucide'];
const OUT = new URL('web/src/icon-builtin.ts', root);

const slots = await readFile(new URL('web/src/icon-slots.ts', root), 'utf8');
const refs = [...new Set([...slots.matchAll(/"((?:tabler-filled|tabler|lucide):[a-z0-9-]+)"/g)].map(m => m[1]))].sort();

const sets = {};
for (const prefix of SETS) {
  sets[prefix] = JSON.parse(await readFile(new URL(`internal/web/static/vendor/icons/${prefix}.json`, root), 'utf8'));
}

const missing = [];
const lines = [];
for (const ref of refs) {
  const [prefix, name] = ref.split(':');
  const nodes = sets[prefix]?.icons[name];
  if (!nodes) { missing.push(ref); continue; }
  lines.push(`  ${JSON.stringify(ref)}: ${JSON.stringify(nodes)},`);
}
if (missing.length) {
  console.error('Unknown icons referenced in icon-slots.ts: ' + missing.join(', '));
  process.exit(1);
}

const styles = Object.fromEntries(SETS.map(p => [p, sets[p].style]));
const text = `// 由 scripts/gen-icons.mjs 根据 icon-slots.ts 生成，勿手改。
// 来源：${SETS.map(p => `${sets[p].package}@${sets[p].version}（${sets[p].license}）`).filter((v, i, a) => a.indexOf(v) === i).join('、')}

export type IconNode = string | [string, Record<string, string>];

export const ICON_SET_STYLES: Record<string, "stroke" | "fill"> = ${JSON.stringify(styles)};

export const BUILTIN_ICONS: Record<string, IconNode[]> = {
${lines.join('\n')}
};
`;

if (process.argv.includes('--check')) {
  const current = await readFile(OUT, 'utf8').catch(() => '');
  if (current !== text) {
    console.error('web/src/icon-builtin.ts is stale; run node scripts/gen-icons.mjs');
    process.exit(1);
  }
} else {
  await writeFile(OUT, text);
}
