import assert from 'node:assert/strict';
import {mkdir} from 'node:fs/promises';
import en from '../internal/web/static/js/locales/en-dynamic.js';
import tw from '../internal/web/static/js/locales/zh-TW-dynamic.js';
import {problemMessages} from '../internal/web/static/js/problems.js';

const id = '0123456789abcdef0123456789abcdef';
const language = (page, locale) => page.evaluate(async locale => {
  const {i18n} = await import('/_v/{{BUILD}}/js/i18n.js');
  i18n.setLanguage(locale);
}, locale);
const expected = (code, locale) => problemMessages[code].map(text => locale === 'en' ? en[text] : locale === 'zh-TW' ? tw[text] : text);
const problem = code => ({code, error:'synthetic-secret /private/config.json',hint:'synthetic-secret',operation_id:id,action:'contact_admin',retryable:false});

export async function loginProblemsSmoke(page) {
  let payload = problem('invalid_credentials');
  const handler = route => route.fulfill({status:401,json:payload});
  await page.route('**/api/login',handler);
  try {
    for (const locale of ['zh-CN','zh-TW','en']) {
      await language(page,locale);
      await page.locator('#login-user').fill('synthetic-user');
      await page.locator('#login-pass').fill('synthetic-password');
      await page.locator('#login-btn').click();
      for (const text of [...expected('invalid_credentials',locale),id]) {
        await page.locator('#login-error').filter({hasText:text}).waitFor();
      }
      assert.ok(!(await page.locator('#login-error').innerText()).includes('synthetic-secret'));
      assert.equal(await page.locator('#login-btn').isDisabled(),false);
    }
    payload = {error:'legacy-login-error'};
    await page.locator('#login-btn').click();
    await page.locator('#login-error').filter({hasText:'legacy-login-error'}).waitFor();
  } finally {
    await page.unroute('**/api/login',handler);
    await language(page,'zh-CN');
  }
}

export async function problemsSmoke(page, base, socket, history) {
  let code='docker_unavailable';
  const handler=route=>route.fulfill({status:500,json:problem(code)});
  await page.route('**/api/sessions/fixture-space/start',handler);
  try {
    await page.goto(base+'/#/sessions/fixture-space/chat');
    await page.locator('#chat-input').waitFor({state:'visible'});
    await page.waitForFunction(()=>!document.querySelector('#chat-input').disabled);
    for (const locale of ['zh-CN','zh-TW','en']) {
      await language(page,locale);
      await page.setViewportSize({width:1280,height:900});
      for (code of ['docker_unavailable','agent_image_missing','storage_full']) {
        await page.locator('#btn-start').click();
        // Toasts use real API -> error formatting -> text rendering.
        for (const text of [...expected(code,locale),id]) await page.getByText(text,{exact:false}).last().waitFor();
        assert.ok(!(await page.locator('body').innerText()).includes('synthetic-secret'));
      }
      for (const width of [1280,390]) {
        await page.setViewportSize({width,height:900});
        for (const wsCode of ['account_access_denied','quota_exhausted']) {
          socket().send(JSON.stringify({type:'error',...problem(wsCode)}));
          const error=page.locator('#chat-log .chip.err').last();
          for (const text of [...expected(wsCode,locale),id]) await error.filter({hasText:text}).waitFor();
          assert.equal(await error.locator('script').count(),0);
          assert.equal(await error.evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'error chip overflows');
        }
      }
      history([{kind:'status',state:'error',ts:'2026-10-05T00:00:00Z',...problem('agent_image_missing')}]);
      await page.evaluate(async()=>{const {loadHistory}=await import('/_v/{{BUILD}}/js/chat.js');await loadHistory();});
      for (const text of [...expected('agent_image_missing',locale),id]) await page.locator('#chat-log .chip.err').last().filter({hasText:text}).waitFor();
    }
    history([{kind:'status',state:'error',ts:'2026-10-05T00:00:00Z',error:'legacy-history-error'}]);
    await page.evaluate(async()=>{const {loadHistory}=await import('/_v/{{BUILD}}/js/chat.js');await loadHistory();});
    await page.locator('#chat-log .chip.err').filter({hasText:'legacy-history-error'}).waitFor();

    await page.locator('#toast').waitFor({state:'hidden'});
    if (await page.locator('#sidebar.open').count()) await page.locator('#btn-sidebar-close').click();
    await page.waitForFunction(()=>document.querySelector('#sidebar').getBoundingClientRect().right<=1);
    const reference=new URL(socket().url()).searchParams.get('connection_id');
    assert.match(reference,/^[a-f0-9]{32}$/);
    socket().close({code:1011,reason:'synthetic disconnect'});
    await page.locator('#chat-conn-text').filter({hasText:reference}).waitFor();
    assert.equal(await page.locator('#chat-conn').evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'connection error overflows on narrow screen');
    await mkdir('output/playwright',{recursive:true});
    await page.screenshot({path:'output/playwright/problems-english-mobile.png',animations:'disabled'});
    console.log('Problems browser: three-language login/start/chat/history, old-server fallback, redaction, narrow layout and WS connection reference passed');
  } finally {
    await page.unroute('**/api/sessions/fixture-space/start',handler);
    history([]);
    await language(page,'zh-CN');
    await page.setViewportSize({width:1280,height:900});
    await page.goto(base+'/#/settings/container');
    await page.locator('#sec-container').waitFor({state:'visible'});
  }
}
