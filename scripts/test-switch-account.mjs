import assert from 'node:assert/strict';

// Workspace account switching on the synthetic API: only other accounts of the
// workspace's agent are offered, the request carries the chosen ID, a workspace
// that was running is started again on the new account, server refusals keep
// the dialog open with their message, and no candidate disables the action.
export async function switchAccountSmoke(page, base) {
  const accounts = [
    { id: 'fixture', type: 'codex', label: 'Fixture', sessions: 1, cred_status: 'ok' },
    { id: 'fixture-b', type: 'codex', label: 'Fixture B', sessions: 0, cred_status: 'ok' },
    { id: 'claude-only', type: 'claude', label: 'Claude only', sessions: 0, cred_status: 'ok' },
  ];
  let session = { id: 'fixture-space', name: 'Fixture workspace', agent: 'codex', account_id: 'fixture', account_label: 'Fixture', status: 'running', default_model: 'fixture' };
  const switches = []; let starts = 0, refuse = false, offered = accounts;
  const routes = {
    '**/api/accounts': route => route.fulfill({ json: offered }),
    '**/api/sessions': route => route.fulfill({ json: [session] }),
    '**/api/sessions/fixture-space/account': async route => {
      assert.equal(route.request().method(), 'PUT');
      switches.push(route.request().postDataJSON());
      if (refuse) {
        await route.fulfill({ status: 409, json: { error: '上一条消息仍在处理中', code: 'chat_busy', operation_id: 'f'.repeat(32), hint: '请等待当前回合完成，或先中断当前回合。', action: 'wait', retryable: true } });
        return;
      }
      const next = accounts.find(a => a.id === route.request().postDataJSON().account_id);
      session = { ...session, account_id: next.id, account_label: next.label, status: 'stopped' };
      await route.fulfill({ json: session });
    },
    '**/api/sessions/fixture-space/start': route => { starts++; session = { ...session, status: 'running' }; return route.fulfill({ json: session }); },
  };
  for (const [pattern, handler] of Object.entries(routes)) await page.route(pattern, handler);
  const dialog = page.locator('#dlg-switch-acct');
  const open = async () => {
    await page.locator('#btn-wb-more').click();
    await page.getByRole('menuitem', { name: '切换账号…', exact: true }).click();
    await dialog.waitFor({ state: 'visible' });
  };
  try {
    await page.goto(base + '/#/sessions/fixture-space/chat');
    await page.reload(); // a hash-only navigation keeps the earlier session list
    await page.locator('#wb-state.run').waitFor();

    // Refusal (a turn is running): message shown, nothing changes, dialog stays.
    refuse = true;
    await open();
    assert.equal(await page.locator('#switch-acct-current').innerText(), '当前账号：Fixture');
    assert.deepEqual(await page.locator('#switch-acct-select option').allInnerTexts(), ['Fixture B'], 'only other accounts of the same agent');
    await page.locator('#switch-acct-ok').click();
    await page.locator('#switch-acct-error').filter({ hasText: '切换失败' }).waitFor();
    assert.ok(await dialog.isVisible());
    assert.equal(starts, 0);
    await page.locator('#switch-acct-cancel').click();
    await dialog.waitFor({ state: 'hidden' });

    // Success: chosen ID sent, header shows the new account, running workspace restarted.
    refuse = false;
    await open();
    assert.equal(await page.locator('#switch-acct-error').isVisible(), false, 'previous error cleared on reopen');
    await page.locator('#switch-acct-ok').click();
    await dialog.waitFor({ state: 'hidden' });
    await page.locator('#wb-meta').filter({ hasText: 'Fixture B' }).waitFor();
    assert.deepEqual(switches.at(-1), { account_id: 'fixture-b' });
    await page.waitForFunction(() => !document.querySelector('#btn-stop').classList.contains('hidden'));
    assert.equal(starts, 1, 'a running workspace starts again on the new account');

    // No other account of this agent: explain and disable the action.
    offered = accounts.filter(a => a.id !== 'fixture');
    await open();
    await page.locator('#switch-acct-empty').filter({ hasText: '没有其他可用的 Codex CLI 账号' }).waitFor();
    assert.equal(await page.locator('#switch-acct-field').isVisible(), false);
    assert.equal(await page.locator('#switch-acct-ok').isDisabled(), true);
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });
    assert.equal(switches.length, 2);
    console.log('Switch account: same-agent candidates, refusal message, rebinding with restart and empty state passed');
  } finally {
    for (const [pattern, handler] of Object.entries(routes)) await page.unroute(pattern, handler);
  }
}
