import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Terminal tabs (term.ts): the main terminal plus independent terminals from the
// desktop client-terminals API, each its own tmux server. Web tabs live under a
// workspace-root project (path "."), created once and then reused; terminals the
// desktop opened in other projects are listed too. Typing reaches only the active
// tab, a terminal closed elsewhere (4005) disappears, and closing a tab deletes it.
// Runs on the terminal page at desktop width with the main tab already connected
// or refused; leaves only the main tab behind.
export async function terminalTabsSmoke(page, {session, agentName, agentShort}) {
  const projects = [], terminals = [], streams = new Map(), input = new Map();
  const posts = {projects: [], terminals: []}, deletes = [];
  let next = 1;
  await page.route(`**/api/sessions/${session}/client-projects`, async route => {
    const req = route.request();
    if (req.method() === 'POST') {
      const body = req.postDataJSON(); posts.projects.push(body);
      const project = {id: 'p' + next++, session_id: session, ...body, revision: 1, created_at: ''};
      projects.push(project);
      return route.fulfill({status: 201, json: project});
    }
    return route.fulfill({json: projects});
  });
  await page.route(`**/api/sessions/${session}/client-terminals**`, async route => {
    const req = route.request(), path = new URL(req.url()).pathname;
    if (req.method() === 'POST') {
      const body = req.postDataJSON(); posts.terminals.push(body);
      const terminal = {id: 't' + next++, session_id: session, project_id: body.project_id, kind: body.kind, state: 'open', arguments: [], created_at: ''};
      terminals.push(terminal);
      return route.fulfill({status: 201, json: terminal});
    }
    if (req.method() === 'DELETE') {
      const id = path.split('/').pop(); deletes.push(id);
      terminals.splice(terminals.findIndex(t => t.id === id), 1);
      return route.fulfill({json: {ok: true}});
    }
    return route.fulfill({json: terminals});
  });
  await page.routeWebSocket(`**/api/sessions/${session}/client-terminals/*/stream?*`, ws => {
    const id = new URL(ws.url()).pathname.split('/').at(-2);
    streams.set(id, ws);
    ws.onMessage(data => { if (typeof data !== 'string') input.set(id, (input.get(id) || '') + data.toString()); });
  });

  const tabs = page.locator('#term-tablist .term-tab-btn');
  const activeTab = page.locator('#term-tablist .term-tab.active .term-tab-btn');
  const visible = page.locator('#term-mount .term-pane:not(.hidden)');
  const create = async label => {
    await page.locator('#term-new').click();
    await page.locator('.menu-pop [role="menuitem"]').filter({hasText: label}).click();
  };
  const typeInto = async text => {
    await visible.locator('.xterm-screen').click({position: {x: 40, y: 20}});
    await page.keyboard.type(text);
  };

  assert.deepEqual(await tabs.allTextContents(), ['主终端'], 'only the main terminal before any tab is opened');

  await create('Shell 终端');
  await activeTab.filter({hasText: 'Shell 1'}).waitFor();
  assert.deepEqual(posts.projects, [{name: '工作区', path: '.', arguments: []}], 'the workspace-root project is created on first use');
  assert.deepEqual(posts.terminals, [{project_id: projects[0].id, kind: 'shell'}]);
  const shell = terminals[0].id;
  await page.waitForFunction(() => document.querySelector('#term-state')?.dataset.state === 'connected');
  assert.ok(streams.has(shell), 'the new tab attaches to its own stream');
  assert.equal(await page.locator('#term-mount .term-pane:not(.hidden) .xterm').count(), 1, 'one visible xterm');

  await typeInto('echo shell');
  await expectInput(page, input, shell, 'echo shell');

  await create(`${agentName} 终端`);
  await activeTab.filter({hasText: `${agentShort} 1`}).waitFor();
  assert.equal(posts.projects.length, 1, 'the root project is reused');
  assert.deepEqual(posts.terminals.at(-1), {project_id: projects[0].id, kind: 'agent'});
  const agent = terminals[1].id;
  await page.waitForFunction(() => document.querySelector('#term-state')?.dataset.state === 'connected');
  await typeInto('hi agent');
  await expectInput(page, input, agent, 'hi agent');
  assert.equal(input.get(shell), 'echo shell', 'input reaches only the active tab');

  // Output written to a background tab stays in that tab.
  streams.get(shell).send('from-shell-background');
  await tabs.filter({hasText: 'Shell 1'}).click();
  // WebGL draws on a canvas, so read the active xterm's buffer rather than DOM rows.
  await page.waitForFunction(async () => {
    const {S} = await import('/_v/{{BUILD}}/js/state.js');
    const buf = S.term?.buffer.active;
    return !!buf && Array.from({length: buf.length}, (_, i) => buf.getLine(i)?.translateToString(true)).join('\n').includes('from-shell-background');
  });
  assert.equal(await page.evaluate(async () => {
    const {S} = await import('/_v/{{BUILD}}/js/state.js');
    return S.term?.element?.closest('.term-pane')?.classList.contains('hidden');
  }), false, 'S.term follows the active tab');

  // A terminal the desktop opened in another project shows up after the list refreshes.
  projects.push({id: 'desk', session_id: session, name: 'api', path: 'services/api', arguments: [], revision: 1, created_at: ''});
  terminals.push({id: 'tdesk', session_id: session, project_id: 'desk', kind: 'shell', state: 'open', arguments: [], created_at: ''});
  await page.locator('.tab[data-tab="chat"]').click();
  await page.locator('.tab[data-tab="term"]').click();
  await tabs.filter({hasText: 'Shell 2 · api'}).waitFor();
  assert.equal(streams.has('tdesk'), false, 'a listed tab connects only when opened');
  if (process.env.AGENTBOX_TERM_TABS_SCREENSHOTS) {
    await mkdir('output/playwright', {recursive: true});
    await page.screenshot({path: 'output/playwright/term-tabs-desktop.png', animations: 'disabled'});
    const size = page.viewportSize();
    await page.setViewportSize({width: 390, height: 844});
    await page.screenshot({path: 'output/playwright/term-tabs-mobile.png', animations: 'disabled'});
    await page.setViewportSize(size);
  }

  // Closed elsewhere: the server ends the stream with 4005 and the list no longer has it.
  await tabs.filter({hasText: `${agentShort} 1`}).click();
  await page.waitForFunction(() => document.querySelector('#term-state')?.dataset.state === 'connected');
  terminals.splice(terminals.findIndex(t => t.id === agent), 1);
  streams.get(agent).close({code: 4005, reason: '终端已结束或正在关闭'});
  await tabs.filter({hasText: `${agentShort} 1`}).waitFor({state: 'detached'});
  await activeTab.filter({hasText: '主终端'}).waitFor();

  // Closing from the tab asks first, then deletes the terminal.
  for (const label of ['Shell 1', 'Shell 1 · api']) {
    const tab = page.locator('#term-tablist .term-tab').filter({has: page.locator('.term-tab-btn', {hasText: new RegExp(`^${label}$`)})});
    await tab.locator('.term-tab-x').click();
    await page.locator('#dlg-ask[open]').waitFor();
    await page.locator('#ask-ok').click();
    await tab.waitFor({state: 'detached'});
  }
  assert.deepEqual(deletes, [shell, 'tdesk']);
  assert.deepEqual(await tabs.allTextContents(), ['主终端']);
  assert.equal(await page.locator('#term-mount .xterm').count(), 1, 'closed tabs dispose their xterm');
  await page.unroute(`**/api/sessions/${session}/client-projects`);
  await page.unroute(`**/api/sessions/${session}/client-terminals**`);
  console.log('Terminal tabs: root project created once, Shell/Agent tabs with their own streams, input only to the active tab, background output, desktop terminals listed lazily, closed elsewhere (4005), close confirm/delete passed');
}

async function expectInput(page, input, id, text) {
  const deadline = Date.now() + 5000;
  while ((input.get(id) || '') !== text) {
    assert.ok(Date.now() < deadline, `terminal ${id} got ${JSON.stringify(input.get(id))}, want ${JSON.stringify(text)}`);
    await page.waitForTimeout(50);
  }
}
