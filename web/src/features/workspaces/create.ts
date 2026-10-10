import {api} from "../../api.js";
import {S,emit} from "../../state.js";
import {refreshAll} from "../../data.js";
import {setText,setTextRender,t} from "../../i18n.js";
import {setSelectValue} from "../../select.js";
import {$,btnBusy,btnDone,fmtBytes} from "../../util.js";
import {actionButton} from "../../icons.js";
import {openSetupSettings} from "../../onboarding.js";
import {CreationFlow,type ImportProgress} from "./creation-flow.js";
import {ProgressBar,RateMeter} from "../../progress.js";
import {progressView} from "../../git-operations.js";
import {renderCreationSummary,resourceKey,validResources} from "./config-summary.js";
import type {Account,GitConnection,Session,WorkspaceCreateSpec,OnboardingSnapshot} from "../../types.js";

/** 导入进度：上传时按字节画进度条与速率，上传完等服务端校验解压时转为不确定进度；
 * Git 导入复用 Git 操作的进度块（不带取消钮，导入结果仍以服务端收据为准）。 */
function importMeter(){
 const host=$("new-import-meter");
 let upload:{box:HTMLElement;label:HTMLElement;bar:ProgressBar;rate:RateMeter}|undefined;
 let git:ReturnType<typeof progressView>|undefined;
 const show=(box:HTMLElement)=>{if(box.parentElement!==host)host.replaceChildren(box);host.classList.remove("hidden");};
 return {
  update(p:ImportProgress){
   if(p.kind==="git"){
    if(!git)git=progressView(undefined,false);
    show(git.box);if(p.op)git.update(p.op);else setText(git.label,"准备 Git 操作…");
    return;
   }
   if(!upload){
    const box=document.createElement("div");box.className="new-upload-progress";
    const label=document.createElement("p");label.setAttribute("role","status");
    upload={box,label,bar:new ProgressBar(()=>t("上传进度")),rate:new RateMeter()};box.append(label,upload.bar.el);
   }
   const u=upload;show(u.box);
   if(p.kind==="processing"){u.bar.set(null);setText(u.label,"上传完成，正在校验并解压项目…");return;}
   const speed=u.rate.sample(p.loaded);
   u.bar.set(p.total?p.loaded/p.total:null);
   setTextRender(u.label,()=>t("上传项目 {p0} / {p1}",{p0:fmtBytes(p.loaded),p1:fmtBytes(p.total)})+(speed>0?" · "+fmtBytes(Math.round(speed))+"/s":"")+(p.total?" · "+Math.floor(p.loaded/p.total*100)+"%":""));
  },
  hide(){host.classList.add("hidden");host.replaceChildren();upload=undefined;git=undefined;},
 };
}

