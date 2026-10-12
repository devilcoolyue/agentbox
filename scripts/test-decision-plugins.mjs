#!/usr/bin/env node
// Synthetic HTTP/browser acceptance. Does not call Jev or either coding model.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {launchBrowser,browserEngine,cancelledFetchError} from './playwright-launch.mjs';
import {validView} from '../internal/web/static/js/features/collaboration/protocol.js';

const shell=await readFile('internal/web/static/index.html','utf8');
const dialogs=[...shell.matchAll(/<dialog\b[\s\S]*?<\/dialog>/g)].map(m=>m[0]).join('');
const settingsSection=shell.slice(shell.indexOf('<section id="sec-decision"'),shell.indexOf('<section id="sec-interface"')).replace('set-section hidden','set-section');
const root=resolve('internal/web/static'),scope='a'.repeat(64),runID='b'.repeat(32);
let lightweight=false,directEvents=[],automatic=true,workspaceProvider='builtin',created,creates=0,lose=false,mismatch=false,writes=[],settlementError=false,reconcileAvailable=false,reconciliations=0,knownCost=40,reservedCost=60,decisionPartial=false;
let decision={revision:1,default_provider:'builtin',fallback_policy:'workspace_default',plugins:{jev:{enabled:false,gateway:'typesafe',model:'jev-1.13.0',timeout_ms:1500,max_request_bytes:32768,max_concurrency:2,access:{mode:'admin'},key_configured:false}}};
const binding=(agent_type,account_id,model)=>({agent_type,account_id,model,effort:'',effort_control:''});
// Rejected protocol values must not expose task controls, even before a browser mounts.
{
 const roles={coordinator:binding('claude','cc','claude-test'),executor:binding('codex','cx','codex-test')};
 const intent={version:1,provider:'builtin',roles:{coordinator:{mode:'auto'},executor:{mode:'fixed'}}};
 const good={version:1,scope,run:{id:runID,request_id:runID,state:'accepted',revision:1,config_version:1,plan_version:0,request:{text:'Task',source_session_id:'source',decision:intent}},configuration:{roles,auto_execute:false,max_steps:20,max_repairs:2,budget_micro_usd:0},step:null,cost:{cost_micro_usd:0,partial:false},decision:{state:'frozen',revision:1,result:{roles:structuredClone(roles),decisions:{coordinator:{binding:structuredClone(roles.coordinator),source:'builtin',reason:'builtin_fallback'},executor:{binding:structuredClone(roles.executor),source:'manual',reason:'fixed_model'}}}}};
 assert(validView(good,runID,scope));
 for(const mutate of [v=>v.decision.state='future_state',v=>delete v.decision.result,v=>v.decision.result=null,v=>delete v.decision,v=>v.decision=null,v=>v.decision.result.decisions.coordinator.binding.model='another-model',v=>v.decision.result.decisions.executor.binding.effort='high']){const bad=structuredClone(good);mutate(bad);assert.equal(validView(bad,runID,scope),false);}
 for(const state of ['pending','claimed','interrupted','deleted']){const v=structuredClone(good);v.decision.state=state;delete v.decision.result;assert(validView(v,runID,scope));}
 const configured=structuredClone(good);configured.run.config_version=2;configured.configuration.roles.coordinator.model='new-configuration-model';assert(validView(configured,runID,scope));
 const deleted=structuredClone(good);deleted.run.state='deleted';delete deleted.run.request;delete deleted.decision;assert(validView(deleted,runID,scope));
}
const options=()=>({version:1,scope,thread_mode:1,...(lightweight?{workflow_version:1}:{}),...(automatic?{automatic_intent_version:1,decision:{jev_available:true,default_provider:'builtin'}}:{}),accounts:[{id:'cc',type:'claude',label:'Claude',default_model:'claude-test',models:[{id:'claude-test',label:'Claude default'},{id:'claude-strong',label:'Claude strong'}],restricted:true},{id:'cx',type:'codex',label:'Codex',default_model:'codex-test',models:[{id:'codex-test',label:'Codex default'}],restricted:true}]});
function view(){
 const run=structuredClone(created),configuration=structuredClone(created.request),auto=created.request.decision;
 if(mismatch&&run.request.workflow){run.request.workflow.mode="reviewed";configuration.workflow.mode="reviewed";}
 if(mismatch&&auto)run.request.decision.roles.coordinator.mode='fixed';
 if(auto)configuration.roles.coordinator={...configuration.roles.coordinator,model:'claude-strong'};
 const decisions=auto?{coordinator:{binding:configuration.roles.coordinator,source:'jev',reason:'provider_suggestion'},executor:{binding:configuration.roles.executor,source:'manual',reason:'fixed_model'}}:null;
 return {version:1,scope,run,configuration,step:null,cost:{cost_micro_usd:200+knownCost,partial:false,...(auto?{decision_cost_micro_usd:knownCost,decision_reserved_micro_usd:reservedCost,decision_calls:1,decision_partial:decisionPartial}:{})},...(auto?{decision:{state:'frozen',revision:2,attempt:{...(settlementError?{error_code:'decision_persistence_failed',can_reconcile:reconcileAvailable}:{})},result:{roles:configuration.roles,decisions}}}:{})};
}
const read=async req=>{let b='';for await(const x of req)b+=x;return b?JSON.parse(b):{}};
const styles=['base','skins','actions','select','chat','collaboration','collaboration-workspace','settings'].map(s=>`<link rel="stylesheet" href="/css/${s}.css">`).join('');
const server=createServer(async(req,res)=>{try{
 const u=new URL(req.url,'http://localhost'),p=u.pathname;let body;
 if(p==='/settings'){
  res.setHeader('Content-Type','text/html');res.end(`<!doctype html><html><head><meta charset="utf-8">${styles}</head><body><div id="set-content">${settingsSection}<div id="set-savebar" hidden><span id="set-savebar-text"></span><button id="set-save">保存</button><button id="set-discard">放弃</button></div><p id="status"></p></div>${dialogs}<script type="module">
   import{S}from'/js/state.js';import{api}from'/js/api.js';import{initI18n}from'/js/i18n.js';import{enhanceSelects}from'/js/select.js';
   import{fillDecisionSettings,decisionSettingsGroup,initDecisionSettings,refreshDecisionSettings}from'/js/features/settings/decision.js';
   import{defineGroups,rebaseline,bindSaveBar,saveDirty,holdDirty}from'/js/features/settings/savebar.js';
   initI18n();S.token='synthetic';const life=new AbortController();let st;
   defineGroups([decisionSettingsGroup()]);initDecisionSettings(life.signal);
   const fill=()=>{fillDecisionSettings(st.decision);rebaseline();};
   window.reloadSettings=async(keep=false)=>{const restore=keep?holdDirty():null;st=await api('/settings');fill();restore?.();refreshDecisionSettings();};
   bindSaveBar(document.getElementById('set-content'),life.signal,()=>void saveDirty(async patch=>{try{st=await api('/settings',{method:'PUT',body:JSON.stringify(patch)});fill();document.getElementById('status').textContent='saved';return st;}catch{document.getElementById('status').textContent='conflict';return null;}}),fill);
   window.disposeSettings=()=>life.abort();await window.reloadSettings();enhanceSelects(document.body);window.ready=true;
  </script></body></html>`);return;
 }
 if(p==='/'){
  res.setHeader('Content-Type','text/html');res.end(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">${styles}</head><body><section id="tab-chat" style="height:96vh;max-width:1100px;margin:auto"><div id="chat-log"></div><div class="composer"><textarea id="chat-input" rows="3"></textarea><div class="composer-bar"><select id="chat-execution-mode"><option value="chat">普通对话</option><option value="collaboration">多 Agent 协作</option></select><button id="chat-collaboration-setup">设置团队</button><span id="chat-collaboration-hint"></span><div id="picker">model</div><button id="send">发送</button></div></div></section>${dialogs}<script type="module">
   import{S}from'/js/state.js';import{initI18n}from'/js/i18n.js';import{initCollaborationComposer}from'/js/features/collaboration/composer.js';import{collaborationAnchor}from'/js/features/collaboration/thread-view.js';
   initI18n();S.token='synthetic';S.draftScope='${scope}';S.current={id:'source',account_id:'cc',agent:'claude'};S.pick.model='claude-test';let controller;
   const input=document.getElementById('chat-input');const read=()=>({text:input.value,attachments:[],...(controller?.draft()?{collaboration:controller.draft()}:{})});
   const reload=async()=>{const r=await fetch('/api/collaboration/runs?session=source&thread=thread').then(r=>r.json());document.getElementById('chat-log').replaceChildren(...r.runs.map(r=>collaborationAnchor(r.id,r.request.text)))};
   controller=initCollaborationComposer({read,apply:d=>{input.value=d.text;controller.apply(d.collaboration)},changed:()=>{},save:()=>{},reload,allowed:()=>true});controller.enter('source','thread');window.controller=controller;document.getElementById('send').onclick=()=>controller.send();await reload();
  </script></body></html>`);return;
 }
 if(p==='/api/settings'){
  if(req.method==='PUT'){
   const b=await read(req);writes.push(b);if(b.decision.expected_revision!==decision.revision){res.writeHead(409,{'Content-Type':'application/json'}).end(JSON.stringify({error:'revision conflict'}));return;}
   const key=b.decision.plugins.jev.api_key;assert(['keep','set','clear'].includes(key.action));
   assert(['typesafe','mindshub'].includes(b.decision.plugins.jev.gateway));
   if(decision.plugins.jev.key_configured&&b.decision.plugins.jev.gateway!==decision.plugins.jev.gateway)assert.notEqual(key.action,'keep');
   decision={...decision,revision:decision.revision+1,default_provider:b.decision.default_provider,plugins:{jev:{...decision.plugins.jev,...b.decision.plugins.jev,key_configured:key.action==='set'?true:key.action==='clear'?false:decision.plugins.jev.key_configured}}};delete decision.plugins.jev.api_key;
  }
  body={decision};
 }else if(p==='/api/collaboration/options')body=options();
 else if(p.endsWith('/collaboration/decision-default')){if(req.method==='PUT'){const b=await read(req);assert.equal(b.scope,scope);workspaceProvider=b.provider;}body={version:1,scope,provider:workspaceProvider};}
 else if(p.endsWith('/collaboration/repositories'))body={version:1,scope,repositories:['.']};
 else if(p.endsWith('/collaboration/context'))body={version:1,scope,context:{thread_id:'thread',anchor:1,include:true,sha256:'e'.repeat(64),text:'User: preserve this context',truncated:false}};
 else if(p==='/api/collaboration/preflight'){const b=await read(req);assert(!JSON.stringify(b).includes('"model":"auto"'));body={version:1,scope,baseline:'f'.repeat(40)}}
 else if(p==='/api/collaboration/runs')body={version:1,scope,runs:created?[created]:[]};
 else if(p.endsWith('/events'))body={version:1,scope,events:directEvents.filter(e=>e.seq>Number(u.searchParams.get('after')||0))};
 else if(p.endsWith('/decision/reconcile')){const b=await read(req);assert.deepEqual(b,{scope,expected_revision:created.revision});assert.equal(req.method,'POST');reconciliations++;settlementError=false;knownCost=100;reservedCost=0;created={...created,state:'awaiting_plan',wait_reason:'',revision:created.revision+1};body={version:1,scope};}
 else if(p.startsWith('/api/collaboration/runs/')){
  if(req.method==='PUT'){const input=await read(req);if(!created){creates++;created={id:runID,request_id:p.split('/').at(-1),state:input.workflow?.mode==='direct'?'executing':'awaiting_plan',phase:input.workflow?.mode==='direct'?'executing':'planning',revision:1,config_version:1,plan_version:input.workflow?.mode==='direct'?0:1,request:input};}else{assert.equal(created.request_id,p.split('/').at(-1));assert.deepEqual(input,created.request);}if(lose){req.socket.destroy();return;}body={version:1,scope,run:created};}
  else{if(!created){res.writeHead(404).end('{}');return;}body=view();}
 }else{
  const file=resolve(root,'.'+p);if(!file.startsWith(root+'/'))throw Error('path');res.setHeader('Content-Type',extname(file)==='.js'?'text/javascript':'text/css');res.end(await readFile(file));return;
 }
 res.setHeader('Content-Type','application/json');res.end(JSON.stringify(body));
 }catch(e){res.writeHead(500).end(JSON.stringify({error:String(e)}));}});
await new Promise(r=>server.listen(0,'127.0.0.1',r));const browser=await launchBrowser();
try{
 const page=await browser.newPage({viewport:{width:1280,height:950}}),errors=[];page.setDefaultTimeout(12000);
 page.on('pageerror',e=>{if(!cancelledFetchError(e)){errors.push(e.message);console.error(e.stack)}});await page.addInitScript(()=>{try{localStorage.setItem('agentbox.language','zh-CN')}catch{}});
 const origin=`http://127.0.0.1:${server.address().port}`;
 await page.goto(origin+'/settings');await page.waitForFunction(()=>window.ready);
 assert.equal(await page.locator('#set-decision-key').inputValue(),'');assert.equal(await page.locator('#set-decision-gateway').inputValue(),'typesafe');assert.equal(writes.length,0);
 await page.locator('#set-decision-key').fill('synthetic-secret');assert.equal(await page.locator('#set-decision-key-action').inputValue(),'set');
 await page.locator('#set-save').click();await page.getByText('saved',{exact:true}).waitFor();assert.equal(writes.length,1);assert.equal(writes[0].decision.plugins.jev.api_key.value,'synthetic-secret');assert.equal(await page.locator('#set-decision-key').inputValue(),'');
 await page.locator('#set-decision-enabled').check();await page.locator('#set-save').click();await page.waitForFunction(()=>document.getElementById('set-decision-revision').value==='3');assert.deepEqual(writes[1].decision.plugins.jev.api_key,{action:'keep'});
 await page.locator('#set-decision-gateway').selectOption('mindshub',{force:true});await page.getByText('Mindshub 需要其对应的 API Key。建议将决策超时设为 15 秒，当前设置不会自动更改。',{exact:true}).waitFor();assert.equal(await page.locator('#set-decision-timeout').inputValue(),'1500');
 await page.locator('#set-save').click();await page.waitForFunction(()=>!document.getElementById('set-decision-key').validity.valid);assert.equal(writes.length,2);assert.equal(decision.plugins.jev.gateway,'typesafe');
 await page.locator('#set-decision-key').fill('synthetic-mindshub-secret');await page.locator('#set-save').click();await page.waitForFunction(()=>document.getElementById('set-decision-revision').value==='4');assert.equal(decision.plugins.jev.gateway,'mindshub');assert.equal(decision.plugins.jev.timeout_ms,1500);assert.equal(writes.at(-1).decision.plugins.jev.api_key.action,'set');assert.equal(await page.locator('#set-decision-key').inputValue(),'');
 await page.locator('#decision-settings-fields').getByText('高级设置',{exact:true}).click();await page.locator('#set-decision-timeout').fill('1600');decision.revision++;await page.evaluate(()=>window.reloadSettings(true));assert.equal(await page.locator('#set-decision-revision').inputValue(),'4');
 await page.locator('#set-save').click();await page.getByText('conflict',{exact:true}).waitFor();assert.equal(decision.plugins.jev.timeout_ms,1500);assert.equal(await page.locator('#set-decision-timeout').inputValue(),'1600');
 await page.evaluate(()=>window.reloadSettings());await page.locator('#set-decision-gateway').selectOption('typesafe',{force:true});await page.locator('#set-decision-key-action').selectOption('clear',{force:true});await page.locator('#set-save').click();await page.waitForFunction(()=>document.getElementById('set-decision-key-state').textContent.includes('尚未'));assert.equal(decision.plugins.jev.key_configured,false);assert.equal(decision.plugins.jev.gateway,'typesafe');assert.equal(decision.plugins.jev.timeout_ms,1500);
 await page.locator('#set-decision-key').fill('discard-on-signout');await page.evaluate(()=>window.disposeSettings());assert.equal(await page.locator('#set-decision-key').inputValue(),'');

 await page.goto(origin);await page.waitForFunction(()=>!document.getElementById('chat-execution-mode').hidden);
 await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();
 await page.locator('[data-use-jev]').check();await page.getByRole('button',{name:'设为此空间默认',exact:true}).click();await page.getByText('已保存此空间的决策偏好',{exact:true}).waitFor();assert.equal(workspaceProvider,'jev');
 await page.reload();await page.waitForFunction(()=>!document.getElementById('chat-execution-mode').hidden);await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();await page.waitForFunction(()=>document.querySelector('[data-use-jev]').checked);
 await page.locator('[data-role-model-mode]').first().selectOption('auto',{force:true});await page.getByRole('button',{name:'交换主辅',exact:true}).click();assert.equal(await page.locator('[data-role-model-mode]').nth(1).inputValue(),'auto');await page.getByRole('button',{name:'交换主辅',exact:true}).click();await page.locator('#chat-input').fill('Implement the feature using prior context');lose=true;
 await page.locator('#send').click();await page.getByRole('button',{name:'查询待确认操作',exact:true}).waitFor();await page.waitForFunction(()=>!window.controller.busy());
 assert.equal(creates,1);assert.equal(created.request.roles.coordinator.model,'claude-test');assert.equal(created.request.decision.roles.coordinator.mode,'auto');assert.equal(created.request.decision.roles.executor.mode,'fixed');assert.equal(created.request.decision.provider,'inherit');
 const stored=await page.evaluate(()=>Object.keys(localStorage).filter(k=>k.includes('.collaboration.pending.')).map(k=>JSON.parse(localStorage.getItem(k))));assert.equal(stored.length,1);assert.equal(stored[0].decision_version,1);assert.match(stored[0].request_fingerprint,/^[a-f0-9]{64}$/);
 mismatch=true;lose=false;await page.reload();await page.getByRole('button',{name:'查询待确认操作',exact:true}).click();await page.getByText('自动任务回执与原请求不一致，请保留待确认记录。',{exact:true}).waitFor();assert.equal(creates,1);
 mismatch=false;await page.evaluate(()=>{for(const k of Object.keys(localStorage).filter(k=>k.includes('.collaboration.pending.'))){const p=JSON.parse(localStorage.getItem(k));p.created=Date.now()-8*86400000;localStorage.setItem(k,JSON.stringify(p));}});await page.reload();await page.getByRole('button',{name:'查询待确认操作',exact:true}).waitFor();
 const expired=await page.evaluate(()=>Object.keys(localStorage).filter(k=>k.includes('.collaboration.pending.')).map(k=>JSON.parse(localStorage.getItem(k))));assert.equal(expired[0].input,null);assert.equal(expired[0].text,'');assert.match(expired[0].request_fingerprint,/^[a-f0-9]{64}$/);
 await page.getByRole('button',{name:'查询待确认操作',exact:true}).click();await page.getByRole('button',{name:'查询待确认操作',exact:true}).waitFor({state:'hidden'});assert.equal(creates,1);
 await page.getByText('主 Agent · claude-strong · Jev 建议的模型',{exact:true}).waitFor();await page.getByText('决策费用 $0.000040',{exact:true}).waitFor();await page.getByText('待核对决策费用，预算占用 $0.000060',{exact:true}).waitFor();
 settlementError=true;created={...created,state:'paused',wait_reason:'decision_persistence_failed',revision:created.revision+1};await page.evaluate(()=>window.controller.refresh());await page.getByText('决策费用尚无确定凭据，请联系管理员核对。',{exact:true}).waitFor();assert.equal(await page.getByRole('button',{name:'核对决策费用',exact:true}).count(),0);assert.equal(await page.getByRole('button',{name:'继续任务',exact:true}).count(),0);
 reconcileAvailable=true;await page.evaluate(()=>window.controller.refresh());await page.getByRole('button',{name:'核对决策费用',exact:true}).click();await page.getByText('决策费用 $0.000100',{exact:true}).waitFor();assert.equal(reconciliations,1);assert.equal(creates,1);
 knownCost=0;reservedCost=0;decisionPartial=true;await page.evaluate(()=>window.controller.refresh());await page.getByText('决策调用用量尚待核对。',{exact:true}).waitFor();assert.equal(await page.getByText('决策费用 $0.000000',{exact:true}).count(),0);assert.equal(creates,1);
 for(const width of [390,820,1440]){await page.setViewportSize({width,height:950});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));}
 await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/decision-plugins-${browserEngine()}.png`,fullPage:true});
 // A server that lacks the capability keeps the original create input shape.
 created=undefined;automatic=false;await page.reload();await page.waitForFunction(()=>!document.getElementById('chat-execution-mode').hidden);await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();assert.equal(await page.locator('[data-role-model-mode]:visible').count(),0);assert.equal(await page.locator('[data-use-jev]:visible').count(),0);
 await page.locator('#chat-input').fill('A fixed task');await page.locator('#send').click();await page.getByRole('button',{name:'确认计划并执行',exact:true}).waitFor();assert.equal(created.request.decision,undefined);assert.equal(creates,2);assert.deepEqual(errors,[]);
 // Opt-in lightweight capability defaults new drafts to direct, retains old preferences,
 // and freezes workflow in pending receipts even when every model is fixed.
 created=undefined;automatic=true;lightweight=true;await page.reload();await page.waitForFunction(()=>!document.getElementById('chat-execution-mode').hidden);
 await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();
 assert.equal(await page.locator('[data-workflow-mode]').inputValue(),'direct');
 assert.equal(await page.locator('.collaboration-roles fieldset:visible').count(),1);
 const oldPreference={mode:'collaboration',repository:'.',include_context:true,configuration:{roles:{coordinator:binding('claude','cc','claude-test'),executor:binding('codex','cx','codex-test')},auto_execute:false,max_repairs:2,max_steps:20,budget_micro_usd:0}};
 await page.evaluate(d=>window.controller.apply(d),oldPreference);assert.equal(await page.locator('[data-workflow-mode]').inputValue(),'reviewed');assert.equal(await page.locator('.collaboration-roles fieldset').first().getByRole('combobox',{name:'选模方式',exact:true}).isEnabled(),true);
 await page.locator('[data-role-model-mode]').first().selectOption('auto',{force:true});
 await page.locator('[data-workflow-mode]').selectOption('direct',{force:true});
 assert.equal(await page.locator('.collaboration-roles fieldset:visible').count(),1);
 for(const width of [390,1440]){await page.setViewportSize({width,height:950});assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1));await page.locator('.collaboration-composer').screenshot({path:`output/playwright/collaboration-direct-settings-${browserEngine()}-${width}.png`});}
 await page.locator('#chat-input').fill('A direct task');lose=true;await page.locator('#send').click();await page.getByRole('button',{name:'查询待确认操作',exact:true}).waitFor();await page.waitForFunction(()=>!window.controller.busy());
 assert.equal(creates,3);assert.equal(created.request.workflow.mode,'direct');assert.equal(created.request.decision,undefined,'unused coordinator auto choice triggered routing');assert.deepEqual(created.request.roles.coordinator,created.request.roles.executor);
 const directPending=await page.evaluate(()=>Object.keys(localStorage).filter(k=>k.includes('.collaboration.pending.')).map(k=>JSON.parse(localStorage.getItem(k))));
 assert.equal(directPending[0].workflow_version,1);assert.equal(directPending[0].decision_version,undefined);assert.match(directPending[0].request_fingerprint,/^[a-f0-9]{64}$/);
 mismatch=true;lose=false;await page.reload();await page.getByRole('button',{name:'查询待确认操作',exact:true}).click();await page.getByText('自动任务回执与原请求不一致，请保留待确认记录。',{exact:true}).waitFor();assert.equal(creates,3);
 mismatch=false;await page.getByRole('button',{name:'查询待确认操作',exact:true}).click();await page.getByRole('button',{name:'查询待确认操作',exact:true}).waitFor({state:'hidden'});assert.equal(creates,3);
 const directStep='1'.repeat(12),directSnapshot='2'.repeat(32),messageID='3'.repeat(32);
 created={...created,state:'completed',revision:4};
 directEvents=[
  {type:'artifact',body:{id:directSnapshot,run_id:runID,step_id:directStep,kind:'snapshot',state:'ready'}},
  {type:'message',body:{id:messageID,step_id:directStep,sender_role:'executor',recipient_role:'user',type:'result',config_version:1,plan_version:0,body:{summary:'Direct fixture delivered',completed:['Implemented'],remaining:[],tests:['Executor checked']}}},
  {type:'step',body:{id:directStep,run_id:runID,role:'executor',kind:'execute',snapshot_id:'0'.repeat(32),state:'completed',config_version:1,plan_version:0}},
  {type:'verification',body:{run_id:runID,config_version:1,snapshot_id:directSnapshot,state:'not_requested',command:'',output:'',exit_code:null}},
  {type:'state',body:{...created}},
 ].map((event,index)=>({...event,seq:index+1,created_at:new Date(1791790000000+index*1000).toISOString()}));
 await page.evaluate(()=>window.controller.refresh());await page.getByText('执行完成，结果待应用',{exact:true}).waitFor();await page.locator('.collaboration-thread-body .collaboration-final-result').getByText('未做独立模型评审',{exact:true}).waitFor();
 assert.equal(await page.getByText('评审通过，结果待应用',{exact:true}).count(),0);
 await page.locator('.collaboration-thread-body').screenshot({path:`output/playwright/collaboration-direct-result-${browserEngine()}.png`});
 await page.getByRole('button',{name:'查看过程',exact:true}).click();
 assert.equal(await page.locator('.collaboration-trace-lane:visible').count(),3);assert.equal(await page.locator('.collaboration-trace-lane[data-lane=coordinator]:visible').count(),0);
 await page.locator('.collaboration-detail .collaboration-trace').screenshot({path:`output/playwright/collaboration-direct-${browserEngine()}.png`});
 assert.deepEqual(errors,[]);await page.evaluate(()=>window.controller.dispose());
 console.log('Decision plugins: redacted settings, revision conflicts, gateway/key isolation without timeout changes, key set/keep/clear, workspace preference, separate fallback/auto intent, mismatched and lost receipt recovery, costs/unknown free usage, direct strategy preference/frozen receipt recovery, truthful direct delivery/three-lane trace and legacy capability gate passed.');
}finally{await browser.close();await new Promise(r=>server.close(r));}
