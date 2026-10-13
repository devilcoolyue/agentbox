#!/usr/bin/env node
// Agent question cards (AskUserQuestion) against a synthetic HTTP/WS chat. No model calls.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {cancelledFetchError,launchBrowser} from './playwright-launch.mjs';

const root=resolve(fileURLToPath(new URL('../internal/web/static/',import.meta.url)));
const shots=process.env.AGENTBOX_QUESTION_SHOTS||'';
const server=createServer(async(req,res)=>{
 try{
  const path=new URL(req.url,'http://fixture').pathname.replace(/^\/_v\/[^/]+/,'');
  const file=resolve(root,'.'+(path==='/'?'/index.html':path));if(!file.startsWith(root+'/')){res.writeHead(403).end();return;}
  res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(file)]||'application/octet-stream');res.end(await readFile(file));
 }catch{res.writeHead(404).end();}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));const base=`http://127.0.0.1:${server.address().port}`;
const scope='a'.repeat(64),thread='thread-one',now=()=>new Date().toISOString();
const later=()=>new Date(Date.now()+9.5*60000).toISOString();
const q1={question:'部署到哪个环境？',header:'环境',options:[{label:'staging',description:'预发布环境'},{label:'production',description:'生产环境'}]};
const q2={question:'跑哪些检查？',header:'检查',multi_select:true,options:[{label:'lint'},{label:'test'},{label:'e2e'}]};
const askEvent=id=>({kind:'event',ts:now(),event:{type:'assistant',message:{content:[{type:'tool_use',id,name:'AskUserQuestion',input:{questions:[]}}]}}});
const history={
 // A finished turn: the question shows as a summary, the raw tool call is not drawn.
 'space-a':[{kind:'user',text:'先部署',ts:now()},askEvent('toolu_old'),
  {kind:'question',ts:now(),event:{id:'toolu_old',state:'pending',questions:[q1],expires_at:later()}},
  {kind:'question',ts:now(),event:{id:'toolu_old',state:'answered',answers:{[q1.question]:'staging'}}},
  {kind:'event',ts:now(),event:{type:'assistant',message:{content:[{type:'text',text:'已部署到 staging。'}]}}},
  {kind:'event',ts:now(),event:{type:'result',subtype:'success',duration_ms:900}}],
 // Reloaded while the latest turn still waits for an answer.
 'space-b':[{kind:'user',text:'还在问',ts:now()},
  {kind:'question',ts:now(),event:{id:'toolu_wait',state:'pending',questions:[q1],expires_at:later()}}],
};

