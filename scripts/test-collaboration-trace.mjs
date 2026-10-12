// Persisted-event projection only: no browser, provider or task mutations.
import test from 'node:test';
import assert from 'node:assert/strict';
import {buildTrace,compactTrace} from '../internal/web/static/js/features/collaboration/trace-model.js';
import {traceStepUsage,traceTokenTotal,dollars,tokens} from '../internal/web/static/js/features/collaboration/trace-usage.js';

const id=n=>n.toString(16).padStart(32,'0');
const sid=n=>n.toString(16).padStart(12,'0');
const binding=model=>({agent_type:'codex',account_id:'account',model,effort:'medium',effort_control:'effort'});
const roles={coordinator:binding('planner-1'),executor:binding('worker-1')};
function fixture(){
 const view={run:{id:id(1),state:'executing',revision:10,config_version:1,plan_version:1},configuration:{roles},step:{id:sid(1),state:'running',role:'executor',revision:3}};
 const events=[];
 const add=(type,body)=>{const seq=events.length+1;events.push({seq,type,body,created_at:new Date(Date.UTC(2026,9,11,1,0,seq)).toISOString()});return events.at(-1);};
 const step=(state,which=1,kind='execute',owner='executor',config=1)=>add('step',{id:sid(which),run_id:view.run.id,role:owner,kind,config_version:config,plan_version:1,state,revision:events.length+1});
 const message=(which,type,from,to,body)=>add('message',{id:id(which),step_id:sid(1),sender_role:from,recipient_role:to,type,body,config_version:1,plan_version:1});
 return {view,events,add,step,message};
}

test('empty/unknown events do not invent a fixed workflow or Jev invocation',()=>{
 const f=fixture();f.view.decision={state:'claimed',revision:2};
 assert.deepEqual(buildTrace(f.view,[]),[]);
 f.add('future_event',{from:'coordinator',to:'executor'});
 f.add('step',{id:sid(1),kind:'imagined',role:'executor',state:'running'});
 assert.deepEqual(buildTrace(f.view,f.events),[]);
});

test('direct trace skips inactive coordinator decisions and never invents an approval',()=>{
 const f=fixture();f.view.configuration.workflow={version:1,mode:'direct'};
 f.add('decision',{roles,decisions:{coordinator:{binding:roles.coordinator,source:'manual',reason:'fixed_model'},executor:{binding:roles.executor,source:'builtin',reason:'unique_candidate'}}});
 f.step('completed');f.message(10,'result','executor','user',{summary:'Delivered'});
 f.add('state',{id:f.view.run.id,state:'completed',revision:20,config_version:1,plan_version:0});
 const trace=buildTrace(f.view,f.events);
 assert.equal(trace.filter(e=>e.kind==='decision').length,1);
 assert.ok(trace.every(e=>e.from!=='coordinator'&&e.to!=='coordinator'));
 assert.equal(trace.at(-1).detail,'未做独立模型评审');
 assert.ok(!trace.some(e=>e.label==='评审通过'));
});

test('one platform dispatch and a stable start precede the actual completion event',()=>{
 const f=fixture();f.add('created',{text:'Fix the project',roles});f.step('accepted');f.step('starting');f.step('running');
 const before=buildTrace(f.view,f.events),work=before.find(r=>r.kind==='work');
 assert.equal(before.filter(r=>r.label==='派发执行').length,1);
 assert.deepEqual([before[1].from,before[1].to],['platform','executor']);
 assert.equal(work.tone,'active');assert.equal(work.detail,'worker-1');
 f.step('finalizing');f.step('completed');f.view.step=null;
 const after=buildTrace(f.view,f.events),started=after.find(r=>r.id===work.id),completed=after.find(r=>r.label==='执行回合结束');
 assert.equal(after.length,before.length+1);assert.equal(started.seq,work.seq);assert.equal(started.at,work.at);assert.equal(started.detail,'worker-1');
 assert.equal(started.label,'开始执行');assert.equal(started.tone,'normal');
 assert.equal(completed.seq,f.events.at(-1).seq);assert.equal(completed.at,f.events.at(-1).created_at);
 assert.equal(completed.label,'执行回合结束');assert.equal(completed.tone,'success');assert.match(completed.detail,/worker-1 · 3\.0s/);
});

