#!/usr/bin/env node
// Delegation in chat against a synthetic API: the composer icon button, its popover switch and
// worker list, the phone layout, the frozen receipt option, live and replayed delegation cards, and the worker-run dialog.
// No Docker, CLI or model calls.
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

const scope='a'.repeat(64),draftScope='c'.repeat(64),delegationID='d'.repeat(32);
const workers=[
 {account_id:'main',account_label:'Main',agent:'claude',model:'claude-sonnet-5-5',model_label:'Sonnet 5.5',tags:{enabled:true,tier:'economy',can:['mechanical_edit','write_tests'],avoid:['billing'],notes:'Steady on explicit interfaces'},priced:true},
 {account_id:'relay',account_label:'Relay',agent:'codex',model:'gpt-5.5-mini',model_label:'GPT mini',tags:{enabled:true,tier:'economy',can:['search']},priced:false},
];
const running={id:delegationID,session_id:'space-a',thread_id:'thread-one',turn_id:'turn-1',seq:1,state:'running',task_type:'mechanical_edit',risks:[],access:'write',
 task:'Rename fooBar to bazQux under src/\nRun the unit tests afterwards.',route:{source:'rules',reason:'only_eligible_worker',eligible:['main/claude-sonnet-5-5'],rejected:{'relay/gpt-5.5-mini':'task_type'}},
 agent:'claude',account_id:'main',model:'claude-sonnet-5-5',worker_label:'Sonnet 5.5',account_label:'Main',created_at:new Date().toISOString(),started_at:new Date().toISOString(),cost_micro_usd:0};
const completed={...running,state:'completed',report:'Renamed **three** call sites.',changes:{known:true,added:['src/new.ts'],modified:['src/a.ts'],deleted:[],total:2,truncated:false},
 finished_at:new Date().toISOString(),duration_ms:65000,cost_micro_usd:210000,feedback:'ok',feedback_note:'checked the diff'};
const declined={...running,id:'e'.repeat(32),seq:2,state:'declined',task_type:'implement_spec',risks:['billing'],task:'Change the settlement rounding',route:{source:'rules',reason:'no_eligible_worker',eligible:[],rejected:{'main/claude-sonnet-5-5':'risk:billing','relay/gpt-5.5-mini':'task_type'}},
 agent:'',account_id:'',model:'',worker_label:'',account_label:'',started_at:'',cost_micro_usd:0};

