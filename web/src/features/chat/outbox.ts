import {readChatReceipt} from "../../contracts/chat.js";
import {S} from "../../state.js";
import {api} from "../../api.js";
import {$,askConfirm} from "../../util.js";
import {actionButton,svgIcon} from "../../icons.js";
import {t,setTextRender,setAttrRender} from "../../i18n.js";
import {APIError,problemMessages} from "../../problems.js";
import {keyboardInput} from "../../modality.js";
import type {ChatRequestInput,ChatRequestReceipt,ChatRequestResponse,ChatRequestList} from "../../types.js";
import type {Draft} from "./draft-store.js";
import {OutboxStore,receiptActive,draftFromRequest,type Outgoing} from "./outbox-store.js";

type Tone="busy"|"attention"|"error"|"done"|"quiet";
type Look={title:string;reason:string;tone:Tone;icon:string};
// Why a turn's outcome is unknown, by receipt code. Shown so "review" is not a
// riddle: each names what the server could not confirm.
const unknownReasons:Record<string,string>={server_restarted:"服务重启时任务正在运行",server_upgraded:"服务升级时任务正在运行",execution_incomplete:"没有收到任务结束信号",history_write_failed:"对话记录没有完整保存",usage_write_failed:"用量没有记账成功",receipt_write_failed:"任务状态没有保存成功",chat_failed:"任务报错结束"};
function look(state:Outgoing["state"],code="",merged=false):Look{
 switch(state){
  case "queued":return {title:"排队中",reason:"这一轮结束后自动执行",tone:"busy",icon:"clock"};
  case "unconfirmed":return {title:"未收到发送确认",reason:"可能没有发出去；重试会沿用原编号，不会重复执行",tone:"attention",icon:"alert"};
  case "accepted":return {title:"已发送",reason:"",tone:"busy",icon:""};
  case "starting":return {title:"正在准备",reason:"",tone:"busy",icon:""};
  case "running":return {title:"正在执行",reason:"",tone:"busy",icon:""};
  case "completed":return merged?{title:"已插入当前任务",reason:"当前步骤结束后生效",tone:"done",icon:"send"}:{title:"已完成",reason:"",tone:"done",icon:"check"};
  // A failure after running may have changed files; one before never ran.
  case "failed":return code==="turn_failed"?{title:"执行出错",reason:"可能已改动部分文件，请查看对话和文件",tone:"error",icon:"x-circle"}:{title:"未执行",reason:problemMessages[code]?.[0]||"没有执行，可以修改后重新发送",tone:"error",icon:"x-circle"};
  case "interrupted":return {title:"已中断",reason:"",tone:"quiet",icon:"stop"};
  case "uncertain":return {title:"结果不确定",reason:unknownReasons[code]||"无法确认任务做到了哪一步",tone:"attention",icon:"alert"};
  case "reviewed":return {title:"已查看结果",reason:unknownReasons[code]||"",tone:"quiet",icon:"check"};
  case "abandoned":return code==="withdrawn"?{title:"已撤回",reason:"没有执行",tone:"quiet",icon:"undo"}:{title:"已放弃",reason:"没有执行",tone:"quiet",icon:"close"};
  case "deleted":return {title:"已删除",reason:"",tone:"quiet",icon:"close"};
 }
}
const settled=(state:Outgoing["state"])=>state!=="unconfirmed"&&state!=="queued"&&!receiptActive(state);
const identifier=()=>Array.from(crypto.getRandomValues(new Uint8Array(16)),b=>b.toString(16).padStart(2,"0")).join("");
const key=(session:string,id:string)=>session+"/"+id;
const delegationEnvelope=(input:ChatRequestInput)=>input.delegation?[input.delegation.version,input.delegation.workers.map(w=>[w.account_id,w.model]),input.delegation.jev]:null;
const envelope=(input:ChatRequestInput)=>JSON.stringify([input.scope,input.thread_id,input.text,input.model,input.effort,input.effort_control,input.attachments||[],delegationEnvelope(input),input.suggestions===true,input.questions===true,input.delivery||"",input.tasks===true]);

