import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { resolve } from 'node:path';

// Workbench top bar folding (features/workspaces/head-fold.ts): a running workspace with a
// conversation folds the title row and tabs away; hovering or clicking the workspace name in
// the thread bar floats them over the log without moving it; the pin keeps them shown and is
// remembered per browser. Every tab folds, with the name in its first toolbar. Stopped
// workspaces, the empty chat greeting and phones keep the bar. No provider calls.
export async function headFoldSmoke(page, { setHistory, setStatus }) {
 const history = [];
 for (let i = 0; i < 6; i++) {
  const ts = new Date(Date.UTC(2026, 9, 28, 1, i)).toISOString();
  history.push({kind:'user',ts,text:`收起顶部栏问题 ${i}`});
  history.push({kind:'event',ts,event:{type:'item.completed',item:{type:'agent_message',text:`回答 ${i}。`+'用于撑开对话高度的合成回答。'.repeat(12)}}});
  history.push({kind:'status',ts,state:'idle'});
 }
 const workbench = page.locator('#workbench'), peek = page.locator('#wb-peek'), fold = page.locator('#btn-wb-fold');
 const topHeight = () => page.locator('#wb-top').evaluate(e => e.getBoundingClientRect().height);
 const logTop = () => page.locator('#chat-log').evaluate(e => Math.round(e.getBoundingClientRect().top));
 const has = (cls, on) => page.waitForFunction(([cls, on]) => document.getElementById('workbench').classList.contains(cls) === on, [cls, on]);
 const open = async () => { await page.goto(page.url().replace(/#.*$/, '') + '#/sessions/fixture-space/chat'); await page.reload(); await page.locator('#chat-log .msg.user').last().waitFor(); };
 await mkdir(resolve('output/playwright'), {recursive:true});
 try {
  await page.setViewportSize({width:1280,height:800});
  await page.evaluate(() => localStorage.removeItem('agentbox_head_fold'));
  setHistory(history); setStatus('running');
  await open();

  // Running + conversation: the bar is folded, the thread bar carries the workspace name.
  await has('head-folded', true);
  assert.ok(await topHeight() < 1, 'folded top bar takes no height');
  assert.equal(await page.locator('#wb-top .wb-top-in').evaluate(e => e.inert), true, 'folded bar is out of the tab order');
  await peek.waitFor({state:'visible'});
  assert.equal(await page.locator('#wb-peek-name').textContent(), 'Fixture workspace');
  assert.equal(await peek.getAttribute('aria-expanded'), 'false');
  const foldedTop = await logTop();
  await page.screenshot({path:'output/playwright/head-fold-folded.png'});

  // Hover floats the bar over the log; the conversation underneath does not move.
  await peek.hover();
  await has('head-peek', true);
  assert.ok(await topHeight() > 60, 'peek shows title row and tabs');
  assert.equal(await logTop(), foldedTop, 'the overlay must not push the conversation');
  assert.equal(await peek.getAttribute('aria-expanded'), 'true');
  assert.equal(await page.locator('#wb-top .wb-top-in').evaluate(e => e.inert), false);
  await page.locator('#btn-stop').waitFor({state:'visible'});
  assert.equal(await fold.getAttribute('aria-label'), '固定显示顶部栏');
  await page.screenshot({path:'output/playwright/head-fold-peek.png'});
  await page.locator('.tab[data-tab="files"]').hover(); // moving into the overlay keeps it open
  await page.waitForTimeout(400);
  assert.equal(await workbench.evaluate(e => e.classList.contains('head-peek')), true);
  await page.locator('#chat-input').hover();
  await has('head-peek', false);

  // Keyboard: Enter opens and moves into the tabs, Escape closes back onto the name.
  await peek.focus();
  await page.keyboard.press('Enter');
  await has('head-peek', true);
  assert.equal(await page.evaluate(() => document.activeElement?.matches('.tab.active')), true);
  await page.keyboard.press('Escape');
  await has('head-peek', false);
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'wb-peek');

  // Every tab folds the same way: a tab chosen from the overlay closes it, the name moves into that
  // tab's first toolbar (its left group where the toolbar is justified) and the content gets the room.
  const hosts = {term:'#tab-term > .term-bar', browser:'#tab-browser > .browser-toolbar', files:'#tab-files .files-left', changes:'#tab-changes > .changes-bar', chat:'#tab-chat > .thread-bar'};
  for (const [tab, host] of Object.entries(hosts)) {
   await peek.click();
   await has('head-peek', true);
   await page.locator(`.tab[data-tab="${tab}"]`).click();
   await page.locator(`#tab-${tab}`).waitFor({state:'visible'});
   await has('head-peek', false);
   await has('head-folded', true);
   await page.locator(`${host} > #wb-peek`).waitFor({state:'visible'});
   assert.ok(await topHeight() < 1, `${tab}: folded top bar takes no height`);
   assert.equal(await page.locator('.wb-peek-host').count(), 1, `${tab}: only the current toolbar makes room`);
   const [p, h] = await Promise.all([peek.boundingBox(), page.locator(host).boundingBox()]);
   assert.ok(p.y >= h.y - 1 && p.y + p.height <= h.y + h.height + 1, `${tab}: the name sits inside the toolbar row`);
   if (tab !== 'chat') await page.screenshot({path:`output/playwright/head-fold-${tab}.png`});
  }
  // Hover works away from the chat too, and the overlay covers the toolbar instead of moving it.
  await page.locator('.tab[data-tab="files"]').evaluate(e => e.click());
  await page.locator('#tab-files #wb-peek').waitFor({state:'visible'});
  const barTop = () => page.locator('#tab-files > .files-bar').evaluate(e => Math.round(e.getBoundingClientRect().top));
  const filesTop = await barTop();
  await peek.hover();
  await has('head-peek', true);
  assert.equal(await barTop(), filesTop);
  await fold.waitFor({state:'visible'});
  await page.mouse.move(640, 600);
  await has('head-peek', false);
  await page.locator('.tab[data-tab="chat"]').evaluate(e => e.click());
  await page.locator('#tab-chat > .thread-bar > #wb-peek').waitFor({state:'visible'});

  // Pin from the overlay: stays shown, remembered across reloads; the same button folds again.
  await peek.click();
  await has('head-peek', true);
  await fold.click();
  await has('head-folded', false);
  assert.equal(await page.evaluate(() => localStorage.getItem('agentbox_head_fold')), 'off');
  assert.ok(await logTop() > foldedTop + 60, 'pinned bar takes its place above the conversation');
  await open();
  await page.waitForTimeout(100);
  assert.equal(await workbench.evaluate(e => e.classList.contains('head-folded')), false, 'pin survives a reload');
  assert.equal(await fold.getAttribute('aria-label'), '收起顶部栏');
  await page.screenshot({path:'output/playwright/head-fold-pinned.png'});
  await fold.click();
  await has('head-folded', true);
  assert.equal(await page.evaluate(() => localStorage.getItem('agentbox_head_fold')), null);

  // Phones keep their own layout: no workspace name in the thread bar, tabs stay in place.
  await page.setViewportSize({width:390,height:800});
  assert.equal(await peek.isVisible(), false);
  assert.ok(await page.locator('.tabs').evaluate(e => e.getBoundingClientRect().height) > 30);
  await page.setViewportSize({width:1280,height:800});

  // Stopped: the start button is the point of the bar, keep it.
  setStatus('stopped');
  await open();
  await page.locator('#btn-start').waitFor({state:'visible'});
  assert.equal(await workbench.evaluate(e => e.classList.contains('head-folded')), false);

  // The empty greeting has room to spare.
  setStatus('running'); setHistory([]);
  await page.reload();
  await page.locator('#chat-hero').waitFor({state:'visible'});
  assert.equal(await workbench.evaluate(e => e.classList.contains('head-folded')), false);
 } finally {
  setHistory([]); setStatus('stopped');
  await page.evaluate(() => localStorage.removeItem('agentbox_head_fold')).catch(() => {});
  await page.setViewportSize({width:1280,height:900});
 }
 console.log('Head fold: running conversation folds the top bar, hover/click/keyboard peek over the log, every tab folds with the name in its toolbar, remembered pin, phone, stopped and empty states passed');
}
