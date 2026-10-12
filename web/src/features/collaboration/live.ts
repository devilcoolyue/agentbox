import {setText,t} from "../../i18n.js";
import {actionButton,buttonLabel} from "../../icons.js";
import {setSelectValue} from "../../select.js";
import {modelBlocks,modelDelta,type OutputBlock} from "./output.js";

interface OutputView {scope:string;run:{id:string;state:string;revision:number};step:{id:string;state:string;revision:number;role:string}|null}
interface OutputEvent {seq:number;type:string;body:unknown}
interface Step {id:string;run_id:string;turn_id:string;participant_generation:number;role:"coordinator"|"executor";kind:string;state:string;revision:number}
interface Transcript {next:number;more:boolean;initialized:boolean;turn:string;blocks:Map<string,Map<string,OutputBlock>>;truncated:boolean;incomplete:boolean;lastRead:number}
const object=(v:unknown):v is Record<string,unknown>=>!!v&&typeof v==="object"&&!Array.isArray(v);
const key=(v:unknown):v is string=>typeof v==="string"&&/^[A-Za-z0-9_-]{1,128}$/.test(v);
const integer=(v:unknown):v is number=>typeof v==="number"&&Number.isSafeInteger(v)&&v>=0;
const executing=new Set(["planning","executing","reviewing","waiting_answer","cancelling"]);
const kinds:Record<string,string>={plan:"规划",execute:"执行",clarify:"答疑",review:"评审",repair:"返工",handoff_repair:"交接格式修复"};
const textLimit=64000,batchPages=8,maxBlocks=128,maxTurns=100;
function readStep(value:unknown):Step|undefined {
 if(!object(value)||!key(value.id)||!key(value.run_id)||!key(value.turn_id)||!integer(value.participant_generation)||value.participant_generation<1||!integer(value.revision)||!["coordinator","executor"].includes(String(value.role))||typeof value.kind!=="string"||typeof value.state!=="string")return;
 return value as unknown as Step;
}
function emptyTranscript():Transcript{return{next:0,more:true,initialized:false,turn:"",blocks:new Map(),truncated:false,incomplete:false,lastRead:0}}
function trimBlocks(blocks:Map<string,OutputBlock>):boolean {
 let length=[...blocks.values()].reduce((sum,b)=>sum+b.text.length,0),trimmed=false;
 for(const [key,b] of blocks){if(length<=textLimit&&blocks.size<=maxBlocks)break;blocks.delete(key);length-=b.text.length;trimmed=true}
 return trimmed;
}

