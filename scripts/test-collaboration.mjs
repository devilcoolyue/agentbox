// Real browser, synthetic API; does not invoke Docker, CLI or a provider.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {launchBrowser,browserEngine} from './playwright-launch.mjs';

const root=resolve('internal/web/static'),scope='a'.repeat(64),runID='b'.repeat(32);
let created,actions=[],createCount=0,loseReply=true,preflightFailure=true,preflightCalls=0,mutationRequests=0;
const question1='e'.repeat(32),question2='f'.repeat(32),eventCursors=[];
const plannerStep={id:'7'.repeat(12),run_id:runID,turn_id:'8'.repeat(12),role:'coordinator',participant_generation:1,config_version:1,plan_version:1,kind:'plan',state:'completed',revision:2};
const executorStep={id:'9'.repeat(12),run_id:runID,turn_id:'a'.repeat(12),role:'executor',participant_generation:1,config_version:1,plan_version:1,kind:'execute',state:'running',revision:2};
const events=Array.from({length:205},(_,i)=>({seq:i+1,type:'state',body:{},created_at:''}));
events[0]={seq:1,type:'created',created_at:'',body:{run_id:runID,text:'Build fixture functionality'}};
events[2]={seq:3,type:'step',created_at:'',body:{...plannerStep,state:'accepted',revision:1}};
events[3]={seq:4,type:'step',created_at:'',body:plannerStep};
events[4]={seq:5,type:'message',created_at:'',body:{id:'d'.repeat(32),step_id:plannerStep.id,sender_role:'coordinator',recipient_role:'executor',type:'plan',body:{summary:'<img src=x onerror="window.injected=1">fixture plan'}}};
events[150]={seq:151,type:'message',created_at:'',body:{id:question1,sender_role:'coordinator',recipient_role:'user',type:'question',body:{text:'First question'}}};
events[204]={seq:205,type:'message',created_at:'',body:{id:question2,sender_role:'coordinator',recipient_role:'user',type:'question',body:{text:'Latest question'}}};
const choices=[{id:'cc',type:'claude',label:'Claude fixture',default_model:'claude-fixture',restricted:true,models:[{id:'claude-fixture',label:'Claude fixture',reasoning:{support:'supported',control:'effort',levels:['low','high']}}]},{id:'cx',type:'codex',label:'Codex fixture',default_model:'codex-fixture',restricted:true,models:[{id:'codex-fixture',label:'Codex fixture',reasoning:{support:'supported',control:'effort',levels:['low','medium','high']}}]}];
const requestJSON=async req=>{let raw='';for await(const part of req)raw+=part;return raw?JSON.parse(raw):{};};
const route=roles=>({roles,provider_model:'jev-fixture',decisions:{coordinator:{binding:roles.coordinator,source:'jev',reason:'provider_suggestion'},executor:{binding:roles.executor,source:'fallback',reason:'provider_abstained'}}});
const view=()=>({version:1,scope,run:created,configuration:{roles:created.request.roles,auto_execute:false,max_steps:20,max_repairs:2,budget_micro_usd:0},step:created.state==="uncertain"?{id:"uncertain-step",revision:2,role:"executor",state:"uncertain"}:created.state==='executing'?executorStep:null,cost:{cost_micro_usd:1207,partial:false,decision_cost_micro_usd:7,decision_calls:1,decision_partial:false},step_usage:[{step_id:plannerStep.id,run_id:runID,turn_id:plannerStep.turn_id,kind:'plan',role:'coordinator',state:'completed',status:'settled',input_tokens:1000,output_tokens:100,cache_read_tokens:200,cache_write_tokens:100,total_tokens:1400,cost_micro_usd:1200,models:['codex-fixture']}],decision:{state:'frozen',revision:1,result:route(created.request.roles),attempt:{id:'a'.repeat(32),run_id:runID,provider:'jev',model:'jev-fixture',actual_model:'jev-fixture',state:'settled',input_tokens:160,output_tokens:1,tokens_known:true,cost_micro_usd:7,reserved_micro_usd:0}}});
const server=createServer(async(req,res)=>{
 try{
  const url=new URL(req.url,'http://localhost'),path=url.pathname;let body;
  if(!['GET','HEAD'].includes(req.method))mutationRequests++;
  if(path==='/'){
   res.setHeader('Content-Type','text/html');res.end(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="stylesheet" href="/css/base.css"><link rel="stylesheet" href="/css/skins.css"><link rel="stylesheet" href="/css/dialogs.css"><link rel="stylesheet" href="/css/select.css"><link rel="stylesheet" href="/css/collaboration.css"><link rel="stylesheet" href="/css/collaboration-workspace.css"></head><body><button id="btn-collaboration" hidden>Open</button><script type="module">import{S}from'/js/state.js';import{initCollaboration}from'/js/features/collaboration/panel.js';import{initI18n}from'/js/i18n.js';S.token='synthetic';S.user='alice';S.sessions=[{id:'source',name:'Fixture',agent:'codex'}];S.current=S.sessions[0];initI18n();window.dispose=initCollaboration();</script></body></html>`);return;
  }
  if(path==='/api/me')body={collaboration_protocol:1};
  else if(path==='/api/collaboration/options')body={version:1,scope,accounts:choices};
  else if(path==='/api/collaboration/preflight'){preflightCalls++;await new Promise(resolve=>setTimeout(resolve,500));if(preflightFailure){res.statusCode=422;body={code:'collaboration_source_not_repository',operation_id:'c'.repeat(32)};}else body={version:1,scope,baseline:'c'.repeat(40)};}
  else if(path==='/api/collaboration/runs')body={version:1,scope,runs:created?[created]:[]};
  else if(path.endsWith('/inspection'))body={version:1,scope,run_id:runID,artifact:{id:'1'.repeat(32)}};
  else if(path.endsWith('/diff'))body={version:1,scope,run_id:runID,snapshot:{id:'1'.repeat(32),sha256:'2'.repeat(64)},changes:[]};
  else if(path.includes('/steps/')&&path.endsWith('/history')){const stepID=path.split('/')[6],planner=stepID===plannerStep.id;body={version:1,scope,run_id:runID,step_id:stepID,role:planner?'coordinator':'executor',generation:1,entries:Number(url.searchParams.get('after')||0)?[]:planner?[{kind:'user',turn:{id:plannerStep.turn_id}},{kind:'event',event:{type:'item.completed',item:{id:'saved-plan',type:'agent_message',text:'Saved planner output'}}}]:[{kind:'status',state:'error',error:'Synthetic interruption'}],next:100,has_more:false,truncated:false};}
  else if(path.endsWith('/stream')){res.writeHead(200,{'Content-Type':'text/event-stream'});res.end('data: {"type":"connected"}\n\n');return;}
  else if(path.endsWith('/events')){const after=Number(url.searchParams.get('after')||0);eventCursors.push(after);body={version:1,scope,run_id:runID,events:events.filter(e=>e.seq>after).slice(0,100)};}
  else if(path.includes('/actions/')){
   const actionID=path.split('/').at(-1);
   if(req.method==='PUT'){const input=await requestJSON(req);actions.push({id:actionID,...input});created.revision++;created.state=input.action==='cancel'?'cancelled':'paused';body={version:1,action:{action_id:actionID,state:'completed'},replayed:false};}
   else{const found=actions.find(a=>a.id===actionID);if(!found){res.writeHead(404).end('{}');return;}body={version:1,action:found};}
  }
  else if(path.startsWith('/api/collaboration/runs/')){
   if(req.method==='PUT'){
    const input=await requestJSON(req);const requestID=path.split('/').at(-1);if(!created){createCount++;created={id:runID,request_id:requestID,state:'awaiting_plan',phase:'executing',revision:1,config_version:1,plan_version:1,request:input};events[1]={seq:2,type:'decision',created_at:'',body:route(input.roles)};}else assert.equal(created.request_id,requestID);
    if(loseReply){req.socket.destroy();return;}body={version:1,run:created};
   }else{if(!created){res.writeHead(404).end('{}');return;}loseReply=false;body=view();}
  }
  else{
   const file=resolve(root,'.'+path);if(!file.startsWith(root+'/')){res.writeHead(404).end();return;}
   res.setHeader('Content-Type',extname(file)==='.js'?'text/javascript':'text/css');res.end(await readFile(file));return;
  }
  res.setHeader('Content-Type','application/json');res.end(JSON.stringify(body));
 }catch{res.writeHead(500).end('{}');}
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await launchBrowser();const page=await browser.newPage({viewport:{width:1200,height:950}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
const text=source=>page.evaluate(async source=>(await import('/js/i18n.js')).t(source),source);
const click=async source=>page.getByRole('button',{name:await text(source),exact:true}).click();
const tab=async name=>page.locator('.collaboration-dialog [role=tab][data-tab='+name+']').click();
try{
 await page.goto(`http://127.0.0.1:${server.address().port}`);await page.locator('#btn-collaboration:not([hidden])').click();
 const dialog=page.locator('dialog[open]');await dialog.waitFor();
 assert.equal(await dialog.locator('fieldset').count(),2);
 assert.equal(await dialog.locator('fieldset:visible').count(),0,'optional configuration is not collapsed');
 assert.equal(await page.locator('.collaboration-trace-row:visible').count(),0,'draft invents execution events');
 assert.equal(await page.locator('.collaboration-viewbar').isVisible(),false);
 await mkdir('output/playwright',{recursive:true});
 await page.screenshot({path:`output/playwright/collaboration-workspace-${browserEngine()}-dark.png`});
 await page.evaluate(()=>document.documentElement.dataset.theme='light');
 await page.screenshot({path:`output/playwright/collaboration-workspace-${browserEngine()}-light.png`});
 await page.evaluate(()=>delete document.documentElement.dataset.theme);
 assert.equal(await page.locator('.collaboration-node').count(),0,'fixed workflow nodes remain');
 assert.equal(await page.locator('.collaboration-trace-row').count(),0,'draft implies execution');
 await page.locator('.collaboration-settings>summary').click();
 await click('交换主辅');
 await dialog.getByLabel(await text('任务需求'),{exact:true}).fill('Build fixture functionality');
 await page.locator('.collaboration-settings>summary').click();
 await click('开始协作');
 await page.getByRole('button',{name:await text('正在检查项目…'),exact:true}).waitFor();
 assert.ok(await page.getByRole('button',{name:await text('正在检查项目…'),exact:true}).isDisabled());
 await page.getByRole('button',{name:await text('调整项目目录'),exact:true}).waitFor();
 assert.equal(createCount,0,'failed preflight created a task');
 assert.equal(preflightCalls,1);
 assert.equal(await page.evaluate(()=>sessionStorage.getItem('agentbox.collaboration.pending.v1')),null);
 assert.ok((await page.locator('.collaboration-feedback').innerText()).includes(await text('当前项目目录不是 Git 仓库')));
 assert.equal(await dialog.getByLabel(await text('任务需求'),{exact:true}).inputValue(),'Build fixture functionality');
 assert.ok(await page.locator('.collaboration-feedback').evaluate(e=>{const r=e.getBoundingClientRect();return r.top>=0&&r.bottom<=innerHeight;}),'error is outside the viewport');
 await click('调整项目目录');
 assert.ok(await dialog.getByLabel(await text('仓库相对路径'),{exact:true}).isVisible());
 await dialog.getByLabel(await text('仓库相对路径'),{exact:true}).fill('project');
 preflightFailure=false;
 await click('开始协作');
 await page.waitForFunction(()=>sessionStorage.getItem('agentbox.collaboration.pending.v1')!==null);
 await page.waitForFunction(()=>!document.querySelector('form[aria-busy="true"]'));
 assert.equal(createCount,1);assert.equal(created.request.roles.coordinator.agent_type,'codex');assert.equal(created.request.roles.executor.agent_type,'claude');
 assert.equal(created.request.roles.coordinator.model,'codex-fixture');assert.equal(created.request.roles.executor.model,'claude-fixture');
 await page.reload();await page.locator('#btn-collaboration:not([hidden])').click();
 await page.getByRole('button',{name:await text('确认计划并执行'),exact:true}).waitFor();assert.equal(createCount,1,'refresh resent create');
 assert.equal(await page.evaluate(()=>sessionStorage.getItem('agentbox.collaboration.pending.v1')),null);
 assert.equal(await dialog.getByLabel(await text('任务需求'),{exact:true}).inputValue(),'Build fixture functionality','restored task requirement blank');
 assert.equal(await page.locator('.collaboration-history img, .collaboration-focus img').count(),0,'message markup executed');
 assert.equal(await page.evaluate(()=>window.injected),undefined);
 await click('新建协作任务');
 assert.equal(await page.locator('.collaboration-trace-row').count(),0);
 assert.equal(await dialog.getByLabel(await text('任务需求'),{exact:true}).isEnabled(),true);
 await page.locator('.collaboration-task').click();
 await page.getByRole('button',{name:await text('确认计划并执行'),exact:true}).waitFor();
 assert.equal(createCount,1,'switching tasks repeated creation');
 await click('协作设置');await page.locator('.collaboration-settings>summary').click();
 for(const lang of ['en','zh-TW','zh-CN']){
  await page.evaluate(async lang=>(await import('/js/i18n.js')).i18n.setLanguage(lang),lang);
  assert.ok(await page.getByRole('button',{name:await text('交换主辅'),exact:true}).isVisible());
 }
 assert.ok((await page.locator('.collaboration-focus').innerText()).includes('fixture plan'),'plan is not visible beside approval');
 await tab('overview');
 // The sequence consists of persisted events, including decision fallback and
 // cross-agent handoff. Empty state pages must not manufacture execution steps.
 const canvas=page.locator('.collaboration-trace');
 assert.equal(await canvas.getAttribute('data-mode'),'waiting');
 assert.equal(await canvas.getAttribute('data-density'),'compact','trace defaults to verbose transport events');
 assert.ok(await canvas.locator('.collaboration-trace-row').count()<8,'compact mode retained every dispatch and completion');
 await canvas.locator('[data-trace-density]').click();
 assert.equal(await canvas.getAttribute('data-density'),'full');
 assert.equal(await canvas.locator('.collaboration-trace-row').count(),8);
 assert.equal(await canvas.locator('.collaboration-trace-row[data-kind=decision]').count(),2);
 assert.deepEqual(await canvas.locator('.collaboration-trace-row[data-kind=decision] [data-trace-tokens]').allTextContents(),['161','—'],'one Jev call was counted once per selected role');
 assert.deepEqual(await canvas.locator('.collaboration-trace-row[data-kind=decision] [data-trace-cost]').allTextContents(),['$0.000007','—']);
 assert.equal(await canvas.locator('[data-trace-total-tokens]').innerText(),'1.4k Token','decision tokens were folded into coding-step tokens');
 assert.ok((await canvas.locator('[data-trace-total-cost]').getAttribute('title')).includes('$0.001207'),'precise total omitted the small decision charge');
 assert.ok((await canvas.locator('.collaboration-trace-lane[data-lane=coordinator]').innerText()).includes('Codex'),'swapped coordinator is not shown');
 assert.ok((await canvas.locator('.collaboration-trace-lane[data-lane=executor]').innerText()).includes('Claude Code'),'swapped executor is not shown');
 assert.ok((await canvas.innerText()).includes(await text('按事件顺序排列')),'missing timestamps were replaced with invented timings');
 assert.equal(await canvas.locator('img').count(),0,'trace interpreted message markup');
 await canvas.locator('.collaboration-trace-row[data-kind=decision]').last().click();
 assert.ok((await canvas.locator('.collaboration-trace-inspect').innerText()).includes('claude-fixture'));
 assert.ok((await canvas.locator('.collaboration-trace-inspect').innerText()).includes(await text('建议不够确定，使用回退模型')));
 await canvas.locator('[data-trace-search]').fill('fixture plan');
 assert.equal(await canvas.locator('.collaboration-trace-row').count(),1,'trace search retained unrelated events');
 await canvas.locator('[data-trace-search]').fill('');
 await canvas.locator('[data-trace-view=events]').click();
 assert.equal(await canvas.locator('.collaboration-trace-row').count(),8,'event list changed durable evidence');
 await canvas.locator('[data-trace-view=sequence]').click();
 // Status animation follows durable state; this fixture is not live provider evidence.
 await page.emulateMedia({reducedMotion:'no-preference'});
 created.state='executing';created.phase='executing';created.revision++;
 events.push({seq:206,type:'step',created_at:'',body:{...executorStep,state:'accepted',revision:1}},{seq:207,type:'step',created_at:'',body:executorStep});
 await page.waitForFunction(()=>document.querySelector('.collaboration-trace')?.dataset.mode==='running');
 await page.waitForFunction(()=>document.querySelectorAll('.collaboration-trace-row').length===10);
 await page.waitForTimeout(350);
 assert.ok(await canvas.evaluate(e=>e.getAnimations({subtree:true}).some(a=>a.effect?.getTiming().iterations===Infinity)),'running state has no motion');
 assert.equal(await page.locator('.collaboration-node').count(),0,'fixed workflow nodes returned');
 await canvas.locator('[data-trace-density]').click();
 await page.setViewportSize({width:1440,height:900});
 for(const theme of ['dark','light']){
  await page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
  await page.waitForTimeout(350);
  await page.screenshot({path:`output/playwright/collaboration-trace-${browserEngine()}-${theme}.png`});
 }
 for(const skin of ['amber','glass','cyberpunk','graphite','verdant','blueprint']){
  await page.evaluate(skin=>document.documentElement.dataset.skin=skin,skin);
  for(const width of [1440,1024,820,390,320]){
   await page.setViewportSize({width,height:950});
   assert.ok(await canvas.evaluate(e=>e.scrollWidth<=e.clientWidth+1),`${skin} trace overflows at ${width}`);
   assert.ok(await dialog.evaluate(e=>e.scrollWidth<=e.clientWidth+1),`${skin} task dialog overflows at ${width}`);
  }
 }
 await page.evaluate(()=>{delete document.documentElement.dataset.skin;document.documentElement.dataset.theme='dark';});
 await page.setViewportSize({width:390,height:950});
 await page.waitForTimeout(350);
 await page.screenshot({path:`output/playwright/collaboration-trace-${browserEngine()}-mobile.png`});
 await page.setViewportSize({width:1200,height:950});
 await canvas.locator('[data-trace-density]').click();
 await page.emulateMedia({reducedMotion:'reduce'});
 assert.equal(await canvas.evaluate(e=>e.getAnimations({subtree:true}).length),0,'reduced motion still animates');
 await page.emulateMedia({reducedMotion:'no-preference'});
 created.state='paused';created.revision++;
 await page.waitForFunction(()=>document.querySelector('.collaboration-trace')?.dataset.mode==='stopped');
 await page.waitForTimeout(350);
 assert.equal(await canvas.evaluate(e=>e.getAnimations({subtree:true}).filter(a=>a.effect?.getTiming().iterations===Infinity).length),0,'paused task still appears to execute');
 for(const lang of ['en','zh-TW','zh-CN']){
  await page.evaluate(async lang=>(await import('/js/i18n.js')).i18n.setLanguage(lang),lang);
  assert.ok((await canvas.locator('.collaboration-trace-status').innerText()).includes(await text('已暂停')));
  assert.equal((await canvas.locator('[data-trace-view=sequence]').innerText()).trim(),await text('派发时序'));
 }
 created.state='completed';created.revision++;
 await page.waitForFunction(()=>document.querySelector('.collaboration-trace')?.dataset.mode==='complete');
 assert.equal(await canvas.locator('.collaboration-trace-row').count(),10,'run completion invented missing step evidence');
 await tab('overview');
 const writesBeforeTrace=mutationRequests;
 await canvas.locator('[data-trace-replay]').click();
 await page.getByRole('button',{name:await text('暂停回放'),exact:true}).waitFor();
 await canvas.locator('[data-trace-replay]').click();
 await canvas.locator('[data-trace-scrub]').evaluate(input=>{input.value='1';input.dispatchEvent(new Event('input',{bubbles:true}));});
 assert.ok(await canvas.locator('.collaboration-trace-row').count()<10,'scrub did not change the replay cursor');
 await canvas.locator('[data-trace-latest]').click();
 assert.equal(await canvas.locator('.collaboration-trace-row').count(),10);
 assert.equal(mutationRequests,writesBeforeTrace,'replay or inspection submitted task mutations');
 await canvas.locator('[data-trace-replay]').click();
 await click('协作设置');
 assert.equal(await canvas.getAttribute('data-playing'),'false','leaving the trace kept replay running');
 await tab('activity');await page.locator('.collaboration-run-output [data-output-step]').selectOption(plannerStep.id,{force:true});await page.locator('.collaboration-run-output [data-output-body]').filter({hasText:'Saved planner output'}).waitFor();
 await tab('overview');
 created.state='awaiting_plan';created.revision++;
 await page.waitForFunction(()=>document.querySelector('.collaboration-trace')?.dataset.mode==='waiting');
 await page.emulateMedia({reducedMotion:'reduce'});
 await tab('records');assert.equal(await page.locator('.collaboration-history article:visible').count(),3);
 await tab('overview');
 await page.getByRole('searchbox',{name:await text('搜索任务')}).fill('no-match');
 assert.equal(await page.locator('.collaboration-task').count(),0);
 await page.getByRole('searchbox',{name:await text('搜索任务')}).fill('fixture');
 assert.equal(await page.locator('.collaboration-task').count(),1);
 await page.locator('.collaboration-task').focus();await page.waitForTimeout(2200);
 assert.equal(await page.locator('.collaboration-task').evaluate(e=>e===document.activeElement),true,'poll replaced focused task');
 await click('确认计划并执行');await page.getByRole('button',{name:await text('继续任务'),exact:true}).waitFor();
 const resumeButton=page.getByRole('button',{name:await text('继续任务'),exact:true});
 await resumeButton.focus();await page.waitForTimeout(2200);
 assert.equal(await resumeButton.evaluate(e=>e===document.activeElement),true,'poll replaced focused action');
 assert.equal(actions.length,1);assert.equal(actions[0].action,'confirm_plan');assert.equal(actions[0].payload.plan_version,1);
 assert.ok(eventCursors.includes(100)&&eventCursors.includes(200),'events beyond first page were lost');
 assert.equal(await page.locator('.collaboration-history').getByText('Latest question',{exact:true}).count(),1);
 created.wait_reason='execution_failed';created.revision++;
 await page.getByRole('button',{name:await text('重新准备任务'),exact:true}).waitFor();
 assert.equal(await page.getByRole('button',{name:await text('继续任务'),exact:true}).count(),0,'startup failure offered the same broken retry');
 assert.ok((await page.locator('.collaboration-feedback').innerText()).includes(await text('Agent 运行环境启动失败')));
 delete created.wait_reason;
 created.state='waiting_user';created.pending_question=question1;created.revision++;
 await page.waitForFunction(id=>Array.from(document.querySelectorAll('select')).some(s=>s.value===id),question1);
 assert.equal(await page.evaluate(id=>Array.from(document.querySelectorAll('select')).find(s=>s.value===id)?.options.length,question1),1,'answered/historical questions selectable');
 await dialog.getByLabel(await text('答复'),{exact:true}).fill('Answer for first question');
 created.pending_question=question2;created.revision++;
 await page.waitForFunction(id=>Array.from(document.querySelectorAll('select')).some(s=>s.value===id),question2);
 assert.ok((await page.locator('.collaboration-focus').innerText()).includes('Latest question'));
 assert.equal(await dialog.getByLabel(await text('答复'),{exact:true}).inputValue(),'','old answer carried into new question');
 await dialog.getByLabel(await text('答复'),{exact:true}).fill('Answer for latest question');
 await click('提交答复');await page.getByRole('button',{name:await text('继续任务'),exact:true}).waitFor();
 assert.equal(actions[1].payload.question_id,question2);
 assert.equal(actions[1].payload.text,'Answer for latest question');
 assert.ok(eventCursors.includes(207),'poll did not preserve event cursor');
 created.state='uncertain';created.revision++;
 await page.getByRole('button',{name:await text('核对运行结果'),exact:true}).waitFor();
 const countBefore=actions.length;await click('核对运行结果');
 await page.getByRole('button',{name:await text('确认核对'),exact:true}).waitFor();
 assert.equal(actions.length,countBefore,'opening inspection acknowledged uncertainty');
 assert.ok(await page.getByRole('button',{name:await text('确认核对'),exact:true}).isDisabled());
 await page.getByLabel(await text('我已核对记录并理解上述影响'),{exact:true}).check();await click('确认核对');
 await page.waitForFunction(()=>!document.querySelector('.collaboration-evidence h3'));
 assert.equal(actions.at(-1).action,'review');assert.equal(actions.at(-1).payload.step_id,'uncertain-step');
 await page.screenshot({path:`output/playwright/collaboration-workspace-${browserEngine()}-activity.png`});
 for(const width of [1440,1024,820,390]){
  await page.setViewportSize({width,height:844});await page.waitForTimeout(100);
  assert.ok(await dialog.evaluate(e=>e.scrollWidth<=e.clientWidth+1),`workspace overflows at ${width}`);
  assert.ok(await page.locator('.collaboration-center').evaluate(e=>e.scrollWidth<=e.clientWidth+1),`task surface overflows at ${width}`);
 }
 await tab('records');
 await tab('overview');
 assert.ok(await page.locator('dialog').evaluate(e=>e.scrollWidth<=e.clientWidth+1),'dialog horizontally overflows');
 await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/collaboration-${browserEngine()}-mobile.png`});
 await page.evaluate(()=>window.dispose());assert.equal(await page.locator('dialog').count(),0);assert.equal(await page.locator('#btn-collaboration').isVisible(),false);
 const available=[...choices];
 for(const kind of ['claude','codex']){
  choices.splice(0,choices.length,...available.filter(a=>a.type===kind));
  await page.evaluate(async()=>{window.dispose=(await import('/js/features/collaboration/panel.js')).initCollaboration();});
  await page.locator('#btn-collaboration:not([hidden])').click();
  const kinds=await page.locator('.collaboration-roles fieldset').evaluateAll(nodes=>nodes.map(n=>n.querySelector('select').value));
  assert.deepEqual(kinds,[kind,kind],'single-provider account needs manual repair');
  assert.equal(await page.locator('.collaboration-settings').evaluate(e=>e.open),false);
  await page.evaluate(()=>window.dispose());
 }
 assert.deepEqual(errors,[]);
 console.log('Collaboration browser: independent roles/models, role swap, frozen creation, lost-response refresh without resend, durable actions, paginated events, authoritative pending questions, safe messages, durable sequence/decision/dispatch views, read-only replay/search, reduced motion, three locales, narrow layout and disposal passed');
}catch(error){console.error('Fixture page:',await page.locator('body').innerText());console.error('Page errors:',errors);console.error('Overflow:',await page.locator('dialog *').evaluateAll(nodes=>nodes.filter(e=>e.scrollWidth>e.clientWidth+2).map(e=>({tag:e.tagName,cls:e.className,width:e.clientWidth,scroll:e.scrollWidth})).slice(0,15)));throw error;}finally{await browser.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
