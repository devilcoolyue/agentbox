import assert from 'node:assert/strict';

// Cache keepalive on the synthetic API: the ⋯ entry exists for Claude
// workspaces, the dialog shows the workspace setting with the administrator's
// defaults, saving sends the workspace values (empty = default), plans list
// and stop one by one, the composer chip follows the state, and an account
// that writes five-minute caches explains why it cannot be kept.
export async function keepaliveSmoke(page, base) {
  const now = Date.now();
  const session = { id: 'keep-space', name: 'Keepalive workspace', agent: 'claude', account_id: 'sub', account_label: 'Subscription', status: 'running', default_model: 'claude-opus-5' };
  const plan = (id, extra) => ({ id, model: 'claude-opus-5', prompt_tokens: 152000, next_at: now + 40 * 60000, until: now + 3 * 3600000, runs: 1, max_runs: 3, keep_micro_usd: 30400, ...extra });
  const base_view = {
    available: true, enabled: false, terminal: false, max_hours: 0, min_context_tokens: 0,
    defaults: { max_hours: 3, min_context_tokens: 100000 }, effective: { max_hours: 3, min_context_tokens: 100000 },
    max_hours_limit: 6, interval_minutes: 50,
    chat: { prompt_tokens: 152000, ttl: '1h', at: now - 5 * 60000, keep_micro_usd: 30400, rewrite_micro_usd: 1216000 },
    plans: [],
  };
  let view = base_view;
  const puts = [], stops = [];
  const routes = {
    '**/api/sessions': route => route.fulfill({ json: [session] }),
    '**/api/sessions/keep-space/keepalive': async route => {
      if (route.request().method() === 'PUT') {
        const body = route.request().postDataJSON();
        puts.push(body);
        view = { ...view, enabled: body.enabled, terminal: body.terminal, max_hours: body.max_hours, min_context_tokens: body.min_context_tokens,
          effective: { max_hours: body.max_hours || 3, min_context_tokens: body.min_context_tokens || 100000 },
          plans: [plan('chat', { source: 'chat', thread: 't1', title: 'Refactor billing' }), plan('terminal:/workspace/app', { source: 'terminal', cwd: '/workspace/app', runs: 0 })] };
      }
      await route.fulfill({ json: view });
    },
    '**/api/sessions/keep-space/keepalive/stop': async route => {
      stops.push(route.request().postDataJSON());
      view = { ...view, plans: view.plans.filter(p => p.id !== route.request().postDataJSON().id) };
      await route.fulfill({ json: view });
    },
  };
  for (const [pattern, handler] of Object.entries(routes)) await page.route(pattern, handler);
  const dialog = page.locator('#dlg-keepalive');
  const open = async () => {
    await page.locator('#btn-wb-more').click();
    const item = page.getByRole('menuitem', { name: '缓存保活…', exact: true });
    assert.equal(await item.locator('.ui-icon').getAttribute('data-icon-name'), 'refresh');
    await item.click();
    await dialog.waitFor({ state: 'visible' });
  };
  try {
    await page.goto(base + '/#/sessions/keep-space/chat');
    await page.reload();
    await page.locator('#wb-state.run').waitFor();

    // Off, but the conversation is worth keeping: the chip is shown dimmed.
    const chip = page.locator('#chat-keepalive');
    await chip.waitFor({ state: 'visible' });
    assert.equal(await chip.evaluate(el => el.classList.contains('on')), false);
    assert.match(await chip.getAttribute('data-tip'), /重写约 152.0K token 的缓存（≈\$1\.22）/);

    await open();
    await page.locator('#ka-defaults').filter({ hasText: '100,000 token、3 小时' }).waitFor();
    assert.equal(await page.locator('#ka-enabled').isChecked(), false);
    assert.equal(await page.locator('#ka-terminal').isDisabled(), true, 'terminal follows the main switch');
    assert.equal(await page.locator('#ka-hours-label').innerText(), '最长保活（小时，最多 6）');
    assert.equal(await page.locator('#ka-hours').getAttribute('placeholder'), '3');
    assert.equal(await page.locator('#ka-unavailable').isVisible(), false);
    await page.locator('.ka-empty').waitFor();

    await page.locator('#ka-enabled').check();
    await page.locator('#ka-terminal').check();
    await page.locator('#ka-hours').fill('2');
    await page.locator('#ka-save').click();
    await page.locator('.ka-plan').nth(1).waitFor();
    assert.deepEqual(puts.at(-1), { enabled: true, terminal: true, max_hours: 2, min_context_tokens: 0 });
    assert.deepEqual(await page.locator('.ka-plan-name').allInnerTexts(), ['对话「Refactor billing」', '终端 /workspace/app']);
    assert.match(await page.locator('.ka-plan-meta').first().innerText(), /^152.0K tokens · 下次 \d\d:\d\d · 已续 1\/3 次 · 每次约 \$0\.030$/);

    await page.locator('.ka-plan').nth(1).getByRole('button', { name: '停止' }).click();
    await page.waitForFunction(() => document.querySelectorAll('.ka-plan').length === 1);
    assert.deepEqual(stops, [{ id: 'terminal:/workspace/app' }]);
    await page.locator('#ka-cancel').click();
    await dialog.waitFor({ state: 'hidden' });
    // Saving told the chip: it lights up and describes the chat plan.
    await page.locator('#chat-keepalive.on').waitFor();
    assert.match(await chip.getAttribute('data-tip'), /^缓存保活中\n152.0K tokens · 下次/);

    // An API key account cannot keep its five-minute caches.
    view = { ...base_view, available: false, reason: 'account', chat: undefined };
    await open();
    await page.locator('#ka-unavailable').filter({ hasText: '只写 5 分钟缓存' }).waitFor();
    assert.equal(await page.locator('#ka-enabled').isDisabled(), true);
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });
    console.log('Cache keepalive: menu entry, defaults, save, plan list, stop, composer chip and unavailable account passed');
  } finally {
    for (const [pattern, handler] of Object.entries(routes)) await page.unroute(pattern, handler);
  }
}
