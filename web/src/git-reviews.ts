import { api } from "./api.js";
import { S,bus } from "./state.js";
import { enhanceSelects,setSelectValue } from "./select.js";
import { gitRequest } from "./git-operations.js";
import { toast } from "./util.js";
import type { GitConnection,GitReview,GitReviewPage,GitReviewPreview } from "./types.js";
let active:HTMLDialogElement|null=null;
export function openGitReviews(repo:string,remote:string,connection:GitConnection,connections:GitConnection[]){
 if(active||!S.current)return;const session=S.current.id,token=S.token,d=document.createElement("dialog");active=d;
 d.id="dlg-git-reviews";d.className="dlg-git-connections";
 d.innerHTML=`<div class="dlg-head"><h2>Pull Request / Merge Request</h2><button class="dlg-x" data-close aria-label="关闭">×</button></div>
 <p data-project class="field-hint"></p><label data-api-label>平台 API 连接<select data-api></select></label>
 <p class="field-hint">先推送当前分支。此处创建同一仓库内的 PR/MR，不自动推送、合并或删除分支。GitLab Token/OAuth 需要 api 权限；GitHub 需要仓库 Pull requests 写权限。</p>
 <div class="git-connection-tools"><button class="btn btn-sm" data-refresh>刷新列表</button><button class="btn btn-sm" data-next disabled>下一页</button></div>
 <p data-error role="alert" class="login-error"></p><div data-list></div>
 <form><div class="dlg-row"><label>来源分支<input name="source" type="text" readonly></label><label>目标分支<input name="target" type="text" required maxlength="240" placeholder="main"></label></div>
 <label>标题<input name="title" type="text" required maxlength="240"></label><label>描述<textarea name="body" rows="4" maxlength="32000"></textarea></label>
 <label class="check"><input name="draft" type="checkbox" checked>创建为草稿</label>
 <div class="dlg-actions"><button class="btn btn-primary" type="submit" disabled>预览创建</button></div></form>
 <div data-preview class="hidden"><p data-summary class="field-hint"></p><pre data-body></pre><div data-existing></div><button class="btn btn-primary" data-create>确认创建</button></div>`;
 const form=d.querySelector("form")!,field=(name:string)=>form.elements.namedItem(name) as HTMLInputElement;
 const apiSelect=d.querySelector<HTMLSelectElement>("[data-api]")!,error=d.querySelector<HTMLElement>("[data-error]")!,list=d.querySelector<HTMLElement>("[data-list]")!,previewBox=d.querySelector<HTMLElement>("[data-preview]")!;
 const button=(name:string)=>d.querySelector<HTMLButtonElement>(`[data-${name}]`)!;
 let page:GitReviewPage|null=null,preview:GitReviewPreview|null=null,busy=false;
 const prefix=`/sessions/${session}/git`;
 const option=(c:GitConnection)=>Object.assign(document.createElement("option"),{value:c.id,textContent:c.label+(c.read_only?" · 只读":"")});
 let eligible=[connection];if(connection.auth_type==="ssh")eligible=connections.filter(c=>c.auth_type!=="ssh"&&c.provider===connection.provider&&c.enabled&&new URL(c.base_url).hostname===new URL(connection.base_url).hostname);
 apiSelect.replaceChildren(...eligible.map(option));if(connection.auth_type!=="ssh")setSelectValue(apiSelect,connection.id);
 d.querySelector("[data-api-label]")!.classList.toggle("hidden",connection.auth_type!=="ssh");
 function link(review:GitReview){
  const row=document.createElement("p"),anchor=document.createElement("a");
  anchor.textContent=`#${review.number} ${review.title}${review.draft?" · 草稿":""}`;
  // Server validates links, and browser checks protocol again for stale fixtures.
  try{const url=new URL(review.url);if(url.protocol==="https:"){anchor.href=url.href;anchor.target="_blank";anchor.rel="noopener noreferrer";}}catch{}
  row.append(anchor,document.createTextNode(` · ${review.source} → ${review.target}`));return row;
 }
 function invalidate(){preview=null;previewBox.classList.add("hidden");}
 function sync(){
  const c=eligible.find(c=>c.id===apiSelect.value);
  (d.querySelector('[type="submit"]') as HTMLButtonElement).disabled=busy||!page||!page.head||!page.source_branch||!c||c.read_only;
  button("next").disabled=busy||!page?.has_more;
 }
 async function load(number=1){
  invalidate();if(!apiSelect.value)throw new Error("请先添加同平台的 HTTPS Token/OAuth API 连接，并授予所需 API 权限。");
  const q=new URLSearchParams({repo,remote,api_connection_id:apiSelect.value,page:String(number)});
  const next=await api<GitReviewPage>(prefix+"/reviews?"+q);if(!d.open||token!==S.token)return;page=next;
  d.querySelector("[data-project]")!.textContent=`${page.provider} · ${page.project} · ${remote} · 第 ${page.page} 页`;
  list.replaceChildren(...page.rows.map(link));if(!page.rows.length)list.textContent="没有打开的 PR/MR。";
  field("source").value=page.source_branch;if(!field("target").value)field("target").value=page.default_branch;
 }
 async function run(fn:()=>Promise<void>){
  if(busy||token!==S.token)return;busy=true;error.textContent="";
  const controls=[...d.querySelectorAll<HTMLInputElement|HTMLButtonElement|HTMLSelectElement|HTMLTextAreaElement>("input,button,select,textarea")];for(const el of controls)el.disabled=true;
  try{await fn();}catch(e){if(d.open&&token===S.token)error.textContent=(e as Error).message;}
  finally{busy=false;for(const el of controls)el.disabled=false;sync();}
 }
 const payload=()=>({repo,remote,api_connection_id:apiSelect.value,title:field("title").value.trim(),body:field("body").value,source:field("source").value,target:field("target").value.trim(),draft:field("draft").checked,expected_head:page?.head||""});
 form.addEventListener("input",invalidate);apiSelect.addEventListener("change",()=>{page=null;void run(()=>load());});
 form.addEventListener("submit",e=>{e.preventDefault();void run(async()=>{
  invalidate();const candidate=await gitRequest<GitReviewPreview>(prefix+"/review-preview",payload(),d);
  if(!d.open||token!==S.token)return;preview=candidate;
  d.querySelector("[data-summary]")!.textContent=`${candidate.project}\n${candidate.source.name} (${candidate.source.sha.slice(0,12)}) → ${candidate.target.name} (${candidate.target.sha.slice(0,12)})${candidate.target.protected?" · 受保护目标分支":""}\n${candidate.draft?"草稿 · ":""}${candidate.title}`;
  d.querySelector("[data-body]")!.textContent=candidate.body;
  d.querySelector("[data-existing]")!.replaceChildren(...candidate.existing.map(link));button("create").classList.toggle("hidden",candidate.existing.length>0);previewBox.classList.remove("hidden");
 });});
 button("create").addEventListener("click",()=>void run(async()=>{
  const checked=preview;if(!checked)return;invalidate();
  const result=await gitRequest<{review:GitReview;existing:boolean}>(prefix+"/reviews",{repo,remote,api_connection_id:checked.connection_id,title:checked.title,body:checked.body,source:checked.source.name,target:checked.target.name,draft:checked.draft,expected_head:checked.source.sha,expected_target:checked.target.sha},d);
  toast(result.existing?"已存在相同 PR/MR":"PR/MR 已创建");await load();
 }));
 button("refresh").addEventListener("click",()=>void run(()=>load()));button("next").addEventListener("click",()=>void run(()=>load((page?.page||1)+1)));button("close").addEventListener("click",()=>d.close());
 d.addEventListener("cancel",e=>{if(busy)e.preventDefault();});d.addEventListener("close",()=>{active=null;d.remove();});document.body.append(d);enhanceSelects(d);d.showModal();void run(()=>load());
}
bus.addEventListener("signed-out",()=>active?.close());
