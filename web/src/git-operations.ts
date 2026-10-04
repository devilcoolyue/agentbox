import { htmlText as trHTML, setText, setTextRender, t as i18nText } from "./i18n.js";
import { createGitSurface, type GitSurface } from "./git-surface.js";
import { api } from "./api.js";
import { S, bus } from "./state.js";
import { fmtBytes, fmtTime } from "./util.js";
import type { GitOperationPage, GitLiveOperation } from "./types.js";
import { actionButton } from "./icons.js";

const phaseNames:Record<string,string>={get preparing() { return i18nText("准备与检查"); },get transferring() { return i18nText("传输数据"); },get checking() { return i18nText("核对工作区"); },get merging() { return i18nText("快进合并"); },get checkout() { return i18nText("检出文件"); },get publishing() { return i18nText("发布仓库目录"); }};
const names:Record<string,string>={get "review.list"() { return i18nText("查询 PR/MR"); },get "review.preview"() { return i18nText("预览 PR/MR"); },get "review.create"() { return i18nText("创建 PR/MR"); },get "connection.share"() { return i18nText("共享授权"); },get "remote.add"() { return i18nText("添加远程"); },get "remote.update"() { return i18nText("修改远程"); },get "remote.remove"() { return i18nText("删除远程"); },get commit() { return i18nText("本地提交"); },get discard() { return i18nText("丢弃改动"); },get "oauth.revoke"() { return i18nText("撤销 OAuth"); },get fetch() { return i18nText("获取"); },get pull() { return i18nText("快进拉取"); },get push() { return i18nText("推送"); },get "push-preview"() { return i18nText("推送预览"); },get clone() { return i18nText("克隆"); },get "connection.test"() { return i18nText("测试读取"); },get "connection.create"() { return i18nText("添加连接"); },get "connection.update"() { return i18nText("编辑连接"); },get "connection.delete"() { return i18nText("删除连接"); },get "binding.update"() { return i18nText("修改绑定"); },get "default.update"() { return i18nText("修改默认连接"); }};
const results:Record<string,string>={get success() { return i18nText("成功"); },get running() { return i18nText("执行中"); },get failed() { return i18nText("失败"); },get failed_unknown() { return i18nText("未确认，请核对远程"); },get cancelled_unknown() { return i18nText("已中断，请核对最终状态"); },get interrupted_unknown() { return i18nText("服务曾中断，请核对最终状态"); },get success_binding_failed() { return i18nText("克隆成功，绑定未保存"); }};
function progress(op:GitLiveOperation) {
  return i18nText("{p0} · {p1} · {p2} 秒 · 接收 {p3} / 发送 {p4}", { p0: String(names[op.operation] || op.operation), p1: String(op.cancel_requested?i18nText("正在取消"):phaseNames[op.phase] || op.phase), p2: String(Math.floor(op.elapsed_ms/1000)), p3: String(fmtBytes(op.received_bytes)), p4: String(fmtBytes(op.sent_bytes)) });
}
function requestID() {
  const bytes=crypto.getRandomValues(new Uint8Array(20));
  return [...bytes].map(b=>b.toString(16).padStart(2,"0")).join("");
}

/** Run a request with a separate cancellation handle. Do not abort the HTTP
 * response: it carries the final outcome, which can still be success if remote
 * accepted a push just before cancellation. Poll only while this request lives. */
export async function gitRequest<T>(path:string,body:unknown,host:HTMLElement):Promise<T> {
  const id=requestID(),token=S.token;
  const box=document.createElement("div");box.className="git-operation-progress";
  const label=document.createElement("p");label.className="field-hint";label.setAttribute("role","status");setText(label, "准备 Git 操作…");
  const cancel=document.createElement("button");cancel.type="button";cancel.className="btn btn-sm btn-danger";actionButton(cancel, () => i18nText("取消"), "close", () => i18nText("取消 Git 操作"));cancel.disabled=true;
  box.append(label,cancel);host.append(box);
  let ended=false,requested=false,timer:ReturnType<typeof setTimeout>|undefined;
  const cancelRequest=async()=>{
    if(ended||token!==S.token)return;
    cancel.disabled=true;
    try {await api(`/git/operations/${id}/cancel`,{method:"POST"});requested=true;setText(label, "正在取消并等待收尾；已到达远程的提交不会回滚。");}
    catch(e){if(!ended){label.textContent=(e as Error).message;cancel.disabled=false;}}
  };
  cancel.addEventListener("click",()=>void cancelRequest());
  const poll=async()=>{
    try {
      const page=await api<GitOperationPage>("/git/operations",{signal:AbortSignal.timeout(5000)});
      if(ended||token!==S.token)return;
      const op=page.active.find(op=>op.request_id===id);
      if(op){label.textContent=progress(op);cancel.disabled=requested||op.cancel_requested;}
    }catch{/* Keep the operation response authoritative if status polling fails. */}
    if(!ended&&token===S.token)timer=setTimeout(()=>void poll(),1000);
  };
  timer=setTimeout(()=>void poll(),150);
  try {return await api<T>(path,{method:"POST",headers:{"X-Git-Request-ID":id},body:JSON.stringify(body)});}
  catch(e){if(requested)throw new Error(i18nText("操作已请求取消；请刷新操作记录与远程状态确认最终结果。"));throw e;}
  finally{ended=true;clearTimeout(timer);box.remove();}
}

