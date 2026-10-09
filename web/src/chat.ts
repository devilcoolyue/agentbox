import { setAttrRender, setText, setTextRender, t as i18nText } from "./i18n.js";
/* chat：对话通道（WS）、历史加载、composer（发送/中断/附件/自增高）、
 * 模型与思考强度选择、空会话引导。渲染管线在 chat-render.ts。 */
"use strict";

import { actionButton, buttonLabel } from "./icons.js";

import { S, bus, emit } from "./state.js";
import type { Pick } from "./state.js";
import type {
  ChatMessage, History, ModelOption, ReasoningCapability, SessionModels,
} from "./types.js";
import { $, spinEl, insertAtCursor, openLightbox, isMobile, onMobileChange, askPrompt, toast, isImeEnter, enterInsertsNewline } from "./util.js";
import { api, wsURL, imgURLFromPath, uploadAttachment } from "./api.js";
import { refreshAll } from "./data.js";
import { chip, renderUserMsg, renderEntry, svgIcon } from "./chat-render.js";
import { answerFooter, observeAnswer } from "./chat-footer.js";
import type { AnswerContext } from "./chat-footer.js";
import { setThreadBar, noteThreadTitle, applyThreadTitle } from "./chat-threads.js";
import { agentIcon, agentAvatar } from "./brand.js";
import { BUDGETS, EFFORT_LABELS, allowedLevels, defaultReasoning, canKeepEffort } from "./reasoning.js";
import { setTip } from "./tip.js";

/* ---------------- 对话通道 ----------------
 * 断线在移动端切网、挂后台时很常见，而一个回合可以跑上半小时，所以断开必须
 * 可见（状态条 + 手动重连），且重连成功后要把断线期间错过的事件补回来——
 * 服务端把完整事件落盘在线程 JSONL 里，重新拉一次历史即可对齐。
 * 连接本身还兼作「唤醒」：服务端 chat WS 会幂等地拉起已休眠的容器。 */

import { formatProblem } from "./problems.js";
import { ChatSender } from "./features/chat/sender.js";
import { createChatStream } from "./features/chat/stream.js";
import { ChatHistory } from "./features/chat/history.js";
import { ChatConnection } from "./features/chat/connection.js";
import {initComposerDrafts} from "./features/chat/drafts.js";
import type {Draft,DraftAttachment} from "./features/chat/draft-store.js";
import {initChatOutbox} from "./features/chat/outbox.js";
let drafts:ReturnType<typeof initComposerDrafts>|undefined;
let outbox:ReturnType<typeof initChatOutbox>|undefined;
let composerThread="";
const legacyDraftVersions=new Map<string,number>();
const connection = new ChatConnection({
 message: data => { try { handleChatMsg(JSON.parse(data)); } catch (_) {} },
 state: (state, attempt, reference) => {
  chatAttempt = attempt;
  if (state === "closed") chatConnectionID = reference;
  if (state === "connected") chatConnectionID = undefined;
  setChatConn(state);
 },
 reconnect: () => { void loadHistory({ silent: true });void outbox?.refresh(); },
});
let chatAttempt = 0;
let chatConnectionID: string | undefined;

/** 对话通道状态条的四种态；connected 表示收起状态条 */
type ChatConn = "connected" | "connecting" | "waking" | "closed";

function setChatConn(state: ChatConn) {
  const bar = $("chat-conn");
  if (!bar) return;
  if (state === "connected") { bar.classList.add("hidden"); return; }
  bar.classList.remove("hidden");
  const dot = $("chat-conn-dot");
  const text = $("chat-conn-text");
  const btn = $("chat-reconnect");
  if (state === "connecting") {
    dot.className = "t-dot warn";
    setTextRender(text, () => {
      const status = chatAttempt > 1 ? i18nText("对话连接重连中…（第 {p0} 次）", { p0: String(chatAttempt) }) : i18nText("对话连接建立中…");
      return chatConnectionID ? `${status} ${formatProblem({code:"websocket_failed", operation_id:chatConnectionID}, i18nText)}` : status;
    });
    btn.classList.add("hidden");
  } else if (state === "waking") {
    dot.className = "t-dot warn";
    setText(text, "工作空间已休眠，正在唤醒…");
    btn.classList.add("hidden");
  } else {
    dot.className = "t-dot bad";
    setTextRender(text, () => formatProblem({code:"websocket_failed", operation_id:chatConnectionID}, i18nText));
    btn.classList.remove("hidden");
  }
}

export function connectChat() {
 if (S.current) connection.connect(wsURL(`/sessions/${S.current.id}/chat`));
}

/* 切换/删除会话时收尾：作废重连定时器、关连接、复位状态 */
const stream = createChatStream({append:appendChat,working:setWorkingLabel,state:()=>S.chatState,
 footer:()=>turnFooter,log:()=>$("chat-log"),nearBottom});
let chatEpoch = 0;
export function chatTeardown() {
  drafts?.leave();
  outbox?.leave();composerThread="";
  sender.cancel();
  ++chatEpoch;
  modelsAbort?.abort();
  pendingPick = null;
  sessionModels = null;
  manualEffort = false;
  omitEffortOnce = false;
  connection.dispose();
  chatConnectionID = undefined;
  setChatConn("connected");
  cancelHistoryLoad();
  stream.reset();
  resetAnswerContext();
  setChatStatus("idle");
}

/* 贴底判定：用户已在底部附近（正在跟读）才把视口拉到最底，
 * 上滚回看历史时不打扰。按钮使用稍宽的出现阈值形成滞回，避免在边界抖动。 */
const CHAT_BOTTOM_HIDE_DISTANCE = 60;
const CHAT_BOTTOM_SHOW_DISTANCE = 96;

function distanceFromBottom(log: HTMLElement) {
  return Math.max(0, log.scrollHeight - log.scrollTop - log.clientHeight);
}

function nearBottom(log: HTMLElement) {
  return distanceFromBottom(log) < CHAT_BOTTOM_HIDE_DISTANCE;
}

function updateScrollBottomButton() {
  const log = $("chat-log");
  const btn = $("chat-scroll-bottom");
  const wasVisible = btn.classList.contains("show");
  const threshold = wasVisible ? CHAT_BOTTOM_HIDE_DISTANCE : CHAT_BOTTOM_SHOW_DISTANCE;
  const show = !log.classList.contains("hidden") &&
    log.childElementCount > 0 && distanceFromBottom(log) > threshold;
  btn.classList.toggle("show", show);
  btn.setAttribute("aria-hidden", String(!show));
}





