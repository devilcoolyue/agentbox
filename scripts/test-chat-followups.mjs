#!/usr/bin/env node
// Follow-up messages while a turn runs (queue / steer) against a synthetic HTTP/WS chat. No model calls.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {cancelledFetchError,launchBrowser} from './playwright-launch.mjs';

const root=resolve(fileURLToPath(new URL('../internal/web/static/',import.meta.url)));
const shots=process.env.AGENTBOX_FOLLOWUP_SHOTS||'';
const server=createServer(async(req,res)=>{
 try{
  const path=new URL(req.url,'http://fixture').pathname.replace(/^\/_v\/[^/]+/,'');
  const file=resolve(root,'.'+(path==='/'?'/index.html':path));if(!file.startsWith(root+'/')){res.writeHead(403).end();return;}
  res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(file)]||'application/octet-stream');res.end(await readFile(file));
 }catch{res.writeHead(404).end();}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));const base=`http://127.0.0.1:${server.address().port}`;
const scope='a'.repeat(64),thread='thread-one',now=()=>new Date().toISOString();

async function scenario(browser,{mobile}={}){
 const context=await browser.newContext(mobile?{viewport:{width:390,height:780},hasTouch:true,isMobile:true}:{viewport:{width:1280,height:900}});
 const page=await context.newPage();page.setDefaultTimeout(15000);
 const errors=[],puts=[],actions=[],sockets=new Set(),receipts=new Map();let seq=0,runningID='';
 const running={},paused={};
 await context.addInitScript(()=>{if(window===window.top&&/^https?:$/.test(location.protocol))localStorage.setItem('agentbox.language','zh-CN');});
 page.on('pageerror',e=>{if(!cancelledFetchError(e))errors.push(e.message);});
 await context.routeWebSocket('**/api/sessions/*/chat?*',ws=>{
  const session=new URL(ws.url()).pathname.split('/')[3];
  const socket={ws,session};sockets.add(socket);ws.onClose(()=>sockets.delete(socket));
  ws.send(JSON.stringify({type:'status',state:running[session]?'running':'idle'}));
 });
 const emit=(session,message)=>{for(const s of sockets)if(s.session===session)s.ws.send(JSON.stringify(message));};
 const update=(id,patch)=>{const c={...receipts.get(id),...patch,revision:receipts.get(id).revision+1,updated_at:now()};receipts.set(id,c);emit(c.session_id,{type:'chat_request',version:1,receipt:c});return c;};
 const finish=async session=>{
  running[session]=false;update(runningID,{state:'completed'});
  emit(session,{type:'status',state:'idle'});
  await page.waitForFunction(()=>document.querySelector('#chat-send')?.classList.contains('stop')===false);
 };
 await context.route('**/api/**',async route=>{
  const req=route.request(),path=new URL(req.url()).pathname,session=path.split('/')[3];let body={},status=200;
  if(path==='/api/login')body={token:'alice-token'};
  else if(path==='/api/me')body={user:'alice',role:'user',draft_protocol:1,draft_scope:'c'.repeat(64),chat_protocol:1,chat_scope:scope,chat_questions:1,chat_followups:1,timezone:'UTC',models:{claude:[{id:'claude-opus-5',label:'Claude Opus 5'}],codex:[{id:'gpt-5.5',label:'GPT-5.5'}]},quota:{metered:false}};
  else if(path==='/api/sessions')body=[{id:'space-a',name:'Space A',user:'alice',agent:'claude',account_id:'cl',status:'running',default_model:'claude-opus-5'},{id:'space-c',name:'Space C',user:'alice',agent:'codex',account_id:'cx',status:'running',default_model:'gpt-5.5'}];
  else if(path==='/api/accounts')body=[{id:'cl',type:'claude',label:'Claude',cred_status:'ok'},{id:'cx',type:'codex',label:'Codex',cred_status:'ok'}];
  else if(path==='/api/onboarding')body={version:1,can_configure:false,can_create:true,has_workspaces:true,accounts:[],default_models:{claude:'claude-opus-5',codex:'gpt-5.5'},container_resources:{cpus:1,memory_mb:512,pids_limit:128}};
  else if(path.endsWith('/models'))body={models:session==='space-c'?[{id:'gpt-5.5',label:'GPT-5.5'}]:[{id:'claude-opus-5',label:'Claude Opus 5'}],discovery:'available'};
  else if(path.endsWith('/history'))body={entries:[],thread:{id:thread,title:'Fixture',turns:0},active_thread:thread,costs:{}};
  else if(path.endsWith('/chat/threads'))body=[{id:thread,title:'Fixture',turns:0}];
  else if(path.includes('/chat/requests')){
   const id=path.split('/')[6];
   const mine=[...receipts.values()].filter(c=>c.session_id===session);
   if(!id)body={version:1,scope,requests:mine.filter(c=>c.state==='queued'),pending:mine.find(c=>['accepted','starting','running','uncertain'].includes(c.state))||null};
   else if(req.method()==='GET'&&receipts.has(id))body={version:1,receipt:receipts.get(id)};
   else if(req.method()==='PUT'){
    const input=req.postDataJSON();puts.push({session,input});seq++;
    let c={request_id:id,session_id:session,thread_id:thread,turn_id:'t'+seq,request:input,revision:3,created_at:now(),updated_at:now()};
    if(running[session]&&input.delivery==='steer'&&session==='space-a')c={...c,state:'completed',turn_id:'',merged_into:runningID};
    else if((running[session]||paused[session])&&input.delivery)c={...c,state:'queued',revision:1};
    else{c={...c,state:'running'};running[session]=true;runningID=id;setTimeout(()=>{emit(session,{type:'status',state:'running'});emit(session,{type:'user_message',text:input.text,turn:{id:c.turn_id}});},20);}
    receipts.set(id,c);body={version:1,receipt:c,replayed:false};status=202;
   }else if(req.method()==='POST'&&receipts.has(id)){
    const action=req.postDataJSON();actions.push({id,...action});
    if(action.action==='withdraw')body={version:1,receipt:update(id,{state:'abandoned',error_code:'withdrawn'})};
    else body={version:1,receipt:receipts.get(id)};
   }else{await route.fulfill({status:404,json:{code:'chat_request_not_found',error:'x'}});return;}
  }
  else if(path.endsWith('/file')){await route.fulfill({status:200,contentType:'image/svg+xml',body:'<svg xmlns="http://www.w3.org/2000/svg" width="1600" height="500"><rect width="1600" height="500" fill="#c96"/></svg>'}).catch(()=>{});return;}
  else if(path.endsWith('/files')||path==='/api/git/connections')body=[];
  await route.fulfill({status,json:body}).catch(()=>{});
 });
 await page.goto(base);
 await page.locator('#login-user').fill('alice');await page.locator('#login-pass').fill('x');await page.locator('#login-btn').click();
 await page.locator('#app').waitFor({state:'visible'});
 const input=page.locator('#chat-input'),follow=page.locator('#chat-follow');
 const open=async session=>{await page.evaluate(id=>{location.hash='#/sessions/'+id+'/chat';},session);await input.waitFor({state:'visible'});await page.waitForTimeout(300);};
 const cleared=()=>page.waitForFunction(()=>document.querySelector('#chat-input').value==='');
 const toast=text=>page.locator('.toast',{hasText:text}).first().waitFor();
 const enter=async(mod)=>{if(mobile)await follow.click();else await input.press(mod?'Control+Enter':'Enter');};
 const noOverflow=async()=>assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1),'page scrolls sideways');

 await open('space-a');
 // Idle: an ordinary send, no follow-up control.
 await input.fill('先跑测试');
 assert.ok(!(await follow.isVisible()),'follow-up control shown while idle');
 await page.locator('#chat-send').click();await cleared();
 assert.equal(puts.at(-1).input.delivery,undefined);
 await page.waitForFunction(()=>document.querySelector('#chat-send')?.classList.contains('stop'));
 // Running: typing offers the preferred way next to the stop button.
 if(!mobile)assert.match(await input.getAttribute('placeholder'),/Enter 排队，Ctrl\+Enter 引导/);
 await input.fill('顺便把 lint 也跑了');
 await follow.waitFor();
 assert.equal(await follow.textContent(),'排队');
 assert.ok(await page.locator('#chat-send').evaluate(el=>el.classList.contains('stop')),'stop button replaced');
 if(shots){await mkdir(shots,{recursive:true});await page.locator('.composer, #chat-input').first().screenshot({path:`${shots}/${mobile?'mobile':'desktop'}-composer.png`}).catch(()=>{});}
 await noOverflow();
 await enter(false);await cleared();
 assert.equal(puts.at(-1).input.delivery,'queue');assert.equal(puts.at(-1).input.text,'顺便把 lint 也跑了');
 await toast('已排队');
 await page.waitForFunction(()=>/后面还有 1 条排队/.test(document.querySelector('#chat-delivery-reason')?.textContent||''));
 const queuedID=puts.length&&[...receipts.values()].find(c=>c.state==='queued').request_id;
 // Withdraw from the status panel: back into the empty composer, not run.
 await page.locator('#chat-delivery-toggle').click();
 const row=page.locator(`.delivery-item[data-request-id="${queuedID}"]`);
 await row.waitFor();assert.equal(await row.getAttribute('data-state'),'queued');
 assert.match(await row.textContent(),/排队中/);
 await row.locator('[data-delivery-action="撤回到输入框，不再自动执行"]').click();
 await page.waitForFunction(()=>document.querySelector('#chat-input').value==='顺便把 lint 也跑了');
 assert.equal(actions.at(-1).action,'withdraw');
 await page.waitForFunction(id=>!document.querySelector(`.delivery-item[data-request-id="${id}"]`),queuedID);
 // The other way for one message: steer into the running turn.
 if(!mobile){
  await enter(true);await cleared();
  assert.equal(puts.at(-1).input.delivery,'steer');
  await toast('已插入当前任务');
 }else{
  // Phones use the preferred way; switch it to steer in settings.
  await page.evaluate(()=>{const s=document.querySelector('#chat-followup-mode');s.value='steer';s.dispatchEvent(new Event('change'));});
  await follow.waitFor();assert.equal(await follow.textContent(),'引导');
  await follow.click();await cleared();
  assert.equal(puts.at(-1).input.delivery,'steer');
  await toast('已插入当前任务');
  await page.evaluate(()=>{const s=document.querySelector('#chat-followup-mode');s.value='queue';s.dispatchEvent(new Event('change'));});
 }
 const steered=puts.at(-1).input.text;
 // Where the CLI took it: a dashed bubble inside the running turn, not a new turn.
 emit('space-a',{type:'agent_event',event:{type:'assistant',message:{content:[{type:'text',text:'正在跑测试…'}]}},ts:now()});
 emit('space-a',{type:'steer',kind:'steer',event:{id:'a'.repeat(32),text:steered,state:'applied'},ts:now()});
 const bubble=page.locator('.steer-msg').first();await bubble.waitFor();
 assert.match(await bubble.textContent(),new RegExp('引导'+steered));
 assert.ok(await bubble.evaluate(el=>!!el.closest('.turn-body')),'steer started a new turn group');
 assert.equal(await page.locator('#chat-log .turn:not(.working)').count(),1);
 // One the turn never took can be copied back.
 emit('space-a',{type:'steer',kind:'steer',event:{id:'b'.repeat(32),text:'来不及的补充',state:'dropped'},ts:now()});
 const dropped=page.locator('.steer-msg[data-state="dropped"]');await dropped.waitFor();
 assert.match(await dropped.textContent(),/引导未生效/);
 await dropped.locator('.steer-copy').click();
 await page.waitForFunction(()=>document.querySelector('#chat-input').value==='来不及的补充');
 // An image in a steer message is a thumbnail like in the user's own bubble, and the bubble stays in the column.
 emit('space-a',{type:'steer',kind:'steer',event:{id:'c'.repeat(32),text:'看这张图 [图片#1 /shared/.images/wide.png] 再说一段很长的补充说明，确认气泡不会超出对话栏左边',state:'applied'},ts:now()});
 const pic=page.locator('.steer-msg .chat-thumb');await pic.waitFor();
 await page.waitForFunction(()=>{const i=document.querySelector('.steer-msg .chat-thumb');return i&&i.complete&&i.naturalWidth>0;});
 const box=await pic.boundingBox();
 assert.ok(box.width<=201&&box.height<=111,`steer thumbnail is capped: ${box.width}x${box.height}`);
 const [log,steerBox]=await Promise.all([page.locator('#chat-log').boundingBox(),pic.locator('xpath=ancestor::*[contains(@class,"steer-text")]').boundingBox()]);
 assert.ok(steerBox.x>=log.x&&steerBox.x+steerBox.width<=log.x+log.width+1,'steer bubble stays inside the chat column');
 if(shots)await page.locator('#chat-log').screenshot({path:`${shots}/${mobile?'mobile':'desktop'}-steer.png`});
 // A queued follow-up that starts later updates in place.
 await input.fill('跑完再部署');
 await enter(false);await cleared();
 const later=[...receipts.values()].find(c=>c.state==='queued');
 assert.ok(later,'second follow-up not queued');
 await finish('space-a');
 runningID=later.request_id;running['space-a']=true;update(later.request_id,{state:'running'});
 emit('space-a',{type:'status',state:'running'});
 await page.waitForFunction(()=>document.querySelector('#chat-send')?.classList.contains('stop'));
 await page.waitForFunction(()=>!/排队/.test(document.querySelector('#chat-delivery-reason')?.textContent||''));
 // A background shell: after the answer the turn waits for it, and says so.
 const ev=e=>emit('space-a',{type:'agent_event',kind:'event',event:e,ts:now()});
 const workText=sel=>page.evaluate(sel=>document.querySelector('#chat-log .turn.working '+sel)?.textContent||'',sel);
 ev({type:'assistant',message:{content:[{type:'tool_use',id:'toolu_bg',name:'Bash',input:{command:'sleep 40',run_in_background:true}}]}});
 ev({type:'system',subtype:'task_started',task_id:'bg1',description:'Sleep for 40 seconds',is_backgrounded:true,task_type:'local_bash'});
 ev({type:'system',subtype:'background_tasks_changed',tasks:[{task_id:'bg1',description:'Sleep for 40 seconds',task_type:'local_bash'}]});
 await page.waitForFunction(()=>/后台任务 1 个运行中/.test(document.querySelector('#chat-log .turn.working .work-bg')?.textContent||''));
 assert.equal(await page.locator('.cc-tool[data-tool="Bash"] .cc-res').first().textContent(),'⎿在后台运行');
 ev({type:'assistant',message:{content:[{type:'text',text:'已在后台启动'}]}});
 ev({type:'result',subtype:'success',is_error:false,result:'已在后台启动'});
 await page.waitForFunction(()=>/等待后台任务结束：Sleep for 40 seconds/.test(document.querySelector('#chat-log .turn.working .work-label')?.textContent||''));
 assert.ok(await page.locator('#chat-log .turn.working .work-bg').isHidden(),'count kept beside the waiting label');
 if(shots)await page.locator('#chat-log').screenshot({path:`${shots}/${mobile?'mobile':'desktop'}-background.png`});
 ev({type:'system',subtype:'task_notification',task_id:'bg1',status:'completed',summary:'Background command "Sleep for 40 seconds" completed (exit code 0)'});
 ev({type:'system',subtype:'background_tasks_changed',tasks:[]});
 await page.locator('.chip.bg-task',{hasText:'后台任务完成：Sleep for 40 seconds'}).waitFor();
 assert.match(await workText('.work-label'),/思考中/);
 // Stopping: the CLI's failed result is not shown as an error, one note says it was stopped.
 emit('space-a',{type:'agent_event',kind:'event',state:'interrupted',event:{type:'result',subtype:'error_during_execution',is_error:true},ts:now()});
 running['space-a']=false;update(runningID,{state:'interrupted',error_code:'operation_cancelled'});
 emit('space-a',{type:'status',kind:'status',state:'interrupted',ts:now()});
 await page.locator('#chat-log .chip.stopped',{hasText:'已中断'}).waitFor();
 await page.waitForFunction(()=>document.querySelector('#chat-send')?.classList.contains('stop')===false);
 assert.equal(await page.locator('#chat-log .chip.err').count(),0,'stop shown as an error');
 assert.equal(await page.locator('#chat-log .turn.working').count(),0);
 if(shots)await page.locator('#chat-log').screenshot({path:`${shots}/${mobile?'mobile':'desktop'}-stopped.png`});
 // History replays it the same way.
 await page.evaluate(()=>{window.__render=import(document.querySelector('script[type=module]').src.replace(/main\.js$/,'chat-render.js'));});
 const replayed=await page.evaluate(async()=>{const m=await window.__render;const html=e=>m.renderEntry(e).map(n=>n.className+':'+n.textContent);
  return [html({kind:'event',state:'interrupted',event:{type:'result',subtype:'error_during_execution',is_error:true}}),html({kind:'status',state:'interrupted'}),html({kind:'event',event:{type:'result',subtype:'error_during_execution',is_error:true}})];});
 assert.deepEqual(replayed[0],[]);
 assert.match(replayed[1].join(),/chip stopped:已中断/);
 assert.match(replayed[2].join(),/chip err has-icon:error_during_execution/,'real failures still shown');
 // A five-hour pause: the turn it stopped reads as stopped, the composer says
 // until when, and a message sent meanwhile queues for the reset.
 emit('space-a',{type:'status',kind:'status',state:'error',code:'chat_quota_paused',error:'synthetic',hint:'synthetic',ts:now()});
 await page.locator('#chat-log .chip.stopped',{hasText:'5 小时额度达到暂停阈值，这一轮已暂停'}).waitFor();
 assert.equal(await page.locator('#chat-log .chip.err').count(),0,'pause shown as an error');
 paused['space-a']=true;
 emit('space-a',{type:'quota_pause',pause:{percent:96,threshold:95,resets_at:Date.now()+2*3600000}});
 await page.waitForFunction(()=>/^5 小时额度已用 96%，暂停到 \d\d:\d\d；现在发送会排队，重置后执行$/.test(document.querySelector('#chat-input').placeholder));
 // The empty composer is as tall as its placeholder: on a phone the pause note wraps and
 // used to lose its second line under a one-line box.
 const inputLines=()=>input.evaluate(el=>{const cs=getComputedStyle(el);return Math.round((el.clientHeight-parseFloat(cs.paddingTop)-parseFloat(cs.paddingBottom))/parseFloat(cs.lineHeight));});
 const placeholderLines=()=>input.evaluate(el=>{const p=el.cloneNode();p.removeAttribute('id');p.value=el.placeholder;p.style.cssText=`position:absolute;visibility:hidden;height:0;min-height:0;width:${el.offsetWidth}px`;el.after(p);const cs=getComputedStyle(el),h=p.scrollHeight;p.remove();return Math.round((h-parseFloat(cs.paddingTop)-parseFloat(cs.paddingBottom))/parseFloat(cs.lineHeight));});
 if(mobile)assert.ok(await placeholderLines()>1,'the pause note wraps on a phone');
 assert.equal(await inputLines(),await placeholderLines(),'composer shows every line of the pause note');
 await input.fill('重置后接着做');await page.locator('#chat-send').click();await cleared();
 assert.equal(puts.at(-1).input.delivery,'queue','sent during the pause without queueing');
 await toast('已排队，5 小时额度重置后自动执行');
 await page.waitForFunction(()=>document.querySelector('#chat-delivery-reason')?.textContent==='5 小时额度重置后自动执行');
 paused['space-a']=false;
 emit('space-a',{type:'quota_pause',pause:null});
 await page.waitForFunction(()=>document.querySelector('#chat-delivery-reason')?.textContent==='这一轮结束后自动执行');
 assert.ok(!/5 小时额度/.test(await input.getAttribute('placeholder')),'placeholder kept the pause');
 await page.waitForFunction(()=>{const el=document.querySelector('#chat-input'),cs=getComputedStyle(el);return el.clientHeight-parseFloat(cs.paddingTop)-parseFloat(cs.paddingBottom)<parseFloat(cs.lineHeight)*1.5;});
 // Codex cannot be steered: always queue, even with the modifier.
 await open('space-c');
 await input.fill('codex 任务');await page.locator('#chat-send').click();await cleared();
 await page.waitForFunction(()=>document.querySelector('#chat-send')?.classList.contains('stop'));
 await input.fill('codex 跟进');
 await follow.waitFor();assert.equal(await follow.textContent(),'排队');
 await enter(true);await cleared();
 assert.equal(puts.at(-1).session,'space-c');assert.equal(puts.at(-1).input.delivery,'queue');
 assert.deepEqual(errors,[]);
 await context.close();
}

const browser=await launchBrowser();
try{
 await scenario(browser);
 await scenario(browser,{mobile:true});
 console.log('chat follow-ups ok');
}finally{await browser.close();server.closeAllConnections();server.close();}
