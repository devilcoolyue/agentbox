#!/usr/bin/env node
// Standalone abox-link UI fixture. No real pairing, credentials, or tunnel.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import en from '../internal/linkapp/static/locales/en.js';
import zhTW from '../internal/linkapp/static/locales/zh-TW.js';

export async function linkI18nSmoke(page) {
  page.setDefaultTimeout(10000);
  const root = fileURLToPath(new URL('../internal/linkapp/static/', import.meta.url));
  const failures = [];
  const server = createServer(async (req, res) => {
    try {
      const url = new URL(req.url, 'http://127.0.0.1');
      const path = resolve(root, '.' + (url.pathname === '/' ? '/index.html' : url.pathname));
      if (!path.startsWith(root)) { res.writeHead(403).end(); return; }
      const data = await readFile(path);
      res.setHeader('Content-Type', ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[extname(path)] || 'application/octet-stream');
      res.end(data);
    } catch { res.writeHead(404).end(); }
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  const base = `http://127.0.0.1:${server.address().port}`;
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  let state = {
    paired: false, running: false, user: '保存', server: 'https://example.invalid',
    allow: ['db.example:5432'], maps: ['3306=10.0.1.5:3306'], auto_connect: true,
    transparent: true, insecure: false, autostart: { supported: true, installed: false, detail: '已保存' },
    status: { state: 'stopped' },
  };
  let mutations = 0;
  let configPayload = null;
  let refuseSave = false;
  const diagnostic = '已保存 <script>window.untrusted = true</script>';
  await page.route('**/api/**', async route => {
    try {
      const req = route.request();
      assert.equal(req.headers()['x-abox-panel'], '1');
      const url = new URL(req.url());
      if (req.method() !== 'GET') mutations++;
      if (url.pathname === '/api/logs') {
        await route.fulfill({ json: { latest: 1, lines: Number(url.searchParams.get('since')) >= 1 ? [] : [{ seq: 1, time: Date.now(), text: diagnostic }] } });
        return;
      }
      if (url.pathname === '/api/pair') {
        assert.deepEqual(req.postDataJSON(), { code: 'ABOX1-fixture-保存', insecure: true });
        state = { ...state, paired: true, running: true, status: { state: 'online', transparent: true, since: Date.now() - 75000, ping_ms: 18, maps: [{ port: 3306, ok: false, detail: '保存' }] } };
      } else if (url.pathname === '/api/config') {
        if (refuseSave) { await route.fulfill({ status: 400, json: { error: diagnostic } }); return; }
        configPayload = req.postDataJSON();
        state = { ...state, ...configPayload };
      } else assert.equal(url.pathname, '/api/state');
      await route.fulfill({ json: state });
    } catch (error) { failures.push(error.message); await route.abort(); }
  });
  try {
    await page.goto(base);
    await page.locator('#card-pair').waitFor({ state: 'visible' });
    const language = page.locator('#interface-language');
    await language.selectOption('zh-CN'); // Stable fixture baseline, independent of the developer's OS.
    assert.equal(await page.locator('html').getAttribute('lang'), 'zh-CN');
    assert.equal((await page.locator('#btn-pair').textContent()).trim(), '接入');
    await page.locator('#btn-pair').click();
    assert.equal(await page.locator('#pair-err').textContent(), '请先粘贴配对码');
    await language.selectOption('en');
    assert.equal(await page.locator('#pair-err').textContent(), 'Paste a pairing code first');
    assert.equal(await page.title(), 'abox-link Private network tunnel');
    assert.equal(await language.getAttribute('aria-label'), 'Interface language');
    await page.locator('#pair-code').fill('ABOX1-fixture-保存');
    await page.locator('#pair-insecure').check();
    await language.selectOption('zh-TW');
    assert.equal(await page.locator('#pair-code').inputValue(), 'ABOX1-fixture-保存');
    assert.equal(await page.locator('#pair-insecure').isChecked(), true);
    assert.equal((await page.locator('#btn-pair').textContent()).trim(), '連線');
    await page.locator('#btn-pair').click();
    await page.locator('#card-status').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#st-title').textContent(), '隧道已連線');
    assert.equal(await page.locator('#hdr-acct').textContent(), '保存 @ example.invalid');
    assert.equal(await page.locator('#boot-detail').textContent(), '已保存');
    assert.equal(await page.locator('#log script').count(), 0);
    assert.match(await page.locator('#log').textContent(), /已保存 <script>/);
    await language.selectOption('en');
    assert.equal(await page.locator('#st-title').textContent(), 'Tunnel connected');
    assert.match(await page.locator('#st-sub').textContent(), /Port 3306 \(failed: 保存\)/);
    assert.equal(await page.locator('#allow-list input').getAttribute('aria-label'), 'Allowed target');
    assert.equal(await page.locator('#allow-list button').getAttribute('title'), 'Remove');

    await page.locator('#allow-list input').fill('保存.example:5432');
    await page.locator('#opt-auto').uncheck();
    await page.locator('#btn-add-map').click();
    await page.locator('#map-list .rule').last().locator('input').first().fill('8080');
    await page.locator('#map-list .rule').last().locator('input').last().fill('保存:8080');
    await page.evaluate(() => {
      window.ruleBeforeLanguage = document.querySelector('#allow-list input');
      window.ruleBeforeLanguage.focus();
      window.ruleBeforeLanguage.setSelectionRange(1, 4);
      const select = document.querySelector('#interface-language');
      select.value = 'zh-TW';
      select.dispatchEvent(new Event('change'));
    });
    assert.deepEqual(await page.evaluate(() => {
      const input = document.querySelector('#allow-list input');
      return { same: input === window.ruleBeforeLanguage, focused: document.activeElement === input, start: input.selectionStart, end: input.selectionEnd, value: input.value };
    }), { same: true, focused: true, start: 1, end: 4, value: '保存.example:5432' });
    assert.equal(await page.locator('#savebar').isVisible(), true);
    assert.equal(await page.locator('#opt-auto').isChecked(), false);
    assert.equal(mutations, 1, 'language changes must not write configuration or restart the tunnel');
    await page.waitForTimeout(2200); // One real panel polling interval must preserve unsaved input.
    assert.equal(await page.locator('#allow-list input').inputValue(), '保存.example:5432');
    assert.equal(await page.locator('#map-list .rule').count(), 2);
    await language.selectOption('en');
    await page.locator('#btn-save').click();
    await page.locator('#savebar').waitFor({ state: 'hidden' });
    assert.deepEqual(configPayload, { allow: ['保存.example:5432'], maps: ['3306=10.0.1.5:3306', '8080=保存:8080'], auto_connect: false, transparent: true, insecure: false });
    assert.equal(await page.locator('#toast').textContent(), 'Saved. Reconnecting with the updated rules.');
    await language.selectOption('zh-TW');
    assert.equal(await page.locator('#toast').textContent(), '已儲存，隧道正以新規則重新連線');

    refuseSave = true;
    await page.locator('#allow-list input').fill('new-target.example');
    await page.locator('#btn-save').click();
    await page.locator('#toast.bad').waitFor();
    await language.selectOption('en');
    assert.equal(await page.locator('#toast').textContent(), diagnostic, 'raw backend diagnostics must remain unchanged');
    assert.equal(await page.locator('#toast script').count(), 0);
    await page.locator('#btn-revert').click();
    assert.equal(await page.locator('#allow-list input').inputValue(), '保存.example:5432');

    const dialogText = new Promise(resolve => page.once('dialog', async dialog => { resolve(dialog.message()); await dialog.dismiss(); }));
    await page.locator('#btn-unpair').click();
    assert.equal(await dialogText, en['解除绑定后需要重新配对才能连接，放行规则会保留。继续？']);
    assert.equal(mutations, 3, 'canceling an unpair confirmation must not send a request');
    await page.reload();
    await page.locator('#card-status').waitFor({ state: 'visible' });
    assert.equal(await language.inputValue(), 'en');
    assert.equal(await page.locator('html').getAttribute('lang'), 'en');
    assert.equal(await page.locator('#st-title').textContent(), 'Tunnel connected');
    for (const width of [1280, 390, 320]) {
      await page.setViewportSize({ width, height: 900 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `English layout overflow at ${width}px`);
    }
    await page.setViewportSize({ width: 390, height: 900 });
    await mkdir(resolve('output/playwright'), { recursive: true });
    await page.screenshot({ path: resolve('output/playwright/link-i18n-english-mobile.png'), fullPage: true });

    // Explicit markers have complete dictionaries, including attributes generated by rules.
    const sources = await page.evaluate(async () => {
      // Parse a fresh, inert copy to inspect the authored sources, not the
      // currently translated UI or any user-supplied content.
      const original = new DOMParser().parseFromString(await (await fetch('/')).text(), 'text/html');
      return [
        ...original.querySelectorAll('[data-i18n]'),
      ].map(el => el.textContent.trim()).concat(
        [...document.querySelectorAll('[data-i18n-aria-label], [data-i18n-title], [data-i18n-data-empty]')]
          .flatMap(el => [...el.attributes].filter(attr => attr.name.startsWith('data-i18n-')).map(attr => attr.value)),
      );
    });
    for (const source of sources) {
      assert.ok(Object.hasOwn(en, source), `missing English marker: ${source}`);
      assert.ok(Object.hasOwn(zhTW, source), `missing Traditional Chinese marker: ${source}`);
    }
    for (const [source, translation] of Object.entries(en)) {
      assert.ok(Object.hasOwn(zhTW, source));
      const placeholders = text => [...text.matchAll(/\{([a-zA-Z][\w]*)\}/g)].map(match => match[1]).sort();
      assert.deepEqual(placeholders(translation), placeholders(source), `English placeholders for ${source}`);
      assert.deepEqual(placeholders(zhTW[source]), placeholders(source), `Traditional Chinese placeholders for ${source}`);
    }
    assert.deepEqual(failures, []);
    assert.deepEqual(errors, []);
    console.log('abox-link i18n: pairing, 3 languages, persistence, live status, draft DOM/cursor preservation, unchanged targets/logs/errors, safe text interpolation and narrow English layouts passed');
  } finally {
    await page.unroute('**/api/**');
    server.closeAllConnections();
    await new Promise(r => server.close(r));
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const { chromium } = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
  const browser = await chromium.launch({ headless: true, ...(process.env.AGENTBOX_BROWSER_CHANNEL ? { channel: process.env.AGENTBOX_BROWSER_CHANNEL } : {}) });
  try { await linkI18nSmoke(await browser.newPage()); } finally { await browser.close(); }
}
