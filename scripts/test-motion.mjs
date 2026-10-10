// Motion regression on the synthetic console, run from test-browser.mjs with motion enabled.
// The other browser regressions emulate prefers-reduced-motion (playwright-launch.mjs), so this
// is the one place the transitions themselves run. It checks behaviour, not timing curves:
// entries and exits start, hosts settle where the selected item is, closing elements stop taking
// input and leave, focus returns, list motion does not replay on repaint, and reduced motion is
// instant. Exit transitions on the top layer need `overlay`; engines without it close at once.
import assert from 'node:assert/strict';

const settle = page => page.evaluate(() => Promise.all(document.getAnimations().filter(a => a.playState === 'running' && a.effect?.getComputedTiming().iterations !== Infinity).map(a => a.finished.catch(() => {}))));

export async function motionSmoke(page, fixture) {
 await page.emulateMedia({reducedMotion: 'no-preference'});
 try {
  assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--dur').trim()), '180ms');
  const overlay = await page.evaluate(() => CSS.supports('overlay', 'auto'));

  // Tab switch: the new pane fades in from @starting-style and the underline slides to the tab.
  const tabSwitch = await page.evaluate(async () => {
   const ink = document.querySelector('.tabs > .ink');
   document.querySelector('.tab[data-tab="chat"]').click();
   const pane = document.querySelector('#tab-chat').getAnimations().map(a => a.transitionProperty).sort();
   await Promise.resolve(); // ink.ts follows .active from a MutationObserver (a microtask)
   return {ready: document.querySelector('.tabs').hasAttribute('data-ink-ready'), pane, ink: ink.getAnimations().map(a => a.transitionProperty)};
  });
  assert.equal(tabSwitch.ready, true);
  assert.deepEqual(tabSwitch.pane, ['opacity', 'translate']);
  assert.ok(tabSwitch.ink.includes('transform'), 'tab underline did not slide: ' + tabSwitch.ink);
  await settle(page);
  const inkBox = await page.evaluate(() => {
   const ink = document.querySelector('.tabs > .ink').getBoundingClientRect(), tab = document.querySelector('.tab.active').getBoundingClientRect();
   return {dx: Math.abs(ink.left - tab.left), dw: Math.abs(ink.width - tab.width), bottom: Math.abs(ink.bottom - tab.bottom)};
  });
  assert.ok(inkBox.dx < 1 && inkBox.dw < 1 && inkBox.bottom < 1, 'tab underline settled away from the tab: ' + JSON.stringify(inkBox));
  // The terminal pane only fades: a translate during xterm measurement would offset it. Shown
  // directly so the terminal socket is not opened ahead of the terminal regression.
  assert.deepEqual(await page.evaluate(() => {
   const pane = document.querySelector('#tab-term');
   pane.classList.remove('hidden');
   const props = pane.getAnimations().map(a => a.transitionProperty);
   pane.classList.add('hidden');
   return props;
  }), ['opacity']);
  await page.locator('.tab[data-tab="files"]').click();
  await settle(page);
  // A resize places the underline directly. Sliding from the old geometry, which can lie outside a
  // narrowed, horizontally scrollable tab bar, toggles its scrollbar mid-slide; measuring inside the
  // ResizeObserver callback made WebKit report a ResizeObserver loop.
  const desktop = page.viewportSize();
  await page.setViewportSize({width: 390, height: 844});
  const snapped = await page.evaluate(() => new Promise(done => {
   const frames = n => n ? requestAnimationFrame(() => frames(n - 1)) : (() => {
    const ink = document.querySelector('.tabs > .ink'), a = ink.getBoundingClientRect(), b = document.querySelector('.tab.active').getBoundingClientRect();
    done({running: ink.getAnimations().length, dx: Math.round(Math.abs(a.left - b.left)), dw: Math.round(Math.abs(a.width - b.width))});
   })();
   frames(4);
  }));
  await page.setViewportSize(desktop);
  assert.deepEqual(snapped, {running: 0, dx: 0, dw: 0}, 'resize should place the tab underline without sliding');

  // The ⋯ menu slides in; a closing menu stays inert until it fades out, and focus returns.
  await page.locator('#btn-wb-more').click();
  assert.ok(await page.locator('.menu-pop').evaluate(el => el.getAnimations().length > 0), 'menu entered without motion');
  // Escape and the check share one task so a slow runner cannot finish the exit in between.
  const closingMenu = await page.evaluate(() => {
   window.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
   return {menus: [...document.querySelectorAll('.menu-pop')].map(el => el.inert), focus: document.activeElement?.id};
  });
  assert.deepEqual(closingMenu, {menus: [true], focus: 'btn-wb-more'});
  await page.locator('#btn-wb-more').click();
  assert.equal(await page.locator('.menu-pop:not([inert])').count(), 1, 'reopened menu mixed with the closing one');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('.menu-pop'));

  // Dialogs fade and scale in; with overlay support they also fade out without blocking clicks.
  await page.locator('#btn-wb-more').focus();
  const confirmed = page.evaluate(async () => (await import('/_v/{{BUILD}}/js/util.js')).askConfirm('motion fixture'));
  const dialog = page.locator('#dlg-ask');
  await dialog.waitFor({state: 'visible'});
  assert.ok((await dialog.evaluate(el => el.getAnimations().map(a => a.transitionProperty))).includes('opacity'), 'dialog entered without motion');
  await settle(page);
  const closing = await dialog.evaluate(el => {
    // offset* ignore the exit transform, so any change here is a relayout rather than the scale-down.
    const layout = () => [el.offsetHeight, ...[...el.children].map(c => c.offsetWidth)].join();
    const before = layout();
    el.close();
    return {open: el.open, display: getComputedStyle(el).display, events: getComputedStyle(el).pointerEvents, before, after: layout()};
  });
  assert.equal(await confirmed, false);
  assert.equal(closing.open, false);
  if (overlay) {
    assert.deepEqual([closing.display !== 'none', closing.events], [true, 'none'], 'dialog exit should stay visible but inert');
    assert.equal(closing.after, closing.before, 'dialog content reflowed during the exit transition');
  } else assert.equal(closing.display, 'none');
  await page.waitForFunction(() => getComputedStyle(document.querySelector('#dlg-ask')).display === 'none');
  assert.equal(await page.evaluate(() => document.activeElement?.id), 'btn-wb-more', 'focus did not return after the dialog closed');

  // First directory read shows skeleton rows that are replaced by the result.
  const skeleton = await page.evaluate(async () => {
   const {loadFiles} = await import('/_v/{{BUILD}}/js/files.js');
   const read = loadFiles();
   const shown = {rows: document.querySelectorAll('#files-list .skeleton-row').length, label: document.querySelector('#files-list [role=status]')?.getAttribute('aria-label')};
   await read;
   return {...shown, left: document.querySelectorAll('#files-list .skeleton-row').length};
  });
  assert.deepEqual(skeleton, {rows: 6, label: '读取目录中…', left: 0});

  // Home cards enter one after another; a repaint resumes the same animations instead of replaying,
  // and a workspace that appears later flashes in the sidebar while known ones do not.
  const original = fixture.sessions();
  fixture.setSessions([...original,
   {...original[0], id: 'motion-a', name: 'Motion A', updated_at: '2026-10-09T08:00:00Z'},
   {...original[0], id: 'motion-b', name: 'Motion B', updated_at: '2026-10-09T07:00:00Z'}]);
  const flashed = await page.evaluate(async () => {
   await (await import('/_v/{{BUILD}}/js/data.js')).refreshAll();
   return [...document.querySelectorAll('.session-card.flash')].map(el => el.dataset.sessionId).sort();
  });
  assert.deepEqual(flashed, ['motion-a', 'motion-b']);
  await page.locator('#btn-home').click();
  await page.locator('#home-list .home-card').nth(2).waitFor();
  const cards = await page.evaluate(async () => {
   const starts = () => [...document.querySelectorAll('#home-list .home-card')].map(el => el.getAnimations().find(a => a.animationName === 'list-enter')?.startTime ?? null);
   const before = starts();
   (await import('/_v/{{BUILD}}/js/state.js')).emit('data-updated');
   return {before, after: starts()};
  });
  assert.equal(cards.before.length, 3);
  assert.ok(cards.before.every(t => typeof t === 'number'), 'home cards did not enter: ' + JSON.stringify(cards.before));
  assert.deepEqual(cards.after, cards.before, 'repaint restarted the card entry');
  const gaps = cards.before.slice(1).map((t, i) => Math.round(t - cards.before[i]));
  assert.ok(gaps.every(g => g === 30), 'cards were not staggered: ' + gaps);
  await settle(page);
  const replayed = await page.evaluate(async () => {
   (await import('/_v/{{BUILD}}/js/state.js')).emit('data-updated');
   return [...document.querySelectorAll('#home-list .home-card')].flatMap(el => el.getAnimations()).filter(a => a.playState === 'running').length;
  });
  assert.equal(replayed, 0, 'finished entries replayed on poll');

  // Reduced motion: tokens are zero, menus leave synchronously, nothing is staggered.
  await page.emulateMedia({reducedMotion: 'reduce'});
  assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--dur').trim()), '0s');
  await page.evaluate(async () => (await import('/_v/{{BUILD}}/js/home.js')).renderHome());
  assert.equal(await page.locator('#home-list .enter').count(), 0);
  fixture.setSessions(original);
  await page.evaluate(async () => (await import('/_v/{{BUILD}}/js/data.js')).refreshAll());
  await page.goBack();
  await page.locator('#tab-files').waitFor({state: 'visible'});
  await page.locator('#btn-wb-more').click();
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('.menu-pop').count(), 0, 'reduced motion kept the closed menu');
  console.log(`Motion: tab underline slide and resize placement, pane entry, menu exit/focus, dialog entry${overlay ? '/exit' : ''}, skeleton rows, staggered home cards without replay, new-workspace flash, reduced motion passed`);
 } finally {
  await page.emulateMedia({reducedMotion: 'reduce'});
 }
}
