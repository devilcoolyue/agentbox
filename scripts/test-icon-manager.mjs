import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { assertActionIcons } from './test-actions.mjs';

// Icon manager on the synthetic browser suite (called by test-browser.mjs): pick an icon for a
// place from the vendored library, live preview across the page, a semantic slot repainting every
// button that uses it, the saved payload, first paint from the cache after reload, the by-icon
// view assigning one icon to more places, and resetting everything. The /api/icons fixture only
// echoes and resolves shapes from the same vendored files; Go tests cover the server rules.
async function library() {
  const shapes = {};
  for (const set of ['tabler', 'tabler-filled', 'lucide']) {
    const doc = JSON.parse(await readFile(new URL(`../internal/web/static/vendor/icons/${set}.json`, import.meta.url), 'utf8'));
    for (const [name, nodes] of Object.entries(doc.icons)) shapes[set + ':' + name] = nodes;
  }
  return shapes;
}

export async function iconManagerSmoke(page) {
  const shapes = await library();
  const state = { overrides: {}, puts: [], hold: null };
  const reply = () => ({ overrides: state.overrides, icons: Object.fromEntries(Object.values(state.overrides).map(r => [r, shapes[r]])),
    sets: ['tabler', 'tabler-filled', 'lucide'], limit: 500, updated_at: 0, updated_by: 'root' });
  await page.route('**/api/icons', async route => {
    const request = route.request();
    await state.hold;
    if (request.method() === 'PUT') {
      assert.ok(request.headers().authorization?.startsWith('Bearer '), 'icon writes use the Authorization header');
      const { overrides } = JSON.parse(request.postData());
      for (const ref of Object.values(overrides)) assert.ok(shapes[ref], 'saved icon exists in the library: ' + ref);
      state.puts.push(overrides);
      state.overrides = overrides;
    }
    await route.fulfill({ json: reply() });
  });
  const ref = selector => page.locator(selector).first().getAttribute('data-icon-ref');
  const slotRefs = slot => page.locator(`svg.ui-icon[data-icon-slot="${slot}"]`).evaluateAll(list => [...new Set(list.map(s => s.dataset.iconRef))]);

  await page.evaluate(() => { location.hash = '#/settings/icons'; });
  await page.locator('#sec-icons').waitFor({ state: 'visible' });
  const usageRow = page.locator('#icons-list .icon-row[data-slot="nav.usage"]');
  await usageRow.waitFor();
  assert.equal(await ref('#btn-usagelog svg'), 'tabler:receipt', 'sidebar starts with the default');
  await assertActionIcons(page, '#sec-icons');

  // Pick from the library: search, choose, confirm — the page previews before saving.
  await usageRow.getByRole('button', { name: '更换' }).click();
  const picker = page.locator('#dlg-icon-picker');
  await picker.waitFor({ state: 'visible' });
  await page.locator('#icon-picker-filter').fill('receipt-2');
  await picker.locator('.ip-item[data-ref="tabler:receipt-2"]').click();
  await page.locator('#icon-picker-ok').click();
  await picker.waitFor({ state: 'hidden' });
  assert.equal(await ref('#btn-usagelog svg'), 'tabler:receipt-2', 'live preview on the sidebar');
  assert.ok(await page.locator('#icons-list .icon-row[data-slot="nav.usage"].changed').count());
  assert.equal(await page.locator('#icons-savebar').isVisible(), true);

  // A semantic slot repaints every button that references it, in another library.
  const refreshRow = page.locator('#icons-list .icon-row[data-slot="refresh"]');
  await refreshRow.getByRole('button', { name: '更换' }).click();
  await picker.locator('[data-set="lucide"]').click();
  await page.locator('#icon-picker-filter').fill('refresh-ccw');
  await picker.locator('.ip-item[data-ref="lucide:refresh-ccw"]').dblclick();
  await picker.waitFor({ state: 'hidden' });
  assert.deepEqual(await slotRefs('refresh'), ['lucide:refresh-ccw'], 'all refresh buttons follow the semantic slot');

  await page.locator('#icons-save').click();
  await page.locator('#icons-savebar').waitFor({ state: 'hidden' });
  assert.deepEqual(state.puts.at(-1), { 'nav.usage': 'tabler:receipt-2', refresh: 'lucide:refresh-ccw' });
  const cached = await page.evaluate(() => JSON.parse(localStorage.getItem('agentbox.icons') || '{}'));
  assert.equal(cached.overrides?.refresh, 'lucide:refresh-ccw');

  // First paint comes from the cache even while the server read is still pending.
  let release;
  state.hold = new Promise(r => { release = r; });
  await page.reload();
  await page.locator('#btn-usagelog svg').waitFor({ state: 'attached' });
  assert.equal(await ref('#btn-usagelog svg'), 'tabler:receipt-2', 'cached icons paint before /api/icons answers');
  release();
  state.hold = null;

  // By icon: one icon, several places.
  await page.evaluate(() => { location.hash = '#/settings/icons'; });
  await page.locator('#icons-list .icon-row').first().waitFor();
  await page.locator('#icons-by-icon').click();
  const card = page.locator('#icons-list .icon-card').filter({ hasText: 'tabler:receipt-2' });
  await card.getByRole('button', { name: '设置到哪些位置…' }).click();
  const slots = page.locator('#dlg-icon-slots');
  await slots.waitFor({ state: 'visible' });
  await page.locator('#icon-slots-filter').fill('nav.git');
  await slots.locator('.is-row').filter({ hasText: 'nav.git' }).locator('input').check();
  await page.locator('#icon-slots-ok').click();
  await slots.waitFor({ state: 'hidden' });
  assert.equal(await ref('#btn-git-management svg'), 'tabler:receipt-2');
  assert.match(await card.innerText(), /Git 管理/);

  // Reset everything and save: back to defaults, empty payload, cache cleared.
  await page.locator('#icons-reset-all').click();
  await page.locator('#ask-ok').click();
  await page.locator('#icons-save').click();
  await page.locator('#icons-savebar').waitFor({ state: 'hidden' });
  assert.deepEqual(state.puts.at(-1), {});
  assert.equal(await ref('#btn-usagelog svg'), 'tabler:receipt');
  assert.deepEqual(await slotRefs('refresh'), ['tabler:refresh']);
  assert.equal(await page.evaluate(() => localStorage.getItem('agentbox.icons')), null);

  // Unsaved previews never leak outside the manager.
  await page.locator('#icons-by-slot').click();
  await page.locator('#icons-list .icon-row[data-slot="nav.usage"]').getByRole('button', { name: '更换' }).click();
  await page.locator('#icon-picker-filter').fill('receipt-2');
  await picker.locator('.ip-item[data-ref="tabler:receipt-2"]').dblclick();
  assert.equal(await ref('#btn-usagelog svg'), 'tabler:receipt-2');
  await page.locator('#set-nav [data-sec="interface"]').click();
  assert.equal(await ref('#btn-usagelog svg'), 'tabler:receipt', 'leaving the section drops the preview');
  await page.locator('#set-nav [data-sec="icons"]').click();
  await page.locator('#icons-discard').click();
  await page.unroute('**/api/icons');
}
