import test from 'node:test';
import assert from 'node:assert/strict';
import { ChatHistory } from '../internal/web/static/js/features/chat/history.js';

function deferred() { let resolve, reject; const promise = new Promise((a,b) => {resolve=a;reject=b;}); return {promise,resolve,reject}; }
const value = {entries:[], thread:null};

test('workspace changes and logout discard delayed responses and cancel transport', async () => {
  let current = 'a'; const requests=[]; const applied=[];
  const history = new ChatHistory((path,signal)=>{const reply=deferred(); requests.push({path,signal,...reply}); return reply.promise;},()=>current);
  const first=history.load('a','/a',async()=>{}, v=>applied.push(v));
  current='b'; const second=history.load('b','/b',async()=>{},v=>applied.push(v));
  assert.equal(requests[0].signal.aborted,true);
  requests[0].resolve(value); assert.equal((await first).state,'stale'); assert.equal(applied.length,0);
  history.cancel(); requests[1].resolve(value); assert.equal((await second).state,'stale'); assert.equal(applied.length,0);
});

test('superseded draft preparation cannot render into another thread', async () => {
  const prepare=deferred(), entered=deferred(), rendered=[];
  const history=new ChatHistory(async()=>value,()=> 'a');
  const first=history.load('a','/a',async()=>{entered.resolve(); await prepare.promise;},()=>rendered.push('old'));
  await entered.promise;
  const second=history.load('a','/a',async()=>{},()=>rendered.push('new'));
  const result=await second; assert.equal(result.state,'loaded');
  prepare.resolve(); assert.equal((await first).state,'stale'); assert.deepEqual(rendered,['new']);
  history.cancel(); assert.equal(result.current(),false,'even settled callbacks must respect logout');
});

test('deadlines abort the request and late transport success does not render', async t => {
  t.mock.timers.enable({apis:['setTimeout']});
  const pending=deferred(); let signal, rendered=false;
  const history=new ChatHistory(async(_path,s)=>{signal=s;return pending.promise;},()=> 'a',10);
  const load=history.load('a','/a',async()=>{},()=>{rendered=true;});
  t.mock.timers.tick(10); assert.equal(signal.aborted,true);
  pending.resolve(value); const result=await load;
  assert.equal(result.state,'failed'); assert.equal(result.timedOut,true); assert.equal(rendered,false);
});

test('polling replacement of the session object keeps history loading valid', async () => {
  let session={id:'a'}; const pending=deferred(); let applied=false;
  const history=new ChatHistory(()=>pending.promise,()=>session?.id);
  const load=history.load('a','/a',async()=>{},()=>{applied=true;});
  session={id:'a'}; pending.resolve(value);
  assert.equal((await load).state,'loaded'); assert.equal(applied,true);
});
