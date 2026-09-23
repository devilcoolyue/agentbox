#!/usr/bin/env node
// Synthetic API/browser regression. No Docker, real accounts or provider calls.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

export async function smoke(page) {
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
 const errors = []; page.on('pageerror', e => errors.push(e.message));
 let settingsWrites = 0, monitorCalls = 0;
 let settings = { listen:'127.0.0.1:8180', agent_image:'fixture', permission_mode:'bypassPermissions', max_upload_mb:10,
  idle_timeout_min:30, timezone:'UTC', container:{memory_mb:512,cpus:1,pids_limit:128,network:'none'},
  resources:{max_running:2,max_running_per_user:1,min_free_bytes:0},models:{claude:[],codex:[]}, default_models:{claude:'fixture',codex:'fixture'},
  terminal_tips:{tips:['fixture'],interval_sec:4,animation:'scroll'},tunnel:{enabled:false},proxy_bridge:{bind:'127.0.0.1:1081'},pricing:{} };
 const me = {user:'fixture',role:'admin',timezone:'UTC',models:{claude:[],codex:[]},quota:{metered:false}};
 await page.route('**/api/**', async route => {
  const u = new URL(route.request().url()), path=u.pathname;
  let body={};
  if (path === '/api/login') body={token:'synthetic-browser-token'};
  else if(path === '/api/me') body=me;
  else if(['/api/sessions','/api/accounts','/api/proxies','/api/users'].includes(path)) body=[];
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
  await page.goto(base);
  await page.locator('#login-user').fill('fixture');await page.locator('#login-pass').fill('fixture-password');await page.locator('#login-btn').click();
  await page.locator('#app').waitFor({state:'visible'});
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
  assert.deepEqual(errors,[]);
  console.log('Browser: login, capacity save, repeated init, usage sync, monitor cleanup, socket generation, mobile re-login passed');
 } finally { await page.unroute('**/api/**'); await new Promise(r=>server.close(r)); }
}
if (process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)) {
 const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
 const browser=await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL ? {channel:process.env.AGENTBOX_BROWSER_CHANNEL}: {})});
 try { await smoke(await browser.newPage()); } finally { await browser.close(); }
}
