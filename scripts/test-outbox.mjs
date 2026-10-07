import test from 'node:test';
import assert from 'node:assert/strict';
import {OutboxStore,draftFromRequest} from '../internal/web/static/js/features/chat/outbox-store.js';
import {DRAFT_TTL} from '../internal/web/static/js/features/chat/draft-store.js';
import {rememberChatIdentity,clearPreviousChatIdentity} from '../internal/web/static/js/features/chat/local-identity.js';

class Storage {
 data=new Map();fail=false;
 get length(){return this.data.size;}
 key(n){return [...this.data.keys()][n]??null;}
 getItem(k){return this.data.get(k)??null;}
 setItem(k,v){if(this.fail)throw Error('quota');this.data.set(k,v);}
 removeItem(k){this.data.delete(k);}
}
const scope='a'.repeat(64),draftScope='d'.repeat(64),id='1'.repeat(32);
const outgoing=(text='Frozen prompt',request=id)=>({version:1,id:request,session:'space',thread:'thread',created:1000,input:{scope,thread_id:'thread',text,model:'model-one',effort:'high',effort_control:'effort',attachments:[]},draft:{text,attachments:[]},revision:0,state:'unconfirmed'});
const receipt=(row,state,revision,thread=row.thread)=>({request_id:row.id,session_id:row.session,thread_id:thread,state,revision});

test('reload preserves frozen envelope and separates instance/user scopes',()=>{
 const disk=new Storage(),a=new OutboxStore(disk,scope,draftScope,()=>1001),row=outgoing();
 assert.equal(a.add(row),true);
 row.input.model='edited';row.draft.text='edited';
 const loaded=new OutboxStore(disk,scope,draftScope,()=>1002).rows('space')[0];
 assert.equal(loaded.input.model,'model-one');assert.equal(loaded.draft.text,'Frozen prompt');
 assert.equal(new OutboxStore(disk,'b'.repeat(64),draftScope,()=>1002).rows().length,0);
 assert.equal(a.rows('another-space').length,0);
 assert.equal(a.add({...outgoing(),session:'another-space'}),false,'an ID collision overwrote another workspace');
 a.remove(id,'another-space');assert.equal(a.rows('space').length,1,'another workspace acknowledged this copy');
 assert.equal(a.add({...outgoing(),input:{...outgoing().input,effort:'low'}}),false,'same ID accepted changed settings');
});

test('cross-tab receipt updates cannot regress revisions or acknowledge another ID',()=>{
 const disk=new Storage(),a=new OutboxStore(disk,scope,draftScope,()=>1001),b=new OutboxStore(disk,scope,draftScope,()=>1001),row=outgoing();
 a.add(row);b.rows();a.observe(receipt(row,'running',3));
 b.observe(receipt(row,'accepted',1));assert.equal(a.rows()[0].state,'running');
 b.add(row);assert.equal(a.rows()[0].revision,3,'stale prepared copy overwrote a newer receipt');
 b.observe(receipt({...row,id:'2'.repeat(32)},'completed',4));assert.equal(a.rows()[0].state,'running');
 b.observe(receipt(row,'completed',4,'other-thread'));assert.equal(a.rows()[0].state,'running');
 a.observe(receipt(row,'completed',4));b.remove(id);
 assert.equal(new OutboxStore(disk,scope,draftScope,()=>1002).rows().length,0);
});

test('storage refusal retains pending text and never reports successful persistence',()=>{
 const disk=new Storage(),a=new OutboxStore(disk,scope,draftScope,()=>1001),row=outgoing();disk.fail=true;
 assert.equal(a.add(row),false);assert.equal(a.error,true);assert.equal(a.rows()[0].draft.text,row.draft.text);
 assert.equal(a.add(row),false);
 assert.equal(new OutboxStore(disk,scope,draftScope,()=>1001).rows().length,0);
 disk.fail=false;assert.equal(a.add(row),true);assert.equal(a.error,false);
 assert.equal(a.add(outgoing('x'.repeat(410000),'2'.repeat(32))),false);
 a.observe(receipt(row,'accepted',1));assert.equal(a.error,true,'saving another record hid a still-unsaved message');
 assert.equal(new OutboxStore(disk,scope,draftScope,()=>1001).rows().length,1,'capacity failure overwrote a saved message');
});

test('expiry removes private payload but preserves the ID needed to fence a late request',()=>{
 const disk=new Storage(),a=new OutboxStore(disk,scope,draftScope,()=>1001);a.add(outgoing());
 const later=new OutboxStore(disk,scope,draftScope,()=>1000+DRAFT_TTL),row=later.rows()[0];
 assert.equal(row.input,null);assert.equal(row.draft,null);assert.equal(row.id,id);assert.equal(row.expired,true);
 assert.ok(![...disk.data.values()].join('').includes('Frozen prompt'));
 later.observe(receipt(row,'abandoned',1,''));assert.equal(later.rows()[0].state,'abandoned');
});

