import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Exercise the 扩展 page against a synthetic admin API: list, detail, saving
// with a revision, conflict reload, plain-text rendering of manifest strings,
// the workspace badge that tells users their model output is inspected, and
// the model gateway living on the settings page instead of the 扩展 page.
export async function extensionsSmoke(page, base, setSessions) {
 const t = text => ({'zh-CN':text,en:text,'zh-TW':text});
 const view = {
  id:'agentbox.loop-guard',version:'1.0.0',publisher:'agentbox',builtin:true,runtime:'builtin',
  name:{'zh-CN':'循环保护','zh-TW':'循環保護',en:'Loop guard'},
  description:t('检测复读。<img src=x onerror="window.__extXSS=1">'),
  homepage:'https://example.invalid/loop-guard',license:'Apache-2.0',
  hooks:['model.request','model.stream'],permissions:['model.request.fingerprint','model.stream.text'],bindings:['account'],failure_policy:'fail_open',
  config_schema:{type:'object',properties:{
   window:{type:'integer',minimum:32,maximum:1024,default:256,title:t('检测窗口（句）')},
   cooldown_seconds:{type:'integer',minimum:0,maximum:600,default:30,title:t('相同请求冷却（秒）')},
  }},
  errors:[{code:'model_output_loop',message:t('模型在重复短句，这次回复已中断。')}],
  metrics:[{name:'trips',title:t('中断次数')}],
  setting:{enabled:false,order:0,config:{cooldown_seconds:30,window:256},permissions:['model.request.fingerprint','model.stream.text'],bindings:{mode:'none'},failure_policy:'',revision:0},
  status:{state:'disabled',restarts:0,active_streams:0},
  requires:[{id:'model_gateway',bind:'172.17.0.1:1083',active:false}],
  accounts:[{id:'relay',label:'Relay',type:'claude',eligible:true},{id:'sub',label:'Subscription',type:'claude',eligible:false},
   {id:'ds4',label:'ds4-1f',type:'codex',eligible:true},{id:'gpt',label:'ChatGPT login',type:'codex',eligible:false}],
 };
 // An event-only extension: no gateway, bound by account or user.
 const webhook = {
  id:'agentbox.webhook',version:'1.0.0',publisher:'agentbox',builtin:true,runtime:'builtin',
  name:{'zh-CN':'Webhook 通知','zh-TW':'Webhook 通知',en:'Webhook notifications'},description:t('推送空间与对话事件。'),
  hooks:['workspace.lifecycle','chat.turn.ended'],permissions:['workspace.events','chat.turns'],bindings:['account','user'],failure_policy:'fail_open',
  config_schema:{type:'object',properties:{url:{type:'string',maxLength:1024,title:t('推送地址')},format:{type:'enum',enum:['json','feishu'],default:'json',title:t('消息格式')}}},
  errors:[],metrics:[{name:'sent',title:t('已送达')}],
  setting:{enabled:false,order:0,config:{format:'json'},permissions:['chat.turns','workspace.events'],bindings:{mode:'none'},failure_policy:'',revision:0},
  status:{state:'disabled',restarts:0,active_streams:0},requires:[],
  accounts:[{id:'relay',label:'Relay',type:'claude',eligible:true},{id:'sub',label:'Subscription',type:'claude',eligible:true}],
 };
 const puts = []; let conflictNext = false, slowNext = false;
 const events = [{seq:1,time:new Date().toISOString(),extension:view.id,kind:'abort',code:'model_output_loop',session:'fixture-space',account:'relay',user:'fixture',detail:{window:256,distinct:12}}];
 const logs = [{seq:1,time:new Date().toISOString(),level:'info',message:'interrupted <b>a</b> repeating text stream'}];
 await page.route('**/api/extensions**', async route => {
  const req = route.request(), path = new URL(req.url()).pathname;
  if (slowNext && req.method() === 'GET') { slowNext = false; await new Promise(r => setTimeout(r, 900)); }
  if (path.endsWith('/events')) return route.fulfill({json:{events}});
  if (path.endsWith('/logs')) return route.fulfill({json:{logs:new URL(req.url()).searchParams.get('since')==='0'?logs:[]}});
  if (req.method() === 'PUT') {
   const body = req.postDataJSON(); puts.push(body);
   if (conflictNext) { conflictNext=false; return route.fulfill({status:409,json:{error:'扩展设置已变化，请刷新后重试'}}); }
   assert.equal(body.expected_revision, view.setting.revision, 'stale revision sent');
   view.setting = {...body, version:'1.0.0', revision:view.setting.revision+1};
   delete view.setting.expected_revision;
   view.requires = [{id:'model_gateway',bind:'172.17.0.1:1083',active:body.enabled}];
   view.status = body.enabled ? {state:'running',restarts:0,active_streams:0,started_at:new Date().toISOString(),metrics:{trips:1},hooks:view.hooks} : {state:'disabled',restarts:0,active_streams:0};
   return route.fulfill({json:view});
  }
  if (path === '/api/extensions') return route.fulfill({json:{extensions:[view,webhook]}});
  return route.fulfill({json:view});
 });
 try {
  await page.evaluate(()=>location.hash='#/extensions');
  const card = page.locator('#ext-list .ext-card', {hasText:'循环保护'});
  await card.waitFor();
  assert.equal(await page.locator('#btn-extensions').isVisible(), true, 'admin sees the 扩展 entry');
  assert.equal(await page.locator('#btn-extensions').getAttribute('aria-current'), 'page');
  assert.match(await card.innerText(), /循环保护[\s\S]*未启用[\s\S]*模型流量/);
  assert.match(await page.locator('#ext-list .ext-card', {hasText:'Webhook 通知'}).innerText(), /空间事件\s*对话事件/);
  // The list is about extensions only: no gateway banner or switch.
  const listPage = await page.locator('#view-extensions').innerText();
  assert.doesNotMatch(listPage, /172\.17\.0\.1|网关/, 'the 扩展 page names the model gateway');
  await mkdir('output/playwright',{recursive:true});
  await page.screenshot({path:'output/playwright/extensions-list.png',animations:'disabled'});
  // The sidebar entry is a puzzle piece, not the security shield.
  assert.equal(await page.locator('#btn-extensions svg').getAttribute('data-icon-name'), 'nav.extensions');
  // A slow request started by a click shows the top loading line until it completes.
  slowNext = true;
  await card.click();
  await page.locator('#top-loader.on').waitFor({timeout:2000});
  await page.locator('#top-loader.on').waitFor({state:'detached',timeout:5000});
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
  // Model hooks are scoped by account only: no user picker, account-only wording.
  assert.equal(await detail.locator('.ext-users').count(), 0);
  assert.deepEqual(await detail.locator('.ext-bindings select option').allInnerTexts(), ['暂不生效','指定账号','全部 API Key / 中转账号（含以后新增的）']);
  await detail.locator('.ext-enable input').check();
  await detail.locator('.ext-bindings select').evaluate(sel=>{sel.value='selected';sel.dispatchEvent(new Event('change',{bubbles:true}));});
  await accountBox.waitFor();
  const relay = accountBox.locator('label', {hasText:'Relay'}).locator('input');
  const sub = accountBox.locator('label', {hasText:'Subscription'}).locator('input');
  assert.equal(await sub.isDisabled(), true);
  // Codex API key / relay accounts are eligible; ChatGPT logins are not.
  assert.equal(await accountBox.locator('label', {hasText:'ds4-1f'}).locator('input').isDisabled(), false);
  assert.equal(await accountBox.locator('label', {hasText:'ChatGPT login'}).locator('input').isDisabled(), true);
  await relay.check();
  await accountBox.locator('label', {hasText:'ds4-1f'}).locator('input').check();
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
  // Only extensions with model hooks name the gateway, on their own page.
  await page.waitForFunction(()=>document.querySelector('#ext-detail .ext-require .gw-state')?.dataset.state==='on');
  assert.match(await detail.locator('.ext-require').innerText(), /依赖模型网关 172\.17\.0\.1:1083 · 运行中/);
  const saved = puts.at(-1);
  assert.equal(saved.enabled, true);
  assert.deepEqual(saved.permissions, ['model.request.fingerprint','model.stream.text']);
  assert.deepEqual(saved.bindings, {mode:'selected',accounts:['relay','ds4'],users:[]});
  assert.deepEqual(saved.config, {cooldown_seconds:30,window:120});
  assert.equal(saved.failure_policy, '');
  // A concurrent edit is reported and the form reloads the stored revision.
  conflictNext = true;
  await detail.locator('.ext-enable input').uncheck();
  await detail.locator('.ext-settings .card-foot button').click();
  await page.locator('#toast').filter({hasText:'已被其他人修改'}).waitFor();
  await page.waitForFunction(()=>document.querySelector('#ext-detail .ext-enable input')?.checked===true);
  assert.equal(puts.at(-1).expected_revision, 1);
  // The dependency links to the gateway card in 系统设置 → IP 代理, which
  // owns the connection mode and saves it with the other settings.
  await detail.locator('.ext-require a').click();
  await page.waitForFunction(()=>location.hash==='#/settings/proxies');
  const gwState = page.locator('#mgw-state');
  await gwState.waitFor();
  assert.match(await gwState.innerText(), /172\.17\.0\.1:1083 · 没有启用检查模型流量的扩展，未监听/);
  const dedicated = page.locator('#set-mgw-dedicated');
  assert.equal(await dedicated.isChecked(), false);
  await dedicated.check();
  const settingsPut = page.waitForRequest(r=>r.method()==='PUT' && new URL(r.url()).pathname==='/api/settings');
  await page.locator('#set-save').click();
  assert.deepEqual((await settingsPut).postDataJSON(), {model_gateway:{dedicated_connections:true}});
  await page.waitForFunction(()=>document.querySelector('#set-savebar')?.hidden===true);
  assert.equal(await dedicated.isChecked(), true);
  await page.locator('.card', {has:gwState}).screenshot({path:'output/playwright/extensions-model-gateway.png',animations:'disabled'});
  await page.evaluate(()=>location.hash='#/extensions');
  await card.waitFor();
  // An event extension: no gateway line, no failure policy or interruption
  // log, every account selectable and a user picker next to them.
  await page.locator('#ext-list .ext-card', {hasText:'Webhook 通知'}).click();
  await page.waitForFunction(()=>location.hash==='#/extensions/agentbox.webhook');
  await detail.locator('.ext-settings').waitFor();
  assert.equal(await detail.locator('.ext-require').count(), 0);
  assert.equal(await detail.locator('.ext-events').count(), 0);
  assert.doesNotMatch(await detail.innerText(), /扩展失效时|检查中的流|网关/);
  assert.deepEqual(await detail.locator('.ext-bindings select option').allInnerTexts(), ['暂不生效','指定账号或用户','全部（含以后新增的）']);
  await detail.locator('.ext-bindings select').evaluate(sel=>{sel.value='selected';sel.dispatchEvent(new Event('change',{bubbles:true}));});
  await detail.locator('.ext-users .user-picker').waitFor();
  assert.equal(await detail.locator('.ext-accounts input:disabled').count(), 0, 'event hooks take any account');
  await page.screenshot({path:'output/playwright/extensions-webhook.png',animations:'disabled'});
  await detail.locator('.ext-back').click();
  await card.waitFor();
  // Users see which extensions inspect their workspace.
  setSessions([{id:'fixture-space',name:'Fixture workspace',agent:'claude',account_id:'relay',account_label:'Relay',status:'stopped',default_model:'fixture',
   extensions:[{id:'agentbox.loop-guard',name:{'zh-CN':'循环保护','zh-TW':'循環保護',en:'Loop guard'},groups:['model']},
    {id:'agentbox.webhook',name:{'zh-CN':'Webhook 通知','zh-TW':'Webhook 通知',en:'Webhook notifications'},groups:['workspace','chat']}]}]);
  // Reload before routing: the page still holds the earlier session list, so a hash change to
  // this workspace first would be redirected home (WebKit does so before the reload starts).
  await page.reload();
  await page.goto(base + '/#/sessions/fixture-space/chat');
  const badge = page.locator('#wb-ext');
  await badge.waitFor();
  assert.match(await badge.innerText(), /循环保护、Webhook 通知/);
  assert.equal(await badge.locator('svg').getAttribute('data-icon-name'), 'puzzle');
  console.log('Extensions: list/detail without gateway, event-only webhook detail with user picker, model gateway dependency and settings card, permission approval, Claude and Codex API key binding with ineligible subscription/ChatGPT accounts, schema form, revisioned save and conflict reload, plain-text manifest strings, events/logs, puzzle icons, top loading line and workspace badge passed');
  await page.screenshot({path:'output/playwright/extensions-workspace.png',animations:'disabled',clip:{x:0,y:0,width:1280,height:160}});
  assert.equal(await badge.getAttribute('aria-label'), '循环保护 正在检查本空间的模型输出；Webhook 通知 会收到本空间的空间启停与对话结果通知');
 } finally {
  await page.unroute('**/api/extensions**');
  setSessions([]);
 }
}