function handleChatMsg(msg: ChatMessage) {
  switch (msg.type) {
    case "chat_request":
      if(msg.version===1)outbox?.message(msg.receipt);
      break;
    case "user_message":
      stream.flush(); // 上一回合的回放立刻放完，不让新消息插到它前面
      noteThreadTitle(msg.text!);
      appendChat(renderUserMsg(msg.text));
      answerContext.turn = msg.turn;
      break;
    case "agent_event":
      observeAnswer(answerContext, msg.event, msg.ts);
      stream.event(msg.event);
      turnFooter?.update();
      break;
    case "agent_raw":
      stream.append(chip(msg.text!)); // 回放中则排队，保持时间线顺序
      break;
    case "turn_cost":
      if (msg.cost && msg.cost.turn_id === answerContext.turn?.id) {
        answerContext.cost = msg.cost;
        turnFooter?.update();
      }
      break;
    case "thread":
      // 另一个页面切换 / 新建 / 删除了对话线程；本端发起的操作已通过
      // thread-changed 重载过（S.thread 已对上），据此跳过重复加载
      if (!S.thread || S.thread.id !== msg.id) reloadThread();
      break;
    case "thread_title":
      // 服务端异步生成好了线程标题
      applyThreadTitle(msg.id!, msg.title!);
      break;
    case "status":
      outbox?.socketStatus(msg.state||"idle");
      if (!answerContext.ts && msg.ts) answerContext.ts = msg.ts;
      setChatStatus(msg.state!, msg.error ? formatProblem(msg, i18nText) : undefined);
      if (msg.state === "error" && msg.retry_text && !outbox?.enabled) {
        const input = $<HTMLTextAreaElement>("chat-input");
        if (!input.value.trim()) { input.value = msg.retry_text; autoGrow(); }
        const retry = document.createElement("button");
        retry.className = "btn btn-sm"; actionButton(retry, () => i18nText("重试"), "refresh", () => i18nText("恢复默认设置并重试本轮对话"));
        retry.addEventListener("click", () => {
          S.pick.effort = ""; pendingPick = null; manualEffort = false; savePick(); renderPickPill();
          omitEffortOnce = true; // 「恢复默认」这一轮不带强度，交给模型和客户端决定
          if (input.value.trim() && input.value.trim() !== msg.retry_text) {
            toast(i18nText("已恢复默认，请确认当前输入后发送")); return;
          }
          input.value = msg.retry_text!; autoGrow(); retry.remove(); sendChat();
        }, { once: true });
        appendChat(retry);
      }
      if (msg.state === "idle" || msg.state === "error") refreshAll();
      break;
    case "error":
      appendChat(chip(formatProblem(msg, i18nText), "err"));
      break;
  }
}

/* 发送按钮双态：空闲=琥珀纸飞机发送，执行中=红色 ■ 中断。
 * 图标是按钮里的两个 SVG，靠 .stop class 切换显隐，别用 textContent 覆盖。
 * 执行进度不再用输入框上方的浮动胶囊，而是在对话流末尾挂一枚会动的品牌头像
 * + 阶段文案（详见 ensureWorking）。 */
export function setChatStatus(state: string, error?: string) {
  S.chatState = state === "running" ? "running" : "idle";
  if (state === "error") stream.flush(); // 出错立刻放完，错误提示紧随其后
  if (S.chatState !== "running") stream.settle();
  const send = $<HTMLButtonElement>("chat-send");
  if (state === "running") {
    // 思考指示接在末尾，若用户正跟读（贴底）就滚出来，免得藏在折叠线下方
    const log = $("chat-log");
    const stick = nearBottom(log);
    ensureWorking();
    if (stick) log.scrollTop = log.scrollHeight;
    send.classList.add("stop");
    setTip(send, () => i18nText("中断"));
    setAttrRender(send, "aria-label", () => i18nText("中断")); // 纯图标按钮，可访问名称得跟着状态走
    send.disabled = !!outbox?.unsupported;
  } else {
    clearWorking();
    send.classList.remove("stop");
    setTip(send, () => i18nText("发送"));
    setAttrRender(send, "aria-label", () => i18nText("发送"));
    send.disabled = chatImgs.uploading > 0;
    if (state === "error" && error) appendChat(chip(error, "err"));
  }
  updateHero();
}



/* 占位提示：窄屏一行放不下长文案，且键盘快捷键提示在手机上无意义 */
function updateChatPlaceholder() {
  setAttrRender($<HTMLTextAreaElement>("chat-input"), "placeholder", () => isMobile() || enterInsertsNewline()
    ? i18nText("向 Agent 下达任务…")
    : i18nText("随心输入，向 Agent 下达任务…（Enter 发送，Shift+Enter 换行，可粘贴图片）"));
}


/* 输入框随内容自动增高（1 行起步，封顶后内部滚动） */

export function autoGrow() {
  const t = $("chat-input");
  t.style.height = "auto";
  t.style.height = Math.min(t.scrollHeight, 220) + "px";
  drafts?.save();
}

const attachmentFingerprint=()=>JSON.stringify([...chatImgs.map].map(([id,{valid,...a}])=>[id,a]));
const sender = new ChatSender({
 allowed:()=>!S.histLoading&&!S.histError&&!outbox?.blocked()&&S.chatState==="idle"&&chatImgs.uploading===0,
 read:()=>{
  if(!S.current)return;
  const raw=$<HTMLTextAreaElement>("chat-input").value;if(!raw.trim())return;
  const effort=omitEffortOnce?"":effectiveEffort();omitEffortOnce=false;
  const draft=composerDraft(),pick={model:S.pick.model,effort,effort_control:currentReasoning().control||""};
  let text=raw.trim();
  for(const [n,info] of chatImgs.map){if(!info.path)continue;text=text.split(`[Image #${n}]`).join(`[图片#${n} ${info.path}]`).split(`[File #${n}]`).join(`[附件#${n} ${info.path}]`);}
  const input={scope:S.chatScope,thread_id:composerThread,text,...pick,attachments:draft.attachments.map(a=>a.path)};
  return {owner:JSON.stringify([S.token,chatEpoch,S.current.id,composerThread]),content:JSON.stringify([raw,pick,attachmentFingerprint()]),input,draft};
 },
 durable:()=>!!outbox?.enabled,connected:()=>connection.ready,
 connect:()=>{chatAttempt=0;connectChat();},
 validate:async()=>drafts?await drafts.validate():true,
 deliver:async(snapshot,current)=>{
  if(outbox?.enabled){await outbox.send(snapshot.input,snapshot.draft,current);return;}
  if(!connection.ready||!current())return;
  const {text,model,effort,effort_control}=snapshot.input;
  if(!connection.send(JSON.stringify({type:"user_message",text,model,effort,effort_control})))return;
  $<HTMLTextAreaElement>("chat-input").value="";resetChatImgs();autoGrow();
 },
 changed:updateHero,edited:()=>toast(i18nText("输入已变化，请确认后重新发送。")),
 waking:()=>setChatConn("waking"),awake:()=>{void refreshAll();},
 timeout:()=>appendChat(chip(i18nText("唤醒工作空间超时，请稍后重试或手动启动工作空间"),"err")),
});
export async function sendChat() {
 if(chatImgs.uploading>0){appendChat(chip(i18nText("附件仍在上传中，请稍候…"),"err"));return;}
 await sender.send();
}