test('saving preference and logout clear persistent copies without a later tab resurrecting them',()=>{
 const disk=new Storage(),a=new OutboxStore(disk,scope,draftScope,()=>1001),b=new OutboxStore(disk,scope,draftScope,()=>1001);
 a.add(outgoing());b.rows();disk.setItem(`agentbox.chat-draft.v1.${draftScope}.enabled`,'off');a.clearPersistent();
 b.observe(receipt(outgoing(),'accepted',1));assert.equal(a.rows()[0].draft.text,'Frozen prompt');
 assert.equal(new OutboxStore(disk,scope,draftScope,()=>1001).rows().length,0);
 assert.equal(b.add(outgoing('Page only','2'.repeat(32))),true);
 assert.ok(![...disk.data.values()].join('').includes('Page only'));
 a.clear();b.clear();assert.equal(a.rows().length,0);assert.equal(b.rows().length,0);
});

test('missing storage fails closed until an explicit page-only choice',()=>{
 const a=new OutboxStore(undefined,scope,draftScope,()=>1001);
 assert.equal(a.add(outgoing()),false);assert.equal(a.error,true);
 a.setEnabled(false);assert.equal(a.add(outgoing()),true);assert.equal(a.rows().length,1);
});

test('a payload accepted under the record limit remains readable after reload',()=>{
 const disk=new Storage(),store=new OutboxStore(disk,scope,draftScope,()=>1001),row=outgoing('x'.repeat(160_000));
 assert.equal(store.add(row),true);
 const restored=new OutboxStore(disk,scope,draftScope,()=>1002).rows()[0];
 assert.equal(restored.input.text,row.input.text);assert.equal(restored.draft.text,row.draft.text);
});

test('server-only recovery restores attachment references as unverified, without overwriting literal placeholders',()=>{
 const input={...outgoing().input,text:'Keep [File #2]; inspect [图片#3 /shared/.images/a.png] and [附件#2 /shared/.file/b.txt]; also /shared/.file/b.txt-extra.',attachments:['/shared/.images/a.png','/shared/.file/b.txt']};
 const restored=draftFromRequest(input);
 assert.equal(restored.attachments.length,2);assert.equal(restored.attachments[0].id,3);assert.notEqual(restored.attachments[1].id,2);
 assert.ok(restored.attachments.every(a=>a.valid===false));assert.ok(restored.text.includes('Keep [File #2]'));assert.ok(restored.text.includes('[Image #3]'));assert.ok(restored.text.includes('/shared/.file/b.txt-extra.'));
 assert.equal(restored.attachments[1].path,'/shared/.file/b.txt');
 const plain=draftFromRequest({...input,text:'Read /shared/.file/b.txt',attachments:['/shared/.file/b.txt']});
 assert.equal(plain.text,'Read [File #1]');assert.equal(plain.attachments[0].valid,false);
 assert.equal(draftFromRequest({...input,text:'Read /shared/.file/b.txt.',attachments:['/shared/.file/b.txt']}).text,'Read [File #1].');
});

test('login replacement clears the previous actor once, without touching tokens, preferences or the new login copies',()=>{
 const disk=new Storage(),hints=new Storage(),old=new OutboxStore(disk,scope,draftScope,()=>1001);old.add(outgoing());
 disk.setItem('agentbox_token','new-login-token');disk.setItem(`agentbox.chat-draft.v1.${draftScope}.enabled`,'on');
 disk.setItem('agentbox.chat-outbox.v1.'+'b'.repeat(64)+'.other','another actor');hints.setItem(`agentbox.chat-hint.v1.${draftScope}.space/thread`,'writer');
 rememberChatIdentity(disk,draftScope,scope);clearPreviousChatIdentity(disk,hints);
 assert.equal(disk.getItem('agentbox_token'),'new-login-token');assert.equal(disk.getItem(`agentbox.chat-draft.v1.${draftScope}.enabled`),'on');assert.equal(hints.length,0);
 assert.ok([...disk.data.values()].includes('another actor'));assert.ok(![...disk.data.values()].join('').includes('Frozen prompt'));
 const fresh=new OutboxStore(disk,scope,draftScope,()=>1002);fresh.add(outgoing('New login message','2'.repeat(32)));rememberChatIdentity(disk,draftScope,scope);
 old.forget();assert.equal(new OutboxStore(disk,scope,draftScope,()=>1003).rows()[0].draft.text,'New login message');
});
