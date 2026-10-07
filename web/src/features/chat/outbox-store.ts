import {chatStates} from "../../contracts/chat.js";
import type {ChatRequestInput,ChatRequestReceipt} from "../../types.js";
import type {Draft,DraftStorage} from "./draft-store.js";
import {DRAFT_TTL} from "./draft-store.js";

export interface Outgoing {
 version:1; id:string; session:string; thread:string; created:number;
 input:ChatRequestInput|null; draft:Draft|null;
 revision:number; state:ChatRequestReceipt["state"]|"unconfirmed";
 expired?:boolean;
}
const scopePattern=/^[a-f0-9]{64}$/,idPattern=/^[a-f0-9]{32}$/;
const states=new Set<string>(["unconfirmed",...chatStates]);
const MAX_RECORD=400_000,MAX_TOTAL=2_000_000,MAX_RECORDS=500;
export const receiptActive=(state:string)=>["accepted","starting","running","uncertain"].includes(state);

/** Server-only recovery (local saving off/expired) must recreate attachment
 * metadata too; plain text paths alone would bypass composer revalidation. */
export function draftFromRequest(input:ChatRequestInput):Draft {
 let text=input.text;
 const markers=[...text.matchAll(/\[(?:图片|附件)#(\d+) (\/shared\/\.(?:images|file)\/[A-Za-z0-9._-]+)\]/g)];
 const used=new Set([...text.matchAll(/\[(?:Image|File) #(\d+)\]/g)].map(m=>Number(m[1])));
 const attachments:Draft["attachments"]=[],tokens=new Map<string,string>();let next=1;
 for(const path of input.attachments||[]){
  if(tokens.has(path)||!/^\/shared\/\.(images|file)\/[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/.test(path))continue;
  const preferred=Number(markers.find(m=>m[2]===path)?.[1]);
  while(used.has(next))++next;
  const id=Number.isInteger(preferred)&&preferred>0&&preferred<=1_000_000&&!used.has(preferred)?preferred:next++;
  used.add(id);const kind=path.startsWith("/shared/.images/")?"img":"file",name=path.split("/").at(-1)!;
  attachments.push({id,path,name,orig:name,kind,valid:false});tokens.set(path,`[${kind==="img"?"Image":"File"} #${id}]`);
 }
 text=text.replace(/\[(?:图片|附件)#\d+ (\/shared\/\.(?:images|file)\/[A-Za-z0-9._-]+)\]/g,(whole,path:string)=>tokens.get(path)||whole);
 text=text.replace(/\/shared\/\.(?:images|file)\/[A-Za-z0-9][A-Za-z0-9._-]{0,255}(?![A-Za-z0-9._/-])/g,path=>{
  const exact=tokens.get(path);if(exact)return exact;
  const leaf=path.replace(/\.+$/,"");return tokens.has(leaf)?tokens.get(leaf)!+path.slice(leaf.length):path;
 });
 return {text,attachments};
}

/** Immutable envelopes, keyed by ID across tabs. Payload expiry preserves the
 * ID fence: an expired unknown request can still be queried or abandoned. */
export class OutboxStore {
 private prefix:string;
 private preference:string;
 private memory=new Map<string,Outgoing>();
 private removed=new Set<string>();
 private localEnabled=true;
 private unsaved=new Set<string>();
 error=false;
 constructor(private disk:DraftStorage|undefined,readonly scope:string,draftScope:string,private now=()=>Date.now()){
  this.prefix=`agentbox.chat-outbox.v1.${scope}.`;
  this.preference=`agentbox.chat-draft.v1.${draftScope}.enabled`;
  if(!scopePattern.test(scope)||!scopePattern.test(draftScope))this.disk=undefined;
 }
 get enabled(){if(!this.localEnabled)return false;try{return !this.disk||this.disk.getItem(this.preference)!=="off";}catch{this.error=true;return true;}}
 setEnabled(enabled:boolean){
  this.localEnabled=enabled;
  if(!enabled){this.unsaved.clear();this.error=false;this.clearPersistent();return;}
  let saved=true;for(const row of this.memory.values())if(!this.persist(row))saved=false;
  if(!saved)this.error=true;
 }
 private expire(row:Outgoing){
  if(this.now()-row.created>=DRAFT_TTL){row.input=null;row.draft=null;row.expired=true;}
  return row;
 }
 private parse(raw:string):Outgoing|undefined{
  if(raw.length>MAX_RECORD)return;
  let r:Outgoing;try{r=JSON.parse(raw);}catch{return;}
  if(!r||r.version!==1||!idPattern.test(r.id)||typeof r.session!=="string"||!r.session||r.session.length>128||typeof r.thread!=="string"||!r.thread||r.thread.length>128||!Number.isFinite(r.created)||r.created>this.now()+60_000||!Number.isSafeInteger(r.revision)||r.revision<0||!states.has(r.state))return;
  if(r.input!==null){
   const i=r.input;
   if(!i||i.scope!==this.scope||i.thread_id!==r.thread||[i.text,i.model,i.effort,i.effort_control].some(v=>typeof v!=="string")||i.text.length>1_048_576||i.attachments!==undefined&&(!Array.isArray(i.attachments)||i.attachments.length>64||i.attachments.some(p=>typeof p!=="string"||!/^\/shared\/\.(images|file)\/[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/.test(p))))return;
  }
  if(r.draft!==null){
   if(!r.draft||typeof r.draft.text!=="string"||r.draft.text.length>1_048_576||!Array.isArray(r.draft.attachments)||r.draft.attachments.length>64)return;
   if(r.draft.attachments.some(a=>!a||!Number.isInteger(a.id)||a.id<1||a.id>1_000_000||!["img","file"].includes(a.kind)||[a.path,a.name,a.orig].some(v=>typeof v!=="string"||v.length>1024)||!/^\/shared\/\.(images|file)\/[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/.test(a.path)))return;
  }
  return this.expire(r);
 }
 rows(session?:string):Outgoing[]{
  if(this.enabled&&this.disk)try{
   for(let n=this.disk.length-1;n>=0;n--){
    const key=this.disk.key(n);if(!key?.startsWith(this.prefix))continue;
    const raw=this.disk.getItem(key);if(!raw)continue;
    const row=this.parse(raw);if(!row||key!==this.prefix+row.id)continue;
    const cached=this.memory.get(row.id);
    if(!this.removed.has(row.id)&&(!cached||row.revision>=cached.revision))this.memory.set(row.id,row);
    if(row.expired&&raw!==JSON.stringify(row))this.persist(row);
   }
  }catch{this.error=true;}
  return [...this.memory.values()].map(row=>this.expire(row)).filter(r=>!session||r.session===session).map(r=>structuredClone(r));
 }
 add(row:Outgoing):boolean{
  if(this.removed.has(row.id))return false;
  const prior=this.rows().find(r=>r.id===row.id);
  if(prior&&(prior.session!==row.session||prior.thread!==row.thread||JSON.stringify(prior.input)!==JSON.stringify(row.input))){this.error=true;return false;}
  if(prior&&prior.revision>row.revision)row={...row,revision:prior.revision,state:prior.state};
  this.memory.set(row.id,structuredClone(row));
  return this.persist(row);
 }
 private persist(row:Outgoing){
  if(!this.enabled){this.unsaved.clear();this.error=false;return true;}
  if(!this.disk){this.unsaved.add(row.id);this.error=true;return false;}
  try{
   const raw=JSON.stringify(this.expire(row));let count=0,total=raw.length;
   for(let n=0;n<this.disk.length;n++){
    const key=this.disk.key(n);if(!key?.startsWith(this.prefix)||key===this.prefix+row.id)continue;
    ++count;total+=(this.disk.getItem(key)||"").length;
   }
   if(raw.length>MAX_RECORD||total>MAX_TOTAL||count>=MAX_RECORDS)throw Error("outbox capacity exceeded");
   this.disk.setItem(this.prefix+row.id,raw);this.unsaved.delete(row.id);this.error=this.unsaved.size>0;return true;
  }catch{this.unsaved.add(row.id);this.error=true;return false;}
 }
 observe(receipt:ChatRequestReceipt){
  const row=this.rows().find(r=>r.id===receipt.request_id&&r.session===receipt.session_id&&(r.thread===receipt.thread_id||!receipt.thread_id&&["abandoned","deleted"].includes(receipt.state)));
  if(!row||receipt.revision<row.revision)return;
  row.revision=receipt.revision;row.state=receipt.state;
  this.memory.set(row.id,row);this.persist(row);
 }
 remove(id:string,session?:string){
  const row=this.rows().find(r=>r.id===id);
  if(session&&row&&row.session!==session)return;
  this.removed.add(id);this.memory.delete(id);
  this.unsaved.delete(id);this.error=this.unsaved.size>0;
  try{this.disk?.removeItem(this.prefix+id);}catch{this.error=true;}
 }
 clearPersistent(){
  try{if(this.disk)for(let n=this.disk.length-1;n>=0;n--){const key=this.disk.key(n);if(key?.startsWith(this.prefix))this.disk.removeItem(key);}}catch{this.error=true;}
 }
 clear(){this.clearPersistent();this.memory.clear();this.removed.clear();this.unsaved.clear();}
 forget(){this.memory.clear();this.removed.clear();this.unsaved.clear();}
}
