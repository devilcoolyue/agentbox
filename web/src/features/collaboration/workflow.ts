import {setText,t} from "../../i18n.js";
import {stateLabels,type View,type Event} from "./client.js";
import {buildTrace,compactTrace,type Lane,type TraceEntry} from "./trace-model.js";

import {createTraceWire} from "./trace-wire.js";
import {isDirect} from "./strategy.js";
import {traceStepUsage,traceTokenTotal,tokens,dollars} from "./trace-usage.js";

const lanes:Lane[]=["user","decision","coordinator","executor","platform"];
const laneLabels:Record<Lane,string>={user:"用户",decision:"决策",coordinator:"主 Agent",executor:"辅 Agent",platform:"平台"};

const node=<K extends keyof HTMLElementTagNameMap>(tag:K,cls="",text?:string)=>{const e=document.createElement(tag);e.className=cls;if(text)setText(e,text);return e;};
const control=(label:string,fn:()=>void)=>{const b=node("button","",label);b.type="button";b.onclick=fn;return b;};
const detailText=(e:TraceEntry)=>e.kind==="decision"?e.detail.split(" · ").map((part,i)=>i===1?t(part):part).join(" · "):e.detail;
function timeLabel(at:string,start:number){const n=Date.parse(at);if(!Number.isFinite(n)||!Number.isFinite(start))return "—";const seconds=Math.max(0,Math.floor((n-start)/1000));return `+${Math.floor(seconds/60).toString().padStart(2,"0")}:${(seconds%60).toString().padStart(2,"0")}`;}