test('handoff arrows use only server sender/recipient, with actual repair loops',()=>{
 const f=fixture();
 f.message(10,'plan','coordinator','executor',{summary:'Plan'});
 f.message(11,'result','executor','coordinator',{summary:'Result'});
 f.message(12,'review','coordinator','executor',{decision:'revise',summary:'Fix edge case'});
 f.step('accepted',2,'repair');f.step('running',2,'repair');f.step('completed',2,'repair');
 f.message(13,'result','executor','coordinator',{summary:'Repaired'});
 f.message(14,'review','coordinator','executor',{decision:'approve',summary:'Approved'});
 const rows=buildTrace(f.view,f.events),messages=rows.filter(r=>r.kind==='handoff'||r.label==='提交执行结果');
 assert.deepEqual(messages.map(r=>[r.from,r.to,r.label]),[
  ['coordinator','executor','交接计划'],['executor','coordinator','提交执行结果'],['coordinator','executor','退回修改'],
  ['executor','coordinator','提交执行结果'],['coordinator','executor','评审通过'],
 ]);
 assert.ok(rows.some(r=>r.label==='派发返工'));
 assert.ok(!rows.some(r=>r.label==='交付结果已生成'),'approval alone is not delivery');
});

test('questions and replies preserve the actual recipients',()=>{
 const f=fixture();f.message(10,'question','executor','coordinator',{text:'Which version?'});
 f.message(11,'question','coordinator','user',{text:'Confirm version'});
 f.add('state',{id:f.view.run.id,state:'waiting_user',config_version:1,revision:5});
 f.message(12,'answer','user','coordinator',{text:'Version two',question_id:id(11)});
 f.message(13,'answer','coordinator','executor',{text:'Use version two',question_id:id(10)});
 const rows=buildTrace(f.view,f.events);
 assert.deepEqual(rows.map(r=>r.label),['请求澄清','向你提问','等待你的答复','用户答复','答复澄清']);
 assert.deepEqual(rows.filter(r=>r.kind==='handoff').map(r=>[r.from,r.to]),[['executor','coordinator'],['coordinator','user'],['user','coordinator'],['coordinator','executor']]);
});

test('decision freezes show models and real fallback reasons, no invented confidence',()=>{
 const f=fixture();f.add('decision',{roles,provider_model:'jev-1.13.0',decisions:{
  coordinator:{binding:roles.coordinator,source:'manual',reason:'fixed_model'},
  executor:{binding:roles.executor,source:'fallback',reason:'provider_abstained'},
 }});
 const rows=buildTrace(f.view,f.events);
 assert.deepEqual(rows.map(r=>r.label),['指定模型','回退模型']);
 assert.equal(rows[1].from,'decision');assert.equal(rows[1].to,'executor');assert.equal(rows[1].tone,'warning');
 assert.equal(rows[1].detail,'worker-1 · 建议不够确定，使用回退模型 · jev-1.13.0');
 assert.ok(!rows.some(r=>/\d+%|延迟/.test(r.detail)));
});

test('historical configurations keep their original model after reconfiguration',()=>{
 const f=fixture();f.add('created',{roles});f.step('accepted');f.step('running');f.step('completed');
 const next={coordinator:binding('planner-2'),executor:binding('worker-2')};
 f.add('configuration',{version:2,configuration:{roles:next}});f.step('accepted',2,'execute','executor',2);f.step('running',2,'execute','executor',2);
 f.view.configuration.roles=next;f.view.run.config_version=2;f.view.step.id=sid(2);
 const rows=buildTrace(f.view,f.events);
 assert.match(rows.find(r=>r.stepID===sid(1)&&r.kind==='work').detail,/^worker-1/);
 assert.match(rows.find(r=>r.stepID===sid(2)&&r.kind==='work').detail,/^worker-2/);
});

test('automatic validation/delivery transitions are not reported as user pauses',()=>{
 const f=fixture(),state=(state,wait_reason)=>f.add('state',{id:f.view.run.id,state,wait_reason,config_version:1});
 state('paused','verification_required');state('paused','verification_required');
 f.add('verification',{run_id:f.view.run.id,helper_id:id(10),snapshot_id:id(20),config_version:1,state:'running',command:'node verify.cjs'});
 f.add('verification',{run_id:f.view.run.id,helper_id:id(10),snapshot_id:id(20),config_version:1,state:'completed',exit_code:0,command:'node verify.cjs'});
 state('paused','delivery_required');state('completed');
 f.add('application',{action_id:id(30),state:'completed'});
 const rows=buildTrace(f.view,f.events);
 assert.deepEqual(rows.map(r=>r.label),['安排平台验证','开始平台验证','平台验证通过','准备交付结果','交付结果已生成','结果已应用到项目']);
 assert.equal(rows.filter(r=>r.kind==='wait').length,0);
});