async function scenario(browser,{mobile}={}){
 const context=await browser.newContext(mobile?{viewport:{width:390,height:780},hasTouch:true,isMobile:true}:{viewport:{width:1280,height:900}});
 const page=await context.newPage();page.setDefaultTimeout(15000);
 const errors=[],puts=[],answers=[],sockets=new Set();let turn=0,last,closedIDs=new Set();
 const running={};
 await context.addInitScript(()=>{if(window===window.top&&/^https?:$/.test(location.protocol))localStorage.setItem('agentbox.language','zh-CN');});
 page.on('pageerror',e=>{if(!cancelledFetchError(e))errors.push(e.message);});
 await context.routeWebSocket('**/api/sessions/*/chat?*',ws=>{
  const session=new URL(ws.url()).pathname.split('/')[3];
  const socket={ws,session};sockets.add(socket);ws.onClose(()=>sockets.delete(socket));
  ws.send(JSON.stringify({type:'status',state:running[session]?'running':'idle'}));
 });
 const emit=(session,message)=>{for(const s of sockets)if(s.session===session)s.ws.send(JSON.stringify(message));};
 const question=(session,view)=>emit(session,{type:'question',kind:'question',event:view,ts:now()});
 const finish=async session=>{
  running[session]=false;last={...last,state:'completed',revision:last.revision+1,updated_at:now()};
  emit(session,{type:'status',state:'idle'});emit(session,{type:'chat_request',version:1,receipt:last});
  await page.waitForFunction(()=>document.querySelector('#chat-send')?.classList.contains('stop')===false);
 };
 await context.route('**/api/**',async route=>{
  const req=route.request(),path=new URL(req.url()).pathname,session=path.split('/')[3];let body={},status=200;
  if(path==='/api/login')body={token:'alice-token'};
  else if(path==='/api/me')body={user:'alice',role:'user',draft_protocol:1,draft_scope:'c'.repeat(64),chat_protocol:1,chat_scope:scope,prompt_suggestions:1,chat_questions:1,timezone:'UTC',models:{claude:[{id:'claude-opus-5',label:'Claude Opus 5'}],codex:[{id:'gpt-5.5',label:'GPT-5.5'}]},quota:{metered:false}};
  else if(path==='/api/sessions')body=[{id:'space-a',name:'Space A',user:'alice',agent:'claude',account_id:'cl',status:'running',default_model:'claude-opus-5'},{id:'space-b',name:'Space B',user:'alice',agent:'claude',account_id:'cl',status:'running',default_model:'claude-opus-5'},{id:'space-c',name:'Space C',user:'alice',agent:'codex',account_id:'cx',status:'running',default_model:'gpt-5.5'}];
  else if(path==='/api/accounts')body=[{id:'cl',type:'claude',label:'Claude',cred_status:'ok'},{id:'cx',type:'codex',label:'Codex',cred_status:'ok'}];
  else if(path==='/api/onboarding')body={version:1,can_configure:false,can_create:true,has_workspaces:true,accounts:[],default_models:{claude:'claude-opus-5',codex:'gpt-5.5'},container_resources:{cpus:1,memory_mb:512,pids_limit:128}};
  else if(path.endsWith('/models'))body={models:session==='space-c'?[{id:'gpt-5.5',label:'GPT-5.5'}]:[{id:'claude-opus-5',label:'Claude Opus 5'}],discovery:'available'};
  else if(path.endsWith('/history'))body={entries:history[session]||[],thread:{id:thread,title:'Fixture',turns:1},active_thread:thread,costs:{}};
  else if(path.endsWith('/chat/threads'))body=[{id:thread,title:'Fixture',turns:1}];
  else if(path.includes('/chat/questions/')){
   const id=decodeURIComponent(path.split('/').pop());answers.push({session,id,body:req.postDataJSON()});
   if(closedIDs.has(id)){await route.fulfill({status:409,json:{code:'question_closed',error:'这个问题已经结束'}});return;}
   body={version:1,state:req.postDataJSON().skip?'skipped':'answered'};
  }
  else if(path.includes('/chat/requests')){
   const id=path.split('/')[6];
   if(!id)body={version:1,scope,requests:[],pending:last?.state==='running'?last:null};
   else if(req.method()==='GET'&&last?.request_id===id)body={version:1,receipt:last};
   else if(req.method()==='PUT'){
    const input=req.postDataJSON();puts.push({session,input});turn++;running[session]=true;
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
 const input=page.locator('#chat-input'),card=id=>page.locator(`.question-card[data-question="${id}"]`);
 const open=async session=>{await page.evaluate(id=>{location.hash='#/sessions/'+id+'/chat';},session);await input.waitFor({state:'visible'});await page.waitForTimeout(300);};
 const send=async text=>{await input.fill(text);await page.locator('#chat-send').click();await page.waitForFunction(()=>document.querySelector('#chat-input').value==='');await page.waitForTimeout(150);};
 const label=()=>page.locator('.turn.working .work-label').textContent();
 const noOverflow=async()=>assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1),'page scrolls sideways');

 await open('space-a');
 // History: one summary card with the answer, no raw AskUserQuestion chip.
 await card('toolu_old').waitFor();
 assert.equal(await card('toolu_old').getAttribute('data-state'),'answered');
 assert.match(await card('toolu_old').textContent(),/部署到哪个环境？[\s\S]*staging/);
 assert.equal(await page.locator('#chat-log .tool-chip,#chat-log .chip').filter({hasText:'AskUserQuestion'}).count(),0,'raw question tool drawn');
 assert.equal(await card('toolu_old').locator('input').count(),0,'finished question still answerable');

 // A new turn asks two questions; the page sent questions:true with the request.
 await send('再部署一次');
 assert.equal(puts.at(-1).input.questions,true);
 question('space-a',{id:'toolu_1',state:'pending',questions:[q1,q2],expires_at:later()});
 const c1=card('toolu_1');await c1.waitFor();
 assert.equal(await c1.getAttribute('data-state'),'pending');
 assert.match(await c1.locator('.qa-state').textContent(),/约 10 分钟后自动跳过/);
 assert.equal(await label(),'等你回答…');
 assert.equal(await input.getAttribute('placeholder'),'Agent 在等你回答上面的问题');
 const submit=c1.locator('.qa-submit'),tabs=c1.locator('.qa-tab'),visible=()=>c1.locator('.qa-panel legend').textContent();
 // Two questions: tabs like the CLI, one question shown at a time.
 assert.equal(await tabs.count(),2);
 assert.deepEqual(await tabs.allTextContents(),['环境','检查']);
 assert.equal(await tabs.nth(0).getAttribute('aria-selected'),'true');
 assert.match(await visible(),/部署到哪个环境？/);
 assert.equal(await c1.locator('.qa-group').count(),1,'both questions shown at once');
 assert.ok(await submit.isDisabled(),'submit enabled before answering');
 assert.match(await c1.locator('.qa-progress').textContent(),/已答 0\/2/);
 // Picking a single choice moves on to the next unanswered question.
 await c1.locator('.qa-option',{hasText:'staging'}).click();
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_1"] .qa-tab.active')?.textContent==='检查');
 assert.match(await visible(),/跑哪些检查？/);
 assert.ok(await tabs.nth(0).evaluate(el=>el.classList.contains('done')),'answered tab not marked');
 assert.ok(await submit.isDisabled(),'submit enabled with one question unanswered');
 await c1.locator('.qa-option',{hasText:'lint'}).click();
 await c1.locator('.qa-option',{hasText:'e2e'}).click();
 assert.match(await visible(),/跑哪些检查？/,'multiple choice jumped away');
 await c1.locator('.qa-other-input').fill('只跑改动的包');
 assert.ok(await c1.locator('.qa-other input[type=checkbox]').isChecked(),'typing did not pick 其他');
 assert.match(await c1.locator('.qa-progress').textContent(),/已答 2\/2/);
 // A status refresh while typing must not rebuild the form.
 emit('space-a',{type:'status',state:'running'});await page.waitForTimeout(100);
 assert.equal(await c1.locator('.qa-other-input').inputValue(),'只跑改动的包');
 if(shots){await mkdir(shots,{recursive:true});await c1.screenshot({path:`${shots}/${mobile?'mobile':'desktop'}-pending.png`});}
 // Back to the first tab: the earlier choice is kept and can be changed in place.
 if(!mobile){await tabs.nth(1).focus();await page.keyboard.press('ArrowLeft');}else await tabs.nth(0).click();
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_1"] .qa-tab.active')?.textContent==='环境');
 assert.ok(await c1.locator('.qa-option',{hasText:'staging'}).locator('input').isChecked(),'earlier answer lost');
 await c1.locator('.qa-option',{hasText:'production'}).click();
 await page.waitForTimeout(100);
 assert.match(await visible(),/部署到哪个环境？/,'changing an answer jumped away with everything answered');
 await tabs.nth(1).click();
 assert.equal(await c1.locator('.qa-other-input').inputValue(),'只跑改动的包','other text lost after switching tabs');
 await noOverflow();
 await submit.click();
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_1"]')?.dataset.state==='answered');
 assert.deepEqual(answers.at(-1),{session:'space-a',id:'toolu_1',body:{answers:{[q1.question]:'production',[q2.question]:'lint, e2e, 只跑改动的包'}}});
 assert.equal(await c1.locator('.qa-line').count(),2,'summary is one line per question');
 assert.match(await c1.locator('.qa-line').nth(1).textContent(),/检查跑哪些检查？lint, e2e, 只跑改动的包/);
 if(shots)await c1.screenshot({path:`${shots}/${mobile?'mobile':'desktop'}-answered.png`});
 assert.equal(await label(),'思考中…');
 assert.notEqual(await input.getAttribute('placeholder'),'Agent 在等你回答上面的问题');
 question('space-a',{id:'toolu_1',state:'answered',answers:{[q1.question]:'production',[q2.question]:'lint, e2e, 只跑改动的包'}});
 await page.waitForTimeout(100);
 assert.equal(await page.locator('.question-card[data-question="toolu_1"]').count(),1,'final record drew a second card');

 // Skip: the model is told to go on by itself. A single question needs no tabs.
 question('space-a',{id:'toolu_2',state:'pending',questions:[q1],expires_at:later()});
 await card('toolu_2').locator('.qa-skip').waitFor();
 assert.equal(await card('toolu_2').locator('.qa-tabs').count(),0);
 assert.match(await card('toolu_2').locator('legend').textContent(),/环境部署到哪个环境？/);
 await card('toolu_2').locator('.qa-skip').click();
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_2"]')?.dataset.state==='skipped');
 assert.deepEqual(answers.at(-1).body,{skip:true});
 assert.match(await card('toolu_2').textContent(),/Agent 会按自己的判断继续/);

 // Another page answered first: this one is told, and the card stops taking answers.
 closedIDs.add('toolu_3');
 question('space-a',{id:'toolu_3',state:'pending',questions:[q1],expires_at:later()});
 await card('toolu_3').locator('.qa-option',{hasText:'production'}).click();
 await card('toolu_3').locator('.qa-submit').click();
 await page.locator('.toast',{hasText:'这个问题已经结束'}).first().waitFor();
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_3"]')?.dataset.state==='stale');
 question('space-a',{id:'toolu_3',state:'answered',answers:{[q1.question]:'staging'}});
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_3"]')?.dataset.state==='answered');

 // Expiry recorded by the server; and a question left open when the turn ends.
 question('space-a',{id:'toolu_4',state:'pending',questions:[q1],expires_at:later()});
 question('space-a',{id:'toolu_4',state:'expired'});
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_4"]')?.dataset.state==='expired');
 question('space-a',{id:'toolu_5',state:'pending',questions:[q1],expires_at:later()});
 await card('toolu_5').locator('.qa-submit').waitFor();
 await finish('space-a');
 assert.equal(await card('toolu_5').getAttribute('data-state'),'stale');
 assert.match(await card('toolu_5').textContent(),/回合已结束，未回答/);
 assert.equal(await card('toolu_5').locator('input').count(),0);

 // Reloaded while waiting: answerable only while the turn still runs.
 running['space-b']=true;
 await open('space-b');
 await card('toolu_wait').locator('.qa-submit').waitFor();
 assert.equal(await card('toolu_wait').getAttribute('data-state'),'pending');
 emit('space-b',{type:'status',state:'idle'});
 await page.waitForFunction(()=>document.querySelector('.question-card[data-question="toolu_wait"]')?.dataset.state==='stale');
 // A later message seals it for good, even if a new turn runs.
 await send('换个问题');
 assert.equal(await card('toolu_wait').getAttribute('data-state'),'stale');
 await finish('space-b');

 // Codex never asks, so the request field is not sent.
 await open('space-c');
 await send('codex 任务');
 assert.equal(puts.at(-1).session,'space-c');assert.equal(puts.at(-1).input.questions,undefined);
 if(shots){await open('space-a');await page.locator('#chat-log').screenshot({path:`${shots}/${mobile?'mobile':'desktop'}-log.png`});}
 assert.deepEqual(errors,[]);
 await context.close();
}

const browser=await launchBrowser();
try{
 await scenario(browser);
 await scenario(browser,{mobile:true});
 console.log('chat questions ok');
}finally{await browser.close();server.closeAllConnections();server.close();}