function sendInterrupt() {
  if(outbox?.unsupported)return;
  if(outbox?.enabled&&outbox.interrupt())return;
  if (connection.ready) {
    connection.send(JSON.stringify({ type: "interrupt" }));
  }
}

/* ---------------- 附件（粘贴图片 / 上传文件） ---------------- */

const chatImgs = { seq: 0, map: new Map<number,Omit<DraftAttachment,"id">>(), uploading: 0 };
const composerDraft=():Draft=>({text:$<HTMLTextAreaElement>("chat-input").value,attachments:[...chatImgs.map].map(([id,a])=>({id,...a}))});
const draftFingerprint=(d:Draft)=>JSON.stringify({text:d.text,attachments:d.attachments.map(({valid,...a})=>a)});
function applyComposerDraft(value:Draft){resetChatImgs();$<HTMLTextAreaElement>("chat-input").value=value.text;for(const {id,...a} of value.attachments){chatImgs.map.set(id,a);chatImgs.seq=Math.max(chatImgs.seq,id);}renderAttach();autoGrow();}
let attachmentEpoch=0;
const attachmentUploads=new Set<AbortController>();

export function resetChatImgs() {
  ++attachmentEpoch;for(const controller of attachmentUploads)controller.abort();attachmentUploads.clear();
  chatImgs.seq = 0;
  chatImgs.map.clear();
  chatImgs.uploading = 0;
  renderAttach();
}

function renderAttach() {
  const strip = $("chat-attach");
  strip.replaceChildren();
  if (!chatImgs.map.size) {
    strip.classList.add("hidden");
  } else {
    strip.classList.remove("hidden");
  }
  for (const [n, info] of chatImgs.map) {
    const remove=document.createElement("button");remove.type="button";remove.className="attach-remove";
    buttonLabel(remove,"","close");setAttrRender(remove,"aria-label",()=>i18nText("移除附件 #{n}",{n}));setTip(remove,()=>i18nText("移除附件 #{n}",{n}));
    remove.addEventListener("click",()=>{
      chatImgs.map.delete(n);
      const input=$<HTMLTextAreaElement>("chat-input");input.value=input.value.split(`[${info.kind==="img"?"Image":"File"} #${n}]`).join("");
      autoGrow();renderAttach();updateHero();
    });
    if(info.valid===false){
      const box=document.createElement("div");box.className="attach-file attachment-invalid";
      const label=document.createElement("span");label.textContent=info.orig||info.name||"#"+n;
      const note=document.createElement("span");setText(note,"附件待检查或需重新上传");box.append(label,note,remove);strip.append(box);continue;
    }
    if (info.kind === "file") {
      const box = document.createElement("div");
      box.className = "attach-file mono";
      const name = document.createElement("span");
      name.className = "fn";
      setTextRender(name, () => info.orig || info.name || i18nText("附件"));
      if (info.path) {
        box.append(document.createTextNode("📄"), name);
        setTip(box, () => i18nText("附件 #{p0} · {p1}", { p0: String(n), p1: String(info.path) }));
      } else {
        box.append(spinEl(), name);
      }
      box.append(remove);strip.appendChild(box);
      continue;
    }
    const box = document.createElement("div");
    box.className = "attach-thumb";
    const badge = document.createElement("span");
    badge.className = "badge";
    badge.textContent = "#" + n;
    box.appendChild(badge);
    if (info.path) {
      const img = document.createElement("img");
      img.src = imgURLFromPath(info.path);
      img.alt = "Image #" + n;
      img.addEventListener("click", () => openLightbox(imgURLFromPath(info.path), i18nText("图片 #{p0} · {p1}", { p0: String(n), p1: String(info.path) })));
      box.appendChild(img);
    } else {
      const up = document.createElement("span");
      up.className = "up";
      up.append(spinEl(), document.createTextNode(i18nText("上传中")));
      box.appendChild(up);
    }
    box.append(remove);strip.appendChild(box);
  }
  // 附件没传完不许发送（执行中按钮是「中断」，不能动）
  if (S.chatState !== "running") $<HTMLButtonElement>("chat-send").disabled = S.histLoading||!!S.histError||chatImgs.uploading>0||sender.busy||!!drafts?.blocked()||!!outbox?.blocked();
}

async function attachFile(file: File) {
  if(!S.current||S.histLoading||S.histError)return;
  if(chatImgs.map.size>=64){toast(i18nText("每条草稿最多保留 64 个附件。"),true);return;}
  const session=S.current.id,owner=S.token,epoch=attachmentEpoch,controller=new AbortController();attachmentUploads.add(controller);
  const isImg = (file.type || "").startsWith("image/");
  const tag = isImg ? "Image" : "File";
  const n = ++chatImgs.seq;
  chatImgs.map.set(n, { path: "", name: "", orig: file.name || "", kind: isImg ? "img" : "file" });
  insertAtCursor($("chat-input"), `[${tag} #${n}]`);
  autoGrow();
  chatImgs.uploading++;
  renderAttach();
  try {
    const res = await uploadAttachment(file,{session,signal:controller.signal});
    if(epoch!==attachmentEpoch||S.current?.id!==session||S.token!==owner||controller.signal.aborted||!chatImgs.map.has(n))return;
    chatImgs.map.set(n, { ...res,orig:res.orig||file.name||"", kind: isImg ? "img" : "file",valid:true });
  } catch (e) {
    if(epoch!==attachmentEpoch||S.current?.id!==session||S.token!==owner||controller.signal.aborted)return;
    chatImgs.map.delete(n);
    $<HTMLTextAreaElement>("chat-input").value = $<HTMLTextAreaElement>("chat-input").value.replace(`[${tag} #${n}]`, "");
    appendChat(chip(i18nText("附件上传失败：") + (e as Error).message, "err"));
  } finally {
    attachmentUploads.delete(controller);
    if(epoch!==attachmentEpoch||S.current?.id!==session||S.token!==owner)return;
    chatImgs.uploading--;
    drafts?.save();renderAttach();updateHero();
  }
}