test('model claims and nonzero verification cannot masquerade as platform test success',()=>{
 const f=fixture();f.message(10,'result','executor','coordinator',{summary:'All tests passed'});
 f.add('verification',{helper_id:id(10),state:'completed',exit_code:1,command:'false'});
 f.add('verification',{helper_id:id(11),state:'not_requested',exit_code:null,command:''});
 const rows=buildTrace(f.view,f.events);
 assert.ok(!rows.some(r=>r.label==='平台验证通过'));assert.ok(rows.some(r=>r.label==='未配置平台验证'));
});

test('sort/dedup do not mutate input or cross run identities',()=>{
 const f=fixture();f.add('created',{text:'Task',roles});f.step('accepted');f.step('running');
 const alien=f.add('state',{id:id(99),state:'completed'});
 f.add('step',{id:sid(99),run_id:id(99),state:'completed',role:'executor',kind:'execute'});
 f.events.push({...f.events[1]});f.events.reverse();
 const original=structuredClone(f.events),rows=buildTrace(f.view,f.events);
 assert.deepEqual(f.events,original);assert.deepEqual(rows.map(r=>r.seq),[1,2,3]);
 assert.ok(!rows.some(r=>r.seq===alien.seq));assert.equal(new Set(rows.map(r=>r.id)).size,rows.length);
});

test('paused or completed views cannot animate a stale running step',()=>{
 const f=fixture();f.step('accepted');f.step('running');f.view.run.state='paused';
 let work=buildTrace(f.view,f.events).find(r=>r.kind==='work');
 assert.notEqual(work.tone,'active');assert.equal(work.label,'开始执行');
 f.view.run.state='completed';work=buildTrace(f.view,f.events).find(r=>r.kind==='work');assert.notEqual(work.tone,'active');
});

test('a retry is a new job, not another status update of its failed predecessor',()=>{
 const f=fixture();f.step('accepted');f.step('running');f.step('failed');f.step('accepted',2);f.step('running',2);
 f.view.step.id=sid(2);
 const rows=buildTrace(f.view,f.events);
 assert.equal(rows.filter(r=>r.kind==='dispatch').length,2);
 assert.equal(rows.filter(r=>r.kind==='work').length,3);
 assert.equal(rows.find(r=>r.stepID===sid(1)&&r.label==='回合执行失败').tone,'error');
 assert.equal(rows.find(r=>r.stepID===sid(2)&&r.kind==='work').tone,'active');
});

test('completed events without a start record do not invent a start time or duration',()=>{
 const f=fixture();f.step('completed');const rows=buildTrace(f.view,f.events);
 assert.equal(rows.length,1);assert.equal(rows[0].label,'执行回合结束');
 assert.equal(rows[0].seq,1);assert.equal(rows[0].detail,'worker-1');
});

test('acknowledgement retains the uncertain or failed event and never adds waiting time to execution',()=>{
 for(const state of ['uncertain','failed']){
  const f=fixture();f.step('accepted');f.step('running');f.step(state);
  const prior=buildTrace(f.view,f.events),outcome=prior.find(r=>r.tone===(state==='failed'?'error':'warning'));
  f.step('reviewed');f.events.at(-1).created_at='2026-10-12T01:00:00.000Z';f.view.run.state='paused';f.view.step=null;
  const rows=buildTrace(f.view,f.events),retained=rows.find(r=>r.id===outcome.id),ack=rows.find(r=>r.label==='回合结果已核对');
  assert.deepEqual(retained,outcome,'acknowledgement rewrote earlier outcome');
  assert.equal(ack.seq,f.events.at(-1).seq);assert.equal(ack.at,f.events.at(-1).created_at);
  assert.equal(ack.tone,'normal');assert.equal(ack.detail,'worker-1');assert.ok(!rows.some(r=>r.tone==='success'));
  assert.match(retained.detail,/ · 1\.0s$/,'human waiting time became execution duration');
 }
});

