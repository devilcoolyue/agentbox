import assert from 'node:assert/strict';

// Account model lists on the synthetic API: the first upstream read selects
// everything, the administrator narrows it (filter + select all, manual IDs,
// per-model effort, default), saving sends one replacement, and a workspace on
// that account offers only those models with effort shown only where the model
// declares levels.
export async function accountModelsSmoke(page, base) {
  const account = { id: 'relay', type: 'codex', label: 'Relay', sessions: 1, cred_status: 'ok', auth_mode: 'apikey', model_reasoning: { 'deepseek-v4-flash': { support: 'unsupported' } } };
  const session = { id: 'fixture-space', name: 'Fixture workspace', agent: 'codex', account_id: 'relay', account_label: 'Relay', status: 'stopped', default_model: 'deepseek-v4' };
  const patches = []; let discoveries = 0, officialReads = 0, upstreamFails = false, extraAccounts = [];
  const upstream = [
    { id: 'deepseek-v4', label: 'DeepSeek V4', reasoning: { support: 'supported', control: 'effort', levels: ['low', 'high'] }, reasoning_source: 'upstream' },
    { id: 'deepseek-v4-flash', label: 'deepseek-v4-flash' },
    { id: 'deepseek-embed', label: 'deepseek-embed' },
  ];
  const effort = levels => ({ support: 'supported', control: 'effort', levels });
  const official = {
    source: 'official', endpoint: '', latency_ms: 3, official: { kind: 'cli', cli_version: '0.162.0' },
    models: [
      { id: 'gpt-6-sol', label: 'GPT-6-Sol', recommended: true, reasoning: effort(['low', 'medium', 'high', 'xhigh', 'max', 'ultra']), reasoning_source: 'official' },
      { id: 'gpt-5.2', label: 'GPT-5.2', reasoning: effort(['low', 'medium', 'high', 'xhigh']), reasoning_source: 'official' },
    ],
  };
  // Global price list: one model priced, the rest on the codex fallback; the
  // catalog lists custom-model.
  const rates = (input, output) => ({ input, output, cache_read: input / 10, cache_write: 0 });
  const pricing = { revision: '1', prices: { 'deepseek-v4': rates(0.5, 2), codex: rates(5, 30) }, managed: {}, catalog: { url: 'https://models.dev/api.json', auto_check: true, auto_apply: false }, history: [] };
  const catalogEntries = { 'custom-model': { price: rates(1, 4), source_url: 'https://example.invalid/pricing', verified_at: '2026-10-01T00:00:00Z' } };
  const pricingView = () => ({
    active: pricing, changes: [], warnings: [], warnings_truncated: false, in_use: Object.keys(pricing.prices),
    candidate: { catalog: { schema: 1, version: 'fixture', published_at: '', source: 'models.dev', entries: catalogEntries }, revision: 'c1', url: pricing.catalog.url, bundled: false, checked_at: Date.now(), attempted_at: Date.now(), error: '' },
  });
  const pricingWrites = [];
  const pricingRoute = ['**/api/pricing**', route => {
    const req = route.request(), path = new URL(req.url()).pathname;
    if (req.method() !== 'GET') {
      const body = req.postDataJSON(); pricingWrites.push({ path, body });
      assert.equal(body.revision, pricing.revision, 'stale price revision');
      if (path.endsWith('/apply')) for (const m of body.models) { pricing.prices[m] = catalogEntries[m].price; pricing.managed[m] = { version: 'fixture' }; }
      else { pricing.prices = body.prices; for (const key of body.custom_models || []) delete pricing.managed[key]; }
      pricing.revision = String(Number(pricing.revision) + 1);
    }
    return route.fulfill({ json: pricingView() });
  }];
  const sessionModels = () => ({
    models: (account.models || []).map(m => ({ ...m, reasoning: m.reasoning || { support: 'unknown', control: 'effort' } })),
    default_reasoning: { support: 'unknown', control: 'effort' }, discovery: 'stopped', restricted: !!account.models?.length, default_model: account.default_model, client_effort: account.type === 'codex' ? 'low' : undefined,
  });
  const routes = {
    '**/api/accounts': route => route.fulfill({ json: [account, ...extraAccounts] }),
    '**/api/accounts/relay': route => {
      assert.equal(route.request().method(), 'PATCH');
      const body = route.request().postDataJSON(); patches.push(body);
      Object.assign(account, body); if (!body.models?.length) delete account.models;
      return route.fulfill({ json: account });
    },
    '**/api/accounts/relay/models/discover': route => {
      discoveries++;
      return route.fulfill({ json: upstreamFails
        ? { ...official, upstream_error: 'relay.example.invalid/v1/models → HTTP 404: not found' }
        : { source: 'api', endpoint: 'https://relay.example.invalid/v1/models', latency_ms: 7, models: upstream, skipped: ['org/unsupported'] } });
    },
    '**/api/sessions': route => route.fulfill({ json: [session] }),
    '**/api/sessions/fixture-space/models': route => route.fulfill({ json: sessionModels() }),
  };
  for (const [pattern, handler] of Object.entries(routes)) await page.route(pattern, handler);
  const officialRoute = [/\/api\/accounts\/relay\/models\/discover\?source=official$/, route => { officialReads++; return route.fulfill({ json: official }); }];
  await page.route(...officialRoute);
  await page.route(...pricingRoute);
  const dialog = page.locator('#dlg-acct-models');
  // Optional review captures: AGENTBOX_ACCOUNT_MODELS_SHOTS=<directory>.
  const shot = async name => { if (process.env.AGENTBOX_ACCOUNT_MODELS_SHOTS) await page.screenshot({ path: `${process.env.AGENTBOX_ACCOUNT_MODELS_SHOTS}/account-models-${name}.png`, animations: 'disabled' }); };
  const row = id => dialog.locator(`.am-row[data-id="${id}"]`);
  try {
    await page.goto(base + '/#/settings/accounts');
    await page.reload();
    const acctRow = page.locator('.acct-row').filter({ hasText: 'relay' });
    await acctRow.locator('.acct-models-link').filter({ hasText: '沿用系统模型列表' }).click();

    // No saved list: the dialog reads upstream at once and selects everything.
    await row('deepseek-embed').waitFor();
    assert.equal(discoveries, 1);
    assert.match(await page.locator('#acct-models-status').innerText(), /已选 3 \/ 3 个模型[\s\S]*读取 3 个模型[\s\S]*1 个模型 ID 含有 CLI 不支持的字符/);
    assert.equal(await row('deepseek-v4').locator('.am-effort').innerText(), '强度：Low / High');
    // Nothing reported for this model: its effort stays unset until chosen.
    assert.equal(await row('deepseek-v4-flash').locator('.am-effort').innerText(), '强度：未设置');

    // Select all applies to the filtered rows only.
    await page.locator('#acct-models-all').uncheck();
    assert.equal(await dialog.locator('.am-row input[type=checkbox]:checked').count(), 0);
    await page.locator('#acct-models-filter').fill('v4');
    await page.locator('#acct-models-all').check();
    await page.locator('#acct-models-filter').fill('');
    assert.equal(await row('deepseek-embed').locator('input[type=checkbox]').isChecked(), false);
    assert.equal(await row('deepseek-v4-flash').locator('input[type=checkbox]').isChecked(), true);

    // Per-model effort: the flash model cannot be adjusted.
    await row('deepseek-v4-flash').locator('.am-effort').click();
    const editor = page.locator('dialog[open]').filter({ has: page.locator('select[name="support"]') });
    await editor.locator('label').filter({ hasText: '支持范围' }).getByRole('combobox').click();
    await editor.getByRole('option', { name: '不支持调整', exact: true }).click();
    await editor.getByRole('button', { name: '保存', exact: true }).click();
    await row('deepseek-v4-flash').locator('.am-effort').filter({ hasText: '强度：不支持调整' }).waitFor();

    // Manual IDs for providers that do not list everything; invalid IDs are refused.
    await page.locator('#acct-models-new').fill('bad/id');
    await page.locator('#acct-models-add').click();
    await page.locator('#acct-models-error').filter({ hasText: '模型 ID 格式不合法' }).waitFor();
    await page.locator('#acct-models-new').fill('custom-model');
    await page.locator('#acct-models-new').press('Enter');
    await row('custom-model').locator('.am-tag').filter({ hasText: '手动' }).waitFor();
    await row('custom-model').locator('.am-default input').check();
    // The radio itself shows the pointer, not only its caption.
    assert.equal(await row('custom-model').locator('.am-default input').evaluate(el => getComputedStyle(el).cursor), 'pointer');

    // Batch effort covers only the checked rows the filter shows.
    await page.locator('#acct-models-filter').fill('embed');
    assert.equal(await page.locator('#acct-models-bulk').isDisabled(), true, 'batch effort offered for unchecked rows');
    await page.locator('#acct-models-filter').fill('custom');
    await page.locator('#acct-models-bulk').click();
    await editor.locator('h2').filter({ hasText: '已选的 1 个模型' }).waitFor();
    await editor.locator('label').filter({ hasText: '支持范围' }).getByRole('combobox').click();
    await editor.getByRole('option', { name: '不支持调整', exact: true }).click();
    await editor.getByRole('button', { name: '保存', exact: true }).click();
    await row('custom-model').locator('.am-effort').filter({ hasText: '强度：不支持调整' }).waitFor();
    await page.locator('#acct-models-filter').fill('');
    assert.equal(await row('deepseek-v4').locator('.am-effort').innerText(), '强度：Low / High');
    await shot('desktop');
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth), true, 'mobile model dialog overflows');
    await shot('mobile');
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('#acct-models-ok').click();
    await dialog.waitFor({ state: 'hidden' });
    assert.deepEqual(patches.at(-1), {
      models: [
        { id: 'deepseek-v4', label: 'DeepSeek V4', reasoning: { support: 'supported', control: 'effort', levels: ['low', 'high'] } },
        { id: 'deepseek-v4-flash', label: 'deepseek-v4-flash', reasoning: { support: 'unsupported' } },
        { id: 'custom-model', label: 'custom-model', reasoning: { support: 'unsupported' } },
      ],
      default_model: 'custom-model', model_reasoning: {},
    });
    await acctRow.locator('.acct-models-link').filter({ hasText: '3 个可用模型' }).waitFor();

    // Reopening shows the saved selection without another upstream read.
    await acctRow.locator('.acct-models-link').click();
    await row('custom-model').waitFor();
    assert.equal(discoveries, 1);
    assert.equal(await dialog.locator('.am-row').count(), 3);

    // Official catalog: listed beside the saved models, not preselected for an
    // account that already has a list, and no saved model is marked missing.
    await page.locator('#acct-models-official').click();
    await row('gpt-6-sol').waitFor();
    assert.equal(officialReads, 1);
    assert.match(await page.locator('#acct-models-status').innerText(), /Codex CLI 0\.162\.0 内置目录[\s\S]*官方目录不代表/);
    assert.equal(await row('gpt-6-sol').locator('.am-tag').innerText(), '官方目录');
    assert.equal(await row('gpt-6-sol').locator('input[type=checkbox]').isChecked(), false);
    assert.equal(await row('gpt-6-sol').locator('.am-effort').innerText(), '强度：Low / Medium / High / Extra High / Max / Ultra');
    assert.equal(await dialog.locator('.am-tag').filter({ hasText: '上游未列出' }).count(), 0);
    // A relay without a model list: reading upstream falls back to the catalog.
    upstreamFails = true;
    await page.locator('#acct-models-fetch').click();
    await page.locator('#acct-models-status').filter({ hasText: '上游没有返回模型列表' }).waitFor();
    assert.match(await page.locator('#acct-models-status').innerText(), /HTTP 404/);
    upstreamFails = false;
    await shot('official');
    await page.locator('#acct-models-cancel').click();
    await dialog.waitFor({ state: 'hidden' });

    // The workspace offers only the account's models. A remembered model the
    // account no longer offers falls back to the account default.
    await page.evaluate(() => localStorage.setItem('agentbox_pick_fixture-space', JSON.stringify({ model: 'gpt-5.5', effort: 'high', version: 2, control: 'effort' })));
    await page.goto(base + '/#/sessions/fixture-space/chat');
    await page.reload();
    const pill = page.locator('#btn-pick .t');
    await pill.filter({ hasText: /^custom-model$/ }).waitFor();
    await page.locator('#btn-pick').click();
    // No effort levels for this model: the menu lists models only.
    const menu = page.locator('#pick-menu');
    assert.deepEqual(await menu.locator('.pick-opt .lbl').evaluateAll(els => els.map(el => el.firstChild.textContent)), ['DeepSeek V4', 'deepseek-v4-flash', 'custom-model']);
    assert.equal(await menu.locator('.pick-row').count(), 0, 'effort row shown for a model without levels');
    await menu.locator('.pick-opt').filter({ hasText: 'DeepSeek V4' }).click();
    // No "default" entry: the account's configured client effort is shown as the level in use.
    await pill.filter({ hasText: /^DeepSeek V4 · Low$/ }).waitFor();
    // A model with levels shows exactly those levels, in English.
    await page.locator('#btn-pick').click();
    await menu.locator('.pick-row').filter({ hasText: '推理强度' }).click();
    assert.deepEqual(await page.locator('#pick-fly .pick-opt .lbl').evaluateAll(els => els.map(el => el.firstChild.textContent)), ['Low', 'High']);
    assert.equal(await page.locator('#pick-fly .pick-opt.on .lbl').innerText(), 'Low');
    await shot('picker');
    await page.locator('#pick-fly .pick-opt').filter({ hasText: 'High' }).click();
    await pill.filter({ hasText: /^DeepSeek V4 · High$/ }).waitFor();
    await page.locator('#btn-pick').click();
    await menu.locator('.pick-row').filter({ hasText: '模型' }).click();
    assert.equal(await page.locator('#pick-fly').getByText('自定义模型…').count(), 0, 'custom model entry offered on a restricted account');
    await page.keyboard.press('Escape');

    // Model visibility page: a switch hides a model from the chat menu without
    // removing it from the account; the default model cannot be hidden.
    const patched = async count => { for (let i = 0; patches.length < count && i < 100; i++) await page.waitForTimeout(50); assert.equal(patches.length, count, 'visibility change not saved'); };
    await page.goto(base + '/#/settings/models');
    await page.reload();
    const card = page.locator('.mv-card[data-account="relay"]');
    const mvRow = id => card.locator(`.mv-row[data-model="${id}"]`);
    await card.locator('.mv-count').filter({ hasText: '显示 3 / 3' }).waitFor();
    assert.equal(await page.locator('#mdl-tab-accounts').getAttribute('aria-selected'), 'true');
    // Accounts are grouped by protocol; the only account with a list starts open
    // and its header can fold it away.
    assert.match(await page.locator('.mv-group[data-type="codex"] .mv-group-head').innerText(), /OpenAI 协议[\s\S]*Codex · 1 个账号/);
    const toggle = card.locator('.mv-toggle');
    assert.equal(await toggle.getAttribute('aria-expanded'), 'true');
    await toggle.click();
    await card.locator('.mv-rows').waitFor({ state: 'detached' });
    assert.equal(await toggle.getAttribute('aria-expanded'), 'false');
    assert.match(await toggle.innerText(), /显示 3 \/ 3[\s\S]*默认 custom-model[\s\S]*2 个价格待核对/);
    await toggle.click();
    await mvRow('deepseek-v4').waitFor();
    assert.equal(await mvRow('custom-model').locator('.mv-switch').isDisabled(), true, 'the default model can be hidden');
    let saved = patches.length;
    // The whole row (not the price button in it) is the switch's label.
    await mvRow('deepseek-v4-flash').locator('.mv-name').click();
    await card.locator('.mv-count').filter({ hasText: '显示 2 / 3' }).waitFor();
    await patched(++saved);
    assert.deepEqual(patches.at(-1).models.map(m => [m.id, !!m.hidden]), [['deepseek-v4', false], ['deepseek-v4-flash', true], ['custom-model', false]]);
    assert.equal('default_model' in patches.at(-1), false, 'a display change must not touch the default');
    assert.equal(await mvRow('deepseek-v4-flash').evaluate(el => el.classList.contains('off')), true);

    // Prices: what billing uses for each model; the agent fallback stands out
    // and can be replaced from the row without changing visibility.
    const price = id => mvRow(id).locator('.mv-price');
    await price('deepseek-v4').filter({ hasText: '$0.5 / $2' }).waitFor();
    assert.equal(await price('deepseek-v4-flash').innerText(), '兜底 $5 / $30');
    assert.equal(await price('deepseek-v4-flash').evaluate(el => el.classList.contains('warn')), true);
    await shot('model-display');
    const priceDialog = page.locator('#dlg-model-price');
    await price('deepseek-v4-flash').click();
    await priceDialog.locator('#mp-desc').filter({ hasText: '兜底价' }).waitFor();
    assert.equal(await page.locator('#mp-input').inputValue(), '', 'the fallback rate offered as this model\'s price');
    await shot('model-price');
    await page.locator('#mp-input').fill('0.1');
    await page.locator('#mp-output').fill('0.4');
    await page.locator('#mp-ok').click();
    await priceDialog.waitFor({ state: 'hidden' });
    await price('deepseek-v4-flash').filter({ hasText: '$0.1 / $0.4' }).waitFor();
    assert.deepEqual(pricingWrites.at(-1).body.prices['deepseek-v4-flash'], { input: 0.1, output: 0.4, cache_read: 0, cache_write: 0 });
    assert.deepEqual(pricingWrites.at(-1).body.custom_models, ['deepseek-v4-flash']);
    assert.deepEqual(pricingWrites.at(-1).body.prices.codex, rates(5, 30), 'other prices changed');
    assert.equal(patches.length, saved, 'a price change touched the account');
    assert.equal(await mvRow('deepseek-v4-flash').locator('.mv-switch').isChecked(), false);
    // A model the price catalog lists can follow it instead.
    await price('custom-model').click();
    await priceDialog.locator('#mp-catalog-text').filter({ hasText: 'models.dev 的价格' }).waitFor();
    await page.locator('#mp-follow').click();
    await priceDialog.waitFor({ state: 'hidden' });
    await price('custom-model').filter({ hasText: '$1 / $4' }).waitFor();
    assert.deepEqual(pricingWrites.at(-1), { path: '/api/pricing/apply', body: { revision: '2', catalog_revision: 'c1', models: ['custom-model'], adopt_custom: [] } });

    // Prices and the system list are tabs beside the account models.
    await page.locator('#mdl-tab-pricing').click();
    await page.locator('#price-rows tr[data-key="deepseek-v4-flash"]').waitFor();
    assert.equal(new URL(page.url()).hash, '#/settings/models/pricing');
    assert.equal(await page.locator('#mv-list').isVisible(), false);
    await shot('pricing-tab');
    await page.locator('#mdl-tab-system').click();
    // Every account has its own list: the system list says it is unused.
    assert.match(await page.locator('#mdl-system-state').innerText(), /所有账号都已配置可用模型/);
    await page.locator('#mdl-tab-accounts').click();
    await card.waitFor();
    // The filter sits at the scroll container's edge; its focus ring must not be clipped.
    await page.locator('#mv-filter').focus();
    assert.ok(await page.locator('#mv-filter').evaluate(el => el.getBoundingClientRect().left - 3 >= el.closest('.set-content').getBoundingClientRect().left), 'filter focus ring clipped');
    // The filter narrows the rows; the available-models dialog keeps the flag.
    await page.locator('#mv-filter').fill('flash');
    assert.equal(await card.locator('.mv-row').count(), 1);
    await page.locator('#mv-filter').fill('');
    await card.locator('.mv-head button').filter({ hasText: '管理可用模型…' }).click();
    await row('deepseek-v4-flash').locator('.am-tag').filter({ hasText: '已隐藏' }).waitFor();
    assert.equal(await row('deepseek-v4-flash').locator('.am-default input').isDisabled(), true, 'a hidden model offered as default');
    await page.locator('#acct-models-cancel').click();
    await dialog.waitFor({ state: 'hidden' });
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, 'model visibility page overflows');
    await shot('model-display-mobile');
    await page.setViewportSize({ width: 1280, height: 900 });

    // Chat lists only the shown models.
    await page.goto(base + '/#/sessions/fixture-space/chat');
    await page.reload();
    await pill.filter({ hasText: /^DeepSeek V4 · High$/ }).waitFor();
    await page.locator('#btn-pick').click();
    await menu.locator('.pick-row').filter({ hasText: '模型' }).click();
    assert.deepEqual(await page.locator('#pick-fly .pick-opt .lbl').evaluateAll(els => els.map(el => el.firstChild.textContent)), ['DeepSeek V4', 'custom-model']);
    await page.keyboard.press('Escape');

    // Turning it back on lists it again.
    await page.goto(base + '/#/settings/models');
    await page.reload();
    await mvRow('deepseek-v4-flash').locator('.mv-switch').check();
    await card.locator('.mv-count').filter({ hasText: '显示 3 / 3' }).waitFor();
    await patched(++saved);
    assert.equal(patches.at(-1).models.some(m => m.hidden), false);

    // With several accounts every card starts folded to its summary line;
    // groups follow the protocol and "expand all" opens them together.
    extraAccounts = [{ id: 'claude-sub', type: 'claude', label: 'Claude subscription', sessions: 0, cred_status: 'ok', default_model: 'claude-opus-5-5',
      models: [{ id: 'claude-opus-5-5', label: 'Claude Opus 5.5', reasoning: { support: 'supported', control: 'effort', levels: ['low', 'medium', 'high', 'xhigh', 'max'] } }, { id: 'claude-haiku-5-5', label: 'Claude Haiku 5.5', hidden: true }] }];
    await page.reload();
    await page.locator('.mv-card[data-account="claude-sub"]').waitFor();
    assert.deepEqual(await page.locator('.mv-group').evaluateAll(els => els.map(el => el.dataset.type)), ['claude', 'codex']);
    assert.equal(await page.locator('.mv-rows').count(), 0, 'cards start expanded');
    await shot('model-display-groups');
    await page.locator('#mv-expand').click();
    await page.locator('.mv-card[data-account="claude-sub"] .mv-rows').waitFor();
    assert.equal(await page.locator('.mv-rows').count(), 2);
    assert.match(await page.locator('#mv-expand').innerText(), /全部收起/);
    // A filter opens just the cards with matching models.
    await page.locator('#mv-expand').click();
    await page.locator('#mv-filter').fill('haiku');
    await page.locator('.mv-card[data-account="claude-sub"] .mv-row[data-model="claude-haiku-5-5"]').waitFor();
    assert.equal(await page.locator('.mv-card').count(), 1);
    await page.locator('#mv-filter').fill('');
    extraAccounts = [];

    // Claude-style menu: with a long model list the effort row stays pinned to
    // the bottom edge instead of scrolling away with the list.
    account.type = 'claude'; session.agent = 'claude';
    account.models = Array.from({ length: 18 }, (_, i) => ({ id: `claude-model-${i}`, label: `Claude Model ${i}`, reasoning: { support: 'supported', control: 'effort', levels: ['low', 'medium', 'high', 'xhigh', 'max'] } }));
    account.default_model = 'claude-model-0';
    await page.evaluate(() => localStorage.removeItem('agentbox_pick_fixture-space'));
    await page.goto(base + '/#/sessions/fixture-space/chat');
    await page.reload();
    await pill.filter({ hasText: /^Claude Model 0 · High$/ }).waitFor();
    await page.locator('#btn-pick').click();
    const foot = menu.locator('.pick-foot .pick-row');
    const pinned = async () => {
      const [m, f] = [await menu.boundingBox(), await foot.boundingBox()];
      return f.y >= m.y && f.y + f.height <= m.y + m.height + 1 && m.y + m.height - (f.y + f.height) < 12;
    };
    assert.equal(await menu.evaluate(el => el.scrollHeight > el.clientHeight), true, 'model list does not scroll');
    assert.equal(await pinned(), true, 'effort row not at the bottom before scrolling');
    await menu.evaluate(el => { el.scrollTop = el.scrollHeight / 3; });
    assert.equal(await pinned(), true, 'effort row scrolled away with the list');
    await foot.hover();
    await page.locator('#pick-fly .pick-opt').first().waitFor();
    assert.deepEqual(await page.locator('#pick-fly .pick-opt .lbl').allInnerTexts(), ['Low', 'Medium', 'High', 'Extra High', 'Max']);
    const [flyBox, rowBox] = [await page.locator('#pick-fly').boundingBox(), await foot.boundingBox()];
    assert.ok(flyBox.y <= rowBox.y + rowBox.height && flyBox.y + flyBox.height >= rowBox.y, 'effort flyout detached from its row');
    await shot('claude-picker');
    await page.keyboard.press('Escape');
    console.log('Account models: upstream read, official catalog and relay fallback, filtered select-all, per-model and batch effort, manual IDs, save, visibility switches and restricted picker passed');
  } finally {
    for (const [pattern, handler] of Object.entries(routes)) await page.unroute(pattern, handler);
    await page.unroute(...officialRoute);
    await page.unroute(...pricingRoute);
    await page.evaluate(() => localStorage.removeItem('agentbox_pick_fixture-space'));
  }
}
