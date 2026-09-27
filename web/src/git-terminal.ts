import { api } from "./api.js";
import { S,bus } from "./state.js";
import { askConfirm,fmtTime } from "./util.js";
import { gitRequest } from "./git-operations.js";
import { actionButton, decorateIcons } from "./icons.js";
interface Grant {id:string;repo:string;remote:string;write:boolean;expires_at:string;command:string;}
const dialogs=new Set<HTMLDialogElement>();
export function openGitTerminal(repo:string,remote:string,readOnly:boolean){
 if(!S.current)return;
 const session=S.current.id,token=S.token,prefix=`/sessions/${session}/git/terminal`,d=document.createElement("dialog");
 d.id="dlg-git-terminal";d.className="dlg-git-connections";
 d.innerHTML=`<div class="dlg-head"><h2>终端 Git 授权</h2><button class="dlg-x" data-close aria-label="关闭" data-icon="close"></button></div>
 <p data-target class="field-hint"></p><p class="field-hint">授权有效期 30 分钟，仅用于当前仓库和 remote。请在终端运行下方命令；原生 git push 不会自动使用此授权。</p>
 <label class="check"><input type="checkbox" data-write>允许推送</label>
 <p class="field-hint">默认允许查看状态、获取更新与快进拉取。授权后，空间内的用户程序和 Agent 都能使用；开启推送代表允许它们写入此远程仓库。长期 Token 和 SSH 私钥保留在服务端。</p>
 <div class="dlg-actions"><button class="btn btn-primary btn-sm" data-create data-icon="key" data-tip="创建当前仓库的终端授权，有效期 30 分钟">创建授权</button><button class="btn btn-sm" data-refresh data-icon="refresh">刷新</button></div>
 <p data-error class="login-error" role="alert"></p><div data-list></div>`;
 const write=d.querySelector<HTMLInputElement>("[data-write]")!,error=d.querySelector<HTMLElement>("[data-error]")!,list=d.querySelector<HTMLElement>("[data-list]")!;
 write.disabled=readOnly;d.querySelector("[data-target]")!.textContent=`${repo||"空间文件根目录"} · ${remote}`;
 let busy=false;
 async function load(){
  const grants=await api<Grant[]>(prefix);if(!d.open||token!==S.token)return;list.replaceChildren();
  const selected=grants.filter(g=>g.repo===repo&&g.remote===remote);
  if(!selected.length)list.textContent="当前没有终端授权。";
  for(const g of selected){
   const row=document.createElement("div");row.className="git-connection-row";
   const label=document.createElement("p");label.className="field-hint";label.textContent=`${g.write?"允许推送":"只读远程"} · 到期 ${fmtTime(Date.parse(g.expires_at))}`;
   const commands=document.createElement("pre");commands.style.whiteSpace="pre-wrap";commands.style.overflowWrap="anywhere";
   commands.textContent=`${g.command} status\n${g.command} fetch\n${g.command} pull`+(g.write?`\n${g.command} push`:"");
   const revoke=document.createElement("button");revoke.className="btn btn-sm";actionButton(revoke,"撤销","shield-off","撤销授权");
   revoke.addEventListener("click",()=>void run(async()=>{await api(prefix+"/"+g.id,{method:"DELETE"});await load();}));
   row.append(label,commands,revoke);list.append(row);
  }
 }
 async function run(fn:()=>Promise<void>){
  if(busy||token!==S.token)return;busy=true;error.textContent="";
  for(const b of d.querySelectorAll<HTMLButtonElement>("button"))b.disabled=true;
  try{await fn();}catch(e){if(d.open&&token===S.token)error.textContent=(e as Error).message;}
  finally{busy=false;for(const b of d.querySelectorAll<HTMLButtonElement>("button"))b.disabled=false;}
 }
 d.querySelector("[data-create]")!.addEventListener("click",()=>void run(async()=>{
  if(write.checked&&!await askConfirm("允许空间内的程序推送此仓库？",{title:"终端推送授权",hint:"30 分钟内可推送当前仓库的分支，不允许强制推送或删除远程分支。命令的确认提示不能阻止空间内的其他程序使用授权。",okLabel:"创建授权",icon:"key"}))return;
  if(!d.open||token!==S.token)return;
  await gitRequest(prefix,{repo,remote,write:write.checked},d);await load();
 }));
 d.querySelector("[data-refresh]")!.addEventListener("click",()=>void run(load));
 d.querySelector("[data-close]")!.addEventListener("click",()=>d.close());
 d.addEventListener("cancel",e=>{if(busy)e.preventDefault();});d.addEventListener("close",()=>{dialogs.delete(d);d.remove();});
 document.body.append(d);dialogs.add(d);decorateIcons(d);d.showModal();void run(load);
}
bus.addEventListener("signed-out",()=>{for(const d of dialogs)d.close();});