export function pastedImages(e: ClipboardEvent) {
  return [...(e.clipboardData?.items || [])]
    .filter((i) => i.kind === "file" && i.type.startsWith("image/"))
    .map((i) => i.getAsFile())
    .filter((f): f is File => !!f);
}






/* ---------------- 模型 / 思考强度选择 ----------------
 * 两级菜单（仿 Codex 桌面端）：主面板是「模型 / 思考强度」两行，
 * 点进去选具体项。模型列表由服务端下发（系统设置可维护），
 * 另有「自定义模型…」可手输任意模型 ID，新模型无需改代码。 */

const FALLBACK_MODELS: Record<string, ModelOption[]> = {
  claude: [
    { id: "claude-opus-5", label: "Opus 5" },
    { id: "claude-fable-5", label: "Fable 5" },
    { id: "claude-opus-4-8", label: "Opus 4.8" },
    { id: "claude-sonnet-5", label: "Sonnet 5" },
    { id: "claude-haiku-4-5", label: "Haiku 4.5" },
  ],
  codex: [
    { id: "gpt-5.5", label: "GPT-5.5" },
    { id: "gpt-5.5-codex", label: "GPT-5.5 Codex" },
  ],
};
/** 思考强度选项：v 是传给 Agent 的取值，l 是展示名，sub 是补充说明 */
interface EffortOpt {
  v: string;
  l: string;
  sub?: string;
}
let sessionModels: SessionModels | null = null;
let modelsAbort: AbortController | null = null;
let manualEffort = false;
/** 出错后点「重试」：这一轮不传强度（真正的客户端默认值），之后恢复正常。 */
let omitEffortOnce = false;
let pendingPick: { effort: string; control?: string } | null = null;
function currentReasoning(): ReasoningCapability {
  const capability = modelOpts().find(m => m.id === S.pick.model)?.reasoning || sessionModels?.default_reasoning;
  return { ...defaultReasoning(pickStyle()), ...capability };
}
/* 账号配置了自己的模型列表时按模型走：只有确定支持调整的模型才出现强度选项，
 * 其余模型直接用默认值，不再提供「手动指定」。 */
function restricted() { return !!sessionModels?.restricted; }
function effortHidden() { return restricted() && currentReasoning().support !== "supported"; }
/* 档位已知的模型不再提供「默认强度」：没手选时用一个具体档位并照实发送，界面显示的就是
 * 实际使用的。优先账号 CLI 配置里的默认强度（Codex model_reasoning_effort 等），
 * 否则 Claude 取 high、Codex 取 medium（两家 API 不指定时的默认值），不在档位里取居中一档。 */
function defaultLevel(r: ReasoningCapability): string {
  const levels = r.levels || [];
  const client = r.control === "effort" ? sessionModels?.client_effort : "";
  if (client && levels.includes(client)) return client;
  const preferred = pickStyle() === "codex" ? "medium" : "high";
  return levels.includes(preferred) ? preferred : levels[Math.floor(levels.length / 2)] || "";
}
/** 本轮实际使用的强度；空串表示不传，由模型和客户端自行决定（仅档位未知或不支持时）。 */
function effectiveEffort(): string {
  const r = currentReasoning();
  return S.pick.effort || (r.support === "supported" ? defaultLevel(r) : "");
}
function effortOpts(): EffortOpt[] {
  const r = currentReasoning();
  const level = (v: string): EffortOpt => ({ v, l: EFFORT_LABELS[v] || v, sub: r.control === "budget" ? i18nText("预算上限 {p0} tokens", { p0: String(BUDGETS[v].toLocaleString("en-US")) }) : "" });
  if (r.support === "supported") return (r.levels || []).map(level);
  const opts: EffortOpt[] = [{ v: "", l: i18nText("默认强度"), sub: i18nText("沿用模型和客户端的默认设置，不等于关闭推理") }];
  if (r.support === "unsupported" || !manualEffort) return opts;
  return [...opts, ...allowedLevels(pickStyle(), r.control).map(level)];
}
/** 强度被重置时提示：档位已知的模型直接说改成了哪一档。 */
function toastEffortReset(byModel: boolean) {
  const named = currentReasoning().support === "supported";
  toast(named
    ? i18nText(byModel ? "新模型不支持原来的强度，已改为 {level}" : "模型能力已变化，强度已改为 {level}", { level: effortLabel("") })
    : i18nText(byModel ? "新模型的支持范围不同，已恢复为默认强度" : "模型能力已变化，已恢复为默认强度"));
}
async function refreshModelCapabilities() {
  const id = S.current?.id, epoch = chatEpoch;
  if (!id) return;
  modelsAbort?.abort();
  const abort = new AbortController(); modelsAbort = abort;
  try {
    const value = await api<SessionModels>(`/sessions/${id}/models`, { signal: abort.signal });
    if (abort.signal.aborted || epoch !== chatEpoch || S.current?.id !== id) return;
    if (!Array.isArray(value.models)) throw new Error(i18nText("模型能力响应无效"));
    let previous = currentReasoning();
    sessionModels = value;
    if (pendingPick) {
      const saved = pendingPick; pendingPick = null;
      const r = currentReasoning();
      manualEffort = !!saved.effort && saved.control === r.control;
      if (manualEffort && effortOpts().some(o => o.v === saved.effort)) S.pick.effort = saved.effort;
      previous = r;
    }
    // The account's list may have dropped the remembered model.
    if (value.restricted && !value.models.some(m => m.id === S.pick.model)) {
      const fallback = value.default_model || value.models[0]?.id;
      if (fallback) {
        if (localStorage.getItem(pickKey())) toast(i18nText("账号的可用模型已调整，已切换到 {model}", { model: modelLabel(fallback) }));
        S.pick.model = fallback; S.pick.effort = ""; manualEffort = false;
        previous = currentReasoning();
      }
    }
    const next = currentReasoning();
    if (S.pick.effort && effortHidden()) {
      S.pick.effort = ""; manualEffort = false;
    } else if (S.pick.effort && !(next.support === "unknown" && previous.control === next.control && manualEffort) && !canKeepEffort(previous, next, S.pick.effort)) {
      S.pick.effort = ""; manualEffort = false;
      toastEffortReset(false);
    }
    savePick(); renderPickPill(); closePickMenu();
  } catch (error) {
    if (!abort.signal.aborted && epoch === chatEpoch) toast(i18nText("读取模型能力失败：") + (error as Error).message, true);
  }
}
/* 尾部 [1m] 是 Claude Code 的 1M 上下文后缀（opus[1m] 等），与后端 modelRe 保持一致 */
export const MODEL_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(\[1m\])?$/;

