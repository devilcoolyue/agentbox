import type {DraftStorage,Draft} from "../chat/draft-store.js";
export interface Pending {id:string;session:string;thread:string;path:string;query:string;input:unknown;created:number;kind:"create"|"action"|"application";text:string;draft?:Draft;decision_version?:1;workflow_version?:1;request_fingerprint?:string}
/** Separate from ordinary chat receipts: a query can never invoke execution. */
export class CollaborationPending {
 private prefix:string;
 private memory=new Map<string,Pending>();
 enabled=true;
 constructor(private storage:DraftStorage,scope:string){this.prefix=`agentbox.chat-draft.v1.${scope}.collaboration.pending.`;}
 save(p:Pending){if(this.enabled){const raw=JSON.stringify(p);if(raw.length>600000)throw new Error("协作输入过长，无法保存待确认副本");this.storage.setItem(this.prefix+p.id,raw);}this.memory.set(p.id,p);}
 list():Pending[]{
  if(this.enabled)for(let i=this.storage.length-1;i>=0;i--){const key=this.storage.key(i);if(!key?.startsWith(this.prefix))continue;
   try{const p=JSON.parse(this.storage.getItem(key)||"null") as Pending;if(!p||! /^[a-f0-9]{32}$/.test(p.id)||typeof p.session!=="string"||typeof p.thread!=="string"||typeof p.text!=="string"||!Number.isFinite(p.created)||p.created>Date.now()+60000||!["create","action","application"].includes(p.kind))continue;
    const create=`/collaboration/runs/${p.id}`,action=/^\/collaboration\/runs\/[a-f0-9]{32}\/actions\/[a-f0-9]{32}$/;
    if(p.kind==="create"?p.path!==create||p.query!==create:p.kind==="application"?!/^\/collaboration\/runs\/[a-f0-9]{32}\/application\/[a-f0-9]{32}$/.test(p.path)||p.query!==p.path.slice(0,-33)+"?action="+p.id||!p.path.endsWith('/'+p.id):!action.test(p.path)||p.query!==p.path||!p.path.endsWith('/'+p.id))continue;
    if((p.decision_version!==undefined||p.workflow_version!==undefined)&&((p.decision_version!==undefined&&p.decision_version!==1)||(p.workflow_version!==undefined&&p.workflow_version!==1)||p.kind!=="create"||typeof p.request_fingerprint!=="string"||!/^[a-f0-9]{64}$/.test(p.request_fingerprint)))continue;
    if(p.kind==="create"&&p.input&&typeof p.input==="object"&&"decision" in p.input&&p.decision_version!==1)continue;
    if(p.kind==="create"&&p.input&&typeof p.input==="object"&&"workflow" in p.input&&p.workflow_version!==1)continue;
    if(Date.now()-p.created>7*86400000){p.input=null;p.text="";delete p.draft;this.storage.setItem(key,JSON.stringify(p));}
    this.memory.set(p.id,p);
   }catch{/* Corrupt local data is never used as a request path. */}
  }
  return [...this.memory.values()];
 }
 remove(id:string){this.storage.removeItem(this.prefix+id);this.memory.delete(id);}
 disable(){for(const p of this.list())this.storage.removeItem(this.prefix+p.id);this.enabled=false;}
 clear(){for(const p of this.list())this.storage.removeItem(this.prefix+p.id);this.memory.clear();}
 forget(){this.memory.clear();}
}
