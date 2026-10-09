#!/usr/bin/env node
// 100 synthetic workspaces in a real browser. No Docker/provider calls.
import {reduceMotion} from './playwright-launch.mjs';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { platform, arch, cpus } from 'node:os';

const root = fileURLToPath(new URL('../internal/web/static/', import.meta.url));
const server = createServer(async (req, res) => {
  try {
    const rel = new URL(req.url, 'http://fixture').pathname.replace(/^\/_v\/[^/]+/, '');
    const file = resolve(root, '.' + (rel === '/' ? '/index.html' : rel));
    if (!file.startsWith(root)) { res.writeHead(403).end(); return; }
    res.setHeader('Content-Type', ({ '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml' })[extname(file)] || 'application/octet-stream');
    res.end(await readFile(file));
  } catch { res.writeHead(404).end(); }
});
await new Promise(r => server.listen(0, '127.0.0.1', r));
const base = `http://127.0.0.1:${server.address().port}`;
const { chromium } = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
let browser;
const report = { device: { platform: platform(), arch: arch(), cpu: cpus()[0]?.model }, browser: '', scope: 'Synthetic API, 100 workspaces; local headless measurements, not agreed device acceptance', locales: [] };
try {
  browser = reduceMotion(await chromium.launch({ headless: true, ...(process.env.AGENTBOX_BROWSER_CHANNEL ? { channel: process.env.AGENTBOX_BROWSER_CHANNEL } : {}) }));
  report.browser = browser.version();
  await mkdir("output/playwright", { recursive: true });
  for (const locale of ['zh-CN', 'zh-TW', 'en']) {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    page.setDefaultTimeout(12000);
    const errors = [], writes = [];
    page.on('pageerror', e => errors.push(e.message));
    let identity = 'alice';
    const spaces = Array.from({ length: 100 }, (_, i) => ({
      id: `space-${i}`, name: `Project ${String(i).padStart(3, '0')}`, user: 'alice', agent: 'codex',
      account_id: 'fixture', account_label: 'Fixture', default_model: 'fixture',
      status: i % 3 === 0 ? 'running' : 'stopped', stop_reason: i % 3 === 2 ? '' : 'idle',
      updated_at: new Date(Date.UTC(2026, 9, 5, 0, i)).toISOString(),
    }));
    spaces[97].name = '项目 Alpha'; spaces[98].name = '<b>Literal</b>'; spaces[99].name = 'Ｐｒｏｊｅｃｔ Café';
    await context.addInitScript(locale => { if (window === window.top && /^https?:$/.test(location.protocol)) localStorage.setItem('agentbox.language', locale); }, locale);
    await context.routeWebSocket('**/api/sessions/*/chat?*', ws => {
      ws.send(JSON.stringify({ type: 'status', state: 'idle' }));
      ws.onMessage(raw => writes.push(String(raw)));
    });
    await context.route('**/api/**', async route => {
      const req = route.request(), path = new URL(req.url()).pathname;
      let body = {};
      if (req.method() !== 'GET' && !['/api/login', '/api/logout'].includes(path)) writes.push(path);
      if (path === '/api/login') body = { token: identity + '-token' };
      else if (path === '/api/me') body = { user: identity, role: 'user', timezone: 'UTC', models: { codex: [{ id: 'fixture', label: 'Fixture' }] }, quota: { metered: false } };
      else if (path === '/api/sessions') body = identity === 'alice' ? spaces : [];
      else if (path === '/api/accounts') body = [{ id: 'fixture', type: 'codex', label: 'Fixture', cred_status: 'ok' }];
      else if (path === '/api/onboarding') body = { version: 1, can_configure: false, can_create: true, has_workspaces: true, accounts: [{ id: 'fixture', type: 'codex', label: 'Fixture', credentials_present: true }], default_models: { codex: 'fixture' }, container_resources: { cpus: 1, memory_mb: 512, pids_limit: 128 } };
      else if (path.endsWith('/models')) body = { models: [{ id: 'fixture', label: 'Fixture' }], discovery: 'available' };
      else if (path.endsWith('/history')) body = { entries: [{ kind: 'user', text: 'Fixture task' }], thread: { id: 'one', title: 'Thread one', turns: 1 }, costs: {} };
      else if (path.endsWith('/chat/threads')) body = { threads: ['one', 'two'].map(id => ({ id, title: `Thread ${id}`, ts: new Date().toISOString(), turns: 1, resumable: true })), active: 'one' };
      else if (path.endsWith('/files') || path === '/api/git/connections') body = [];
      await route.fulfill({ json: body }).catch(() => {});
    });
    const search = page.locator('#session-search'), filter = page.locator('#session-filter');
    const trigger = page.locator('#session-filter + .select-trigger');
    const selectStatus = async value => {
      await trigger.click();
      const panel = page.locator('#' + await trigger.getAttribute('aria-controls'));
      await panel.getByRole('option').nth(['all', 'run', 'idle', 'off'].indexOf(value)).click();
    };
    const cards = page.locator('#session-list .session-card');
    const count = async n => page.waitForFunction(n => document.querySelectorAll('#session-list .session-card').length === n, n);
    const refresh = () => page.evaluate(async () => (await import('/_v/{{BUILD}}/js/data.js')).refreshAll());
    const login = async () => {
      await page.locator('#login-user').fill(identity); await page.locator('#login-pass').fill('fixture');
      await page.locator('#login-btn').click(); await page.locator('#app').waitFor({ state: 'visible' });
    };
    try {
      await page.goto(base); await login(); await count(100);
      const recent = await page.locator('#home-recent').innerText();
      for (const [state, n] of [['run', 34], ['idle', 33], ['off', 33], ['all', 100]]) {
        await selectStatus(state); await count(n);
      }
      await search.fill('  pRoJeCt 00  '); await count(10);
      await selectStatus('idle'); await count(3);
      assert.equal(await page.locator('#session-count').innerText(), '3/100');
      await selectStatus('all'); await search.fill('项目'); await count(1);
      await search.fill('project cafe\u0301'); await count(1); // Unicode width and canonical equivalence.
      await search.fill('<b>'); await count(1); assert.equal(await cards.locator('b').count(), 0);
      await search.fill('Fixture'); await count(0); // Names only, not the account label.
      assert.equal(await page.locator('#home-recent').innerText(), recent);
      await page.locator('#session-filter-clear').click(); await count(100); assert.equal(await search.inputValue(), '');
      assert.equal(await search.evaluate(el => el === document.activeElement), true);
      // Composition text remains editable; polling does not apply an unfinished IME query.
      await search.evaluate(el => { el.dispatchEvent(new CompositionEvent('compositionstart')); el.value = '项目'; el.dispatchEvent(new InputEvent('input', { bubbles: true, isComposing: true })); });
      await refresh(); await count(100);
      await search.evaluate(el => el.dispatchEvent(new CompositionEvent('compositionend'))); await count(1);
      await search.press('Escape'); await count(100);
      // Polling retains keyboard focus and scroll position; active workspace stays open when filtered out.
      await cards.last().focus();
      const scroll = await page.locator('#session-list').evaluate(el => el.scrollTop);
      await refresh();
      assert.equal(await page.evaluate(() => document.activeElement.dataset.sessionId), 'space-99');
      assert.equal(await page.locator('#session-list').evaluate(el => el.scrollTop), scroll);
      await search.fill('Project 042'); await cards.click();
      await page.waitForFunction(() => document.querySelector('#wb-name').textContent === 'Project 042');
      await page.locator('#chat-input').waitFor({ state: 'visible' });
      await search.fill('missing'); await count(0);
      assert.equal(await page.locator('#wb-name').innerText(), 'Project 042');
      await page.locator('#btn-threads').click(); await page.locator('#tp-search').fill('two');
      await page.waitForFunction(() => document.querySelectorAll('#tp-list .tp-open').length === 1 && document.querySelector('#tp-list .tp-title')?.textContent === 'Thread two');
      await page.keyboard.press('Escape');
      await search.fill('Project 042'); await selectStatus('run'); await count(1);
      await cards.focus();
      spaces[42].status = 'stopped'; spaces[42].stop_reason = 'idle'; await refresh(); await count(0);
      assert.equal(await search.evaluate(el => el === document.activeElement), true);
      await selectStatus('idle'); await count(1);
      spaces[42].name = 'Renamed project'; await refresh(); await count(0);
      await search.fill('renamed'); await count(1);
      spaces.splice(42, 1); await refresh(); await count(0);
      await page.locator('#session-filter-clear').click(); await count(99);
      // Collapsed sidebar always offers an explicit way back to the search controls.
      await page.locator('#btn-sidebar-toggle').click();
      assert.equal(await search.isVisible(), false);
      await page.locator('#session-search-open').click();
      assert.equal(await search.evaluate(el => el === document.activeElement), true);
      // Browser event -> DOM + two frames; informational measurement, no invented acceptance threshold.
      spaces.splice(42, 0, { ...spaces[0], id: 'space-42', name: 'Project 042' }); await refresh(); await count(100);
      const timings = await page.evaluate(async () => {
        const search = document.querySelector('#session-search'), filter = document.querySelector('#session-filter');
        const measure = async action => { const start = performance.now(); action(); await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))); return performance.now() - start; };
        const searchMS = [], filterMS = [];
        for (let i = 0; i < 100; i++) {
          searchMS.push(await measure(() => { search.value = i % 2 ? '' : 'Project 00'; search.dispatchEvent(new Event('input', { bubbles: true })); }));
          filterMS.push(await measure(() => { filter.value = i % 2 ? 'all' : 'run'; filter.dispatchEvent(new Event('change', { bubbles: true })); }));
        }
        return { searchMS, filterMS };
      });
      const switchMS = [];
      for (const id of Array.from({ length: 30 }, (_, i) => ['space-0', 'space-99', 'space-1', 'space-98', 'space-42'][i % 5])) {
        const start = performance.now();
        await page.locator(`.session-card[data-session-id="${id}"]`).click();
        await page.waitForFunction(id => location.hash.includes(`/sessions/${id}/chat`) && document.querySelector('#chat-input') && !document.querySelector('#chat-input').disabled, id);
        switchMS.push(performance.now() - start);
      }
      await page.screenshot({ path: `output/playwright/workspace-filter-${locale}-desktop.png` });
      // Narrow drawer focus trap includes the new input/select; first Escape clears, second closes.
      await page.setViewportSize({ width: 390, height: 844 }); await page.locator('#btn-menu').click();
      await search.focus();
      await search.evaluate(el => el.dispatchEvent(new CompositionEvent('compositionstart')));
      await search.press('Escape'); assert.equal(await page.locator('#sidebar').evaluate(el => el.classList.contains('open')), true);
      await search.evaluate(el => el.dispatchEvent(new CompositionEvent('compositionend')));
      await page.keyboard.press('Tab'); assert.equal(await trigger.evaluate(el => el === document.activeElement), true);
      await search.fill('Project 00'); await search.press('Escape'); assert.equal(await search.inputValue(), '');
      assert.equal(await page.locator('#sidebar').evaluate(el => el.classList.contains('open')), true);
      await search.press('Escape'); assert.equal(await page.locator('#sidebar').evaluate(el => el.classList.contains('open')), false);
      await page.locator('#btn-menu').click(); await search.fill('Project 00'); await selectStatus('idle'); await count(3);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await page.screenshot({ path: `output/playwright/workspace-filter-${locale}-mobile.png` });
      await cards.first().click(); assert.equal(await page.locator('#sidebar').evaluate(el => el.classList.contains('open')), false);
      // Live language switch keeps the search and selected enum; user names stay literal.
      await page.locator('#btn-menu').click();
      await page.evaluate(async () => (await import('/_v/{{BUILD}}/js/i18n.js')).i18n.setLanguage('en'));
      assert.equal(await search.inputValue(), 'Project 00'); assert.equal(await filter.inputValue(), 'idle');
      assert.equal(await search.getAttribute('aria-label'), 'Search workspace names');
      await page.locator('#btn-user-menu').click(); await page.locator('#btn-logout').click();
      await page.locator('#login').waitFor({ state: 'visible' });
      assert.equal(await search.inputValue(), ''); assert.equal(await filter.inputValue(), 'all');
      assert.equal(await cards.count(), 0);
      identity = 'bob'; await login(); await page.locator('#btn-menu').click(); await count(0);
      assert.equal(await search.inputValue(), ''); assert.equal(await filter.inputValue(), 'all');
      assert.deepEqual(writes, []); assert.deepEqual(errors, []);
      report.locales.push({ locale, workspaces: 100, samples: 100, switchSamples: 30, switchMS, ...timings });
      console.log(`${locale}: search/status/IME/polling/recent/thread/collapse/mobile/logout passed`);
    } catch (error) { console.error({ errors }); throw error; } finally { await context.close(); }
  }
  await writeFile('output/playwright/workspace-filter-performance.json', JSON.stringify(report, null, 2) + '\n');
  console.log(JSON.stringify(report));
} finally { await browser?.close(); await new Promise(r => server.close(r)); }
