import test from 'node:test';
import assert from 'node:assert/strict';
import {ChatSender} from '../internal/web/static/js/features/chat/sender.js';
import {SettingsRequests} from '../internal/web/static/js/features/settings/requests.js';
const defer=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b;});return {promise,resolve,reject};};
const snapshot=()=>({owner:'login-a/workspace/thread',content:'first',input:{text:'first'},draft:{text:'first',attachments:[]}});
function senderHarness(){let value=snapshot();const pending=defer(),sent=[];let edits=0,ready=true;
 const sender=new ChatSender({read:()=>value,allowed:()=>true,durable:()=>false,connected:()=>ready,connect(){},validate:()=>pending.promise,deliver:async(c,current)=>{if(current())sent.push(c.input.text);},changed(){},edited(){edits++;},waking(){},awake(){},timeout(){}});
 return {sender,pending,sent,set:(v)=>{value=v;},get edits(){return edits;},ready:(v)=>{ready=v;}};
}
test('double click submits once; edits while validation waits require another explicit send',async()=>{
 const h=senderHarness();const first=h.sender.send(),second=h.sender.send();h.set({...snapshot(),content:'edited',input:{text:'edited'}});h.pending.resolve(true);
 await Promise.all([first,second]);assert.deepEqual(h.sent,[]);assert.equal(h.edits,1);assert.equal(h.sender.busy,false);
});
test('cancelled validation cannot deliver or clear a later send',async()=>{
 const h=senderHarness();const first=h.sender.send();h.sender.cancel();h.set({...snapshot(),owner:'new-owner'});h.pending.resolve(true);await first;assert.deepEqual(h.sent,[]);
 await h.sender.send();assert.deepEqual(h.sent,['first']);
});
test('legacy wake preserves the clicked input and cancels its timer on leaving',async t=>{
 t.mock.timers.enable({apis:['setTimeout']});const h=senderHarness();h.ready(false);
 const first=h.sender.send();h.set({...snapshot(),content:'edited'});h.ready(true);h.pending.resolve(true);t.mock.timers.tick(400);await first;
 assert.deepEqual(h.sent,[]);assert.equal(h.edits,1);
 h.ready(false);const next=h.sender.send();h.sender.cancel();await next;assert.deepEqual(h.sent,[]);
});
test('settings writes serialize and a stale read never overwrites a newer save',async()=>{
 const calls=[];let owner=true;
 const scope=new SettingsRequests((path,options)=>{const pending=defer();calls.push({...pending,path,options});return pending.promise;},()=>owner);
 const read=scope.read();const one=scope.save('one'),two=scope.save('two');await Promise.resolve();await Promise.resolve();
 assert.equal(calls.length,2);assert.equal(calls[0].options.signal.aborted,true);
 calls[0].resolve({version:'old'});assert.equal(await read,undefined);
 calls[1].resolve({version:'one'});assert.deepEqual(await one,{version:'one'});await Promise.resolve();await Promise.resolve();
 assert.equal(calls.length,3);assert.equal(calls[2].options.body,'two');
 owner=false;scope.dispose();calls[2].resolve({private:'other user'});assert.equal(await two,undefined);
});
test('logout aborts reads and prevents queued writes and post-confirmation actions',async()=>{
 const calls=[];const scope=new SettingsRequests((_path,options)=>{const p=defer();calls.push({...p,options});return p.promise;},()=>true);
 const first=scope.save('first'),queued=scope.save('queued');await Promise.resolve();await Promise.resolve();
 let cleaned=0;scope.cleanup(()=>cleaned++);scope.dispose();assert.equal(cleaned,1);assert.equal(calls[0].options.signal.aborted,true);
 calls[0].resolve({});assert.equal(await first,undefined);assert.equal(await queued,undefined);
 await scope.request('/cache/marketplace',{method:'DELETE'});assert.equal(calls.length,1);
});