/** A single wizard owns receipt recovery, account checks and import requests. */
export function initWorkspaceCreation(){
 const lifetime=new AbortController(),owner=S.token,flow=new CreationFlow();
 const dialog=$<HTMLDialogElement>("dlg-new"),form=$<HTMLFormElement>("new-form"),fields=$<HTMLFieldSetElement>("new-fields"),core=$<HTMLFieldSetElement>("new-core");
 const account=$<HTMLSelectElement>("new-account"),git=$<HTMLSelectElement>("new-git-connection"),importConnection=$<HTMLSelectElement>("new-import-connection");
 const source=$<HTMLSelectElement>("new-source-kind"),directory=$<HTMLInputElement>("new-project-dir"),uploadMode=$<HTMLSelectElement>("new-upload-mode"),files=$<HTMLInputElement>("new-project-file"),repository=$<HTMLInputElement>("new-git-url");
 const submit=$<HTMLButtonElement>("new-ok"),close=$<HTMLButtonElement>("new-cancel"),refresh=$<HTMLButtonElement>("new-refresh"),pending=$<HTMLSelectElement>("new-pending");
 const options={signal:lifetime.signal};
 let controller:AbortController|undefined,epoch=0,busy=false,loading=false,loadFailed=false,accounts:Account[]=[],connections:GitConnection[]=[];
 let setup:OnboardingSnapshot|undefined;
 const current=()=>!lifetime.signal.aborted&&S.token===owner;
 const radios=[...form.querySelectorAll<HTMLInputElement>('input[name="agent"]')];
 const agent=()=>radios.find(r=>r.checked)!.value;
 // An account with its own model list starts workspaces on its own default.
 const newModel=(id:string,kind:string)=>setup?.accounts.find(a=>a.id===id)?.default_model||setup?.default_models[kind]||"";
 const ready=()=>accounts.some(a=>a.type===agent()&&a.id===account.value);
 const showError=(message:string)=>{$("new-error").textContent=message;$("new-error").classList.remove("hidden");};
 const validAccounts=(rows:Account[])=>{if(!Array.isArray(rows))throw new Error(t("读取账号列表失败，请刷新重试。"));return rows.filter(a=>a&&typeof a.id==="string"&&["claude","codex"].includes(a.type));};
 const setSpec=(restoreSource=true)=>{
  if(!flow.spec)return;
  $<HTMLInputElement>("new-name").value=flow.spec.name;
  for(const radio of radios)radio.checked=radio.value===flow.spec.agent;
  if(restoreSource){setSelectValue(source,flow.latest()?.kind||flow.spec.source||"empty");directory.value=flow.latest()?.directory||flow.spec.directory||"project";}
 };
 const render=()=>{
  if(!current())return;
  const locked=!!flow.requestID,exists=!!flow.view?.workspace_exists,last=flow.latest();
  const uncertain=last?.state==="uncertain"||last?.state==="running";
  const previous=locked?flow.spec!.account_id:account.value,available=new Set(accounts.map(a=>a.type));
  if(!locked&&accounts.length&&!available.has(agent())){const first=radios.find(r=>available.has(r.value));if(first)first.checked=true;}
  for(const radio of radios)radio.disabled=loading||busy||locked||!available.has(radio.value);
  account.replaceChildren();
  for(const a of accounts.filter(a=>a.type===agent())){const option=new Option(a.label||a.id,a.id);account.append(option);}
  if(locked&&!accounts.some(a=>a.id===previous))account.append(new Option(previous,previous));
  if([...account.options].some(o=>o.value===previous))setSelectValue(account,previous);else if(account.options.length)setSelectValue(account,account.options[0].value);
  const missing=!accounts.length&&!locked;
  fields.classList.toggle("hidden",missing);fields.disabled=loading||busy||loadFailed||missing||!!uncertain||!!flow.view?.busy||last?.state==="succeeded"||flow.view?.state==="complete";
  core.disabled=locked;
  $("new-setup").classList.toggle("hidden",!loading&&!missing&&!loadFailed);
  setTextRender($("new-setup-message"),()=>loading?t("正在读取可用账号…"):loadFailed?t("账号状态尚未确认，请刷新后再创建。"):S.role==="admin"?t("还没有可用账号，请先配置账号后再创建空间。"):t("当前没有授权账号，请联系管理员配置账号或调整使用范围。"));
  $("new-configure").classList.toggle("hidden",S.role!=="admin"||loading||loadFailed);
  refresh.disabled=loading||busy;close.disabled=busy;
  $("new-import-fields").classList.toggle("hidden",source.value==="empty");
  $("new-upload-fields").classList.toggle("hidden",source.value!=="upload");
  $("new-git-fields").classList.toggle("hidden",source.value!=="git");
  directory.required=source.value!=="empty";repository.required=source.value==="git"&&!locked;
  $("new-flow-actions").classList.toggle("hidden",!locked);
  $<HTMLButtonElement>("new-query").disabled=busy||loading;
  $<HTMLButtonElement>("new-inspect").disabled=busy||!exists;
  $<HTMLButtonElement>("new-review").disabled=busy||!!flow.view?.busy||!uncertain;
  $<HTMLButtonElement>("new-restart").disabled=busy||!!flow.view?.busy||!!uncertain;
  $("new-review").classList.toggle("hidden",!uncertain);
  const view=flow.view;
  const resources=view?.container_resources||setup?.container_resources;
  const model=view?.session.default_model||newModel(account.value,agent());
  renderCreationSummary($("new-config-summary"),view?.session.account_label||accounts.find(a=>a.id===account.value)?.label||account.value,model,resources);
  $("new-config-summary").classList.toggle("hidden",loading||missing||loadFailed);
  setTextRender($("new-progress"),()=>{
   if(!locked)return "";
   if(view?.state==="abandoned")return t("该创建已放弃或空间已删除，请明确开始新的创建。");
   if(view?.busy)return t("导入仍在执行，请稍后查询状态。");
   if(uncertain)return t("导入结果待核对，请先查看已有文件并确认。");
   if(!exists)return t("尚未确认空间创建结果，请查询状态后继续。");
   return t("已保留空间「{name}」，后续重试继续使用它。",{name:view!.session.name})+(last?.state==="succeeded"?" "+t("项目已导入，可直接打开空间。"):"");
  });
  actionButton(submit,()=>t(exists?"继续并打开空间":"创建并继续"),source.value==="git"?"git-clone":source.value==="upload"?"upload":"plus");
  submit.disabled=loading||busy||loadFailed||missing||!model||!validResources(resources)||!!uncertain||!!view?.busy||view?.state==="abandoned"||(!locked&&!ready());
  $("new-recover").classList.toggle("hidden",locked||!flow.index.length);
  // Native fieldsets affect :disabled without mutating the select itself.
  for(const select of form.querySelectorAll<HTMLSelectElement>("select"))setSelectValue(select,select.value);
 };
 const updateConnections=()=>{
  const old=importConnection.value;importConnection.replaceChildren(...connections.filter(c=>c.enabled).map(c=>new Option(c.label,c.id)));
  if(connections.some(c=>c.enabled&&c.id===(old||git.value)))setSelectValue(importConnection,old||git.value);
 };
 const load=async()=>{
  controller?.abort();const request=controller=new AbortController(),turn=++epoch,timer=setTimeout(()=>request.abort(),15000);
  loading=true;loadFailed=false;accounts=[];$("new-error").classList.add("hidden");render();
  try{
   const [rows,snapshot]=await Promise.all([api<Account[]>("/accounts",{signal:request.signal}),api<OnboardingSnapshot>("/onboarding",{signal:request.signal}),flow.initialize(request.signal)]);
   if(!current()||!dialog.open||turn!==epoch||request.signal.aborted)return;
   if(!snapshot.default_models||!validResources(snapshot.container_resources))throw new Error(t("无法读取创建配置，请刷新或更新服务端后重试。"));
   setup=snapshot;
   accounts=validAccounts(rows);setSpec();
   pending.replaceChildren(...flow.index.map(c=>new Option(c.request.name||c.session.name||c.request_id,c.request_id)));
   git.replaceChildren(new Option(t("不绑定"),""));
   try{
    const [cs,defaults]=await Promise.all([api<GitConnection[]>("/git/connections",{signal:request.signal}),api<{connection_id:string}>("/me/git/default",{signal:request.signal})]);
    if(!current()||!dialog.open||turn!==epoch||request.signal.aborted)return;
    connections=cs;for(const c of cs)if(c.enabled)git.append(new Option(c.label,c.id));
    const selected=flow.spec?.git_connection_id??defaults.connection_id;if([...git.options].some(o=>o.value===selected))setSelectValue(git,selected);
    updateConnections();
   }catch(error){connections=[];updateConnections();if(current()&&dialog.open)showError(t("读取 Git 连接失败，可选择不绑定后创建：")+(error as Error).message);}
  }catch(error){if(current()&&dialog.open&&turn===epoch){loadFailed=true;showError((error as Error).message);}}
  finally{clearTimeout(timer);if(current()&&dialog.open&&turn===epoch){loading=false;render();}}
 };
 const showWorkspace=async(session:Session,tab:string)=>{
  dialog.close();await refreshAll();if(!current())return;
  if(!S.sessions.some(s=>s.id===session.id))S.sessions.push(session);
  location.hash=`#/sessions/${encodeURIComponent(session.id)}/${tab}`;
 };
 const perform=async(action:(signal:AbortSignal)=>Promise<void>)=>{
  if(!current()||busy||loading)return;
  controller?.abort();const request=controller=new AbortController();busy=true;render();btnBusy(submit,()=>t("处理中…"));$("new-error").classList.add("hidden");
  const timer=setTimeout(()=>request.abort(),180000);
  try{await action(request.signal);}
  catch(error){if(current()&&dialog.open){showError((error as Error).message);try{await flow.reconcile(AbortSignal.timeout(8000));}catch{/* keep pending pointer; next action queries again */}}}
  finally{clearTimeout(timer);busy=false;if(current()){btnDone(submit);render();}}
 };
 $("btn-new").addEventListener("click",()=>{if(current()&&!dialog.open&&!busy){dialog.showModal();void load();}},options);
 refresh.addEventListener("click",()=>void load(),options);
 $("new-configure").addEventListener("click",()=>{dialog.close();openSetupSettings("accounts");},options);
 close.addEventListener("click",()=>{if(!busy)dialog.close();},options);
 dialog.addEventListener("cancel",event=>{if(busy)event.preventDefault();},options);
 dialog.addEventListener("close",()=>{if(!dialog.open){++epoch;controller?.abort();}},options);
 for(const radio of radios)radio.addEventListener("change",render,options);
 account.addEventListener("change",render,options);
 source.addEventListener("change",render,options);
 uploadMode.addEventListener("change",()=>{files.value="";files.multiple=uploadMode.value!=="archive";files.accept=uploadMode.value==="archive"?".zip,.tar,.tgz,.tar.gz":"";files.toggleAttribute("webkitdirectory",uploadMode.value==="directory");},options);
 $("new-git-manage").addEventListener("click",()=>{dialog.close();emit("open-git","connections");},options);
 $("new-resume").addEventListener("click",()=>void perform(async signal=>{await flow.resume(pending.value,signal);setSpec();}),options);
 $("new-query").addEventListener("click",()=>void perform(async signal=>{await flow.reconcile(signal);}),options);
 $("new-review").addEventListener("click",()=>void perform(async signal=>{await flow.review(signal);}),options);
 $("new-restart").addEventListener("click",()=>void perform(async signal=>{await flow.abandon(signal);$<HTMLInputElement>("new-name").value="";files.value="";repository.value="";}),options);
 $("new-inspect").addEventListener("click",()=>{if(flow.view?.workspace_exists)void showWorkspace(flow.view.session,"files");},options);
 form.addEventListener("submit",event=>{
  event.preventDefault();if(busy||loading||loadFailed||!current()||!dialog.open||flow.view?.busy||["running","uncertain"].includes(flow.latest()?.state||"")||(!flow.requestID&&!ready()))return;
  const spec:WorkspaceCreateSpec={name:$<HTMLInputElement>("new-name").value.trim(),agent:agent(),account_id:account.value,git_connection_id:git.value,source:source.value as WorkspaceCreateSpec["source"],directory:source.value==="empty"?"project":directory.value.trim()||"project"};
  const expectedModel=flow.view?.session.default_model||newModel(spec.account_id,spec.agent);
  const expectedResources=resourceKey(flow.view?.container_resources||setup?.container_resources);
  void perform(async signal=>{
   if(!flow.requestID){
    accounts=validAccounts(await api<Account[]>("/accounts",{signal}));
    if(!ready()||!accounts.some(a=>a.id===spec.account_id&&a.type===spec.agent))throw new Error(t("账号列表已变化，请重新选择账号后创建。"));
   }
   // Validate the selected source before allocating a new workspace.
   if(spec.source==="upload"&&!files.files?.length&&!flow.view?.workspace_exists)throw new Error(t("请选择项目压缩包、文件或目录。"));
   if(spec.source==="git"&&(!importConnection.value||!repository.value.trim())&&!flow.view?.workspace_exists)throw new Error(t("请选择 Git 连接并填写仓库地址。"));
   const view=await flow.ensure(spec,signal);if(!current())return;setSpec(false);
   if(view.session.default_model!==expectedModel||resourceKey(view.container_resources)!==expectedResources){
    render();throw new Error(t("配置已变化，空间已保留。请核对上方的实际配置，再次点击继续。"));
   }
   if(view.state!=="complete"&&spec.source!=="empty"){
    const meter=importMeter();
    try{await flow.import(spec.source,directory.value.trim(),[...(files.files||[])],uploadMode.value,importConnection.value,repository.value.trim(),signal,p=>{if(current()&&dialog.open)meter.update(p);});}
    finally{meter.hide();}
   }
   const session=await flow.finish(signal);if(!current())return;
   emit("workspace-created",{session,source:spec.source,directory:spec.directory});
   $<HTMLInputElement>("new-name").value="";files.value="";repository.value="";
   await showWorkspace(session,spec.source==="empty"?"chat":spec.source==="git"?"changes":"files");
  });
 },options);
 return ()=>{
  lifetime.abort();++epoch;controller?.abort();accounts=[];setup=undefined;$("new-config-summary").replaceChildren();
  if(S.token!==owner)flow.clear();
  if(dialog.open)dialog.close();$<HTMLInputElement>("new-name").value="";files.value="";repository.value="";account.replaceChildren();git.replaceChildren();importConnection.replaceChildren();pending.replaceChildren();
  setSelectValue(source,"empty");directory.value="project";setSelectValue(uploadMode,"archive");files.multiple=false;files.accept=".zip,.tar,.tgz,.tar.gz";files.removeAttribute("webkitdirectory");
  $("new-error").textContent="";$("new-progress").textContent="";$("new-error").classList.add("hidden");$("new-setup-message").textContent="";
  for(const radio of radios){radio.checked=radio.value==="claude";radio.disabled=false;}
  fields.disabled=true;btnDone(submit);submit.disabled=true;close.disabled=false;
 };
}
