#!/usr/bin/env node
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir,writeFile} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {launchBrowser,browserEngine,cancelledFetchError} from './playwright-launch.mjs';
import {verifyTraceGeometry} from './collaboration-trace-geometry.mjs';
import {assertActionIcons} from './test-actions.mjs';
const shell=await readFile('internal/web/static/index.html','utf8');const dialogs=[...shell.matchAll(/<dialog\b[\s\S]*?<\/dialog>/g)].map(m=>m[0]).join('');
const tooltip=shell.match(/<div id="tip"[^>]*><\/div>/)?.[0]||'';assert.ok(tooltip,'shell tooltip is missing');
const root=resolve('internal/web/static'),scope='a'.repeat(64),runID='b'.repeat(32),otherRunID='c'.repeat(32),q='d'.repeat(32);let created,otherRun,creates=0,lose=true,loseAction=true,actions=[],mutationRequests=0;
const step=(id,role,kind,state='completed',task=runID)=>({id,run_id:task,role,kind,state,revision:1,participant_generation:1,config_version:1,plan_version:1,turn_id:id,attempt:1,snapshot_id:'0'.repeat(32)});
const planStep=step('1'.repeat(12),'coordinator','plan'),executeStep=step('2'.repeat(12),'executor','execute','running'),reviewStep=step('3'.repeat(12),'coordinator','review'),otherStep=step('4'.repeat(12),'coordinator','plan','running',otherRunID);
const snapshot='9'.repeat(32);reviewStep.snapshot_id=snapshot;
const events=[
 {seq:1,type:'created',created_at:'2026-10-11T10:00:00Z',body:{run_id:runID,text:'Build the feature'}},
 {seq:2,type:'step',created_at:'2026-10-11T10:00:01Z',body:{...planStep,state:'accepted'}},
 {seq:3,type:'step',created_at:'2026-10-11T10:00:04Z',body:planStep},
 {seq:4,type:'message',created_at:'2026-10-11T10:00:04Z',body:{id:q,step_id:planStep.id,sender_role:'coordinator',recipient_role:'executor',type:'plan',config_version:1,plan_version:1,body:{summary:'Plan from prior discussion'}}},
];
const otherEvents=[{seq:1,type:'step',body:otherStep}];
const historyRequests=[],streams=new Map(),heldHistory=[];let delayHistory=false;
const userTurn=step=>({kind:'user',text:'Private internal task prompt',turn:{id:step.turn_id,model:'fixture',effort:'',control:''}});
const codexText=text=>({kind:'event',event:{type:'item.completed',item:{id:'message-'+text,type:'agent_message',text}}});
const claudeText=text=>({kind:'event',event:{type:'assistant',message:{id:'message-'+text,content:[{type:'text',text}]}}});
const histories=new Map([[`${runID}:coordinator`,[userTurn(planStep),codexText('Planner durable output')]], [ `${runID}:executor`,[userTurn(executeStep),claudeText('Executor durable start')]], [`${otherRunID}:coordinator`,[userTurn(otherStep),codexText('Other task durable output')]]]);
const sendStream=(run,step,event)=>{for(const response of streams.get(run)||[])response.write(`data: ${JSON.stringify({version:1,run_id:run,step_id:step.id,role:step.role,event})}\n\n`)};
const until=async fn=>{for(let n=0;n<100;n++){if(fn())return;await new Promise(r=>setTimeout(r,20));}assert.fail('fixture event did not arrive');};
const binding=(agent_type,account_id,model)=>({agent_type,account_id,model,effort:'',effort_control:''});
const options={version:1,scope,thread_mode:1,accounts:[{id:'cc',type:'claude',label:'Claude',default_model:'claude-test',models:[],restricted:false},{id:'cx',type:'codex',label:'Codex',default_model:'codex-test',models:[],restricted:false}]};
// These values follow the API's disjoint token categories, not provider totals
// (where cached input and reasoning can already be included). Null is unknown.
const settledUsage=(s,input,output,cacheRead,cacheWrite,cost)=>({step_id:s.id,run_id:s.run_id,turn_id:s.turn_id,kind:s.kind,role:s.role,state:'completed',status:'settled',input_tokens:input,output_tokens:output,cache_read_tokens:cacheRead,cache_write_tokens:cacheWrite,total_tokens:input+output+cacheRead+cacheWrite,cost_micro_usd:cost,models:['fixture']});
const pendingUsage=s=>({step_id:s.id,run_id:s.run_id,turn_id:s.turn_id,kind:s.kind,role:s.role,state:s.state,status:'pending',input_tokens:null,output_tokens:null,cache_read_tokens:null,cache_write_tokens:null,total_tokens:null,cost_micro_usd:null,models:[]});
const completedUsage=[settledUsage(planStep,1000,100,200,100,1200),settledUsage(executeStep,2000,300,400,100,2800),settledUsage(reviewStep,2500,500,500,0,4200)];
const view=(run=created)=>({version:1,scope,run,configuration:run.request,step:run.id===otherRunID?otherStep:run.state==='executing'?executeStep:null,step_usage:run.id===otherRunID?[pendingUsage(otherStep)]:run.state==='completed'?completedUsage:[completedUsage[0],pendingUsage(executeStep)],cost:{cost_micro_usd:run.state==='completed'?8207:1207,partial:false,decision_cost_micro_usd:7,decision_calls:1,decision_partial:false}});
const read=async req=>{let b='';for await(const x of req)b+=x;return b?JSON.parse(b):{}};
const server=createServer(async(req,res)=>{try{
 const u=new URL(req.url,'http://localhost'),p=u.pathname;let body;
 if(!['GET','HEAD'].includes(req.method))mutationRequests++;
 if(p==='/') {res.setHeader('Content-Type','text/html');res.end(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/css/base.css"><link rel="stylesheet" href="/css/skins.css"><link rel="stylesheet" href="/css/actions.css"><link rel="stylesheet" href="/css/select.css"><link rel="stylesheet" href="/css/chat.css"><link rel="stylesheet" href="/css/collaboration.css"><link rel="stylesheet" href="/css/collaboration-workspace.css"></head><body><section id="tab-chat" class="pane" style="height:96vh;max-width:1100px;margin:auto;display:flex;flex-direction:column;overflow:hidden"><div id="chat-log" class="chat-log"></div><div class="composer"><textarea id="chat-input" rows="3"></textarea><div class="composer-bar"><select id="chat-execution-mode"><option value="chat">普通对话</option><option value="collaboration">多 Agent 协作</option></select><button id="chat-collaboration-setup">设置团队</button><span id="chat-collaboration-hint"></span><div id="picker">model</div><button id="send">发送</button></div></div></section>${dialogs}${tooltip}<script type="module">
 import{S}from'/js/state.js';import{initI18n}from'/js/i18n.js';import{initCollaborationComposer}from'/js/features/collaboration/composer.js';import{collaborationAnchor}from'/js/features/collaboration/thread-view.js';
 initI18n();S.token='synthetic';S.draftScope='${scope}';S.current={id:'source',account_id:'cc',agent:'claude'};S.pick.model='claude-test';let controller;let attachments=[];
 const input=document.getElementById('chat-input');const read=()=>({text:input.value,attachments,...(controller?.draft()?{collaboration:controller.draft()}:{})});
 let activeSession='source',activeThread='thread';
 const reload=async()=>{const r=await fetch('/api/collaboration/runs?session='+activeSession+'&thread='+activeThread).then(r=>r.json());document.getElementById('chat-log').replaceChildren(...r.runs.map(r=>collaborationAnchor(r.id,r.request.text)))};
 const originalFetch=window.fetch.bind(window);window.fetch=(input,options)=>{const url=typeof input==='string'?input:input.url;return originalFetch(input,window.ignoreHistoryAbort&&url.includes('/steps/')&&url.includes('/history')?{...options,signal:undefined}:options);};
 controller=initCollaborationComposer({read,apply:d=>{input.value=d.text;attachments=d.attachments;controller.apply(d.collaboration)},changed:()=>{},save:()=>{},reload,allowed:()=>true});controller.enter('source','thread');window.controller=controller;window.openThread=async(s,th)=>{activeSession=s;activeThread=th;controller.enter(s,th);await reload();await controller.refresh();};window.attach=()=>attachments=[{id:1,path:'/shared/.file/a',name:'a',orig:'a',kind:'file'}];window.detach=()=>attachments=[];document.getElementById('send').onclick=()=>controller.send();await reload();</script></body></html>`);return;}
 if(p==='/api/collaboration/options')body=options;
 else if(p.endsWith('/collaboration/context'))body={version:1,scope,context:{thread_id:'thread',anchor:1,include:u.searchParams.get('include')!=='0',sha256:'e'.repeat(64),text:'User: prior discussion',truncated:false}};
 else if(p==='/api/collaboration/preflight'){const b=await read(req);assert.equal(b.source_session_id,'source');body={version:1,scope,baseline:'f'.repeat(40)}}
 else if(p==='/api/collaboration/runs')body={version:1,scope,runs:u.searchParams.get('session')==='other'?(otherRun?[otherRun]:[]):created?[created]:[]};
 else if(p.endsWith('/application'))body={version:1,application:null};
 else if(p.endsWith('/stream')){const run=p.split('/')[4];res.writeHead(200,{'Content-Type':'text/event-stream','Cache-Control':'no-cache'});res.write('data: {"type":"connected"}\n\n');const clients=streams.get(run)||new Set();clients.add(res);streams.set(run,clients);res.on('close',()=>clients.delete(res));return;}
 else if(p.includes('/steps/')&&p.endsWith('/history')){
  const run=p.split('/')[4],stepID=p.split('/')[6],selected=[planStep,executeStep,reviewStep,otherStep].find(s=>s.id===stepID&&s.run_id===run);assert(selected);
  const entries=histories.get(`${run}:${selected.role}`)||[],offset=Number(u.searchParams.get('after')||0);historyRequests.push({run,step:stepID,offset});
  let index=0,at=0;while(index<entries.length&&at<offset)at+=Buffer.byteLength(JSON.stringify(entries[index++])+'\n');assert.equal(at,offset);
  const page=entries.slice(index,index+1),next=at+page.reduce((n,e)=>n+Buffer.byteLength(JSON.stringify(e)+'\n'),0);
  body={version:1,scope,run_id:run,step_id:stepID,role:selected.role,generation:1,entries:page,next,has_more:index+page.length<entries.length,truncated:false};
  if(delayHistory&&run===runID){heldHistory.push(()=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify({...body,entries:[userTurn(selected),codexText('STALE task output')]}));});return;}
 }
 else if(p.endsWith('/events')){const run=p.split('/')[4],rows=run===otherRunID?otherEvents:events;body={version:1,scope,run_id:run,events:rows.filter(e=>e.seq>Number(u.searchParams.get('after')||0)).slice(0,100)};}
 else if(p.includes('/actions/')){const id=p.split('/').at(-1);if(req.method==='PUT'){const input=await read(req);if(!actions.some(a=>a.id===id)){assert.equal(input.expected_revision,created.revision);actions.push({id,...input});created={...created,revision:created.revision+1,state:input.action==='confirm_plan'?'executing':'paused'};}if(loseAction){req.socket.destroy();return;}}body={version:1,scope,action:{...actions.find(a=>a.id===id),action_id:id}};}
 else if(p.startsWith('/api/collaboration/runs/')){if(req.method==='PUT'){const input=await read(req);if(!created){creates++;created={id:runID,request_id:p.split('/').at(-1),state:'awaiting_plan',phase:'planning',revision:1,config_version:1,plan_version:1,request:input};}else assert.equal(created.request_id,p.split('/').at(-1));if(lose){req.socket.destroy();return;}body={version:1,run:created};}else{if(!created){res.writeHead(404).end('{}');return;}body=view(p.split('/').at(-1)===otherRunID?otherRun:created);}}
 else{const file=resolve(root,'.'+p);if(!file.startsWith(root+'/'))throw Error();res.setHeader('Content-Type',extname(file)==='.js'?'text/javascript':'text/css');res.end(await readFile(file));return;}
 res.setHeader('Content-Type','application/json');res.end(JSON.stringify(body));
 }catch(e){res.writeHead(500).end(JSON.stringify({error:String(e)}));}});
await new Promise(r=>server.listen(0,'127.0.0.1',r));const browser=await launchBrowser();
try{const page=await browser.newPage({viewport:{width:1280,height:900}}),errors=[];page.setDefaultTimeout(12000);page.on('pageerror',e=>{if(!cancelledFetchError(e))errors.push(e.message);console.error(e.stack)});await page.addInitScript(()=>{try{localStorage.setItem('agentbox.language','zh-CN')}catch{}});await page.goto(`http://127.0.0.1:${server.address().port}`);await page.waitForFunction(()=>!document.getElementById('chat-execution-mode').hidden);
 await assertActionIcons(page,'#tab-chat');
 await page.locator('#chat-input').fill('Build the feature');await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();
 await page.getByRole('button',{name:'交换主辅',exact:true}).click();await page.getByRole('button',{name:'查看携带的上下文'}).click();await page.getByText('User: prior discussion',{exact:true}).waitFor();
 await page.evaluate(()=>window.attach());await page.locator('#send').click();await page.getByText('协作模式暂不支持附件，请先移除附件或使用普通对话',{exact:true}).waitFor();assert.equal(creates,0);assert.equal(await page.locator('#chat-input').inputValue(),'Build the feature');await page.evaluate(()=>window.detach());
 await page.locator('#send').click();await page.getByRole('button',{name:'查询待确认操作',exact:true}).waitFor();await page.waitForFunction(()=>!window.controller.busy());assert.equal(creates,1);assert.equal(created.request.roles.coordinator.agent_type,'codex');assert.equal(created.request.roles.executor.agent_type,'claude');assert.equal(created.request.context.text,'User: prior discussion');await assertActionIcons(page,'#tab-chat');
 lose=false;await page.reload();await page.getByRole('button',{name:'查询待确认操作',exact:true}).click();await page.getByRole('button',{name:'确认计划并执行',exact:true}).waitFor();assert.equal(creates,1);assert.equal(await page.locator('dialog[open]').count(),0);
 await page.getByRole('button',{name:'确认计划并执行',exact:true}).click();await page.getByRole('button',{name:'查询待确认操作',exact:true}).waitFor();await page.waitForFunction(()=>!window.controller.busy());loseAction=false;await page.getByRole('button',{name:'查询待确认操作',exact:true}).click();await page.getByText('执行中',{exact:true}).waitFor();assert.equal(actions.length,1);
 for(const event of [
  {type:'step',body:{...executeStep,state:'accepted'}},
  {type:'step',body:executeStep},
  {type:'message',body:{id:'7'.repeat(32),step_id:executeStep.id,sender_role:'executor',recipient_role:'coordinator',type:'question',body:{text:'Clarify expected behavior'}}},
  {type:'message',body:{id:'8'.repeat(32),step_id:planStep.id,sender_role:'coordinator',recipient_role:'executor',type:'answer',body:{text:'Acceptance criteria confirmed'}}},
 ])events.push({seq:events.length+1,created_at:'2026-10-11T10:00:05Z',...event});
 await page.evaluate(()=>window.controller.refresh());
 await page.getByRole('button',{name:'查看过程',exact:true}).click();const output=page.locator('.collaboration-detail .collaboration-run-output');await output.waitFor({state:'attached'});
 const detail=page.locator('.collaboration-detail'),tab=async name=>detail.locator('[role=tab][data-tab='+name+']').click();
 assert.equal(await detail.locator('[role=tabpanel]:visible').count(),1,'detail panes stack instead of switching');
 assert.equal(await output.isVisible(),false,'output occupies the process canvas');
 const trace=page.locator('.collaboration-detail .collaboration-trace');
 assert.equal(await trace.getAttribute('data-mode'),'running');
 assert.equal(await trace.getAttribute('data-density'),'compact');
 assert.ok(await trace.locator('.collaboration-trace-row').count()<8,'compact trace retained transport chatter');
 assert.equal(await trace.locator('[data-trace-total-tokens]').innerText(),'≥1.4k Token');
 assert.ok((await trace.locator('.collaboration-trace-row[data-step="'+executeStep.id+'"] [data-trace-tokens]').allTextContents()).includes('待结算'),'unknown running usage became zero');
 await trace.locator('[data-trace-density]').click();
 assert.equal(await trace.locator('.collaboration-trace-row').count(),8,'full trace added unobserved workflow phases');
 assert.ok((await trace.locator('.collaboration-trace-lane[data-lane=coordinator]').innerText()).includes('Codex'));
 assert.ok((await trace.locator('.collaboration-trace-lane[data-lane=executor]').innerText()).includes('Claude Code'));
 const writesBeforeInspect=mutationRequests;
 await trace.locator('[data-trace-view=events]').click();
 await trace.locator('[data-trace-search]').fill('Acceptance criteria confirmed');
 assert.equal(await trace.locator('.collaboration-trace-row').count(),1);
 await trace.locator('.collaboration-trace-row').click();
 assert.ok((await trace.locator('.collaboration-trace-inspect').innerText()).includes('Acceptance criteria confirmed'));
 await trace.getByRole('button',{name:'查看步骤输出',exact:true}).click();
 await output.locator('[data-output-body]').filter({hasText:'Planner durable output'}).waitFor();
 await output.locator('[data-output-step]').selectOption(executeStep.id,{force:true});
 await tab('overview');await trace.locator('[data-trace-search]').fill('');await trace.locator('[data-trace-view=sequence]').click();await trace.locator('[data-trace-density]').click();
 assert.equal(mutationRequests,writesBeforeInspect,'trace inspection dispatched work');
 await trace.locator('.collaboration-trace-inspect').getByRole('button',{name:'关闭',exact:true}).click();
 await page.emulateMedia({reducedMotion:'no-preference'});await page.waitForTimeout(350);
 assert.ok(await trace.evaluate(e=>e.getAnimations({subtree:true}).some(a=>a.effect?.getTiming().iterations===Infinity)),'running trace has no ongoing motion');
 await mkdir('output/playwright',{recursive:true});
 // The condensed running row represents dispatch; its real self-loop start
 // remains available among the full persisted events.
 await trace.locator('[data-trace-density]').click();
 const motionGeometry=await verifyTraceGeometry(page,trace,{widths:[1440,1024,390],zooms:[1,.8,1.25]});
 await trace.locator('[data-trace-density]').click();
 await writeFile(`output/playwright/collaboration-trace-geometry-${browserEngine()}.json`,JSON.stringify(motionGeometry,null,2)+'\n');
 for(const width of [1440,390]){
  await page.setViewportSize({width,height:900});
  await trace.screenshot({path:`output/playwright/collaboration-chat-trace-${browserEngine()}-${width}.png`});
 }
 await page.emulateMedia({reducedMotion:'reduce'});await page.waitForTimeout(350);
 assert.equal(await trace.evaluate(e=>e.getAnimations({subtree:true}).length),0,'reduced motion still moves or pulses');
 await page.setViewportSize({width:1280,height:900});await tab('output');
 await output.locator('[data-output-body]').filter({hasText:'Executor durable start'}).waitFor();await assertActionIcons(page,'#tab-chat');assert(!((await output.locator('[data-output-body]').innerText()).includes('Private internal task prompt')));
 await until(()=>streams.get(runID)?.size>0);
 sendStream(runID,executeStep,{type:'agent_event',event:{type:'stream_event',event:{type:'content_block_delta',index:0,delta:{type:'text_delta',text:'Streaming executor'}}}});
 await output.locator('[data-output-body]').filter({hasText:'Streaming executor'}).waitFor();
 sendStream(runID,executeStep,{type:'step_event',step_id:executeStep.id,role:'executor',entry:claudeText('Streaming executor finished')});
 await output.locator('[data-output-body]').filter({hasText:'Streaming executor finished'}).waitFor();assert.equal((await output.locator('[data-output-body]').innerText()).split('Streaming executor').length-1,1,'complete provider event duplicated its earlier delta');
 await page.emulateMedia({reducedMotion:'no-preference'});await page.waitForTimeout(100);
 assert.ok(await trace.locator('.collaboration-trace-packet').evaluateAll(nodes=>nodes.every(e=>e.getAnimations().every(a=>a.playState!=='running'))),'hidden output tab kept trace particles running');
 await tab('overview');await page.waitForTimeout(100);
 assert.ok(await trace.locator('.collaboration-trace-packet').evaluateAll(nodes=>nodes.some(e=>e.getAnimations().some(a=>a.playState==='running'))),'returning to a running trace did not resume motion');
 await page.locator('.collaboration-detail').getByRole('button',{name:'关闭',exact:true}).click();
 assert.ok(await trace.locator('.collaboration-trace-packet').evaluateAll(nodes=>nodes.every(e=>e.getAnimations().every(a=>a.playState!=='running'))),'closed process kept trace particles running');
 await page.emulateMedia({reducedMotion:'reduce'});
 created={...created,state:'waiting_user',pending_question:q,revision:created.revision+1};await page.evaluate(()=>window.controller.refresh());await page.getByRole('button',{name:'答复',exact:true}).click();await page.locator('#chat-input').fill('Use the discussed approach');await page.locator('#send').click();await page.getByText('已暂停',{exact:true}).waitFor();assert.equal(actions.at(-1).payload.question_id,q);assert.equal(creates,1);
 await page.getByRole('button',{name:'查看过程',exact:true}).click();await page.locator('.collaboration-detail:not([hidden])').waitFor();await page.getByRole('button',{name:'关闭',exact:true}).click();
 created={...created,state:'completed',phase:'delivery',revision:created.revision+1};delete created.pending_question;
 for(const event of [
  {type:'artifact',body:{id:snapshot,run_id:runID,step_id:executeStep.id,kind:'snapshot',state:'ready'}},
  {type:'message',body:{id:'5'.repeat(32),step_id:executeStep.id,sender_role:'executor',recipient_role:'coordinator',type:'result',config_version:1,plan_version:1,body:{summary:'Implemented requested feature',completed:['Source file updated'],tests:['Model-reported smoke check'],remaining:[]}}},
  {type:'step',body:{...executeStep,state:'completed',revision:2}},
  {type:'verification',body:{run_id:runID,snapshot_id:snapshot,config_version:1,state:'completed',command:'test -f source.txt',output:'platform fixture verified',exit_code:0}},
  {type:'message',body:{id:'6'.repeat(32),step_id:reviewStep.id,sender_role:'coordinator',recipient_role:'executor',type:'review',config_version:1,plan_version:1,body:{decision:'approve',snapshot_id:snapshot,plan_version:1,summary:'Reviewed the implementation against the plan',changes:[]}}},
  {type:'step',body:reviewStep},
  {type:'state',body:created},
 ])events.push({seq:events.length+1,created_at:'2026-10-11T10:00:30Z',...event});
 histories.set(`${runID}:coordinator`,[userTurn(planStep),codexText('Planner durable output'),userTurn(reviewStep),codexText('Review durable final output')]);
 histories.set(`${runID}:executor`,[userTurn(executeStep),claudeText('Executor durable complete')]);
 await page.evaluate(()=>window.controller.refresh());const finalCard=page.locator('.collaboration-thread-body .collaboration-final-result');await finalCard.locator('.collaboration-final-summary').filter({hasText:'Implemented requested feature'}).waitFor();
 assert.equal(await finalCard.getByRole('heading',{name:'最终结果',exact:true}).count(),1);assert((await finalCard.innerText()).includes('Source file updated'));assert((await finalCard.locator('.collaboration-final-review').innerText()).includes('Reviewed the implementation against the plan'));assert(!(await finalCard.locator('.collaboration-final-verification').innerText()).includes('Model-reported smoke check'));
 for(const width of [1440,390]){
  await page.setViewportSize({width,height:900});
  const dialogue=page.locator(`[data-collaboration-run="${runID}"]`);
  const layout=await dialogue.evaluate(e=>{const request=e.querySelector('.collaboration-request'),reply=e.querySelector('.collaboration-thread-body'),final=e.querySelector('.collaboration-final-result');return{thread:e.getBoundingClientRect().toJSON(),request:request.getBoundingClientRect().toJSON(),reply:reply.getBoundingClientRect().toJSON(),requestLabel:e.querySelector('.collaboration-request-label').textContent,borders:[e,reply,final].map(n=>getComputedStyle(n).borderLeftWidth)};});
  assert.ok(Math.abs(layout.request.right-layout.thread.right)<1,'task request is not right aligned');
  assert.ok(Math.abs(layout.reply.left-layout.thread.left)<1,'team answer is not left aligned');
  assert.ok(layout.request.left>layout.reply.left,'request and response are visually indistinguishable');
  assert.ok(layout.requestLabel.includes('你')&&layout.requestLabel.includes('任务需求'));
  assert.deepEqual(layout.borders,['0px','0px','0px'],'conversation retained a vertical rail or card border');
  assert.equal(await dialogue.locator('.collaboration-reply-brands .agent-ico').count(),2);
  assert.equal(await dialogue.locator('.collaboration-reply-role').count(),2);
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
  await page.screenshot({path:`output/playwright/collaboration-dialogue-${browserEngine()}-${width}.png`});
 }
 await page.setViewportSize({width:1280,height:900});
 await assertActionIcons(page,'#tab-chat');
 const writesBefore=mutationRequests;await page.reload();await page.locator('.collaboration-thread-body .collaboration-final-summary').filter({hasText:'Implemented requested feature'}).waitFor();assert.equal(mutationRequests,writesBefore,'refresh reran a completed task');
 await page.getByRole('button',{name:'查看过程',exact:true}).click();await tab('output');await output.locator('[data-output-body]').filter({hasText:'Review durable final output'}).waitFor();await tab('result');assert.equal(await page.locator('.collaboration-detail-body .collaboration-final-summary').filter({hasText:'Implemented requested feature'}).count(),1);await tab('overview');
 assert.equal(await trace.getAttribute('data-mode'),'complete');
 assert.equal(await trace.locator('.collaboration-trace-row[data-kind=result]').count(),2,'execution result and final platform delivery must remain separate');
 assert.equal(await trace.locator('[data-trace-total-tokens]').innerText(),'7.7k Token');
 assert.equal(await trace.locator('[data-trace-total-cost]').innerText(),'$0.0082');
 assert.equal(await trace.locator('[data-trace-decision-cost]').innerText(),'Jev $0.000007');
 for(const [stepID,token,cost] of [[planStep.id,'1.4k','$0.0012'],[executeStep.id,'2.8k','$0.0028'],[reviewStep.id,'3.5k','$0.0042']]){
  const rows=trace.locator('.collaboration-trace-row[data-step="'+stepID+'"]');
  assert.equal((await rows.locator('[data-trace-tokens]').allTextContents()).filter(t=>t===token).length,1,'step token usage was duplicated across its events');
  assert.equal((await rows.locator('[data-trace-cost]').allTextContents()).filter(t=>t===cost).length,1,'step charge was duplicated across its events');
 }
 assert.ok((await trace.locator('.collaboration-trace-row[data-step="'+executeStep.id+'"] [data-trace-duration]').allTextContents()).includes('25.0s'));
 await page.setViewportSize({width:1440,height:900});
 assert.equal(await trace.locator('.collaboration-trace-row').count(),9,'completed compact fixture should fit its nine meaningful steps');
 assert.ok(await trace.locator('.collaboration-trace-viewport').evaluate(e=>e.scrollHeight<=e.clientHeight+1),'nine compact steps require vertical scrolling at 1440×900');
 assert.ok(await detail.evaluate(e=>{const r=e.getBoundingClientRect();return r.top>=0&&r.bottom<=innerHeight+1}),'collaboration workspace extends below the viewport');
 assert.ok(await detail.evaluate(e=>Math.abs(e.getBoundingClientRect().width-document.getElementById('tab-chat').getBoundingClientRect().width)<=1),'workspace leaves the previous chat exposed beside the process');
 const geometry=await page.evaluate(()=>({viewport:innerHeight,document:document.documentElement.scrollHeight,body:document.body.scrollHeight,chat:document.getElementById('tab-chat').getBoundingClientRect().toJSON(),workspace:document.querySelector('.collaboration-detail').getBoundingClientRect().toJSON(),detailScroll:document.querySelector('.collaboration-detail').scrollHeight,detailClient:document.querySelector('.collaboration-detail').clientHeight}));
 assert.ok(geometry.detailScroll<=geometry.detailClient+1,'workspace requires outer scrolling: '+JSON.stringify(geometry));
 assert.ok(geometry.document<=geometry.viewport+1,'process requires page scrolling: '+JSON.stringify(geometry));
 const selectedTab=detail.locator('[role=tab][aria-selected=true]');
 const tabStyle=()=>selectedTab.evaluate(e=>{const s=getComputedStyle(e);return{hover:e.matches(':hover'),selected:e.getAttribute('aria-selected'),width:e.getBoundingClientRect().width,color:s.color,border:s.borderBottomColor,borderWidth:s.borderBottomWidth,borderStyle:s.borderBottomStyle,background:s.backgroundColor};});
 const tabBefore=await tabStyle();
 // A viewport resize can put the pointer over the selected tab. Its hover
 // text deliberately uses --text while the underline retains the accent.
 await page.mouse.move(1,1);
 const tabAfter=await tabStyle();
 await writeFile(`output/playwright/collaboration-tab-style-${browserEngine()}.json`,JSON.stringify({before:tabBefore,after:tabAfter},null,2)+'\n');
 assert.ok(tabBefore.borderWidth==='2px'&&tabBefore.border!=='rgba(0, 0, 0, 0)'&&tabBefore.borderStyle==='solid','selected workspace tab has no visible underline');
 assert.ok(!tabAfter.hover&&tabAfter.border===tabAfter.color,'idle selected workspace tab does not use its accent');
 assert.equal(await detail.locator('[role=tabpanel]:visible').count(),1);
 for(const theme of ['dark','light']){
  await page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
  await page.screenshot({path:`output/playwright/collaboration-compact-${browserEngine()}-${theme}-1440x900.png`});
 }
 await page.evaluate(()=>delete document.documentElement.dataset.theme);
 await page.setViewportSize({width:390,height:900});await trace.locator('[data-trace-view=events]').click();
 assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 await page.screenshot({path:`output/playwright/collaboration-compact-${browserEngine()}-mobile.png`});
 await page.setViewportSize({width:1440,height:900});await trace.locator('[data-trace-view=sequence]').click();
 await page.emulateMedia({reducedMotion:'no-preference'});
 await trace.locator('[data-trace-replay]').click();await trace.getByRole('button',{name:'暂停回放',exact:true}).waitFor();
 await trace.locator('[data-trace-scrub]').evaluate(input=>{input.value=input.max;input.dispatchEvent(new Event('input',{bubbles:true}));});
 const pausedCursor=await trace.locator('[data-trace-scrub]').inputValue();await page.waitForTimeout(180);
 assert.equal(await trace.locator('[data-trace-scrub]').inputValue(),pausedCursor,'paused replay advanced its event cursor');
 assert.ok(await trace.locator('.collaboration-trace-packet').evaluateAll(nodes=>nodes.every(e=>e.getAnimations().every(a=>a.playState!=='running'))),'paused replay kept packets moving');
 assert.equal(await trace.locator('[data-trace-total-tokens]').innerText(),'回放中隐藏当前结算');
 assert.equal(await trace.locator('[data-trace-total-cost]').innerText(),'');
 assert.equal(await trace.locator('[data-trace-decision-cost]').isVisible(),false);
 assert.ok((await trace.locator('[data-trace-cost]').allTextContents()).every(value=>!value.includes('$')),'replay exposed current settlement as historical cost');
 await trace.locator('[data-trace-latest]').click();assert.equal(mutationRequests,writesBefore,'replay reran a completed task');await page.emulateMedia({reducedMotion:'reduce'});
 assert(historyRequests.some(r=>r.run===runID&&r.step===reviewStep.id&&r.offset>0),'durable history stopped at its first byte page');
 await tab('output');const coordinatorStarts=historyRequests.filter(r=>r.run===runID&&r.offset===0&&[planStep.id,reviewStep.id].includes(r.step)).length;
 await output.locator('[data-output-step]').selectOption(planStep.id,{force:true});await output.locator('[data-output-body]').filter({hasText:'Planner durable output'}).waitFor();assert(!(await output.locator('[data-output-body]').innerText()).includes('Review durable final output'));
 await output.locator('[data-output-step]').selectOption(reviewStep.id,{force:true});await output.locator('[data-output-body]').filter({hasText:'Review durable final output'}).waitFor();assert.equal(historyRequests.filter(r=>r.run===runID&&r.offset===0&&[planStep.id,reviewStep.id].includes(r.step)).length,coordinatorStarts,'same participant generation was fetched twice when selecting another step');
 await output.locator('[data-output-step]').selectOption(executeStep.id,{force:true});await output.locator('[data-output-body]').filter({hasText:'Executor durable complete'}).waitFor();assert(!(await output.locator('[data-output-body]').innerText()).includes('Planner durable output'));
 // A response that outlives cancellation must not populate the next task.
 await page.locator('.collaboration-detail').getByRole('button',{name:'关闭',exact:true}).click();delayHistory=true;await page.evaluate(()=>window.ignoreHistoryAbort=true);await page.getByRole('button',{name:'查看过程',exact:true}).click();await until(()=>heldHistory.length>0);
 otherRun={...created,id:otherRunID,request_id:otherRunID,state:'planning',phase:'planning',revision:1,request:{...created.request,source_session_id:'other',text:'Other task',context:{...created.request.context,thread_id:'other-thread'}}};
 await page.evaluate(()=>window.openThread('other','other-thread'));await page.getByRole('button',{name:'查看过程',exact:true}).click();await tab('output');await output.locator('[data-output-body]').filter({hasText:'Other task durable output'}).waitFor();
 for(const release of heldHistory.splice(0))release();delayHistory=false;await page.waitForTimeout(80);assert(!(await output.innerText()).includes('STALE task output'));assert(!(await output.innerText()).includes('Review durable final output'));assert.equal(mutationRequests,writesBefore);
 await tab('overview');assert.equal(await trace.getAttribute('data-replay'),'false','another task inherited the replay cursor');
 assert.ok(await trace.locator('.collaboration-trace-row').evaluateAll((rows,id)=>rows.every(row=>row.dataset.entry.startsWith(id+':')),otherRunID),'late result restored the old task trace');
 assert.equal(await trace.locator('.collaboration-trace-inspect').isVisible(),false,'another task inherited the selected event');
 // A history cursor freezes the evidence being replayed. A new configuration
 // and events arriving from polling must not leak future model bindings into it.
 await trace.locator('[data-trace-scrub]').evaluate(input=>{input.value='0';input.dispatchEvent(new Event('input',{bubbles:true}));});
 assert.equal(await trace.getAttribute('data-replay'),'true');
 const frozenLanes=await trace.locator('.collaboration-trace-lanes').innerText();
 const frozenLimit=await trace.locator('[data-trace-scrub]').getAttribute('max');
 assert(!frozenLanes.includes('codex-test')&&!frozenLanes.includes('claude-test'),'replay header exposed final model selections before their events');
 const nextRoles={coordinator:{...otherRun.request.roles.coordinator,model:'future-planner-fixture'},executor:{...otherRun.request.roles.executor,model:'future-executor-fixture'}};
 otherRun={...otherRun,revision:otherRun.revision+1,config_version:2,request:{...otherRun.request,roles:nextRoles}};
 otherEvents.push({seq:2,type:'configuration',body:{run_id:otherRunID,version:2,configuration:{roles:nextRoles}}});
 await page.evaluate(()=>window.controller.refresh());
 assert.equal(await trace.locator('.collaboration-trace-lanes').innerText(),frozenLanes,'live configuration changed the replay header');
 assert.equal(await trace.locator('[data-trace-scrub]').getAttribute('max'),frozenLimit,'live events changed the frozen replay range');
 assert.equal(await trace.locator('.collaboration-trace-row').count(),0,'live events escaped the replay cursor');
 await trace.locator('[data-trace-latest]').click();
 assert.ok((await trace.locator('.collaboration-trace-lanes').innerText()).includes('future-planner-fixture'));
 assert.equal(await trace.locator('.collaboration-trace-row').count(),2);
 assert.equal(mutationRequests,writesBefore,'replay polling or returning to latest submitted work');
 for(const width of [390,820,1440]){await page.setViewportSize({width,height:900});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));}
 // A phone opening the task starts with readable event rows. A deliberate
 // sequence selection remains available and must survive viewport changes.
 const mobile=await browser.newPage({viewport:{width:390,height:900}});
 mobile.on('pageerror',e=>{if(!cancelledFetchError(e))errors.push(e.message);});
 await mobile.addInitScript(()=>{try{localStorage.setItem('agentbox.language','zh-CN');}catch{}});
 await mobile.goto(`http://127.0.0.1:${server.address().port}`);
 await mobile.getByRole('button',{name:'查看过程',exact:true}).click();
 const mobileTrace=mobile.locator('.collaboration-detail .collaboration-trace');
 assert.equal(await mobileTrace.getAttribute('data-view'),'events','phone opened a mostly offscreen sequence');
 assert.ok((await mobileTrace.innerText()).includes('交接计划'));
 assert.ok(await mobile.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));
 await mobileTrace.screenshot({path:`output/playwright/collaboration-chat-trace-${browserEngine()}-mobile-first.png`});
 await mobileTrace.locator('[data-trace-view=sequence]').click();
 assert.equal(await mobileTrace.getAttribute('data-view'),'sequence');
 assert.ok((await mobileTrace.innerText()).includes('左右滑动查看全部角色'));
 await mobile.setViewportSize({width:1280,height:900});await mobile.setViewportSize({width:390,height:900});
 assert.equal(await mobileTrace.getAttribute('data-view'),'sequence','resize overrode the selected trace view');
 await mobile.evaluate(()=>window.controller.dispose());await mobile.close();
 assert.equal(mutationRequests,writesBefore,'mobile history viewing submitted a task mutation');
 await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/collaboration-chat-${browserEngine()}.png`,fullPage:true});assert.deepEqual(errors,[]);await page.evaluate(()=>window.controller.dispose());assert.equal(await page.locator('.collaboration-detail').count(),0);
 console.log('Collaboration chat: inline modes/context, query-only recovery, same-thread actions, durable dispatch/clarification trace with read-only inspection/replay, approved final result/review/verification, completed durable paginated output, live delta replacement, participant cache, late task isolation and responsive disposal passed.');
}finally{await browser.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
