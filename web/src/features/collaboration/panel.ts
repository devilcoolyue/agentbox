import {createWorkflow} from "./workflow.js";
import {validView} from "./protocol.js";
import {createLiveOutput} from "./live.js";
import {renderFinalResult,collaborationMessageLabel} from "./final-result.js";
import {createEvidence} from "./evidence.js";
import {APIError} from "../../problems.js";
import {api} from "../../api.js";
import {S} from "../../state.js";
import {setAttr,setText,t} from "../../i18n.js";
import {enhanceSelects,setSelectValue} from "../../select.js";
import {roleSelector,workflowSelector,type Choice} from "./roles.js";
import type {Me} from "../../types.js";
import type {CollaborationBinding as Binding,CollaborationRoles as Roles,CollaborationConfiguration as Configuration} from "../../contracts/collaboration-v1.js";

import {isDirect,completionLabel} from "./strategy.js";
import {automaticRequestDigest,matchesAutomaticReceipt} from "./decision.js";
import {messageText,type Run,type View,type Event} from "./client.js";
interface Pending {scope:string;run?:string;request?:string;action?:string;request_fingerprint?:string}
function readPending(raw:string,scope:string):Pending|undefined {
 try {
  const p=JSON.parse(raw) as Pending;
  if(!p||typeof p!=="object"||p.scope!==scope||Object.keys(p).some(k=>!["scope","run","request","action","request_fingerprint"].includes(k)))return;
  if(p.request_fingerprint!==undefined&&(!p.request||typeof p.request_fingerprint!=="string"||!/^[a-f0-9]{64}$/.test(p.request_fingerprint)))return;
  const valid=(value:unknown)=>typeof value==="string"&&/^[a-f0-9]{32}$/.test(value);
  if(valid(p.request)&&p.run===undefined&&p.action===undefined||valid(p.run)&&valid(p.action)&&p.request===undefined)return p;
 }catch{/* Invalid tab-local recovery data must never become a request path. */}
}
const formatMessage=messageText;
const key="agentbox.collaboration.pending.v1";
const id=()=>crypto.randomUUID().replaceAll("-","");
function node<K extends keyof HTMLElementTagNameMap>(tag:K,text?:string){const e=document.createElement(tag);if(text)setText(e,text);return e;}
function button(text:string,fn:()=>void){const e=node("button",text);e.type="button";e.className="btn btn-sm";e.addEventListener("click",fn);return e;}
function field(text:string,input:HTMLElement){const label=node("label");label.className="collaboration-field";label.append(node("span",text),input);return label;}


