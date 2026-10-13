#!/usr/bin/env node
// Next-prompt suggestions in the composer against a synthetic HTTP/WS chat. No model calls.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {cancelledFetchError,launchBrowser} from './playwright-launch.mjs';

const root=resolve(fileURLToPath(new URL('../internal/web/static/',import.meta.url)));
const server=createServer(async(req,res)=>{
 try{
  const path=new URL(req.url,'http://fixture').pathname.replace(/^\/_v\/[^/]+/,'');
  const file=resolve(root,'.'+(path==='/'?'/index.html':path));if(!file.startsWith(root+'/')){res.writeHead(403).end();return;}
  res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(file)]||'application/octet-stream');res.end(await readFile(file));
 }catch{res.writeHead(404).end();}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));const base=`http://127.0.0.1:${server.address().port}`;
const scope='a'.repeat(64),thread='thread-one',now=()=>new Date().toISOString();
const event=e=>({kind:'event',event:e,ts:now()});
const history={
 'space-a':[{kind:'user',text:'先看一下项目结构',ts:now()},event({type:'assistant',message:{content:[{type:'text',text:'这是一个 Go 服务。'}]}}),
  event({type:'result',subtype:'success',duration_ms:1200}),event({type:'prompt_suggestion',suggestion:'把单元测试也补上',session_id:'p'})],
 // A turn still running: its predecessor's suggestion no longer applies.
 'space-b':[event({type:'prompt_suggestion',suggestion:'旧提示',session_id:'p'}),{kind:'user',text:'还在跑',ts:now()}],
};

