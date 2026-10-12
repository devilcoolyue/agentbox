// Pure selector regression. No browser, provider, filesystem mutations or CLI.
import test from 'node:test';
import assert from 'node:assert/strict';
import {selectFinalOutcome} from '../internal/web/static/js/features/collaboration/outcome.js';

const id=n=>n.toString(16).padStart(32,'0');
const stepID=n=>n.toString(16).padStart(12,'0');
function fixture({config=1,plan=1,revision=20,base=100,repair=false,handoff=false,reviewHandoff=false,verified=true}={}){
 const run={id:id(1),state:'completed',revision,config_version:config,plan_version:plan};
 const events=[],add=(type,body)=>{const event={seq:events.length+1,type,body,created_at:''};events.push(event);return event;};
 const input=id(base),snapshot=id(base+1),writer=stepID(base+2),resultStep=handoff?stepID(base+3):writer,reviewStep=stepID(base+4);
 const step=(which,role,kind,snapshot_id)=>({id:which,run_id:run.id,role,kind,snapshot_id,state:'completed',config_version:config,plan_version:plan});
 const message=(which,step_id,sender_role,type,body)=>({id:which,step_id,sender_role,type,config_version:config,plan_version:plan,body});
 add('artifact',{id:snapshot,run_id:run.id,step_id:writer,kind:'snapshot',state:'ready'});
 if(handoff)add('step',step(writer,'executor',repair?'repair':'execute',input));
 add('message',message(id(base+5),resultStep,'executor','result',{summary:`result-${base}`,completed:['Implemented'],remaining:[],tests:['Executor reports tests passed']}));
 add('step',step(resultStep,'executor',handoff?'handoff_repair':repair?'repair':'execute',handoff?snapshot:input));
 add('verification',{run_id:run.id,config_version:config,snapshot_id:snapshot,state:verified?'completed':'not_requested',command:verified?'node verify.cjs':'',output:verified?'PLATFORM_OK':'',exit_code:verified?0:null});
 add('message',message(id(base+6),reviewStep,'coordinator','review',{decision:'approve',summary:`review-${base}`,snapshot_id:snapshot,plan_version:plan,changes:[]}));
 add('step',step(reviewStep,'coordinator',reviewHandoff?'handoff_repair':'review',snapshot));
 add('state',{...run});
 return {view:{run},events,snapshot,writer,resultStep,reviewStep};
}
const clone=x=>structuredClone(x);
const renumber=events=>events.map((event,i)=>({...event,seq:i+1}));

test('published output, executor report and platform verification remain distinct',()=>{
 const f=fixture();const original=clone(f);const result=selectFinalOutcome(f.view,f.events);
 assert.equal(result.snapshotID,f.snapshot);
 assert.equal(result.result.summary,'result-100');assert.equal(result.review.summary,'review-100');
 assert.deepEqual(result.result.tests,['Executor reports tests passed']);assert.equal(result.verification.output,'PLATFORM_OK');
 assert.equal(result.verification.exit_code,0);assert.deepEqual(f,original,'selector mutated input');
 result.result.tests.push('local UI edit');assert.deepEqual(f,original,'returned arrays alias persisted input');
});

test('normal writer input snapshot is not its published output snapshot',()=>{
 const f=fixture();assert.notEqual(f.events.find(e=>e.type==='step').body.snapshot_id,f.snapshot);
 assert.ok(selectFinalOutcome(f.view,f.events));
 f.events.find(e=>e.type==='artifact').body.step_id=id(999);
 assert.equal(selectFinalOutcome(f.view,f.events),undefined);
});

test('repair cycle selects the last approved snapshot and current executor report',()=>{
 const old=fixture(),latest=fixture({base:200,repair:true,revision:30});
 old.events=old.events.filter(e=>e.type!=='state');
 const review=old.events.find(e=>e.type==='message'&&e.body.type==='review').body.body;
 review.decision='revise';review.changes=['Fix one issue'];
 const result=selectFinalOutcome(latest.view,renumber([...old.events,...latest.events]));
 assert.equal(result.result.summary,'result-200');assert.equal(result.snapshotID,latest.snapshot);
});

test('old configuration and plan messages cannot become the current final answer',()=>{
 const old=fixture(),current=fixture({config:2,plan:2,revision:40,base:300});
 assert.equal(selectFinalOutcome(current.view,old.events),undefined);
 const events=renumber([...old.events,...current.events]);
 assert.equal(selectFinalOutcome(current.view,events).result.summary,'result-300');
 const changed=clone(events);changed.findLast(e=>e.type==='message'&&e.body.type==='result').body.config_version=1;
 assert.equal(selectFinalOutcome(current.view,changed),undefined);
 const wrongPlan=clone(events);wrongPlan.findLast(e=>e.type==='message'&&e.body.type==='result').body.plan_version=1;
 assert.equal(selectFinalOutcome(current.view,wrongPlan),undefined);
});