// A participant owns one transcript across several steps. Cache its byte cursor
// and route entries by persisted user.turn.id before showing any model output.
export function createLiveOutput(owner:string,owns:()=>boolean){
 const box=document.createElement("details"),title=document.createElement("summary"),status=document.createElement("p"),selector=document.createElement("select"),label=document.createElement("label"),body=document.createElement("pre"),controls=document.createElement("div"),more=document.createElement("button"),follow=document.createElement("button");
 box.className="collaboration-evidence collaboration-run-output";box.hidden=true;box.open=true;setText(title,"步骤输出");status.dataset.outputStatus="";status.setAttribute("role","status");selector.dataset.outputStep="";body.dataset.outputBody="";more.dataset.outputMore="";
 const caption=document.createElement("span");setText(caption,"选择步骤");label.append(caption,selector);for(const b of [more,follow]){b.type="button";b.className="btn btn-sm"}actionButton(more,()=>t("加载更多记录"),"chevron");actionButton(follow,()=>t("跟随最新步骤"),"activity");controls.append(more,follow);box.append(title,label,status,body,controls);
 let view:OutputView|undefined,generation=0,selectionGeneration=0,selected="",following=true,eventsAfter=0,eventsMore=false,eventsLoading=false,historyLoading=false,historyError=false,eventsError=false,streamError=false,liveTruncated=false;
 let life:AbortController|undefined,historyAbort:AbortController|undefined,streamAbort:AbortController|undefined,streamStep="",reconnectAfter=0,optionSignature="";
 const steps=new Map<string,Step>(),stepOrder=new Map<string,number>(),transcripts=new Map<string,Transcript>(),liveBlocks=new Map<string,OutputBlock>();let draft:{kind:"text"|"thinking";text:string}|undefined;
 const valid=(g:number)=>owns()&&!!view&&g===generation&&!life?.signal.aborted;
 const current=(g:number,s:number,id:string)=>valid(g)&&s===selectionGeneration&&selected===id;
 const transcript=(step:Step)=>{const k=step.role+":"+step.participant_generation;let value=transcripts.get(k);if(!value){value=emptyTranscript();transcripts.set(k,value)}return value};
 const active=()=>!!view&&executing.has(view.run.state)&&view.step?.id===selected&&["accepted","starting","running","finalizing"].includes(view.step.state);
 const ordered=()=>[...steps.values()].sort((a,b)=>(stepOrder.get(a.id)??Number.MAX_SAFE_INTEGER)-(stepOrder.get(b.id)??Number.MAX_SAFE_INTEGER));
 const latest=()=>view?.step&&steps.has(view.step.id)?view.step.id:ordered().at(-1)?.id||"";
 function render(){
  const step=steps.get(selected),cache=step?transcript(step):undefined;
  const signature=ordered().map(s=>[s.id,s.kind,s.role].join(":")).join("|");
  if(signature!==optionSignature){optionSignature=signature;selector.replaceChildren(...ordered().map((s,i)=>new Option(`${i+1}. ${t(s.role==="coordinator"?"主 Agent":"辅 Agent")} · ${t(kinds[s.kind]||"步骤输出")}`,s.id)));}
  setSelectValue(selector,selected);label.hidden=!step;follow.hidden=following||!steps.size;
  const all=new Map(cache?.blocks.get(step?.turn_id||"")||[]);for(const [id,b] of liveBlocks)all.set(id,b);
  const fragments=[...all.values()].map(b=>(b.kind==="thinking"?t("思考摘要")+"\n":"")+b.text);
  if(draft?.text)fragments.push((draft.kind==="thinking"?t("思考摘要")+"\n":"")+draft.text);
  const text=fragments.join("\n\n"),nearBottom=body.scrollHeight-body.scrollTop-body.clientHeight<48;
  if(body.textContent!==text.slice(-textLimit)){body.textContent=text.slice(-textLimit);if(nearBottom)body.scrollTop=body.scrollHeight}body.hidden=!body.textContent;
  if(historyError||eventsError)setText(status,"暂时无法读取步骤记录，请重试。");
  else if(historyLoading||eventsLoading)setText(status,"正在加载步骤输出…");
  else if(!step)setText(status,"任务尚未保存步骤输出。");
  else if(streamError&&active())setText(status,"实时连接已断开，已保存的记录仍可查看。");
  else if(cache?.incomplete)setText(status,"部分记录尚未写完，请稍后刷新。");
  else if(!body.textContent)setText(status,active()?"正在等待模型输出…":"该步骤暂未保存模型输出。");
  else setText(status,active()?"正在接收当前步骤输出。":"已保存的步骤输出。");
  if(cache?.truncated||liveTruncated||text.length>textLimit)status.textContent+=" "+t("部分内容过长，预览已省略。");
  more.hidden=!(historyError||eventsError||eventsMore||cache?.more);more.disabled=historyLoading||eventsLoading;
  buttonLabel(more,()=>t(historyError||eventsError?"重试":"加载更多记录"),historyError||eventsError?"refresh":"chevron");controls.hidden=more.hidden&&follow.hidden;
 }
 function stopStream(){streamAbort?.abort();streamAbort=undefined;streamStep="";}
 function choose(id:string){
  if(id===selected)return;
  selectionGeneration++;historyAbort?.abort();historyAbort=undefined;historyLoading=false;stopStream();selected=id;historyError=false;streamError=false;liveTruncated=false;reconnectAfter=0;liveBlocks.clear();draft=undefined;
  render();void loadHistory();connect();
 }
 function ingest(events:readonly OutputEvent[]){
  for(const e of events){if(!integer(e.seq)||e.seq<1)continue;eventsAfter=Math.max(eventsAfter,e.seq);if(e.type!=="step")continue;const step=readStep(e.body);if(!step||step.run_id!==view?.run.id)continue;stepOrder.set(step.id,Math.min(stepOrder.get(step.id)??e.seq,e.seq));const old=steps.get(step.id);if(!old||step.revision>=old.revision)steps.set(step.id,step);}
 }
 async function json(path:string,signal:AbortSignal):Promise<Record<string,unknown>>{
  const response=await fetch(path,{headers:{Authorization:`Bearer ${owner}`},signal:AbortSignal.any([signal,AbortSignal.timeout(15000)])});if(!response.ok)throw Error("history unavailable");const result:unknown=await response.json();if(!object(result)||result.version!==1||result.scope!==view?.scope)throw Error("invalid history scope");return result;
 }
 async function loadEvents(){
  if(!view||eventsLoading||!life)return;const g=generation,id=view.run.id;eventsLoading=true;eventsError=false;render();
  try{for(let page=0;page<batchPages;page++){
   const after=eventsAfter,result=await json(`/api/collaboration/runs/${id}/events?after=${after}`,life.signal);if(!valid(g))return;
   if(result.run_id!==id||!Array.isArray(result.events))throw Error("invalid events");let seq=after;
   for(const e of result.events){if(!object(e)||e.seq!==seq+1||typeof e.type!=="string")throw Error("invalid event cursor");seq++}
   ingest(result.events as unknown as OutputEvent[]);eventsMore=result.events.length===100;if(!eventsMore)break;
  }
  if(valid(g)){if(following)choose(latest());render();}
  }catch{if(valid(g))eventsError=true}finally{if(valid(g)){eventsLoading=false;render();}}
 }
 function acceptEntries(cache:Transcript,entries:unknown[]){
  for(const value of entries){if(!object(value))continue;
   if(value.kind==="user"){cache.turn=object(value.turn)&&key(value.turn.id)?value.turn.id:"";continue}
   if(value.kind==="status"&&value.state==="truncated"){cache.truncated=true;continue}
   if(value.kind!=="event"||!cache.turn)continue;
   let blocks=cache.blocks.get(cache.turn);if(!blocks){blocks=new Map();cache.blocks.set(cache.turn,blocks);if(cache.blocks.size>maxTurns){cache.blocks.delete(cache.blocks.keys().next().value!);cache.truncated=true}}
   for(const b of modelBlocks(value.event)){if(b.text.length>textLimit)cache.truncated=true;blocks.set(b.key,{...b,text:b.text.slice(-textLimit)})}
   if(trimBlocks(blocks))cache.truncated=true;
  }
 }
 async function loadHistory(force=false){
  const step=steps.get(selected);if(!view||!step||historyLoading||!life)return;const cache=transcript(step);
  if(!force&&cache.initialized&&!cache.more&&!active()&&cache.lastRead>=view.run.revision){render();return}
  const g=generation,s=selectionGeneration,id=view.run.id,stepID=step.id,runRevision=view.run.revision;historyAbort?.abort();historyAbort=new AbortController();const signal=AbortSignal.any([life.signal,historyAbort.signal]);historyLoading=true;historyError=false;render();
  try{for(let page=0;page<batchPages;page++){
   const after=cache.next,result=await json(`/api/collaboration/runs/${id}/steps/${stepID}/history?after=${after}`,signal);if(!current(g,s,stepID))return;
   if(result.run_id!==id||result.step_id!==stepID||result.role!==step.role||result.generation!==step.participant_generation||!Array.isArray(result.entries)||!integer(result.next)||result.next<after||typeof result.has_more!=="boolean"||typeof result.truncated!=="boolean")throw Error("invalid step history");
   if(result.next===after&&result.entries.length)throw Error("history cursor did not advance");
   acceptEntries(cache,result.entries);cache.initialized=true;cache.next=result.next;cache.more=result.has_more;cache.truncated ||=result.truncated;cache.lastRead=runRevision;cache.incomplete=cache.more&&cache.next===after;
   if(draft?.text&&[...(cache.blocks.get(step.turn_id)?.values()||[])].some(b=>b.kind===draft!.kind&&b.text.startsWith(draft!.text)))draft=undefined;
   if(!cache.more||cache.incomplete)break;
  }
  if(current(g,s,stepID)&&!active()&&!cache.more&&cache.lastRead>=view!.run.revision){liveBlocks.clear();draft=undefined}
  }catch{if(current(g,s,stepID))historyError=true}finally{if(current(g,s,stepID)){historyLoading=false;render();if(!historyError&&cache.lastRead<view!.run.revision)void loadHistory();}}
 }
 function connect(){
  if(!view||!active()||!selected){stopStream();return}
  if(streamAbort&&streamStep===selected||Date.now()<reconnectAfter)return;
  stopStream();const id=view.run.id,stepID=selected,g=generation,s=selectionGeneration;streamStep=stepID;streamAbort=new AbortController();const abort=streamAbort,signal=AbortSignal.any([life!.signal,abort.signal]);
  void(async()=>{try{
   const response=await fetch(`/api/collaboration/runs/${id}/stream`,{headers:{Authorization:`Bearer ${owner}`},signal});if(!response.ok||!response.headers.get("Content-Type")?.includes("text/event-stream")||!response.body)throw Error("stream unavailable");
   const reader=response.body.pipeThrough(new TextDecoderStream()).getReader();let buffer="";
   while(true){const {value,done}=await reader.read();if(done)break;if(!current(g,s,stepID))return;buffer=(buffer+value).replace(/\r\n/g,"\n");if(buffer.length>512*1024)throw Error("stream frame too large");let end:number;
    while((end=buffer.indexOf("\n\n"))>=0){const frame=buffer.slice(0,end);buffer=buffer.slice(end+2);const raw=frame.split("\n").filter(l=>l.startsWith("data:")).map(l=>l.slice(5).trimStart()).join("\n");if(!raw)continue;const message:unknown=JSON.parse(raw);
     if(!object(message)||message.version!==1||message.run_id!==id||message.step_id!==stepID||message.role!==steps.get(stepID)?.role||!object(message.event))continue;
     const wrapper=message.event;let event:unknown;
     if(wrapper.type==="agent_event")event=wrapper.event;
     else if(wrapper.type==="step_event"&&object(wrapper.entry)&&wrapper.entry.kind==="event")event=wrapper.entry.event;
     else continue;
     const delta=modelDelta(event);if(delta){if(delta.start||draft?.kind!==delta.kind)draft={kind:delta.kind,text:""};liveTruncated ||=draft!.text.length+delta.text.length>textLimit;draft!.text=(draft!.text+delta.text).slice(-textLimit)}
     else{const blocks=modelBlocks(event);if(blocks.length){draft=undefined;for(const b of blocks){liveTruncated ||=b.text.length>textLimit;liveBlocks.set(b.key,{...b,text:b.text.slice(-textLimit)})}liveTruncated=trimBlocks(liveBlocks)||liveTruncated}}
     streamError=false;render();
    }
   }
   if(current(g,s,stepID)&&active())streamError=true;
  }catch{if(current(g,s,stepID)&&!signal.aborted)streamError=true}finally{if(current(g,s,stepID)&&streamAbort===abort){streamAbort=undefined;streamStep="";reconnectAfter=Date.now()+5000;render();}}})();
 }
 const close=()=>{generation++;selectionGeneration++;life?.abort();life=undefined;historyAbort?.abort();historyAbort=undefined;stopStream();view=undefined;selected="";eventsAfter=0;eventsMore=eventsLoading=historyLoading=historyError=eventsError=streamError=liveTruncated=false;steps.clear();stepOrder.clear();transcripts.clear();liveBlocks.clear();draft=undefined;following=true;optionSignature="";selector.replaceChildren();body.textContent="";status.textContent="";box.hidden=true;};
 function show(value:OutputView,events?:readonly OutputEvent[]){
  if(!owns())return;if(value.run.state==="deleted"){close();return}
  if(!view||view.run.id!==value.run.id||view.scope!==value.scope){close();life=new AbortController();view=value;box.hidden=false}else view=value;
  if(events)ingest(events);const step=readStep(value.step);if(step&&step.run_id===value.run.id){const old=steps.get(step.id);if(!old||step.revision>=old.revision)steps.set(step.id,step)}
  if(following)choose(latest());render();
  if(!steps.size||events===undefined)void loadEvents();
  void loadHistory();connect();
 }
 selector.addEventListener("change",()=>{following=false;choose(selector.value);render()});
 follow.addEventListener("click",()=>{following=true;choose(latest());render()});
 more.addEventListener("click",()=>{if(eventsMore||eventsError||!steps.size)void loadEvents();else void loadHistory(true)});
 const select=(stepID:string)=>{if(!steps.has(stepID))return;following=false;box.open=true;choose(stepID);render();};
 return{box,show,close,select};
}
