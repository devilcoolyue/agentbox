import assert from 'node:assert/strict';
import { mkdir, readFile } from 'node:fs/promises';

// Shared synthetic browser suite: Claude tool calls drawn the way Claude Code's
// terminal shows them. The history is real stream-json from Claude Code 2.1.280 /
// 2.1.304 (scripts/fixtures/claude-cli-2.1-events.json, ids and local paths
// scrubbed): reads, searches and plain commands fold into one summary line,
// Write / Update show a summary line that opens into numbered content and diffs,
// Fetch / Web Search / Agent their result lines, failures their error. Live events
// then check the running state.
// No provider calls.
export async function chatClaudeToolsSmoke(page, { setHistory, send, socket, setAgent }) {
 const fixture = JSON.parse(await readFile(new URL('./fixtures/claude-cli-2.1-events.json', import.meta.url), 'utf8'));
 const reload = async () => {
  const before = socket();
  await page.reload();
  await page.locator('#chat-log .turn, #chat-hero:not(.hidden)').first().waitFor();
  for (let i = 0; socket() === before; i++) { assert.ok(i < 100, 'chat socket did not reconnect'); await page.waitForTimeout(20); }
 };
 const until = async (fn, what) => {
  for (let i = 0; !(await fn()); i++) { assert.ok(i < 150, what); await page.waitForTimeout(20); }
 };
 const ts = '2026-10-21T08:00:01Z';
 const turn = id => ({id, model:'claude-haiku-5-5', effort:'', control:''});
 const entries = [];
 for (const [name, list] of Object.entries(fixture)) {
  for (const e of list) entries.push(e.kind === 'user' ? {...e, ts, turn:turn(name)} : {...e, ts});
 }
 // What a history page sends after a reload (server/history_tools.go): the
 // tool_result text cut to its first lines, how many were left out, no stdout.
 entries.push({kind:'user', ts, text:'跑全部测试', turn:turn('slim')},
  {kind:'event', ts, event:{type:'assistant', message:{content:[{type:'tool_use', id:'toolu_slim', name:'Bash', input:{command:'npm test'}}]}}},
  {kind:'event', ts, event:{type:'user', message:{role:'user', content:[{type:'tool_result', tool_use_id:'toolu_slim', content:'ok 1\nok 2\nok 3\nok 4', omitted_lines:120}]}}},
  {kind:'event', ts, event:{type:'result', subtype:'success', duration_ms:1000}}, {kind:'status', ts, state:'idle'});
 setAgent('claude');
 setHistory(entries);
 await reload();
 const turns = page.locator('#chat-log .turn');
 await until(async () => await turns.count() === 4, 'four turns rendered');
 // One line per top-level tool element, as text, to compare with the terminal.
 const lines = n => turns.nth(n).locator(':scope > .turn-body > .tool-call').evaluateAll(list => list.map(r =>
  r.classList.contains('cc-group') ? r.querySelector('.cc-gsum').textContent : r.querySelector('.cc-head').textContent));
 const results = n => turns.nth(n).locator(':scope > .turn-body > .cc-tool > .cc-res').evaluateAll(list => list.map(r => r.textContent));

 // Turn 1: Bash / Read / Write / Edit, failures included.
 assert.deepEqual(await lines(0), [
  '列出 1 个目录，读取 1 个文件，搜索 1 个模式 · 1 处出错',
  '搜索 2 个模式 · 1 处出错',
  '列出 1 个目录',
  '●Write(a.py)',
  '●Update(a.py)',
  '运行 1 条命令，读取 1 个文件 · 2 处出错',
 ], 'reads, searches and commands fold like the CLI; anything else ends the group');
 const write = turns.nth(0).locator('.cc-tool[data-tool="Write"]');
 assert.equal(await write.locator('.cc-res .cc-line').first().textContent(), '已写入 1 行到 a.py');
 assert.equal(await write.locator('.cc-num').count(), 0, 'written content stays folded until the row is opened');
 await write.locator('.cc-head').click();
 assert.equal(await write.locator('.cc-num').textContent(), '1 print(1)\n');
 const edit = turns.nth(0).locator('.cc-tool[data-tool="Edit"]');
 assert.equal(await edit.locator('.cc-res .cc-line').first().textContent(), '新增 1 行，删除 1 行');
 assert.equal(await edit.locator('.cc-diff').count(), 0, 'the diff stays folded until the row is opened');
 await edit.locator('.cc-head').click();
 assert.deepEqual(await edit.locator('.cc-diff .dl').evaluateAll(l => l.map(d => d.className + ':' + d.textContent)),
  ['dl del: -print(1)', 'dl add: +print(2)']);
 // A folded group opens into a list of its calls; each call opens into its full output.
 const failed = turns.nth(0).locator('.cc-group').last();
 assert.equal(await failed.locator('.cc-gitems').isVisible(), false);
 await failed.locator('.cc-gsum').click();
 assert.deepEqual(await failed.locator('.cc-gitems > .cc-tool > .cc-head').evaluateAll(l => l.map(h => h.textContent)),
  ['●Bash(python3 a.py; exit 3)', '●Read(nope.txt)']);
 assert.equal(await failed.locator('.cc-gitems .cc-res').count(), 0, 'the opened group lists calls without their output');
 for (const head of await failed.locator('.cc-gitems > .cc-tool > .cc-head').all()) await head.click();
 assert.equal(await failed.locator('.cc-tool[data-tool="Bash"] .cc-pre.err').textContent(), 'Error: Exit code 3\n2');
 assert.match(await failed.locator('.cc-tool[data-tool="Read"] .cc-pre.err').textContent(), /^Error: File does not exist/);
 assert.equal(await failed.locator('.cc-tool[data-status="failed"]').count(), 2);

 // Turn 2: ToolSearch is hidden as in the CLI; the sub-agent's own Bash stays inside its row.
 assert.deepEqual(await lines(1), [
  '●Web Search("golang 1.26 release notes")',
  '●Fetch(https://example.com)',
  '●Agent(运行 uname 并汇报)',
  '运行 1 条命令',
 ]);
 assert.deepEqual(await results(1), [
  '⎿搜索 1 次，用时 9s', '⎿收到 577 bytes（200 OK）', '⎿完成（1 次工具调用 · 17.4k tokens · 4s）',
 ]);
 const agent = turns.nth(1).locator('.cc-tool[data-tool="Agent"]');
 await agent.locator('.cc-head').click();
 assert.deepEqual(await agent.locator('.cc-tool > .cc-head').evaluateAll(l => l.map(h => h.textContent)), ['●Bash(uname -a)']);
 assert.match(await agent.locator('.cc-reply').textContent(), /Linux/);
 const search = turns.nth(1).locator('.cc-tool[data-tool="WebSearch"]');
 await search.locator('.cc-head').click();
 assert.ok(await search.locator('.cc-links a').count() >= 4, 'search results listed when expanded');
 assert.ok((await search.locator('.cc-links a').evaluateAll(l => l.map(a => a.href))).every(h => /^https:\/\//.test(h)));

 // Turn 3: two edits with their hunks, a background command.
 assert.deepEqual(await lines(2), [
  '读取 1 个文件，搜索 1 个模式，列出 1 个目录',
  '●Update(src/x.ts)',
  '●Update(src/x.ts)',
  '●Bash(sleep 1; echo bg)',
  '运行 1 条命令',
  '读取 1 个文件',
 ]);
 await turns.nth(2).locator('.cc-tool[data-tool="Edit"]').first().locator('.cc-head').click();
 assert.deepEqual(await turns.nth(2).locator('.cc-tool[data-tool="Edit"]').first().locator('.cc-diff').evaluate(p => p.textContent),
  '1 -export const a = 1;\n1 +export const a = 2;\n2  // TODO fix\n');
 assert.equal(await turns.nth(2).locator(':scope > .turn-body > .cc-tool[data-tool="Bash"] > .cc-res').textContent(), '⎿在后台运行');
 assert.match(await page.locator('#chat-log .chip.bg-task').textContent(), /后台任务完成：Background task: sleep 1 then echo/);
 const slim = turns.nth(3).locator('.cc-group');
 await slim.locator('.cc-gsum').click();
 await slim.locator('.cc-gitems .cc-head').click();
 assert.equal(await slim.locator('.cc-pre').textContent(), 'ok 1\nok 2\nok 3\nok 4');
 assert.equal(await slim.locator('.cc-out > .cc-line').last().textContent(), '… 还有 120 行', 'lines the page left out are counted');
 assert.equal(await page.locator('#chat-log [data-status="running"]').count(), 0, 'nothing runs once the history is over');
 await mkdir('output/playwright', {recursive:true});
 await turns.nth(0).screenshot({path:'output/playwright/chat-claude-tools-1.png', animations:'disabled'});
 await turns.nth(1).screenshot({path:'output/playwright/chat-claude-tools-2.png', animations:'disabled'});
 await turns.nth(2).screenshot({path:'output/playwright/chat-claude-tools-3.png', animations:'disabled'});

 // Live: a command runs inside a folded group, the working line names it, the
 // group counts on, and the result updates the same row.
 send({type:'user_message', text:'跑下测试', turn:turn('live'), ts});
 send({type:'status', state:'running'});
 const ev = event => send({type:'agent_event', event, ts});
 ev({type:'assistant', message:{content:[{type:'tool_use', id:'toolu_live1', name:'Bash', input:{command:'npm test', description:'Run tests'}}]}});
 const live = page.locator('#chat-log .turn:not(.working)').last();
 const group = live.locator('.cc-group');
 await group.waitFor();
 assert.equal(await group.getAttribute('data-status'), 'running');
 assert.equal(await group.locator('.cc-gsum').textContent(), '正在运行 1 条命令…');
 assert.equal(await group.locator('.cc-gsum .cc-icon svg').getAttribute('data-icon-name'), 'chat.tool-shell', 'command groups lead with the terminal icon');
 assert.equal(await group.locator('.cc-hint').textContent(), '⎿$ npm test');
 await until(async () => await page.locator('.turn.working .work-label').textContent() === '执行 npm test…', 'working line names the command');
 await until(async () => /· \ds…$/.test(await group.locator('.cc-gsum').textContent()), 'running group counts seconds');
 await page.screenshot({path:'output/playwright/chat-claude-running.png', animations:'disabled'});
 ev({type:'assistant', message:{content:[{type:'tool_use', id:'toolu_live2', name:'Read', input:{file_path:'/workspace/package.json'}}]}});
 await until(async () => (await group.locator('.cc-gsum').textContent()).startsWith('正在运行 1 条命令，正在读取 1 个文件'), 'second call joins the group');
 ev({type:'user', message:{role:'user', content:[{type:'tool_result', tool_use_id:'toolu_live1', content:'ok 1\nok 2', is_error:false}]},
  tool_use_result:{stdout:'ok 1\nok 2', stderr:'', interrupted:false}});
 ev({type:'user', message:{role:'user', content:[{type:'tool_result', tool_use_id:'toolu_live2', content:'1\t{}'}]},
  tool_use_result:{type:'text', file:{filePath:'/workspace/package.json', content:'{}', numLines:1, startLine:1, totalLines:1}}});
 await until(async () => await group.getAttribute('data-status') === 'completed', 'results complete the group in place');
 assert.equal(await group.locator('.cc-gsum').textContent(), '运行 1 条命令，读取 1 个文件');
 assert.equal(await group.locator('.cc-gsum .cc-icon svg').getAttribute('data-icon-name'), 'chat.tool-group', 'mixed groups lead with the wrench');
 assert.equal(await live.locator(':scope > .turn-body > .tool-call').count(), 1);
 // Text ends the group; a sub-agent shows its latest call while it runs.
 ev({type:'assistant', message:{content:[{type:'text', text:'测试通过，再让子 Agent 看下。'}]}});
 ev({type:'assistant', message:{content:[{type:'tool_use', id:'toolu_agent', name:'Agent', input:{description:'检查依赖', prompt:'看看依赖', subagent_type:'general-purpose'}}]}});
 ev({type:'assistant', parent_tool_use_id:'toolu_agent', message:{content:[{type:'tool_use', id:'toolu_sub', name:'Bash', input:{command:'npm ls'}}]}});
 const sub = live.locator('.cc-tool[data-tool="Agent"]');
 await until(async () => await sub.locator('.cc-child').count() === 1, 'sub-agent lists its call');
 assert.equal(await sub.locator('.cc-child').textContent(), 'Bash(npm ls)');
 assert.equal(await live.locator(':scope > .turn-body > .tool-call').count(), 2, 'the sub-agent call is not a top-level row');
 // The turn ends while it runs: nothing keeps blinking.
 send({type:'status', state:'idle'});
 await until(async () => await live.locator('[data-status="running"]').count() === 0, 'settled when idle');
 assert.equal(await sub.getAttribute('data-status'), 'unfinished');

 // A long command's heartbeats fold into one row with a count; consecutive thinking folds too.
 send({type:'user_message', text:'部署', turn:turn('beats'), ts});
 send({type:'status', state:'running'});
 ev({type:'assistant', message:{content:[{type:'tool_use', id:'toolu_long', name:'Bash', input:{command:'npm run deploy'}}]}});
 for (const [i, sec] of [30, 60, 90].entries()) ev({type:'tool_progress', tool_use_id:`toolu_long-heartbeat-${i}`, tool_name:'Bash', parent_tool_use_id:'toolu_long', elapsed_time_seconds:sec, heartbeat:true});
 const beats = page.locator('#chat-log .turn:not(.working)').last().locator('.fold-beat');
 await until(async () => await beats.count() === 1 && await beats.locator('.fold-item').count() === 3, 'heartbeats fold into one row');
 assert.equal(await beats.locator('.fold-head').textContent(), '心跳检查 · 3 次 · Bash(npm run deploy) 已运行 1m 30s');
 assert.equal(await page.locator('#chat-log details.raw').count(), 0, 'heartbeats are no longer raw events');
 assert.equal(await beats.locator('.fold-items').isVisible(), false);
 await beats.locator('.fold-head').click();
 assert.deepEqual(await beats.locator('.fold-item > summary').evaluateAll(l => l.map(x => x.textContent)),
  ['Bash(npm run deploy)已运行 30s', 'Bash(npm run deploy)已运行 1m 0s', 'Bash(npm run deploy)已运行 1m 30s']);
 assert.equal(await beats.locator('.beat-raw').first().isVisible(), false);
 await beats.locator('.fold-item > summary').first().click();
 assert.match(await beats.locator('.beat-raw').first().textContent(), /"elapsed_time_seconds": 30/);
 ev({type:'user', message:{role:'user', content:[{type:'tool_result', tool_use_id:'toolu_long', content:'deployed'}]}, tool_use_result:{stdout:'deployed', stderr:''}});
 for (const text of ['Checking the logs.\nMore detail.', 'Looks fine.', 'Writing the summary.']) ev({type:'assistant', message:{content:[{type:'thinking', thinking:text, signature:'sig'}]}});
 const thinks = page.locator('#chat-log .turn:not(.working)').last().locator('.fold-think');
 await until(async () => await thinks.locator('details.think').count() === 3, 'consecutive thinking folds into one row');
 assert.equal(await page.locator('#chat-log .turn:not(.working)').last().locator(':scope > .turn-body > details.think').count(), 0);
 assert.equal(await thinks.locator('.fold-head').textContent(), '思考过程 · 3 段');
 assert.equal(await thinks.locator('.fold-head svg').getAttribute('data-icon-name'), 'chat.thinking');
 await thinks.locator('.fold-head').click();
 assert.deepEqual(await thinks.locator('.think-peek').evaluateAll(l => l.map(x => x.textContent)), ['Checking the logs.', 'Looks fine.', 'Writing the summary.']);
 assert.equal(await thinks.locator('.think-body').first().isVisible(), false, 'each step stays closed until clicked');
 await thinks.locator('details.think > summary').first().click();
 assert.equal(await thinks.locator('.think-body').first().textContent(), 'Checking the logs.\nMore detail.');
 await page.locator('#chat-log .turn:not(.working)').last().screenshot({path:'output/playwright/chat-claude-folds.png', animations:'disabled'});
 send({type:'status', state:'idle'});

 // Narrow screens: long commands and diffs scroll inside their rows.
 await page.setViewportSize({width:390, height:844});
 await turns.nth(0).scrollIntoViewIfNeeded();
 assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
 await page.screenshot({path:'output/playwright/chat-claude-tools-mobile.png', animations:'disabled'});
 await page.setViewportSize({width:1280, height:900});
 setHistory([]);
 // Leave the workspace as Codex, as the Codex items suite does: later suites expect it.
 setAgent('codex');
 console.log('Chat Claude tools: real Claude Code stream-json rendered like the CLI (folded read/search/command groups with failures, Write/Update summaries opening into numbered content and diffs, Fetch/Web Search/Agent result lines, hidden ToolSearch, sub-agent calls kept in their row, background command), calls listed on expand and opened one by one, live running group with seconds completing in place, settled when idle, heartbeats and consecutive thinking folded with counts, mobile passed');
}
