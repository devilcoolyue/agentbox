import {api,apiUpload} from "../../api.js";
import {APIError} from "../../problems.js";
import {t} from "../../i18n.js";
import type {GitLiveOperation,GitOperationPage,WorkspaceCreateSpec,WorkspaceCreation,WorkspaceCreationIndex,WorkspaceImport} from "../../types.js";

/** 导入进度：上传按字节；上传完服务端还要校验、解压；Git 导入轮询同一个操作编号的实时状态 */
export type ImportProgress={kind:"upload";loaded:number;total:number}|{kind:"processing"}|{kind:"git";op?:GitLiveOperation};

const id=()=>Array.from(crypto.getRandomValues(new Uint8Array(16)),b=>b.toString(16).padStart(2,"0")).join("");
interface Pending {id:string;spec:WorkspaceCreateSpec;attempt?:string;}

/** Durable server receipts plus a small per-tab, per-identity pending pointer.
 * No file bytes, Git URL, token or password is stored in the browser. */
export class CreationFlow {
  private key="";
  private pending:Pending|undefined;
  view:WorkspaceCreation|undefined;
  index:WorkspaceCreation[]=[];
  get requestID(){return this.pending?.id;}
  get spec(){return this.pending?.spec;}
  private path(){return "/session-creations/"+this.pending!.id;}
  private persist(){
    if(!this.key)throw new Error(t("创建恢复协议未就绪，请刷新重试。"));
    try{sessionStorage.setItem(this.key,JSON.stringify(this.pending));}
    catch{throw new Error(t("无法保存创建确认信息，请允许本站使用会话存储后重试。"));}
  }
  async initialize(signal:AbortSignal){
    let result:WorkspaceCreationIndex;
    try{result=await api<WorkspaceCreationIndex>("/session-creations",{signal});}
    catch(error){if(error instanceof APIError&&error.status===404)throw new Error(t("服务端不支持创建恢复，请更新后重试。"));throw error;}
    if(result.version!==1||!/^[a-f0-9]{64}$/.test(result.actor_key)||!Array.isArray(result.creations))throw new Error(t("服务端不支持创建恢复，请更新后重试。"));
    this.key="agentbox.creation."+result.actor_key;this.index=result.creations;
    try{
      const value=JSON.parse(sessionStorage.getItem(this.key)||"null") as Pending|null;
      if(value&&/^[a-f0-9]{32}$/.test(value.id)&&value.spec&&typeof value.spec.name==="string")this.pending=value;
    }catch{ /* A corrupt pointer is never sent as an API request. */ }
    if(this.pending)await this.reconcile(signal);
  }
  async resume(requestID:string,signal?:AbortSignal){
    const view=await api<WorkspaceCreation>("/session-creations/"+encodeURIComponent(requestID),{signal});
    this.pending={id:view.request_id,spec:view.request};this.view=view;this.persist();
  }
  async reconcile(signal?:AbortSignal){
    if(!this.pending)return;
    try{this.view=await api<WorkspaceCreation>(this.path(),{signal});}
    catch(error){if(error instanceof APIError&&error.status===404){this.view=undefined;return;}throw error;}
    return this.view;
  }
  async ensure(spec:WorkspaceCreateSpec,signal:AbortSignal){
    if(!this.pending)this.pending={id:id(),spec};
    this.persist();
    // Query before every retry; a missing receipt still uses the SAME ID.
    await this.reconcile(signal);
    if(this.view?.state==="abandoned")throw new Error(t("该创建已放弃或空间已删除，请明确开始新的创建。"));
    if(!this.view||this.view.state==="reserved"){
      await api(this.path(),{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify(this.pending.spec),signal});
      await this.reconcile(signal);
    }
    if(!this.view?.workspace_exists)throw new Error(t("尚未确认空间创建结果，请查询状态后继续。"));
    return this.view;
  }
  latest(){return this.view?.imports[0];}
  async import(kind:"upload"|"git",directory:string,files:File[],uploadMode:string,connection:string,url:string,signal:AbortSignal,onProgress?:(progress:ImportProgress)=>void){
    await this.reconcile(signal);
    if(this.view?.busy)throw new Error(t("导入仍在执行，请稍后查询状态。"));
    const previous=this.latest();
    if(previous?.state==="running"||previous?.state==="uncertain")throw new Error(t("导入结果待核对，请先查看已有文件并确认。"));
    if(previous?.state==="succeeded")return previous;
    if(!this.pending!.attempt||previous?.state==="failed"||previous?.state==="reviewed")this.pending!.attempt=id();
    this.persist();
    let result:WorkspaceImport;
    if(kind==="upload"){
      if(!files.length)throw new Error(t("请选择项目压缩包、文件或目录。"));
      const data=new FormData();data.append("mode",uploadMode==="archive"?"archive":"files");
      data.append("paths",JSON.stringify(files.map(file=>file.webkitRelativePath||file.name)));
      for(const file of files)data.append("files",file,file.name);
      result=await apiUpload<WorkspaceImport>(this.path()+`/upload?attempt_id=${this.pending!.attempt}&directory=${encodeURIComponent(directory)}`,data,{signal,
        onProgress:(loaded,total)=>onProgress?.(loaded<total?{kind:"upload",loaded,total}:{kind:"processing"})});
    }else{
      // 服务端用尝试编号作为 Git 操作编号：只读轮询它的实时进度，不改变导入请求本身
      const attempt=this.pending!.attempt!;let polling=true;
      const poll=async()=>{
        while(polling&&!signal.aborted){
          try{const page=await api<GitOperationPage>("/git/operations",{signal:AbortSignal.any([signal,AbortSignal.timeout(5000)])});if(polling)onProgress?.({kind:"git",op:page.active.find(op=>op.request_id===attempt)});}
          catch{/* 进度只是观察，读失败不影响导入结果 */}
          await new Promise(resolve=>setTimeout(resolve,700));
        }
      };
      onProgress?.({kind:"git"});void poll();
      try{result=await api<WorkspaceImport>(this.path()+"/git",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({attempt_id:attempt,connection_id:connection,url,directory}),signal});}
      finally{polling=false;}
    }
    await this.reconcile(signal);
    if(result.state!=="succeeded")throw new Error(t(result.state==="failed"?"项目导入失败，可修正后在同一空间重试。":"导入结果待核对，请先查看已有文件并确认。"));
    return result;
  }
  async review(signal:AbortSignal){
    await this.reconcile(signal);
    const attempt=this.latest();if(!attempt||this.view?.busy)throw new Error(t("导入仍在执行，请稍后查询状态。"));
    await api(this.path(),{method:"POST",body:JSON.stringify({action:"review",attempt_id:attempt.attempt_id}),signal});
    this.pending!.attempt=undefined;this.persist();await this.reconcile(signal);
  }
  async finish(signal:AbortSignal){
    await this.reconcile(signal);
    if(!this.view?.workspace_exists)throw new Error(t("尚未确认空间创建结果，请查询状态后继续。"));
    const session=this.view.session;
    await api(this.path(),{method:"POST",body:JSON.stringify({action:"finish"}),signal});
    this.clear();return session;
  }
  async abandon(signal:AbortSignal){
    if(this.pending){await api(this.path(),{method:"POST",body:JSON.stringify({action:"abandon"}),signal});}
    this.clear();
  }
  clear(){if(this.pending)this.index=this.index.filter(item=>item.request_id!==this.pending!.id);if(this.key)try{sessionStorage.removeItem(this.key);}catch{}this.pending=undefined;this.view=undefined;}
}