function modelOpts(): ModelOption[] {
  // 隐藏的模型不进下拉；当前正在用的那个照常列出，免得选中项凭空消失。
  if (sessionModels?.restricted) return sessionModels.models.filter(m => !m.hidden || m.id === S.pick.model);
  const agent = (S.current && S.current.agent) as string;
  const fromSrv = sessionModels?.models || (S.models && S.models[agent]);
  const opts = [...(fromSrv?.length ? fromSrv : FALLBACK_MODELS[agent] || FALLBACK_MODELS.claude)];
  const initial = S.current?.default_model;
  if (initial && !opts.some((o) => o.id === initial)) {
    opts.unshift({ id: initial, label: initial });
  }
  return opts;
}
function pickKey() { return "agentbox_pick_" + (S.current ? S.current.id : ""); }
function modelLabel(v: string) {
  if (!v) v = workspaceModel();
  const hit = modelOpts().find((o) => o.id === v);
  return hit ? hit.label : v; // 自定义 ID 直接展示
}
function effortLabel(v: string) {
  if (currentReasoning().support === "unsupported") return i18nText("不支持调整");
  const opts = effortOpts(), e = v || effectiveEffort();
  return (opts.find(o => o.v === e) || opts[0])?.l || "";
}

function workspaceModel() {
  return sessionModels?.default_model || S.current?.default_model || (S.current?.agent === "codex" ? "gpt-5.5" : "claude-opus-5");
}

export function loadPick() {
  sessionModels = null;
  let p: Partial<Pick> & { version?: number; control?: string } = {};
  try { p = JSON.parse(localStorage.getItem(pickKey())!) || {}; } catch (_) {}
  S.pick = { model: typeof p.model === "string" && MODEL_ID_RE.test(p.model) ? p.model : workspaceModel(), effort: "" };
  const r = currentReasoning();
  pendingPick = p.version === 2 && p.effort ? { effort: p.effort, control: p.control } : null;
  // v1 Claude choices were budgets. Never silently migrate them to native effort.
  manualEffort = p.version === 2 && !!p.effort;
  if (p.version === 2 && p.control === r.control && effortOpts().some(o => o.v === p.effort)) S.pick.effort = p.effort!;
  renderPickPill(); closePickMenu();
  void refreshModelCapabilities();
}

function savePick() { localStorage.setItem(pickKey(), JSON.stringify({ ...S.pick, version: 2, control: currentReasoning().control })); }
function closePickMenu() { $("pick-menu").classList.add("hidden"); hideFly(); }

function renderPickPill() {
  const btn = $("btn-pick");
  btn.replaceChildren();
  if (S.current) btn.appendChild(agentIcon(S.current.agent, 13));
  btn.appendChild(Object.assign(document.createElement("span"), {
    className: "t",
    textContent: effortHidden() ? modelLabel(S.pick.model) : `${modelLabel(S.pick.model)} · ${effortLabel(S.pick.effort)}`,
  }));
  btn.appendChild(svgIcon("chevron", 12)); // 独立元素：既能垂直居中，也不被文字省略号裁掉
}

/* 面板样式按 Agent 区分：
 * codex —— 只有「模型 / 推理强度」两行入口，悬停某行在左侧浮出该属性列表，
 *          移到另一行即换列表，移出整个选择器则收回浮层，只剩两行。
 * claude —— 直接铺开模型列表，底部单独一行「思考强度」，悬停浮出强度列表。
 * 两者都：点击选项即选中并收起，点击外部收起整个面板。
 * 移动端无悬停，退回「点击进二级 + 返回」的抽屉式。 */
function pickStyle() { return S.current && S.current.agent === "codex" ? "codex" : "claude"; }
function effortTitle() { return currentReasoning().control === "budget" ? i18nText("思考预算") : i18nText("推理强度"); }
function customModel() {
  return !restricted() && S.pick.model && !modelOpts().some((o) => o.id === S.pick.model) ? S.pick.model : "";
}

function choose(kind: string, v: string) {
  pendingPick = null;
  if (kind === "model") {
    const before = currentReasoning();
    S.pick.model = v || workspaceModel();
    if (!canKeepEffort(before, currentReasoning(), S.pick.effort)) {
      S.pick.effort = "";
      toastEffortReset(true);
    }
    manualEffort = false;
  } else S.pick.effort = v;
  savePick();
  renderPickPill();
  closePickMenu();
}

async function askCustomModel() {
  const v = await askPrompt({
    get title() { return i18nText("自定义模型"); },
    get label() { return i18nText("模型 ID"); },
    value: customModel(),
    get hint() { return i18nText("留空恢复为 {p0}。", { p0: String(modelLabel(workspaceModel())) }); },
    validate: (s) => (s.trim() && !MODEL_ID_RE.test(s.trim())
      ? i18nText("模型 ID 格式不合法（字母数字开头，可含 . _ -）") : ""),
  });
  if (v === null) return;
  choose("model", v.trim());
}

