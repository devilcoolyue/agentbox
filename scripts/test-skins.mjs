import assert from 'node:assert/strict';

// Interface style (<html data-skin>) regression on the synthetic browser suite:
// picker, switching, independence from light/dark, first-paint restore, fallback,
// and that every style × theme defines its own tokens instead of leaking amber.
const SKINS = ['amber', 'glass', 'cyberpunk', 'graphite', 'verdant', 'blueprint'];
const LABELS = ['琥珀', '液态玻璃', '赛博朋克', '石墨极简', '青野绿意', '工程蓝图'];
// Distinctive per-style tokens; each must differ from amber in the same theme.
const DISTINCT = ['--bg', '--panel-2', '--amber', '--line', '--muted', '--code-bg'];

export async function skinsSmoke(page, base) {
  const rootData = name => page.evaluate(name => document.documentElement.dataset[name], name);
  const tokens = names => page.evaluate(names => {
    const style = getComputedStyle(document.documentElement);
    return Object.fromEntries(names.map(name => [name, style.getPropertyValue(name).trim()]));
  }, names);
  const ready = async () => { await page.locator('#app').waitFor({ state: 'visible' }); };

  await page.evaluate(() => localStorage.removeItem('agentbox_skin'));
  await page.goto(base + '/#/');
  await page.reload();
  await ready();
  assert.equal(await rootData('skin'), 'amber', 'default style');
  assert.equal((await tokens(['--radius-scale']))['--radius-scale'], '1');

  await page.locator('#btn-user-menu').click();
  const userMenuOpen = () => page.locator('#sidebar-user-menu').evaluate(el => el.classList.contains('open'));
  const picker = page.locator('#pref-skin'), trigger = picker.locator('.pref-trigger'), menu = picker.locator('.pref-menu');
  // One row like balance and latency: the title on the left, the current style and a right arrow on the right.
  assert.equal(await picker.locator('.pref-picker-title').innerText(), '风格');
  assert.equal((await trigger.innerText()).trim(), '琥珀');
  assert.equal(await trigger.locator('.ui-icon').getAttribute('data-icon-name'), 'chevron-right');
  assert.ok(await picker.evaluate(el => {
    const t = el.querySelector('.pref-picker-title').getBoundingClientRect(), b = el.querySelector('.pref-trigger').getBoundingClientRect();
    return Math.abs((t.top + t.bottom) / 2 - (b.top + b.bottom) / 2) < 2 && b.left > t.right && el.getBoundingClientRect().height < 44;
  }), 'title and choice share one row');
  assert.equal(await trigger.getAttribute('aria-expanded'), 'false');
  await trigger.click();
  assert.equal(await trigger.getAttribute('aria-expanded'), 'true');
  const options = menu.locator('.pref-menu-item');
  assert.deepEqual((await options.allInnerTexts()).map(t => t.trim()), LABELS);
  assert.deepEqual(await options.evaluateAll(rows => rows.map(row => row.dataset.value)), SKINS);
  assert.equal(await menu.locator('.pref-menu-item.active').getAttribute('data-value'), 'amber');
  assert.ok(await page.evaluate(() => {
    const m = document.querySelector('#pref-skin .pref-menu').getBoundingClientRect(), r = document.querySelector('#pref-skin .pref-trigger').getBoundingClientRect();
    return m.bottom <= r.top && m.top >= 0;
  }), 'the full list opens above its row');
  await options.filter({ hasText: '赛博朋克' }).click();
  assert.equal(await rootData('skin'), 'cyberpunk');
  assert.equal(await page.evaluate(() => localStorage.getItem('agentbox_skin')), 'cyberpunk');
  assert.equal(await menu.isHidden(), true, 'choosing closes the style menu');
  assert.equal(await userMenuOpen(), true, 'choosing keeps the user menu');
  assert.equal((await trigger.innerText()).trim(), '赛博朋克');
  assert.equal(await picker.locator('.pref-menu-item.active').getAttribute('aria-checked'), 'true');
  // Sign out sits on the right, in line with the values above it.
  assert.ok(await page.evaluate(() => {
    const b = document.querySelector('#btn-logout'), r = b.getBoundingClientRect(), text = b.lastElementChild.getBoundingClientRect();
    return r.right - text.right < 12 && b.firstElementChild.getBoundingClientRect().left - r.left > 40;
  }), 'sign out is right aligned');
  assert.equal(await page.locator('.btn').first().evaluate(el => getComputedStyle(el).borderTopLeftRadius), '0px', 'sharp style squares controls');
  const { '--bg': bg } = await tokens(['--bg']);
  assert.equal(await page.locator('meta[name="theme-color"]').getAttribute('content'), bg, 'address bar follows the style');
  await page.keyboard.press('Escape');
  assert.equal(await userMenuOpen(), false);

  // Mouse: hovering the arrow opens its menu and turns the arrow up; leaving the menu
  // closes only that menu, and a choice made there keeps the user menu open.
  if (await page.evaluate(() => matchMedia('(hover: hover) and (pointer: fine)').matches)) {
    const lang = page.locator('#pref-language'), langTrigger = lang.locator('.pref-trigger'), langMenu = lang.locator('.pref-menu');
    const userMenuClosed = () => page.waitForFunction(() => !document.querySelector('#sidebar-user-menu').classList.contains('open'));
    // Hover alone: moving away closes the user menu as before.
    await page.locator('#btn-user-menu').hover();
    await page.locator('#sidebar-user-menu.open').waitFor();
    await page.mouse.move(1250, 880);
    await userMenuClosed();
    await page.locator('#btn-user-menu').hover();
    await page.locator('#sidebar-user-menu.open').waitFor();
    await langTrigger.hover();
    await langMenu.waitFor({ state: 'visible' });
    await page.waitForFunction(() => /^matrix\([-\d.e]+, -1, 1,/.test(getComputedStyle(document.querySelector('#pref-language .pref-trigger svg')).transform), null, { timeout: 2000 });
    assert.deepEqual((await langMenu.locator('.pref-menu-item').allInnerTexts()).map(t => t.trim()), ['跟随系统', '简体中文', '繁體中文', 'English']);
    // Leaving through the top of the list, past the user menu's edge, closes only the list.
    const first = await langMenu.locator('.pref-menu-item').first().boundingBox();
    await page.mouse.move(first.x + first.width / 2, first.y + first.height / 2, { steps: 8 });
    assert.equal(await langMenu.isVisible(), true, 'crossing the gap keeps the menu');
    const above = await page.evaluate(() => Math.min(...['#pref-language .pref-menu', '#sidebar-user-menu'].map(q => document.querySelector(q).getBoundingClientRect().top)) - 12);
    await page.mouse.move(first.x + first.width / 2, above, { steps: 4 });
    await langMenu.waitFor({ state: 'hidden' });
    await page.waitForTimeout(300);
    assert.equal(await userMenuOpen(), true, 'leaving the language menu keeps the user menu');
    await langTrigger.hover();
    await langMenu.waitFor({ state: 'visible' });
    const box = await langMenu.locator('[data-value="zh-TW"]').boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 8 });
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    assert.equal(await page.evaluate(() => document.documentElement.lang), 'zh-TW');
    assert.equal(await userMenuOpen(), true, 'a hover choice keeps the user menu');
    assert.equal((await langTrigger.innerText()).trim(), '繁體中文');
    await langTrigger.hover();
    await langMenu.locator('[data-value="zh-CN"]').click();
    assert.equal(await page.evaluate(() => document.documentElement.lang), 'zh-CN');
    await trigger.hover();
    await menu.waitFor({ state: 'visible' });
    assert.equal(await langMenu.isHidden(), true, 'one list at a time');
    await page.locator('#btn-logout').hover();
    await menu.waitFor({ state: 'hidden' });
    assert.equal(await userMenuOpen(), true, 'leaving the style menu keeps the user menu');
    // After a choice the user menu stays until Escape or a click elsewhere, in every engine.
    await page.mouse.move(1250, 880);
    await page.waitForTimeout(400);
    assert.equal(await userMenuOpen(), true, 'a used user menu does not close on leave');
    await page.mouse.click(1250, 880);
    await userMenuClosed();
  }

  // Keyboard: Enter opens and focuses the chosen style; the first Escape closes only the style menu.
  await page.locator('#btn-user-menu').click();
  await trigger.focus();
  await page.keyboard.press('Enter');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.value), 'cyberpunk');
  await page.keyboard.press('ArrowDown');
  assert.equal(await page.evaluate(() => document.activeElement.dataset.value), 'graphite');
  await page.keyboard.press('Escape');
  assert.equal(await menu.isHidden(), true);
  assert.equal(await page.evaluate(() => document.activeElement.classList.contains('pref-trigger')), true, 'focus returns to the row');
  assert.equal(await userMenuOpen(), true, 'first Escape keeps the user menu');
  await page.keyboard.press('Escape');
  assert.equal(await userMenuOpen(), false);

  // Touch has no hover: a tap on the row's choice toggles the list, a tap elsewhere in the user
  // menu closes only the list. Synthetic touch pointers keep this engine-neutral.
  await page.locator('#btn-user-menu').click();
  const tap = selector => page.evaluate(selector => {
    const el = document.querySelector(selector);
    for (const type of ['pointerdown', 'pointerup']) el.dispatchEvent(new PointerEvent(type, { bubbles: true, pointerType: 'touch', isPrimary: true }));
    el.dispatchEvent(new MouseEvent('click', { bubbles: true, detail: 1 }));
  }, selector);
  await tap('#pref-skin .pref-trigger');
  assert.equal(await menu.isVisible(), true);
  assert.equal(await page.evaluate(() => document.activeElement?.closest('.pref-menu')), null, 'a tap does not move focus into the menu');
  await tap('#pref-skin .pref-trigger');
  assert.equal(await menu.isHidden(), true, 'a second tap closes it');
  await tap('#pref-skin .pref-trigger');
  await tap('#sidebar-user-name');
  assert.equal(await menu.isHidden(), true, 'tapping elsewhere closes the style menu');
  assert.equal(await userMenuOpen(), true);
  await page.keyboard.press('Escape');

  // Light/dark stays an independent choice.
  const mode = await rootData('themeMode');
  await page.locator('.theme-options [data-theme-option="light"]').click();
  assert.equal(await rootData('theme'), 'light');
  assert.equal(await rootData('skin'), 'cyberpunk');
  await page.locator(`.theme-options [data-theme-option="${mode}"]`).click();

  // The head script restores the style before modules run.
  await page.reload();
  assert.equal(await rootData('skin'), 'cyberpunk', 'first paint restores the saved style');
  await ready();

  // Every style × theme defines a complete set: nothing falls back to amber.
  // Layers stacked over content (menus, dialogs, sticky headers) must stay opaque,
  // or text underneath shows through translucent styles such as glass.
  const solidAlpha = () => page.evaluate(() => {
    const probe = document.body.appendChild(document.createElement('div'));
    probe.style.background = 'var(--panel-solid)';
    const color = getComputedStyle(probe).backgroundColor;
    probe.remove();
    const parts = color.match(/[\d.]+/g) || [];
    return parts.length > 3 ? Number(parts[3]) : 1;
  });
  for (const theme of ['dark', 'light']) {
    await page.evaluate(theme => { document.documentElement.dataset.theme = theme; document.documentElement.dataset.skin = 'amber'; }, theme);
    const amber = await tokens(DISTINCT);
    assert.equal(await solidAlpha(), 1, `amber/${theme} --panel-solid is translucent`);
    for (const skin of SKINS.slice(1)) {
      await page.evaluate(skin => { document.documentElement.dataset.skin = skin; }, skin);
      const values = await tokens(DISTINCT);
      for (const name of DISTINCT) assert.notEqual(values[name], amber[name], `${skin}/${theme} ${name} leaks amber`);
      assert.equal(await solidAlpha(), 1, `${skin}/${theme} --panel-solid is translucent`);
    }
  }
  // The glass user menu used to let the drawer's links show through it.
  await page.evaluate(() => { document.documentElement.dataset.skin = 'glass'; });
  assert.equal(await page.locator('#sidebar-user-menu').evaluate(el => getComputedStyle(el).backgroundColor.match(/[\d.]+/g).length), 3, 'glass user menu is opaque');

  await page.evaluate(() => localStorage.setItem('agentbox_skin', 'unknown'));
  await page.reload();
  assert.equal(await rootData('skin'), 'amber', 'unknown saved style falls back to amber');
  await page.evaluate(() => localStorage.removeItem('agentbox_skin'));
  await page.reload();
  await ready();
  console.log('Skins: one-row picker with hover/keyboard/touch list, right-aligned sign out, switching, light/dark independence, first-paint restore, complete token sets, opaque overlays and fallback passed');
}
