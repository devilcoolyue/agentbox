import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Shared synthetic browser suite: threads of one workspace run side by side
// (server chat.go chatTurn). Events carry their thread; the page draws only its
// own thread's, marks the others as running in the thread bar and list, says
// when one finishes, restores a thread's own running state and timer when it is
// opened, and still lets the user switch and start threads while one runs.
// No provider calls.
export async function chatParallelSmoke(page, { setHistory, send, setThread }) {
 const ts = '2026-10-28T08:00:01Z';
 const main = {id:'par-main', title:'Main task', ts, updated:ts, turns:1, resumable:true};
 const other = {id:'par-other', title:'Other task', ts, updated:ts, turns:2, resumable:true};
 const activated = [];
 const threadsRoute = async r => {
  const url = new URL(r.request().url());
  if (r.request().method() === 'POST' && url.pathname.endsWith('/activate')) {
   const id = url.pathname.split('/').at(-2);
   activated.push(id);
   setThread(id === other.id ? other : main);
   await r.fulfill({json:{id, resumable:true}});
   return;
  }
  await r.fulfill({json:{threads:[main, other], active:main.id}});
 };
 await page.route('**/api/sessions/*/chat/threads**', threadsRoute);
 const until = async (fn, what) => {
  for (let i = 0; !(await fn()); i++) { assert.ok(i < 150, what); await page.waitForTimeout(20); }
 };
 const reloadThread = () => page.evaluate(async () => (await import('/_v/{{BUILD}}/js/state.js')).emit('thread-changed'));
 const sendButton = page.locator('#chat-send');
 const running = () => sendButton.evaluate(el => el.classList.contains('stop'));
 const badge = page.locator('#thread-running');

 setThread(main);
 setHistory([{kind:'user', text:'main question', ts, turn:{id:'m1', model:'fixture'}}]);
 await reloadThread();
 await until(async () => (await page.locator('#thread-title').innerText()) === 'Main task', 'main thread not shown');
 assert.equal(await running(), false);

 // Another thread starts: marked, not drawn here, and this thread stays idle.
 send({type:'status', state:'running', thread:other.id});
 await badge.waitFor({state:'visible'});
 assert.equal(await badge.innerText(), '1 个运行中');
 send({type:'agent_event', thread:other.id, event:{type:'assistant', message:{content:[{type:'text', text:'OTHER-THREAD-TEXT'}]}}});
 send({type:'user_message', thread:other.id, text:'OTHER-THREAD-PROMPT', ts});
 await page.waitForTimeout(200);
 assert.equal(await page.locator('#chat-log').innerText().then(t => t.includes('OTHER-THREAD')), false, 'another thread drew into this one');
 assert.equal(await running(), false, 'another thread made this one look busy');
 if (process.env.AGENTBOX_PARALLEL_SCREENSHOTS) {
  await mkdir('output/playwright', {recursive:true});
  const size = page.viewportSize();
  await page.setViewportSize({width:390, height:844});
  await page.screenshot({path:'output/playwright/chat-parallel-mobile.png', animations:'disabled'});
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'thread bar overflows on a phone');
  await page.setViewportSize(size);
 }

 // The list shows it running; switching to it is allowed and restores its state.
 await page.locator('#btn-threads').click();
 const otherRow = page.locator('.tp-item.running').filter({hasText:'Other task'});
 await otherRow.waitFor();
 assert.match(await otherRow.innerText(), /运行中/);
 if (process.env.AGENTBOX_PARALLEL_SCREENSHOTS) {
  await mkdir('output/playwright', {recursive:true});
  await page.screenshot({path:'output/playwright/chat-parallel-list.png', animations:'disabled'});
 }
 setHistory([{kind:'user', text:'other question', ts, turn:{id:'o1', model:'fixture'}}]);
 await otherRow.locator('.tp-open').click();
 await until(async () => (await page.locator('#thread-title').innerText()) === 'Other task', 'switch blocked while running');
 assert.deepEqual(activated, [other.id]);
 await until(running, 'the running thread did not show as running');
 assert.equal(await badge.isVisible(), false, 'the open thread counted as another');

 // It finishes: drawn as usual here; then the main thread runs in the background.
 send({type:'status', state:'idle', thread:other.id});
 await until(async () => !(await running()), 'finish not applied');
 send({type:'status', state:'running', thread:main.id});
 await badge.waitFor({state:'visible'});
 send({type:'status', state:'idle', thread:main.id});
 await page.locator('#toast.show').filter({hasText:'对话「Main task」已完成'}).waitFor();
 await badge.waitFor({state:'hidden'});

 // A page joining mid-turn learns from the snapshot which threads run; the
 // top level describes the server's active thread, not necessarily this one.
 send({type:'status', state:'running', elapsed_ms:1000, threads:{[main.id]:{elapsed_ms:1000}}});
 await badge.waitFor({state:'visible'});
 assert.equal(await running(), false, 'snapshot of another thread applied here');
 send({type:'status', state:'idle', threads:{[other.id]:{elapsed_ms:65000}}});
 await until(running, 'snapshot of this thread not applied');
 await until(async () => /1 分 0\d 秒/.test(await page.locator('#chat-log').innerText()), 'elapsed time not restored');
 assert.equal(await badge.isVisible(), false);

 // A running thread cannot be deleted; the panel says why.
 send({type:'status', state:'idle', threads:{[main.id]:{}}});
 await page.locator('#btn-threads').click();
 const mainRow = page.locator('.tp-item.running').filter({hasText:'Main task'});
 await mainRow.waitFor();
 await mainRow.locator('.tp-del').last().click();
 await page.locator('#toast.show').filter({hasText:'这个对话正在运行'}).waitFor();
 assert.equal(await page.locator('#dlg-ask[open]').count(), 0);
 await page.keyboard.press('Escape');

 // The limit comes from the server (max_parallel_chats): the join status
 // carries it and a settings change pushes chat_limit; junk keeps the last.
 const limit = () => page.evaluate(async () => (await import('/_v/{{BUILD}}/js/features/chat/running.js')).maxParallelChats());
 send({type:'chat_limit', max_parallel:5});
 await until(async () => (await limit()) === 5, 'chat_limit not applied');
 send({type:'chat_limit', max_parallel:0});
 send({type:'status', state:'idle', threads:{}, max_parallel:4});
 await until(async () => (await limit()) === 4, 'status max_parallel not applied');
 send({type:'status', state:'idle', threads:{}, max_parallel:3});
 await until(async () => (await limit()) === 3, 'limit not restored');

 // Clean up: no thread runs, the fixture thread is cleared.
 send({type:'status', state:'idle', threads:{}});
 await page.unroute('**/api/sessions/*/chat/threads**', threadsRoute);
 setThread(null);
 setHistory([]);
 await reloadThread();
 await badge.waitFor({state:'hidden'});
 console.log('Chat parallel: other threads marked not drawn, list running rows, switch while running restores state and timer, finish toast, join snapshot per thread, running thread not deletable, server-set limit passed');
}
