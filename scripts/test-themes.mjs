import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { assertActionIcons } from './test-actions.mjs';

// Interface theme manager regression on the synthetic browser suite (called by test-browser.mjs):
// built-in templates, import/apply/export/delete for personal and shared themes, contrast and
// validation messages, first-paint restore from the cache, and falling back when a theme is gone.
// The /api/themes fixture mirrors internal/theme's allowlist but does not re-implement its
// validation; Go tests cover the server rules.
const SKINS = ['amber', 'glass', 'cyberpunk', 'graphite', 'verdant', 'blueprint'];

// Children with a background must not poke out of a rounded parent that does not clip them
// (the active row once painted square corners over the list's rounded border).
const cornerLeaks = root => [...document.querySelectorAll(root + ' *')].flatMap(parent => {
  const ps = getComputedStyle(parent);
  if (ps.overflowX !== 'visible' || ps.overflowY !== 'visible') return [];
  const pr = parent.getBoundingClientRect(), bw = parseFloat(ps.borderTopWidth) || 0;
  return [...parent.children].filter(child => {
    const cs = getComputedStyle(child), cr = child.getBoundingClientRect();
    const alpha = (cs.backgroundColor.match(/[\d.]+/g) || [])[3];
    if (!cr.width || cs.position === 'absolute' || (cs.backgroundImage === 'none' && (alpha === undefined ? cs.backgroundColor === 'transparent' : Number(alpha) < .02))) return false;
    const corner = (touch, outer, inner) => touch && (parseFloat(outer) || 0) - bw > 2 && (parseFloat(inner) || 0) < (parseFloat(outer) || 0) - bw - 2;
    const top = cr.top <= pr.top + bw + .5, bottom = cr.bottom >= pr.bottom - bw - .5, left = cr.left <= pr.left + bw + .5, right = cr.right >= pr.right - bw - .5;
    return corner(top && left, ps.borderTopLeftRadius, cs.borderTopLeftRadius) || corner(top && right, ps.borderTopRightRadius, cs.borderTopRightRadius)
      || corner(bottom && left, ps.borderBottomLeftRadius, cs.borderBottomLeftRadius) || corner(bottom && right, ps.borderBottomRightRadius, cs.borderBottomRightRadius);
  }).map(child => (parent.id || parent.className) + ' > ' + child.className);
});

async function allowlist() {
  const source = await readFile(new URL('../internal/theme/manifest.go', import.meta.url), 'utf8');
  const block = source.slice(source.indexOf('var tokenList'), source.indexOf('var tokenKinds'));
  const kinds = { KindColor: 'color', KindShadow: 'shadow', KindCanvas: 'canvas', KindScale: 'scale', KindRadius: 'radius', KindFont: 'font' };
  return [...block.matchAll(/\{"(--[a-z0-9-]+)", (Kind\w+)\}/g)].map(([, name, kind]) => ({ name, kind: kinds[kind] }));
}