async function scenario(browser,{mobile}={}){
 const context=await browser.newContext(mobile?{viewport:{width:390,height:780},hasTouch:true,isMobile:true}:{viewport:{width:1280,height:900}});
 const page=await context.newPage();page.setDefaultTimeout(15000);
 const DEFAULT=mobile?'向 Agent 下达任务…':'随心输入，向 Agent 下达任务…（Enter 发送，Shift+Enter 换行，可粘贴图片）';
 const errors=[],puts=[],sockets=new Set();let turn=0,last;
 await context.addInitScript(()=>{if(window===window.top&&/^https?:$/.test(location.protocol))localStorage.setItem('agentbox.language','zh-CN');});
 page.on('pageerror',e=>{if(!cancelledFetchError(e))errors.push(e.message);});
 await context.routeWebSocket('**/api/sessions/*/chat?*',ws=>{
  const socket={ws,session:new URL(ws.url()).pathname.split('/')[3]};sockets.add(socket);ws.onClose(()=>sockets.delete(socket));
  ws.send(JSON.stringify({type:'status',state:'idle'}));
 });
 const emit=(session,message)=>{for(const s of sockets)if(s.session===session)s.ws.send(JSON.stringify(message));};
 const finish=async session=>{
  last={...last,state:'completed',revision:last.revision+1,updated_at:now()};
  emit(session,{type:'status',state:'idle'});emit(session,{type:'chat_request',version:1,receipt:last});
  await page.waitForFunction(()=>document.querySelector('#chat-send')?.classList.contains('stop')===false);
 };
 await context.route('**/api/**',async route=>{
  const req=route.request(),path=new URL(req.url()).pathname,session=path.split('/')[3];let body={},status=200;
  if(path==='/api/login')body={token:'alice-token'};
  else if(path==='/api/me')body={user:'alice',role:'user',draft_protocol:1,draft_scope:'c'.repeat(64),chat_protocol:1,chat_scope:scope,prompt_suggestions:1,timezone:'UTC',models:{claude:[{id:'claude-opus-5',label:'Claude Opus 5'}],codex:[{id:'gpt-5.5',label:'GPT-5.5'}]},quota:{metered:false}};
  else if(path==='/api/sessions')body=[{id:'space-a',name:'Space A',user:'alice',agent:'claude',account_id:'cl',status:'running',default_model:'claude-opus-5'},{id:'space-b',name:'Space B',user:'alice',agent:'claude',account_id:'cl',status:'running',default_model:'claude-opus-5'},{id:'space-c',name:'Space C',user:'alice',agent:'codex',account_id:'cx',status:'running',default_model:'gpt-5.5'}];
  else if(path==='/api/accounts')body=[{id:'cl',type:'claude',label:'Claude',cred_status:'ok'},{id:'cx',type:'codex',label:'Codex',cred_status:'ok'}];
  else if(path==='/api/onboarding')body={version:1,can_configure:false,can_create:true,has_workspaces:true,accounts:[],default_models:{claude:'claude-opus-5',codex:'gpt-5.5'},container_resources:{cpus:1,memory_mb:512,pids_limit:128}};
  else if(path.endsWith('/models'))body={models:session==='space-c'?[{id:'gpt-5.5',label:'GPT-5.5'}]:[{id:'claude-opus-5',label:'Claude Opus 5'}],discovery:'available'};
  else if(path.endsWith('/history'))body={entries:history[session]||[],thread:{id:thread,title:'Fixture',turns:1},active_thread:thread,costs:{}};
  else if(path.endsWith('/chat/threads'))body=[{id:thread,title:'Fixture',turns:1}];
  else if(path.includes('/chat/requests')){
   const id=path.split('/')[6];
   if(!id)body={version:1,scope,requests:[],pending:last?.state==='running'?last:null};
   else if(req.method()==='GET'&&last?.request_id===id)body={version:1,receipt:last};
   else if(req.method()==='PUT'){
    const input=req.postDataJSON();puts.push({session,input});turn++;
    last={request_id:id,session_id:session,thread_id:thread,turn_id:'t'+turn,request:input,state:'running',revision:3,created_at:now(),updated_at:now()};
    body={version:1,receipt:last,replayed:false};status=202;
    setTimeout(()=>{emit(session,{type:'status',state:'running'});emit(session,{type:'user_message',text:input.text,turn:{id:'t'+turn}});},20);
   }else{await route.fulfill({status:404,json:{code:'chat_request_not_found',error:'x'}});return;}
  }
  else if(path.endsWith('/files')||path==='/api/git/connections')body=[];
  await route.fulfill({status,json:body}).catch(()=>{});
 });
 await page.goto(base);
 await page.locator('#login-user').fill('alice');await page.locator('#login-pass').fill('x');await page.locator('#login-btn').click();
 await page.locator('#app').waitFor({state:'visible'});
 const input=page.locator('#chat-input'),key=page.locator('#chat-suggest');
 const placeholder=()=>input.getAttribute('placeholder');
 const open=async session=>{await page.evaluate(id=>{location.hash='#/sessions/'+id+'/chat';},session);await input.waitFor({state:'visible'});await page.waitForTimeout(300);};
 await open('space-a');
 // Restored from the latest turn's history; never drawn as a raw event.
 await page.waitForFunction(()=>document.querySelector('#chat-input')?.getAttribute('placeholder')==='把单元测试也补上');
 assert.equal(await page.locator('#chat-log details.raw').count(),0,'suggestion rendered as raw event');
 assert.ok(await key.isVisible(),'insert key hidden');
 assert.equal(await key.locator('kbd').isVisible(),!mobile,'Tab keycap visibility');
 if(!mobile){
  await input.focus();await page.keyboard.press('Tab');
  assert.equal(await input.inputValue(),'把单元测试也补上');
  assert.ok(await input.evaluate(el=>document.activeElement===el),'Tab moved focus instead of inserting');
  assert.ok(!(await key.isVisible()),'insert key shown over typed text');
  await page.keyboard.press('Tab'); // non-empty input: Tab keeps its normal meaning
  assert.equal(await input.inputValue(),'把单元测试也补上');
  await input.fill('');
 }
 await key.waitFor({state:'visible'});
 await key.click();
 assert.equal(await input.inputValue(),'把单元测试也补上');
 assert.ok(await input.evaluate(el=>document.activeElement===el),'insert left focus elsewhere');
 // Sending asks for the next suggestion and the new turn clears the old one.
 const before=puts.length;await page.locator('#chat-send').click();
 await page.waitForFunction(()=>document.querySelector('#chat-input').value==='');
 await page.waitForTimeout(200);
 assert.equal(puts.length,before+1);assert.equal(puts.at(-1).input.suggestions,true);
 assert.equal(await placeholder(),DEFAULT);assert.ok(!(await key.isVisible()));
 // A live suggestion at the end of the turn.
 emit('space-a',{type:'agent_event',event:{type:'result',subtype:'success'},ts:now()});
 emit('space-a',{type:'agent_event',event:{type:'prompt_suggestion',suggestion:'再跑一遍构建'},ts:now()});
 await finish('space-a');
 await page.waitForFunction(()=>document.querySelector('#chat-input')?.getAttribute('placeholder')==='再跑一遍构建');
 assert.equal(await page.locator('#chat-log details.raw').count(),0);
 // Turning the setting off hides it and stops requesting; on brings it back.
 const toggle=async on=>page.locator('#chat-suggest-enabled').evaluate((el,on)=>{el.checked=on;el.dispatchEvent(new Event('change'));},on);
 await toggle(false);
 assert.equal(await placeholder(),DEFAULT);assert.ok(!(await key.isVisible()));
 if(!mobile){await input.focus();await page.keyboard.press('Tab');assert.equal(await input.inputValue(),'');}
 await input.fill('不要提示');await page.locator('#chat-send').click();
 await page.waitForFunction(()=>document.querySelector('#chat-input').value==='');await page.waitForTimeout(200);
 assert.equal(puts.at(-1).input.suggestions,undefined,'disabled setting still requested suggestions');
 emit('space-a',{type:'agent_event',event:{type:'prompt_suggestion',suggestion:'关闭时的提示'},ts:now()});
 await finish('space-a');
 await page.waitForTimeout(100);assert.equal(await placeholder(),DEFAULT);
 await toggle(true);
 assert.equal(await placeholder(),'关闭时的提示');
 assert.equal(await page.evaluate(()=>localStorage.getItem('agentbox_chat_suggestions')),null);
 // A running turn's history has no suggestion yet.
 await open('space-b');
 assert.equal(await placeholder(),DEFAULT);assert.ok(!(await key.isVisible()));
 // Codex never predicts a next prompt, so the request field is not sent.
 await open('space-c');
 assert.equal(await placeholder(),DEFAULT);
 await input.fill('codex 任务');await page.locator('#chat-send').click();
 await page.waitForFunction(()=>document.querySelector('#chat-input').value==='');await page.waitForTimeout(200);
 assert.equal(puts.at(-1).session,'space-c');assert.equal(puts.at(-1).input.suggestions,undefined);
 assert.deepEqual(errors,[]);
 await context.close();
}

const browser=await launchBrowser();
try{
 await scenario(browser);
 await scenario(browser,{mobile:true});
 console.log('chat suggestions ok');
}finally{await browser.close();server.closeAllConnections();server.close();}