/* 某属性的完整选项列表，浮层与移动端二级面板共用 */
function optList(kind: string) {
  if (kind === "effort") {
    const r = currentReasoning();
    if (r.support === "unsupported") {
      const info = document.createElement("p"); info.className = "muted";
      setText(info, "此模型不支持调整。CLI 中已配置的强度可能仍需清除。");
      return [info];
    }
    const current = effectiveEffort();
    const opts: HTMLElement[] = effortOpts().map(o => pickOpt(o.l, o.sub || "", current === o.v, () => choose("effort", o.v)));
    if (r.support === "unknown") {
      const note = document.createElement("p"); note.className = "muted";
      setText(note, "支持情况未知，手动指定可能被模型或中转服务拒绝。");
      opts.unshift(note);
      if (!manualEffort) opts.push(pickOpt(i18nText("手动指定…"), i18nText("仅在确认服务支持时使用"), false, () => {
        manualEffort = true; hideFly(); buildPickSub("effort");
      }));
    }
    return opts;
  }
  const out: HTMLElement[] = [];
  for (const o of modelOpts()) {
    out.push(pickOpt(o.label, o.id, S.pick.model === o.id, () => choose("model", o.id)));
  }
  if (restricted()) return out; // 只能用账号勾选的模型
  const cur = customModel();
  out.push(pickOpt(i18nText("自定义模型…"), cur, !!cur, askCustomModel));
  return out;
}

/* ---- 悬停浮层 ---- */

let flyTimer = 0;
function cancelHideFly() { clearTimeout(flyTimer); }
function scheduleHideFly() { clearTimeout(flyTimer); flyTimer = setTimeout(hideFly, 160); }
function hideFly() {
  clearTimeout(flyTimer);
  $("pick-fly").classList.add("hidden");
  for (const r of document.querySelectorAll(".pick-row.open")) r.classList.remove("open");
}

function showFly(row: HTMLElement, kind: string) {
  cancelHideFly();
  const menu = $("pick-menu"), fly = $("pick-fly");
  if (fly.dataset.kind !== kind || fly.classList.contains("hidden")) {
    const head = document.createElement("div");
    head.className = "pick-fly-h";
    setTextRender(head, () => kind === "model" ? i18nText("模型") : effortTitle());
    fly.replaceChildren(head, ...optList(kind));
    fly.dataset.kind = kind;
  }
  for (const r of document.querySelectorAll(".pick-row.open")) r.classList.remove("open");
  row.classList.add("open");
  fly.classList.remove("hidden");
  // 与悬停行顶端对齐，贴在面板左侧；超出视口时上下回拉
  fly.style.right = menu.offsetWidth + 6 + "px";
  // 按实际位置计算：底部固定的强度行不随列表滚动，offsetTop 不可靠。
  const host = (fly.offsetParent as HTMLElement | null) || menu.parentElement!;
  const top = row.getBoundingClientRect().top - host.getBoundingClientRect().top - host.clientTop;
  fly.style.top = top + "px";
  const r = fly.getBoundingClientRect();
  let dy = 0;
  if (r.top < 8) dy = 8 - r.top;
  else if (r.bottom > window.innerHeight - 8) dy = Math.max(8 - r.top, window.innerHeight - 8 - r.bottom);
  if (dy) fly.style.top = top + dy + "px";
}

/* ---- 面板 ---- */

function pickRow(label: string, value: string, kind: string) {
  const b = document.createElement("button");
  b.className = "pick-row";
  const l = document.createElement("span");
  buttonLabel(l, label, kind === "model" ? "cpu" : "sliders");
  const r = document.createElement("span");
  r.className = "val";
  r.textContent = value + " ›";
  b.append(l, r);
  if (isMobile()) {
    b.addEventListener("click", (e) => { e.stopPropagation(); buildPickSub(kind); });
  } else {
    b.addEventListener("mouseenter", () => showFly(b, kind));
    b.addEventListener("click", (e) => { e.stopPropagation(); showFly(b, kind); });
  }
  return b;
}

function buildPickMain() {
  const menu = $("pick-menu");
  hideFly();
  menu.replaceChildren();
  if (effortHidden()) {
    // 当前模型没有可选强度：只列模型，选到支持调整的模型后再出现强度行。
    const head = document.createElement("div");
    head.className = "pick-fly-h";
    setText(head, "模型");
    menu.append(head, ...optList("model"));
    return;
  }
  if (pickStyle() === "codex") {
    menu.append(
      pickRow(i18nText("模型"), modelLabel(S.pick.model), "model"),
      pickRow(effortTitle(), effortLabel(S.pick.effort), "effort"),
    );
    return;
  }
  const head = document.createElement("div");
  head.className = "pick-fly-h";
  setText(head, "模型");
  // 强度行固定在面板底部，模型多时只滚动上面的列表。
  const foot = document.createElement("div");
  foot.className = "pick-foot";
  foot.append(pickRow(effortTitle(), effortLabel(S.pick.effort), "effort"));
  menu.append(head, ...optList("model"), foot);
}

/* 移动端二级面板（无悬停可用） */
function buildPickSub(kind: string) {
  const menu = $("pick-menu");
  menu.replaceChildren();
  const h = document.createElement("button");
  h.className = "pick-back";
  buttonLabel(h, () => kind === "model" ? i18nText("模型") : effortTitle(), "chevron-left");
  h.addEventListener("click", (e) => { e.stopPropagation(); buildPickMain(); });
  menu.append(h, ...optList(kind));
}

function pickOpt(label: string, sub: string, on: boolean, onPick: () => void) {
  const b = document.createElement("button");
  b.className = "pick-opt" + (on ? " on" : "");
  const lbl = document.createElement("span");
  lbl.className = "lbl";
  lbl.textContent = label;
  if (sub) {
    const s = document.createElement("span");
    s.className = "sub";
    s.textContent = sub;
    lbl.appendChild(s);
  }
  const check = document.createElement("span");
  check.className = "check";
  check.textContent = "✓";
  b.append(lbl, check);
  b.addEventListener("click", (e) => { e.stopPropagation(); onPick(); });
  return b;
}


// 悬停行外的任何位置（面板其余部分 / 选择器之外）都收回浮层，回到默认层级




onMobileChange(closePickMenu);

/* ---------------- 空状态引导 ---------------- */