export async function themesSmoke(page) {
  const tokens = await allowlist();
  assert.ok(tokens.length > 50, 'token allowlist parsed');
  const state = { site: [], user: [], admin: true, puts: 0 };
  const view = (scope, manifest) => ({ scope, manifest, updated_at: Date.now() });
  await page.route('**/api/themes**', async route => {
    const request = route.request(), url = new URL(request.url()), method = request.method();
    if (url.pathname === '/api/themes') {
      await route.fulfill({ json: { format: 1, bases: SKINS, tokens, site: state.site, user: state.user, can_manage_site: state.admin, limits: { site: 50, user: 20, bytes: 65536 } } });
      return;
    }
    const [, , , scope, id] = url.pathname.split('/');
    assert.ok(request.headers().authorization?.startsWith('Bearer '), 'theme writes use the Authorization header');
    if (method === 'PUT') {
      state.puts++;
      const manifest = JSON.parse(request.postData());
      assert.equal(manifest.id, decodeURIComponent(id));
      if (JSON.stringify(manifest).includes('url(')) {
        await route.fulfill({ status: 400, json: { error: 'dark 里 --bg 的取值无效：不允许的函数 url()', theme_error: { code: 'bad_value', token: '--bg', section: 'dark' } } });
        return;
      }
      state[scope] = state[scope].filter(v => v.manifest.id !== manifest.id).concat(view(scope, manifest));
      await route.fulfill({ json: view(scope, manifest) });
    } else if (method === 'DELETE') {
      state[scope] = state[scope].filter(v => v.manifest.id !== decodeURIComponent(id));
      await route.fulfill({ json: { deleted: true } });
    } else await route.fallback();
  });
  const root = name => page.evaluate(name => document.documentElement.dataset[name], name);
  const token = name => page.evaluate(name => getComputedStyle(document.documentElement).getPropertyValue(name).trim(), name);
  const statusText = async () => (await page.locator('#themes-status').innerText()).trim();
  const dialog = page.locator('#dlg-themes');
  const openManager = async () => {
    await page.locator('#btn-user-menu').click();
    const trigger = page.locator('#pref-skin .pref-trigger');
    await trigger.click();
    await page.locator('#pref-skin .pref-menu-action').click();
    await dialog.waitFor({ state: 'visible' });
    await page.locator('#themes-builtin .themes-row').first().waitFor();
  };
  const importFile = async (button, manifest) => {
    const chooser = page.waitForEvent('filechooser');
    await page.locator(button).click();
    await (await chooser).setFiles({ name: 'theme.json', mimeType: 'application/json', buffer: Buffer.from(typeof manifest === 'string' ? manifest : JSON.stringify(manifest)) });
  };
  const moduleURL = () => page.evaluate(() => new URL('themes.js', document.querySelector('script[type="module"]').src).href);

  try {
    await page.evaluate(() => { localStorage.removeItem('agentbox_skin'); localStorage.removeItem('agentbox_skin_custom'); });
    await page.reload();
    await page.locator('#app').waitFor({ state: 'visible' });
    await openManager();
    assert.equal(await page.locator('#sidebar-user-menu').evaluate(el => el.classList.contains('open')), false, 'opening the manager closes the user menu');
    assert.deepEqual(await page.locator('#themes-builtin .themes-row').evaluateAll(rows => rows.map(r => r.dataset.key)), SKINS);
    assert.equal(await page.locator('#themes-builtin .themes-row.active').getAttribute('data-key'), 'amber');
    assert.deepEqual(await page.evaluate(`(${cornerLeaks})('#dlg-themes')`), [], 'the active first row stays inside the rounded list');
    assert.equal(await page.locator('#themes-import-site').isVisible(), true, 'admins can import shared themes');
    assert.match(await page.locator('#themes-user .themes-empty').innerText(), /还没有个人主题/);
    await assertActionIcons(page, '#dlg-themes');

    // Every built-in style exports a complete template that passes the contrast check.
    const url = await moduleURL();
    const templates = await page.evaluate(async ({ url, tokens, skins }) => {
      const { builtinManifest, contrastIssues } = await import(url);
      return skins.map(skin => { const m = builtinManifest(skin, tokens); return { m, issues: contrastIssues(m, tokens) }; });
    }, { url, tokens, skins: SKINS });
    const perMode = tokens.filter(t => !['scale', 'radius', 'font'].includes(t.kind)).map(t => t.name);
    for (const [i, { m, issues }] of templates.entries()) {
      assert.equal(m.base, SKINS[i]);
      assert.deepEqual(issues, [], `${m.base} fails its own contrast check`);
      for (const mode of ['dark', 'light']) {
        // Effect tokens only exist for the styles that use them.
        const missing = perMode.filter(name => !(name in m[mode]) && !['--skin-detail', '--glass-glint', '--glass-edge'].includes(name));
        assert.deepEqual(missing, [], `${m.base}/${mode} template is incomplete`);
        for (const value of Object.values(m[mode])) assert.ok(!/\n|\s{2}/.test(value), 'template values are single-line');
      }
      assert.ok(m.common['--radius-scale'] && m.common['--sans'], `${m.base} template has shape and fonts`);
    }
    const glass = templates[1].m;
    assert.equal(glass.dark['--bg'], '#0b0e1c');
    assert.equal(glass.light['--bg'], '#eef1fa');
    assert.equal(glass.common['--radius-scale'], '1.45');
    assert.ok(glass.dark['--glass-glint'], 'skin-effects tokens are exported for their style');

    const downloaded = page.waitForEvent('download');
    await page.locator('#themes-builtin .themes-row[data-key="verdant"] button[aria-label="导出为模板"]').click();
    const download = await downloaded;
    assert.equal(download.suggestedFilename(), 'my-verdant.agentbox-theme.json');
    const exported = JSON.parse(await readFile(await download.path(), 'utf8'));
    assert.equal(exported.agentbox_theme, 1);
    assert.equal(exported.name, '青野绿意（自定义）');
    assert.match(await statusText(), /已导出模板/);

    // Importing a personal theme applies it at once, adds it to the style menu, and survives a reload.
    const night = { agentbox_theme: 1, id: 'fixture-night', name: '夜航 <b>', base: 'glass', author: 'Fixture',
      common: { '--radius-scale': '0.5' }, dark: { '--bg': '#101418', '--accent': '#2563eb', '--amber': '#ffb38a' }, light: { '--bg': '#fbf7f2', '--accent': '#1d4ed8', '--amber': '#9a3412' } };
    await importFile('#themes-import-user', night);
    await page.locator('#themes-user .themes-row[data-key="user:fixture-night"].active').waitFor();
    assert.match(await statusText(), /已导入并启用「夜航 <b>」/);
    assert.equal(await page.locator('#themes-status').getAttribute('class'), 'themes-status ok');
    assert.equal(await page.locator('#themes-user .themes-row-name').innerText(), '夜航 <b>', 'names render as text');
    assert.equal(await page.locator('#themes-user .themes-row-meta').innerText(), '基于液态玻璃 · Fixture');
    assert.equal(await root('skin'), 'glass', 'the base style keeps its shapes and effects');
    assert.equal(await root('skinCustom'), 'user:fixture-night');
    const theme = await root('theme');
    assert.equal(await token('--bg'), night[theme]['--bg']);
    assert.equal(await token('--radius-scale'), '0.5');
    assert.deepEqual(await page.evaluate(`(${cornerLeaks})('#dlg-themes')`), [], 'personal active row stays inside the rounded list');
    assert.equal(await page.evaluate(() => localStorage.getItem('agentbox_skin')), 'user:fixture-night');
    assert.equal(await page.locator('meta[name="theme-color"]').getAttribute('content'), night[theme]['--bg']);
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });

    await page.locator('#btn-user-menu').click();
    const trigger = page.locator('#pref-skin .pref-trigger'), menu = page.locator('#pref-skin .pref-menu');
    assert.equal((await trigger.innerText()).trim(), '夜航 <b>');
    await trigger.click();
    assert.deepEqual((await menu.locator('.pref-menu-group').allInnerTexts()).map(t => t.trim()), ['我的主题']);
    assert.equal(await menu.locator('.pref-menu-item.active').getAttribute('data-value'), 'user:fixture-night');
    // Switching light/dark swaps the theme's own token set.
    await page.keyboard.press('Escape');
    await page.keyboard.press('Escape');
    const other = theme === 'dark' ? 'light' : 'dark', savedMode = await root('themeMode');
    await page.locator(`.theme-options [data-theme-option="${other}"]`).click();
    assert.equal(await token('--bg'), night[other]['--bg']);
    assert.equal(await token('--radius-scale'), '0.5', 'common tokens apply in both modes');

    // The head script restores the cached theme before any module runs.
    await page.route('**/js/main.js', route => route.abort());
    await page.reload();
    assert.equal(await root('skin'), 'glass');
    assert.equal(await root('skinCustom'), 'user:fixture-night');
    assert.equal(await page.evaluate(() => document.documentElement.style.getPropertyValue('--bg')), night[other]['--bg']);
    await page.unroute('**/js/main.js');
    await page.reload();
    await page.locator('#app').waitFor({ state: 'visible' });
    assert.equal(await token('--bg'), night[other]['--bg']);

    // Built-in styles drop every inline token again.
    await openManager();
    await page.locator('#themes-builtin .themes-row[data-key="graphite"] .themes-use').focus();
    await page.keyboard.press('Enter');
    assert.equal(await root('skin'), 'graphite');
    // Rows are rebuilt; keyboard focus stays in the same row instead of falling to the page.
    assert.equal(await page.evaluate(() => document.activeElement?.closest('.themes-row')?.dataset.key), 'graphite');
    assert.equal(await root('skinCustom'), undefined);
    assert.equal(await page.evaluate(() => document.documentElement.style.length), 0, 'custom tokens are removed');
    assert.equal(await page.evaluate(() => localStorage.getItem('agentbox_skin_custom')), null);

    // Low contrast is a warning; the theme is still saved and applied.
    await importFile('#themes-import-user', { agentbox_theme: 1, id: 'murky', name: 'Murky', base: 'amber', dark: { '--bg': '#222222', '--text': '#333333' }, light: { '--bg': '#eeeeee', '--text': '#dddddd' } });
    await page.locator('#themes-user .themes-row[data-key="user:murky"].active').waitFor();
    assert.equal(await page.locator('#themes-status').getAttribute('class'), 'themes-status warn');
    assert.match(await statusText(), /对比度偏低：深色 正文 \/ 背景 1\.\d:1（建议至少 4\.5:1）/);

    // Server validation errors are shown as-is in Simplified Chinese and translated by code elsewhere.
    const puts = state.puts;
    await importFile('#themes-import-user', { agentbox_theme: 1, id: 'evil', name: 'Evil', base: 'amber', dark: { '--bg': 'url(https://example.invalid/x)' } });
    await page.locator('#themes-status.error').waitFor();
    assert.equal(await statusText(), 'dark 里 --bg 的取值无效：不允许的函数 url()');
    await page.evaluate(async url => (await import(new URL('i18n.js', url).href)).i18n.setLanguage('en'), url);
    await importFile('#themes-import-user', { agentbox_theme: 1, id: 'evil', name: 'Evil', base: 'amber', dark: { '--bg': 'url(https://example.invalid/x)' } });
    await page.waitForFunction(() => document.querySelector('#themes-status').textContent === 'Invalid value for --bg in Dark');
    await page.evaluate(async url => (await import(new URL('i18n.js', url).href)).i18n.setLanguage('zh-CN'), url);
    assert.equal(state.puts, puts + 2);
    assert.equal(await root('skinCustom'), 'user:murky', 'a rejected import keeps the current theme');
    // Malformed files never reach the server.
    await importFile('#themes-import-user', '{not json');
    assert.equal(await statusText(), '主题文件不是有效的 JSON 对象');
    await importFile('#themes-import-user', { agentbox_theme: 1, id: '../x', name: 'X', base: 'amber' });
    assert.match(await statusText(), /^主题 ID 只能包含/);
    assert.equal(state.puts, puts + 2);

    // Re-importing an existing ID asks before replacing it.
    await importFile('#themes-import-user', { ...night, name: '夜航 2' });
    await page.locator('#dlg-ask').waitFor({ state: 'visible' });
    assert.match(await page.locator('#ask-text').innerText(), /「夜航 <b>」已存在/);
    await page.locator('#ask-cancel').click();
    assert.equal(state.user.find(v => v.manifest.id === 'fixture-night').manifest.name, '夜航 <b>', 'cancel keeps the old theme');

    // Shared themes: admins import and delete; ordinary users only choose.
    const harbor = { agentbox_theme: 1, id: 'harbor', name: 'Harbor', base: 'blueprint', dark: { '--bg': '#0f1b2d', '--accent': '#5aa9e6' } };
    await importFile('#themes-import-site', harbor);
    await page.locator('#themes-site .themes-row[data-key="site:harbor"].active').waitFor();
    assert.equal(await root('skin'), 'blueprint');
    assert.equal(await page.locator('#themes-site-count').innerText(), '1 / 50');
    await page.keyboard.press('Escape');
    state.admin = false;
    await openManager();
    await page.locator('#themes-import-site.hidden').waitFor({ state: 'attached' });
    assert.equal(await page.locator('#themes-site button[aria-label="删除"]').count(), 0, 'users cannot delete shared themes');
    assert.equal(await page.locator('#themes-site button[aria-label="导出"]').count(), 1);
    await page.keyboard.press('Escape');
    await page.locator('#btn-user-menu').click();
    await trigger.click();
    assert.deepEqual((await menu.locator('.pref-menu-group').allInnerTexts()).map(t => t.trim()), ['全站主题', '我的主题']);
    await page.keyboard.press('Escape');
    await page.keyboard.press('Escape');

    // A selected theme that disappears from the authoritative list falls back to amber.
    state.site = [];
    await page.reload();
    await page.locator('#app').waitFor({ state: 'visible' });
    await page.waitForFunction(() => document.documentElement.dataset.skin === 'amber' && !document.documentElement.dataset.skinCustom);
    assert.equal(await page.evaluate(() => localStorage.getItem('agentbox_skin')), 'amber');

    // Deleting the active personal theme also returns to amber.
    state.admin = true;
    await openManager();
    await page.locator('#themes-user .themes-row[data-key="user:fixture-night"] .themes-use').click();
    assert.equal(await root('skinCustom'), 'user:fixture-night');
    await page.locator('#themes-user .themes-row[data-key="user:fixture-night"] button[aria-label="删除"]').click();
    await page.locator('#dlg-ask').waitFor({ state: 'visible' });
    await page.locator('#ask-ok').click();
    await page.locator('#themes-user .themes-row[data-key="user:fixture-night"]').waitFor({ state: 'detached' });
    assert.equal(await root('skin'), 'amber');
    assert.equal(await root('skinCustom'), undefined);
    assert.match(await statusText(), /已删除「夜航 <b>」/);
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });
    await page.locator(`.theme-options [data-theme-option="${savedMode}"]`).click();
    console.log('Themes: built-in templates, personal/shared import-apply-export-delete, contrast and validation messages, first-paint restore and fallback passed');
  } finally {
    await page.unroute('**/api/themes**');
    await page.evaluate(() => { localStorage.removeItem('agentbox_skin'); localStorage.removeItem('agentbox_skin_custom'); }).catch(() => {});
  }
}