export function initCollaboration(){
 const life=new AbortController(),owner=S.token;
 const entry=document.getElementById("btn-collaboration") as HTMLButtonElement|null;
 if(entry)entry.hidden=true;
 let returnFocus:HTMLElement|null=null;
 const stateLabels:Record<string,string>={accepted:"已接收",preparing:"准备项目",planning:"规划中",awaiting_plan:"等待确认计划",executing:"执行中",waiting_answer:"正在答疑",waiting_user:"等待你的答复",reviewing:"评审中",paused:"已暂停",cancelling:"正在取消",cancelled:"已取消",completed:"评审通过，结果待应用",failed:"任务失败",uncertain:"结果待核对",deleted:"已删除"};
 let dialog:HTMLDialogElement|undefined,scope="",choices:Choice[]=[],view:View|undefined,pending:Pending|undefined,timer:number|undefined,epoch=0,busy=false;
 let main:ReturnType<typeof roleSelector>,worker:ReturnType<typeof roleSelector>,status:HTMLParagraphElement,history:HTMLElement,runs:HTMLSelectElement,source:HTMLSelectElement,repository:HTMLInputElement,goal:HTMLTextAreaElement,auto:HTMLInputElement;
 let stepTime:HTMLInputElement,totalTime:HTMLInputElement,waitTime:HTMLInputElement;
 let repairs:HTMLInputElement,steps:HTMLInputElement,budget:HTMLInputElement,answer:HTMLTextAreaElement,questions:HTMLSelectElement,actions:HTMLElement,form:HTMLFormElement,verification:HTMLInputElement;
 let eventRun="",eventLog:Event[]=[];
 let taskList:HTMLElement,taskSearch:HTMLInputElement,taskTitle:HTMLElement,taskBadge:HTMLElement,metrics:HTMLElement,recovery:HTMLElement,submit:HTMLButtonElement,project:HTMLDetailsElement,overview:HTMLElement,activity:HTMLElement,activityTitle:HTMLElement,emptyHistory:HTMLElement;
 let feedback:HTMLElement,adjustProject:HTMLButtonElement;
 let settings:HTMLDetailsElement,teamSummary:HTMLElement,toolbar:HTMLElement,focusPanel:HTMLElement,focusTitle:HTMLElement,focusText:HTMLElement,more:HTMLDetailsElement,secondaryActions:HTMLElement,configActions:HTMLElement;
 let runList:Run[]=[],historyFilter="",listSignature="",actionSignature="",activeTab="progress",finalSignature="";
 let finalHost:HTMLElement,finalEmpty:HTMLElement,workspaceBody:HTMLElement,outputPage:HTMLElement,resultPage:HTMLElement,recordsPage:HTMLElement,setupPage:HTMLElement,setupButton:HTMLButtonElement;
 const strategy=workflowSelector();let workflowSupported=false;let swap:HTMLButtonElement,autoField:HTMLElement,roleBoxes:HTMLElement;
 let workflow:ReturnType<typeof createWorkflow>;
 const tabs:HTMLButtonElement[]=[];
 const selectRun=(runID:string)=>{if(busy)return;setSelectValue(runs,runID);runs.dispatchEvent(new Event("change",{bubbles:true}));};
 const renderList=()=>{
  if(!taskList)return;const signature=JSON.stringify([runList.map(r=>[r.id,r.state,r.request?.text]),view?.run.id,taskSearch.value,t("还没有协作任务")]);if(signature===listSignature)return;listSignature=signature;const scroll=taskList.scrollTop;taskList.replaceChildren();const query=taskSearch.value.trim().normalize("NFKC").toLocaleLowerCase();
  for(const r of runList){const title=r.request?.text||r.id;if(query&&!title.normalize("NFKC").toLocaleLowerCase().includes(query))continue;
   const b=button("",()=>selectRun(r.id));b.className="collaboration-task";b.dataset.run=r.id;b.setAttribute("aria-current",String(view?.run.id===r.id));
   const name=node("strong"),meta=node("span",(r.state==="completed"?completionLabel({run:r}):stateLabels[r.state])||"未知状态");name.textContent=title;b.title=title;b.append(name,meta);taskList.append(b);
  }
  if(!taskList.childElementCount)taskList.append(node("p",query?"没有匹配的任务":"还没有协作任务"));taskList.scrollTop=scroll;
 };
 const setTab=(tab:string)=>{activeTab=tab;if(tab!=="overview")workflow?.close();if(!workspaceBody)return;overview.hidden=!view||tab!=="overview";outputPage.hidden=!view||tab!=="activity";resultPage.hidden=!view||tab!=="result";recordsPage.hidden=!view||tab!=="records";setupPage.hidden=!!view&&tab!=="progress";focusPanel.hidden=!view||view.run.state==="completed";finalHost.hidden=!view||view.run.state!=="completed";finalEmpty.hidden=view?.run.state==="completed";live.box.hidden=!view||view.run.state==="deleted";setupButton.setAttribute("aria-pressed",String(tab==="progress"));for(const b of tabs){const active=b.dataset.tab===tab;b.setAttribute("aria-selected",String(active));b.setAttribute("aria-pressed",String(active));b.tabIndex=active||tab==="progress"&&b.dataset.tab==="overview"?0:-1;if(b.dataset.tab==="result")b.dataset.ready=String(view?.run.state==="completed");}if(tab==="overview")workflow?.resume();};
 const filterHistory=(stage="")=>{
  historyFilter=stage;
  const titles:Record<string,string>={preparing:"准备项目",planning:"规划",executing:"执行",verification:"平台验证",reviewing:"评审",delivery:"交付"};
  if(activityTitle)setText(activityTitle,titles[stage]||"协作记录");
  const types:Record<string,string[]>={planning:["plan"],executing:["result","question","answer"],verification:["verification"],reviewing:["review"],delivery:[],preparing:[]};
  for(const item of history.children)(item as HTMLElement).hidden=!!stage&&!types[stage]?.includes((item as HTMLElement).dataset.kind||"");
  emptyHistory.hidden=Array.from(history.children).some(e=>!(e as HTMLElement).hidden);
 };
 const updateWorkspace=()=>{
  if(!taskTitle)return;const r=view?.run;taskTitle.textContent=r?.request?.text||t("你想完成什么？");setText(taskBadge,r?(r.state==="completed"?completionLabel({run:r}):stateLabels[r.state])||"未知状态":"草稿");taskBadge.dataset.state=r?.state||"draft";
  submit.hidden=!!view;recovery.hidden=!pending;metrics.textContent=view?t("已用 {p0} 步 · 费用 ${p1}",{p0:String(r?.steps_used||0),p1:(view.cost.cost_micro_usd/1e6).toFixed(6)})+(view.cost.partial?" · "+t("费用尚不完整"):""):t("选好项目，写下目标即可开始。");
  toolbar.hidden=!view;configActions.hidden=!view;more.hidden=!view||!secondaryActions.childElementCount;dialog!.dataset.mode=view?"run":"new";
  const hints:Record<string,string>={accepted:"任务已接收，正在准备项目。",preparing:"正在准备独立工作副本。",planning:"主 Agent 正在制定计划。",awaiting_plan:"确认计划后，Agent 将开始执行。",executing:"辅 Agent 正在执行任务，暂时不需要操作。",reviewing:"正在检查执行结果。",waiting_answer:"Agent 之间正在澄清问题。",waiting_user:"请回答下面的问题，任务才能继续。",completed:"结果已准备好，可以查看变更或下载。",uncertain:"需要先核对运行结果，再决定是否继续。",paused:"任务已暂停，可以继续或调整协作设置。",failed:"查看任务记录，了解失败原因。",cancelled:"任务已取消，已有费用和记录仍然保留。",cancelling:"正在停止任务，请稍候。",deleted:"任务已删除"};
  setText(focusTitle,r?.state==="awaiting_plan"?"请确认计划":r?.state==="waiting_user"?"需要你的答复":"当前进度");
  let message="";
  if(r?.state==="awaiting_plan"||r?.state==="waiting_user"){
   for(const e of [...eventLog].reverse()){if(e.type!=="message"||!e.body||typeof e.body!=="object")continue;const m=e.body as {id:string;type:string;body:unknown};if(r.state==="waiting_user"?m.id===r.pending_question:m.type==="plan"){message=formatMessage(m.body);break;}}
  }
  if(message)focusText.textContent=message;else setText(focusText,r?.state==="paused"&&r.wait_reason==="execution_failed"?"请重新准备任务，将保留原需求和 Agent 配置并重新规划。":isDirect(view||{})&&r?.state==="executing"?"执行 Agent 正在处理任务，暂时不需要操作。":hints[r?.state||""]||"查看任务记录，了解当前进度。");
  main.active(!strategy.direct());swap.hidden=autoField.hidden=strategy.direct();roleBoxes.dataset.direct=String(strategy.direct());setText(worker.box.querySelector("legend")!,strategy.direct()?"执行 Agent":"辅 Agent：执行与测试");
  try{const executor=worker.read(),roles={coordinator:strategy.direct()?executor:main.read(),executor};workflow.render(view,eventLog);teamSummary.textContent=strategy.direct()?`${executor.agent_type==="claude"?"Claude Code":"Codex"} · ${t("直接执行")}`:`${roles.coordinator.agent_type==="claude"?"Claude Code":"Codex"} → ${roles.executor.agent_type==="claude"?"Claude Code":"Codex"}`;}catch{workflow.render(view,eventLog);setText(teamSummary,"请选择可用账号和模型");}
  setTab(activeTab);
  renderList();filterHistory(historyFilter);
 };
 const owns=()=>!life.signal.aborted&&S.token===owner;
 const evidence=createEvidence(owner,life.signal,()=>scope,owns);
 const live=createLiveOutput(owner,owns);
 const report=(e:unknown)=>{if(!owns()||!status)return;status.textContent=e instanceof Error?e.message:String(e);feedback.dataset.kind="error";adjustProject.hidden=!(e instanceof APIError&&e.problem.code==="collaboration_source_not_repository");
  if(!view)queueMicrotask(()=>{if(owns()&&!view&&dialog?.open){status.focus({preventScroll:true});feedback.scrollIntoView({block:"nearest"});}});
 };
 const savePending=(p:Pending)=>{sessionStorage.setItem(key,JSON.stringify(p));pending=p;if(recovery)recovery.hidden=false;};
 const clearPending=()=>{if(sessionStorage.getItem(key)===JSON.stringify(pending))sessionStorage.removeItem(key);pending=undefined;if(recovery)recovery.hidden=true;};
 const send=<T>(path:string,body:unknown)=>api<T>(path,{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify(body),signal:life.signal});
 const config=():Configuration=>{const executor=worker.read(),workflow=strategy.read();const c={...(workflow?{workflow}:{}),step_timeout_seconds:Number(stepTime.value),total_runtime_seconds:Number(totalTime.value),wait_timeout_seconds:Number(waitTime.value),verification_command:verification.value,roles:{coordinator:strategy.direct()?executor:main.read(),executor},auto_execute:strategy.direct()||auto.checked,max_repairs:Number(repairs.value),max_steps:Number(steps.value),budget_micro_usd:Math.round(Number(budget.value)*1e6)};if(![c.step_timeout_seconds,c.total_runtime_seconds,c.wait_timeout_seconds].every(Number.isSafeInteger)||c.step_timeout_seconds<10||c.step_timeout_seconds>1800||c.total_runtime_seconds<10||c.total_runtime_seconds>86400||c.wait_timeout_seconds<60||c.wait_timeout_seconds>2592000||!Number.isInteger(c.max_repairs)||c.max_repairs<0||c.max_repairs>20||!Number.isInteger(c.max_steps)||c.max_steps<1||c.max_steps>100||!Number.isSafeInteger(c.budget_micro_usd)||c.budget_micro_usd<0)throw new Error(t("请检查任务限额"));return c;};
 const showConfig=(c:Configuration)=>{strategy.configure(c.workflow,workflowSupported,true,true);stepTime.value=String(c.step_timeout_seconds||1800);totalTime.value=String(c.total_runtime_seconds||21600);waitTime.value=String(c.wait_timeout_seconds||604800);verification.value=c.verification_command||"";main.set(c.roles.coordinator);worker.set(c.roles.executor);auto.checked=c.auto_execute;repairs.value=String(c.max_repairs);steps.value=String(c.max_steps);budget.value=String(c.budget_micro_usd/1e6);};
 const withBusy=async(fn:()=>Promise<void>)=>{if(busy)return;busy=true;submit.disabled=true;form?.setAttribute("aria-busy","true");try{await fn();}catch(e){report(e);}finally{busy=false;submit.disabled=false;setText(submit,"开始协作");form?.removeAttribute("aria-busy");}};
 const requestAction=async(action:string,payload?:unknown)=>{
  if(!view||pending)throw new Error(t("请先查询待确认操作"));const run=view.run,actionID=id();
  const p:Pending={scope,run:run.id,action:actionID};savePending(p);
  await send(`/collaboration/runs/${run.id}/actions/${actionID}`,{scope,expected_revision:run.revision,action,...(payload?{payload}:{})});
  if(!owns())return;clearPending();await loadRun(run.id);
 };
 const refreshList=async()=>{const result=await api<{version:number;scope:string;runs:Run[]}>("/collaboration/runs",{signal:life.signal});if(!owns())return;if(result.version!==1||result.scope!==scope)throw new Error(t("登录状态已变化"));const selected=runs.value;const createOption=node("option","新建协作任务");createOption.value="";runs.replaceChildren(createOption);for(const r of result.runs){const text=r.request?.text||r.id;runs.append(new Option(text.slice(0,55),r.id));}setSelectValue(runs,selected);runList=result.runs;renderList();};
 const renderActions=()=>{
  const signature=view?`${view.run.id}:${view.run.revision}:${view.step?.id}:${view.step?.revision}`:"";if(signature&&signature===actionSignature&&actions.childElementCount)return;actionSignature=signature;
  actions.replaceChildren();secondaryActions.replaceChildren();configActions.replaceChildren();if(!view)return;const r=view.run;
  (r.state==="completed"?actions:secondaryActions).append(button("查看文件变更",()=>void withBusy(async()=>{setTab("records");await evidence.showDiff(r.id);})));
  const add=(label:string,action:string,payload?:()=>unknown)=>{const b=button(label,()=>void withBusy(()=>requestAction(action,payload?.())));if(["confirm_plan","resume","answer","repair_handoff"].includes(action))b.classList.add("btn-primary");(action==="configure"?configActions:action==="cancel"?secondaryActions:actions).append(b);};
  if(r.state==="completed"){const download=button("下载协作结果",()=>void withBusy(async()=>{const response=await fetch(`/api/collaboration/runs/${r.id}/download`,{headers:{Authorization:`Bearer ${owner}`},signal:life.signal});if(!response.ok)throw new Error(t("结果下载失败"));const blob=await response.blob();if(!owns())return;const url=URL.createObjectURL(blob),link=document.createElement("a");link.href=url;link.download=`collaboration-${r.id}.zip`;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}));download.classList.add("btn-primary");actions.prepend(download);}
  if(r.state==="awaiting_plan")add("确认计划并执行","confirm_plan",()=>({plan_version:r.plan_version}));
  if(["planning","executing","reviewing","waiting_answer","preparing"].includes(r.state))add("本步结束后暂停","pause");
  if(r.state==="paused"&&!["execution_failed","verification_required","delivery_required","verification_failed","instance_restored","artifacts_missing","result_reviewed","handoff_invalid","budget_limit","runtime_limit","usage_incomplete","repair_limit","context_limit"].includes(r.wait_reason||""))add("继续任务","resume");
  if(r.state==="paused"&&r.wait_reason==="execution_failed"){const configuration=view.configuration;const retry=button("重新准备任务",()=>void withBusy(()=>requestAction("configure",configuration)));retry.classList.add("btn-primary");actions.append(retry);}
  if(["accepted","paused","waiting_user","awaiting_plan"].includes(r.state))add("应用主辅配置","configure",config);
  if(r.state==="paused"&&r.wait_reason==="handoff_invalid")add("仅修复交接格式","repair_handoff");
  if(r.state==="uncertain"&&view.step){const step=view.step;actions.append(button("核对运行结果",()=>void withBusy(async()=>{setTab("records");await evidence.showReview(r.id,step,r.revision,()=>requestAction("review",{step_id:step.id,step_revision:step.revision,acknowledged:true}));})));}
  if(["paused","completed","cancelled","failed","deleted"].includes(r.state))secondaryActions.append(button("删除任务和副本",()=>{
   setTab("records");evidence.clear();const warning=node("p","删除会清理任务记录、工作副本、快照和参与者缓存，保留费用流水与请求标识。");const agree=node("input");agree.type="checkbox";const confirm=button("确认删除",()=>void withBusy(async()=>{await api(`/collaboration/runs/${r.id}`,{method:"DELETE",headers:{"Content-Type":"application/json"},body:JSON.stringify({scope,expected_revision:r.revision}),signal:life.signal});if(!owns())return;evidence.clear();view=undefined;eventLog=[];eventRun="";history.replaceChildren();actions.replaceChildren();source.disabled=repository.disabled=goal.disabled=false;setSelectValue(runs,"");await refreshList();goal.value="";project.open=true;historyFilter="";setTab("progress");updateWorkspace();status.textContent=t("任务已删除");}));confirm.disabled=true;agree.addEventListener("change",()=>{confirm.disabled=!agree.checked;},{signal:life.signal});evidence.box.append(warning,field("我确认删除任务副本",agree),confirm);
  }));
  if(!["completed","cancelled","deleted","failed"].includes(r.state))add("取消任务","cancel");
  if(r.state==="waiting_user")add("提交答复","answer",()=>({question_id:questions.value,text:answer.value}));
  if(actions.childElementCount===1)actions.firstElementChild!.classList.add("btn-primary");
 };
 const loadRun=async(runID:string,initial=false)=>{
  const generation=++epoch;
  const result=await api<View>(`/collaboration/runs/${runID}`,{signal:life.signal});
  if(!owns()||generation!==epoch)return;
  if(!validView(result,runID,scope))throw new Error(t("请检查任务记录"));
  const log=eventRun===runID?[...eventLog]:[];
  // Bounded catch-up; subsequent polls continue from the last durable sequence.
  for(let page=0;page<20;page++){
   let after=log.at(-1)?.seq||0;
   const events=await api<{version:number;scope:string;events:Event[]}>(`/collaboration/runs/${runID}/events?after=${after}`,{signal:life.signal});
   if(!owns()||generation!==epoch)return;
   if(events.version!==1||events.scope!==scope||!Array.isArray(events.events))throw new Error(t("登录状态已变化"));
   for(const event of events.events){if(!Number.isSafeInteger(event.seq)||event.seq!==after+1)throw new Error(t("请检查任务记录"));log.push(event);after=event.seq;}
   if(events.events.length<100)break;
  }
  eventRun=runID;eventLog=log;
  const previousState=view?.run.state;
  const previousQuestion=view?.run.id===runID?view.run.pending_question:undefined;
  view=result;if(initial){project.open=false;settings.open=false;more.open=false;setTab("overview");historyFilter="";if(result.run.state!=="deleted")showConfig(result.configuration);goal.value=result.run.request?.text||"";repository.value=result.run.request?.repository||".";setSelectValue(source,result.run.request?.source_session_id||"");}
  const resultSignature=`${result.run.id}:${result.run.revision}:${eventLog.at(-1)?.seq||0}`;
  if(resultSignature!==finalSignature){finalSignature=resultSignature;finalHost.replaceChildren(renderFinalResult(result,eventLog));}
  if(!initial&&(previousQuestion!==result.run.pending_question&&!!result.run.pending_question||previousState!==result.run.state&&["awaiting_plan","waiting_user","uncertain"].includes(result.run.state)))setTab("progress");
  status.textContent=result.run.wait_reason?t(({execution_failed:"Agent 运行环境启动失败",verification_required:"等待验证",delivery_required:"正在交付",runtime_limit:"累计执行时间已用完",wait_expired:"等待已到期",verification_failed:"验证失败",model_review_only:"仅模型评审",verification_not_requested:"未设置平台验证命令",usage_incomplete:"费用尚不完整",budget_limit:"费用预算已用完",step_limit:"已达步骤上限",repair_limit:"已达返工上限",context_limit:"必要上下文超过上限，请精简需求后新建任务。",handoff_invalid:"交接内容需要修复",user_paused:"用户已暂停",server_restarted:"服务重启后待核对",instance_restored:"恢复副本需重新配置",artifacts_missing:"备份不含项目文件",result_reviewed:"结果已核对，可重新配置"} as Record<string,string>)[result.run.wait_reason]||"请检查任务记录"):"";
  const priorScroll=history.scrollTop;history.replaceChildren();const questionOptions:HTMLOptionElement[]=[];
  for(const event of log){
   if(!event.body||typeof event.body!=="object")continue;
   if(event.type==="verification"||event.type==="verification_stopped"){
    const v=event.body as {state?:string;output?:string;snapshot_id?:string};
    const item=node("article"),label=node("strong","平台验证记录"),body=node("pre");
    const verificationStates:Record<string,string>={running:"验证中",completed:"验证通过",failed:"验证失败",uncertain:"验证结果待核对",not_requested:"未设置平台验证命令"};
    body.textContent=[t(verificationStates[v.state||""]||"未知状态"),v.snapshot_id||"",v.output||""].join("\n");item.dataset.kind="verification";item.append(label,body);history.append(item);continue;
   }
   if(event.type!=="message")continue;const m=event.body as {id:string;sender_role:string;type:string;body:unknown};
   const item=node("article"),label=node("strong");label.textContent=`${t(({coordinator:"主 Agent",executor:"辅 Agent",user:"用户"} as Record<string,string>)[m.sender_role]||m.sender_role)} · ${collaborationMessageLabel(m.type)}`;const body=node("pre");body.textContent=formatMessage(m.body);item.dataset.kind=m.type;item.dataset.role=m.sender_role;item.append(label,body);history.append(item);
   if(m.id===result.run.pending_question)questionOptions.push(new Option(formatMessage(m.body).slice(0,120)||m.id,m.id));
  }
  history.scrollTop=priorScroll;
  if(result.run.pending_question&&!questionOptions.length)questionOptions.push(new Option(result.run.pending_question,result.run.pending_question));
  questions.replaceChildren(...questionOptions);setSelectValue(questions,result.run.pending_question||"");
  if(previousQuestion!==result.run.pending_question)answer.value="";
  questions.parentElement!.hidden=true;answer.parentElement!.hidden=result.run.state!=="waiting_user";
  source.disabled=repository.disabled=goal.disabled=true;renderActions();const listed=runList.findIndex(r=>r.id===result.run.id);if(listed>=0)runList[listed]=result.run;updateWorkspace();
  if(dialog?.open){live.show(result,eventLog);setTab(activeTab);}else live.close();
 };
 const queryPending=async()=>{if(!pending)return;const p=pending;if(p.scope!==scope)throw new Error(t("登录状态已变化"));if(p.action){await api(`/collaboration/runs/${p.run}/actions/${p.action}`,{signal:life.signal});}if(!owns())return;await refreshList();await loadRun(p.run||p.request!,true);if(!owns())return;if(p.request_fingerprint&&(!view?.run.request||view.run.request_id!==p.request||await automaticRequestDigest(view.run.request)!==p.request_fingerprint))throw new Error(t("自动任务回执与原请求不一致，请保留待确认记录。"));if(!owns())return;setSelectValue(runs,view!.run.id);clearPending();};
 const abandonPending=async()=>{if(!pending)return;const p=pending;const url=p.action?`/collaboration/runs/${p.run}/actions/${p.action}/abandon`:`/collaboration/requests/${p.request}/abandon`;await send(url,{scope});if(!owns())return;clearPending();status.textContent=t("未接收操作已放弃");};
 const build=()=>{
  dialog=node("dialog");dialog.className="collaboration-dialog";dialog.setAttribute("aria-labelledby","collaboration-title");
  const head=node("div");head.className="dlg-head";const title=node("h2","协作任务");title.id="collaboration-title";const brand=node("span");brand.className="collaboration-brand";brand.textContent="AB /";const identity=node("div");identity.className="collaboration-identity";identity.append(brand,title);head.append(identity,button("关闭",()=>dialog?.close()));
  form=node("form");const body=node("div");body.className="dlg-body";status=node("p");status.setAttribute("role","status");status.tabIndex=-1;
  runs=node("select");source=node("select");for(const sess of S.sessions)source.append(new Option(sess.name,sess.id));setSelectValue(source,S.current?.id||source.options[0]?.value||"");
  repository=node("input");repository.value=".";repository.required=true;goal=node("textarea");goal.rows=4;goal.required=true;goal.maxLength=100000;
  main=roleSelector("主 Agent：规划与评审",choices,life.signal);worker=roleSelector("辅 Agent：执行与测试",choices,life.signal);
  if(choices.some(a=>a.type==="codex")){const a=choices.find(a=>a.type==="codex")!;worker.set({agent_type:"codex",account_id:a.id,model:a.default_model,effort:"",effort_control:""});}
  strategy.configure(undefined,workflowSupported);
  const roles=node("div");roleBoxes=roles;roles.className="collaboration-roles";roles.append(main.box,worker.box);
  repairs=node("input");steps=node("input");budget=node("input");for(const n of [repairs,steps,budget])n.type="number";repairs.min="0";repairs.max="20";repairs.value="2";steps.min="1";steps.max="100";steps.value="20";budget.min="0";budget.step="0.01";budget.value="0";
  stepTime=node("input");totalTime=node("input");waitTime=node("input");for(const n of [stepTime,totalTime,waitTime])n.type="number";stepTime.min="10";stepTime.max="1800";stepTime.value="1800";totalTime.min="10";totalTime.max="86400";totalTime.value="21600";waitTime.min="60";waitTime.max="2592000";waitTime.value="604800";
  auto=node("input");auto.type="checkbox";verification=node("input");verification.maxLength=8192;
  const limits=node("div");limits.className="collaboration-limits";limits.append(field("最多返工次数",repairs),field("最多模型步骤",steps),field("费用预算 USD（0 不限制）",budget),field("单步超时（秒）",stepTime),field("累计模型执行上限（秒）",totalTime),field("等待答复上限（秒）",waitTime));
  submit=node("button","开始协作");submit.type="submit";submit.className="btn btn-primary";
  actions=node("div");actions.className="collaboration-actions";history=node("div");history.className="collaboration-history";questions=node("select");answer=node("textarea");answer.rows=3;
  const nav=node("aside");nav.className="collaboration-nav";setAttr(nav,"aria-label","任务导航");
  const navHead=node("div");navHead.className="collaboration-nav-head";navHead.append(node("h3","全部任务"),button("刷新任务列表",()=>void withBusy(refreshList)));
  const newTask=button("新建协作任务",()=>selectRun(""));newTask.classList.add("btn-primary");
  taskSearch=node("input");taskSearch.type="search";setAttr(taskSearch,"placeholder","搜索任务");setAttr(taskSearch,"aria-label","搜索任务");taskSearch.addEventListener("input",renderList,{signal:life.signal});
  taskList=node("div");taskList.className="collaboration-task-list";
  const picker=field("任务列表",runs);picker.classList.add("collaboration-mobile-picker");nav.append(navHead,newTask,taskSearch,taskList,picker);
  const center=node("main");center.className="collaboration-center collaboration-workspace";
  const taskHead=node("header");taskHead.className="collaboration-task-head";taskBadge=node("span");taskBadge.className="collaboration-badge";taskTitle=node("h2");metrics=node("p");metrics.className="collaboration-metrics";taskHead.append(taskBadge,taskTitle,metrics);
  toolbar=node("div");toolbar.className="collaboration-viewbar";const viewSwitch=node("div");viewSwitch.className="collaboration-workspace-tabs";viewSwitch.setAttribute("role","tablist");setAttr(viewSwitch,"aria-label","协作过程");
  workspaceBody=node("div");workspaceBody.className="collaboration-workspace-body";
  const pages=new Map<string,HTMLElement>();for(const key of ["overview","activity","result","records","progress"]){const page=node("section");page.className="collaboration-workspace-pane";page.dataset.pane=key;page.id=`collaboration-panel-${key}`;if(key!=="progress"){page.setAttribute("role","tabpanel");page.setAttribute("aria-labelledby",`${page.id}-tab`);}pages.set(key,page);workspaceBody.append(page);}
  overview=pages.get("overview")!;outputPage=pages.get("activity")!;resultPage=pages.get("result")!;recordsPage=pages.get("records")!;setupPage=pages.get("progress")!;
  for(const [key,label] of [["overview","过程"],["activity","步骤输出"],["result","最终结果"],["records","文件与记录"]]){const b=button(label,()=>{filterHistory();setTab(key);});b.dataset.tab=key;b.id=`collaboration-panel-${key}-tab`;b.setAttribute("role","tab");b.setAttribute("aria-controls",`collaboration-panel-${key}`);tabs.push(b);viewSwitch.append(b);}
  viewSwitch.addEventListener("keydown",event=>{const current=tabs.findIndex(b=>b.dataset.tab===activeTab);let next=current;if(event.key==="ArrowRight")next=(current+1)%tabs.length;else if(event.key==="ArrowLeft")next=(current+tabs.length-1)%tabs.length;else if(event.key==="Home")next=0;else if(event.key==="End")next=tabs.length-1;else return;event.preventDefault();setTab(tabs[next].dataset.tab!);tabs[next].focus();},{signal:life.signal});
  setupButton=button("协作设置",()=>setTab("progress"));setupButton.classList.add("collaboration-workspace-settings");toolbar.append(viewSwitch,setupButton);
  workflow=createWorkflow(entry=>{if(entry.stepID){setTab("activity");live.select(entry.stepID);}});overview.append(workflow.box);
  project=node("details");project.className="collaboration-project";project.open=true;project.append(node("summary","任务需求与来源"));
  const sourceRow=node("div");sourceRow.className="collaboration-source";sourceRow.append(field("源工作空间",source));
  project.append(field("任务需求",goal),sourceRow);
  activity=node("section");activity.className="collaboration-activity";const activityHead=node("div");activityHead.className="collaboration-section-head";activityTitle=node("h3","协作记录");activityTitle.tabIndex=-1;activityHead.append(activityTitle,button("全部记录",()=>filterHistory()));
  emptyHistory=node("p","暂无对应记录，任务推进后会显示在这里。");emptyHistory.className="collaboration-empty";activity.append(activityHead,history,emptyHistory);
  recovery=node("div");recovery.className="collaboration-recovery";recovery.hidden=!pending;recovery.append(node("p","有操作尚未确认，请先查询结果。"),button("查询待确认操作",()=>void withBusy(queryPending)),button("放弃未接收操作",()=>void withBusy(abandonPending)));
  focusPanel=node("section");focusPanel.className="collaboration-focus";focusTitle=node("h3");focusText=node("pre");focusPanel.append(focusTitle,focusText);
  finalHost=node("div");finalHost.className="collaboration-final-host";finalEmpty=node("p","最终结果将在任务完成后显示。");finalEmpty.className="collaboration-workspace-empty";
  more=node("details");more.className="collaboration-more";secondaryActions=node("div");secondaryActions.className="collaboration-actions";more.append(node("summary","更多操作"),secondaryActions);configActions=node("div");configActions.className="collaboration-actions";
  const inspector=node("aside");inspector.className="collaboration-inspector";const teamHead=node("div");teamHead.className="collaboration-section-head";teamHead.append(node("h3","Agent 团队"),swap=button("交换主辅",()=>{try{const a=main.read(),b=worker.read();main.set(b);worker.set(a);updateWorkspace();}catch(e){report(e);}}));
  const teamHint=node("p","主辅角色可自由交换，模型独立设置。");teamHint.className="collaboration-hint";
  const advanced=node("details");advanced.className="collaboration-advanced";advanced.append(node("summary","预算与运行限制"),limits);
  autoField=field("规划后自动执行",auto);
  inspector.append(teamHead,teamHint,strategy.box,roles,field("仓库相对路径",repository),field("验证命令（可选）",verification),autoField,advanced,configActions);
  settings=node("details");settings.className="collaboration-settings";const settingsTitle=node("summary");teamSummary=node("span");settingsTitle.append(node("span","协作设置"),teamSummary);settings.append(settingsTitle,inspector);
  feedback=node("div");feedback.className="collaboration-feedback";adjustProject=button("调整项目目录",()=>{setTab("progress");settings.open=true;repository.focus();});adjustProject.hidden=true;feedback.append(status,adjustProject);
  const commandbar=node("div");commandbar.className="collaboration-commandbar";commandbar.append(actions,more);
  outputPage.append(live.box);resultPage.append(finalHost,finalEmpty);recordsPage.append(evidence.box,activity);setupPage.append(focusPanel,field("待答问题",questions),field("答复",answer),project,settings,submit);
  center.append(taskHead,toolbar,feedback,recovery,commandbar,workspaceBody);
  inspector.addEventListener("change",()=>{updateWorkspace();},{signal:life.signal});
  body.append(nav,center);
  form.addEventListener("invalid",event=>{status.textContent=t("请填写必填项或修正标记的设置。");feedback.dataset.kind="error";setTab("progress");let e=(event.target as HTMLElement).parentElement;while(e&&e!==form){if(e instanceof HTMLDetailsElement)e.open=true;e=e.parentElement;}},{capture:true,signal:life.signal});
  const foot=node("div","协作结果保留在独立副本，完成后需另外应用。费用预算在回合结束后检查。");foot.className="dlg-actions";form.append(body,foot);dialog.append(head,form);document.body.append(dialog);questions.parentElement!.hidden=answer.parentElement!.hidden=true;enhanceSelects(dialog);setTab("progress");updateWorkspace();
  runs.addEventListener("change",()=>void withBusy(async()=>{epoch++;view=undefined;evidence.clear();live.close();workflow.render(undefined);if(runs.value){await loadRun(runs.value,true);}else{strategy.configure(undefined,workflowSupported);source.disabled=repository.disabled=goal.disabled=false;goal.value="";eventRun="";eventLog=[];questions.parentElement!.hidden=answer.parentElement!.hidden=true;history.replaceChildren();actions.replaceChildren();status.textContent="";historyFilter="";project.open=true;settings.open=false;setTab("progress");updateWorkspace();}}),{signal:life.signal});
  form.addEventListener("submit",event=>{event.preventDefault();void withBusy(async()=>{
   if(view||pending)throw new Error(t("请选择新建任务或先查询待确认操作"));
   if(!source.value)throw new Error(t("请先选择源工作空间"));
   const configuration=config();const frozen={scope,source_session_id:source.value,repository:repository.value,text:goal.value,...configuration};
   const controls=Array.from(form.elements).filter((e):e is HTMLInputElement|HTMLSelectElement|HTMLTextAreaElement|HTMLButtonElement=>e instanceof HTMLInputElement||e instanceof HTMLSelectElement||e instanceof HTMLTextAreaElement||e instanceof HTMLButtonElement).map(e=>({e,disabled:e.disabled}));
   for(const {e} of controls)e.disabled=true;
   feedback.dataset.kind="progress";adjustProject.hidden=true;setText(submit,"正在检查项目…");setText(status,"正在检查源项目，请稍候…");
   try{
    const preflight=await api<{version:number;scope:string;baseline:string}>("/collaboration/preflight",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({scope,source_session_id:frozen.source_session_id,repository:frozen.repository,roles:configuration.roles,...(configuration.workflow?{workflow:configuration.workflow}:{})}),signal:life.signal});
    if(!owns())return;if(preflight.scope!==scope||preflight.version!==1)throw new Error(t("登录状态已变化"));
    const request=id(),input={...frozen,baseline:preflight.baseline};const fingerprint=configuration.workflow?await automaticRequestDigest(input):undefined;if(!owns())return;savePending({scope,request,...(fingerprint?{request_fingerprint:fingerprint}:{})});setText(submit,"正在创建任务…");setText(status,"项目检查通过，正在创建协作任务…");
    const result=await send<{run:Run}>(`/collaboration/runs/${request}`,input);
    if(!owns())return;if(!matchesAutomaticReceipt(result,input,request))throw new Error(t("自动任务回执与原请求不一致，请保留待确认记录。"));clearPending();await refreshList();setSelectValue(runs,result.run.id);await loadRun(result.run.id,true);feedback.dataset.kind="";
   }finally{
    for(const {e,disabled} of controls)e.disabled=disabled;
    source.disabled=repository.disabled=goal.disabled=!!view;
   }
  });},{signal:life.signal});
  dialog.addEventListener("close",()=>{epoch++;live.close();workflow?.close();if(timer)clearTimeout(timer);timer=undefined;if(returnFocus?.isConnected)returnFocus.focus();else document.getElementById("chat-input")?.focus();returnFocus=null;},{signal:life.signal});
 };
 const openRun=(event:globalThis.Event)=>{const raw=(event as CustomEvent<unknown>).detail;const run=typeof raw==="string"?raw:(raw&&typeof raw==="object"&&"id" in raw?String((raw as {id:unknown}).id):"");if(run&&!/^[a-f0-9]{32}$/.test(run))return;void(async()=>{try{await ready;if(!owns()||!scope)return;if(!dialog)build();if(!dialog!.open){returnFocus=document.activeElement instanceof HTMLElement?document.activeElement:null;dialog!.showModal();}await withBusy(async()=>{if(view?.run.id!==run){epoch++;view=undefined;live.close();workflow.render(undefined);evidence.clear();setTab("overview");}await refreshList();if(run){setSelectValue(runs,run);await loadRun(run,true);}else{setSelectValue(runs,"");runs.dispatchEvent(new Event("change"));}});poll();}catch(e){report(e);}})();};
 window.addEventListener("collaboration-open",openRun,{signal:life.signal});
 const poll=()=>{if(!owns()||!dialog?.open)return;timer=window.setTimeout(async()=>{if(view&&!busy)try{await loadRun(view.run.id);}catch(e){report(e);}poll();},2000);};
 entry?.addEventListener("click",()=>void(async()=>{try{if(!dialog)build();returnFocus=entry;dialog!.showModal();await withBusy(async()=>{await refreshList();if(pending)await queryPending();});poll();}catch(e){report(e);}})(),{signal:life.signal});
 const ready=(async()=>{try{const me=await api<Me&{collaboration_protocol?:number}>("/me",{signal:life.signal});if(!owns()||me.collaboration_protocol!==1)return;const options=await api<{version:number;scope:string;workflow_version?:number;accounts:Choice[]}>("/collaboration/options",{signal:life.signal});if(!owns()||options.version!==1||!/^[a-f0-9]{64}$/.test(options.scope))return;scope=options.scope;choices=options.accounts;workflowSupported=options.workflow_version===1;const raw=sessionStorage.getItem(key);if(raw){pending=readPending(raw,scope);}if(entry)entry.hidden=false;}catch{/* Ordinary single-agent UI remains available if collaboration is disabled. */}})();
 return ()=>{epoch++;live.close();workflow?.dispose();life.abort();if(timer)clearTimeout(timer);dialog?.remove();if(entry)entry.hidden=true;if(!S.token&&pending){try{clearPending();}catch{ /* best effort during sign-out */ }}};
}
