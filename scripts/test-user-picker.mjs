import assert from 'node:assert/strict';

// Picking users instead of typing names, on the synthetic API: the account
// access dialog, the add-account dialog and the decision settings card all
// list /users with checkboxes. Administrators are listed as always allowed and
// stay out of the saved list; a saved name whose user was deleted is shown and
// dropped on save; an empty selection asks for "administrators only" instead.
export async function userPickerSmoke(page, base, baseSettings) {
  const user = (name, role = 'user') => ({ name, role, sessions: 0, created_at: Date.now(), quota: { metered: false } });
  const users = [user('fixture', 'admin'), user('carol'), user('alice'), user('bob')];
  const account = { id: 'acl', type: 'claude', label: 'ACL fixture', sessions: 0, cred_status: 'ok', auth_mode: 'oauth', access: { mode: 'users', users: ['alice', 'ghost'] } };
  const accounts = [account];
  const patches = [], creates = [], puts = [];
  let decision = { revision: 3, default_provider: 'builtin', fallback_policy: 'workspace_default', plugins: { jev: {
    gateway: 'typesafe', enabled: false, model: 'jev-1.13.0', timeout_ms: 1500, max_request_bytes: 32768, max_concurrency: 2,
    access: { mode: 'users', users: ['bob'] }, key_configured: false } } };
  const routes = {
    '**/api/users': route => route.fulfill({ json: users }),
    '**/api/accounts': route => {
      if (route.request().method() === 'POST') {
        const body = route.request().postDataJSON(); creates.push(body);
        const created = { ...body, cred_status: 'missing', sessions: 0 }; accounts.push(created);
        return route.fulfill({ json: created });
      }
      return route.fulfill({ json: accounts });
    },
    '**/api/accounts/acl': route => {
      assert.equal(route.request().method(), 'PATCH');
      const body = route.request().postDataJSON(); patches.push(body);
      Object.assign(account, body);
      return route.fulfill({ json: account });
    },
    '**/api/settings': route => {
      if (route.request().method() === 'PUT') {
        const body = route.request().postDataJSON(); puts.push(body);
        const jev = body.decision?.plugins?.jev;
        if (jev) decision = { ...decision, revision: decision.revision + 1, plugins: { jev: { ...decision.plugins.jev, access: jev.access } } };
      }
      return route.fulfill({ json: { ...baseSettings(), decision } });
    },
  };
  for (const [pattern, handler] of Object.entries(routes)) await page.route(pattern, handler);
  const shot = async name => { if (process.env.AGENTBOX_USER_PICKER_SHOTS) await page.screenshot({ path: `${process.env.AGENTBOX_USER_PICKER_SHOTS}/user-picker-${name}.png`, animations: 'disabled' }); };
  const names = picker => picker.locator('.up-row').evaluateAll(rows => rows.map(r => [r.dataset.name, r.querySelector('input').checked, r.querySelector('input').disabled, r.querySelector('.up-tag')?.textContent || '']));
  try {
    await page.goto(base + '/#/settings/accounts');
    await page.reload();

    // Existing account: listed users, admin locked on, deleted name flagged.
    await page.locator('.acct-row').filter({ hasText: 'ACL fixture' }).locator('.more-btn').click();
    await page.getByRole('menuitem', { name: '使用范围 · 指定用户', exact: true }).click();
    const dialog = page.locator('dialog[open]').filter({ hasText: '使用范围' });
    const picker = dialog.locator('.user-picker');
    await picker.locator('.up-row[data-name="bob"]').waitFor();
    assert.deepEqual(await names(picker), [
      ['alice', true, false, ''], ['bob', false, false, ''], ['carol', false, false, ''],
      ['fixture', true, true, '管理员 · 始终可用'], ['ghost', false, true, '用户不存在，保存时移除'],
    ]);
    assert.equal(await picker.locator('.up-count').innerText(), '已选 1 人');
    assert.equal(await picker.locator('.up-search').isVisible(), false, 'search shown for a short list');
    assert.equal(await dialog.locator('textarea').count(), 0, 'free-text usernames remain');
    await picker.getByRole('checkbox', { name: 'bob' }).check();
    assert.equal(await picker.locator('.up-count').innerText(), '已选 2 人');
    await shot('access-dialog');
    await dialog.getByRole('button', { name: '保存', exact: true }).click();
    await dialog.waitFor({ state: 'detached' });
    assert.deepEqual(patches.at(-1), { access: { mode: 'users', users: ['alice', 'bob'] } }, 'deleted name kept or admin added');

    // Select all toggles every listed user; nobody selected asks for admin-only.
    await page.locator('.acct-row').filter({ hasText: 'ACL fixture' }).locator('.more-btn').click();
    await page.getByRole('menuitem', { name: '使用范围 · 指定用户', exact: true }).click();
    await picker.locator('.up-row[data-name="carol"]').waitFor();
    const all = picker.locator('.up-all input');
    assert.equal(await all.evaluate(el => el.indeterminate), true);
    await all.check();
    assert.equal(await picker.locator('.up-count').innerText(), '已选 3 人');
    await all.uncheck();
    assert.equal(await picker.locator('.up-count').innerText(), '已选 0 人');
    await dialog.getByRole('button', { name: '保存', exact: true }).click();
    await picker.locator('.up-error').filter({ hasText: '请至少选择一个用户，或改为「仅管理员」' }).waitFor();
    assert.equal(patches.length, 1, 'empty selection was saved');
    await dialog.locator('label').filter({ hasText: '使用范围' }).getByRole('combobox').click();
    await page.getByRole('option', { name: '仅管理员', exact: true }).click();
    assert.equal(await picker.isVisible(), false);
    await dialog.getByRole('button', { name: '保存', exact: true }).click();
    await dialog.waitFor({ state: 'detached' });
    assert.deepEqual(patches.at(-1), { access: { mode: 'admin' } });

    // Add-account dialog: the same picker, sent with the create request.
    await page.locator('#btn-acct-add').click();
    assert.equal(await page.locator('#acct-users-row').isVisible(), false);
    await page.locator('#acct-form label').filter({ hasText: '使用范围' }).getByRole('combobox').click();
    await page.getByRole('option', { name: '指定用户', exact: true }).click();
    const create = page.locator('#acct-users');
    await create.locator('.up-row[data-name="carol"]').waitFor();
    await page.locator('#acct-id').fill('picked-users');
    await page.locator('#auth-gen').click();
    await create.locator('.up-error').filter({ hasText: '请至少选择一个用户' }).waitFor();
    assert.equal(creates.length, 0, 'created with nobody selected');
    await create.getByRole('checkbox', { name: 'carol' }).check();
    await shot('add-account');
    await page.locator('#auth-gen').click();
    for (const until = Date.now() + 5000; !creates.length && Date.now() < until;) await page.waitForTimeout(50);
    assert.deepEqual(creates[0].access, { mode: 'users', users: ['carol'] });
    await page.locator('#auth-close').click();

    // Decision settings: picking a user lights the save bar; discard restores ticks.
    await page.locator('#set-nav [data-sec="decision"]').click();
    const decisionPicker = page.locator('#set-decision-users-picker');
    await decisionPicker.locator('.up-row[data-name="bob"]').waitFor();
    assert.equal(await decisionPicker.getByRole('checkbox', { name: 'bob' }).isChecked(), true);
    assert.equal(await page.locator('#set-savebar').isVisible(), false);
    await decisionPicker.getByRole('checkbox', { name: 'carol' }).check();
    await page.locator('#set-savebar').waitFor();
    await page.locator('#set-discard').click();
    assert.equal(await decisionPicker.getByRole('checkbox', { name: 'carol' }).isChecked(), false, 'discard kept the tick');
    assert.equal(await page.locator('#set-savebar').isVisible(), false);
    await decisionPicker.getByRole('checkbox', { name: 'carol' }).check();
    await shot('decision');
    await page.locator('#set-save').click();
    for (const until = Date.now() + 5000; !puts.length && Date.now() < until;) await page.waitForTimeout(50);
    assert.deepEqual(puts[0].decision.plugins.jev.access, { mode: 'users', users: ['bob', 'carol'] });
    await page.locator('#set-savebar').waitFor({ state: 'hidden' });
    // Nobody selected: the patch is refused before any request.
    await decisionPicker.locator('.up-all input').check();
    await decisionPicker.locator('.up-all input').uncheck();
    await page.locator('#set-save').click();
    await decisionPicker.locator('.up-error').filter({ hasText: '请至少选择一个用户' }).waitFor();
    assert.equal(puts.length, 1);
    await page.locator('#set-discard').click();
    // Other modes hide the list.
    await page.locator('label').filter({ hasText: '允许使用的用户' }).getByRole('combobox').click();
    await page.getByRole('option', { name: '所有用户', exact: true }).click();
    assert.equal(await page.locator('#set-decision-users-row').isVisible(), false);
    await page.locator('#set-discard').click();
    assert.equal(await page.locator('#set-decision-users-row').isVisible(), true);

    // Narrow screens: the dialog list fits without horizontal scrolling.
    await page.setViewportSize({ width: 390, height: 844 });
    // Longer lists get a search box; select all then applies to the matches only.
    account.access = { mode: 'users', users: ['alice'] };
    users.push(user('dave'), user('erin'), user('frank'));
    await page.goto(base + '/#/settings/accounts');
    await page.reload();
    await page.locator('.acct-row').filter({ hasText: 'ACL fixture' }).locator('.more-btn').click();
    await page.getByRole('menuitem', { name: '使用范围 · 指定用户', exact: true }).click();
    await picker.locator('.up-row[data-name="frank"]').waitFor();
    assert.equal(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth), true, 'access dialog overflows on mobile');
    await shot('access-mobile');
    await picker.locator('.up-search').fill('FR');
    assert.deepEqual(await picker.locator('.up-row:visible').evaluateAll(rows => rows.map(r => r.dataset.name)), ['frank']);
    await picker.locator('.up-all input').check();
    await picker.locator('.up-search').press('Enter'); // filters only; must not submit the dialog
    await picker.locator('.up-search').fill('zz');
    await picker.locator('.up-empty-match').waitFor();
    await picker.locator('.up-search').fill('');
    assert.equal(await picker.locator('.up-count').innerText(), '已选 2 人');
    assert.equal(patches.length, 2, 'Enter in the search box submitted the dialog');
    await dialog.getByRole('button', { name: '取消', exact: true }).click();
    await page.setViewportSize({ width: 1280, height: 900 });
    console.log('User picker: account access, add-account and decision settings select users from the list; admin/deleted rows, select all, empty-selection guard, search, save bar and narrow layout passed');
  } finally {
    for (const [pattern, handler] of Object.entries(routes)) await page.unroute(pattern, handler);
  }
}
