import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { readChatReceipt, chatStates } from '../internal/web/static/js/contracts/chat.js';
import { workspaceState } from '../internal/web/static/js/contracts/workspace-state.js';
import { validView } from '../internal/web/static/js/features/collaboration/protocol.js';
import { automaticRequestDigest, matchesAutomaticReceipt } from '../internal/web/static/js/features/collaboration/decision.js';
import { CollaborationPending } from '../internal/web/static/js/features/collaboration/pending.js';
import { cleanCollaboration } from '../internal/web/static/js/features/chat/draft-store.js';

const schema = JSON.parse(await readFile(new URL('../contracts/chat-errors-v1.schema.json', import.meta.url)));
const examples = JSON.parse(await readFile(new URL('../contracts/chat-v1.examples.json', import.meta.url)));

test('Go/TS share every durable state, frozen envelope and tombstone shape', () => {
  assert.equal(examples.version, 1);
  assert.deepEqual(chatStates, schema.$defs.ChatRequestReceipt.properties.state.enum);
  assert.deepEqual(examples.receipts.map(c => c.state), chatStates);
  for (const c of examples.receipts) {
    assert.equal(readChatReceipt(c, c.session_id, c.request_id), c);
    assert.throws(() => readChatReceipt(c, 'foreign-space'));
    assert.throws(() => readChatReceipt(c, c.session_id, 'a'.repeat(32)));
  }
});

test('malformed/future receipts cannot acknowledge or release a saved task', () => {
  const original = examples.receipts[0];
  for (const patch of [
    {state: 'future_success'}, {state: 'unconfirmed'}, {state: 'constructor'},
    {revision: 0}, {revision: 1.5}, {revision: Number.MAX_SAFE_INTEGER + 1},
    {request_id: 'unsafe'}, {request: null}, {created_at: undefined}, {error_code: {}},
    {request: {...original.request, scope: 'foreign'}},
    {request: {...original.request, thread_id: 'other-thread'}},
    {request: {...original.request, attachments: ['/shared/.file/../../secret']}},
    {request: {...original.request, attachments: Array(65).fill('/shared/.file/fixture')}},
    {request: {...original.request, effort: 'x'.repeat(513)}},
  ]) assert.throws(() => readChatReceipt({...original, ...patch}, original.session_id), JSON.stringify(patch));
  assert.doesNotThrow(() => readChatReceipt({...original, future_additive_field: true}, original.session_id));
});

test('shared workspace status gives running precedence and distinguishes idle stop', () => {
  const t = value => value;
  assert.equal(workspaceState({status:'running', stop_reason:'idle'}, t).label, '运行中');
  assert.equal(workspaceState({status:'stopped', stop_reason:'idle'}, t).label, '休眠');
  assert.equal(workspaceState({status:'stopped'}, t).label, '已停止');
});

test('collaboration step usage preserves unknowns, separates cache and rejects mixed task evidence', () => {
  const run='b'.repeat(32),scope='a'.repeat(64),binding={agent_type:'codex',account_id:'a',model:'model',effort:'',effort_control:''};
  const view={version:1,scope,run:{id:run,request_id:run,state:'executing',revision:1,config_version:1,plan_version:1},configuration:{roles:{coordinator:binding,executor:binding},auto_execute:true,max_steps:20,max_repairs:2,budget_micro_usd:0},step:null,cost:{cost_micro_usd:42,partial:false}};
  assert(validView(view,run,scope),'legacy API without optional step accounting remains readable');
  const row={step_id:'step',run_id:run,turn_id:'turn',kind:'plan',role:'coordinator',state:'completed',status:'settled',input_tokens:10,output_tokens:5,cache_read_tokens:20,cache_write_tokens:7,total_tokens:42,cost_micro_usd:42,models:['model']};
  const settled={...view,step_usage:[row]};
  assert(validView(settled,run,scope));
  const pending={...row,state:'running',status:'pending',input_tokens:null,output_tokens:null,cache_read_tokens:null,cache_write_tokens:null,total_tokens:null,cost_micro_usd:null,models:[]};
  assert(validView({...view,step_usage:[pending]},run,scope));
  assert(validView({...view,step_usage:[{...row,status:'partial',cost_micro_usd:null}]},run,scope));
  for(const patch of [
    {run_id:'c'.repeat(32)},{total_tokens:62},{total_tokens:-1},{cost_micro_usd:undefined},
    {cost_micro_usd:null},{status:'settled',state:'running'},{status:'pending'},
    {status:'future_status'},{models:['model',42]},
  ]) assert.equal(validView({...view,step_usage:[{...row,...patch}]},run,scope),false,JSON.stringify(patch));
  assert.equal(validView({...view,step_usage:[row,row]},run,scope),false,'duplicate steps could double totals');
  assert.equal(validView({...view,step_usage:[row,{...row,step_id:'other'}]},run,scope),false,'duplicate turns could double totals');
  assert.equal(validView({...view,step_usage:[{...pending,total_tokens:0}]},run,scope),false,'missing usage was converted to zero');
});

