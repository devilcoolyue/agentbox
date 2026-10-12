import {api} from "../../api.js";
import {setText,t} from "../../i18n.js";

interface Change {path:string;status:string;before?:{kind:string;mode:number};after?:{kind:string;mode:number}}
interface Diff {version:number;scope:string;run_id:string;snapshot:{id:string;sha256:string};changes:Change[];diff?:string;binary?:boolean;too_large?:boolean}
export function createEvidence(owner:string,signal:AbortSignal,scope:()=>string,owns:()=>boolean){
 const box=document.createElement("section");box.className="collaboration-evidence";
 let generation=0;
 const clear=()=>{generation++;box.replaceChildren();};
 const check=(v:{version:number;scope:string;run_id:string},run:string)=>{if(v.version!==1||v.scope!==scope()||v.run_id!==run)throw new Error(t("请检查任务记录"));};
 const showDiff=async(run:string,snapshot="")=>{
  const current=++generation;
  const result=await api<Diff>(`/collaboration/runs/${run}/diff${snapshot?"?snapshot="+encodeURIComponent(snapshot):""}`,{signal});
  if(!owns()||current!==generation)return;check(result,run);
  const title=document.createElement("h3");setText(title,"文件变更");
  const caption=document.createElement("p");caption.textContent=t(snapshot?"异常工作副本快照，仅用于核对":"已确认快照（运行中或异常退出的未确认修改不在此列）")+" · "+result.snapshot.id;
  const list=document.createElement("div"),preview=document.createElement("pre");preview.className="collaboration-diff";
  if(result.changes.length===0)setText(list,"没有文件变更");
  let selection=0;
  for(const change of result.changes){const button=document.createElement("button");button.type="button";button.className="btn btn-sm";button.textContent=`${t(({added:"新增",deleted:"删除",modified:"修改"} as Record<string,string>)[change.status]||"修改")} ${change.path}`;
   button.addEventListener("click",()=>{const selected=++selection;void(async()=>{try{const diff=await api<Diff>(`/collaboration/runs/${run}/diff?snapshot=${encodeURIComponent(result.snapshot.id)}&path=${encodeURIComponent(change.path)}`,{signal});if(!owns()||current!==generation||selected!==selection)return;check(diff,run);preview.textContent=diff.too_large?t("文件较大，请下载结果查看"):diff.binary?t("二进制文件已变化"):(diff.diff||t("文件类型或权限已变化"));}catch(e){if(owns()&&current===generation)preview.textContent=String(e);}})();},{signal});list.append(button);
  }
  box.replaceChildren(title,caption,list,preview);
 };
 const showReview=async(run:string,step:{id:string;revision:number},runRevision:number,submit:()=>Promise<void>)=>{
  const current=++generation;
  const inspection=await api<{version:number;scope:string;run_id:string;artifact:{id:string}}>(`/collaboration/runs/${run}/inspection`,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({scope:scope(),expected_revision:runRevision}),signal});
  if(!owns()||current!==generation)return;check(inspection,run);
  const changes=await api<Diff>(`/collaboration/runs/${run}/diff?snapshot=${encodeURIComponent(inspection.artifact.id)}`,{signal});
  if(!owns()||current!==generation)return;check(changes,run);
  const result=await api<{version:number;scope:string;run_id:string;entries:unknown[];next:number;has_more:boolean}>(`/collaboration/runs/${run}/steps/${step.id}/history`,{signal});
  if(!owns()||current!==generation)return;check(result,run);
  const title=document.createElement("h3");setText(title,"核对异常运行结果");
  const notice=document.createElement("p");setText(notice,"请核对运行记录、费用和已确认快照。异常退出可能留下额外修改；核对不会把任务标成成功，也不会重新执行或撤销扣费。继续任务需要重新配置并规划。");
  const download=document.createElement("button");download.type="button";download.className="btn btn-sm";setText(download,"下载核对副本");download.addEventListener("click",()=>{download.disabled=true;void(async()=>{try{const response=await fetch(`/api/collaboration/runs/${run}/download?snapshot=${encodeURIComponent(inspection.artifact.id)}`,{headers:{Authorization:`Bearer ${owner}`},signal});if(!response.ok)throw new Error(t("结果下载失败"));const blob=await response.blob();if(!owns()||current!==generation)return;const url=URL.createObjectURL(blob),link=document.createElement("a");link.href=url;link.download=`collaboration-${run}-inspection.zip`;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}catch(e){if(owns()&&current===generation)notice.textContent=String(e);}finally{download.disabled=false;}})();},{signal});
  const files=document.createElement("div"),diffBody=document.createElement("pre");let selected=0;
  const fileTitle=document.createElement("p");setText(fileTitle,"异常工作副本快照，仅用于核对");files.append(fileTitle);
  for(const file of changes.changes){const b=document.createElement("button");b.type="button";b.className="btn btn-sm";b.textContent=file.path;b.addEventListener("click",()=>{const version=++selected;void(async()=>{try{const data=await api<Diff>(`/collaboration/runs/${run}/diff?snapshot=${encodeURIComponent(inspection.artifact.id)}&path=${encodeURIComponent(file.path)}`,{signal});if(!owns()||current!==generation||version!==selected)return;check(data,run);diffBody.textContent=data.too_large?t("文件较大，请下载结果查看"):data.binary?t("二进制文件已变化"):data.diff||t("文件类型或权限已变化");}catch(e){if(owns()&&current===generation)diffBody.textContent=String(e);}})();},{signal});files.append(b);}
  if(changes.changes.length===0){const empty=document.createElement("p");setText(empty,"没有文件变更");files.append(empty);}
  const transcript=document.createElement("pre");transcript.textContent=JSON.stringify(result.entries,null,2);
  const more=document.createElement("button");more.type="button";setText(more,"加载更多记录");more.hidden=!result.has_more;let cursor=result.next;
  more.addEventListener("click",()=>{more.disabled=true;void(async()=>{try{const page=await api<typeof result>(`/collaboration/runs/${run}/steps/${step.id}/history?after=${cursor}`,{signal});if(!owns()||current!==generation)return;check(page,run);transcript.textContent+=("\n"+JSON.stringify(page.entries,null,2));cursor=page.next;more.hidden=!page.has_more;}catch(e){if(owns())notice.textContent=String(e);}finally{more.disabled=false;}})();},{signal});
  const label=document.createElement("label"),checkbox=document.createElement("input"),text=document.createElement("span");checkbox.type="checkbox";setText(text,"我已核对记录并理解上述影响");label.append(checkbox,text);
  const confirm=document.createElement("button");confirm.type="button";confirm.className="btn";confirm.disabled=true;setText(confirm,"确认核对");checkbox.addEventListener("change",()=>{confirm.disabled=!checkbox.checked;},{signal});
  confirm.addEventListener("click",()=>{confirm.disabled=true;void submit().then(clear).catch(e=>{if(owns()&&current===generation){notice.textContent=String(e);confirm.disabled=false;}});},{signal});
  box.replaceChildren(title,notice,files,diffBody,download,transcript,more,label,confirm);
 };
 return {box,clear,showDiff,showReview};
}