export function updateHero() {
  const loading = S.histLoading;
  const failed = !!S.histError;
  const blocked = loading || failed;
  $("chat-loading").classList.toggle("hidden", !blocked);
  $("chat-loading").classList.toggle("history-error", failed);
  $("chat-loading-spin").classList.toggle("hidden", !loading);
  setTextRender($("chat-loading-text"), () => failed ? S.histError : i18nText("正在加载历史对话…"));
  $("chat-loading-retry").classList.toggle("hidden", !failed);
  const show = !blocked && !!S.current &&
    !$("chat-log").childElementCount && S.chatState !== "running";
  if (show) {
    // 问候区头像跟随会话的 Agent 品牌
    const agent = S.current!.agent || "claude";
    const mount = $("hero-avatar");
    if (mount.dataset.agent !== agent) {
      mount.dataset.agent = agent;
      mount.replaceChildren(agentAvatar(agent, { icon: 30 }));
    }
  }
  $("chat-hero").classList.toggle("hidden", !show);
  $("chat-log").classList.toggle("hidden", blocked || show);
  updateScrollBottomButton();
  $<HTMLTextAreaElement>("chat-input").disabled = blocked;
  if (S.chatState !== "running") {
    $<HTMLButtonElement>("chat-send").disabled = blocked || chatImgs.uploading > 0||sender.busy||!!drafts?.blocked()||!!outbox?.blocked();
  }
  emit("chat-view-updated");
}

for (const c of document.querySelectorAll<HTMLElement>(".hero-pill")) {
  c.addEventListener("click", () => {
    insertAtCursor($<HTMLTextAreaElement>("chat-input"), c.dataset.fill!);
    autoGrow();
  });
}

/* ---------------- 历史对话（当前线程） ---------------- */

const history = new ChatHistory((path, signal) => api<History>(path, {signal}), () => S.current?.id);
function cancelHistoryLoad() {
  history.cancel();
  S.histLoading = false;
  S.histError = "";
}

/* opts.silent：重连后的对齐式补拉——不显示加载态（页面已有内容，闪一下
 * 加载中反而像出了故障），拉到后整体替换对话流以补上断线期间错过的事件，
 * 并保持用户原本的阅读位置（除非本来就贴着底）。 */
export async function loadHistory(opts: { silent?: boolean } = {}) {
  const sess = S.current; if (!sess) return;
  const silent = !!opts.silent;
  const log = $("chat-log");
  const wasAtBottom = silent ? nearBottom(log) : true;
  const prevScroll = log.scrollTop;
  if (!silent) {
    S.histLoading = true;
    S.histError = "";
    updateHero(); // 中央显示加载态，加载完一次性呈现，避免先闪新会话引导页
  }
  let draftThread = "";
  const result = await history.load(sess.id, `/sessions/${sess.id}/history`+(S.draftProtocol===1?"?draft_context=1":""), async ({active_thread,thread}) => {
    if(S.draftProtocol===1&&!active_thread)throw new Error(i18nText("无法确认对话草稿的归属，请重新加载历史。"));
    draftThread=active_thread||thread?.id||"legacy-empty-"+(legacyDraftVersions.get(sess.id)||0);
    await drafts?.enter(sess.id,draftThread);
  }, ({entries,thread,costs}) => {
    composerThread=draftThread;outbox?.enter(sess.id,draftThread);
    setThreadBar(thread);
    stream.reset();
    log.replaceChildren(); // 重连与首次加载都以服务端记录为准
    resetAnswerContext();
    for (const raw of entries) {
      if (raw.kind === "event") observeAnswer(answerContext, raw.event, raw.ts);
      for (const n of renderEntry(raw)) appendChat(n);
      if (raw.kind === "user" || raw.kind === "turn_context") {
        answerContext.turn = raw.turn;
        answerContext.cost = raw.turn ? costs?.[raw.turn.id] : undefined;
      }
      answerContext.historical = true;
      if (raw.kind === "status" && !answerContext.ts) answerContext.ts = raw.ts;
      turnFooter?.update();
    }
  });
  if (result.state === "stale" || !result.current()) return;
  if (result.state === "failed" && !silent) {
    S.histError = result.timedOut ? i18nText("历史对话加载超时")
      : i18nText("历史对话加载失败：") + ((result.error as Error).message || i18nText("未知错误"));
  }
  if (!silent) { S.histLoading = false; updateHero(); }
  if (result.state === "loaded") {
    log.scrollTop = wasAtBottom ? log.scrollHeight : prevScroll;
    updateScrollBottomButton();
  }
}

/* 对话线程发生切换（本端操作经 bus，或其它页面广播）后重载对话流 */
export async function reloadThread() {
  if (!S.current) return;
  if(S.draftProtocol!==1)legacyDraftVersions.set(S.current.id,(legacyDraftVersions.get(S.current.id)||0)+1);
  ++chatEpoch;sender.cancel();
  drafts?.leave();
  outbox?.leave();composerThread="";
  history.cancel(); // 立刻作废在途加载，clear 之后它们不得再往里追加
  stream.reset(); // 回放动画与积压一并丢弃，历史里有完整内容
  resetAnswerContext();
  $("chat-log").replaceChildren();
  setThreadBar(null);
  await loadHistory();
}



/* Agent 回合分组：两条用户消息之间的 agent 输出（文字/工具行/思考…）
 * 归入同一个 .turn 容器，左侧挂品牌头像（OpenWebUI 式对话流）。
 * 用户消息与分割线打断分组；切会话清空 log 后 isConnected 失效自动重开。 */
let agentTurn: HTMLElement | null = null; // 当前回合的内容列（.turn-body）
let answerContext: AnswerContext = {};
let turnFooter: ReturnType<typeof answerFooter> | null = null;

function resetAnswerContext() {
  agentTurn = null;
  turnFooter = null;
  answerContext = {};
}

function turnBody() {
  if (agentTurn && agentTurn.isConnected) return agentTurn;
  const turn = document.createElement("div");
  turn.className = "turn";
  const av = document.createElement("span");
  av.className = "turn-avatar";
  av.appendChild(agentIcon(S.current ? S.current.agent : "claude", 24));
  const body = document.createElement("div");
  body.className = "turn-body";
  turnFooter = answerFooter(body, answerContext);
  body.appendChild(turnFooter.el);
  turn.append(av, body);
  $("chat-log").appendChild(turn);
  agentTurn = body;
  return body;
}