test('handoff repair reuses the original writer snapshot with a new message step',()=>{
 const f=fixture({handoff:true,reviewHandoff:true});
 assert.notEqual(f.writer,f.resultStep);assert.ok(selectFinalOutcome(f.view,f.events));
 const wrong=clone(f.events);wrong.find(e=>e.type==='step'&&e.body.id===f.resultStep).body.snapshot_id=id(999);
 assert.equal(selectFinalOutcome(f.view,wrong),undefined);
 const missingWriter=f.events.filter(e=>e.type!=='step'||e.body.id!==f.writer);
 assert.equal(selectFinalOutcome(f.view,missingWriter),undefined);
 const failedWriter=clone(f.events);failedWriter.find(e=>e.type==='step'&&e.body.id===f.writer).body.state='failed';
 assert.equal(selectFinalOutcome(f.view,failedWriter),undefined,'failed writer cannot be acknowledged by a format repair');
});

test('approve alone or a view ahead of its terminal event is not completed evidence',()=>{
 const f=fixture();assert.equal(selectFinalOutcome(f.view,f.events.slice(0,-1)),undefined);
 assert.equal(selectFinalOutcome({run:{...f.view.run,revision:21}},f.events),undefined);
 assert.equal(selectFinalOutcome({run:{...f.view.run,state:'paused'}},f.events),undefined);
 assert.equal(selectFinalOutcome(f.view,f.events.filter(e=>e.type!=='step'||e.body.id!==f.reviewStep)),undefined);
});

test('future events cannot contaminate the already observed completed revision',()=>{
 const f=fixture();const future=fixture({config:2,plan:2,revision:30,base:400});
 const result=selectFinalOutcome(f.view,renumber([...f.events,...future.events]));
 assert.equal(result.result.summary,'result-100');
});

test('platform test state must match the approved snapshot and follow execution',()=>{
 for(const mutate of [v=>v.state='failed',v=>v.snapshot_id=id(999),v=>v.config_version=2,v=>v.exit_code=1]){
  const f=fixture();mutate(f.events.find(e=>e.type==='verification').body);assert.equal(selectFinalOutcome(f.view,f.events),undefined);
 }
 const f=fixture({verified:false});const result=selectFinalOutcome(f.view,f.events);
 assert.equal(result.verification.state,'not_requested');assert.equal(result.verification.command,'');
 assert.deepEqual(result.result.tests,['Executor reports tests passed'],'model claims remain separate from absent platform tests');
});

test('missing version identity and malformed handoffs do not fall back to older messages',()=>{
 for(const mutate of [row=>delete row.config_version,row=>row.step_id=id(999),row=>row.sender_role='coordinator',row=>row.body.summary={text:'wrong'},row=>row.body.tests=['ok',{}]]){
  const f=fixture();mutate(f.events.find(e=>e.type==='message'&&e.body.type==='result').body);assert.equal(selectFinalOutcome(f.view,f.events),undefined);
 }
 const f=fixture();f.events[1].seq=f.events[0].seq;assert.equal(selectFinalOutcome(f.view,f.events),undefined);
});

test('valid server handoff empty slices may be omitted or null',()=>{
 const f=fixture(),body=f.events.find(e=>e.type==='message'&&e.body.type==='result').body.body;
 delete body.completed;body.remaining=null;delete body.tests;
 assert.deepEqual(selectFinalOutcome(f.view,f.events).result,{summary:'result-100',completed:[],remaining:[],tests:[]});
});

function directFixture(options={}){
 const f=fixture({...options,plan:0});f.view.configuration={workflow:{version:1,mode:'direct'}};
 f.view.run.request={workflow:{version:1,mode:'direct'}};
 f.events=renumber(f.events.filter(e=>!(e.type==='message'&&e.body.type==='review')&&!(e.type==='step'&&e.body.role==='coordinator')));
 return f;
}
test('direct delivery confirms executor artifact and verification without inventing review',()=>{
 for(const handoff of [false,true])for(const verified of [false,true]){
  const f=directFixture({handoff,verified}),out=selectFinalOutcome(f.view,f.events);
  assert.ok(out);assert.equal(out.snapshotID,f.snapshot);assert.equal(out.review,null);
  assert.equal(out.verification.state,verified?'completed':'not_requested');
  assert.equal(out.result.summary,'result-100');
 }
});
test('direct delivery still requires aligned completed revision, snapshot and successful verification',()=>{
 for(const mutate of [
  f=>f.events.pop(),
  f=>f.events.find(e=>e.type==='verification').body.exit_code=1,
  f=>f.events.find(e=>e.type==='verification').body.snapshot_id=id(999),
  f=>f.events.find(e=>e.type==='artifact').body.step_id=stepID(999),
  f=>f.events.find(e=>e.type==='message').body.body.remaining=['Unfinished work'],
  f=>f.view.run.revision++,
 ]){const f=directFixture();mutate(f);assert.equal(selectFinalOutcome(f.view,f.events),undefined);}
});
test('absence of review is only accepted for an explicitly frozen direct strategy',()=>{
 const f=directFixture();
 for(const workflow of [undefined,{version:1,mode:'reviewed'}]){
  const v=structuredClone(f.view);v.configuration={workflow};v.run.request={workflow};
  assert.equal(selectFinalOutcome(v,f.events),undefined);
 }
});
