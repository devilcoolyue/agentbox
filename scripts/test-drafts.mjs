import test from 'node:test';
import assert from 'node:assert/strict';
import {DraftStore,DRAFT_TTL} from '../internal/web/static/js/features/chat/draft-store.js';

class Storage {
 data=new Map();fail=false;
 get length(){return this.data.size;}
 key(i){return [...this.data.keys()][i]??null;}
 getItem(k){return this.data.get(k)??null;}
 setItem(k,v){if(this.fail)throw Error('quota');this.data.set(k,v);}
 removeItem(k){this.data.delete(k);}
}
const scope='a'.repeat(64),draft=text=>({text,attachments:[]});

test('reload keeps exact text and separates actor, workspace and thread',()=>{
 const disk=new Storage(),hints=new Storage();
 let store=new DraftStore(disk,hints,scope,'page-one');
 for(const [space,thread,text] of [['one','t1','first\n保存 <script>'],['one','t2','second'],['two','t1','third']])assert.equal(store.save(space,thread,draft(text)),true);
 store=new DraftStore(disk,hints,scope,'page-two');
 assert.equal(store.load('one','t1').text,'first\n保存 <script>');
 assert.equal(store.load('one','t2').text,'second');assert.equal(store.load('two','t1').text,'third');
 assert.equal(new DraftStore(disk,hints,'b'.repeat(64),'page').load('one','t1').text,'');
});

test('independent pages and copied tab hints do not overwrite each other',()=>{
 const disk=new Storage(),aHints=new Storage(),bHints=new Storage();let time=1000;
 const a=new DraftStore(disk,aHints,scope,'a',()=>time);
 a.save('space','thread',draft('A'));
 bHints.data=new Map(aHints.data);
 const b=new DraftStore(disk,bHints,scope,'b',()=>time);
 assert.equal(b.load('space','thread').text,'A');
 time++;b.save('space','thread',draft('B'));time++;a.save('space','thread',draft('A edited'));
 assert.equal(new DraftStore(disk,bHints,scope,'b-reload',()=>time).load('space','thread').text,'B');
 assert.equal(new DraftStore(disk,aHints,scope,'a-reload',()=>time).load('space','thread').text,'A edited');
 b.save('space','thread',draft(''));
 assert.equal(new DraftStore(disk,bHints,scope,'b-again',()=>time).load('space','thread').text,'','clear rediscovered another page draft');
});

test('expiry removes stale records and disabled saving cannot be resurrected by another page',()=>{
 const disk=new Storage(),hints=new Storage();let time=1000;
 const a=new DraftStore(disk,hints,scope,'a',()=>time),b=new DraftStore(disk,new Storage(),scope,'b',()=>time);
 a.save('s','t',draft('expired'));time+=DRAFT_TTL;
 assert.equal(b.load('s','t').text,'');assert.equal(disk.length,0);
 a.save('s','t',draft('new'));a.setEnabled(false);
 assert.equal(b.save('s','t',draft('other page')),false);
 assert.equal([...disk.data.values()].some(v=>v.includes('other page')||v.includes('new')),false);
 assert.equal(b.load('s','t').text,'other page','disabled mode must keep page-local editing');
 assert.equal(new DraftStore(disk,new Storage(),scope,'next').load('s','t').text,'');
 a.clear();assert.equal(a.load('s','t').text,'');
});

test('failed writes retain editing without falsely claiming the next retry was saved',()=>{
 const disk=new Storage(),hints=new Storage(),a=new DraftStore(disk,hints,scope,'a');
 a.save('s','t',draft('original'));disk.fail=true;
 assert.equal(a.save('s','t',draft('edited')),false);assert.equal(a.error,true);
 assert.equal(a.save('s','t',draft('edited')),false,'repeated failure was mistaken for a saved source');
 assert.equal(a.load('s','t').text,'edited');
 assert.equal(new DraftStore(disk,hints,scope,'b').load('s','t').text,'original');
 disk.fail=false;assert.equal(a.save('s','t',draft('edited')),true);
 assert.equal(new DraftStore(disk,hints,scope,'c').load('s','t').text,'edited');
 assert.equal(a.save('s','t',draft('x'.repeat(160000))),false);
 assert.equal(new DraftStore(disk,hints,scope,'d').load('s','t').text,'edited');
});

test('restored attachment references are bounded and malformed records are not used',()=>{
 const disk=new Storage(),hints=new Storage(),a=new DraftStore(disk,hints,scope,'a');
 a.save('s','t',{text:'[File #1]',attachments:[{id:1,path:'/shared/.file/readme.txt',name:'readme.txt',orig:'readme.txt',kind:'file',valid:true}]});
 const recovered=new DraftStore(disk,hints,scope,'b').load('s','t');
 assert.equal(recovered.attachments[0].path,'/shared/.file/readme.txt');assert.equal(recovered.attachments[0].valid,undefined,'validity was persisted as proof');
 const [key,value]=[...disk.data][0];const record=JSON.parse(value);record.attachments[0].path='/shared/.file/../../private';disk.setItem(key,JSON.stringify(record));
 assert.equal(new DraftStore(disk,hints,scope,'c').load('s','t').text,'');
});