let activeDialog:GitSurface|null=null;
export function openGitOperations(){
  if(activeDialog)return;
  const token=S.token,d=createGitSurface(() => i18nText("Git 操作记录"), `
    <p class="field-hint"><span data-i18n="仅显示你发起的操作。中断或未确认不代表远程回滚，需核对仓库状态。活动操作每秒刷新。">${trHTML("仅显示你发起的操作。中断或未确认不代表远程回滚，需核对仓库状态。活动操作每秒刷新。")}</span></p>
    <p data-error role="alert" class="login-error"></p><div data-active></div><div data-history></div>
    <div class="dlg-actions"><button class="btn btn-sm" data-refresh data-icon="refresh" data-tip="${trHTML("回到最新 Git 操作记录")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;回到最新 Git 操作记录&quot;}"><span class="action-label" data-i18n="最新">${trHTML("最新")}</span></button><button class="btn btn-sm" data-more disabled data-icon="arrow-left" data-tip="${trHTML("查看更早的 Git 操作记录")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;查看更早的 Git 操作记录&quot;}"><span class="action-label" data-i18n="更早">${trHTML("更早")}</span></button></div>`);
  activeDialog=d;d.id="dlg-git-operations";
  const history=d.querySelector<HTMLElement>("[data-history]")!,live=d.querySelector<HTMLElement>("[data-active]")!,error=d.querySelector<HTMLElement>("[data-error]")!;
  const more=d.querySelector<HTMLButtonElement>("[data-more]")!;
  let next=0,generation=0,timer:ReturnType<typeof setTimeout>|undefined;
  async function load(before=0){
    const gen=++generation;more.disabled=true;
    try{
      const page=await api<GitOperationPage>("/git/operations"+(before?`?before=${before}`:""),{signal:AbortSignal.timeout(8000)});
      if(!d.open||token!==S.token||gen!==generation)return;
      error.textContent="";history.replaceChildren();next=page.next_before;
      for(const op of page.rows){
        const row=document.createElement("div");row.className="git-connection-row";
        const title=document.createElement("strong");title.textContent=`${names[op.operation]||op.operation} · ${results[op.result]||op.result}`;
        const info=document.createElement("p");info.className="field-hint";setTextRender(info, () => `${fmtTime(Date.parse(op.started_at))} · ${op.session_id || i18nText("个人连接")}${op.repo?" / "+op.repo:""}\n${op.target || ""}`);
        row.append(title,info);history.append(row);
      }
      if(!page.rows.length)setText(history, "暂无 Git 操作记录。");
      renderActive(page.active);more.disabled=!next;
    }catch(e){if(d.open&&gen===generation){error.textContent=(e as Error).message;more.disabled=!next;}}
  }
  function renderActive(operations:GitLiveOperation[]){
    live.replaceChildren();
    for(const op of operations){
      const row=document.createElement("div"),label=document.createElement("p"),cancel=document.createElement("button");
      row.className="git-connection-row";label.className="field-hint";label.textContent=progress(op);
      cancel.className="btn btn-sm btn-danger";actionButton(cancel, () => i18nText("取消"), "close", () => i18nText("取消 Git 操作"));cancel.disabled=op.cancel_requested;
      cancel.addEventListener("click",async()=>{cancel.disabled=true;try{await api(`/git/operations/${op.request_id}/cancel`,{method:"POST"});}catch(e){error.textContent=(e as Error).message;}});
      row.append(label,cancel);live.append(row);
    }
  }
  async function poll(){
    try{const page=await api<GitOperationPage>("/git/operations",{signal:AbortSignal.timeout(5000)});if(d.open&&token===S.token)renderActive(page.active);}catch{}
    if(d.open&&token===S.token)timer=setTimeout(()=>void poll(),1000);
  }
  d.querySelector("[data-refresh]")!.addEventListener("click",()=>void load());
  more.addEventListener("click",()=>void load(next));
  d.addEventListener("close",()=>{clearTimeout(timer);generation++;d.remove();activeDialog=null;});
  void load();timer=setTimeout(()=>void poll(),1000);
}
bus.addEventListener("signed-out",()=>activeDialog?.close());
