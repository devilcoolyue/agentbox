#!/usr/bin/env node
// Real server/browser reliability matrix. Only the Docker/CLI boundary is synthetic.
import {reduceMotion} from './playwright-launch.mjs';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { harness, until, sleep } from './chat-integration-harness.mjs';

const h = await harness(); let browser, activePage;
const errors = [], failedRequests = [];
try {
 const { chromium } = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
 browser = reduceMotion(await chromium.launch({ headless: true, ...(process.env.AGENTBOX_BROWSER_CHANNEL ? { channel: process.env.AGENTBOX_BROWSER_CHANNEL } : {}) }));
 h.report.browser = browser.version();
 const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
 const page = await context.newPage(); activePage = page; page.setDefaultTimeout(15000);
 const puts = [], sockets = new Set(); let blockReads = false, dropAck = false, putFault = '', releaseResponse;
 context.on('requestfailed', request => {
  if (failedRequests.length === 100) failedRequests.shift();
  failedRequests.push({ path: new URL(request.url()).pathname, error: request.failure()?.errorText });
 });
 context.on('page', p => p.on('pageerror', e => errors.push(e.message))); page.on('pageerror', e => errors.push(e.message));
 await context.addInitScript(() => { if (window === window.top && /^https?:$/.test(location.protocol)) localStorage.setItem('agentbox.language', 'zh-CN'); });
 h.report.static_transport = { mode: 'real-server-http-via-playwright-request', responses: 0, failures: [] };
 await context.route('**/*', async route => {
  const req = route.request(), url = new URL(req.url());
  if (url.origin !== h.base) { await route.abort(); return; }
  // Docker bridge/veth address changes on Linux can make Chromium abort the
  // module graph with ERR_NETWORK_CHANGED even though loopback stays reachable.
  // Load immutable assets through Playwright's Node HTTP transport, preserving
  // the real embedded server response. No cache, synthetic body or retry; API,
  // document and WebSocket traffic still uses the browser's network stack.
  if (req.method() === 'GET' && /^\/_v\/[a-f0-9]{12}\//.test(url.pathname)) {
   let response;
   try {
    response = await route.fetch({ maxRedirects: 0, maxRetries: 0, timeout: 10000 });
    assert.equal(response.status(), 200, 'embedded asset ' + url.pathname);
    await route.fulfill({ response });
    h.report.static_transport.responses++;
   } catch (error) {
    h.report.static_transport.failures.push({ path: url.pathname, error: error.message });
    await route.abort('failed').catch(() => {});
   } finally { await response?.dispose(); }
   return;
  }
  if (/\/chat\/requests(?:\/|$)/.test(url.pathname)) {
   if (req.method() === 'GET' && blockReads && url.pathname.startsWith('/api/sessions/space-a/')) { await route.abort('internetdisconnected'); return; }
   if (req.method() === 'PUT') {
    puts.push({ id: url.pathname.split('/').at(-1), session: url.pathname.split('/')[3], input: req.postDataJSON() });
    if (putFault === 'offline-before') { await context.setOffline(true); await route.abort('internetdisconnected'); return; }
    if (putFault === 'before') { await route.abort('failed'); return; }
    if (putFault === 'after' || putFault === 'hold') {
     const hold = putFault === 'hold'; await route.fetch();
     if (hold) await new Promise(resolve => { releaseResponse = resolve; });
     await route.abort('failed').catch(() => {}); return;
    }
   }
  }
  await route.continue();
 });
 await context.routeWebSocket('**/api/sessions/*/chat?*', socket => {
  sockets.add(socket); socket.onClose(() => sockets.delete(socket));
  const upstream = socket.connectToServer();
  upstream.onMessage(data => { if (dropAck && JSON.parse(String(data)).type === 'chat_request') return; socket.send(data); });
 });
 const input = page.locator('#chat-input');
 const ready = async (p = page) => { await p.locator('#chat-input').waitFor({ state: 'visible' }); await p.waitForFunction(() => !document.querySelector('#chat-input').disabled); };
 const go = async (id, p = page) => { await p.evaluate(id => location.hash = `#/sessions/${id}/chat`, id); await p.waitForFunction(id => document.querySelector('#wb-name').textContent === id, id); await ready(p); };
 const login = async (name = 'alice', p = page) => { await p.locator('#login-user').fill(name); await p.locator('#login-pass').fill('synthetic-password'); await p.locator('#login-btn').click(); await p.locator('#app').waitFor({ state: 'visible' }); };
 const token = () => page.evaluate(() => localStorage.getItem('agentbox_token'));
 const api = async (path, body, method = body === undefined ? 'GET' : 'POST', bearer) => {
  const res = await fetch(h.base + '/api' + path, { method, headers: { Authorization: 'Bearer ' + (bearer || await token()), 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(10000) });
  return { status: res.status, body: await res.json() };
 };
 const receipt = async id => (await api('/sessions/space-a/chat/requests/' + id)).body.receipt;
 const state = (id, value, p = page) => p.locator(`.delivery-item[data-request-id="${id}"][data-state="${value}"]`).waitFor({state:'attached'});
 const records = () => page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('agentbox.chat-outbox.v1.')).map(k => JSON.parse(localStorage.getItem(k))));
 const action = async (id, label, p = page) => { if (await p.locator('#chat-delivery-toggle').getAttribute('aria-expanded') !== 'true') await p.locator('#chat-delivery-toggle').click(); const row = p.locator(`.delivery-item[data-request-id="${id}"]`); if (!await row.evaluate(el => el.open)) await row.locator('summary').click(); await row.getByRole('button', { name: label, exact: true }).click(); };
 const confirm = async () => { await page.locator('#dlg-ask').waitFor({ state: 'visible' }); await page.locator('#ask-ok').click(); };
 const turns = async () => (await h.ctl('state')).turns || [];
 const started = prompt => until(async () => (await turns()).find(t => t.prompt === prompt), 'runner did not start: ' + prompt);
 const completedOutput = prompt => JSON.stringify({ type: 'thread.started', thread_id: 'synthetic-provider-thread' }) + '\n' + JSON.stringify({ type: 'item.completed', item: { id: 'synthetic-answer', type: 'agent_message', text: 'Synthetic answer: ' + prompt } }) + '\n' + JSON.stringify({ type: 'turn.completed', usage: { input_tokens: 150, output_tokens: 1 } }) + '\n';
 const finish = async prompt => { await started(prompt); await h.ctl('finish', { Prompt: prompt, Output: completedOutput(prompt) }); };
 async function accounting(id, expectedCost = 150) {
  const c = await receipt(id), a = await h.ctl('audit');
  const usage = a.usage.filter(u => u.turn_id === c.turn_id); assert.equal(usage.length, expectedCost ? 1 : 0, 'usage count per turn');
  if (expectedCost) { assert.equal(usage[0].cost_micro_usd, expectedCost); const rows = a.ledger.filter(l => l.ref === 'usage:' + usage[0].id); assert.equal(rows.length, 1); assert.equal(rows[0].delta_micro_usd, -expectedCost); }
  for (const q of a.quotas) assert.equal(q.balance_micro_usd, a.ledger.filter(l => l.user === q.user).reduce((sum, l) => sum + l.delta_micro_usd, 0), 'quota and ledger diverged');
  assert.equal(new Set(a.usage.map(u => u.turn_id)).size, a.usage.length, 'duplicate charge turn');
  return a;
 }
 async function send(prompt, double = false) {
  const before = puts.length; await input.fill(prompt);
  if (double) await page.locator('#chat-send').dblclick(); else await page.locator('#chat-send').click();
  return until(() => puts.slice(before).find(p => p.input.text === prompt), 'browser did not PUT frozen input');
 }
 function passed(name) { h.report.checks.push(name); console.log('PASS ' + name); }
 await page.goto(h.base); await login(); await go('space-a');
 const initialScope = (await api('/me')).body.chat_scope;
 dropAck = true; blockReads = true; putFault = 'after';
 const lost = await send('lost acknowledgement and double click', true); await started(lost.input.text); await state(lost.id, 'unconfirmed');
 assert.equal((await turns()).length, 1); assert.equal((await receipt(lost.id)).state, 'running');
 assert.equal((await records()).filter(r => r.id === lost.id).length, 1);
 await input.fill('unsent draft survives reload'); const putCount = puts.length;
 await page.reload(); await ready(); await state(lost.id, 'unconfirmed'); assert.equal(await input.inputValue(), 'unsent draft survives reload'); assert.equal(puts.length, putCount);
 dropAck = false; blockReads = false; putFault = ''; await page.locator('#chat-delivery-refresh').click(); await state(lost.id, 'running');
 const peer = await context.newPage(); peer.setDefaultTimeout(15000); await peer.goto(h.base + '/#/sessions/space-a/chat'); await ready(peer); await state(lost.id, 'running', peer);
 const replays = await Promise.all(Array.from({ length: 12 }, (_, i) => (i % 2 ? peer : page).evaluate(async args => {
  const res = await fetch(args.path, { method: 'PUT', headers: { Authorization: 'Bearer ' + localStorage.getItem('agentbox_token'), 'Content-Type': 'application/json' }, body: JSON.stringify(args.input) }); return { status: res.status, body: await res.json() };
 }, { path: '/api/sessions/space-a/chat/requests/' + lost.id, input: lost.input })));
 assert.ok(replays.every(r => r.status === 200 && r.body.receipt.request_id === lost.id)); assert.equal((await turns()).length, 1);
 await finish(lost.input.text); await state(lost.id, 'completed'); await state(lost.id, 'completed', peer); await accounting(lost.id); await peer.close();
 passed('lost HTTP/WS acknowledgement, double click, refresh, 12 same-ID PUTs from two tabs execute/charge once');
 // Cut the network at the outgoing PUT boundary, before it reaches the server.
 // Going offline before the click races the read-only poll and can correctly block sending.
 putFault = 'offline-before'; await send('offline before transmission');
 const offline = await until(async () => (await records()).find(r => r.draft?.text === 'offline before transmission'), 'offline copy missing');
 putFault = ''; await context.setOffline(false); await page.locator('#chat-delivery-refresh').click(); await state(offline.id, 'unconfirmed'); assert.equal((await turns()).length, 1);
 await action(offline.id, '重试原消息'); await started('offline before transmission'); await finish('offline before transmission'); await state(offline.id, 'completed'); await accounting(offline.id);
 passed('offline before transmission, explicit retry keeps the original ID');
 const crash = await send('crash after runner start'); await started(crash.input.text); await state(crash.id, 'running');
 await input.fill('draft during server restart'); const beforeCrash = (await turns()).length;
 await h.stop(true); await h.start(); assert.equal((await api('/me')).body.chat_scope, initialScope);
 await page.reload(); await ready(); await state(crash.id, 'uncertain'); assert.equal(await input.inputValue(), 'draft during server restart');
 const completedReplay = await api('/sessions/space-a/chat/requests/' + lost.id, lost.input, 'PUT');
 assert.equal(completedReplay.status, 200); assert.equal(completedReplay.body.receipt.state, 'completed'); await accounting(lost.id);
 assert.equal((await api('/sessions/space-a/chat/requests/' + lost.id, { ...lost.input, text: 'changed frozen input' }, 'PUT')).status, 409);
 assert.equal((await turns()).length, beforeCrash); assert.equal((await receipt(crash.id)).error_code, 'server_restarted'); await accounting(crash.id, 0);
 assert.equal(await page.locator('#chat-send').isDisabled(), true);
 const rejectedLegacy = await page.evaluate(() => new Promise((resolve, reject) => {
  const ws = new WebSocket(location.origin.replace('http', 'ws') + '/api/sessions/space-a/chat?token=' + encodeURIComponent(localStorage.getItem('agentbox_token')));
  const timer = setTimeout(() => { ws.close(); reject(Error('legacy gate did not reply')); }, 10000);
  ws.onopen = () => ws.send(JSON.stringify({ type: 'user_message', text: 'legacy must not bypass uncertain', model: 'gpt-5.5' }));
  ws.onmessage = event => { const message = JSON.parse(event.data); if (message.type === 'error') { clearTimeout(timer); ws.close(); resolve(message); } };
 }));
 assert.equal(rejectedLegacy.code, 'chat_pending');
 assert.equal((await api('/sessions/space-a/chat/threads/thread-two/activate', {})).status, 409);
 assert.equal((await api('/sessions/space-a/chat/requests/' + crash.id, crash.input, 'PUT')).status, 200);
 await action(crash.id, '已检查结果，继续'); await confirm(); await state(crash.id, 'reviewed'); assert.equal((await turns()).length, beforeCrash);
 const after = await send('explicit new task after review'); await finish(after.input.text); await state(after.id, 'completed'); await accounting(after.id);
 passed('SIGKILL real server, same-volume restart, stable scope, uncertain receipt, no rerun/charge; review and explicit new task');
 // Disconnect after acceptance: the server completes and settles while the browser is offline.
 const disconnected = await send('offline after acceptance'); await started(disconnected.input.text); await state(disconnected.id, 'running');
 const disconnectCount = puts.length; await context.setOffline(true); for (const socket of sockets) socket.close();
 await input.fill('draft edited while offline'); await finish(disconnected.input.text);
 await until(async () => (await receipt(disconnected.id)).state === 'completed', 'server did not finish offline turn');
 await context.setOffline(false); await page.reload(); await ready(); await page.locator('#chat-delivery-refresh').click();
 assert.equal(await input.inputValue(), 'draft edited while offline'); assert.equal(puts.length, disconnectCount); await accounting(disconnected.id);
 passed('disconnect after acceptance, server completion/settlement while offline, reconnect only queries');
 // SIGTERM is not a user-confirmed interruption, even if the synthetic CLI responds to SIGINT.
 const shutdown = await send('graceful shutdown with active turn'); await started(shutdown.input.text); await state(shutdown.id, 'running');
 const beforeShutdown = (await turns()).length; await h.stop(); await h.start(); await page.reload(); await ready(); await state(shutdown.id, 'uncertain');
 assert.equal((await turns()).length, beforeShutdown); await accounting(shutdown.id, 0);
 await action(shutdown.id, '已检查结果，继续'); await confirm(); await state(shutdown.id, 'reviewed');
 passed('SIGTERM active turn stays uncertain rather than successful or user-interrupted');
 const interrupted = await send('user interrupt'); await started(interrupted.input.text); await state(interrupted.id, 'running');
 const signals = (await h.ctl('state')).interruptions; await action(interrupted.id, '中断'); await state(interrupted.id, 'interrupted');
 assert.equal((await h.ctl('state')).interruptions, signals + 1); await accounting(interrupted.id, 0);
 await input.fill(''); await action(interrupted.id, '复制回输入框'); assert.equal(await input.inputValue(), interrupted.input.text);
 passed('explicit interruption, exactly one signal, interrupted receipt and recoverable input');
 // Real SQLite transaction failure must not expose a completed receipt or partial debit.
 await h.ctl('settlement-fault', { Enabled: true });
 const settlement = await send('settlement transaction failure'); await finish(settlement.input.text); await state(settlement.id, 'uncertain');
 assert.equal((await receipt(settlement.id)).error_code, 'usage_write_failed'); await accounting(settlement.id, 0);
 await h.ctl('settlement-fault', { Enabled: false }); const beforeReview = (await turns()).length;
 await action(settlement.id, '已检查结果，继续'); await confirm(); await state(settlement.id, 'reviewed');
 assert.equal((await turns()).length, beforeReview); await accounting(settlement.id, 0);
 passed('SQLite settlement failure rolls back usage/debit and stays uncertain; review never retries or charges');
 // An unreceived message remains in its original thread; switching cannot change its envelope.
 putFault = 'before'; const transfer = await send('pending belongs to original thread'); await state(transfer.id, 'unconfirmed');
 const beforeThread = (await turns()).length;
 await page.locator('#btn-threads').click(); await page.locator('#tp-list .tp-open').filter({ hasText: 'thread-two' }).click();
 await page.waitForFunction(() => document.querySelector('#thread-title').textContent === 'thread-two');
 await ready(); await state(transfer.id, 'unconfirmed');
 assert.equal(await page.locator(`.delivery-item[data-request-id="${transfer.id}"]`).getByRole('button', { name: '重试原消息', exact: true, includeHidden: true }).isDisabled(), true);
 putFault = ''; await action(transfer.id, '放弃未接收消息'); await confirm(); await state(transfer.id, 'abandoned');
 await input.fill(''); await action(transfer.id, '复制回输入框'); await confirm(); await page.waitForFunction(text => document.querySelector('#chat-input').value === text, transfer.input.text);
 assert.equal((await turns()).length, beforeThread);
 assert.equal((await api('/sessions/space-a/chat/requests/' + transfer.id, transfer.input, 'PUT')).status, 410);
 await action(transfer.id, '关闭状态'); await input.fill('');
 passed('thread switch, explicit cross-thread copy, abandon tombstone permanently fences original ID');
 // Hold the real response until the user has moved to another workspace.
 putFault = 'hold'; dropAck = true; blockReads = true;
 const held = await send('held acknowledgement from old workspace'); await started(held.input.text); await until(() => releaseResponse, 'real response not held');
 await go('space-b'); await input.fill('space B private draft'); putFault = ''; releaseResponse(); releaseResponse = undefined; await sleep(100);
 assert.equal(await input.inputValue(), 'space B private draft'); assert.equal(await page.locator('#chat-delivery').isVisible(), false);
 dropAck = false; blockReads = false; await go('space-a'); await state(held.id, 'running'); await finish(held.input.text); await state(held.id, 'completed'); await accounting(held.id);
 await go('space-b'); assert.equal(await input.inputValue(), 'space B private draft'); await go('space-a');
 passed('late real HTTP failure cannot modify the new workspace composer; original receipt remains queryable');
 // Uploaded references are validated by the real filesystem API on draft recovery.
 const uploadResponse = page.waitForResponse(r => new URL(r.url()).pathname === '/api/sessions/space-a/images' && r.request().method() === 'POST');
 await page.locator('#attach-input').setInputFiles({ name: 'fixture.txt', mimeType: 'text/plain', buffer: Buffer.from('synthetic attachment, no user content') });
 const upload = await uploadResponse, uploaded = await upload.json();
 if (h.report.mode !== 'Linux containers' && process.getuid?.() !== 0) {
  assert.equal(upload.status(), 500); assert.match(uploaded.error, /operation not permitted/);
  await page.waitForFunction(() => !document.querySelector('#chat-attach').textContent.includes('fixture.txt'));
  h.report.not_applicable = ['Successful attachment upload needs UID 1000 chown; covered by docker.chat-reliability, not this unprivileged native run'];
  passed('unprivileged native attachment upload reports actual chown denial (successful upload requires Linux fixture)');
 } else {
 assert.equal(upload.status(), 200); assert.ok(uploaded.path.startsWith('/shared/.file/'));
 await input.fill('attachment draft [File #1]'); await page.reload(); await ready();
 await page.waitForFunction(() => !document.querySelector('#chat-send').disabled); assert.equal(await page.locator('.attachment-invalid').count(), 0);
 const beforeAttachment = (await turns()).length;
 assert.equal((await api('/sessions/space-a/files?scope=shared&path=' + encodeURIComponent(uploaded.path.slice('/shared/'.length)), undefined, 'DELETE')).status, 200);
 await page.reload(); await ready(); await page.locator('.attachment-invalid').waitFor();
 assert.equal(await page.locator('#chat-send').isDisabled(), true); assert.ok((await input.inputValue()).includes('attachment draft'));
 assert.equal((await turns()).length, beforeAttachment); await page.locator('#chat-attach button').click(); await input.fill('');
 passed('real attachment upload, valid draft reload, deletion and invalid-reference recovery blocks sending');
 }
 // Real logout invalidates the token and peer tabs; Bob cannot query Alice's pending turn.
 const logoutTurn = await send('Alice task survives logout'); await started(logoutTurn.input.text); await state(logoutTurn.id, 'running');
 const oldToken = await token(); const peerLogout = await context.newPage(); await peerLogout.goto(h.base + '/#/sessions/space-a/chat'); await ready(peerLogout);
 await page.bringToFront(); await page.locator('#btn-user-menu').click(); await page.locator('#btn-logout').click();
 await page.locator('#login').waitFor({ state: 'visible' }); await peerLogout.locator('#login').waitFor({ state: 'visible' });
 assert.equal((await records()).length, 0); assert.equal((await api('/me', undefined, 'GET', oldToken)).status, 401);
 await login('bob'); assert.equal((await api('/sessions/space-a/chat/requests/' + logoutTurn.id)).status, 404);
 await go('space-c'); assert.equal(await input.inputValue(), ''); assert.equal(await page.locator('.delivery-item').count(), 0);
 await page.locator('#btn-user-menu').click(); await page.locator('#btn-logout').click(); await page.locator('#login').waitFor({ state: 'visible' });
 await login(); await go('space-a'); await state(logoutTurn.id, 'running');
 await finish(logoutTurn.input.text); await state(logoutTurn.id, 'completed'); await accounting(logoutTurn.id); await peerLogout.close();
 passed('real logout/relogin, peer-tab signout, token revocation, another-user isolation and server-only pending recovery');

 assert.deepEqual(errors, []);
 assert.ok(h.report.static_transport.responses > 0, 'no embedded assets loaded');
 assert.deepEqual(h.report.static_transport.failures, [], 'embedded asset transport failed');
 const engineState = await h.ctl('state'); assert.deepEqual(engineState.unexpected || [], []);
 assert.equal(new Set((engineState.turns || []).map(turn => turn.prompt)).size, (engineState.turns || []).length, 'a task executed twice');
 h.report.audit = await h.ctl('audit'); h.report.engine = engineState; h.report.status = 'passed';
 await page.screenshot({ path: join(h.evidence, 'recovered.png') }); await context.close(); await h.stop();
 console.log('Evidence: ' + h.evidence);
} catch (error) {
 await activePage?.screenshot({ path: join(h.evidence, 'failure.png') }).catch(() => {});
 h.report.status = 'failed'; h.report.error = error.stack;
 h.report.browser_errors = errors; h.report.failed_requests = failedRequests;
 // Fixture evidence only: preserve startup state without tokens or message text.
 h.report.page = await activePage?.evaluate(() => ({
  ready: document.readyState, path: location.pathname, hash: location.hash,
  appHidden: document.querySelector('#app')?.classList.contains('hidden'),
  loginHidden: document.querySelector('#login')?.classList.contains('hidden'),
  hasToken: !!localStorage.getItem('agentbox_token'),
 })).catch(() => null);
 throw error;
}
finally { await browser?.close(); await h.cleanup(); }