const browser=await launchBrowser();
try{
 for(const locale of ['zh-CN','zh-TW','en']){
  const context=await browser.newContext({viewport:{width:1280,height:900}}),page=await context.newPage();page.setDefaultTimeout(15000);
  const errors=[],puts=[],sockets=new Set();let history=[],executions=0,offered=workers,optionReads=0;
  const spaces=[
   {id:'space-a',name:'Space A',user:'alice',agent:'claude',account_id:'main',status:'running',default_model:'claude-opus-5'},
   {id:'space-b',name:'Space B',user:'alice',agent:'codex',account_id:'relay',status:'running',default_model:'gpt-5.5'},
  ];
  await context.addInitScript(locale=>{if(window===window.top&&/^https?:$/.test(location.protocol))localStorage.setItem('agentbox.language',locale);},locale);
  const pageError=e=>{if(!cancelledFetchError(e))errors.push(e.message);};page.on('pageerror',pageError);
  await context.routeWebSocket('**/api/sessions/*/chat?*',ws=>{
   const socket={ws,session:new URL(ws.url()).pathname.split('/')[3]};sockets.add(socket);ws.onClose(()=>sockets.delete(socket));
   ws.send(JSON.stringify({type:'status',state:'idle'}));
  });
  const emit=message=>{for(const socket of sockets)if(socket.session==='space-a')socket.ws.send(JSON.stringify(message));};
  await context.route('**/api/**',async route=>{
   const req=route.request(),url=new URL(req.url()),path=url.pathname,session=path.split('/')[3],method=req.method();let body={},status=200;
   if(path==='/api/login')body={token:'alice-token'};
   else if(path==='/api/me')body={user:'alice',role:'user',draft_protocol:1,draft_scope:draftScope,chat_protocol:1,chat_scope:scope,delegation_protocol:1,timezone:'UTC',models:{claude:[{id:'claude-opus-5',label:'Opus 5'}],codex:[{id:'gpt-5.5',label:'GPT-5.5'}]},quota:{metered:false}};
   else if(path==='/api/sessions')body=spaces;
   else if(path==='/api/accounts')body=[{id:'main',type:'claude',label:'Main',cred_status:'ok'},{id:'relay',type:'codex',label:'Relay',cred_status:'ok'}];
   else if(path==='/api/onboarding')body={version:1,can_configure:false,can_create:true,has_workspaces:true,accounts:[],default_models:{},container_resources:{cpus:1,memory_mb:512,pids_limit:128}};
   else if(path.endsWith('/delegation/options')){optionReads++;body={version:1,available:session==='space-a',workers:offered,jev:true,jev_default:false,task_types:['search','mechanical_edit','write_tests','implement_spec'],risks:['billing'],max_calls:8,max_workers:8};}
   else if(path.endsWith('/models'))body={models:session==='space-a'?[{id:'claude-opus-5',label:'Opus 5'}]:[{id:'gpt-5.5',label:'GPT-5.5'}],discovery:'available'};
   else if(path.endsWith('/history'))body={entries:history,thread:{id:'thread-one',title:'Fixture conversation',turns:1},active_thread:'thread-one',costs:{}};
   else if(path.endsWith('/chat/threads'))body=[{id:'thread-one',title:'Fixture conversation',turns:1}];
   else if(path.includes(`/delegations/${delegationID}/log`)){
    assert.equal(url.searchParams.get('after'),'0');
    body={entries:[{kind:'user',text:'Worker input: rename fooBar'},{kind:'event',event:{type:'assistant',message:{content:[{type:'text',text:'Renamed all call sites in the worker run'}]}}}],next:2,has_more:false};
   }
   else if(path.includes('/chat/requests')){
    const id=path.split('/')[6];
    if(!id)body={version:1,scope,requests:[],pending:null};
    else if(method==='PUT'){
     const input=req.postDataJSON();puts.push(input);executions++;status=202;
     body={version:1,receipt:{request_id:id,session_id:session,thread_id:input.thread_id,turn_id:'turn-'+executions,request:input,state:'running',revision:3,created_at:new Date().toISOString(),updated_at:new Date().toISOString()},replayed:false};
     emit({type:'status',state:'running'});
    }else body={version:1,receipt:null};
   }
   else if(path.endsWith('/files')||path==='/api/git/connections')body=[];
   await route.fulfill({status,json:body}).catch(()=>{});
  });
  const tr=(text,vars)=>page.evaluate(async([text,vars])=>(await import('/_v/{{BUILD}}/js/i18n.js')).t(text,vars),[text,vars]);
  const ready=async()=>{await page.locator('#chat-input').waitFor({state:'visible'});await page.waitForFunction(()=>!document.querySelector('#chat-input').disabled);};
  const go=async(id,name)=>{await page.evaluate(id=>location.hash=`#/sessions/${id}/chat`,id);await page.waitForFunction(name=>document.querySelector('#wb-name').textContent===name,name);await ready();};
  const button=page.locator('#chat-delegate'),count=page.locator('#chat-delegate-count'),pop=page.locator('#chat-delegate-pop'),toggle=pop.locator('.dg-switch');
  try{
   await page.goto(base);await page.locator('#login-user').fill('alice');await page.locator('#login-pass').fill('fixture');await page.locator('#login-btn').click();await page.locator('#app').waitFor({state:'visible'});
   await go('space-a','Space A');
   // Off by default: every tagged worker is preselected, Jev follows the default decision mode.
   // The composer shows one icon button; the switch and the worker list live in its popover.
   await button.waitFor({state:'visible'});
   assert.equal(await button.getAttribute('aria-label'),await tr('委派：未开启，点击选择跑腿模型'));
   assert.equal(await count.isHidden(),true);
   await button.click();await pop.waitFor({state:'visible'});
   assert.equal(await button.getAttribute('aria-expanded'),'true');
   assert.equal(await toggle.isChecked(),false);
   await toggle.check();
   assert.equal(await button.evaluate(el=>el.classList.contains('on')),true);
   assert.equal(await count.innerText(),'2');
   assert.equal(await button.getAttribute('aria-label'),await tr('委派已开启：{names}',{names:'Sonnet 5.5、GPT mini'}));
   // The worker list shows tags; unticking a worker and ticking Jev are remembered per workspace.
   const rows=pop.locator('.dg-pop-worker');assert.equal(await rows.count(),3,'two workers and the Jev switch');
   assert.match(await rows.nth(0).innerText(),/Sonnet 5\.5/);
   assert.ok((await rows.nth(0).innerText()).includes(await tr('不接：{list}',{list:await tr('计费')})),'avoided risks are listed');
   assert.ok((await rows.nth(0).innerText()).includes('Steady on explicit interfaces'),'notes are shown');
   assert.ok((await rows.nth(1).innerText()).includes(await tr('未定价')),'unpriced workers are marked');
   assert.ok(!(await rows.nth(0).innerText()).includes(await tr('未定价')));
   assert.equal(await rows.nth(2).locator('input').isChecked(),false,'Jev is off unless it is the default decision mode');
   const han=/[\u4e00-\u9fff]/;
   if(locale==='en')assert.ok(!han.test(await pop.innerText()),'untranslated popover text: '+await pop.innerText());
   await rows.nth(1).locator('input').uncheck();await rows.nth(2).locator('input').check();
   assert.equal(await count.innerText(),'1');
   assert.equal(await pop.locator('.dg-pop-worker').count(),3,'ticking does not rebuild the list');
   // Unticking every worker turns delegation off; the switch waits for a worker again.
   await rows.nth(0).locator('input').uncheck();
   assert.equal(await toggle.isChecked(),false);assert.equal(await toggle.isDisabled(),true);
   assert.equal(await count.isHidden(),true);
   await rows.nth(0).locator('input').check();
   assert.equal(await toggle.isChecked(),true,'ticking a worker switches delegation on');
   await page.keyboard.press('Escape');await pop.waitFor({state:'hidden'});
   assert.equal(await page.evaluate(()=>document.activeElement?.id),'chat-delegate');
   // Phone width: the button is a plain icon next to attach and voice, and the popover stays on screen.
   await page.setViewportSize({width:390,height:844});
   const boxes=await page.evaluate(()=>['btn-attach','btn-voice','chat-delegate','btn-pick','chat-send'].map(id=>document.getElementById(id).getBoundingClientRect().toJSON()));
   for(let i=1;i<boxes.length;i++)assert.ok(boxes[i].left>=boxes[i-1].right-0.5,`composer controls overlap: ${JSON.stringify(boxes)}`);
   assert.ok(boxes[2].width<=36,'the delegate button is icon sized');
   await button.click();await pop.waitFor({state:'visible'});
   const popBox=await pop.boundingBox();
   assert.ok(popBox.x>=0&&popBox.x+popBox.width<=390,'popover fits the phone width');
   await page.mouse.click(20,200);await pop.waitFor({state:'hidden'});
   await page.setViewportSize({width:1280,height:900});
   const saved=JSON.parse(await page.evaluate(()=>localStorage.getItem('agentbox_delegation_space-a')));
   assert.deepEqual(saved,{on:true,workers:['main/claude-sonnet-5-5'],jev:true});
   // Sending freezes the chosen workers and the Jev choice into the receipt.
   await page.locator('#chat-input').fill('Rename fooBar everywhere');await page.locator('#chat-send').click();
   for(let i=0;i<150&&!puts.length;i++)await page.waitForTimeout(100);
   assert.equal(puts.length,1);
   assert.deepEqual(puts[0].delegation,{version:1,workers:[{account_id:'main',model:'claude-sonnet-5-5'}],jev:true});
   // Live: one card per delegation, updated in place; worker output streams into it.
   emit({type:'delegation',delegation:running});
   const card=page.locator(`.delegation-card[data-delegation="${delegationID}"]`);
   await card.waitFor({state:'visible'});assert.equal(await card.getAttribute('data-state'),'running');
   assert.equal((await card.locator('.dg-state').innerText()).trim(),await tr('跑腿执行中'));
   await card.locator('summary').click();
   emit({type:'delegation_event',id:delegationID,entry:{kind:'event',event:{type:'assistant',message:{content:[{type:'text',text:'Working on the rename'}]}},ts:new Date().toISOString()}});
   await card.locator('.dg-live').getByText('Working on the rename').waitFor();
   emit({type:'delegation',delegation:completed});
   await page.locator(`.delegation-card[data-delegation="${delegationID}"][data-state="completed"]`).waitFor();
   assert.equal(await page.locator('.delegation-card').count(),1,'the end update reuses the card');
   assert.equal(await card.locator('.dg-live').count(),0,'live output is dropped once the worker ends');
   emit({type:'delegation',delegation:running});await new Promise(r=>setTimeout(r,50));
   assert.equal(await card.getAttribute('data-state'),'completed','a late start update does not reopen a finished card');
   const detail=await card.innerText();
   assert.ok(detail.includes('src/new.ts')&&detail.includes('src/a.ts'),'platform-measured changes are listed');
   assert.ok(detail.includes('three'),'the report is rendered');
   assert.ok(detail.includes(await tr('按标签：只有这个跑腿能接这类任务')),'the route reason is explained');
   assert.ok(detail.includes(await tr('主 Agent 验收通过')),'the main agent review is shown');
   assert.match(await card.locator('.dg-meta').innerText(),/\$0\.21/);
   if(locale==='en')assert.ok(!han.test(detail),'untranslated card text: '+detail);
   // The worker run is read on demand from its own log.
   await card.locator('.dg-log').click();
   const dialog=page.locator('dialog.dlg-delegation-log');await dialog.waitFor({state:'visible'});
   await dialog.getByText('Renamed all call sites in the worker run').waitFor();
   assert.ok((await dialog.innerText()).includes('Worker input: rename fooBar'));
   await dialog.locator('.dg-log-ok').click();await dialog.waitFor({state:'hidden'});
   // Replayed history merges the start and end records into one card and explains declines.
   history=[
    {kind:'user',text:'Rename fooBar everywhere',ts:new Date().toISOString(),turn:{id:'turn-1'}},
    {kind:'delegation',event:running,ts:new Date().toISOString()},
    {kind:'delegation',event:completed,ts:new Date().toISOString()},
    {kind:'delegation',event:declined,ts:new Date().toISOString()},
   ];
   await page.reload();await page.locator('#app').waitFor({state:'visible'});await ready();
   await page.locator(`.delegation-card[data-delegation="${delegationID}"][data-state="completed"]`).waitFor();
   assert.equal(await page.locator('.delegation-card').count(),2);
   const declinedCard=page.locator(`.delegation-card[data-delegation="${declined.id}"]`);
   assert.equal(await declinedCard.getAttribute('data-state'),'declined');
   await declinedCard.locator('summary').click();
   const why=await declinedCard.innerText();
   assert.ok(why.includes(await tr('不接「{risk}」风险',{risk:await tr('计费')})),'the tag rejection is explained');
   assert.equal(await declinedCard.locator('.dg-log').count(),0,'a declined task has no worker run');
   if(locale==='en')assert.ok(!/[\u4e00-\u9fff]/.test(why),'untranslated declined card: '+why);
   // Preferences survive the reload; a Codex workspace has no main-agent delegation.
   await button.waitFor({state:'visible'});
   assert.equal(await count.innerText(),'1');
   // Worker tags change without a page reload: coming back to the chat tab re-reads the options.
   offered=[workers[0]];
   const reads=optionReads;
   await page.evaluate(()=>location.hash='#/sessions/space-a/files');await page.waitForFunction(()=>document.querySelector('#tab-chat').classList.contains('hidden'));
   await page.evaluate(()=>location.hash='#/sessions/space-a/chat');
   for(let i=0;i<50&&optionReads===reads;i++)await page.waitForTimeout(100);
   assert.ok(optionReads>reads,'returning to the chat tab re-reads the worker list');
   await button.click();await pop.waitFor({state:'visible'});
   await page.waitForFunction(()=>document.querySelectorAll('#chat-delegate-pop .dg-pop-worker').length===2);
   assert.equal(await count.innerText(),'1');
   // Saving model settings in this page re-reads at once; a newly offered worker shows up unticked
   // because this workspace already has a chosen list.
   offered=workers;
   await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/state.js')).emit('models-updated'));
   await page.waitForFunction(()=>document.querySelectorAll('#chat-delegate-pop .dg-pop-worker').length===3);
   assert.equal(await pop.locator('.dg-pop-worker').nth(1).locator('input').isChecked(),false);
   await page.keyboard.press('Escape');await pop.waitFor({state:'hidden'});
   // No worker left: the button disappears and the next message is a plain chat.
   offered=[];
   await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/state.js')).emit('models-updated'));
   await page.waitForFunction(()=>document.querySelector('#chat-delegate-picker').hidden);
   offered=workers;
   await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/state.js')).emit('models-updated'));
   await button.waitFor({state:'visible'});
   assert.equal(await count.innerText(),'1','the remembered choice comes back with the worker');
   await go('space-b','Space B');
   await page.waitForFunction(()=>document.querySelector('#chat-delegate-picker').hidden);
   assert.equal(await button.isHidden(),true);
   assert.deepEqual(errors,[]);
   console.log(`Delegation ${locale}: composer button and switch, worker list, live option refresh, phone layout, frozen receipt option, live/replayed cards, worker log and Codex hiding passed. Synthetic API only.`);
  }finally{await context.close();}
 }
}finally{await browser.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