test('recovered verification stop records unknown outcome, not a passed or failed test assertion',()=>{
 const f=fixture(),snapshot=id(30);
 f.add('verification',{snapshot_id:snapshot,config_version:1,state:'uncertain',reason:'server_restarted'});
 f.add('verification_stopped',{snapshot_id:snapshot,config_version:1,state:'failed',reason:'outcome_unknown'});
 const rows=buildTrace(f.view,f.events);
 assert.deepEqual(rows.map(r=>r.label),['验证结果待核对','验证已停止，结果未知']);
 assert.equal(rows[1].seq,2);assert.equal(rows[1].at,f.events[1].created_at);
 assert.ok(rows.every(r=>r.tone==='warning'));assert.ok(!rows.some(r=>['平台验证通过','平台验证失败'].includes(r.label)));
});

test('a normal completed collaboration condenses into nine real rows without losing full evidence',()=>{
 const f=fixture();f.add('created',{text:'Fix calculator',roles});
 f.add('decision',{roles,decisions:{coordinator:{binding:roles.coordinator,source:'manual',reason:'fixed_model'},executor:{binding:roles.executor,source:'builtin',reason:'fixed_model'}}});
 for(const [which,kind,owner,messageType,to] of [[1,'plan','coordinator','plan','executor'],[2,'execute','executor','result','coordinator'],[3,'review','coordinator','review','executor']]){
  f.step('accepted',which,kind,owner);f.step('running',which,kind,owner);
  f.add('message',{id:id(which+10),step_id:sid(which),sender_role:owner,recipient_role:to,type:messageType,body:{summary:kind+' actual handoff',...(messageType==='review'?{decision:'approve'}:{})}});
  f.step('completed',which,kind,owner);
 }
 f.add('state',{id:f.view.run.id,state:'paused',wait_reason:'verification_required',config_version:1});
 f.add('verification',{helper_id:id(30),state:'running',command:'node --test'});
 f.add('verification',{helper_id:id(30),state:'completed',exit_code:0,command:'node --test'});
 f.add('state',{id:f.view.run.id,state:'paused',wait_reason:'delivery_required',config_version:1});
 f.add('state',{id:f.view.run.id,state:'completed',config_version:1});
 f.add('application',{action_id:id(31),state:'completed'});f.view.run.state='completed';f.view.step=null;
 const full=buildTrace(f.view,f.events),copy=structuredClone(full),compact=compactTrace(full);
 assert.equal(compact.length,9);assert.ok(full.length>20);assert.deepEqual(full,copy,'compact view mutated the source trace');
 assert.deepEqual(compact.map(e=>e.label),['提交任务','指定模型','规则选模','交接计划','提交执行结果','评审通过','平台验证通过','交付结果已生成','结果已应用到项目']);
 assert.deepEqual(compact.filter(e=>e.stepID).map(e=>[e.from,e.to]),[['coordinator','executor'],['executor','coordinator'],['coordinator','executor']]);
 assert.ok(compact.filter(e=>e.stepID).every(e=>e.durationMS===2000));
});

test('compact history prefixes never import future completion or handoff content',()=>{
 const f=fixture();f.step('accepted');f.step('running');
 const prefix=buildTrace(f.view,f.events);f.message(10,'result','executor','coordinator',{summary:'Future answer'});f.step('completed');
 const full=buildTrace(f.view,f.events),past=compactTrace(full.filter(e=>e.seq<=prefix.at(-1).seq));
 assert.equal(past.length,1);assert.equal(past[0].label,'开始执行');assert.equal(past[0].durationMS,undefined);
 assert.ok(past.every(e=>!e.detail.includes('Future answer')));
 assert.equal(compactTrace(full).at(-1).label,'提交执行结果');
});

test('compact steps keep clarification, rework and uncertain outcomes visible',()=>{
 const f=fixture();f.step('accepted');f.step('running');
 f.message(10,'question','executor','coordinator',{text:'Clarify version'});
 f.message(11,'answer','coordinator','executor',{text:'Version two'});
 f.message(12,'review','coordinator','executor',{decision:'revise',summary:'Change the edge case'});
 f.step('uncertain');f.step('reviewed');
 const rows=compactTrace(buildTrace(f.view,f.events));
 for(const label of ['开始执行','请求澄清','答复澄清','退回修改','回合结果待核对','回合结果已核对'])assert.ok(rows.some(e=>e.label===label),label+' disappeared');
 assert.ok(!rows.some(e=>e.tone==='success'),'acknowledgement became success');
});