/** An event-sequence view. Positions are event order, never invented durations or a workflow DAG. */
export function createWorkflow(onSelect?:(entry:TraceEntry)=>void){
 const life=new AbortController();
 const box=node("section","collaboration-trace");
 const head=node("div","collaboration-trace-head"),identity=node("div");
 identity.append(node("h3","","协作轨迹"));
 const status=node("span","collaboration-trace-status");status.setAttribute("role","status");identity.append(status);
 const totals=node("div","collaboration-trace-totals"),totalTokens=node("span"),totalCost=node("strong"),decisionCost=node("small");totalTokens.dataset.traceTotalTokens="";totalCost.dataset.traceTotalCost="";decisionCost.dataset.traceDecisionCost="";totals.append(totalTokens,totalCost,decisionCost);head.append(identity,totals);
 const toolbar=node("div","collaboration-trace-toolbar"),tabs=node("div","collaboration-trace-tabs");
 const sequence=control("派发时序",()=>{mode="sequence";paint();}),list=control("事件列表",()=>{mode="events";paint();});sequence.dataset.traceView="sequence";list.dataset.traceView="events";tabs.append(sequence,list);
 const search=node("input","collaboration-trace-search");search.type="search";search.dataset.traceSearch="";search.setAttribute("aria-label",t("搜索轨迹"));search.placeholder=t("搜索轨迹");
 const density=control("展开全部事件",()=>{compact=!compact;follow=false;paint();});density.dataset.traceDensity="";
 toolbar.append(tabs,density,search);
 const transport=node("div","collaboration-trace-transport"),play=control("时序重放",()=>{if(playing){stop();paint();return;}if(!snapshot||cursor>=snapshot.length){snapshot=entries.map(e=>({...e}));cursor=0;selected="";}playing=true;paint();schedule();});play.dataset.traceReplay="";
 const scrub=node("input","collaboration-trace-scrub");scrub.type="range";scrub.min="0";scrub.step="1";scrub.value="0";scrub.dataset.traceScrub="";scrub.setAttribute("aria-label",t("回放位置"));
 const position=node("span","collaboration-trace-position"),latest=control("返回最新",()=>{stop();snapshot=undefined;selected="";follow=true;windowSize=200;paint();});latest.dataset.traceLatest="";
 transport.append(play,scrub,position,latest);
 const viewport=node("div","collaboration-trace-viewport");viewport.tabIndex=0;viewport.setAttribute("aria-label",t("协作轨迹"));
 const surface=node("div","collaboration-trace-surface"),headers=node("div","collaboration-trace-lanes");
 headers.append(node("span","collaboration-trace-time-title","时间"));
 const laneRail=node("div","collaboration-trace-lane-rail");headers.append(laneRail);
 const laneNodes=lanes.map(lane=>{const e=node("div","collaboration-trace-lane");e.dataset.lane=lane;const mark=node("span","collaboration-trace-mark"),info=node("div"),name=node("strong","",laneLabels[lane]),model=node("small");info.append(name,model);e.append(mark,info);laneRail.append(e);return{lane,e,mark,model,name};});
 for(const text of ["Token","费用","耗时"])headers.append(node("span","collaboration-trace-column",text));
 const rows=node("div","collaboration-trace-rows");const older=control("显示更早轨迹",()=>{windowSize+=200;follow=false;paint();});older.className="collaboration-trace-older";
 const empty=node("p","collaboration-trace-empty");surface.append(headers,older,rows,empty);viewport.append(surface);
 const inspect=node("section","collaboration-trace-inspect");inspect.hidden=true;
 const inspectHead=node("div"),inspectTitle=node("strong"),inspectMeta=node("span"),inspectBody=node("pre"),inspectUsage=node("div","collaboration-trace-breakdown"),output=control("查看步骤输出",()=>{const e=visibleEntries().find(e=>e.id===selected);if(e)onSelect?.(e);});
 inspectHead.append(inspectTitle,output,control("关闭",()=>{selected="";paint();}));inspect.append(inspectHead,inspectMeta,inspectUsage,inspectBody);
 const footer=node("div","collaboration-trace-footer"),count=node("span"),hint=node("span","","按事件顺序排列 · 点击查看详情");footer.append(count,hint);
 box.append(head,toolbar,viewport,inspect,transport,footer);
 let view:View|undefined,entries:TraceEntry[]=[],snapshot:TraceEntry[]|undefined,cursor=0,playing=false,timer:number|undefined,mode=window.matchMedia("(max-width: 600px)").matches?"events":"sequence",selected="",follow=true,windowSize=200,signature="",disposed=false,compact=true,suspended=false,wireWidth=500,resizeFrame=0;
 let activeLanes=[...lanes];
 const laneX=(lane:Lane)=>(activeLanes.indexOf(lane)+.5)*600/activeLanes.length;
 const dom=new Map<string,HTMLButtonElement>(),wires=new Map<string,ReturnType<typeof createTraceWire>>();
 const reduced=window.matchMedia("(prefers-reduced-motion: reduce)");
 const visible=()=>box.isConnected&&!box.closest("[hidden]")&&!box.closest("dialog:not([open])")&&!document.hidden;
 function measureWires(){const width=Number.parseFloat(getComputedStyle(laneRail).width);if(width>0&&Math.abs(width-wireWidth)>.01){wireWidth=width;for(const wire of wires.values())wire.resize(width);}}
 function queueMeasure(){if(resizeFrame||disposed)return;resizeFrame=requestAnimationFrame(()=>{resizeFrame=0;if(!disposed){measureWires();syncMotion();}});}
 const resizeObserver=new ResizeObserver(queueMeasure);resizeObserver.observe(laneRail);
 function syncMotion(){
  const enabled=!disposed&&!suspended&&visible()&&mode==="sequence"&&!reduced.matches;
  const last=rows.lastElementChild as HTMLElement|null;
  for(const [id,wire] of wires){const row=dom.get(id),replay=!!snapshot&&last?.dataset.entry===id;const live=!snapshot&&box.dataset.mode==="running"&&row?.dataset.tone==="active";
   wire.sync(replay?"replay":live?"live":"none",id,enabled,replay&&!playing);
  }
 }

 function stop(){playing=false;if(timer!==undefined)clearTimeout(timer);timer=undefined;}
 function schedule(){
  if(!playing)return;
  const last=rows.lastElementChild as HTMLElement|null,latest=visibleEntries().at(-1);
  const delay=last&&latest&&last.dataset.entry===latest.id?(wires.get(latest.id)?.duration||1100)+180:180;
  timer=window.setTimeout(()=>{
   if(disposed||!visible()){stop();paint();return;}
   if(cursor>=(snapshot?.length||0)){stop();paint();return;}
   cursor++;paint();schedule();
  },delay);
 }
 function visibleEntries(){return snapshot?snapshot.slice(0,cursor):entries;}
 function laneText(lane:Lane){return t(lane==="executor"&&isDirect(view||{})?"执行 Agent":laneLabels[lane]);}
 function paintInspector(){
  const all=visibleEntries(),e=(compact&&!search.value.trim()?compactTrace(all):all).find(e=>e.id===selected);inspect.hidden=!e;if(!e)return;
  setText(inspectTitle,e.label);inspectMeta.textContent=`${laneText(e.from)} → ${laneText(e.to)} · #${e.seq}${e.at?" · "+e.at:""}`;
  inspectBody.textContent=detailText(e)||t("此事件没有附加说明。");output.hidden=!e.stepID||!onSelect;
  const usage=!snapshot&&e.stepID?view?.step_usage?.find(s=>s.step_id===e.stepID):undefined;
  inspectUsage.replaceChildren();inspectUsage.hidden=!usage;
  if(usage)for(const [key,value] of [["输入",usage.input_tokens],["输出",usage.output_tokens],["缓存读取",usage.cache_read_tokens],["缓存写入",usage.cache_write_tokens]] as const){const cell=node("span");cell.textContent=`${t(key)} ${value===null?"—":value.toLocaleString()}`;inspectUsage.append(cell);}
  if(usage){const state=node("span","",usage.status==="settled"?"已结算":usage.status==="pending"?"待结算":usage.status==="partial"?"部分已记录":"未记录用量");inspectUsage.append(state);}

 }
 function makeRow(e:TraceEntry,start:number){
  let row=dom.get(e.id);if(!row){row=node("button","collaboration-trace-row");row.type="button";row.dataset.entry=e.id;row.onclick=()=>{selected=selected===e.id?"":e.id;paint();};row.onpointerenter=()=>wires.get(e.id)?.hover();dom.set(e.id,row);}
  const usage=traceStepUsage(e,entries,view?.step_usage,!!snapshot);
  const attempt=view?.decision?.attempt;
  const decision=!snapshot&&attempt&&e.id===entries.find(row=>row.kind==="decision"&&["Jev 选模","回退模型"].includes(row.label))?.id?attempt:undefined;
  const rowSignature=JSON.stringify([e,usage,decision,t(e.label),mode,start,!!snapshot,activeLanes]);
  if(row.dataset.signature!==rowSignature){
   row.dataset.signature=rowSignature;wires.get(e.id)?.dispose();wires.delete(e.id);row.replaceChildren();row.dataset.kind=e.kind;row.dataset.tone=e.tone;row.dataset.step=e.stepID||"";
   const time=node("span","collaboration-trace-time");time.textContent=timeLabel(e.at,start);time.title=e.at||t("未记录时间");
   const diagram=node("span","collaboration-trace-diagram"),copy=node("span","collaboration-trace-copy"),label=node("strong","",e.label),route=node("small");route.textContent=`${laneText(e.from)} → ${laneText(e.to)}`;copy.append(label,route);
   const brief=node("span","collaboration-trace-brief");brief.textContent=detailText(e).replaceAll("\n"," · ").slice(0,240);
   const a=laneX(e.from),b=laneX(e.to),self=a===b;
   copy.style.left=`${(self?Math.min(a+10,442):Math.min(a,b)+8)/6}%`;copy.style.maxWidth=`${(self?150:Math.max(110,Math.abs(b-a)-12))/6}%`;
   const wire=createTraceWire(activeLanes.indexOf(e.from),activeLanes.indexOf(e.to),wireWidth,activeLanes.length);wires.set(e.id,wire);
   diagram.append(wire.element,copy,brief);
   const token=node("span","collaboration-trace-value"),cost=node("span","collaboration-trace-value"),duration=node("span","collaboration-trace-value");token.dataset.traceTokens="";cost.dataset.traceCost="";duration.dataset.traceDuration="";
   token.textContent=usage?.total_tokens!=null?tokens(usage.total_tokens):usage?t(usage.status==="pending"?"待结算":"未记录"):"—";
   cost.textContent=usage?.cost_micro_usd!=null?dollars(usage.cost_micro_usd):usage?t(usage.status==="pending"?"待结算":usage.status==="partial"?"不完整":"未记录"):"—";
   if(usage?.status==="partial"){if(usage.total_tokens!==null)token.textContent="≥"+token.textContent;if(usage.cost_micro_usd!==null)cost.textContent="≥"+cost.textContent;}
   if(decision){
    const n=decision.tokens_known&&decision.input_tokens!==undefined&&decision.output_tokens!==undefined?decision.input_tokens+decision.output_tokens:null;
    token.textContent=n!==null&&Number.isSafeInteger(n)?tokens(n):t("未记录");
    cost.textContent=decision.state==="settled"&&decision.cost_micro_usd!==undefined?dollars(decision.cost_micro_usd):t("待核对");
   }
   token.dataset.hasTokens=String(usage?.total_tokens!=null||!!decision?.tokens_known);
   token.title=usage?.total_tokens!=null?`${usage.total_tokens.toLocaleString()} Token`:token.textContent;cost.title=usage?.cost_micro_usd!=null?`$${(usage.cost_micro_usd/1e6).toFixed(6)}`:cost.textContent;
   duration.textContent=e.durationMS!==undefined?`${(e.durationMS/1000).toFixed(1)}s`:e.tone==="active"?t("运行中"):"—";
   row.append(time,diagram,token,cost,duration);
   row.setAttribute("aria-label",`${time.textContent} · ${t(e.label)} · ${route.textContent} · Token ${token.textContent} · ${t("费用")} ${cost.textContent} · ${t("耗时")} ${duration.textContent}`);
  }
  row.setAttribute("aria-pressed",String(e.id===selected));return row;
 }
 function paint(){
  if(disposed)return;
  activeLanes=isDirect(view||{})?lanes.filter(lane=>lane!=="coordinator"&&(lane!=="decision"||!!view?.run.request?.decision||entries.some(e=>e.kind==="decision"))):[...lanes];
  box.style.setProperty("--trace-lane-count",String(activeLanes.length));
  const replay=!!snapshot,all=visibleEntries(),query=search.value.trim().normalize("NFKC").toLocaleLowerCase();
  const filtered=query?all.filter(e=>[t(e.label),detailText(e),laneText(e.from),laneText(e.to)].join(" ").normalize("NFKC").toLocaleLowerCase().includes(query)):(compact?compactTrace(all):all);
  const shown=filtered.slice(-windowSize),start=Date.parse((snapshot||entries).find(e=>Number.isFinite(Date.parse(e.at)))?.at||"");
  box.dataset.view=mode;box.dataset.replay=String(replay);box.dataset.playing=String(playing);box.dataset.density=compact?"compact":"full";
  setText(density,compact?"展开全部事件":"收起状态事件");density.setAttribute("aria-pressed",String(!compact));
  const total=traceTokenTotal(view?.step_usage);
  totalTokens.textContent=replay?t("回放中隐藏当前结算"):total.value===null?`Token —`:`${total.partial?"≥":""}${tokens(total.value)} Token`;
  totalTokens.title=replay?"":total.value===null?t("未记录用量"):`${total.value.toLocaleString()} Token · ${t("含缓存读写，不含决策模型")}`;
  totalCost.textContent=!replay&&view?`${view.cost.partial||view.cost.decision_partial?"≥":""}${dollars(view.cost.cost_micro_usd)}`:"";totalCost.title=!replay&&view?`${t("已记录总费用，含决策费用")} · $${(view.cost.cost_micro_usd/1e6).toFixed(6)}`:t("已记录总费用，含决策费用");
  decisionCost.hidden=replay||!view?.cost.decision_calls;decisionCost.textContent=`Jev ${view?.cost.decision_partial?t("待核对"):view?.cost.decision_cost_micro_usd===undefined?"—":dollars(view.cost.decision_cost_micro_usd)}`;

  const r=view?.run;const autoWaiting=r?.state==="paused"&&["verification_required","delivery_required"].includes(r.wait_reason||"");
  const deciding=r?.state==="accepted"&&view?.decision?.state==="claimed";
  const running=!!r&&(deciding||["preparing","planning","executing","reviewing","waiting_answer"].includes(r.state)||autoWaiting);
  const tone=!r?"idle":r.state==="completed"?"complete":["awaiting_plan","waiting_user"].includes(r.state)?"waiting":running?"running":["paused","uncertain","failed","cancelled","cancelling"].includes(r.state)?"stopped":"idle";
  box.dataset.mode=tone;
  setText(hint,mode==="sequence"&&window.matchMedia("(max-width: 600px)").matches?"左右滑动查看全部角色":"按事件顺序排列 · 点击查看详情");
  setText(status,replay?"历史回放":!r?"待开始":deciding?"正在决策":autoWaiting?(r.wait_reason==="verification_required"?"平台验证":"正在交付"):r.state==="completed"?(entries.some(e=>e.label==="结果已应用到项目")?"已应用到当前项目":"已完成"):stateLabels[r.state]||"未知状态");
  sequence.setAttribute("aria-pressed",String(mode==="sequence"));list.setAttribute("aria-pressed",String(mode==="events"));
  play.disabled=!entries.length;setText(play,playing?"暂停回放":"时序重放");play.setAttribute("aria-label",t(playing?"暂停回放":"时序重放"));play.setAttribute("aria-pressed",String(playing));latest.setAttribute("aria-pressed",String(!replay&&follow));
  scrub.disabled=!entries.length;scrub.max=String((snapshot||entries).length);scrub.value=String(replay?cursor:entries.length);
  position.textContent=replay?`${cursor} / ${snapshot!.length}`:t("最新");
  for(const {lane,e,mark,model,name} of laneNodes){
   e.hidden=!activeLanes.includes(lane);setText(name,lane==="executor"&&isDirect(view||{})?"执行 Agent":laneLabels[lane]);
   const binding=!replay&&(lane==="coordinator"||lane==="executor")?view?.configuration.roles[lane]:undefined;
   mark.textContent=binding?(binding.agent_type==="claude"?"C":">_"):lane==="user"?"U":lane==="decision"?"◇":lane==="coordinator"?"A":lane==="executor"?"B":"⌘";
   model.textContent=binding?`${binding.agent_type==="claude"?"Claude Code":"Codex"} · ${binding.model}`:replay&&(lane==="coordinator"||lane==="executor")?"":lane==="decision"?(replay?t("模型决策"):view?.run.request?.decision?.provider==="jev"||view?.cost?.decision_calls?"Jev":t("内置规则")):lane==="user"?t("需求与确认"):t("调度与验证");model.title=model.textContent;
   const role=autoWaiting?"platform":view?.step?.role||(r?.phase==="executing"?"executor":["planning","reviewing","answering"].includes(r?.phase||"")?"coordinator":"platform");
   e.dataset.active=String(!replay&&running&&(view?.decision?.state==="claimed"?lane==="decision":role===lane));
  }
  // Keep stable row nodes across polling; do not restart completed-event animations.
  const oldTop=viewport.scrollTop,oldLeft=viewport.scrollLeft,allowed=new Set(shown.map(e=>e.id));
  for(const [id,row] of dom)if(!allowed.has(id)){wires.get(id)?.dispose();wires.delete(id);row.remove();dom.delete(id);}
  let previous:Element|null=null;
  for(const e of shown){const row=makeRow(e,start);const expected:Element|null=previous?previous.nextElementSibling:rows.firstElementChild;if(expected!==row)rows.insertBefore(row,expected);previous=row;}
  older.hidden=filtered.length<=windowSize;
  empty.hidden=!!shown.length;setText(empty,!entries.length?"任务开始后，真实决策与交接会出现在这里。":query?"没有匹配的轨迹":replay?"拖动时间轴或播放，查看协作如何发生。":"暂无轨迹");
  count.textContent=compact&&!query?t("{p0} 个关键步骤 · {p1} 条事件",{p0:String(filtered.length),p1:String(all.length)}):t("{p0} 条事件",{p0:String(filtered.length)});paintInspector();
  viewport.scrollLeft=oldLeft;viewport.scrollTop=follow||playing?viewport.scrollHeight:oldTop;measureWires();syncMotion();
 }
 function render(value?:View,events:readonly Event[]=[]){
  if(disposed)return;
  const changed=view?.run.id!==value?.run.id;if(changed){stop();snapshot=undefined;selected="";follow=true;windowSize=200;search.value="";signature="";for(const wire of wires.values())wire.dispose();wires.clear();dom.clear();rows.replaceChildren();}
  const wasSuspended=suspended;if(visible())suspended=false;
  view=value;
  const nextSignature=JSON.stringify([value,events,t("协作轨迹")]);if(nextSignature===signature){if(wasSuspended&&!suspended)paint();return;}signature=nextSignature;
  entries=buildTrace(value,[...events]);paint();
 }
 search.addEventListener("input",()=>{if(!composing){follow=false;paint();}},{signal:life.signal});let composing=false;
 search.addEventListener("compositionstart",()=>{composing=true;},{signal:life.signal});search.addEventListener("compositionend",()=>{composing=false;follow=false;paint();},{signal:life.signal});
 scrub.addEventListener("input",()=>{stop();if(!snapshot)snapshot=entries.map(e=>({...e}));cursor=Math.min(snapshot.length,Number(scrub.value));follow=true;paint();},{signal:life.signal});
 viewport.addEventListener("wheel",()=>{follow=false;},{passive:true,signal:life.signal});viewport.addEventListener("touchstart",()=>{follow=false;},{passive:true,signal:life.signal});viewport.addEventListener("keydown",e=>{if(["ArrowUp","PageUp","Home"].includes(e.key))follow=false;},{signal:life.signal});
 window.addEventListener("agentbox-language-change",()=>{search.placeholder=t("搜索轨迹");search.setAttribute("aria-label",t("搜索轨迹"));scrub.setAttribute("aria-label",t("回放位置"));paint();},{signal:life.signal});
 document.addEventListener("visibilitychange",()=>{if(document.hidden)stop();paint();},{signal:life.signal});
 reduced.addEventListener("change",syncMotion,{signal:life.signal});
 const close=()=>{suspended=true;stop();for(const wire of wires.values())wire.stop();paint();};
 const resume=()=>{suspended=false;paint();queueMeasure();};
 const dispose=()=>{stop();life.abort();disposed=true;resizeObserver.disconnect();if(resizeFrame)cancelAnimationFrame(resizeFrame);for(const wire of wires.values())wire.dispose();wires.clear();dom.clear();};
 paint();return{box,render,close,resume,dispose};
}
