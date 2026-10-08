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
  const trigger = page.locator('.skin-select.select-trigger');
  assert.equal(await trigger.getAttribute('data-value'), 'amber');
  await trigger.click();
  const options = page.locator('.select-panel-skin:popover-open .select-option');
  assert.deepEqual(await options.allInnerTexts(), LABELS);
  assert.deepEqual(await options.evaluateAll(rows => rows.map(row => row.dataset.value)), SKINS);
  await options.filter({ hasText: '赛博朋克' }).click();
  assert.equal(await rootData('skin'), 'cyberpunk');
  assert.equal(await page.evaluate(() => localStorage.getItem('agentbox_skin')), 'cyberpunk');
  assert.equal(await trigger.getAttribute('data-value'), 'cyberpunk');
  assert.equal(await page.locator('.btn').first().evaluate(el => getComputedStyle(el).borderTopLeftRadius), '0px', 'sharp style squares controls');
  const { '--bg': bg } = await tokens(['--bg']);
  assert.equal(await page.locator('meta[name="theme-color"]').getAttribute('content'), bg, 'address bar follows the style');
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
  for (const theme of ['dark', 'light']) {
    await page.evaluate(theme => { document.documentElement.dataset.theme = theme; document.documentElement.dataset.skin = 'amber'; }, theme);
    const amber = await tokens(DISTINCT);
    for (const skin of SKINS.slice(1)) {
      await page.evaluate(skin => { document.documentElement.dataset.skin = skin; }, skin);
      const values = await tokens(DISTINCT);
      for (const name of DISTINCT) assert.notEqual(values[name], amber[name], `${skin}/${theme} ${name} leaks amber`);
    }
  }

  await page.evaluate(() => localStorage.setItem('agentbox_skin', 'unknown'));
  await page.reload();
  assert.equal(await rootData('skin'), 'amber', 'unknown saved style falls back to amber');
  await page.evaluate(() => localStorage.removeItem('agentbox_skin'));
  await page.reload();
  await ready();
  console.log('Skins: six-style picker, switching, light/dark independence, first-paint restore, complete token sets and fallback passed');
}
