import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Exercise the 扩展 page against a synthetic admin API: list, detail, saving
// with a revision, conflict reload, plain-text rendering of manifest strings,
// and the workspace badge that tells users their model output is inspected.
export async function extensionsSmoke(page, base, setSessions) {
 const t = text => ({'zh-CN':text,en:text,'zh-TW':text});
 const view = {
  id:'agentbox.loop-guard',version:'1.0.0',publisher:'agentbox',builtin:true,runtime:'builtin',
  name:{'zh-CN':'循环保护','zh-TW':'循環保護',en:'Loop guard'},
  description:t('检测复读。<img src=x onerror="window.__extXSS=1">'),
  homepage:'https://example.invalid/loop-guard',license:'Apache-2.0',
  hooks:['model.request','model.stream'],permissions:['model.request.fingerprint','model.stream.text'],failure_policy:'fail_open',
  config_schema:{type:'object',properties:{
   window:{type:'integer',minimum:32,maximum:1024,default:256,title:t('检测窗口（句）')},
   cooldown_seconds:{type:'integer',minimum:0,maximum:600,default:30,title:t('相同请求冷却（秒）')},
  }},
  errors:[{code:'model_output_loop',message:t('模型在重复短句，这次回复已中断。')}],
  metrics:[{name:'trips',title:t('中断次数')}],
  setting:{enabled:false,order:0,config:{cooldown_seconds:30,window:256},permissions:['model.request.fingerprint','model.stream.text'],bindings:{mode:'none'},failure_policy:'',revision:0},
  status:{state:'disabled',restarts:0,active_streams:0},
  accounts:[{id:'relay',label:'Relay',type:'claude',eligible:true},{id:'sub',label:'Subscription',type:'claude',eligible:false}],
 };
 const puts = []; let conflictNext = false;
 const events = [{seq:1,time:new Date().toISOString(),extension:view.id,kind:'abort',code:'model_output_loop',session:'fixture-space',account:'relay',user:'fixture',detail:{window:256,distinct:12}}];
 const logs = [{seq:1,time:new Date().toISOString(),level:'info',message:'interrupted <b>a</b> repeating text stream'}];
 await page.route('**/api/extensions**', async route => {
  const req = route.request(), path = new URL(req.url()).pathname;
  if (path.endsWith('/events')) return route.fulfill({json:{events}});
  if (path.endsWith('/logs')) return route.fulfill({json:{logs:new URL(req.url()).searchParams.get('since')==='0'?logs:[]}});
  if (req.method() === 'PUT') {
   const body = req.postDataJSON(); puts.push(body);
   if (conflictNext) { conflictNext=false; return route.fulfill({status:409,json:{error:'扩展设置已变化，请刷新后重试'}}); }
   assert.equal(body.expected_revision, view.setting.revision, 'stale revision sent');
   view.setting = {...body, version:'1.0.0', revision:view.setting.revision+1};
   delete view.setting.expected_revision;
   view.status = body.enabled ? {state:'running',restarts:0,active_streams:0,started_at:new Date().toISOString(),metrics:{trips:1},hooks:view.hooks} : {state:'disabled',restarts:0,active_streams:0};
   return route.fulfill({json:view});
  }
  if (path === '/api/extensions') return route.fulfill({json:{gateway:{bind:'172.17.0.1:1083',host:'172.17.0.1',active:view.setting.enabled},extensions:[view]}});
  return route.fulfill({json:view});
 });
 try {
  await page.evaluate(()=>location.hash='#/extensions');
  const card = page.locator('#ext-list .ext-card');
  await card.waitFor();
  assert.equal(await page.locator('#btn-extensions').isVisible(), true, 'admin sees the 扩展 entry');
  assert.equal(await page.locator('#btn-extensions').getAttribute('aria-current'), 'page');
  assert.match(await page.locator('#ext-gateway').innerText(), /172\.17\.0\.1:1083/);
  assert.match(await card.innerText(), /循环保护[\s\S]*未启用/);
  await card.click();
  await page.waitForFunction(()=>location.hash==='#/extensions/agentbox.loop-guard');
  const detail = page.locator('#ext-detail');
  await detail.locator('.ext-settings').waitFor();
  // Manifest text is plain text, never markup.
  assert.match(await detail.locator('.ext-overview .ext-desc').innerText(), /<img src=x/);
  assert.equal(await detail.locator('img').count(), 0);
  assert.equal(await page.evaluate(()=>window.__extXSS), undefined);
  assert.match(await detail.locator('.ext-events').innerText(), /中断回复[\s\S]*model_output_loop[\s\S]*(Fixture workspace|fixture-space)/);
  assert.match(await detail.locator('.ext-logs').innerText(), /interrupted <b>a<\/b> repeating/);
  // Subscription accounts cannot be bound.
  await mkdir('output/playwright',{recursive:true});
  await page.screenshot({path:'output/playwright/extensions-overview.png',animations:'disabled'});
  const accountBox = detail.locator('.ext-accounts');
  assert.equal(await accountBox.isVisible(), false, 'account list hidden until "selected accounts"');
  await detail.locator('.ext-enable input').check();
  await detail.locator('.ext-bindings select').evaluate(sel=>{sel.value='accounts';sel.dispatchEvent(new Event('change',{bubbles:true}));});
  await accountBox.waitFor();
  const relay = accountBox.locator('label', {hasText:'Relay'}).locator('input');
  const sub = accountBox.locator('label', {hasText:'Subscription'}).locator('input');
  assert.equal(await sub.isDisabled(), true);
  await relay.check();
  await detail.locator('.ext-config [data-key="window"] input').fill('120');
  await mkdir('output/playwright',{recursive:true});
  for (const [width, name] of [[1280,'desktop'],[390,'mobile']]) {
   await page.setViewportSize({width,height:900});
   if(await page.locator('#sidebar.open').count()) await page.locator('#btn-sidebar-close').click();
   await page.screenshot({path:`output/playwright/extensions-${name}.png`,animations:'disabled',fullPage:false});
  }
  await page.setViewportSize({width:1280,height:900});
  await detail.locator('.ext-settings .card-foot button').click();
  await page.waitForFunction(()=>document.querySelector('#ext-detail .ext-state')?.dataset.state==='running');
  const saved = puts.at(-1);
  assert.equal(saved.enabled, true);
  assert.deepEqual(saved.permissions, ['model.request.fingerprint','model.stream.text']);
  assert.deepEqual(saved.bindings, {mode:'accounts',accounts:['relay']});
  assert.deepEqual(saved.config, {cooldown_seconds:30,window:120});
  assert.equal(saved.failure_policy, '');
  // A concurrent edit is reported and the form reloads the stored revision.
  conflictNext = true;
  await detail.locator('.ext-enable input').uncheck();
  await detail.locator('.ext-settings .card-foot button').click();
  await page.locator('#toast').filter({hasText:'已被其他人修改'}).waitFor();
  await page.waitForFunction(()=>document.querySelector('#ext-detail .ext-enable input')?.checked===true);
  assert.equal(puts.at(-1).expected_revision, 1);
  await detail.locator('.ext-back').click();
  await page.waitForFunction(()=>location.hash==='#/extensions');
  await card.waitFor();
  // Users see which extensions inspect their workspace.
  setSessions([{id:'fixture-space',name:'Fixture workspace',agent:'claude',account_id:'relay',account_label:'Relay',status:'stopped',default_model:'fixture',
   extensions:[{id:'agentbox.loop-guard',name:{'zh-CN':'循环保护','zh-TW':'循環保護',en:'Loop guard'}}]}]);
  await page.goto(base + '/#/sessions/fixture-space/chat');
  await page.reload(); // pick up the replaced session list
  const badge = page.locator('#wb-ext');
  await badge.waitFor();
  assert.match(await badge.innerText(), /循环保护/);
  console.log('Extensions: list/detail, permission approval, account binding with ineligible subscription accounts, schema form, revisioned save and conflict reload, plain-text manifest strings, events/logs and workspace badge passed');
  await page.screenshot({path:'output/playwright/extensions-workspace.png',animations:'disabled',clip:{x:0,y:0,width:1280,height:160}});
  assert.match(await badge.getAttribute('aria-label'), /循环保护 正在检查本空间的模型输出/);
 } finally {
  await page.unroute('**/api/extensions**');
  setSessions([]);
 }
}
