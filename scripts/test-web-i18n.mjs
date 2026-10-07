#!/usr/bin/env node
// Locale behavior against synthetic HTTP/WS only; no real users or model calls.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { extname, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

export async function webI18nSmoke(browser) {
 const root = fileURLToPath(new URL('../internal/web/static/', import.meta.url));
 const server = createServer(async (req,res) => {
  try {
   if (req.url === '/language-peer') { res.setHeader('Content-Type','text/html'); res.end('<!doctype html><title>Language fixture</title>'); return; }
   const rel = new URL(req.url,'http://fixture').pathname.replace(/^\/_v\/[^/]+/,'');
   const path = resolve(root,'.'+(rel==='/'?'/index.html':rel));
   if (!path.startsWith(root)) { res.writeHead(403).end(); return; }
   res.setHeader('Content-Type',({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'})[extname(path)] || 'application/octet-stream');
   res.end(await readFile(path));
  } catch {res.writeHead(404).end();}
 });
 await new Promise(r=>server.listen(0,'127.0.0.1',r));
 const base=`http://127.0.0.1:${server.address().port}`;
 const context=await browser.newContext({locale:'en-GB', viewport:{width:1280,height:900}});
 const page=await context.newPage();
 const errors=[]; page.on('pageerror',e=>errors.push(e.stack||e.message));
 const space={id:'locale-space',name:'保存',agent:'codex',account_id:'fixture',account_label:'取消',status:'running',default_model:'fixture'};
 let sockets=0, writes=0, usageReads=0, terminalSocket;
 const usageRows=Array.from({length:20},(_,i)=>({id:i+1,ts:Date.now(),user:'保存',session_id:space.id,session_name:space.name,thread_id:'thread',turn_id:'turn-'+i,agent:'claude',account_id:'fixture',model:'保存',kind:'chat',billing:'table',input_tokens:100,output_tokens:1000,cache_read_tokens:80,cache_write_tokens:10,total_tokens:1190,cost_micro_usd:2128,ttft_ms:100,wall_ms:1000,duration_ms:900,rate:{input:1,output:2,cache_read:0.1,cache_write:2,snapshot:true,per_request:true,key:'保存'}}));
 await page.routeWebSocket('**/api/sessions/*/term?*',ws=>{sockets++; terminalSocket=ws; ws.send(Buffer.from('保存 Cancel <code>\r\n'));});
 await page.routeWebSocket('**/api/sessions/*/chat?*',()=>{});
 await page.route('**/api/**',async route=>{
  const path=new URL(route.request().url()).pathname;
  if (route.request().method()!=='GET' && path!=='/api/updates/check') writes++;
  if(path==='/api/usage/events') usageReads++;
  let body={};
  if(path==='/api/login')body={token:'synthetic-locale-token'};
  else if(path==='/api/me')body={user:'保存',role:'admin',timezone:'Asia/Tokyo',models:{claude:[],codex:[]},quota:{metered:false}};
  else if(path==='/api/sessions')body=[space];
  else if(path==='/api/session-creations')body={version:1,actor_key:'a'.repeat(64),creations:[]};
  else if(path==='/api/onboarding')body={version:1,can_configure:true,can_create:true,has_workspaces:true,accounts:[{id:'fixture',label:'取消',type:'codex',credentials_present:true}],default_models:{codex:'fixture'},container_resources:{cpus:1,memory_mb:512,pids_limit:128}};
  else if(path==='/api/accounts')body=[{id:'fixture',label:'取消',type:'codex',cred_status:'ok',sessions:1}];
  else if(path==='/api/settings')body={listen:'127.0.0.1:8180',agent_image:'fixture',permission_mode:'default',max_upload_mb:20,idle_timeout_min:30,timezone:'Asia/Tokyo',container:{memory_mb:512,cpus:1,pids_limit:128,network:'none'},resources:{max_running:0,max_running_per_user:0,min_free_bytes:0},models:{claude:[],codex:[]},default_models:{claude:'fixture',codex:'fixture'},terminal_tips:{tips:['保存'],interval_sec:0,animation:'none'},tunnel:{enabled:false},proxy_bridge:{bind:'127.0.0.1:1081'},pricing:{}};
  else if(path.endsWith('/models'))body={models:[],discovery:'available'};
  else if(path.endsWith('/files'))body=[];
  else if(path.endsWith('/chat/threads'))body={threads:[],active:null};
  else if(path.endsWith('/history'))body={entries:[],thread:null,costs:{}};
  else if(path==='/api/git/connections'||path==='/api/users'||path==='/api/tunnel/clients')body=[];
  else if(path==='/api/me/git')body={user:'保存',name:'取消',email:'fixture@example.invalid'};
  else if(path==='/api/me/git/default')body={connection_id:''};
  else if(path==='/api/proxies')body={proxies:[],bridge_up:false,bridge_host:'127.0.0.1'};
  else if(path==='/api/usage/events')body={rows:usageRows,total:{rows:20,turns:20,input_tokens:2000,output_tokens:20000,cache_read_tokens:1600,cache_write_tokens:200,cost_micro_usd:42560},facets:{users:[],agents:[],models:[]},scope:'all',timezone:'Asia/Tokyo',order:'desc',limit:20,offset:0,sync:{last_scan_at:Date.now(),last_success_at:Date.now(),scanning:false,errors:0}};
  else if(path==='/api/updates'||path==='/api/updates/check')body={current_version:'v0.1.9',revision:'0123456789abcdef',built_at:'2026-10-04T08:00:00Z',latest_version:'',available:false,comparable:true,checked_at:0,attempted_at:0,error:''};
  else if(path==='/api/updates/components')body={version:1,observed_at:Date.now(),server:{version:'v0.1.9',schema:11,candidate_compatibility:'preflight_required'},image:{reference:'fixture',id:'',claude:'',codex:'',state:'unavailable',observed_at:0},desktop:{installed_version_state:'browser_unknown',server_protocol:1,sync_enabled:false}};
  else if(path==='/api/updates/upgrade')body={supported:false,reason:'fixture',current_version:'v0.1.9',job:null};
  await route.fulfill({json:body});
 });
 const language=async value=>{await page.evaluate(async value=>{const {i18n}=await import('/_v/{{BUILD}}/js/i18n.js');i18n.setLanguage(value);},value);};
 try {
  await page.goto(base);
  await page.locator('#login').waitFor({state:'visible'});
  assert.equal(await page.locator('html').getAttribute('lang'),'en');
  assert.equal((await page.locator('#login-btn').textContent()).trim(),'Sign in',await page.locator('#login-btn').evaluate(el=>el.outerHTML));
  await page.locator('#login-user').fill('保存');
  await page.locator('#login-pass').fill('fixture-password');
  // Exercise the visible shared dropdown, not only the runtime API.
  await page.locator('#login-language + .select-trigger').click();
  await page.getByRole('option',{name:'繁體中文',exact:true}).click();
  assert.equal(await page.locator('html').getAttribute('lang'),'zh-TW');
  assert.equal(await page.locator('#login-user').inputValue(),'保存');
  assert.equal(await page.locator('#login-pass').inputValue(),'fixture-password');
  assert.equal(await page.locator('#login-btn svg').count(),1,'switch preserves action icon');
  await language('en');
  await page.reload();
  await page.locator('#login').waitFor({state:'visible'});
  assert.equal(await page.locator('html').getAttribute('lang'),'en');
  await page.locator('#login-user').fill('保存'); await page.locator('#login-pass').fill('fixture-password');
  await page.locator('#login-btn').click();
  await page.locator('#app').waitFor({state:'visible'});
  const afterLoginWrites=writes;
  await page.locator('#btn-new').click();
  await page.locator('#new-name').fill('取消 <draft>');
  await page.locator('#new-name').evaluate(el=>{window.localeDraftInput=el;el.focus();el.setSelectionRange(1,3);});
  await language('zh-CN');
  assert.equal(await page.locator('#new-name').inputValue(),'取消 <draft>');
  assert.equal(await page.locator('#new-name').evaluate(el=>el===window.localeDraftInput && el===document.activeElement && el.selectionStart===1 && el.selectionEnd===3),true);
  assert.equal((await page.locator('#new-title').textContent()).trim(),'创建 Agent 工作空间');
  assert.equal(await page.locator('#new-ok svg').count(),1);
  await language('en');
  assert.equal((await page.locator('#new-title').textContent()).trim(),'Create an Agent workspace');
  assert.equal(await page.locator('#new-name').inputValue(),'取消 <draft>');
  assert.equal(writes,afterLoginWrites,'language switch must not save drafts');
  await page.locator('#new-cancel').click();
  await page.goto(base+'/#/sessions/locale-space/term');
  await page.locator('#term-state[data-state="connected"]').waitFor();
  const connected=sockets;
  await page.evaluate(()=>{window.localeTerminal=document.querySelector('.xterm');});
  await language('zh-TW'); await language('en');
  assert.equal(sockets,connected,'language change must not reconnect terminal');
  assert.equal(await page.evaluate(()=>document.querySelector('.xterm')===window.localeTerminal),true);
  assert.equal(await page.locator('#wb-name').textContent(),'保存','user workspace name stays verbatim');
  assert.match(await page.locator('#term-state').textContent(),/connected/i);
  assert.equal(await page.evaluate(async()=>{const {S}=await import('/_v/{{BUILD}}/js/state.js');return S.timeZone;}),'Asia/Tokyo');
  // Server refusal reasons override the generic connection label. Keep the raw
  // diagnostic and terminal buffer intact when the interface language changes.
  const refusal='账号授权已撤销；permission denied <code> 保存';
  await terminalSocket.close({code:4004,reason:refusal});
  await page.waitForFunction(reason=>document.querySelector('#term-state').textContent===reason,refusal);
  const terminalText=()=>page.evaluate(async()=>{
   const {S}=await import('/_v/{{BUILD}}/js/state.js');
   const buffer=S.term.buffer.active;
   return Array.from({length:buffer.length},(_,i)=>buffer.getLine(i)?.translateToString()).join('\n');
  });
  await page.waitForFunction(async reason=>{
   const {S}=await import('/_v/{{BUILD}}/js/state.js');
   const buffer=S.term.buffer.active;
   return Array.from({length:buffer.length},(_,i)=>buffer.getLine(i)?.translateToString()).join('\n').includes(reason);
  },refusal);
  const refusedOutput=await terminalText();
  for (const preference of ['zh-CN','zh-TW','en']) {
   await language(preference);
   assert.equal(await page.locator('#term-state').textContent(),refusal,'language change preserves the raw server refusal reason');
   assert.equal(await page.locator('#term-state').getAttribute('data-state'),'closed');
   assert.equal(await terminalText(),refusedOutput,'language change preserves terminal diagnostics and output');
  }
  assert.equal(sockets,connected,'language change must not reconnect a refused terminal');
  assert.equal(await page.evaluate(()=>document.querySelector('.xterm')===window.localeTerminal),true);
  // Explicit bindings must never translate user content, even if it matches a key.
  await page.evaluate(async()=>{
   const {setText,setTextRender,htmlText,t}=await import('/_v/{{BUILD}}/js/i18n.js');
   const {decorateIcons}=await import('/_v/{{BUILD}}/js/icons.js');
   const {btnBusy}=await import('/_v/{{BUILD}}/js/util.js');
   const host=document.createElement('div');document.body.append(host);
   const button=document.createElement('button');button.id='locale-busy';button.className='btn';button.dataset.icon='save';host.append(button);setText(button,'保存');decorateIcons(host);btnBusy(button,()=>t('读取中…'));
   const ui=document.createElement('span'); ui.id='locale-owned';document.body.append(ui);setText(ui,'保存');
   const raw=document.createElement('span');raw.id='locale-raw';raw.textContent='保存';document.body.append(raw);
   const replaced=document.createElement('span');replaced.id='locale-replaced';document.body.append(replaced);setText(replaced,'保存');replaced.textContent='保存';
   const mixed=document.createElement('span');mixed.id='locale-mixed';document.body.append(mixed);setTextRender(mixed,()=>t('删除 {name}',{name:'保存 <script>'}));
   const html=document.createElement('span');html.id='locale-html';html.innerHTML=htmlText('名称 {name}',{name:'&lt;script&gt;保存&lt;/script&gt;'});document.body.append(html);
  });
  await language('zh-TW');
  assert.equal(await page.locator('#locale-raw').textContent(),'保存');
  assert.equal(await page.locator('#locale-replaced').textContent(),'保存');
  assert.equal(await page.locator('#locale-html script').count(),0);
  await language('en');
  assert.equal(await page.locator('#locale-owned').textContent(),'Save');
  assert.equal((await page.locator('#locale-busy').textContent()).trim(),'Loading…');
  await page.evaluate(async()=>{const {btnDone}=await import('/_v/{{BUILD}}/js/util.js');btnDone(document.querySelector('#locale-busy'));});
  assert.equal((await page.locator('#locale-busy').textContent()).trim(),'Save');
  assert.equal(await page.locator('#locale-busy svg').count(),1,'restore the original icon and translated saved caption');
  await page.locator('#btn-usagelog').click();
  await page.locator('#uf-date-range').waitFor({state:'visible'});
  await page.locator('#usage-rows .u-why').first().waitFor();
  await page.evaluate(()=>{window.localeUsageRow=document.querySelector('#usage-rows tr');});
  const usageCount=usageReads;
  const pageNumber=await page.locator('#usage-page').textContent();
  await language('zh-CN'); await language('en');
  assert.equal(usageReads,usageCount,'switching loaded usage must not refetch');
  assert.equal(await page.evaluate(()=>document.querySelector('#usage-rows tr')===window.localeUsageRow),true,'keep loaded row nodes');
  assert.equal(await page.locator('#usage-page').textContent(),pageNumber);
  assert.equal(await page.locator('#usage-rows .u-hit').first().getAttribute('data-l'),'Cache hit rate');
  await page.locator('#usage-rows .u-why').first().click();
  const amounts=await page.locator('#cost-rows td.num').allTextContents();
  await page.locator('#cost-close').focus();
  await language('zh-TW');
  assert.deepEqual(await page.locator('#cost-rows td.num').allTextContents(),amounts,'cost snapshot and totals must remain identical');
  assert.equal(await page.locator('#cost-close').evaluate(el=>el===document.activeElement),true);
  assert.equal(await page.locator('#cost-rows tr').first().locator('td').first().textContent(),'輸入');
  assert.equal(usageReads,usageCount);
  await page.locator('#cost-close').click(); await language('en');
  await page.locator('#uf-date-range').click();
  const start=page.locator('[data-dr="since-day"]');
  await start.fill('2026/09/22');
  const until=await page.locator('[data-dr="until-day"]').inputValue();
  await language('zh-CN'); await language('en');
  assert.equal(await start.inputValue(),'2026/09/22');
  assert.equal(await page.locator('[data-dr="until-day"]').inputValue(),until);
  assert.equal(await page.locator('[data-dr="zone"]').textContent(),'Asia/Tokyo');
  assert.equal(await page.locator('.dr-weekdays span').first().textContent(),'Sun');
  await page.locator('[data-dr="cancel"]').click();
  for(const width of [1280,390,320]) {
   await page.setViewportSize({width,height:900});
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),`English overflow at ${width}px`);
  }
  if (await page.locator('#scrim').evaluate(el=>el.classList.contains('show'))) await page.locator('#btn-sidebar-close').click();
  await mkdir(resolve('output/playwright'),{recursive:true});
  await page.waitForFunction(()=>document.querySelector('#sidebar').getBoundingClientRect().right<=1);
  await page.screenshot({path:resolve('output/playwright/web-i18n-english-mobile.png'),animations:'disabled'});
  await page.evaluate(async()=>{const {emit}=await import('/_v/{{BUILD}}/js/state.js');emit('unauthorized');});
  await page.locator('#login').waitFor({state:'visible'});
  await language('zh-TW'); await language('en');
  assert.equal(await page.locator('#usage-rows tr').count(),0,'logout clears previous user records');
  // A second same-origin window can change the stored preference without reloading this one.
  const other=await context.newPage(); await other.goto(base+'/language-peer');
  await other.evaluate(()=>localStorage.setItem('agentbox.language','zh-TW'));
  await page.waitForFunction(()=>document.documentElement.lang==='zh-TW');
  await other.close();
  assert.deepEqual(errors,[]);
  console.log('Web locale: detection/persistence, live labels, icons, drafts/focus, timezone, raw content, terminal continuity/refusal diagnostics and English responsive layout passed');
 } finally {await context.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
}
if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)) {
 const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE?pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href:'playwright');
 const browser=await chromium.launch({headless:true});
 try {await webI18nSmoke(browser);} finally {await browser.close();}
}