function receipt(value:unknown,session:string,id?:string):ChatRequestReceipt {
 try { return readChatReceipt(value,session,id); }
 catch { throw Error(t("消息状态响应无效，请重新查询。")); }
}

/** Owns only delivery/recovery. Polling and socket events never submit a task. */
export function initChatOutbox(hooks:{changed:()=>void;connected:()=>boolean;move:(expected:Draft)=>void;restore:(draft:Draft)=>boolean;followed:(how:"queued"|"steered")=>void}){
 const owner=S.token,scope=S.chatScope,protocol=S.chatProtocol,lifetime=new AbortController();
 const storage=new OutboxStore(localStorage,scope,S.draftScope);
 const enabled=protocol===1;
 const unsupported=protocol!==0&&protocol!==1;
 let active=true,context:{session:string;thread:string}|undefined,epoch=0,viewAbort=new AbortController();
 let loading=false,queryFailed=false,scopeChanged=false,refreshing=false,again=false,notice="",noticeError=false,noticeTranslated=false;
 const dismissed=new Set<string>(),fullText=new Set<string>();
 let listSignature="",expanded=false;
 const showDetails=(open:boolean)=>{expanded=open;$("chat-delivery-toggle").setAttribute("aria-expanded",String(open));$("chat-delivery-list").classList.toggle("hidden",!open);};
 let confirmation=false;
 let socketState:"running"|"idle"|undefined;
 const confirm=async(message:string)=>{confirmation=true;try{return await askConfirm(()=>t(message));}finally{confirmation=false;}};
 const known=new Map<string,ChatRequestReceipt>(),views=new Map<string,Outgoing>(),busy=new Set<string>(),missing=new Set<string>();
 const owns=()=>{try{return active&&S.token===owner&&localStorage.getItem("agentbox_token")===owner;}catch{return active&&S.token===owner;}};
 const current=(session:string,generation:number)=>owns()&&context?.session===session&&epoch===generation;
 const endpoint=(session:string,id?:string)=>`/sessions/${encodeURIComponent(session)}/chat/requests${id?"/"+encodeURIComponent(id):""}`;
 const rows=()=>{
  const result=new Map<string,Outgoing>();
  for(const row of views.values())if(row.session===context?.session)result.set(row.id,row);
  for(const row of storage.rows(context?.session))if(row.session===context?.session)result.set(row.id,row);
  for(const row of result.values()){const c=known.get(key(row.session,row.id));if(c&&c.revision>=row.revision){row.revision=c.revision;row.state=c.state;}}
  return [...result.values()].filter(row=>row.thread===context?.thread||row.state==="unconfirmed"||receiptActive(row.state)||row.state!=="completed"&&(row.draft||row.input)).sort((a,b)=>b.created-a.created);
 };
 // A follow-up may go while a turn runs; never past an unconfirmed send or an unreviewed outcome.
 const blocked=(follow=false)=>!owns()||unsupported||enabled&&(!scope||!context||loading||queryFailed||scopeChanged||rows().some(r=>r.state==="unconfirmed"||(follow?r.state==="uncertain":receiptActive(r.state))));
 const say=(message:string,error=false)=>{notice=message;noticeError=error;noticeTranslated=false;render();};
 const apply=(c:ChatRequestReceipt)=>{
  const id=key(c.session_id,c.request_id),old=known.get(id);
  if(old&&old.revision>c.revision)return;
  const original=storage.rows(c.session_id).find(r=>r.id===c.request_id);
  if(original&&original.revision>c.revision)return;
  if(original&&(c.thread_id&&c.thread_id!==original.thread||original.input&&c.request&&envelope(original.input)!==envelope(c.request)))throw Error(t("消息状态响应无效，请重新查询。"));
  known.set(id,c);missing.delete(id);storage.observe(c);
  if(dismissed.has(id))return;
  if(!views.has(id))views.set(id,{version:1,id:c.request_id,session:c.session_id,thread:c.thread_id,created:Date.parse(c.created_at)||Date.now(),input:null,draft:null,revision:c.revision,state:c.state});
  const local=storage.rows(c.session_id).find(r=>r.id===c.request_id);
  if(local)views.set(id,local);
  if(c.state==="completed")storage.remove(c.request_id,c.session_id);
 };
 const response=(value:ChatRequestResponse,session:string,id:string)=>{
  if(value?.version!==1)throw Error(t("消息状态响应无效，请重新查询。"));
  const c=receipt(value.receipt,session,id);apply(c);return c;
 };
 async function query(row:Outgoing,signal:AbortSignal=lifetime.signal){
  const id=key(row.session,row.id);
  try{
   const result=await api<ChatRequestResponse>(endpoint(row.session,row.id),{signal:AbortSignal.any([signal,AbortSignal.timeout(12000)])});
   if(!owns()||signal.aborted)return;
   return response(result,row.session,row.id);
  }catch(error){
   if(!owns()||signal.aborted)return;
   if(error instanceof APIError&&error.status===404&&error.problem.code==="chat_request_not_found"){missing.add(id);return;}
   throw error;
  }
 }
 async function refresh(){
  if(!enabled||!owns()||!context||document.hidden)return;
  if(refreshing){again=true;return;}
  refreshing=true;const {session}=context,generation=epoch,signal=viewAbort.signal;
  try{
   const result=await api<ChatRequestList>(endpoint(session)+"?pending_only=1",{signal:AbortSignal.any([signal,AbortSignal.timeout(12000)])});
   if(!current(session,generation))return;
   if(result?.version!==1||typeof result.scope!=="string"||!Array.isArray(result.requests)||result.requests.length>50||!Object.hasOwn(result,"pending")||result.pending!==null&&(!result.pending||typeof result.pending!=="object"||Array.isArray(result.pending)))throw Error(t("消息状态响应无效，请重新查询。"));
   if(result.scope!==scope){scopeChanged=true;throw Error(t("消息所属的实例或登录身份已变化"));}
   if(result.pending)apply(receipt(result.pending,session));
   // The queue: every page shows it; one that left the list ran or was withdrawn.
   const listed=new Set<string>();
   for(const value of result.requests){const c=receipt(value,session);listed.add(c.request_id);apply(c);}
   // Query saved IDs even when the list has no active turn: a lost response can
   // belong to a task that already completed or has not arrived yet.
   const candidates=new Map(storage.rows(session).map(row=>[row.id,row]));
   for(const row of views.values())if(row.session===session&&receiptActive(known.get(key(session,row.id))?.state||row.state))candidates.set(row.id,row);
   for(const [id,c] of known)if(c.session_id===session&&c.state==="queued"&&!listed.has(c.request_id)){const row=views.get(id);if(row)candidates.set(row.id,row);}
   for(const row of candidates.values()){
    const c=known.get(key(session,row.id));
    if(!c||receiptActive(c.state)||c.state==="queued"&&!listed.has(row.id))await query(row,signal);
    if(!current(session,generation))return;
   }
   queryFailed=false;notice="";noticeTranslated=false;
  }catch{
   if(current(session,generation)){queryFailed=true;notice="无法确认消息结果，已保留待确认内容。请重新查询。";noticeError=true;noticeTranslated=false;}
  }finally{
   refreshing=false;
   if(current(session,generation)){loading=false;render();}
   if(again){again=false;void refresh();}
  }
 }
 const guard=()=>owns()&&!scopeChanged&&/^[a-f0-9]{64}$/.test(scope);
 async function transmit(row:Outgoing){
  const id=key(row.session,row.id);if(!guard()||busy.has(id)||!row.input)return;
  const generation=epoch;
  busy.add(id);render();
  try{
   const value=await api<ChatRequestResponse>(endpoint(row.session,row.id),{method:"PUT",body:JSON.stringify(row.input),signal:AbortSignal.any([lifetime.signal,AbortSignal.timeout(15000)])});
   if(owns())response(value,row.session,row.id);
  }catch(error){
   if(owns()){
    // A lost reply is the unconfirmed row's own story; only a server answer adds news.
    if(current(row.session,generation)&&error instanceof APIError){notice=error.message;noticeError=true;noticeTranslated=true;}
    try{await query(row);}catch{if(current(row.session,generation))queryFailed=true;}
   }
  }finally{busy.delete(id);if(owns())render();}
 }
 async function retry(row:Outgoing){
  if(!guard()||busy.has(key(row.session,row.id)))return;
  const generation=epoch;
  // An explicit retry always queries first and only reuses the same envelope.
  try{
   const found=await query(row);
   if(!current(row.session,generation))return;
   if(found){render();return;}
   if(!missing.has(key(row.session,row.id))||!row.input){say("待确认正文已过期；仍可查询结果或放弃原消息编号。",true);return;}
   if(context?.session!==row.session||context.thread!==row.thread){say("请返回原对话后重试这条消息。",true);return;}
   if(!storage.add(row)){say("待确认副本未保存，暂未发送。请检查本机存储，或关闭本机保存后重试。",true);return;}
   if(row.draft)hooks.move(row.draft);
   await transmit(row);
  }catch{if(current(row.session,generation))say("无法确认消息结果，已保留待确认内容。请重新查询。",true);}
 }
 async function act(row:Outgoing,action:"review"|"abandon"|"interrupt"|"withdraw"){
  if(!guard()||busy.has(key(row.session,row.id)))return;
  const generation=epoch;
  if(action==="review"||action==="abandon"){
   const message=action==="review"?"请先查看对话记录和工作区文件，确认上一个任务做到了哪一步。继续后可以发送新消息；原消息不会重新执行。服务若曾重启，容器里的任务可能仍在运行。":"放弃会阻止这条尚未接收的消息以后执行。已接收的任务不会被取消；请先查询并核对结果。";
   if(!await confirm(message))return;
  }
  if(!guard()||!current(row.session,generation))return;
  const id=key(row.session,row.id);busy.add(id);render();
  try{
   const found=await query(row);if(!guard()||!current(row.session,generation))return;
   if(action==="abandon"&&found){say("消息已经接收，请按当前状态处理。",true);return;}
   if(action==="withdraw"&&found?.state!=="queued"){say("这条消息已经开始执行，不能撤回。",true);return;}
   if(action!=="abandon"&&!found){say("尚未查到该消息的接收记录",true);return;}
   const result=await api<ChatRequestResponse>(endpoint(row.session,row.id),{method:"POST",body:JSON.stringify({action,scope,revision:found?.revision}),signal:AbortSignal.any([lifetime.signal,AbortSignal.timeout(15000)])});
   if(!owns())return;
   const done=response(result,row.session,row.id);
   if(current(row.session,generation)){notice=action==="interrupt"?"已请求中断，正在等待任务结束确认。":"";noticeError=false;noticeTranslated=false;}
   // Withdrawn: the message goes back to the composer for editing, like an unsent draft.
   if(action==="withdraw"&&done.state==="abandoned"&&current(row.session,generation)){
    const frozen=row.input||known.get(key(row.session,row.id))?.request;
    const draft=row.draft||(frozen?draftFromRequest(frozen):undefined);
    // A composer that already has a draft keeps it; the row stays for "copy back".
    if(draft&&hooks.restore(structuredClone(draft)))dismiss(row);
   }
   void refresh();
  }catch(error){if(current(row.session,generation)){notice=error instanceof Error?error.message:t("无法确认消息结果，已保留待确认内容。请重新查询。");noticeError=true;noticeTranslated=true;}}
  finally{busy.delete(id);if(owns())render();}
 }
 const dismiss=(row:Outgoing)=>{const id=key(row.session,row.id);dismissed.add(id);fullText.delete(id);storage.remove(row.id,row.session);views.delete(id);known.delete(id);};
 // The one action a row needs before anything else can happen. The collapsed
 // bar offers it too, so a blocked composer never hides its way out.
 const primary=(row:Outgoing)=>row.state==="unconfirmed"?{label:"重试",tip:"用原编号重新发送，不会重复执行",icon:"refresh",run:()=>void retry(row),disabled:!row.input||row.thread!==context?.thread}
  :row.state==="uncertain"?{label:"已查看，继续",tip:"确认已查看结果，然后可以发送新消息",icon:"check",run:()=>void act(row,"review"),disabled:false}:undefined;
 const control=(label:string,tip:string,icon:string,run:()=>void,disabled:boolean,kind=""):HTMLButtonElement=>{
  const b=document.createElement("button");b.type="button";b.className="btn btn-sm delivery-act"+(kind?" "+kind:"");b.dataset.deliveryAction=tip;
  actionButton(b,label?()=>t(label):"",icon,()=>t(label||tip));setAttrRender(b,"data-tip",()=>t(tip));
  b.disabled=disabled;b.addEventListener("click",()=>{if(owns())run();});return b;
 };
 const mark=(view:Look)=>{const m=document.createElement("span");m.className="delivery-mark";m.setAttribute("aria-hidden","true");if(view.icon)m.append(svgIcon(view.icon,14));return m;};
 let quickSignature="";
 function render(){
  if(!owns())return;
  const root=$("chat-delivery"),list=$("chat-delivery-list"),status=$("chat-delivery-status"),reason=$("chat-delivery-reason"),quick=$("chat-delivery-quick");
  root.classList.toggle("hidden",!context);
  if(!context)return;
  const items=rows();
  const code=(row:Outgoing)=>known.get(key(row.session,row.id))?.error_code||"";
  const merged=(row:Outgoing)=>!!known.get(key(row.session,row.id))?.merged_into;
  const waiting=items.filter(r=>r.state==="queued").length;
  const other=(row:Outgoing)=>row.thread!==context?.thread?" · "+t("另一对话的消息"):"";
  const highlighted=items.find(r=>r.state==="unconfirmed"||r.state==="uncertain")||items.find(r=>receiptActive(r.state))||items.find(r=>r.state!=="completed")||items[0];
  $<HTMLButtonElement>("chat-delivery-toggle").disabled=!items.length;
  if(!items.length)showDetails(false);
  setTextRender($("chat-delivery-count"),()=>String(items.length));
  setAttrRender($("chat-delivery-count"),"aria-label",()=>t("{count} 条消息",{count:items.length}));
  $("chat-delivery-count").classList.toggle("hidden",items.length<2);
  // A finished turn says nothing the conversation does not already show.
  root.classList.toggle("hidden",enabled&&!items.some(r=>r.state!=="completed")&&!loading&&!queryFailed&&!scopeChanged&&!notice&&!storage.error);
  // Any of these replaces the headline, so the row's reason and action step aside.
  const pageIssue=unsupported||!enabled||!scope||scopeChanged||!!notice||storage.error;
  const view=highlighted&&look(highlighted.state,code(highlighted),merged(highlighted));
  root.dataset.tone=pageIssue&&noticeError||queryFailed||unsupported||scopeChanged||storage.error?"attention":loading?"busy":view?.tone||"quiet";
  // Status precedence: a page/protocol problem, then this row's own story.
  const headline=()=>unsupported?t("当前网页不支持此服务端的消息协议，请刷新页面或升级客户端。"):!enabled?t("此服务端使用旧发送方式，无法按消息编号确认接收结果。"):!scope||scopeChanged?t("消息所属的实例或登录身份已变化"):notice?(noticeTranslated?notice:t(notice)):storage.error?t("本机待确认副本保存失败，请保留本页并检查存储设置。"):loading?t("正在查询消息接收状态…"):view?t(view.title)+other(highlighted):t("消息确认与恢复");
  const detail=()=>pageIssue||loading||!view?"":highlighted.state==="unconfirmed"&&!storage.enabled?t("本机保存已关闭；待确认内容仅保留在当前页面。"):receiptActive(highlighted.state)&&waiting?t("后面还有 {n} 条排队",{n:waiting}):view.reason?t(view.reason):"";
  setTextRender(status,headline);setTextRender(reason,detail);
  reason.classList.toggle("hidden",!detail());
  status.classList.toggle("error",noticeError||storage.error);
  $<HTMLButtonElement>("chat-delivery-refresh").disabled=!enabled||loading||refreshing;
  const next=highlighted&&!pageIssue&&!loading?primary(highlighted):undefined;
  const quickKey=JSON.stringify([highlighted?.id,highlighted?.state,next?.label,next?.disabled,highlighted&&busy.has(key(highlighted.session,highlighted.id))]);
  if(quickKey!==quickSignature){
   quickSignature=quickKey;
   const keep=quick.contains(document.activeElement);
   quick.replaceChildren(...(next&&highlighted?[control(next.label,next.tip,next.icon,next.run,next.disabled||busy.has(key(highlighted.session,highlighted.id)),"delivery-primary")]:[]));
   if(keep)(quick.querySelector<HTMLElement>("button:not(:disabled)")||$("chat-delivery-toggle")).focus();
  }
  const signature=JSON.stringify([context,waiting,items.map(row=>{const id=key(row.session,row.id);return [row,known.get(id)?.request?.text,code(row),merged(row),busy.has(id),fullText.has(id)];})]);
  if(signature===listSignature){hooks.changed();return;}
  listSignature=signature;
  const focused=document.activeElement instanceof HTMLElement&&list.contains(document.activeElement)?document.activeElement:undefined;
  const focusID=focused?.closest<HTMLElement>(".delivery-item")?.dataset.requestId,focusAction=focused?.dataset.deliveryAction;
  list.replaceChildren();
  for(const row of items){
   const id=key(row.session,row.id),c=known.get(id),working=busy.has(id),rowView=look(row.state,c?.error_code||"",!!c?.merged_into);
   const entry=document.createElement("div");entry.className="delivery-item";entry.dataset.requestId=row.id;entry.dataset.state=row.state;entry.dataset.tone=rowView.tone;
   const main=document.createElement("div");main.className="delivery-main";
   const line=document.createElement("p");line.className="delivery-line";
   const label=document.createElement("span");label.className="delivery-label";setTextRender(label,()=>t(rowView.title)+other(row));line.append(label);
   if(rowView.reason){const why=document.createElement("span");why.className="delivery-reason";setTextRender(why,()=>t(rowView.reason));line.append(why);}
   main.append(line);
   const text=row.draft?.text||row.input?.text||c?.request?.text||"";
   if(text){
    // One line until asked: long prompts must not push the composer away.
    const open=fullText.has(id),body=document.createElement("button");body.type="button";body.className="delivery-text";body.dataset.deliveryAction="text";body.textContent=text;
    body.classList.toggle("full",open);body.setAttribute("aria-expanded",String(open));
    body.addEventListener("click",()=>{if(!owns())return;if(fullText.has(id))fullText.delete(id);else fullText.add(id);render();});
    main.append(body);
   }else{const gone=document.createElement("p");gone.className="delivery-text expired";setTextRender(gone,()=>t("待确认正文已过期；仍可查询结果或放弃原消息编号。"));main.append(gone);}
   const frozen=row.input||c?.request;
   if(frozen||row.draft?.attachments.length){
    const meta=document.createElement("p");meta.className="delivery-settings";
    const files=row.draft?.attachments.map(a=>a.orig||a.name).join(" · ")||"";
    setTextRender(meta,()=>[frozen?t("模型：{model}；推理强度：{effort}",{model:frozen.model,effort:frozen.effort||t("默认")}):"",files].filter(Boolean).join(" · "));
    main.append(meta);
   }
   const actions=document.createElement("div");actions.className="delivery-actions";
   const next=primary(row);
   if(next)actions.append(control(next.label,next.tip,next.icon,next.run,working||next.disabled,"delivery-primary"));
   if(row.state==="unconfirmed")actions.append(control("放弃发送","放弃这条消息，以后也不会执行","close",()=>void act(row,"abandon"),working,"btn-danger"));
   if(row.state==="queued")actions.append(control("撤回","撤回到输入框，不再自动执行","undo",()=>void act(row,"withdraw"),working));
   if(["accepted","starting","running"].includes(row.state))actions.append(control("","中断任务","stop",()=>void act(row,"interrupt"),working,"btn-danger"));
   if(settled(row.state)&&row.state!=="completed")actions.append(control("","复制回输入框","copy",()=>void(async()=>{
    if(context?.session!==row.session)return;
    const generation=epoch;
    if(context.thread!==row.thread&&!await confirm("这条消息属于另一对话。复制到当前对话的输入框？不会自动发送，也不会覆盖已有草稿。"))return;
    if(!current(row.session,generation))return;
    const frozen=row.input||c?.request;
    const draft=row.draft||(frozen?draftFromRequest(frozen):undefined);
    if(draft)hooks.restore(structuredClone(draft));
   })(),working||!row.draft&&!row.input&&!c?.request));
   if(settled(row.state))actions.append(control("","从列表移除","close",()=>{dismiss(row);render();},working));
   entry.append(mark(rowView),main,actions);list.append(entry);
  }
  if(focused&&keyboardInput()){
   const row=[...list.querySelectorAll<HTMLElement>(".delivery-item")].find(item=>item.dataset.requestId===focusID);
   const button=[...row?.querySelectorAll<HTMLButtonElement>("button")||[]].find(b=>b.dataset.deliveryAction===focusAction&&!b.disabled);
   (button||row?.querySelector<HTMLElement>("button:not(:disabled)")||$("chat-input")).focus();
  }
  hooks.changed();
 }
 $("chat-delivery-toggle").addEventListener("click",()=>showDetails(!expanded),{signal:lifetime.signal});
 $("chat-delivery-list").addEventListener("keydown",event=>{if(event.key==="Escape"){event.stopPropagation();showDetails(false);$("chat-delivery-toggle").focus();}},{signal:lifetime.signal});
 $("chat-delivery-refresh").addEventListener("click",()=>void refresh(),{signal:lifetime.signal});
 window.addEventListener("online",()=>void refresh(),{signal:lifetime.signal});
 window.addEventListener("focus",()=>void refresh(),{signal:lifetime.signal});
 window.addEventListener("storage",event=>{
  if(!owns())return;
  if(event.key?.startsWith("agentbox.chat-outbox.v1.")||event.key?.startsWith("agentbox.chat-draft.v1.")){
   if(!storage.enabled)storage.clearPersistent();render();void refresh();
  }
 },{signal:lifetime.signal});
 window.addEventListener("chat-local-saving-changed",event=>{storage.setEnabled((event as CustomEvent<{enabled:boolean}>).detail.enabled);render();},{signal:lifetime.signal});
 document.addEventListener("visibilitychange",()=>{if(!document.hidden)void refresh();},{signal:lifetime.signal});
 const timer=setInterval(()=>void refresh(),4000);
 const closeConfirmation=()=>{if(confirmation){const dialog=$<HTMLDialogElement>("dlg-ask");if(dialog.open)dialog.close("");}};
 const leave=()=>{showDetails(false);++epoch;closeConfirmation();viewAbort.abort();viewAbort=new AbortController();context=undefined;loading=false;queryFailed=false;notice="";socketState=undefined;$("chat-delivery").classList.add("hidden");};
 return {
  enabled,unsupported,blocked,
  socketStatus(state:string){socketState=state==="running"?"running":"idle";},
  executionState(){
   if(!owns()||!enabled||loading)return;
   const items=rows();
   if(items.some(r=>["accepted","starting","running"].includes(r.state)))return "running";
   if(items.some(r=>r.state==="unconfirmed"))return;
   if(items.some(r=>r.state==="uncertain"&&known.has(key(r.session,r.id))))return "idle";
   // A terminal receipt from a prior turn must not erase a newer legacy WS
   // turn's running status. With a live socket its latest status remains valid.
   if((!hooks.connected()||socketState==="idle")&&items.some(r=>known.has(key(r.session,r.id))&&!receiptActive(r.state)))return "idle";
  },
  enter(session:string,thread:string){if(context?.session!==session||context.thread!==thread){leave();context={session,thread};loading=enabled;render();}void refresh();},
  leave,refresh,
  message(c:unknown){if(!owns()||!context||!enabled)return;try{const value=receipt(c,context.session);apply(value);render();}catch{void refresh();}},
  // The message this page is sending or the server is starting, until the
  // server records it (a stopped workspace starts first): its text and turn.
  pending(){
   if(!owns()||!enabled||!context)return;
   const row=rows().find(r=>r.thread===context?.thread&&!!r.input&&!r.input.delivery&&(r.state==="unconfirmed"?busy.has(key(r.session,r.id)):["accepted","starting","running"].includes(r.state)));
   if(!row?.input)return;
   return {id:row.id,text:row.input.text,turn:known.get(key(row.session,row.id))?.turn_id||""};
  },
  interrupt(){if(!owns())return true;const row=rows().find(r=>["accepted","starting","running"].includes(r.state));if(row){void act(row,"interrupt");return true;}return false;},
  async send(input:ChatRequestInput,draft:Draft,unchanged:()=>boolean){
   if(!enabled||!guard()||!context)return false;
   const {session,thread}=context,generation=epoch;
   const prepare=async()=>{
    if(!current(session,generation)||thread!==context?.thread||!unchanged()||blocked(!!input.delivery)){render();return false;}
    const row:Outgoing={version:1,id:identifier(),session,thread,created:Date.now(),input:structuredClone(input),draft:structuredClone(draft),revision:0,state:"unconfirmed"};
    if(!storage.add(row)){say("待确认副本未保存，暂未发送。请检查本机存储，或关闭本机保存后重试。",true);return false;}
    for(const [id,c] of known)if(c.state==="completed"){known.delete(id);views.delete(id);}
    views.set(key(session,row.id),row);showDetails(false);hooks.move(draft);render();await transmit(row);
    const sent=known.get(key(session,row.id));
    if(input.delivery&&sent&&current(session,generation)&&(sent.merged_into||sent.state==="queued"))hooks.followed(sent.merged_into?"steered":"queued");
    // Accepted means the user moved on: finished rows of this conversation go.
    if(current(session,generation)&&known.has(key(session,row.id))){for(const old of rows())if(old.id!==row.id&&old.thread===thread&&settled(old.state))dismiss(old);render();}
    return true;
   };
   if(navigator.locks)return navigator.locks.request("agentbox-chat-send-"+scope+"-"+session,{ifAvailable:true},lock=>lock?prepare():false);
   return prepare();
  },
  dispose(){if(!active)return;if(S.token!==owner){if(S.preserveChatCopiesOnSignout)storage.forget();else storage.clear();}active=false;closeConfirmation();lifetime.abort();viewAbort.abort();clearInterval(timer);context=undefined;known.clear();views.clear();$("chat-delivery-list").replaceChildren();$("chat-delivery").classList.add("hidden");},
 };
}