const usageRow=(which,total,cost,status='settled')=>({step_id:sid(which),run_id:id(1),turn_id:sid(which),kind:'execute',role:'executor',state:status==='settled'?'completed':'running',status,input_tokens:total===null?null:1000,output_tokens:total===null?null:100,cache_read_tokens:total===null?null:200,cache_write_tokens:total===null?null:100,total_tokens:total,cost_micro_usd:cost,models:['worker-1']});

test('step accounting appears once across dispatch, start, handoff and completion',()=>{
 const f=fixture();f.step('accepted');f.step('running');f.message(10,'result','executor','coordinator',{summary:'Implemented'});f.step('completed');
 const full=buildTrace(f.view,f.events),usage=[usageRow(1,1400,1200)];
 for(const rows of [full,compactTrace(full)]){
  const charged=rows.map(e=>({e,usage:traceStepUsage(e,rows,usage)})).filter(e=>e.usage);
  assert.equal(charged.length,1);assert.equal(charged[0].usage.cost_micro_usd,1200);
  assert.ok(charged[0].e.id.includes(':end:'));
 }
 assert.deepEqual(traceTokenTotal(usage),{value:1400,partial:false});
});

test('missing tokens, actual zero, and incomplete totals remain distinct',()=>{
 const unknown=usageRow(1,null,null,'pending'),zero=usageRow(2,0,0),known=usageRow(3,1400,1200);
 assert.deepEqual(traceTokenTotal(),{value:null,partial:false});
 assert.deepEqual(traceTokenTotal([unknown]),{value:null,partial:true});
 assert.deepEqual(traceTokenTotal([zero]),{value:0,partial:false});
 assert.deepEqual(traceTokenTotal([known,unknown]),{value:1400,partial:true});
 const unpriced={...known,cost_micro_usd:null,status:'partial'};
 assert.deepEqual(traceTokenTotal([unpriced]),{value:1400,partial:true});
 assert.equal(dollars(0),'$0.0000');assert.equal(dollars(7),'$0.000007');assert.equal(tokens(1400),'1.4k');
});

test('replay never exposes final ledger figures without historical settlement evidence',()=>{
 const f=fixture();f.step('accepted');f.step('running');f.step('completed');
 const rows=buildTrace(f.view,f.events),usage=[usageRow(1,1400,1200)];
 assert.ok(rows.some(e=>traceStepUsage(e,rows,usage)));
 for(const row of rows)assert.equal(traceStepUsage(row,rows,usage,true),undefined);
 for(const row of compactTrace(rows))assert.equal(traceStepUsage(row,compactTrace(rows),usage,true),undefined);
});

test('a completed usage snapshot cannot settle rows whose ending event has not arrived',()=>{
 const f=fixture();f.step('accepted');f.step('running');
 const early=buildTrace(f.view,f.events),settled=[usageRow(1,1400,1200)];
 for(const row of early)assert.equal(traceStepUsage(row,early,settled),undefined,'new ledger snapshot leaked into old event page');
 const pending=[{...usageRow(1,null,null,'pending'),state:'running'}];
 assert.equal(early.filter(row=>traceStepUsage(row,early,pending)).length,1,'in-progress row lost its pending state');
 f.step('completed');const completed=buildTrace(f.view,f.events);
 assert.equal(completed.filter(row=>traceStepUsage(row,completed,settled)).length,1);
});

test('row token totals preserve all disjoint ledger categories and safe sum limits',()=>{
 const plan=usageRow(1,1400,1200),execute=usageRow(2,2800,2800),review=usageRow(3,3500,4200);
 assert.deepEqual(traceTokenTotal([plan,execute,review]),{value:7700,partial:false});
 assert.equal([plan,execute,review].reduce((sum,row)=>sum+row.cost_micro_usd,0),8200,'decision cost must stay separate from model step rows');
 assert.equal(traceTokenTotal([usageRow(1,Number.MAX_SAFE_INTEGER,0),usageRow(2,1,0)]).value,null,'unsafe token aggregate was rounded and displayed');
});
