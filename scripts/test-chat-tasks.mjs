import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Shared synthetic browser suite: the agent's task list above the composer —
// Claude Code's task files (read by the server), TodoWrite and Codex's plan —
// the working line naming the task in progress, and Codex's context meter.
// No provider calls.
export async function chatTasksSmoke(page, { setHistory, send, socket, setAgent, setThread }) {
 let files = [];
 const reads = [];
 const route = async r => {
  reads.push(new URL(r.request().url()).searchParams.get('thread'));
  await r.fulfill({json:{tasks:files}}).catch(() => {});
 };
 await page.route('**/api/sessions/*/chat/tasks*', route);
 const panel = page.locator('#chat-tasks'), toggle = page.locator('#chat-tasks-toggle');
 const current = () => panel.locator('.tasks-current').innerText();
 const reload = async () => {
  const before = socket();
  await page.reload();
  await page.locator('#chat-log .turn, #chat-hero:not(.hidden)').first().waitFor();
  for (let i = 0; socket() === before; i++) { assert.ok(i < 100, 'chat socket did not reconnect'); await page.waitForTimeout(20); }
 };
 const ts = '2026-10-14T08:00:01Z';
 const turn = id => ({id, model:'claude-opus-4-7', effort:'', control:'effort'});
 const tool = (id, name, input) => ({type:'assistant', parent_tool_use_id:null, message:{id, model:'claude-opus-4-7', content:[{type:'tool_use', id:'tu-' + id, name, input}], usage:{input_tokens:10}}});
 const until = async (fn, what) => {
  for (let i = 0; !(await fn()); i++) { assert.ok(i < 150, what); await page.waitForTimeout(20); }
 };

 // History: each task tool leaves one line; the list itself comes from the
 // task files the server reads, shown collapsed while idle.
 files = [
  {id:'1', subject:'Read the config', active_form:'Reading the config', status:'completed'},
  {id:'2', subject:'Fix the bug', active_form:'Fixing the bug', status:'in_progress'},
  {id:'3', subject:'Run <b>tests</b>', active_form:'Running tests', status:'pending', blocked_by:['2']},
 ];
 // The list belongs to the thread (its provider session) the history names.
 setThread({id:'tasks-thread', title:'修一下这个 bug', ts, updated:ts, turns:1, resumable:true});
 setAgent('claude');
 setHistory([
  {kind:'user', ts, text:'修一下这个 bug', turn:turn('t1')},
  {kind:'event', ts, event:tool('a1', 'TaskCreate', {subject:'Read the config', description:'x', activeForm:'Reading the config'})},
  {kind:'event', ts, event:tool('a2', 'TaskCreate', {subject:'Fix the bug', description:'x', activeForm:'Fixing the bug'})},
  {kind:'event', ts, event:tool('a3', 'TaskUpdate', {taskId:'1', status:'completed'})},
  {kind:'event', ts, event:tool('a4', 'TaskList', {})},
  {kind:'event', ts, event:{type:'result', subtype:'success', is_error:false, num_turns:4}},
  {kind:'status', ts, state:'idle'},
 ]);
 const readsBefore = reads.length;
 await reload();
 await panel.waitFor();
 assert.ok(reads.length > readsBefore && reads.at(-1) === 'tasks-thread', 'task list read for the thread');
 assert.equal(await toggle.getAttribute('aria-expanded'), 'false', 'collapsed while idle');
 assert.equal(await panel.locator('.tasks-count').innerText(), '任务 1/3');
 assert.equal(await current(), 'Fixing the bug');
 const chips = await page.locator('#chat-log .chip.tool').evaluateAll(list => list.map(c => c.textContent));
 assert.deepEqual(chips, ['新建任务Read the config', '新建任务Fix the bug', '更新任务#1 → 已完成'], 'TaskList is read-only and not shown');
 await toggle.click();
 const rows = panel.locator('.task-item');
 assert.deepEqual(await rows.evaluateAll(list => list.map(li => [li.dataset.status, li.querySelector('.task-text').textContent, li.querySelector('.task-wait')?.textContent || ''])),
  [['completed', 'Read the config', ''], ['in_progress', 'Fix the bug', ''], ['pending', 'Run <b>tests</b>', '等待 #2']]);
 assert.equal(await panel.locator('b').count(), 0, 'task text written by the model stays text');
 assert.equal(await rows.first().locator('.task-mark').getAttribute('aria-label'), '已完成');
 await mkdir('output/playwright', {recursive:true});
 await page.locator('.composer').screenshot({path:'output/playwright/chat-tasks.png', animations:'disabled'});

 // Live: a task update re-reads the list; the working line names the task in
 // progress, as the CLI's spinner does; the panel opens while the turn runs.
 send({type:'user_message', text:'继续', turn:turn('t2'), ts:'2026-10-14T08:05:00Z'});
 send({type:'status', state:'running'});
 await until(async () => await toggle.getAttribute('aria-expanded') === 'true', 'panel opens while running');
 files = [files[0], {...files[1], status:'completed'}, {...files[2], status:'in_progress', blocked_by:[]}];
 send({type:'agent_event', event:tool('a5', 'TaskUpdate', {taskId:'3', status:'in_progress'})});
 const label = page.locator('.turn.working .work-label');
 await until(async () => await label.textContent() === 'Running tests…', 'working line names the task');
 assert.equal(await panel.locator('.tasks-count').innerText(), '任务 2/3');
 await page.screenshot({path:'output/playwright/chat-tasks-running.png', animations:'disabled'});
 // A plain stage change keeps the task's words while one is in progress.
 send({type:'agent_event', event:{type:'stream_event', event:{type:'content_block_start', index:0, content_block:{type:'thinking'}}}});
 await page.waitForTimeout(100);
 assert.equal(await label.textContent(), 'Running tests…');
 files = files.map(t => ({...t, status:'completed'}));
 send({type:'agent_event', event:tool('a6', 'TaskUpdate', {taskId:'3', status:'completed'})});
 await until(async () => await current() === '全部完成', 'all done while running');
 send({type:'agent_event', event:{type:'result', subtype:'success', is_error:false, num_turns:2}});
 send({type:'status', state:'idle'});
 await panel.waitFor({state:'hidden'});
 // The plan is over: an ordinary question next does not bring it back (the
 // server leaves finished plans out once a turn begins; the page drops its
 // copy right away instead of waiting for that read).
 const staysHidden = async what => {
  for (let i = 0; i < 20; i++) { assert.ok(await panel.isHidden(), what); await page.waitForTimeout(20); }
 };
 files = [];
 send({type:'user_message', text:'谢谢，顺便问一下', turn:turn('t2b'), ts:'2026-10-14T08:07:00Z'});
 send({type:'status', state:'running'});
 await staysHidden('finished plan back in the next turn');
 send({type:'agent_event', event:{type:'result', subtype:'success', is_error:false, num_turns:1}});
 send({type:'status', state:'idle'});

 // TodoWrite (older CLIs, or the task files turned off): the latest call's list.
 files = [];
 setHistory([
  {kind:'user', ts, text:'做两件事', turn:turn('t3')},
  {kind:'event', ts, event:tool('w1', 'TodoWrite', {todos:[{content:'A', status:'pending', activeForm:'Doing A'}]})},
  {kind:'event', ts, event:tool('w2', 'TodoWrite', {todos:[{content:'A', status:'completed', activeForm:'Doing A'}, {content:'B', status:'in_progress', activeForm:'Doing B'}]})},
  {kind:'event', ts, event:{type:'result', subtype:'success', is_error:false, num_turns:2}},
  {kind:'status', ts, state:'idle'},
 ]);
 await reload();
 await panel.waitFor();
 assert.equal(await panel.locator('.tasks-count').innerText(), '任务 1/2');
 assert.equal(await current(), 'Doing B');
 assert.deepEqual(await page.locator('#chat-log .chip.tool').evaluateAll(list => list.map(c => c.textContent)), ['更新任务列表0/1', '更新任务列表1/2']);
 // Finished, the list ends with the turn, in the history and live alike.
 setHistory([
  {kind:'user', ts, text:'做两件事', turn:turn('t3')},
  {kind:'event', ts, event:tool('w3', 'TodoWrite', {todos:[{content:'A', status:'completed', activeForm:'Doing A'}, {content:'B', status:'completed', activeForm:'Doing B'}]})},
  {kind:'event', ts, event:{type:'result', subtype:'success', is_error:false, num_turns:2}},
  {kind:'status', ts, state:'idle'},
  {kind:'user', ts, text:'下一个问题', turn:turn('t4')},
  {kind:'event', ts, event:{type:'result', subtype:'success', is_error:false, num_turns:1}},
  {kind:'status', ts, state:'idle'},
 ]);
 await reload();
 send({type:'user_message', text:'再问一个', turn:turn('t5'), ts:'2026-10-14T08:09:00Z'});
 send({type:'status', state:'running'});
 await staysHidden('finished TodoWrite list back in a later turn');
 setHistory([
  {kind:'user', ts, text:'做两件事', turn:turn('t3')},
  {kind:'event', ts, event:tool('w4', 'TodoWrite', {todos:[{content:'A', status:'completed', activeForm:'Doing A'}, {content:'B', status:'completed', activeForm:'Doing B'}]})},
  {kind:'event', ts, event:{type:'result', subtype:'success', is_error:false, num_turns:2}},
  {kind:'status', ts, state:'idle'},
 ]);
 await reload();
 send({type:'user_message', text:'再问一个', turn:turn('t6'), ts:'2026-10-14T08:10:00Z'});
 send({type:'status', state:'running'});
 await staysHidden('finished TodoWrite list back in the next live turn');
 send({type:'status', state:'idle'});

 // Codex: the plan comes from todo_list events (app-server keeps "in progress"),
 // the context meter from token_count; neither shows up as raw JSON.
 setAgent('codex');
 const codexTurn = id => ({id, model:'gpt-5.5', effort:'medium', control:'effort'});
 const plan = items => ({type:'item.updated', item:{type:'todo_list', items}});
 setHistory([
  {kind:'user', ts, text:'改一下配置读取', turn:codexTurn('c1')},
  {kind:'event', ts, event:plan([{text:'读配置', completed:true, status:'completed'}, {text:'改代码', completed:false, status:'in_progress'}, {text:'跑测试', completed:false, status:'pending'}])},
  {kind:'event', ts, event:{type:'token_count', info:{last_token_usage:{input_tokens:110000, cached_input_tokens:90000, output_tokens:3000, reasoning_output_tokens:1000, total_tokens:113000}, model_context_window:272000}}},
  {kind:'event', ts, event:{type:'turn.completed', usage:{input_tokens:110000, output_tokens:3000}}},
  {kind:'status', ts, state:'idle'},
 ]);
 const codexReads = reads.length;
 await reload();
 await panel.waitFor();
 assert.equal(await panel.locator('.tasks-count').innerText(), '任务 1/3');
 assert.equal(await current(), '改代码');
 assert.equal(reads.length, codexReads, 'Codex keeps no task files to read');
 const meter = page.locator('#chat-context');
 await meter.waitFor();
 // Codex's own "context left": (112000 − 12000) / (272000 − 12000) used.
 assert.equal(await meter.locator('.ctx-value').innerText(), '38%');
 assert.match(await meter.getAttribute('data-tip'), /Codex 终端/);
 assert.equal(await page.locator('#chat-log details.raw').count(), 0);
 // exec --json's todo_list (completed only) works too; a new turn drops the old plan.
 send({type:'user_message', text:'再跑一次测试', turn:codexTurn('c2'), ts:'2026-10-14T08:10:00Z'});
 await panel.waitFor({state:'hidden'});
 send({type:'status', state:'running'});
 send({type:'agent_event', event:{type:'item.started', item:{id:'item_1', type:'todo_list', items:[{text:'跑测试', completed:false}]}}});
 await panel.waitFor();
 assert.equal(await panel.locator('.tasks-count').innerText(), '任务 0/1');
 assert.equal(await current(), '下一项：跑测试');
 send({type:'agent_event', event:{type:'token_count', info:{last_token_usage:{input_tokens:250000, output_tokens:10000, reasoning_output_tokens:2000, total_tokens:260000}, model_context_window:272000}}});
 await until(async () => await meter.locator('.ctx-value').innerText() === '95%', 'live Codex context');
 assert.equal(await meter.getAttribute('data-level'), 'critical');
 send({type:'agent_event', event:{type:'item.completed', item:{id:'item_1', type:'todo_list', items:[{text:'跑测试', completed:true}]}}});
 send({type:'agent_event', event:{type:'turn.completed', usage:{input_tokens:1, output_tokens:1}}});
 send({type:'status', state:'idle'});
 await panel.waitFor({state:'hidden'});

 // Narrow screens: the panel stays inside the composer.
 setHistory([
  {kind:'user', ts, text:'改一下配置读取', turn:codexTurn('c1')},
  {kind:'event', ts, event:plan([{text:'读取并核对配置文件里每个账号的出口代理设置，确认没有遗漏', completed:false, status:'in_progress'}, {text:'跑测试', completed:false, status:'pending'}])},
  {kind:'event', ts, event:{type:'item.completed', item:{type:'agent_message', text:'先核对配置。'}}},
  {kind:'status', ts, state:'idle'},
 ]);
 await reload();
 await page.setViewportSize({width:390, height:844});
 await panel.waitFor();
 await toggle.click();
 assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
 const box = await panel.boundingBox(), composer = await page.locator('.composer').boundingBox();
 assert.ok(box.x >= composer.x && box.x + box.width <= composer.x + composer.width + 0.5, 'panel overflows the composer');
 await page.screenshot({path:'output/playwright/chat-tasks-mobile.png', animations:'disabled'});
 await page.setViewportSize({width:1280, height:900});

 await page.unroute('**/api/sessions/*/chat/tasks*', route);
 // Leave the workspace as Codex, as the mentions suite does: later suites expect it.
 setThread(null);
 setHistory([]);
 console.log('Chat tasks: Claude task files read per thread, one line per task tool (TaskList hidden), collapsed idle / open running, working line names the task, all-done while running then hidden and gone from the next turn, TodoWrite lists (finished ones gone from later turns), Codex plans (app-server and exec) reset per turn, Codex context meter from token_count, mobile passed');
}
