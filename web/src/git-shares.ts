import { htmlText as trHTML, setText, setTextRender, t as i18nText } from "./i18n.js";
import { skelRows } from "./skeleton.js";
import { createGitSurface, type GitSurface } from "./git-surface.js";
import { api } from "./api.js";
import { S,bus } from "./state.js";
import { toast } from "./util.js";
import type { GitConnection,User } from "./types.js";
let active:GitSurface|null=null;
interface Share {user:string;write:boolean;}
export function openGitShares(c:GitConnection,saved:()=>Promise<void>){
 if(S.role!=="admin"||active)return;const token=S.token,d=createGitSurface(() => c.label+i18nText(" · 使用授权"), `
 <p class="field-hint"><span data-i18n="仅对明确选中的用户开放连接。只读用户不能推送或创建 PR/MR；连接本身设为只读时，所有用户都只读。私钥和 Token 不向用户提供，个人 OAuth 不共享。">${trHTML("仅对明确选中的用户开放连接。只读用户不能推送或创建 PR/MR；连接本身设为只读时，所有用户都只读。私钥和 Token 不向用户提供，个人 OAuth 不共享。")}</span></p>
 <p class="field-hint"><span data-i18n="撤权阻止后续请求，已经发到上游的操作可能继续完成。用户使用各自隧道，不会借用管理员内网。">${trHTML("撤权阻止后续请求，已经发到上游的操作可能继续完成。用户使用各自隧道，不会借用管理员内网。")}</span></p>
 <div data-users></div><p data-error role="alert" class="login-error"></p><div class="dlg-actions"><button class="btn btn-primary" data-save disabled data-icon="save" data-tip="${trHTML("保存 Git 连接的用户使用授权")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;保存 Git 连接的用户使用授权&quot;}"><span class="action-label" data-i18n="保存">${trHTML("保存")}</span></button></div>`);
 active=d;d.id="dlg-git-shares";d.querySelector("[data-users]")!.append(skelRows(3,i18nText("读取用户中…"),{rowClass:"git-connection-row",lines:0,title:[18,20]}));
 setTextRender(d.querySelector("h2")!, () => c.label+i18nText(" · 使用授权"));
 const error=d.querySelector<HTMLElement>("[data-error]")!,save=d.querySelector<HTMLButtonElement>("[data-save]")!,close=d.querySelector<HTMLButtonElement>("[data-close]")!;
 let revision=0,busy=false;
 const picks=new Map<string,{enabled:HTMLInputElement;write:HTMLInputElement}>();
 d.addEventListener("cancel",e=>{if(busy)e.preventDefault();});d.addEventListener("close",()=>{active=null;d.remove();});
 save.addEventListener("click",async()=>{
  if(busy||token!==S.token)return;busy=true;save.disabled=close.disabled=true;error.textContent="";
  const users:Share[]=[];for(const [user,p] of picks)if(p.enabled.checked)users.push({user,write:p.write.checked});
  try{await api(`/git/connections/${c.id}/shares`,{method:"PUT",body:JSON.stringify({revision,users})});d.close();if(token===S.token){toast(i18nText("共享授权已保存"));await saved();}}
  catch(e){if(d.open)error.textContent=(e as Error).message;else if(token===S.token)toast((e as Error).message,true);}
  finally{busy=false;save.disabled=close.disabled=false;}
 });

 void Promise.all([api<User[]>("/users"),api<{users:Share[];revision:number}>(`/git/connections/${c.id}/shares`)]).then(([users,access])=>{
  if(!d.open||token!==S.token)return;revision=access.revision;const list=d.querySelector<HTMLElement>("[data-users]")!;list.replaceChildren();
  for(const user of users){if(user.name===c.owner)continue;
   const row=document.createElement("div");row.className="git-connection-row git-connection-tools";
   const enabled=document.createElement("input"),write=document.createElement("input");enabled.type=write.type="checkbox";
   const entry=access.users.find(s=>s.user===user.name);enabled.checked=!!entry;write.checked=entry?.write??false;write.disabled=!enabled.checked;
   const name=document.createElement("label");name.className="check";name.append(enabled,document.createTextNode(user.name));
   const permission=document.createElement("label");permission.className="check";permission.append(write,document.createTextNode(i18nText("允许写入")));
   enabled.addEventListener("change",()=>{write.disabled=!enabled.checked;});row.append(name,permission);list.append(row);picks.set(user.name,{enabled,write});
  }
  if(!picks.size)setText(list, "暂无其他用户。");save.disabled=false;
 }).catch(e=>{error.textContent=(e as Error).message;d.querySelector(".skeleton-list")?.remove();});
}
bus.addEventListener("signed-out",()=>active?.close());
