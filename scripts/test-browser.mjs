#!/usr/bin/env node
// Synthetic API/browser regression. No Docker, real accounts or provider calls.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { responsiveSmoke } from './test-responsive.mjs';
import { pricingSmoke } from './test-pricing.mjs';
import { chatFooterSmoke } from './test-chat-footer.mjs';
import { assertActionIcons, fileActionSmoke } from './test-actions.mjs';

export async function smoke(page) {
 page.setDefaultTimeout(15000);
 const root = resolve(fileURLToPath(new URL('../internal/web/static/', import.meta.url)));
 const server = createServer(async (req,res) => {
  try {
   const rel = decodeURIComponent(req.url.split('?')[0]).replace(/^\/_v\/[^/]+/, '');
   const path = resolve(root, '.' + (rel === '/' ? '/index.html' : rel));
   if (!path.startsWith(root + '/')) {res.writeHead(403).end();return;}
   const data = await readFile(path);
   res.setHeader('Content-Type', ({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(path)] || 'application/octet-stream');
   res.end(data);
  } catch { res.writeHead(404).end(); }
 });
 await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
 const base = `http://127.0.0.1:${server.address().port}`;
 const errors = []; page.on('pageerror', e => { errors.push(e.stack || e.message); console.error('Browser page error:', e.stack || e.message); });
 let settingsWrites = 0, monitorCalls = 0, updateChecks = 0, updateReads = 0;
 let release = {current_version:'v0.1.0-rc.2-updates2',revision:'0123456789abcdef',built_at:'2026-09-24T08:00:00Z',latest_version:'',available:false,comparable:true,release_url:'https://github.com/devilcoolyue/agentbox-releases/releases',notes:'',checked_at:0,attempted_at:0,error:''};
 let nextRelease = {latest_version:'v0.2.0',available:true,notes:'新增版本提醒。\n<script>untrusted release notes</script>'};
 let upgrade = {supported:true,reason:'',current_version:release.current_version,job:null};
 let upgradeStarts = 0, upgradeReads = 0, loseUpgradeResponse = false, upgradeOffline = false;
 const accounts = []; let accountCreates = 0, failOAuthOnce = true, oauthFinishes = 0, keyWrites = 0;
 let settings = { listen:'127.0.0.1:8180', agent_image:'fixture', permission_mode:'bypassPermissions', max_upload_mb:10,
  idle_timeout_min:30, timezone:'UTC', container:{memory_mb:512,cpus:1,pids_limit:128,network:'none'},
  resources:{max_running:2,max_running_per_user:1,min_free_bytes:0},models:{claude:[],codex:[{id:'fixture',label:'Fixture',reasoning:{support:'supported',control:'effort',levels:['low','high','xhigh']}},{id:'fixture-lite',label:'Fixture Lite',reasoning:{support:'supported',control:'effort',levels:['low','high']}},{id:'fixture-none',label:'No adjustment',reasoning:{support:'unsupported'}},{id:'fixture-custom',label:'Custom'}]}, default_models:{claude:'fixture',codex:'fixture'},
  terminal_tips:{tips:['fixture'],interval_sec:4,animation:'scroll'},tunnel:{enabled:false},proxy_bridge:{bind:'127.0.0.1:1081'},pricing:{} };
 const me = {user:'fixture',role:'admin',timezone:'UTC',models:{claude:[],codex:[]},quota:{metered:false}};
 let sessions = [{id:'fixture-space',name:'Fixture workspace',agent:'codex',account_id:'fixture',account_label:'Fixture',status:'stopped',default_model:'fixture'}];
 const chatMessages = []; let chatSocket, historyEntries = [], historyCosts = {};
 await page.routeWebSocket('**/api/sessions/*/chat?*', ws => { chatSocket=ws; ws.onMessage(data => chatMessages.push(JSON.parse(data))); });
 const terminalInput = []; let terminalSocket;
 await page.routeWebSocket('**/api/sessions/*/term?*', ws => {
  terminalSocket = ws;
  ws.onMessage(data => { if (typeof data !== 'string') terminalInput.push(data.toString()); });
 });
 await page.route('**/api/**', async route => {
  const u = new URL(route.request().url()), path=u.pathname;
  let body={};
  if (path === '/api/login') body={token:'synthetic-browser-token'};
  else if(path === '/api/me') body=me;
  else if(path === '/api/git/connections') body=[];
  else if(path === '/api/me/git') body={user:'fixture',name:'Fixture',email:'fixture@example.com'};
  else if(path === '/api/proxies') body={proxies:[],bridge_up:false,bridge_host:'127.0.0.1'};
  else if(path === '/api/pricing') body={active:{revision:'1',prices:{},managed:{},history:[],catalog:{url:'',auto_check:false}},candidate:null,changes:[],warnings:[]};
  else if(path === '/api/me/git/default') body={connection_id:''};
  else if(path === '/api/updates') { updateReads++; body=release; }
  else if(path === '/api/updates/upgrade') {
   if(route.request().method()==='POST') {
    upgradeStarts++;
    assert.equal(route.request().postDataJSON().version,'v0.2.0');
    upgrade.job={id:'a'.repeat(32),version:'v0.2.0',from_version:release.current_version,phase:'downloading',message:'正在下载发布包',error:'',started_at:Date.now(),updated_at:Date.now()};
    if(loseUpgradeResponse) { loseUpgradeResponse=false; await route.abort(); return; }
   } else {
    upgradeReads++;
    if(upgradeOffline) { await route.abort(); return; }
   }
   body=upgrade;
  }
  else if(path === '/api/updates/check') {
   assert.equal(route.request().method(),'POST'); updateChecks++;
   release={...release,...nextRelease,checked_at:Date.now(),attempted_at:Date.now()};body=release;
  }
  else if(path === '/api/system') body={version:'v0.1.0',go_version:'go1.26.6',docker_version:'28.5.2',schema_version:2,sessions_running:0,sessions_total:1,users:1,accounts:0,listen:'127.0.0.1:8180',data_dir:'/fixture/data',config_path:'/fixture/config.json',started_at:Date.now()};
  else if(path === '/api/tunnel/status') body={enabled:true,transparent:true,client_transparent:true,connected:true,proxy_up:true,since:Date.now()-10000,remote:'192.0.2.10:1234',maps:[],rules:['db.corp:5432','10.20.0.0/16'],workspaces:[{session:'fixture-space',name:'Fixture workspace',ready:true}]};
  else if(path === '/api/tunnel/probe') { assert.equal(route.request().postDataJSON().target,'db.corp:5432'); body={ok:true,elapsed_ms:12}; }
  else if(path === '/api/sessions') body=sessions;
  else if(path.endsWith('/models') && path.startsWith('/api/sessions/')) body={models:settings.models.codex,default_reasoning:{support:'unknown',control:'effort'},discovery:'available'};
  else if(path === '/api/accounts') {
   if(route.request().method()==='POST') {
    const data=route.request().postDataJSON(); accountCreates++;
    body={...data,cred_status:'missing',sessions:0};accounts.push(body);
   } else body=accounts;
  } else if(path.startsWith('/api/accounts/') && route.request().method()==='PATCH') {
   const id=path.split('/')[3], account=accounts.find(a=>a.id===id);Object.assign(account,route.request().postDataJSON());body=account;
  } else if(path.endsWith('/oauth/start')) {
   body={url:'https://auth.example.invalid/authorize?state=fixture'};
  } else if(path.endsWith('/oauth/finish')) {
   oauthFinishes++;
   if(failOAuthOnce){failOAuthOnce=false;await route.fulfill({status:400,json:{error:'state 不匹配，请重新粘贴回调地址'}});return;}
   accounts.find(a=>a.id===path.split('/')[3]).auth_mode='oauth';
   accounts.find(a=>a.id===path.split('/')[3]).cred_status='ok'; body={ok:true};
  } else if(path.endsWith('/apikey')) {
   keyWrites++; accounts.find(a=>a.id===path.split('/')[3]).auth_mode='apikey';
   accounts.find(a=>a.id===path.split('/')[3]).cred_status='ok';body={ok:true};
  } else if(['/api/proxies','/api/users','/api/tunnel/clients'].includes(path) || path.endsWith('/files')) body=[];
  else if(path.endsWith('/history')) body={entries:historyEntries,thread:null,costs:historyCosts};
  else if(path === '/api/settings') {
   if(route.request().method()==='PUT'){settingsWrites++;settings={...settings,...route.request().postDataJSON()};}
   body=settings;
  } else if(path==='/api/monitor') {
   monitorCalls++;
   body={now:Date.now(),window_ms:0,process:{rss:0,heap_alloc:0,goroutines:1,uptime_ms:100},host:{cpu_count:1,load1:0,mem_total:0,mem_used:0},summary:{total:0,running:0,cpu_percent:0,mem_usage:0},containers:[]};
  } else if(path==='/api/usage/events') body={rows:[],total:{rows:0,turns:0,input_tokens:0,output_tokens:0,cache_read_tokens:0,cache_write_tokens:0,cost_micro_usd:0},facets:{users:[],agents:[],models:[]},scope:'all',timezone:'UTC',order:'desc',limit:20,offset:0,sync:{last_scan_at:Date.now(),last_success_at:Date.now(),scanning:false,errors:0}};
  await route.fulfill({json:body});
 });
 try {
  await page.goto(base + '/#/settings/container');
  await page.locator('#login-user').fill('fixture');await page.locator('#login-pass').fill('fixture-password');
  const password = page.locator('#login-pass'), reveal = page.locator('#login-password-toggle');
  assert.equal(await password.getAttribute('type'),'password');
  assert.equal(await reveal.getAttribute('data-icon'),'eye','hidden password offers the reveal action');
  assert.equal(await page.locator('.login-field-icon .ui-icon').count(),2);
  await reveal.click();
  assert.equal(await password.getAttribute('type'),'text');
  assert.equal(await reveal.getAttribute('aria-label'),'隐藏密码');
  assert.equal(await reveal.getAttribute('data-icon'),'eye-off','visible password offers the hide action');
  assert.equal(await password.inputValue(),'fixture-password');
  assert.equal(await page.locator('#login').isVisible(),true,'reveal must not submit login');
  await reveal.press('Space');
  assert.equal(await password.getAttribute('type'),'password');
  assert.equal(await password.inputValue(),'fixture-password');
  await reveal.click();
  await page.evaluate(async()=>{const {showLogin}=await import('/_v/{{BUILD}}/js/login.js');showLogin();});
  assert.equal(await password.getAttribute('type'),'password','returning to login resets visibility');
  await reveal.click();
  await page.locator('#login-btn').click();
  await page.locator('#app').waitFor({state:'visible'});
  assert.equal(await password.getAttribute('type'),'password');
  assert.equal(await password.inputValue(),'','successful login clears the password');
  await page.locator('#sec-container').waitFor({state:'visible'});
  assert.equal(new URL(page.url()).hash,'#/settings/container','login preserves destination');
  assert.equal(await page.locator('#login-btn').isDisabled(),false);
  // Transient /me errors must show a usable login form, including stored-token reload.
  await page.route('**/api/me',route=>route.fulfill({status:503,json:{error:'fixture unavailable'}}));
  await page.reload();
  await page.locator('#login').waitFor({state:'visible'});
  assert.match(await page.locator('#login-error').innerText(),/读取登录状态失败/);
  assert.equal(await page.locator('#login-btn').isDisabled(),false);
  await page.unroute('**/api/me');
  await page.locator('#login-user').fill('fixture');await page.locator('#login-pass').fill('fixture-password');
  await page.locator('#login-btn').click();
  await page.locator('#sec-container').waitFor({state:'visible'});
  // A stalled login request must time out and allow another attempt without reload.
  await page.evaluate(async()=>{const {showLogin}=await import('/_v/{{BUILD}}/js/login.js');showLogin();});
  await page.evaluate(()=>{window.__originalTimeout=AbortSignal.timeout;AbortSignal.timeout=()=>window.__originalTimeout(100);});
  await page.route('**/api/login',async route=>{await new Promise(resolve=>setTimeout(resolve,400));await route.abort().catch(()=>{});});
  await password.fill('fixture-password');await page.locator('#login-btn').click();
  await page.locator('#login-error').filter({hasText:'无法连接服务器'}).waitFor();
  assert.equal(await page.locator('#login-btn').isDisabled(),false,'timed out login remains disabled');
  await page.evaluate(()=>{AbortSignal.timeout=window.__originalTimeout;});
  await page.unroute('**/api/login');
  await password.fill('fixture-password');await page.locator('#login-btn').click();
  await page.locator('#sec-container').waitFor({state:'visible'});
  // Version entry, shared state, failure recovery, responsive layout and focus.
  await page.locator('#version-badge.has-update').waitFor();
  assert.equal(updateChecks,1,'initial automatic update check missing or duplicated');
  await page.locator('#version-badge').click();
  await page.locator('#version-menu').waitFor({state:'visible'});
  assert.match(await page.locator('#version-menu [data-update-status]').innerText(),/v0.2.0 可用/);
  await page.keyboard.press('Escape');
  await page.locator('#version-menu').waitFor({state:'hidden'});
  await page.locator('#version-badge').click();
  await page.locator('#version-upgrade').click();
  await page.locator('#sec-about').waitFor({state:'visible'});
  await page.waitForFunction(()=>location.hash==='#/settings/about');
  await page.locator('#update-notes-wrap summary').click();
  assert.match(await page.locator('#update-notes').innerText(),/<script>/,'release notes must stay plain text');
  assert.equal(await page.locator('#update-notes script').count(),0);
  await page.locator('#update-notes-wrap summary').click();
  const screenshot = async name => {
   if(process.env.AGENTBOX_UPDATE_SCREENSHOTS) {
    await mkdir('output/playwright',{recursive:true});
    await page.screenshot({path:`output/playwright/updates-${name}.png`,animations:'disabled'});
   }
  };
  const check = () => page.locator('#sec-about [data-update-check]').click();
  const status = text => page.locator('#sec-about [data-update-status]').filter({hasText:text}).waitFor();
  await page.setViewportSize({width:1440,height:960});
  for(const theme of ['dark','light']) {
   await page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
   await page.locator('#version-badge').click();
   await screenshot(theme);
   const bounds=await page.locator('#version-menu').boundingBox();
   assert.ok(bounds.x>=0 && bounds.x+bounds.width<=1440,'desktop popover outside viewport');
   await page.keyboard.press('Escape');
  }
  await page.evaluate(()=>document.documentElement.dataset.theme='dark');
  nextRelease={available:true,error:'暂时无法获取发布信息，请稍后重试。'};
  await check(); await status('上次检查结果');
  assert.equal(await page.locator('#version-badge.has-update').count(),1,'failure lost update indicator');
  nextRelease={current_version:'v0.1.0',latest_version:'v0.1.0',available:false,error:''};
  await check(); await status('已是最新版本');
  assert.equal(await page.locator('#version-badge.has-update').count(),0);
  await page.locator('#version-badge').click();
  assert.equal(await page.locator('#version-current').isVisible(),true);
  await screenshot('current');
  await page.keyboard.press('Escape');
  nextRelease={error:'暂时无法获取发布信息，请稍后重试。'};
  await check(); await status('检查未完成');
  nextRelease={latest_version:'',error:''};
  await check(); await status('暂无正式发布的版本');
  nextRelease={current_version:'dev',latest_version:'v0.2.0',comparable:false};
  await check(); await status('开发构建，无法比较版本');
  nextRelease={current_version:'v0.1.0-rc.2-updates2',latest_version:'v0.2.0',comparable:true,available:true};
  await check(); await status('v0.2.0 可用');
  // Upgrade confirmation, lost submission response, reload recovery and actual-version verification.
  const upgradeMessage = text => page.locator('#upgrade-message').filter({hasText:text}).waitFor();
  await page.locator('#update-install').click();
  await page.locator('#ask-cancel').click();
  assert.equal(upgradeStarts,0,'cancelled confirmation submitted an upgrade');
  loseUpgradeResponse=true;
  await page.locator('#update-install').click();
  await page.locator('#ask-ok').click();
  await upgradeMessage('正在下载发布包');
  assert.equal(upgradeStarts,1,'lost response repeated POST');
  assert.equal(await page.locator('#update-install').isDisabled(),true);
  await screenshot('upgrading');
  await page.setViewportSize({width:390,height:844});
  await screenshot('upgrading-mobile');
  assert.equal(await page.locator('#sec-about').evaluate(el=>el.scrollWidth<=el.clientWidth),true,'mobile upgrade progress overflows');
  await page.setViewportSize({width:1440,height:960});
  await page.reload();
  await page.locator('#sec-about').waitFor({state:'visible'});
  await upgradeMessage('正在下载发布包');
  assert.equal(upgradeStarts,1,'page reload restarted upgrade');
  upgradeOffline=true;
  await page.locator('#upgrade-refresh').click();
  await page.locator('#upgrade-error').filter({hasText:'等待恢复'}).waitFor();
  upgradeOffline=false;
  upgrade.job={...upgrade.job,phase:'failed',message:'升级失败',error:'校验发布包失败'};
  await page.locator('#upgrade-refresh').click();
  await page.locator('#upgrade-error').filter({hasText:'校验发布包失败'}).waitFor();
  assert.equal(await page.locator('#update-install').isEnabled(),true);
  upgrade.job={...upgrade.job,phase:'succeeded',message:'升级完成',error:''};
  await page.locator('#upgrade-refresh').click();
  await upgradeMessage('当前运行版本与目标不一致');
  assert.equal(await page.locator('#upgrade-reload').isVisible(),false,'claimed success before actual version matched');
  upgrade.current_version='v0.2.0';
  await page.locator('#upgrade-refresh').click();
  await upgradeMessage('当前运行 v0.2.0');
  assert.equal(await page.locator('#upgrade-reload').isVisible(),true);
  upgrade={supported:false,reason:'当前部署请使用手工升级。',current_version:release.current_version,job:null};
  await page.locator('#upgrade-refresh').click();
  await page.locator('#upgrade-support').filter({hasText:'手工升级'}).waitFor();
  assert.equal(await page.locator('#update-install').isVisible(),false);
  upgrade={supported:true,reason:'',current_version:release.current_version,job:null};
  await check(); await status('v0.2.0 可用');
  await page.locator('#btn-sidebar-toggle').click();
  await page.locator('#version-badge').click();
  await screenshot('collapsed');
  await page.keyboard.press('Escape');
  await page.locator('#btn-sidebar-toggle').click();
  await page.setViewportSize({width:390,height:844});
  await screenshot('mobile-about');
  assert.equal(await page.locator('#sec-about').evaluate(el=>el.scrollWidth<=el.clientWidth),true,'mobile update panel overflows');
  await page.locator('#btn-menu').click();
  await page.locator('#version-badge').click();
  await screenshot('mobile-menu');
  const mobileBounds=await page.locator('#version-menu').boundingBox();
  assert.ok(mobileBounds.x>=0 && mobileBounds.x+mobileBounds.width<=390,'mobile popover outside viewport');
  await page.keyboard.press('Escape');
  await page.locator('#btn-sidebar-close').click();
  await page.setViewportSize({width:1280,height:900});
  // Reload restores the shared server cache without another upstream check.
  const beforeReload=updateChecks;
  await page.reload();
  await page.locator('#version-badge.has-update').waitFor();
  assert.equal(updateChecks,beforeReload,'fresh cache triggered another automatic check');
  // Open through public event used by sidebar; user UI may nest the button in a menu.
  await page.evaluate(async () => { const {emit}=await import('/_v/{{BUILD}}/js/state.js');emit('open-settings'); });
  await page.locator('#set-nav [data-sec="container"]').click();
  await page.locator('#set-running').fill('4');
  await page.locator('#btn-save-resources').click();
  await page.waitForFunction(() => document.querySelector('#set-running').value==='4');
  assert.equal(settingsWrites,1);
  assert.equal(settings.resources.max_running,4);
  // Reinitialization must not double-bind form submission.
  await page.evaluate(async () => { const m=await import('/_v/{{BUILD}}/js/settings.js');m.initSettings();m.initSettings(); });
  await page.locator('#btn-save-resources').click();
  await page.waitForTimeout(100);assert.equal(settingsWrites,2);
  // Account creation stays in one dialog; authorization failure is retryable.
  await page.locator('#set-nav [data-sec="accounts"]').click();
  await page.locator('#btn-acct-add').click();
  await page.locator('#acct-form label.agent-codex').click();
  await page.locator('#acct-id').fill('codex-oauth-fixture');
  await page.locator('#acct-label').fill('Codex subscription fixture');
  assert.match(await page.locator('#auth-oauth-hint').innerText(),/localhost:1455/);
  if(process.env.AGENTBOX_ACCOUNT_SCREENSHOTS) await page.screenshot({path:'/tmp/agentbox-account-desktop.png',animations:'disabled'});
  await page.locator('#auth-gen').click();
  await page.locator('#auth-linkrow').waitFor({state:'visible'});
  assert.equal(accountCreates,1);assert.equal(accounts[0].type,'codex');
  assert.equal(await page.locator('#acct-id').isDisabled(),true);
  await page.locator('#auth-code').fill('http://localhost:1455/auth/callback?code=fixture&state=fixture');
  await page.locator('#auth-finish').click();
  await page.locator('#auth-msg').filter({hasText:'state 不匹配'}).waitFor();
  assert.equal(await page.locator('#dlg-auth').isVisible(),true);
  await page.locator('#auth-finish').click();
  await page.locator('#dlg-auth').waitFor({state:'hidden'});
  assert.equal(accountCreates,1);assert.equal(oauthFinishes,2);
  for (const type of ['claude','codex']) {
   await page.locator('#btn-acct-add').click();
   await page.locator(`#acct-form label.agent-${type}`).click();
   await page.locator('#acct-id').fill(type+'-key-fixture');
   await page.locator('#auth-mode-key').click();
   assert.equal(await page.locator('#auth-key').isVisible(),true);
   assert.equal(await page.locator('#auth-oauth').isVisible(),false);
   await page.locator('#auth-apikey').fill('synthetic-key');
   await page.locator('#auth-baseurl').fill('https://relay.example.invalid/v1');
   await page.locator('#auth-savekey').click();
   await page.locator('#dlg-auth').waitFor({state:'hidden'});
  }
  assert.equal(accountCreates,3);assert.equal(keyWrites,2);
  const acctRow = page.locator('.acct-row').filter({hasText:'codex-key-fixture'});
  await acctRow.getByRole('button',{name:'模型能力',exact:true}).click();
  await page.locator('#ask-input-field').fill('fixture');await page.locator('#ask-input-ok').click();
  const overrideDialog=page.locator('dialog[open]').filter({has:page.locator('select[name="support"]')});
  await overrideDialog.locator('label').filter({hasText:'支持范围'}).getByRole('combobox').click();
  await overrideDialog.getByRole('option',{name:'不支持调整',exact:true}).click();
  await assertActionIcons(page);await overrideDialog.getByRole('button',{name:'保存',exact:true}).click();
  await page.waitForTimeout(150);
  assert.equal(accounts.find(a=>a.id==='codex-key-fixture').model_reasoning.fixture.support,'unsupported');

  await page.setViewportSize({width:390,height:844});
  await page.locator('#btn-acct-add').click();
  await page.locator('#acct-form label.agent-codex').click();
  await page.locator('#acct-id').fill('mobile-fixture');
  await page.locator('#auth-mode-key').click();
  if(process.env.AGENTBOX_ACCOUNT_SCREENSHOTS) await page.screenshot({path:'/tmp/agentbox-account-mobile.png',animations:'disabled'});
  assert.equal(await page.locator('#dlg-auth').evaluate(el=>el.scrollWidth<=el.clientWidth),true,'mobile dialog overflows');
  await page.locator('#auth-close').click();
  assert.equal(accountCreates,3,'closing untouched form created an account');
  await page.setViewportSize({width:1280,height:900});
  await page.locator('#set-nav [data-sec="monitor"]').click();
  await page.waitForTimeout(100);assert.equal(monitorCalls,1);
  await page.evaluate(async()=>{ const {emit}=await import('/_v/{{BUILD}}/js/state.js');emit('open-usage'); });
  await page.locator('#view-usage').waitFor({state:'visible'});
  await page.locator('#usage-sub').filter({hasText:'终端全量扫描'}).waitFor();
  await page.evaluate(async()=>{ const {emit}=await import('/_v/{{BUILD}}/js/state.js');emit('unauthorized'); });
  await page.locator('#login').waitFor({state:'visible'});
  await page.waitForTimeout(5200);assert.equal(monitorCalls,1,'monitor leaked after leaving view/login');
  const state=await page.evaluate(async()=>{const {S}=await import('/_v/{{BUILD}}/js/state.js');return {user:S.user,current:S.current,token:S.token};});
  assert.deepEqual(state,{user:'',current:null,token:''});
  // Connection generations drop stale messages and dispose cancels pending retry.
  const result=await page.evaluate(async()=>{
   const {ChatConnection}=await import('/_v/{{BUILD}}/js/features/chat/connection.js');
   const RealWS=window.WebSocket, sockets=[]; let messages=0;
   class FakeWS { static OPEN=1;readyState=1; constructor(){sockets.push(this);} close(){} send(){} }
   window.WebSocket=FakeWS;
   try {
    const c=new ChatConnection({message:()=>messages++,state:()=>{},reconnect:()=>{}});
    c.connect('ws://fixture/one');c.connect('ws://fixture/two');
    sockets[0].onmessage({data:'stale'});sockets[1].onmessage({data:'current'});
    sockets[1].onclose({code:1006});c.dispose();
    await new Promise(r=>setTimeout(r,1200));return {messages,opened:sockets.length};
   } finally { window.WebSocket=RealWS; }
  });
  assert.deepEqual(result,{messages:1,opened:2});assert.deepEqual(errors,[]);
  await page.setViewportSize({width:390,height:844});
  await page.locator('#login-user').fill('fixture');await page.locator('#login-pass').fill('fixture-password');await page.locator('#login-btn').click();
  await page.locator('#app').waitFor({state:'visible'});
  // URL navigation survives refresh, history traversal and a fresh login.
  await page.setViewportSize({width:1280,height:900});
  const at = async (hash, selector) => {
   await page.waitForFunction(hash => location.hash === hash, hash);
   await page.locator(selector).waitFor({state:'visible'});
  };
  const open = event => page.evaluate(async event => {
   const {emit}=await import('/_v/{{BUILD}}/js/state.js');emit(event);
  }, event);
  await at('#/usage','#view-usage');
  await page.reload();
  await at('#/usage','#view-usage');
  await open('open-settings');
  await page.locator('#set-nav [data-sec="container"]').click();
  await at('#/settings/container','#sec-container');
  await page.reload();
  await at('#/settings/container','#sec-container');
  await page.locator('#set-nav [data-sec="models"]').click();
  await at('#/settings/models','#sec-models');
  const capabilityRow = page.locator('.model-row').filter({hasText:'Fixture Lite'});
  await capabilityRow.getByRole('button',{name:'推理强度 · low / high'}).click();
  const capabilityDialog = page.locator('dialog[open]').filter({hasText:'模型能力'});
  await capabilityDialog.locator('input[value="low"]').uncheck();
  await page.setViewportSize({width:390,height:844});
  assert.equal(await capabilityDialog.evaluate(el=>el.scrollWidth<=el.clientWidth),true,'capability dialog overflows');
  await mkdir(resolve('output/playwright'),{recursive:true});
  await page.screenshot({path:resolve('output/playwright/reasoning-editor-mobile.png')});
  await assertActionIcons(page);await capabilityDialog.getByRole('button',{name:'保存',exact:true}).click();
  await capabilityRow.getByRole('button',{name:'模型能力 · 推理强度 · high',exact:true}).waitFor();
  assert.deepEqual(settings.models.codex.find(m=>m.id==='fixture-lite').reasoning.levels,['high']);
  await capabilityRow.getByRole('button',{name:'模型能力 · 推理强度 · high',exact:true}).click();
  await capabilityDialog.locator('input[value="low"]').check();
  await assertActionIcons(page);await capabilityDialog.getByRole('button',{name:'保存',exact:true}).click();
  await capabilityRow.getByRole('button',{name:'推理强度 · low / high'}).waitFor();
  await page.setViewportSize({width:1280,height:900});
  await page.goBack();
  await at('#/settings/container','#sec-container');
  await page.goForward();
  await at('#/settings/models','#sec-models');
  await open('open-tunnel');
  await at('#/tunnel','#view-tunnel');
  await page.locator('#tun-network').waitFor({state:'visible'});
  assert.match(await page.locator('#tun-modes').innerText(),/默认推荐/);
  assert.match(await page.locator('#tun-modes').innerText(),/暂不推荐/);
  assert.match(await page.locator('#tun-mode-current').innerText(),/当前连接：透明模式/);
  assert.match(await page.locator('#tun-network-rules').innerText(),/db.corp:5432/);
  assert.match(await page.locator('#tun-network-workspaces').innerText(),/网络就绪/);
  await page.locator('#tun-probe-target').fill('db.corp:5432');
  await page.locator('#tun-probe').click();
  await page.locator('#tun-probe-result').filter({hasText:'12 ms'}).waitFor();
  await mkdir(resolve('output/playwright'),{recursive:true});
  await page.screenshot({path:resolve('output/playwright/transparent-network.png')});
  await page.reload();
  await at('#/tunnel','#view-tunnel');
  await page.locator('[data-session-id="fixture-space"]').click();
  await at('#/sessions/fixture-space/chat','#tab-chat');
  // Model capabilities: compatible choices survive, incompatible/unknown ones reset.
  await page.setViewportSize({width:1280,height:900});
  const pick = async (kind, label) => {
   await page.locator('#btn-pick').click();
   await page.locator('#pick-menu .pick-row').filter({hasText:kind}).hover();
   await page.locator('#pick-fly .pick-opt').filter({hasText:label}).click();
  };
  await page.locator('#btn-pick').filter({hasText:'Fixture'}).waitFor();
  await pick('推理强度','极高');
  await pick('模型','Fixture Lite');
  assert.match(await page.locator('#btn-pick').innerText(),/跟随 CLI 默认/);
  await pick('推理强度','高');
  await page.locator('#chat-input').fill('synthetic reasoning message');
  await page.locator('#chat-send').click();
  await page.waitForTimeout(100);
  assert.equal(chatMessages.at(-1).effort,'high');
  assert.equal(chatMessages.at(-1).effort_control,'effort');
  assert.equal(chatMessages.at(-1).model,'fixture-lite');
  await pick('模型','No adjustment');
  assert.match(await page.locator('#btn-pick').innerText(),/不支持调整/);
  await pick('模型','Custom');
  await page.locator('#btn-pick').click();
  await page.locator('#pick-menu .pick-row').filter({hasText:'推理强度'}).hover();
  assert.equal(await page.locator('#pick-fly .pick-opt').count(),2,'unknown model must require manual opt-in');
  await page.locator('#pick-fly .pick-opt').filter({hasText:'手动指定'}).click();
  await page.locator('#pick-menu .pick-opt').filter({hasText:'极高'}).click();
  await page.reload();
  await page.locator('#btn-pick').filter({hasText:'极高'}).waitFor();
  // Seed the legacy fixture before the next document's application scripts.
  // The pill can render before /models finishes; writing in the live page
  // races with refreshModelCapabilities saving its current v2 selection.
  await page.addInitScript(origin => {
   if (window !== window.top || location.origin !== origin) return;
   const seeded = 'agentbox_test_legacy_pick_seeded';
   if (sessionStorage.getItem(seeded)) return;
   localStorage.setItem('agentbox_pick_fixture-space',JSON.stringify({model:'fixture-lite',effort:'xhigh'}));
   sessionStorage.setItem(seeded,'1');
  },base);
  await page.reload();
  await page.locator('#btn-pick').filter({hasText:'Fixture Lite'}).waitFor();
  assert.match(await page.locator('#btn-pick').innerText(),/跟随 CLI 默认/,'v1 persisted choice must migrate safely');
  await page.setViewportSize({width:390,height:844});
  await page.locator('#btn-pick').click();
  await page.locator('#pick-menu .pick-row').filter({hasText:'推理强度'}).click();
  assert.equal(await page.locator('#pick-menu .pick-opt').count(),3);
  await mkdir(resolve('output/playwright'),{recursive:true});
  await page.screenshot({path:resolve('output/playwright/reasoning-mobile.png')});
  await page.locator('#pick-menu .pick-opt').filter({hasText:'轻度'}).click();
  await page.setViewportSize({width:1280,height:900});
  await chatFooterSmoke(page,{setHistory:(entries,costs={})=>{historyEntries=entries;historyCosts=costs;},send:msg=>chatSocket.send(JSON.stringify(msg))});
  await page.locator('.tab[data-tab="files"]').click();
  await at('#/sessions/fixture-space/files','#tab-files');
  await page.reload();
  await at('#/sessions/fixture-space/files','#tab-files');
  await fileActionSmoke(page);
  await page.locator('#btn-home').click();
  await at('#/','#empty');
  await page.goBack();
  await at('#/sessions/fixture-space/files','#tab-files');
  await page.goto(base + '/#/sessions/fixture-space/skills');
  await at('#/sessions/fixture-space/chat','#tab-chat'); // Codex has no skills tab.
  // Mobile keys go through the real xterm input and binary WebSocket path.
  await page.setViewportSize({width:390,height:844});
  await page.locator('.tab[data-tab="term"]').click();
  await page.locator('#term-state[data-state="connected"]').waitFor();
  const key = name => page.locator(`[data-term-key="${name}"]`);
  const modifier = name => page.locator(`[data-term-mod="${name}"]`);
  const lastInput = async expected => {
   await page.waitForTimeout(50);
   assert.equal(terminalInput.at(-1),expected);
  };
  await key('escape').click(); await lastInput('\x1b');
  await key('tab').click(); await lastInput('\t');
  await key('shift-tab').click(); await lastInput('\x1b[Z');
  await key('interrupt').click(); await lastInput('\x03');
  assert.equal(await page.locator('.xterm-helper-textarea').evaluate(el=>el===document.activeElement),true);
  await modifier('ctrl').click();
  assert.equal(await modifier('ctrl').getAttribute('aria-pressed'),'true');
  await page.keyboard.type('c'); await lastInput('\x03');
  await page.keyboard.type('c'); await lastInput('c');
  assert.equal(await modifier('ctrl').getAttribute('aria-pressed'),'false');
  await modifier('alt').click(); await page.keyboard.type('b'); await lastInput('\x1bb');
  await key('up').click(); await lastInput('\x1b[A');
  terminalSocket.send('\x1b[?1h'); // Vim/tmux application cursor mode.
  await page.waitForFunction(async()=> (await import('/_v/{{BUILD}}/js/state.js')).S.term.modes.applicationCursorKeysMode);
  await key('up').click(); await lastInput('\x1bOA');
  await modifier('ctrl').click(); await key('left').click(); await lastInput('\x1b[1;5D');
  await key('|').click(); await lastInput('|');
  await page.locator('#term-keyboard').click();
  assert.equal(await page.locator('.xterm-helper-textarea').evaluate(el=>el===document.activeElement),false);
  await page.locator('#term-keyboard').click();
  assert.equal(await page.locator('.xterm-helper-textarea').evaluate(el=>el===document.activeElement),true);
  // Emulate a keyboard shrinking/panning only the visual viewport (iOS behavior).
  await page.evaluate(()=>{
   Object.defineProperty(visualViewport,'height',{configurable:true,value:440});
   Object.defineProperty(visualViewport,'offsetTop',{configurable:true,value:18});
   visualViewport.dispatchEvent(new Event('resize'));
  });
  const keysBounds = await page.locator('#term-keys').boundingBox();
  assert.ok(keysBounds.y+keysBounds.height<=458 && keysBounds.y>=18,'keys obscured by keyboard');
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
  await page.locator('.term-keys-scroll').evaluate(el=>{el.scrollLeft=0;});
  await mkdir(resolve('output/playwright'),{recursive:true});
  await page.screenshot({path:resolve('output/playwright/terminal-mobile-keyboard.png')});
  await page.evaluate(()=>{
   delete visualViewport.height; delete visualViewport.offsetTop;
   visualViewport.dispatchEvent(new Event('resize'));
  });
  await modifier('ctrl').click();
  await page.locator('.tab[data-tab="chat"]').click();
  await page.waitForFunction(()=>!document.querySelector('#app').classList.contains('term-viewport'));
  assert.equal(await modifier('ctrl').getAttribute('aria-pressed'),'false');
  await page.locator('.tab[data-tab="term"]').click();
  terminalSocket.close({code:4003,reason:'fixture quota'});
  await page.locator('#term-state[data-state="closed"]').waitFor();
  assert.equal(await key('interrupt').isDisabled(),true);
  await page.setViewportSize({width:1280,height:900});
  assert.equal(await page.locator('#term-keys').isVisible(),false,'desktop toolbar should stay hidden');
  sessions = [];
  await page.reload();
  await at('#/','#empty'); // Deleted or inaccessible workspace.
  await responsiveSmoke(page, base);
  await pricingSmoke(page);
  const checksBeforeUser=updateChecks, readsBeforeUser=updateReads, upgradesBeforeUser=upgradeReads;
  me.role = 'user';
  await page.reload();
  await page.locator('#app').waitFor({state:'visible'});
  await page.goto(base + '/#/settings/security');
  await at('#/','#empty');
  await page.goto(base + '/#/sessions/%E0%A4%A/files');
  await at('#/','#empty'); // Malformed URL must not break initialization.
  assert.equal(await page.locator('#version-badge').isVisible(),false);
  assert.equal(updateChecks,checksBeforeUser,'ordinary user initiated update check');
  assert.equal(updateReads,readsBeforeUser,'ordinary user fetched update metadata');
  assert.equal(upgradeReads,upgradesBeforeUser,'ordinary user fetched upgrade status');
  assert.deepEqual(errors,[]);
  console.log('Browser: model capabilities/editor/account overrides/safe selection migration, update status/cache/permissions, responsive update popover, unified account creation, Codex OAuth retry, Claude/Codex API keys, mobile account form, login, capacity save, repeated init, usage sync, monitor cleanup, socket generation, mobile re-login, URL refresh/history/deep links/permissions passed');
 } finally { await page.unroute('**/api/**'); server.closeAllConnections(); await new Promise(r=>server.close(r)); }
}
if (process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)) {
 const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
 const browser=await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL ? {channel:process.env.AGENTBOX_BROWSER_CHANNEL}: {})});
 try { await smoke(await browser.newPage()); } finally { await browser.close(); }
}