export function appendChat(node: HTMLElement | null | undefined) {
  if (!node) return;
  const log = $("chat-log");
  const stick = nearBottom(log);
  const breaks = node.classList.contains("user") || node.classList.contains("chat-divider");
  if (breaks) {
    resetAnswerContext();
    log.appendChild(node);
  } else {
    const body = turnBody();
    if (node.classList.contains("result") && body.querySelector(".msg.agent")) {
      turnFooter!.setReceipt(node);
    } else body.insertBefore(node, turnFooter!.el);
    turnFooter!.update();
  }
  ensureWorking(); // 新内容后把执行指示重新压回末尾（仅运行中生效）
  updateHero();
  if (stick) log.scrollTop = log.scrollHeight;
  updateScrollBottomButton();
}

/* ---------------- 执行指示 ----------------
 * 仿 Claude 桌面端的「Contemplating」：运行中在对话流末尾挂一枚会动的品牌
 * 头像 + 阶段文案，取代旧的输入框上方浮动胶囊。头像动画由 chat.css 按品牌区分
 * （Claude 星芒脉动 / Codex 图标转圈），文案随阶段更新（思考 / 生成 / 运行工具…）。 */
let workingEl: HTMLElement | null = null;

function ensureWorking() {
  if (S.chatState !== "running") return;
  if (!workingEl) {
    const agent = S.current ? S.current.agent : "claude";
    const turn = document.createElement("div");
    turn.className = "turn working";
    const av = document.createElement("span");
    av.className = "turn-avatar";
    av.appendChild(agentIcon(agent, 24));
    const body = document.createElement("div");
    body.className = "turn-body";
    const label = document.createElement("span");
    label.className = "work-label";
    setTextRender(label, () => agent === "codex" ? i18nText("推理中…") : i18nText("思考中…"));
    body.append(label);
    turn.append(av, body);
    workingEl = turn;
  }
  $("chat-log").appendChild(workingEl); // 移到末尾（已在 DOM 中则只是重新排位）
}

function clearWorking() {
  if (workingEl) workingEl.remove();
  workingEl = null;
}

function setWorkingLabel(text: string | (() => string)) {
  const l = workingEl && workingEl.querySelector(".work-label");
  if (l) {
    if (typeof text === "function") setTextRender(l, text);
    else l.textContent = text;
  }
}

let disposeChat: (() => void) | undefined;
export function initChat() {
 disposeChat?.();
 legacyDraftVersions.clear();
 const lifetime = new AbortController();
 drafts=initComposerDrafts({
  read:composerDraft,
  apply:applyComposerDraft,
  validated:(paths,valid)=>{for(const info of chatImgs.map.values())info.valid=!!info.path&&valid[paths.indexOf(info.path)]===true;renderAttach();},
  changed:updateHero,
 });
 outbox=initChatOutbox({
  connected:()=>connection.ready,
  changed:()=>{const state=outbox?.executionState();if(state&&S.chatState!==state)setChatStatus(state);else updateHero();},
  move:expected=>{if(draftFingerprint(composerDraft())===draftFingerprint(expected))applyComposerDraft({text:"",attachments:[]});},
  restore:value=>{
   const current=composerDraft();if(current.text||current.attachments.length){toast(i18nText("输入框已有草稿，请先保存或清空后再复制。"),true);return;}
   applyComposerDraft({...value,attachments:value.attachments.map(a=>({...a,valid:false}))});void drafts?.validate();
  },
 });
 window.matchMedia("(max-width: 760px)").addEventListener("change", updateChatPlaceholder, { signal: lifetime.signal });
 updateChatPlaceholder();
 $("chat-reconnect").addEventListener("click", () => {
  if (!S.current) return;
  chatAttempt = 0;
  connectChat();
}, { signal: lifetime.signal });
$("chat-log").addEventListener("scroll", updateScrollBottomButton, { ...({ passive: true }), signal: lifetime.signal });
$("chat-scroll-bottom").addEventListener("click", () => {
  $("chat-log").scrollTo({
    top: $("chat-log").scrollHeight,
    behavior: window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth",
  });
}, { signal: lifetime.signal });
window.addEventListener("resize", updateScrollBottomButton, { signal: lifetime.signal });
$("chat-send").addEventListener("click", event => {
  if(event.detail>1)return;
  if (S.chatState === "running") sendInterrupt();
  else sendChat();
}, { signal: lifetime.signal });
$("chat-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !isImeEnter(e) && !enterInsertsNewline()) {
    e.preventDefault();
    sendChat();
  }
}, { signal: lifetime.signal });
$("chat-input").addEventListener("input", autoGrow, { signal: lifetime.signal });
$("chat-input").addEventListener("paste", (e) => {
  const files = pastedImages(e);
  if (!files.length || !S.current) return;
  e.preventDefault();
  for (const f of files) attachFile(f);
}, { signal: lifetime.signal });
$("btn-attach").addEventListener("click", () => $("attach-input").click(), { signal: lifetime.signal });
$("attach-input").addEventListener("change", () => {
  if (!S.current) return;
  for (const f of [...$<HTMLInputElement>("attach-input").files!]) attachFile(f);
  $<HTMLInputElement>("attach-input").value = "";
}, { signal: lifetime.signal });
$("btn-pick").addEventListener("click", (e) => {
  e.stopPropagation();
  const menu = $("pick-menu");
  if (menu.classList.contains("hidden")) {
    buildPickMain();
    menu.classList.remove("hidden");
  } else {
    closePickMenu();
  }
}, { signal: lifetime.signal });
$("pick-menu").addEventListener("mouseover", (e) => {
  if (!(e.target as Element).closest(".pick-row")) scheduleHideFly();
}, { signal: lifetime.signal });
$("pick-fly").addEventListener("mouseenter", cancelHideFly, { signal: lifetime.signal });
$("picker").addEventListener("mouseleave", scheduleHideFly, { signal: lifetime.signal });
document.addEventListener("click", (e) => {
  if (!(e.target as Element).closest(".picker")) closePickMenu();
}, { signal: lifetime.signal });
bus.addEventListener("models-updated", () => { void refreshModelCapabilities(); }, { signal: lifetime.signal });
bus.addEventListener("thread-changed", () => { reloadThread(); }, { signal: lifetime.signal });
$("chat-loading-retry").addEventListener("click", reloadThread, { signal: lifetime.signal });
 disposeChat = () => { if(lifetime.signal.aborted)return; lifetime.abort();outbox?.dispose();outbox=undefined; drafts?.dispose();drafts=undefined;chatTeardown(); };
 return disposeChat;
}

// Only redraw model controls; preserve the composer, chat stream and connections.
window.addEventListener("agentbox-language-change", () => {
  renderPickPill();
  closePickMenu();
});
