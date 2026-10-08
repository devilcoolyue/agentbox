#!/usr/bin/env node
// Browser delivery/recovery matrix against synthetic HTTP/WS receipts. No model calls.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {browserEngine,launchBrowser} from './playwright-launch.mjs';

const root=resolve(fileURLToPath(new URL('../internal/web/static/',import.meta.url)));
const server=createServer(async(req,res)=>{
 try{
  const path=new URL(req.url,'http://fixture').pathname.replace(/^\/_v\/[^/]+/,'');
  if(path==='/storage-helper'){res.setHeader('Content-Type','text/html');res.end('<!doctype html><title>Storage fixture</title>');return;}
  const file=resolve(root,'.'+(path==='/'?'/index.html':path));if(!file.startsWith(root+'/')){res.writeHead(403).end();return;}
  res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(file)]||'application/octet-stream');res.end(await readFile(file));
 }catch{res.writeHead(404).end();}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));const base=`http://127.0.0.1:${server.address().port}`;
const browser=await launchBrowser();
try{
 for(const locale of ['zh-CN','zh-TW','en']){
  const context=await browser.newContext({viewport:{width:1280,height:900}}),page=await context.newPage();page.setDefaultTimeout(15000);
  const errors=[],receipts=new Map(),puts=[],requests=[],sockets=new Set(),held=[],validations=[];let heldAccepted=()=>{};let identity='alice',mode='normal',executions=0,holdValidation=false,attachmentsValid=true,protocol=1;
  const actorScope=()=> (identity==='alice'?'a':'b').repeat(64),draftScope=()=> (identity==='alice'?'c':'d').repeat(64);
  const spaces=['a','b'].map(id=>({id:'space-'+id,name:'Space '+id.toUpperCase(),user:'alice',agent:'codex',account_id:'fixture',status:'stopped',default_model:'model-one'}));
  const threads={'space-a':'thread-one','space-b':'thread-one'};
  let threadCreationGate;
  const receiptKey=(session,id)=>identity+'/'+session+'/'+id;
  await context.addInitScript(locale=>{if(window===window.top&&/^https?:$/.test(location.protocol))localStorage.setItem('agentbox.language',locale);},locale);
  context.on('page',p=>p.on('pageerror',e=>errors.push(e.message)));page.on('pageerror',e=>errors.push(e.message));
  await context.routeWebSocket('**/api/sessions/*/chat?*',ws=>{
   const socket={ws,session:new URL(ws.url()).pathname.split('/')[3]};sockets.add(socket);ws.onClose(()=>sockets.delete(socket));
   ws.send(JSON.stringify({type:'status',state:'idle'}));ws.onMessage(raw=>{assert.notEqual(JSON.parse(String(raw)).type,'user_message','new client used legacy send');});
  });
  const emit=(session,message)=>{for(const socket of sockets)if(socket.session===session)socket.ws.send(JSON.stringify(message));};
  const update=(session,id,state,broadcast=true)=>{const c=receipts.get(receiptKey(session,id));assert.ok(c);c.state=state;c.revision++;c.updated_at=new Date().toISOString();if(broadcast){if(!['accepted','starting','running'].includes(state))emit(session,{type:'status',state:'idle'});emit(session,{type:'chat_request',version:1,receipt:c});}return c;};
  await context.route('**/api/**',async route=>{
   const req=route.request(),url=new URL(req.url()),path=url.pathname,session=path.split('/')[3],method=req.method();requests.push({method,path});let body={},status=200;
   const fail=(code,status=409)=>route.fulfill({status,json:{code,error:code,hint:'Synthetic response',action:'check_result',retryable:false}}).catch(()=>{});
   if(path==='/api/login')body={token:identity+'-token'};
   else if(path==='/api/me')body={user:identity,role:'user',draft_protocol:1,draft_scope:draftScope(),chat_protocol:protocol,chat_scope:actorScope(),timezone:'UTC',models:{codex:[{id:'model-one',label:'Model one'},{id:'model-two',label:'Model two'}]},quota:{metered:false}};
   else if(path==='/api/sessions')body=spaces;
   else if(path==='/api/accounts')body=[{id:'fixture',type:'codex',label:'Fixture',cred_status:'ok'}];
   else if(path==='/api/onboarding')body={version:1,can_configure:false,can_create:true,has_workspaces:true,accounts:[{id:'fixture',type:'codex',label:'Fixture',credentials_present:true}],default_models:{codex:'model-one'},container_resources:{cpus:1,memory_mb:512,pids_limit:128}};
   else if(path.endsWith('/models'))body={models:[{id:'model-one',label:'Model one'},{id:'model-two',label:'Model two'}],discovery:'available'};
   else if(path.endsWith('/history'))body={entries:[{kind:'user',text:'Earlier fixture task',ts:new Date().toISOString()}],thread:{id:threads[session],title:'Fixture conversation',turns:1},active_thread:threads[session],costs:{}};
   else if(path.endsWith('/chat/threads')){
    if(method==='POST'){await threadCreationGate;threads[session]='thread-two';body={id:'thread-two',created:true};}
    else body=[{id:'thread-one',title:'Fixture conversation',turns:1},{id:'thread-two',title:'Another conversation',turns:1}];
   }
   else if(path.includes('/chat/requests')){
    if(mode==='offline'){await route.abort('internetdisconnected').catch(()=>{});return;}
    const id=path.split('/')[6],all=[...receipts.entries()].filter(([key,c])=>key.startsWith(identity+'/')&&c.session_id===session).map(([,c])=>c);
    if(!id)body={version:1,scope:actorScope(),requests:[],pending:all.find(c=>['accepted','starting','running','uncertain'].includes(c.state))||null};
    else {
     let c=receipts.get(receiptKey(session,id));
     if(method==='GET'){
      if(!c){await fail('chat_request_not_found',404);return;}body={version:1,receipt:c};
     }else if(method==='PUT'){
      const input=req.postDataJSON();puts.push({id,session,input});
      if(mode==='drop-before'){await route.abort('failed').catch(()=>{});return;}
      if(c){if(['abandoned','deleted'].includes(c.state)){await fail('chat_request_gone',410);return;}if(JSON.stringify(c.request)!==JSON.stringify(input)){await fail('chat_request_conflict');return;}}
      else{
       if(all.some(c=>['accepted','starting','running','uncertain'].includes(c.state))){await fail('chat_pending');return;}
       assert.equal(input.scope,actorScope());assert.equal(input.thread_id,threads[session]);executions++;
       c={request_id:id,session_id:session,thread_id:input.thread_id,turn_id:'turn-'+executions,request:input,state:'running',revision:3,created_at:new Date().toISOString(),updated_at:new Date().toISOString()};receipts.set(receiptKey(session,id),c);status=202;
       emit(session,{type:'status',state:'running'});
      }
      if(mode==='drop-after'){await route.abort('failed').catch(()=>{});return;}
      const failReply=mode==='hold-after-fail';
      if(mode==='hold-after'||failReply)await new Promise(resolve=>{held.push(resolve);heldAccepted(id);});
      if(failReply){await route.abort('failed').catch(()=>{});return;}
      body={version:1,receipt:c,replayed:status===200};
     }else if(method==='POST'){
      const input=req.postDataJSON();assert.equal(input.scope,actorScope());
      if(input.action==='abandon'){
       if(c&&!['abandoned','deleted'].includes(c.state)){await fail('chat_request_conflict');return;}
       if(!c){c={request_id:id,session_id:session,thread_id:'',turn_id:'',request:null,state:'abandoned',revision:1,created_at:new Date().toISOString(),updated_at:new Date().toISOString()};receipts.set(receiptKey(session,id),c);}
      }else{
       if(!c){await fail('chat_request_not_found',404);return;}
       if(input.revision!==c.revision){await fail('chat_request_conflict');return;}
       if(input.action==='review'){assert.equal(c.state,'uncertain');c=update(session,id,'reviewed',false);}
       else if(input.action==='interrupt')c=update(session,id,'interrupted',false);
       else throw Error('Unexpected action');
      }
      body={version:1,receipt:c};
     }
    }
   }else if(path.endsWith('/attachments/validate')){body={valid:req.postDataJSON().paths.map(()=>attachmentsValid)};if(holdValidation)await new Promise(resolve=>validations.push(resolve));}
   else if(path.endsWith('/images'))body={path:'/shared/.file/outbox-fixture.txt',name:'outbox-fixture.txt',orig:'fixture.txt'};
   else if(path.endsWith('/files')||path==='/api/git/connections')body=[];
   await route.fulfill({status,json:body}).catch(()=>{});
  });
  const tr=(text,p=page)=>p.evaluate(async text=>(await import('/_v/{{BUILD}}/js/i18n.js')).t(text),text);
  const ready=async(p=page)=>{await p.locator('#chat-input').waitFor({state:'visible'});await p.waitForFunction(()=>!document.querySelector('#chat-input').disabled&&!document.querySelector('#chat-delivery-refresh').disabled);};
  const go=async(session,p=page)=>{await p.evaluate(id=>location.hash=`#/sessions/${id}/chat`,session);await p.waitForFunction(name=>document.querySelector('#wb-name').textContent===name,session==='space-a'?'Space A':'Space B');await ready(p);};
  const entry=(id,p=page)=>p.locator(`.delivery-item[data-request-id="${id}"]`);
  const state=async(id,value,p=page)=>{await p.locator(`.delivery-item[data-request-id="${id}"][data-state="${value}"]`).waitFor({state:'attached'});};
  const expand=async(p=page)=>{if(await p.locator('#chat-delivery-toggle').getAttribute('aria-expanded')!=='true')await p.locator('#chat-delivery-toggle').click();};
  const action=async(id,label,p=page)=>{await expand(p);const item=entry(id,p);if(!await item.evaluate(e=>e.open))await item.locator('summary').click();await item.getByRole('button',{name:await tr(label,p),exact:true}).click();};
  const confirm=async()=>{await page.locator('#dlg-ask').waitFor({state:'visible'});await page.locator('#ask-ok').click();};
  const records=()=>page.evaluate(()=>Object.keys(localStorage).filter(k=>k.startsWith('agentbox.chat-outbox.v1.')).map(k=>JSON.parse(localStorage.getItem(k))));
  const input=page.locator('#chat-input');
  try{
   await page.goto(base);await page.locator('#login-user').fill('alice');await page.locator('#login-pass').fill('fixture');await page.locator('#login-btn').click();await page.locator('#app').waitFor({state:'visible'});await go('space-a');
   // All visible tools have the same icon and hit target; no draft caption. Voice is
   // hidden without Web Speech (Playwright WebKit, Firefox); Chromium must keep it.
   const speech=await page.evaluate(()=>Boolean(window.SpeechRecognition||window.webkitSpeechRecognition));
   if(browserEngine()==='chromium')assert.ok(speech,'Chromium lost Web Speech; the voice tool would be hidden');
   const tools=await page.locator('#btn-attach, #btn-voice, #chat-draft-options summary').evaluateAll(nodes=>nodes.map(node=>{const r=node.getBoundingClientRect(),s=node.querySelector('svg').getBoundingClientRect();return {width:r.width,height:r.height,iconWidth:s.width,iconHeight:s.height,text:node.textContent.trim()};}));
   assert.equal(tools.length,3);assert.equal(tools[0].iconWidth,18);assert.equal(tools[0].text,'');
   if(speech)assert.deepEqual(tools,[tools[0],tools[0],tools[0]]);
   else{assert.deepEqual(tools[1],{width:0,height:0,iconWidth:0,iconHeight:0,text:''});assert.deepEqual(tools[2],tools[0]);}
   if(locale==='zh-CN'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:'output/playwright/composer-idle-desktop.png'});}
   // Model edits during attachment validation require another explicit send.
   await page.locator('#attach-input').setInputFiles({name:'fixture.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic attachment')});await page.waitForFunction(()=>!document.querySelector('#chat-send').disabled);
   holdValidation=true;const validation=page.waitForRequest(r=>new URL(r.url()).pathname.endsWith('/attachments/validate'));
   await input.fill('Validation snapshot [File #1]');await page.locator('#chat-send').click();await validation;
   await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/state.js')).S.pick.model='model-two';});holdValidation=false;for(const release of validations.splice(0))release();
   await page.waitForFunction(()=>!document.querySelector('#chat-send').disabled);assert.equal(puts.length,0);assert.equal(await input.inputValue(),'Validation snapshot [File #1]');
   await page.locator('#chat-attach button').click();await input.fill('');await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/state.js')).S.pick.model='model-one';});
   mode='drop-before';await input.fill('Frozen first prompt');await page.locator('#chat-send').dblclick();await page.waitForFunction(()=>document.querySelector('.delivery-item[data-state="unconfirmed"]'));
   assert.equal(puts.length,1);const first=puts[0].id;assert.equal(await input.inputValue(),'');assert.equal(executions,0);
   emit('space-a',{type:'user_message',text:'Frozen first prompt'});assert.equal((await records()).length,1,'text broadcast incorrectly acknowledged an ID');
   const frozen=(await records())[0].input;
   emit('space-a',{type:'chat_request',version:1,receipt:{request_id:first,session_id:'space-a',thread_id:'wrong-thread',turn_id:'wrong-turn',request:{...frozen,thread_id:'wrong-thread'},state:'completed',revision:99,created_at:new Date().toISOString(),updated_at:new Date().toISOString()}});
   await page.waitForTimeout(60);assert.equal((await records()).length,1,'mismatched receipt cleared the outgoing copy');
   await expand();await page.keyboard.press('Tab');await entry(first).getByRole('button',{name:await tr('重试原消息'),exact:true}).focus();
   await page.evaluate(()=>window.dispatchEvent(new Event('online')));await page.waitForTimeout(80);
   assert.equal(await entry(first).getByRole('button',{name:await tr('重试原消息'),exact:true}).evaluate(e=>e===document.activeElement),true,'read-only polling stole keyboard focus');
   await input.fill('Separate next draft');await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/state.js')).S.pick.model='model-two';});
   const peer=await context.newPage();peer.setDefaultTimeout(15000);await peer.goto(base+'/#/sessions/space-a/chat');await ready(peer);assert.equal(await peer.locator('#chat-send').isDisabled(),true);assert.equal(puts.length,1,'reload/tab performed an automatic resend');
   mode='normal';await action(first,'重试原消息');await state(first,'running');assert.equal(executions,1);assert.equal(puts[1].id,first);assert.equal(puts[1].input.model,'model-one');assert.equal(await input.inputValue(),'Separate next draft');
   const completed=structuredClone(update('space-a',first,'completed'));await state(first,'completed');
   emit('space-a',{type:'chat_request',version:1,receipt:{...completed,state:'accepted',revision:1}});await page.waitForTimeout(60);await state(first,'completed');assert.equal(executions,1);await peer.close();
   emit('space-a',{type:'status',state:'running'});await page.locator('#chat-delivery-refresh').click();await page.waitForTimeout(60);assert.equal(await page.locator('#chat-send').evaluate(e=>e.classList.contains('stop')),true,'historical receipt erased a newer legacy turn');emit('space-a',{type:'status',state:'idle'});
   mode='drop-after';await input.fill('Accepted with lost response');await page.locator('#chat-send').click();await page.waitForFunction(()=>document.querySelectorAll('.delivery-item[data-state="running"]').length===1);
   const second=puts.at(-1).id;await state(second,'running');assert.equal(executions,2);await input.fill('Draft after lost acknowledgement');
   const beforeReload=puts.length;await page.reload();await ready();await state(second,'running');assert.equal(puts.length,beforeReload);assert.equal(await input.inputValue(),'Draft after lost acknowledgement');
   assert.equal(await page.locator('#chat-delivery-toggle').getAttribute('aria-expanded'),'false');
   assert.equal(await page.locator('#chat-delivery-status').innerText(),await tr('任务正在执行'));
   assert.ok((await page.locator('#chat-delivery').boundingBox()).height<=36,'running status consumed more than one row');
   if(locale==='zh-CN'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:'output/playwright/composer-running-desktop.png'});}
   mode='normal';update('space-a',second,'uncertain',false);await page.locator('#chat-delivery-refresh').click();await state(second,'uncertain');
   assert.equal(await page.locator('#chat-send').isDisabled(),true,'unknown outcome must not remain an active stop button');
   await action(second,'已检查结果，继续');await confirm();await state(second,'reviewed');assert.equal(executions,2);
   await action(second,'复制回输入框');assert.equal(await input.inputValue(),'Draft after lost acknowledgement');await input.fill('');await action(second,'复制回输入框');assert.equal(await input.inputValue(),'Accepted with lost response');await action(second,'关闭状态');
   // Offline delivery is retained and reconnect only queries.
   mode='offline';await input.fill('Offline task');await page.locator('#chat-send').click();await page.locator('.delivery-item[data-state="unconfirmed"]').waitFor({state:'attached'});
   const offline=(await records()).find(r=>r.draft?.text==='Offline task').id;const beforeOnline=puts.length;
   mode='normal';await page.evaluate(()=>window.dispatchEvent(new Event('online')));await page.locator('#chat-delivery-refresh').click();await page.waitForTimeout(80);assert.equal(puts.length,beforeOnline);
   await action(offline,'放弃未接收消息');await confirm();await state(offline,'abandoned');await action(offline,'关闭状态');
   // Failed local persistence never submits. Explicit opt-out permits page-only recovery.
   await page.evaluate(()=>{const set=Storage.prototype.setItem;window.denyOutbox=true;Storage.prototype.setItem=function(k,v){if(window.denyOutbox&&k.startsWith('agentbox.chat-outbox.v1.'))throw Error('quota');return set.call(this,k,v);};});
   await page.locator('#attach-input').setInputFiles({name:'fixture.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic recovery attachment')});await page.waitForFunction(()=>!document.querySelector('#chat-send').disabled);
   const beforeDenied=puts.length;await input.fill('Storage denied task [File #1]');await page.locator('#chat-send').click();await page.locator('.delivery-item[data-state="unconfirmed"]').waitFor({state:'attached'});assert.equal(puts.length,beforeDenied);assert.equal(await input.inputValue(),'Storage denied task [File #1]');
   const denied=await page.locator('.delivery-item[data-state="unconfirmed"]').getAttribute('data-request-id');
   await page.locator('#chat-draft-options summary').click();await page.locator('#chat-draft-enabled').uncheck();await action(denied,'重试原消息');await state(denied,'running');assert.equal((await records()).length,0);
   const beforeOffReload=puts.length;await page.reload();await ready();await state(denied,'running');assert.equal(puts.length,beforeOffReload);
   update('space-a',denied,'uncertain');await state(denied,'uncertain');await action(denied,'已检查结果，继续');await confirm();await state(denied,'reviewed');
   attachmentsValid=false;await action(denied,'复制回输入框');await page.locator('.attachment-invalid').waitFor();assert.equal(await page.locator('#chat-send').isDisabled(),true,'server-only recovery lost attachment validation');assert.equal(puts.length,beforeOffReload);
   await page.locator('#chat-attach button').click();await input.fill('');await action(denied,'关闭状态');attachmentsValid=true;
   await page.locator('#chat-draft-options summary').click();await page.locator('#chat-draft-enabled').check();
   // Late acknowledgement belongs to the old workspace and cannot clear new editing.
   // A local unconfirmed row precedes HTTP delivery. Wait for this fixture's
   // actual acceptance/held response, otherwise puts.at(-1) may be the prior ID.
   const acceptedHeld=new Promise(resolve=>{heldAccepted=resolve;});
   mode='hold-after-fail';await input.fill('Workspace A held reply');await page.locator('#chat-send').click();
   let heldTimeout;
   const heldID=await Promise.race([acceptedHeld,new Promise((_,reject)=>{heldTimeout=setTimeout(()=>reject(Error('fixture did not accept the held request')),15000);})]).finally(()=>clearTimeout(heldTimeout));
   heldAccepted=()=>{};await go('space-b');await input.fill('Workspace B independent draft');mode='normal';for(const release of held.splice(0))release();await page.waitForTimeout(80);assert.equal(await input.inputValue(),'Workspace B independent draft');assert.equal(await page.locator('#chat-delivery').isVisible(),false,'late failure leaked into the new workspace');
   await go('space-a');await state(heldID,'running');update('space-a',heldID,'completed');await state(heldID,'completed');
   // Expired text leaves its original ID queryable/abandonable.
   mode='drop-before';await input.fill('Expired pending private prompt');await page.locator('#chat-send').click();await page.locator('.delivery-item[data-state="unconfirmed"]').waitFor({state:'attached'});const expired=puts.at(-1).id;
   await page.goto(base+'/storage-helper');await page.evaluate(()=>{for(const k of Object.keys(localStorage)){if(!k.startsWith('agentbox.chat-outbox.v1.'))continue;const r=JSON.parse(localStorage.getItem(k));r.created=Date.now()-8*24*60*60*1000;localStorage.setItem(k,JSON.stringify(r));}});
   await page.goto(base+'/#/sessions/space-a/chat');await ready();await state(expired,'unconfirmed');assert.equal((await records()).find(r=>r.id===expired).input,null);
   assert.equal(await entry(expired).getByRole('button',{name:await tr('重试原消息'),exact:true,includeHidden:true}).isDisabled(),true);
   mode='normal';await action(expired,'放弃未接收消息');await confirm();await state(expired,'abandoned');await action(expired,'关闭状态');
   // Switching threads cannot silently move or resend an unconfirmed message.
   // The local row is written before PUT; identify the actual request instead
   // of sampling puts.at(-1) while delivery may still be queued.
   mode='drop-before';await input.fill('Recover into another conversation');
   const transferRequest=page.waitForEvent('requestfailed',{predicate:r=>r.method()==='PUT'&&r.postDataJSON()?.text==='Recover into another conversation'});
   await page.locator('#chat-send').click();const transfer=new URL((await transferRequest).url()).pathname.split('/').at(-1);await state(transfer,'unconfirmed');
   const beforeThreadSwitch=puts.length;
   let releaseThread;threadCreationGate=new Promise(resolve=>{releaseThread=resolve;});
   const creating=page.waitForRequest(r=>r.method()==='POST'&&new URL(r.url()).pathname.endsWith('/chat/threads'));
   try{
    await page.locator('#btn-thread-new').click();await creating;
    // ready() can already pass on the old, empty composer. Hold the response
    // to make this race deterministic, then wait for the new history to apply.
    await ready();assert.equal(await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/state.js')).S.thread?.id),'thread-one');
   }finally{releaseThread();threadCreationGate=undefined;}
   await page.waitForFunction(async()=>{const {S}=await import('/_v/{{BUILD}}/js/state.js');return S.thread?.id==='thread-two'&&!S.histLoading;});
   await ready();assert.equal(await input.inputValue(),'');await state(transfer,'unconfirmed');
   assert.equal(await entry(transfer).getByRole('button',{name:await tr('重试原消息'),exact:true,includeHidden:true}).isDisabled(),true);
   mode='normal';await action(transfer,'放弃未接收消息');await confirm();await state(transfer,'abandoned');await action(transfer,'复制回输入框');await confirm();await page.waitForFunction(()=>document.querySelector('#chat-input').value==='Recover into another conversation');assert.equal(await input.inputValue(),'Recover into another conversation');assert.equal(puts.length,beforeThreadSwitch);await action(transfer,'关闭状态');
   // Mobile state/action layout and logout/another-user isolation.
   mode='drop-before';await input.fill('Private pending before logout');await page.locator('#chat-send').click();await page.locator('.delivery-item[data-state="unconfirmed"]').waitFor({state:'attached'});
   await page.setViewportSize({width:390,height:844});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
   if(locale==='en'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:'output/playwright/chat-outbox-mobile.png',animations:'disabled'});}
   // A frozen old tab needs CDP Page.setWebLifecycleState, which only Chromium exposes.
   const freezable=browserEngine()==='chromium';if(!freezable&&locale==='zh-CN')console.log(`Outbox: frozen-tab late sign-out not run in ${browserEngine()} (CDP-only)`);
   const suspended=freezable?await context.newPage():null;if(suspended){suspended.setDefaultTimeout(15000);await suspended.goto(base+'/#/sessions/space-a/chat');await ready(suspended);await page.bringToFront();}
   const cdp=suspended&&await context.newCDPSession(suspended);if(cdp)await cdp.send('Page.setWebLifecycleState',{state:'frozen'});
   const logoutID=(await records()).find(r=>r.state==='unconfirmed').id;await action(logoutID,'放弃未接收消息');await page.locator('#dlg-ask').waitFor({state:'visible'});
   await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/login.js')).showLogin();});assert.equal((await records()).length,0);assert.equal(await page.locator('#chat-delivery-list').innerText(),'');assert.equal(await page.locator('#dlg-ask').isVisible(),false,'logout left a delivery confirmation open');
   await page.locator('#login-user').fill('alice');await page.locator('#login-pass').fill('fixture');await page.locator('#login-btn').click();await page.locator('#app').waitFor({state:'visible'});await go('space-a');
   await input.fill('New login pending survives old page');await page.locator('#chat-send').click();await page.locator('.delivery-item[data-state="unconfirmed"]').waitFor({state:'attached'});const newLoginID=(await records()).find(r=>r.draft?.text==='New login pending survives old page').id;
   if(cdp){await cdp.send('Page.setWebLifecycleState',{state:'active'});await suspended.locator('#login').waitFor({state:'visible'});
   const currentToken=await page.evaluate(()=>localStorage.getItem('agentbox_token'));
   await suspended.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/login.js')).showLogin('synthetic late authentication failure');});
   assert.equal(await page.evaluate(()=>localStorage.getItem('agentbox_token')),currentToken,'a delayed auth failure removed the newer token');await suspended.close();}
   assert.ok((await records()).some(r=>r.id===newLoginID),'a delayed sign-out cleared new login data');await page.reload();await ready();await state(newLoginID,'unconfirmed');
   await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/login.js')).showLogin();});assert.equal((await records()).length,0);
   identity='bob';mode='normal';await page.locator('#login-user').fill('bob');await page.locator('#login-pass').fill('fixture');await page.locator('#login-btn').click();await page.locator('#app').waitFor({state:'visible'});await go('space-a');assert.equal(await input.inputValue(),'');assert.equal(await page.locator('.delivery-item').count(),0);
   // A long recovery history remains one line until opened, and scrolls within a bound.
   for(let n=0;n<12;n++){
    const id=(n+100).toString(16).padStart(32,'0');
    const c={request_id:id,session_id:'space-a',thread_id:threads['space-a'],turn_id:'layout-'+n,request:{scope:actorScope(),thread_id:threads['space-a'],text:'Review the calculator and its tests — '+n,model:'model-one',effort:'',effort_control:'',attachments:[]},state:'interrupted',revision:3,created_at:new Date(Date.now()+n).toISOString(),updated_at:new Date().toISOString()};
    receipts.set(receiptKey('space-a',id),c);emit('space-a',{type:'chat_request',version:1,receipt:c});
   }
   await page.waitForFunction(()=>document.querySelectorAll('.delivery-item').length===12);
   assert.equal(await page.locator('#chat-delivery-toggle').getAttribute('aria-expanded'),'false');
   assert.ok((await page.locator('#chat-delivery').boundingBox()).height<=36,'collapsed history grew with each turn');
   await expand();assert.equal(await page.locator('.delivery-item summary').count(),12);
   const listSize=await page.locator('#chat-delivery-list').evaluate(e=>({height:e.clientHeight,scroll:e.scrollHeight}));assert.ok(listSize.height<=240&&listSize.scroll>listSize.height);
   assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
   if(locale==='zh-CN')await page.screenshot({path:'output/playwright/composer-history-mobile.png'});
   await page.locator('.delivery-item summary').last().focus();await page.keyboard.press('Escape');assert.equal(await page.locator('#chat-delivery-toggle').getAttribute('aria-expanded'),'false');assert.equal(await page.locator('#chat-delivery-toggle').evaluate(e=>e===document.activeElement),true);
   const writesBeforeQuery=puts.length;await page.locator('#chat-delivery-refresh').click();assert.equal(await page.locator('#chat-delivery-toggle').getAttribute('aria-expanded'),'false');assert.equal(puts.length,writesBeforeQuery);
   protocol=2;const beforeUnsupported=puts.length;await page.reload();await page.locator('#chat-input').waitFor({state:'visible'});await page.waitForFunction(()=>!document.querySelector('#chat-input').disabled);await input.fill('Unsupported protocol must not downgrade');assert.equal(await page.locator('#chat-send').isDisabled(),true);
   await page.evaluate(async()=>{await (await import('/_v/{{BUILD}}/js/chat.js')).sendChat();});assert.equal(puts.length,beforeUnsupported);
   assert.deepEqual(errors,[]);console.log(`Outbox ${locale}: double-click, lost replies, query-first retry, frozen model, reload/tabs, restart review, offline, opt-out, storage refusal, expiry, late replies and logout passed`);
  }finally{for(const release of [...held.splice(0),...validations.splice(0)])release();await context.close();}
 }
}finally{await browser.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