test('decision attempt only declares tokens known with explicit numeric evidence', () => {
  const run='b'.repeat(32),scope='a'.repeat(64),binding={agent_type:'codex',account_id:'a',model:'model',effort:'',effort_control:''};
  const view={version:1,scope,run:{id:run,request_id:run,state:'accepted',revision:1,config_version:1,plan_version:0},configuration:{roles:{coordinator:binding,executor:binding},auto_execute:true,max_steps:20,max_repairs:2,budget_micro_usd:0},step:null,cost:{cost_micro_usd:0,partial:false},decision:{state:'claimed',revision:1,attempt:{run_id:run,state:'unknown',tokens_known:false,input_tokens:0,output_tokens:0,cost_micro_usd:0}}};
  assert(validView(view,run,scope));
  assert(validView({...view,decision:{...view.decision,attempt:{}}},run,scope),'older attempt metadata is compatible');
  for(const patch of [{run_id:'c'.repeat(32)},{tokens_known:'true'},{tokens_known:true,input_tokens:undefined},{output_tokens:-1},{state:'future_status'}]){
    assert.equal(validView({...view,decision:{...view.decision,attempt:{...view.decision.attempt,...patch}}},run,scope),false,JSON.stringify(patch));
  }
});

test('direct request receipt binds frozen strategy and fixed models even without automatic selection', async () => {
 const request='a'.repeat(32),binding={agent_type:'codex',account_id:'a',model:'mini',effort:'',effort_control:''};
 const input={scope:'scope',text:'Task',source_session_id:'source',roles:{coordinator:binding,executor:binding},workflow:{version:1,mode:'direct'}};
 const response={version:1,scope:input.scope,run:{request_id:request,request:structuredClone(input)}};
 assert.equal(matchesAutomaticReceipt(response,input,request),true);
 const digest=await automaticRequestDigest(input);assert.match(digest,/^[a-f0-9]{64}$/);
 for(const alter of [v=>delete v.workflow,v=>v.workflow.mode='reviewed',v=>v.roles.executor.model='strong',v=>v.text='Another task']){
  const changed=structuredClone(response);alter(changed.run.request);
  assert.equal(matchesAutomaticReceipt(changed,input,request),false);
  if(changed.run.request.workflow)assert.notEqual(await automaticRequestDigest(changed.run.request),digest);
 }
 const legacy={...input,decision:{version:1,provider:'builtin',roles:{coordinator:{mode:'auto'},executor:{mode:'fixed'}}}};delete legacy.workflow;
 assert.equal(await automaticRequestDigest(legacy),await automaticRequestDigest({...legacy,workflow:undefined}),'old pending digests remain byte compatible');
});

test('workflow pending marker survives reload and unknown workflow drafts fail closed',()=>{
 const values=new Map(),storage={get length(){return values.size},key:i=>[...values.keys()][i],getItem:k=>values.get(k)??null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
 const id='b'.repeat(32),path=`/collaboration/runs/${id}`,input={workflow:{version:1,mode:'direct'}};
 const p={id,session:'space',thread:'thread',path,query:path,input,created:Date.now(),kind:'create',text:'Task',workflow_version:1,request_fingerprint:'f'.repeat(64)};
 new CollaborationPending(storage,'scope').save(p);
 assert.deepEqual(new CollaborationPending(storage,'scope').list(),[p]);
 values.clear();const bad={...p};delete bad.workflow_version;new CollaborationPending(storage,'scope').save(bad);
 assert.equal(new CollaborationPending(storage,'scope').list().length,0,'unstamped strategy cannot release or replay a request');
 const binding={agent_type:'codex',account_id:'a',model:'mini',effort:'',effort_control:''},draft={mode:'collaboration',repository:'.',include_context:true,configuration:{roles:{coordinator:binding,executor:binding}}};
 assert.ok(cleanCollaboration(draft),'legacy reviewed preference survives');
 assert.equal(cleanCollaboration({...draft,configuration:{...draft.configuration,workflow:{version:2,mode:'direct'}}}),undefined);
});
