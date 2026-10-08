#!/usr/bin/env node
// Draft and attachment recovery with synthetic APIs only; no model calls.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {launchBrowser} from './playwright-launch.mjs';

const root=resolve(fileURLToPath(new URL('../internal/web/static/',import.meta.url)));
const server=createServer(async(req,res)=>{
 try{
  const rel=new URL(req.url,'http://fixture').pathname.replace(/^\/_v\/[^/]+/,'');const file=resolve(root,'.'+(rel==='/'?'/index.html':rel));
  if(rel==='/storage-helper'){res.setHeader('Content-Type','text/html');res.end('<!doctype html><title>Storage fixture</title>');return;}
  if(!file.startsWith(root+'/')){res.writeHead(403).end();return;}
  res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(file)]||'application/octet-stream');res.end(await readFile(file));
 }catch{res.writeHead(404).end();}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));
const base=`http://127.0.0.1:${server.address().port}`;
const browser=await launchBrowser();
try{
 for(const locale of ['zh-CN','zh-TW','en']){
  const context=await browser.newContext({viewport:{width:1280,height:900}});const page=await context.newPage();page.setDefaultTimeout(12000);
  const errors=[],sent=[],sockets=new Map(),allSockets=new Set();let identity='alice',holdUpload=false;const uploads=[],validPaths=new Set();let uploadCount=0;
  const threads={'space-a':'thread-one','space-b':'thread-one'};
  const spaces=['a','b'].map(id=>({id:'space-'+id,name:'Space '+id.toUpperCase(),user:'alice',agent:'codex',account_id:'fixture',account_label:'Fixture',status:'stopped',default_model:'fixture'}));
  await context.addInitScript(locale=>{if(window===window.top&&/^https?:$/.test(location.protocol))localStorage.setItem('agentbox.language',locale);},locale);
  context.on('page',p=>p.on('pageerror',e=>errors.push(e.message)));page.on('pageerror',e=>errors.push(e.message));
  await context.routeWebSocket('**/api/sessions/*/chat?*',ws=>{const id=new URL(ws.url()).pathname.split('/')[3];sockets.set(id,ws);allSockets.add(ws);ws.onClose(()=>allSockets.delete(ws));ws.send(JSON.stringify({type:'status',state:'idle'}));ws.onMessage(raw=>sent.push(JSON.parse(String(raw))));});
  await context.route('**/api/**',async route=>{
   const request=route.request(),url=new URL(request.url()),path=url.pathname,id=path.split('/')[3];let body={};
   if(path==='/api/login')body={token:identity+'-token'};
   else if(path==='/api/me')body={user:identity,role:'user',draft_scope:(identity==='alice'?'a':'b').repeat(64),draft_protocol:1,timezone:'UTC',models:{codex:[{id:'fixture',label:'Fixture'}]},quota:{metered:false}};
   else if(path==='/api/sessions')body=spaces;
   else if(path==='/api/accounts')body=[{id:'fixture',type:'codex',label:'Fixture',cred_status:'ok'}];
   else if(path==='/api/onboarding')body={version:1,can_configure:false,can_create:true,has_workspaces:true,accounts:[{id:'fixture',type:'codex',label:'Fixture',credentials_present:true}],default_models:{codex:'fixture'},container_resources:{cpus:1,memory_mb:512,pids_limit:128}};
   else if(path.endsWith('/models'))body={models:[{id:'fixture',label:'Fixture'}],discovery:'available'};
   else if(path.endsWith('/history'))body={entries:threads[id]==='thread-one'?[{kind:'user',text:'Earlier fixture task',ts:new Date().toISOString()}]:[{kind:'draft_context',ts:new Date().toISOString()}],thread:{id:threads[id],title:threads[id]==='thread-one'?'thread-one':'',turns:threads[id]==='thread-one'?1:0},active_thread:threads[id],costs:{}};
   else if(path.endsWith('/chat/threads')&&request.method()==='POST'){threads[id]='thread-two';body={id:'thread-two',created:true};}
   else if(path.endsWith('/chat/threads'))body={threads:['thread-one','thread-two'].map(id=>({id,title:id,ts:new Date().toISOString(),updated:new Date().toISOString(),turns:0,resumable:true})),active:threads[id]};
   else if(path.endsWith('/activate')){threads[id]=path.split('/')[6];body={resumable:true};}
   else if(path.endsWith('/attachments/validate'))body={valid:request.postDataJSON().paths.map(path=>validPaths.has(path))};
   else if(path.endsWith('/images')){
    const n=++uploadCount;body={path:`/shared/.file/file-${n}.txt`,name:`file-${n}.txt`,orig:'attachment.txt'};validPaths.add(body.path);
    if(holdUpload)await new Promise(r=>uploads.push(r));
   }else if(path.endsWith('/files')||path==='/api/git/connections')body=[];
   await route.fulfill({json:body}).catch(()=>{});
  });
  const ready=async p=>{await p.locator('#chat-input').waitFor({state:'visible'});await p.waitForFunction(()=>!document.querySelector('#chat-input').disabled);};
  const go=async(id,p=page)=>{await p.evaluate(id=>location.hash=`#/sessions/${id}/chat`,id);await p.waitForFunction(name=>document.querySelector('#wb-name').textContent===name,id==='space-a'?'Space A':'Space B');await ready(p);};
  const input=page.locator('#chat-input');
  try{
   await page.goto(base);await page.locator('#login-user').fill('alice');await page.locator('#login-pass').fill('fixture');await page.locator('#login-btn').click();await page.locator('#app').waitFor({state:'visible'});await go('space-a');
   await input.fill('A private draft');await go('space-b');assert.equal(await input.inputValue(),'');await input.fill('B private draft');await go('space-a');assert.equal(await input.inputValue(),'A private draft');
   await page.reload();await ready(page);assert.equal(await input.inputValue(),'A private draft');
   const peer=await context.newPage();peer.setDefaultTimeout(12000);await peer.goto(base+'/#/sessions/space-a/chat');await ready(peer);
   assert.equal(await peer.locator('#chat-input').inputValue(),'A private draft');
   await peer.locator('#chat-input').fill('Peer draft');await input.fill('A revised draft');
   await peer.reload();await ready(peer);assert.equal(await peer.locator('#chat-input').inputValue(),'Peer draft');
   await page.reload();await ready(page);assert.equal(await input.inputValue(),'A revised draft');await peer.close();
   await page.locator('#btn-thread-new').click();await ready(page);await page.waitForFunction(()=>document.querySelector('#chat-input').value==='');
   // A metadata-only draft thread still adopts its first user-message title.
   sockets.get('space-a').send(JSON.stringify({type:'user_message',text:'First message in draft thread'}));
   await page.locator('#thread-title').filter({hasText:'First message in draft thread'}).waitFor();
   await input.fill('Second thread draft');await page.locator('#btn-threads').click();await page.locator('#tp-list button').filter({hasText:'thread-one'}).click();await page.waitForFunction(()=>document.querySelector('#chat-input').value==='A revised draft');
   // Interrupted uploads are never written into a different workspace.
   holdUpload=true;const upload=page.waitForRequest(r=>new URL(r.url()).pathname.endsWith('/images'));
   await page.locator('#attach-input').setInputFiles({name:'attachment.txt',mimeType:'text/plain',buffer:Buffer.from('fixture')});await upload;
   await go('space-b');holdUpload=false;for(const release of uploads.splice(0))release();
   assert.equal(await input.inputValue(),'B private draft');assert.equal(await page.locator('#chat-attach').isVisible(),false);
   await go('space-a');assert.ok((await input.inputValue()).includes('[File #1]'));assert.equal(await page.locator('#chat-send').isDisabled(),true);
   await page.locator('.attachment-invalid button').click();assert.ok(!(await input.inputValue()).includes('[File #1]'));
   await page.locator('#attach-input').setInputFiles({name:'attachment.txt',mimeType:'text/plain',buffer:Buffer.from('fixture')});
   await page.waitForFunction(()=>!document.querySelector('#chat-send').disabled);
   await page.reload();await ready(page);assert.equal(await page.locator('.attachment-invalid').count(),0);
   validPaths.clear();await page.reload();await ready(page);assert.equal(await page.locator('#chat-send').isDisabled(),true);
   assert.equal(await page.locator('.attachment-invalid').count(),1);
   await page.locator('.attachment-invalid button').click();
   // Denied storage keeps the input and does not report a durable save.
   await page.evaluate(()=>{const set=Storage.prototype.setItem;window.denyDraft=true;Storage.prototype.setItem=function(k,v){if(window.denyDraft&&k.startsWith('agentbox.chat-draft.v1.'))throw new Error('quota');return set.call(this,k,v);};});
   await input.fill('Unsaved draft');assert.equal(await input.inputValue(),'Unsaved draft');
   const failure=await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/i18n.js')).t('草稿暂未保存到本机，请复制文本或检查浏览器存储空间。'));
   assert.equal(await page.locator('#chat-draft-status').innerText(),failure);
   await page.evaluate(()=>window.denyDraft=false);await input.fill('Saved after retry');await page.reload();await ready(page);assert.equal(await input.inputValue(),'Saved after retry');
   // Turning saving off clears all persisted copies but keeps current editing.
   await page.locator('#chat-draft-options summary').click();await page.locator('#chat-draft-enabled').uncheck();assert.equal(await input.inputValue(),'Saved after retry');
   await page.reload();await ready(page);assert.equal(await input.inputValue(),'');
   await page.locator('#chat-draft-options summary').click();await page.locator('#chat-draft-enabled').check();await input.fill('Expired draft');
   await page.goto(base+'/storage-helper');
   await page.evaluate(()=>{for(const key of Object.keys(localStorage)){if(!key.startsWith('agentbox.chat-draft.v1.')||key.endsWith('.enabled'))continue;const value=JSON.parse(localStorage.getItem(key));value.updated=Date.now()-8*24*60*60*1000;localStorage.setItem(key,JSON.stringify(value));}});
   await page.goto(base+'/#/sessions/space-a/chat');await ready(page);assert.equal(await input.inputValue(),'');
   await input.fill('Logout private draft');await page.setViewportSize({width:390,height:844});
   if(locale==='en'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:'output/playwright/chat-drafts-mobile.png',animations:'disabled'});}
   const watcher=await context.newPage();watcher.setDefaultTimeout(12000);await watcher.goto(base+'/#/sessions/space-a/chat');await ready(watcher);
   await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/login.js')).showLogin();});await watcher.locator('#login').waitFor({state:'visible'});
   assert.equal(await watcher.locator('#chat-input').inputValue(),'');
   assert.equal(await page.evaluate(()=>Object.entries(localStorage).some(([k,v])=>k.startsWith('agentbox.chat-draft.v1.')&&!k.endsWith('.enabled')&&v.includes('private draft'))),false);
   identity='bob';await page.locator('#login-user').fill('bob');await page.locator('#login-pass').fill('fixture');await page.locator('#login-btn').click();await page.locator('#app').waitFor({state:'visible'});await go('space-a');assert.equal(await input.inputValue(),'');
   assert.equal(sent.length,0,'draft restoration invoked a model');assert.deepEqual(errors,[]);
   console.log(`Drafts ${locale}: scoped reload, multi-tab, threads, upload cancellation, reference validation, denied storage, opt-out, expiry and logout passed`);
  }catch(error){console.error(`Drafts ${locale} failed:`,error);throw error;
  }finally{
   for(const release of uploads.splice(0))release();
   for(const p of context.pages())await p.close();
   for(const ws of allSockets)ws.close();
   await context.close();
  }
 }
}finally{await browser.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
