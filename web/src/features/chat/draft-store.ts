export interface DraftAttachment {id:number;path:string;name:string;orig:string;kind:"img"|"file";valid?:boolean;}
export interface Draft {text:string;attachments:DraftAttachment[];}
interface Record extends Draft {version:1;session:string;thread:string;writer:string;updated:number;}
export interface DraftStorage {readonly length:number;key(index:number):string|null;getItem(key:string):string|null;setItem(key:string,value:string):void;removeItem(key:string):void;}
export const DRAFT_TTL=7*24*60*60*1000;
const MAX_RECORD=150_000,MAX_TOTAL=1_000_000,MAX_RECORDS=500;
const empty=():Draft=>({text:"",attachments:[]});
const clean=(draft:Draft):Draft=>({text:draft.text,attachments:draft.attachments.map(({id,path,name,orig,kind})=>({id,path,name,orig,kind}))});

/** One record per page writer prevents one tab from overwriting another.
 * Per-tab hints contain only writer IDs and survive reload/duplicated tabs. */
export class DraftStore {
 private prefix:string;
 private hintsPrefix:string;
 private memory=new Map<string,{draft:Draft;source:string;hash:string}>();
 private localEnabled=true;
 error=false;
 constructor(private storage:DraftStorage|undefined,private hints:DraftStorage|undefined,scope:string,private writer:string,private now=()=>Date.now()){
  this.prefix=`agentbox.chat-draft.v1.${scope}.`;this.hintsPrefix=`agentbox.chat-hint.v1.${scope}.`;
  if(!/^[a-f0-9]{64}$/.test(scope))this.storage=undefined;
 }
 private context(session:string,thread:string){return encodeURIComponent(session)+"/"+encodeURIComponent(thread);}
 get enabled(){try{return this.localEnabled&&this.storage?.getItem(this.prefix+"enabled")!=="off";}catch{this.error=true;return false;}}
 get supported(){return !!this.storage;}
 private records(){
  const rows:{key:string;raw:string;record:Record}[]=[];
  if(!this.storage)return rows;
  for(let i=this.storage.length-1;i>=0;i--){
   const key=this.storage.key(i);if(!key?.startsWith(this.prefix)||key===this.prefix+"enabled")continue;
   const raw=this.storage.getItem(key);if(!raw||raw.length>MAX_RECORD)continue;
   let value:Record;try{value=JSON.parse(raw);}catch{continue;}
   if(value.version!==1||typeof value.text!=="string"||typeof value.session!=="string"||typeof value.thread!=="string"||typeof value.writer!=="string"||!Number.isFinite(value.updated)||!Array.isArray(value.attachments)||value.attachments.length>64)continue;
   if(value.updated>this.now()+60_000||this.now()-value.updated>=DRAFT_TTL){this.storage.removeItem(key);continue;}
   if(value.attachments.some(a=>!a||!Number.isInteger(a.id)||a.id<1||a.id>1_000_000||!["img","file"].includes(a.kind)||[a.path,a.name,a.orig].some(v=>typeof v!=="string"||v.length>1024)||a.path!==""&&!/^\/shared\/\.(images|file)\/[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/.test(a.path)))continue;
   if(new Set(value.attachments.map(a=>a.id)).size!==value.attachments.length)continue;
   rows.push({key,raw,record:value});
  }
  return rows;
 }
 load(session:string,thread:string):Draft{
  const context=this.context(session,thread),cached=this.memory.get(context);
  if(cached)return structuredClone(cached.draft);
  let draft=empty(),source="";
  if(this.enabled&&this.storage)try{
   const hint=this.hints?.getItem(this.hintsPrefix+context);
   const rows=this.records().filter(r=>r.record.session===session&&r.record.thread===thread).sort((a,b)=>b.record.updated-a.record.updated);
   const item=rows.find(r=>r.record.writer===hint)||rows[0];
   if(item){draft=clean(item.record);source=item.key;}
  }catch{this.error=true;}
  this.memory.set(context,{draft:structuredClone(draft),source,hash:JSON.stringify(draft)});
  return draft;
 }
 save(session:string,thread:string,value:Draft):boolean{
  const draft=clean(value),hash=JSON.stringify(draft),context=this.context(session,thread),prior=this.memory.get(context);
  this.memory.set(context,{draft,source:prior?.hash===hash?prior.source:"",hash});
  if(!this.enabled||!this.storage)return false;
  try{
   const rows=this.records();
   if(prior?.hash===hash&&prior.source&&rows.some(r=>r.key===prior.source&&JSON.stringify(clean(r.record))===hash))return true;
   // Empty records are intentional tombstones: a cleared composer must not
   // rediscover another tab's older draft on the next reload.
   const key=this.prefix+context+"."+this.writer;
   const raw=JSON.stringify({version:1,session,thread,writer:this.writer,updated:this.now(),...draft});
   const others=rows.filter(r=>r.key!==key);
   if(raw.length>MAX_RECORD||others.length>=MAX_RECORDS||others.reduce((n,r)=>n+r.raw.length,raw.length)>MAX_TOTAL)throw new Error("draft capacity exceeded");
   this.storage.setItem(key,raw);
   this.hints?.setItem(this.hintsPrefix+context,this.writer);
   this.memory.set(context,{draft,source:key,hash});this.error=false;return true;
  }catch{this.error=true;return false;}
 }
 setEnabled(enabled:boolean){
  this.localEnabled=enabled;
  try{
   if(!enabled)this.clearStored();
   this.storage?.setItem(this.prefix+"enabled",enabled?"on":"off");this.error=false;
  }catch{this.error=true;}
 }
 private clearStored(){
  for(const [storage,prefix] of [[this.storage,this.prefix],[this.hints,this.hintsPrefix]] as const){
   if(!storage)continue;
   for(let i=storage.length-1;i>=0;i--){const key=storage.key(i);if(key?.startsWith(prefix)&&key!==this.prefix+"enabled")storage.removeItem(key);}
  }
 }
 clear(){try{this.clearStored();}catch{this.error=true;}this.memory.clear();}
 forget(){
  this.memory.clear();
  try{if(this.hints)for(let i=this.hints.length-1;i>=0;i--){const key=this.hints.key(i);if(key?.startsWith(this.hintsPrefix))this.hints.removeItem(key);}}catch{this.error=true;}
 }
}
