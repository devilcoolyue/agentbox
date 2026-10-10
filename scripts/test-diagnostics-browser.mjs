import assert from 'node:assert/strict';
import {readFile,mkdir} from 'node:fs/promises';
import {diagnosticMessages} from '../internal/web/static/js/diagnostic-messages.js';
import en from '../internal/web/static/js/locales/en-dynamic.js';
import tw from '../internal/web/static/js/locales/zh-TW-dynamic.js';

const id='1234567890abcdef1234567890abcdef';
const row=(id,state,code)=>({id,state,code,message:'synthetic-private-path /operator/config',hint:'synthetic-private-credential'});
// Awaiting import() inside evaluate intermittently fails in Chromium with "Promise was collected" (Playwright
// reports it as a destroyed context): nothing holds the pending promise. Keep it on the page, wait for the module,
// then switch languages synchronously.
const language=async(page,locale)=>{
  await page.evaluate(()=>{window.__agentboxI18n??=import('/_v/{{BUILD}}/js/i18n.js').then(m=>window.__agentboxI18n=m.i18n);});
  await page.waitForFunction(()=>!(window.__agentboxI18n instanceof Promise));
  await page.evaluate(locale=>window.__agentboxI18n.setLanguage(locale),locale);
};

export async function diagnosticsSmoke(page,base){
  let scope='instance',failure=false,hang=false,requests=0,sockets=0;
  const handler=route=>{
    requests++;
    return route.fulfill({json:{version:1,scope,checked_at:Date.now(),operation_id:id,
      private_user:'private-user',secret:'private-credential',disk_total:123456,
      checks:[row('configuration','passed','config_ok'),row('docker','passed','docker_ok'),row('agent_image','failed','image_missing'),
        row('data_disk','failed','storage_full'),{id:'data_permissions',state:['passed'],code:['write_ok']},row('account_configuration','passed','accounts_ok'),
        row('websocket','not_checked','websocket_not_checked'),row('model','passed','accounts_ok'),
        row('unrecognized-private-user','passed','accounts_ok'),
        ...(scope==='session'?[row('account_access','failed','account_access_denied'),row('quota','failed','quota_exhausted')]:[])]}});
  };
  const socket=ws=>{
    sockets++;
    if(hang)return;
    if(failure){ws.close({code:1011,reason:'synthetic failure'});return;}
    const reference=new URL(ws.url()).searchParams.get('connection_id');
    ws.send(JSON.stringify({type:'diagnostic',id:'websocket',state:'passed',code:'websocket_ok',operation_id:reference}));
  };
  await page.route('**/api/diagnostics',handler);
  await page.route('**/api/sessions/*/diagnostics',handler);
  await page.routeWebSocket('**/api/diagnostics/ws?*',socket);
  await page.routeWebSocket('**/api/sessions/*/diagnostics/ws?*',socket);
  const dialog=page.locator('.diagnostics-dialog');
  const complete=()=>dialog.locator('button[data-icon="download"]');
  try{
    await page.goto(base+'/#/settings/container');
    for(const locale of ['zh-CN','zh-TW','en']){
      await language(page,locale);
      await page.locator('#btn-environment-check').click();
      await dialog.waitFor({state:'visible'});
      await page.waitForFunction(()=>{const d=document.querySelector('.diagnostics-dialog');return d && [...d.querySelectorAll('button')].some(b=>b.dataset.icon==="download" && !b.disabled);});
      for(const [check,code] of [['agent_image','image_missing'],['model','model_not_checked'],['websocket','websocket_ok']]){
        const source=diagnosticMessages[code][0];
        const text=locale==='en'?en[source]:locale==='zh-TW'?tw[source]:source;
        await dialog.locator(`[data-check="${check}"]`).filter({hasText:text}).waitFor();
      }
      assert.equal(await dialog.locator('[data-check="model"] [data-state]').getAttribute('data-state'),'not_checked');
      assert.ok(!(await dialog.innerText()).includes('synthetic-private'));
      assert.equal(await dialog.locator('[data-check="unrecognized-private-user"]').count(),0);
      // Switching languages updates the existing results without rerunning checks.
      const before=requests;
      await language(page,locale==='en'?'zh-TW':'en');await language(page,locale);
      assert.equal(requests,before);
      await dialog.locator('button').last().click();
    }
    await language(page,'en');
    await page.setViewportSize({width:390,height:844});
    await page.locator('#btn-environment-check').click();
    await page.waitForFunction(()=>{const d=document.querySelector('.diagnostics-dialog');return d && [...d.querySelectorAll('button')].some(b=>b.dataset.icon==="download" && !b.disabled);});
    assert.equal(await dialog.evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'diagnostic dialog overflows');
    const downloadPromise=page.waitForEvent('download');
    await complete().click();
    const download=await downloadPromise;
    const raw=await readFile(await download.path(),'utf8');
    const exported=JSON.parse(raw);
    assert.deepEqual(Object.keys(exported).sort(),['version','scope','checked_at','operation_id','checks','client_checks'].sort());
    assert.equal(exported.checks.find(row=>row.id==='model').state,'not_checked');
    assert.equal(exported.checks.find(row=>row.id==='data_permissions').state,'not_checked','malformed enum values must not become passed checks');
    assert.equal(exported.checks.find(row=>row.id==='websocket').state,'not_checked','browser observation must not rewrite server evidence');
    assert.equal(exported.client_checks[0].source,'browser');
    assert.equal(exported.client_checks[0].state,'passed');
    for(const secret of ['private-user','private-credential','/operator/config','disk_total'])assert.ok(!raw.includes(secret));
    await mkdir('output/playwright',{recursive:true});
    await page.mouse.move(0,0);
    await page.screenshot({path:'output/playwright/diagnostics-english-mobile.png',animations:'disabled'});
    await dialog.locator('button').last().click();

    // Workspace entry is shared by ordinary users; it never asks for /diagnostics.
    scope='session';failure=true;
    await page.setViewportSize({width:1280,height:900});
    await page.goto(base+'/#/sessions/fixture-space/chat');
    await page.locator('#btn-wb-more').click();
    await page.getByRole('menuitem',{name:'Environment checks',exact:true}).click();
    await dialog.locator('[data-check="websocket"] [data-state="failed"]').waitFor();
    await dialog.locator('[data-check="account_access"] [data-state="failed"]').waitFor();
    await dialog.locator('[data-check="quota"] [data-state="failed"]').waitFor();
    assert.equal(await dialog.locator('[data-check="configuration"]').count(),0,'session report exposed an instance-only check');
    assert.equal(await dialog.locator('[data-check="model"] [data-state]').getAttribute('data-state'),'not_checked');
    // Retry replaces the failure and gets a new observation.
    failure=false;
    await dialog.locator('button').first().click();
    await dialog.locator('[data-check="websocket"] [data-state="passed"]').waitFor();
    await dialog.locator('button').last().click();

    // Close during a waiting WS probe, then reopen. The old completion must not render.
    hang=true;
    await page.locator('#btn-wb-more').click();
    await page.getByRole('menuitem',{name:'Environment checks',exact:true}).click();
    await dialog.locator('[data-check="websocket"]').waitFor();
    await page.keyboard.press('Escape');
    await dialog.waitFor({state:'detached'});
    hang=false;
    await page.locator('#btn-wb-more').click();
    await page.getByRole('menuitem',{name:'Environment checks',exact:true}).click();
    await dialog.locator('[data-check="websocket"] [data-state="passed"]').waitFor();
    await dialog.locator('button').last().click();
    assert.ok(sockets>=6);
    console.log('Diagnostics browser: three states/locales, model never claimed, sanitized download, dedicated WS success/failure/retry, narrow layout and close/reopen lifecycle passed');
  }finally{
    // close removes this dialog asynchronously; avoid a count/evaluate race
    // that waits for a dialog which has already finished disposing.
    await page.evaluate(()=>document.querySelector('.diagnostics-dialog')?.close());
    await page.unroute('**/api/diagnostics',handler);await page.unroute('**/api/sessions/*/diagnostics',handler);
    await language(page,'zh-CN');await page.setViewportSize({width:1280,height:900});
    await page.goto(base+'/#/settings/container');await page.locator('#sec-container').waitFor({state:'visible'});
  }
}
