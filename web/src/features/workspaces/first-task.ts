import {S,bus} from "../../state.js";
import {setTab} from "../../sessions.js";
import {autoGrow} from "../../chat.js";
import {archiveDownloadURL} from "../../api.js";
import {$,startDownload,toast} from "../../util.js";
import {setTextRender,t} from "../../i18n.js";
import {actionButton} from "../../icons.js";
import type {Session,WorkspaceCreateSpec} from "../../types.js";

interface Created {session:Session;source:WorkspaceCreateSpec["source"];directory:string;}

/** A login owns the tour; only an explicit action fills input or downloads.
 * Completion is scoped to this browser and account, never a bearer token. */
export function initFirstTask(){
 const lifetime=new AbortController(),owner=S.token,options={signal:lifetime.signal};
 const panel=$("workspace-guide"),spotlight=$("guide-spotlight"),button=$<HTMLButtonElement>("guide-example");
 const created=new Map<string,Created>();
 const key="agentbox.workspace-tour.v1."+(S.draftScope||JSON.stringify([location.origin,S.user]));
 let seen=false,step=0,selected="",frame=0,active=false,returnFocus:HTMLElement|null=null;
 try{seen=localStorage.getItem(key)==="seen";}catch{/* Storage denial must not prevent using the workspace. */}
 const current=()=>!lifetime.signal.aborted&&S.token===owner;
 const close=(restore=false)=>{
  active=false;panel.hidePopover();spotlight.hidePopover();cancelAnimationFrame(frame);frame=0;
  if(restore){
   const fallback=[$("btn-wb-more"),$("btn-kebab")].find(el=>el.getClientRects().length);
   const target=returnFocus?.isConnected&&returnFocus.matches("button,input,textarea,[tabindex]")&&returnFocus.getClientRects().length?returnFocus:fallback;
   target?.focus({preventScroll:true});
  }
  returnFocus=null;
 };
 const targets=()=>[S.tab==="chat"?document.querySelector<HTMLElement>(".composer"):document.querySelector<HTMLElement>('.tab[data-tab="chat"]'),document.querySelector<HTMLElement>('.tab[data-tab="files"]'),document.querySelector<HTMLElement>('.tab[data-tab="changes"]')];
 const position=()=>{
  frame=0;if(!active||!current())return;
  const target=targets()[step];if(!target||!target.getClientRects().length){close();return;}
  const v=window.visualViewport,left=v?.offsetLeft||0,top=v?.offsetTop||0,width=v?.width||innerWidth,height=v?.height||innerHeight;
  const box=target.getBoundingClientRect(),gap=12,pad=5;
  Object.assign(spotlight.style,{left:box.left-pad+"px",top:box.top-pad+"px",width:box.width+2*pad+"px",height:box.height+2*pad+"px"});
  panel.style.maxHeight=Math.max(120,height-24)+"px";
  panel.style.width=Math.min(340,width-24)+"px";
  const card=panel.getBoundingClientRect();
  let y=box.bottom+gap;
  if(y+card.height>top+height-12)y=box.top-card.height-gap;
  Object.assign(panel.style,{left:Math.max(left+12,Math.min(box.left,left+width-card.width-12))+"px",top:Math.max(top+12,Math.min(y,top+height-card.height-12))+"px"});
 };
 const schedule=()=>{if(active&&!frame)frame=requestAnimationFrame(position);};
 const render=()=>{
  if(!current()||!active||!S.current)return;
  panel.dataset.step=String(step);
  setTextRender($("guide-title"),()=>t(["1. 编辑任务","2. 检查结果","3. 交付成果"][step]));
  setTextRender($("guide-note"),()=>t([
   "描述你想完成的事，也可以先编辑一个示例。只有点击发送才会开始任务。",
   "任务完成后，在这里查看文件和验证结果。",
   "有 Git 仓库时先审查变更再提交；也可以直接下载工作区。"
  ][step]));
  $("guide-progress").textContent=`${step+1} / 3`;
  setTextRender($("guide-config"),()=>S.current?t("{account} · 空间默认模型 {model}",{account:S.current.account_label||S.current.account_id,model:S.current.default_model||t("尚未确认")}):"");
  $("guide-config").classList.toggle("hidden",step!==0);
  for(const [id,visible] of [["guide-example",step===0],["guide-files",step===1],["guide-changes",step===2],["guide-download",step===2]] as const)$(id).classList.toggle("hidden",!visible);
  button.disabled=S.histLoading||!!S.histError||S.chatState==="running";
  $<HTMLButtonElement>("guide-back").disabled=step===0;
  actionButton($("guide-next"),()=>t(step===2?"完成引导":"下一步"),step===2?"check":"arrow-right");
  position();
 };
 const start=()=>{
  if(!current()||!S.current||S.view!=="work")return;
  returnFocus=document.activeElement as HTMLElement;selected=S.current.id;step=0;active=true;seen=true;
  try{localStorage.setItem(key,"seen");}catch{/* Remember for this login when storage is unavailable. */}
  spotlight.showPopover();panel.showPopover();render();$("guide-next").focus({preventScroll:true});
 };
 const update=()=>{
  if(!current())return;
  if(active&&(!S.current||S.current.id!==selected||S.view!=="work")){close();return;}
  if(!active&&!seen&&S.current&&created.has(S.current.id)&&S.view==="work"&&!S.histLoading&&!document.querySelector("dialog[open]"))start();
  else if(active)render();
 };
 bus.addEventListener("workspace-created",event=>{if(current()){const detail=(event as CustomEvent<Created>).detail;created.set(detail.session.id,detail);queueMicrotask(update);}},options);
 bus.addEventListener("workspace-guide-open",start,options);
 for(const event of ["navigation-changed","view-changed","data-updated","chat-view-updated"])bus.addEventListener(event,update,options);
 $("guide-next").addEventListener("click",()=>{if(step===2)close(true);else{step++;render();}},options);
 $("guide-back").addEventListener("click",()=>{if(step>0){step--;render();}},options);
 $("guide-skip").addEventListener("click",()=>close(true),options);
 document.addEventListener("keydown",event=>{if(active&&event.key==="Escape"){event.preventDefault();event.stopPropagation();close(true);}}, {...options,capture:true});
 document.addEventListener("pointerdown",event=>{if(active&&!panel.contains(event.target as Node))close();},options);
 window.addEventListener("resize",schedule,options);window.addEventListener("scroll",schedule,{...options,capture:true});
 window.visualViewport?.addEventListener("resize",schedule,options);window.visualViewport?.addEventListener("scroll",schedule,options);
 const observer=new ResizeObserver(schedule);observer.observe(panel);for(const target of targets())if(target)observer.observe(target);
 button.addEventListener("click",()=>{
  if(!current()||!S.current||S.histLoading||S.histError||S.chatState==="running")return;
  setTab("chat");const input=$<HTMLTextAreaElement>("chat-input");
  if(input.value.trim()){close();toast(t("已保留对话框中的内容，请先编辑或发送现有任务。"));input.focus();return;}
  const context=created.get(S.current.id);
  input.value=context&&context.source!=="empty"
   ?t("请先查看 {path} 中的项目与 README，概述运行方式；完成一项小而明确的文档改进，保留已有内容。最后列出修改文件、验证结果和仍待处理的问题。",{path:JSON.stringify("/workspace/"+context.directory)})
   :t("请先查看 /workspace 中的现有文件。若为空，请创建一个简洁的 README.md，说明项目目标、运行方式和下一步；已有文件时先总结结构并完成一项小的文档改进。最后列出修改的文件与验证结果。");
  autoGrow();close();input.focus();input.setSelectionRange(0,input.value.length);
 },options);
 $("guide-files").addEventListener("click",()=>{if(current()&&S.current){close();S.fileScope="workspace";S.filePath="";setTab("files");}},options);
 $("guide-changes").addEventListener("click",()=>{if(current()&&S.current){close();setTab("changes");}},options);
 $("guide-download").addEventListener("click",()=>{if(current()&&S.current)startDownload(archiveDownloadURL("workspace"));},options);
 return ()=>{if(lifetime.signal.aborted)return;close();lifetime.abort();observer.disconnect();created.clear();$("guide-config").textContent="";};
}
