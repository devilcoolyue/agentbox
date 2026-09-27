import { api } from "./api.js";
import { S,bus } from "./state.js";
import { askConfirm,toast } from "./util.js";
import { enhanceSelects,setSelectValue } from "./select.js";
import { gitRequest } from "./git-operations.js";
import type { GitBranches,GitBranch } from "./types.js";
import { actionButton, decorateIcons } from "./icons.js";
let active:HTMLDialogElement|null=null;
export function openGitBranches(repo:string,refreshed:()=>Promise<void>){
 if(active||!S.current)return;const session=S.current.id,token=S.token,d=document.createElement("dialog");active=d;
 d.id="dlg-git-branches";d.className="dlg-git-connections";
 d.innerHTML=`<div class="dlg-head"><h2>分支管理</h2><button class="dlg-x" data-close aria-label="关闭" data-icon="close"></button></div>
 <p data-current class="field-hint"></p><p data-error role="alert" class="login-error"></p>
 <form><div class="dlg-row"><label>新分支名称<input name="name" type="text" required maxlength="240" placeholder="feature/my-task"></label><label>创建起点<select name="start"></select></label></div>
 <div class="dlg-actions"><button type="submit" class="btn btn-primary btn-sm" disabled data-icon="plus" data-tip="创建新分支并切换到该分支">创建</button></div></form>
 <div data-list></div><div class="dlg-actions"><button class="btn btn-sm" data-refresh data-icon="refresh">刷新</button></div>`;
 const form=d.querySelector("form")!,start=form.elements.namedItem("start") as HTMLSelectElement,error=d.querySelector<HTMLElement>("[data-error]")!,list=d.querySelector<HTMLElement>("[data-list]")!;
 let data:GitBranches|null=null,busy=false;
 const prefix=`/sessions/${session}/git`;
 const close=d.querySelector<HTMLButtonElement>("[data-close]")!,create=d.querySelector<HTMLButtonElement>('[type=submit]')!;
 function option(value:string,textContent:string){return Object.assign(document.createElement("option"),{value,textContent});}
 async function load(){
  data=await api<GitBranches>(prefix+"/branches?repo="+encodeURIComponent(repo));if(!d.open||token!==S.token)return;
  const state=data.state;
  d.querySelector("[data-current]")!.textContent=`${repo||"空间文件根目录"} · ${state.detached?"游离 HEAD":state.branch} · ${state.head.slice(0,12)}${data.dirty?" · 有未提交改动":""}${state.unborn?" · 尚无提交":""}`;
  start.replaceChildren(option("","当前 HEAD"),...data.branches.map(b=>option((b.remote?"refs/remotes/":"refs/heads/")+b.name,b.name+(b.remote?" · 远程跟踪":" · 本地"))));
  setSelectValue(start,"");list.replaceChildren();
  for(const b of data.branches){
   const row=document.createElement("div");row.className="git-connection-row";
   const title=document.createElement("strong");title.textContent=b.name+(b.current?" · 当前":"")+(b.remote?" · 远程跟踪":"");
   const meta=document.createElement("p");meta.className="field-hint";meta.textContent=b.head.slice(0,12)+(b.upstream?" → "+b.upstream:"");
   const buttons=document.createElement("div");buttons.className="git-connection-tools";
   const button=(label:string,action:string,disabled:boolean,tip=label)=>{const btn=document.createElement("button");btn.className="btn btn-sm";actionButton(btn,label,action==="delete"?"trash":action==="switch"?"arrow-right":"branch",tip);btn.disabled=disabled;btn.addEventListener("click",()=>void run(()=>act(action,b)));return btn;};
   if(b.remote)buttons.append(button("设为上游","upstream",state.detached||state.unborn,"设为当前分支上游"));
   else buttons.append(button("切换","switch",b.current||data.dirty),button("删除","delete",b.current,"删除已合并的本地分支"));
   row.append(title,meta,buttons);list.append(row);
  }
  if(data.truncated){const msg=document.createElement("p");msg.className="field-hint";msg.textContent="最多显示 500 个分支，请在终端管理更多分支。";list.append(msg);}
  create.disabled=data.dirty||state.unborn;
 }
 async function act(action:string,b?:GitBranch){
  if(!data)return;const state=data.state;
  if(action==="delete"&&!await askConfirm(`删除本地分支「${b!.name}」？`,{title:"删除本地分支",hint:"仅删除已合并分支，不删除远程分支，不允许强制删除。",danger:true,okLabel:"删除", icon: "trash"}))return;
  const base=data.branches.find(branch=>(branch.remote?"refs/remotes/":"refs/heads/")+branch.name===start.value);
  await gitRequest(prefix+"/branches",{repo,action,name:b?.name||(form.elements.namedItem("name") as HTMLInputElement).value.trim(),expected_head:state.head,expected_branch:state.branch,target_head:b?.head||base?.head||"",start:action==="create"?start.value:""},d);
  toast("分支操作已完成");if(action==="create")(form.elements.namedItem("name") as HTMLInputElement).value="";
  await load();if(S.current?.id===session)await refreshed();
 }
 async function run(fn:()=>Promise<void>){
  if(busy||token!==S.token)return;busy=true;error.textContent="";
  const controls=[...d.querySelectorAll<HTMLInputElement|HTMLSelectElement|HTMLButtonElement>("button,input,select")].map(el=>({el,disabled:el.disabled}));
  for(const {el} of controls)el.disabled=true;
  try{await fn();}catch(e){if(d.open&&token===S.token)error.textContent=(e as Error).message;}
  finally{busy=false;for(const {el,disabled} of controls)if(el.isConnected)el.disabled=disabled;close.disabled=false;d.querySelector<HTMLButtonElement>("[data-refresh]")!.disabled=false;if(data)create.disabled=data.dirty||data.state.unborn;}
 }
 form.addEventListener("submit",e=>{e.preventDefault();void run(()=>act("create"));});
 d.querySelector("[data-refresh]")!.addEventListener("click",()=>void run(load));close.addEventListener("click",()=>d.close());
 d.addEventListener("cancel",e=>{if(busy)e.preventDefault();});d.addEventListener("close",()=>{active=null;d.remove();});
 document.body.append(d);decorateIcons(d);enhanceSelects(d);d.showModal();void run(load);
}
bus.addEventListener("signed-out",()=>active?.close());
