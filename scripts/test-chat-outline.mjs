import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { resolve } from 'node:path';
import { emulateMotion } from './playwright-launch.mjs';

// Conversation rail (features/chat/outline.ts) on a synthetic paged thread:
// one tick per user message including pages not loaded yet, dock-style lift
// that only changes length (colour: hovered and current tick only), the
// question/answer tip, jumps to loaded and unloaded turns, keyboard, a live
// message, and the phone layout (right edge, drag to preview, outline sheet).
// No provider calls.
export async function chatOutlineSmoke(page, { session, send }) {
 const thread = {id:'thread-outline',title:'Outline thread',ts:'2026-10-02T00:00:00Z',updated:'2026-10-02T01:00:00Z',turns:30,resumable:true};
 const N = 30, PAGE = 10;
 const all = [], starts = [];
 const ts = i => new Date(Date.UTC(2026, 9, 2, 0, i)).toISOString();
 for (let i = 0; i < N; i++) {
  starts.push(all.length);
  all.push({kind:'user',ts:ts(i),text:`第 ${i} 个问题`,turn:{id:'outline-'+i,model:'fixture',effort:'',control:'effort'}});
  all.push({kind:'event',ts:ts(i),event:{type:'assistant',message:{content:[{type:'text',text:`回答 ${i}。`+'这是一段用于撑开高度的合成回答。'.repeat(14)}]}}});
  all.push({kind:'event',ts:ts(i),event:{type:'result',subtype:'success',result:`回答 ${i}。后面还有别的句子。`,duration_ms:1000}});
  all.push({kind:'status',ts:ts(i),state:'idle'});
 }
 const items = () => starts.map((line, i) => ({line, ts:ts(i), text:all[line].text, answer:`回答 ${i}。后面还有别的句子。`}));
 const newest = starts[N - PAGE];
 const requests = [];
 const history = async route => {
  const q = new URL(route.request().url()).searchParams;
  requests.push(Object.fromEntries(q));
  if (q.has('before')) {
   const end = Number(q.get('before')), start = Math.max(0, end - PAGE * 4);
   await route.fulfill({json:{entries:all.slice(start, end),thread_id:thread.id,costs:{},start,has_more:start > 0}});
   return;
  }
  const from = q.get('thread') === thread.id && q.has('from') ? Number(q.get('from')) : newest;
  await route.fulfill({json:{entries:all.slice(from),thread,active_thread:thread.id,costs:{},start:from,has_more:from > 0}});
 };
 let outlineCalls = 0;
 const outline = async route => { outlineCalls++; await route.fulfill({json:{thread_id:thread.id,items:items()}}); };
 const isHistory = url => url.pathname === `/api/sessions/${session}/history`;
 const isOutline = url => url.pathname === `/api/sessions/${session}/outline`;
 await page.route(isHistory, history);
 await page.route(isOutline, outline);
 await mkdir(resolve('output/playwright'), {recursive:true});
 try {
  await page.setViewportSize({width:1280,height:900});
  const log = page.locator('#chat-log'), rail = page.locator('#tab-chat > .outline-rail'), tip = page.locator('#tab-chat > .outline-tip');
  const user = n => page.locator('#chat-log .msg.user').filter({hasText:new RegExp(`^第 ${n} 个问题$`)});
  const tick = i => rail.locator('.outline-tick').nth(i);
  const ticks = () => page.evaluate(() => [...document.querySelectorAll('#tab-chat > .outline-rail .outline-tick')].map(t => {
   const line = t.firstElementChild, m = /scaleX\(([\d.]+)\)/.exec(line.style.transform);
   return {scale: m ? Number(m[1]) : NaN, opacity: Number(getComputedStyle(line).opacity), hot: t.classList.contains('hot'), active: t.classList.contains('active')};
  }));
  const center = async locator => { const b = await locator.boundingBox(); return {x: b.x + b.width / 2, y: b.y + b.height / 2}; };
  // where a message sits in the log, as a fraction of the visible height
  const place = n => user(n).evaluate(e => { const l = document.getElementById('chat-log').getBoundingClientRect(); return (e.getBoundingClientRect().top - l.top) / l.height; });
  const settle = () => page.waitForTimeout(450); // longest transition on the rail

  await page.reload();
  await user(N - 1).waitFor();
  await rail.waitFor({state:'visible'});
  assert.equal(await rail.locator('.outline-tick').count(), N, 'one tick per message, including pages not loaded yet');
  assert.equal(await page.locator('#chat-log .msg.user').count(), PAGE);
  assert.equal(await rail.getAttribute('aria-label'), '对话导航');
  await settle();
  let state = await ticks();
  assert.ok(state[N - 1].active && state.filter(t => t.active).length === 1, 'at the bottom the newest tick is current');
  assert.ok(state[N - 1].scale > state[0].scale, 'the current tick is longer than the rest');

  // Hover: the tick under the pointer and its neighbours lift; only the hovered
  // and the current tick change colour. With reduced motion only the hovered one lifts.
  const hovered = 25;
  const at = await center(tick(hovered));
  await page.mouse.move(at.x, at.y - 60);
  await page.mouse.move(at.x, at.y, {steps:6});
  await tip.and(page.locator('.show')).waitFor();
  state = await ticks();
  assert.deepEqual(state.map((t, i) => i === hovered || i === N - 1 || t.scale), state.map((t, i) => i === hovered || i === N - 1 || 0.357), 'reduced motion: no lift around the pointer');
  await emulateMotion(page, 'no-preference');
  await page.mouse.move(at.x, at.y + 1);
  await page.mouse.move(at.x, at.y);
  await settle();
  state = await ticks();
  assert.equal(state[hovered].hot, true);
  assert.equal(state.filter(t => t.hot).length, 1);
  assert.ok(Math.abs(state[hovered].scale - 1) < 0.05, 'hovered tick at full length');
  assert.ok(state[hovered - 1].scale > state[hovered - 3].scale && state[hovered - 3].scale > state[0].scale, 'neighbours taper off: '+JSON.stringify(state.map(t => t.scale)));
  for (const [i, t] of state.entries()) {
   const bright = i === hovered || i === N - 1;
   assert.equal(t.opacity, bright ? 1 : 0.32, `tick ${i} colour: only hovered and current are bright`);
  }
  assert.equal(await tip.locator('.outline-tip-q').innerText(), `第 ${hovered} 个问题`);
  assert.equal(await tip.locator('.outline-tip-a').innerText(), `回答 ${hovered}。`, 'answer is the first sentence');
  const tipBox = await tip.boundingBox(), railBox = await rail.boundingBox();
  assert.ok(tipBox.x > railBox.x + 20, 'desktop tip opens to the right of the rail');
  await page.screenshot({path:resolve('output/playwright/chat-outline-hover.png')});

  // Click: jumps to a loaded turn, which becomes current, and flashes it.
  await page.evaluate(() => {
   window.__outlineFlashed = [];
   new MutationObserver(list => { for (const m of list) if (m.target.classList.contains('outline-flash')) window.__outlineFlashed.push(m.target.textContent); })
    .observe(document.getElementById('chat-log'), {subtree:true, attributes:true, attributeFilter:['class']});
  });
  await page.mouse.down(); await page.mouse.up();
  await page.waitForFunction(n => {
   const e = [...document.querySelectorAll('#chat-log .msg.user')].find(m => m.textContent === `第 ${n} 个问题`), l = document.getElementById('chat-log');
   return e && Math.abs(e.getBoundingClientRect().top - l.getBoundingClientRect().top - l.clientHeight * 0.18) < 6;
  }, hovered);
  assert.ok((await page.evaluate(() => window.__outlineFlashed)).includes(`第 ${hovered} 个问题`), 'the target message flashes');
  await emulateMotion(page, 'reduce');
  await page.mouse.move(at.x + 400, at.y);
  await settle();
  state = await ticks();
  assert.ok(state[hovered].active && !state.some(t => t.hot), 'jumped turn is current once the pointer leaves');

  // Click a turn that is not loaded: history reloads from its line, then it is shown.
  const far = 3;
  assert.equal(await user(far).count(), 0);
  const sent = requests.length;
  const b = await center(tick(far));
  await page.mouse.move(b.x, b.y, {steps:4});
  await page.mouse.down(); await page.mouse.up();
  await user(far).waitFor();
  assert.deepEqual(requests[sent], {thread:thread.id, from:String(starts[far - 1])}, 'reload starts one message above the target');
  await page.waitForFunction(() => !document.querySelector('#tab-chat > .outline-rail .outline-tick.loading'));
  assert.ok(Math.abs(await place(far) - 0.18) < 0.02, 'unloaded target lands where loaded ones do');
  await page.mouse.move(b.x + 400, b.y);
  await settle();
  assert.ok((await ticks())[far].active, 'unloaded target becomes current');

  // Keyboard: Tab into the rail, arrows move, Enter jumps.
  await tick(far).focus();
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowDown');
  assert.equal(await tip.locator('.outline-tip-q').innerText(), `第 ${far + 2} 个问题`, 'focus shows the tip');
  await page.keyboard.press('Enter');
  await page.waitForFunction(n => {
   const e = [...document.querySelectorAll('#chat-log .msg.user')].find(m => m.textContent === `第 ${n} 个问题`), l = document.getElementById('chat-log');
   return e && Math.abs(e.getBoundingClientRect().top - l.getBoundingClientRect().top - l.clientHeight * 0.18) < 6;
  }, far + 2);
  await page.keyboard.press('Escape');

  // Scrolling by hand moves the current tick with the reading position.
  await user(20).evaluate(e => { const l = document.getElementById('chat-log'); l.dispatchEvent(new WheelEvent('wheel')); l.scrollTop = e.parentElement.offsetTop - 40; });
  await page.waitForFunction(() => document.querySelectorAll('#tab-chat > .outline-rail .outline-tick')[20]?.classList.contains('active'));

  // A message sent now gets its tick at once; the answer arrives with the next outline read.
  const calls = outlineCalls;
  send({type:'user_message', ts:ts(N), text:'刚发出的问题', turn:{id:'outline-live',model:'fixture',effort:'',control:'effort'}});
  await page.locator('#chat-log .msg.user').filter({hasText:'刚发出的问题'}).waitFor();
  await page.waitForFunction(n => document.querySelectorAll('#tab-chat > .outline-rail .outline-tick').length === n, N + 1);
  starts.push(all.length);
  all.push({kind:'user',ts:ts(N),text:'刚发出的问题'});
  send({type:'status', state:'idle'});
  await page.waitForFunction(() => true);
  await page.waitForTimeout(900);
  assert.ok(outlineCalls > calls, 'the outline is read again once the turn ends');
  assert.equal(await rail.locator('.outline-tick').count(), N + 1);

  // Phone: the rail moves to the right edge and shows while scrolling.
  await page.setViewportSize({width:390,height:844});
  await page.waitForFunction(() => document.querySelector('#tab-chat > .outline-rail')?.classList.contains('mobile'));
  const toc = page.locator('.thread-bar .outline-toc');
  await toc.waitFor({state:'visible'});
  assert.equal(await toc.getAttribute('aria-label'), '对话目录');
  // resizing scrolls the log too; once it rests the rail fades out and lets touches through
  await page.waitForFunction(() => { const r = document.querySelector('#tab-chat > .outline-rail'); return getComputedStyle(r).opacity === '0' && getComputedStyle(r).pointerEvents === 'none'; }, null, {timeout:4000});
  // a scroll the page makes itself (streaming keeps the log at the bottom) does not show it
  await log.evaluate(e => { e.scrollTop = e.scrollHeight / 2; });
  await page.waitForTimeout(150);
  assert.equal(await rail.evaluate(e => e.classList.contains('awake')), false, 'programmatic scroll leaves the rail asleep');
  await log.evaluate(e => { e.dispatchEvent(new WheelEvent('wheel')); e.scrollTop -= 300; });
  await page.waitForFunction(() => document.querySelector('#tab-chat > .outline-rail')?.classList.contains('awake'));
  const mrail = await rail.boundingBox();
  assert.ok(mrail.x + mrail.width >= 389 && mrail.x > 300, 'phone rail sits on the right edge');

  // Press and drag: the tick under the finger lifts and the tip follows to its left.
  const from = await center(tick(N - 2)), to = await center(tick(12));
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  await page.mouse.move(to.x, to.y, {steps:12});
  await tip.and(page.locator('.show')).waitFor();
  assert.equal(await tip.locator('.outline-tip-q').innerText(), '第 12 个问题');
  const mtip = await tip.boundingBox();
  assert.ok(mtip.x + mtip.width <= mrail.x + 2 && mtip.y + mtip.height <= to.y, 'tip sits above-left of the finger');
  await page.screenshot({path:resolve('output/playwright/chat-outline-mobile-scrub.png')});
  await page.mouse.up();
  await user(12).waitFor();
  await page.waitForFunction(() => {
   const e = [...document.querySelectorAll('#chat-log .msg.user')].find(m => m.textContent === '第 12 个问题'), l = document.getElementById('chat-log');
   return e && Math.abs(e.getBoundingClientRect().top - l.getBoundingClientRect().top - l.clientHeight * 0.18) < 6;
  });

  // Dragging far to the left before letting go cancels.
  const keep = await log.evaluate(e => e.scrollTop);
  await log.evaluate(e => { e.dispatchEvent(new WheelEvent('wheel')); e.scrollTop += 1; });
  await page.waitForFunction(() => document.querySelector('#tab-chat > .outline-rail')?.classList.contains('awake'));
  const c1 = await center(tick(5));
  await page.mouse.move(c1.x, c1.y); await page.mouse.down();
  await page.mouse.move(c1.x, c1.y + 30, {steps:4});
  await page.mouse.move(c1.x - 150, c1.y + 30, {steps:4});
  assert.match(await tip.getAttribute('class'), /cancel/);
  await page.mouse.up();
  await page.waitForTimeout(300);
  assert.ok(Math.abs(await log.evaluate(e => e.scrollTop) - keep - 1) < 2, 'cancelled drag does not jump');

  // The outline button lists every turn; the current one is marked, a tap jumps.
  await toc.click();
  const sheet = page.locator('dialog.dlg-outline');
  await sheet.waitFor({state:'visible'});
  assert.equal(await sheet.locator('.outline-item').count(), N + 1);
  assert.equal(await sheet.locator('.outline-count').innerText(), `共 ${N + 1} 轮`);
  assert.match(await sheet.locator('.outline-item.current .outline-item-q').innerText(), /^第 1[12] 个问题$/);
  assert.equal(await sheet.locator('.outline-item').nth(7).locator('.outline-item-a').innerText(), '回答 7。');
  const sb = await sheet.boundingBox();
  assert.ok(sb.y + sb.height >= 843 && sb.width >= 389, 'phone outline is a bottom sheet');
  await page.screenshot({path:resolve('output/playwright/chat-outline-mobile-sheet.png')});
  await sheet.locator('.outline-item').nth(7).click();
  await sheet.waitFor({state:'hidden'});
  await page.waitForFunction(() => {
   const e = [...document.querySelectorAll('#chat-log .msg.user')].find(m => m.textContent === '第 7 个问题'), l = document.getElementById('chat-log');
   return e && Math.abs(e.getBoundingClientRect().top - l.getBoundingClientRect().top - l.clientHeight * 0.18) < 6;
  });

  // Other languages label the rail and sheet.
  await page.evaluate(async () => (await import('/_v/{{BUILD}}/js/i18n.js')).i18n.setLanguage('en'));
  assert.equal(await rail.getAttribute('aria-label'), 'Conversation navigation');
  assert.equal(await toc.getAttribute('aria-label'), 'Conversation outline');
  await page.evaluate(async () => (await import('/_v/{{BUILD}}/js/i18n.js')).i18n.setLanguage('zh-CN'));
  await page.setViewportSize({width:1280,height:900});
  console.log('Chat outline: ticks for unloaded pages, dock lift with two-tone colour, tip, jumps (loaded/unloaded/keyboard), live message, phone edge rail, drag/cancel and outline sheet passed');
 } finally {
  await page.unroute(isHistory, history);
  await page.unroute(isOutline, outline);
 }
}
