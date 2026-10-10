import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { resolve } from 'node:path';

// Shared synthetic browser suite: the newest history page renders first, older
// pages load on scrolling up without moving the reading position, a turn the
// server split across pages renders as one turn, failures offer a retry, and a
// reconnect re-reads everything already loaded. No provider calls.
export async function chatPagesSmoke(page, { session, closeChat }) {
 const thread = {id:'thread-pages',title:'Paged thread',ts:'2026-10-01T00:00:00Z',updated:'2026-10-01T01:00:00Z',turns:25,resumable:true};
 const all = [], starts = [];
 const ts = i => new Date(Date.UTC(2026, 9, 1, 0, i)).toISOString();
 for (let i = 0; i < 25; i++) {
  starts.push(all.length);
  const turn = {id:'paged-'+i,model:'fixture',effort:'',control:'effort'};
  all.push({kind:'user',ts:ts(i),text:`第 ${i} 个问题`,turn});
  for (let j = 0; j < (i === 12 ? 6 : 1); j++) {
   all.push({kind:'event',ts:ts(i),event:{type:'assistant',message:{content:[{type:'text',text:`回答 ${i}.${j}\n\n`+'这是一段用于撑开高度的合成回答。'.repeat(12)}]}}});
  }
  all.push({kind:'event',ts:ts(i),event:{type:'result',subtype:'success',duration_ms:1000}});
  all.push({kind:'status',ts:ts(i),state:'idle'});
 }
 // Pages as the server cuts them: whole turns, except the long turn 12 which
 // is split after its third answer; the page starting inside it carries the
 // request snapshot as turn_context.
 const older = starts[6], split = starts[12] + 4;
 const slice = start => [...(all[start].kind === 'user' || start === 0 ? [] : [{kind:'turn_context',ts:ts(12),turn:all[starts[12]].turn}]), ...all.slice(start)];
 const requests = [];
 let hold = null, failNext = false;
 const handler = async route => {
  const u = new URL(route.request().url()), q = u.searchParams;
  requests.push(Object.fromEntries(q));
  if (q.has('before')) {
   if (hold) await hold;
   if (failNext) { failNext = false; await route.fulfill({status:500,json:{error:'fixture history failure'}}); return; }
   const end = Number(q.get('before')), start = end === split ? older : 0;
   await route.fulfill({json:{entries:all.slice(start, end),thread_id:thread.id,costs:{},start,has_more:start > 0}});
   return;
  }
  const from = q.get('thread') === thread.id && q.has('from') ? Number(q.get('from')) : split;
  await route.fulfill({json:{entries:slice(from),thread,active_thread:thread.id,costs:{},start:from,has_more:from > 0}});
 };
 const matches = url => url.pathname === `/api/sessions/${session}/history`;
 await page.route(matches, handler);
 try {
  const log = page.locator('#chat-log'), bar = page.locator('#chat-log > .chat-older');
  const user = n => page.locator('#chat-log .msg.user').filter({hasText:new RegExp(`^第 ${n} 个问题$`)});
  const top = locator => locator.evaluate(e => e.getBoundingClientRect().top);
  await page.reload();
  await user(24).waitFor();
  assert.equal(await page.locator('#chat-log .msg.user').count(), 12, 'first paint holds only the newest page');
  assert.equal(await bar.innerText(), '加载更早的对话');
  assert.ok(await log.evaluate(e => e.scrollHeight - e.scrollTop - e.clientHeight < 2), 'newest page opens at the bottom');
  assert.equal(requests.length, 1);

  // Scrolling up requests the page before; the reading position stays put.
  let release; hold = new Promise(r => { release = r; });
  await log.evaluate(e => { e.scrollTop = 0; });
  await bar.filter({hasText:'正在加载更早的对话…'}).waitFor();
  await mkdir(resolve('output/playwright'), {recursive:true});
  await page.screenshot({path:resolve('output/playwright/chat-pages-loading.png')});
  assert.deepEqual(requests.at(-1), {thread:thread.id,before:String(split)});
  const anchor = await top(user(13));
  release(); hold = null;
  await user(6).waitFor();
  assert.ok(Math.abs(await top(user(13)) - anchor) <= 1, 'prepending must not move the visible messages');
  assert.equal(await bar.innerText(), '加载更早的对话');
  const joined = await page.evaluate(() => {
   const answer = text => [...document.querySelectorAll('#chat-log .msg.agent')].find(n => n.textContent.startsWith(text));
   const first = answer('回答 12.0'), last = answer('回答 12.5'), turn = first?.closest('.turn');
   return {same: !!turn && turn === last?.closest('.turn'), ordered: !!first && !!(first.compareDocumentPosition(last) & Node.DOCUMENT_POSITION_FOLLOWING),
    footers: turn?.querySelectorAll('.answer-footer').length, results: turn?.querySelectorAll('.chip.result').length,
    turns: document.querySelectorAll('#chat-log > .turn').length};
  });
  assert.deepEqual(joined, {same:true,ordered:true,footers:1,results:1,turns:19}, 'a split turn renders as one turn with one footer');

  // A failed page offers a retry and loads on demand.
  failNext = true;
  await log.evaluate(e => { e.scrollTop = 0; });
  await page.locator('#chat-log > .chat-older.error').waitFor();
  assert.match(await bar.innerText(), /更早的对话加载失败\s*重试/);
  await page.screenshot({path:resolve('output/playwright/chat-pages-error.png')});
  const visible = await top(user(6));
  await bar.getByRole('button', {name:'重试'}).click();
  await user(0).waitFor();
  assert.equal(await bar.count(), 0, 'no loader once the thread start is shown');
  assert.ok(Math.abs(await top(user(6)) - visible) <= 1, 'removing the loader must not move the view');
  assert.equal(await page.locator('#chat-log .msg.user').count(), 25);
  assert.equal(await page.locator('#chat-log > .turn').count(), 25);

  // A reconnect while reading older messages re-reads from the oldest loaded
  // page instead of dropping it, and keeps the reading position.
  await user(10).evaluate(e => { e.scrollIntoView({block:'center'}); e.dataset.before = '1'; });
  const reading = await top(user(10));
  const sent = requests.length;
  closeChat();
  await page.waitForFunction(() => [...document.querySelectorAll('#chat-log .msg.user')].some(n => n.textContent === '第 10 个问题' && !n.dataset.before), null, {timeout:10000});
  assert.deepEqual(requests.slice(sent), [{thread:thread.id,from:'0'}]);
  assert.equal(await page.locator('#chat-log .msg.user').count(), 25);
  assert.ok(Math.abs(await top(user(10)) - reading) <= 1, 'reconnect keeps the reading position');
  console.log('Chat pages: newest page first, older pages on scroll without moving the view, split turn joined, retry, reconnect keeps loaded pages passed');
 } finally {
  await page.unroute(matches, handler);
 }
}
