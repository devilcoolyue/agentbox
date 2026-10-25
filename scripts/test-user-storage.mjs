import assert from 'node:assert/strict';

// Per-user storage limits on the synthetic API: with storage quotas enabled the
// add-user form offers optional data/cache limits (defaults as placeholders,
// budget below), each row shows data usage and a 存储 action whose dialog edits
// both limits, keeps server rejections in the dialog and blank means default.
// With quotas off none of it is shown and the create request carries no limits.
export async function userStorageSmoke(page, base) {
  const GB = 1 << 30;
  const user = (name, role, storage) => ({ name, role, sessions: 0, created_at: Date.now(), quota: { metered: false }, storage });
  let enabled = true;
  const users = () => [
    user('fixture', 'admin', enabled ? { data_bytes: 7 * GB, cache_bytes: 3 * GB, data_custom: true, cache_custom: true, data_used: 6330781696, cache_used: 3 * GB } : undefined),
    user('alice', 'user', enabled ? { data_bytes: GB, cache_bytes: GB / 4, data_custom: false, cache_custom: false, data_used: 200 << 20 } : undefined),
  ];
  const summary = () => enabled
    ? { enabled: true, cache_enabled: true, default_data_bytes: GB, default_cache_bytes: GB / 4, budget_bytes: 16 * GB, allocated_bytes: 11.25 * GB }
    : { enabled: false, cache_enabled: false, default_data_bytes: 0, default_cache_bytes: 0, budget_bytes: 0, allocated_bytes: 0 };
  const creates = [], puts = [];
  const routes = {
    '**/api/users': route => {
      if (route.request().method() === 'POST') {
        creates.push(route.request().postDataJSON());
        return route.fulfill({ status: 201, json: user(creates.at(-1).username, 'user') });
      }
      return route.fulfill({ json: users() });
    },
    '**/api/storage/quotas': route => route.fulfill({ json: summary() }),
    '**/api/users/alice/storage': route => {
      const body = route.request().postDataJSON();
      puts.push(body);
      if (body.data_bytes && body.data_bytes < 200 << 20) return route.fulfill({ status: 409, json: { error: '数据上限不能低于已用的 200 MB' } });
      return route.fulfill({ json: users()[1].storage });
    },
  };
  for (const [pattern, handler] of Object.entries(routes)) await page.route(pattern, handler);
  const shot = async name => { if (process.env.AGENTBOX_USER_STORAGE_SHOTS) await page.screenshot({ path: `${process.env.AGENTBOX_USER_STORAGE_SHOTS}/user-storage-${name}.png`, animations: 'disabled' }); };
  const waitFor = async (list, n) => { for (const until = Date.now() + 5000; list.length < n && Date.now() < until;) await page.waitForTimeout(50); assert.equal(list.length, n); };
  try {
    await page.goto(base + '/#/settings/security');
    await page.reload();
    const row = name => page.locator('#user-list .user-row').filter({ has: page.locator('.u-name', { hasText: name }) });
    await row('alice').locator('.u-storage').waitFor();
    assert.equal(await row('fixture').locator('.u-storage').innerText(), '5.9 GB / 7.0 GB');
    assert.equal(await row('fixture').locator('.u-storage').evaluate(el => el.classList.contains('hot')), true, 'full cache not flagged');
    assert.equal(await row('alice').locator('.u-storage').innerText(), '200 MB / 1.0 GB');

    // Add form: placeholders carry the defaults, the budget sits below.
    const data = page.locator('#user-new-data'), cache = page.locator('#user-new-cache');
    assert.equal(await data.getAttribute('placeholder'), '数据上限 GB（默认 1）');
    assert.equal(await cache.getAttribute('placeholder'), '缓存上限 GB（默认 0.25）');
    assert.equal(await page.locator('#user-storage-note').innerText(), '存储预算：已分配 11 GB / 16 GB，剩余 4.8 GB');
    await shot('list');
    await page.locator('#user-new-name').fill('bob');
    await page.locator('#user-new-pass').fill('long-enough');
    await data.fill('abc');
    await page.locator('#btn-user-add').click();
    await page.locator('.toast').filter({ hasText: '存储上限请填 GB 数' }).waitFor();
    assert.equal(creates.length, 0, 'invalid limit was sent');
    await data.fill('2.5');
    await page.locator('#btn-user-add').click();
    await waitFor(creates, 1);
    assert.deepEqual(creates[0], { username: 'bob', password: 'long-enough', data_bytes: 2.5 * GB, cache_bytes: 0 });
    for (const until = Date.now() + 5000; await page.locator('#user-new-name').inputValue() && Date.now() < until;) await page.waitForTimeout(50);
    assert.equal(await data.inputValue(), '', 'limit kept after create');

    // Editor: blank fields follow the defaults; rejections stay in the dialog.
    await row('alice').getByRole('button', { name: '调整用户存储上限', exact: true }).click();
    const dialog = page.locator('#dlg-user-storage');
    await dialog.waitFor();
    assert.equal(await dialog.locator('#us-user').innerText(), 'alice');
    assert.equal(await dialog.locator('#us-data').inputValue(), '');
    assert.equal(await dialog.locator('#us-data').getAttribute('placeholder'), '默认 1');
    assert.equal(await dialog.locator('#us-data-used').innerText(), '已用 200 MB');
    assert.equal(await dialog.locator('#us-cache-used').innerText(), '');
    assert.match(await dialog.locator('#us-budget').innerText(), /该用户最多可设 6\.0 GB（数据\+缓存）$/);
    await dialog.locator('#us-data').fill('0.1');
    await dialog.getByRole('button', { name: '保存', exact: true }).click();
    await dialog.locator('#us-error').filter({ hasText: '数据上限不能低于已用的 200 MB' }).waitFor();
    assert.equal(await dialog.isVisible(), true);
    await shot('dialog-error');
    await dialog.locator('#us-data').fill('3');
    await dialog.locator('#us-cache').fill('0.5');
    await dialog.locator('#us-cache').press('Enter');
    await dialog.waitFor({ state: 'hidden' });
    assert.deepEqual(puts.at(-1), { data_bytes: 3 * GB, cache_bytes: GB / 2 });

    // Narrow screens: the extra chip and action still fit.
    await page.setViewportSize({ width: 390, height: 844 });
    const overflow = await page.locator('#user-list').evaluate(el => el.scrollWidth - el.clientWidth);
    assert.ok(overflow <= 1, `user list overflows by ${overflow}px`);
    await shot('narrow');
    await page.setViewportSize({ width: 1280, height: 900 });

    // Quotas off: no inputs, chips or action; create sends no limits.
    enabled = false;
    await page.reload();
    await row('alice').waitFor();
    assert.equal(await page.locator('#user-list .u-storage').count(), 0);
    assert.equal(await row('alice').getByRole('button', { name: '调整用户存储上限', exact: true }).count(), 0);
    assert.equal(await data.isVisible(), false);
    assert.equal(await page.locator('#user-add-storage').isVisible(), false);
    await page.locator('#user-new-name').fill('carol');
    await page.locator('#user-new-pass').fill('long-enough');
    await page.locator('#btn-user-add').click();
    await waitFor(creates, 2);
    assert.deepEqual(creates[1], { username: 'carol', password: 'long-enough' });
    console.log('User storage: add-user limits with defaults and budget, row usage chips, limit editor with server rejection, narrow layout and quota-off fallback passed');
  } finally {
    for (const [pattern, handler] of Object.entries(routes)) await page.unroute(pattern, handler);
  }
}
