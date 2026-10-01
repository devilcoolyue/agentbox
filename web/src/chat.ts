/* chat：对话通道（WS）、历史加载、composer（发送/中断/附件/自增高）、
 * 模型与思考强度选择、空会话引导。渲染管线在 chat-render.ts。 */
"use strict";

import { actionButton, buttonLabel } from "./icons.js";

import { S, bus } from "./state.js";
import type { Pick } from "./state.js";
import type {
  AgentEvent, ChatMessage, History, LiveNode, ModelOption, StreamEvent, ReasoningCapability, SessionModels,
} from "./types.js";
import { $, spinEl, insertAtCursor, openLightbox, isMobile, onMobileChange, askPrompt, toast, isImeEnter, enterInsertsNewline } from "./util.js";
import { api, wsURL, imgURLFromPath, uploadAttachment } from "./api.js";
import { refreshAll } from "./data.js";
import { chip, renderUserMsg, renderEvent, renderEntry, liveNode, formatText, svgIcon, answerSources } from "./chat-render.js";
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

import { ChatConnection } from "./features/chat/connection.js";
const connection = new ChatConnection({
 message: data => { try { handleChatMsg(JSON.parse(data)); } catch (_) {} },
 state: (state, attempt) => { chatAttempt = attempt; setChatConn(state); },
 reconnect: () => { void loadHistory({ silent: true }); },
});
let chatAttempt = 0;

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
    text.textContent = chatAttempt > 1 ? `对话连接重连中…（第 ${chatAttempt} 次）` : "对话连接建立中…";
    btn.classList.add("hidden");
  } else if (state === "waking") {
    dot.className = "t-dot warn";
    text.textContent = "工作空间已休眠，正在唤醒…";
    btn.classList.add("hidden");
  } else {
    dot.className = "t-dot bad";
    text.textContent = "对话连接已断开，消息无法发送";
    btn.classList.remove("hidden");
  }
}

export function connectChat() {
 if (S.current) connection.connect(wsURL(`/sessions/${S.current.id}/chat`));
}

/* 切换/删除会话时收尾：作废重连定时器、关连接、复位状态 */
let chatEpoch = 0;
export function chatTeardown() {
  ++chatEpoch;
  modelsAbort?.abort();
  pendingPick = null;
  sessionModels = null;
  manualEffort = false;
  waking = false;
  connection.dispose();
  setChatConn("connected");
  cancelHistoryLoad();
  replayReset();
  resetAnswerContext();
  setChatStatus("idle");
}

/* 打字机状态：正在逐字放出的一段文本块。真流式与 codex 整段回放共用这个结构，
 * replay / rate 只在回放时出现（真流式按积压动态加速，不定速）。 */
interface LiveBlock extends LiveNode {
  /** "text" | "thinking" */
  kind: string;
  /** 已上屏的部分 */
  shown: string;
  /** 尚未放出的缓冲 */
  buf: string;
  /** 上一帧攒下的不足一个字的余量 */
  carry: number;
  /** 上一帧的时间戳 */
  tick: number;
  /** requestAnimationFrame 句柄，0 = 当前没有在跑 */
  raf: number;
  /** codex 整段回放标记 */
  replay?: boolean;
  /** 回放定速（字/秒）；真流式为 undefined */
  rate?: number;
}

/* ---------------- 流式增量渲染 ----------------
 * Claude：--include-partial-messages 下，完整 assistant 事件之前会先收到
 * stream_event（content_block_start/delta/stop）。delta 到达节奏不均
 * （常一次一整句），直接上屏会一段一段蹦：先进缓冲，由 rAF 打字机
 * 匀速放出，积压越多放得越快，显示只滞后生成零点几秒。text 块每次
 * 放出后整段重跑 Markdown（与最终渲染同一管线），代码块、列表边生成
 * 边呈现；完整事件到达后移除临时节点交回 renderEvent 正式渲染，
 * 两边产物一致，替换无观感跳变。增量事件服务端只广播不落盘，
 * 历史回放天然只有完整事件。
 *
 * Codex：服务端经 app-server 协议拿到真增量，翻译成与 Claude 相同的
 * stream_event 形状广播，这条管线原样消费——真流式与 Claude 无异；
 * 完整 item.completed 到达时同样移除临时节点交回正式渲染。
 *
 * 兜底（旧容器里的 codex 走 exec --json，没有增量，全文只在
 * item.completed 一次性到达）：把全文喂进同一条 rAF 管线做「整段回放」，
 * 速度取 max(140 字/秒, 全文/3s)，短文按打字节奏、长文封顶 3 秒放完。
 * 回放中到达的其余事件（工具行、回合完成章等）排队，动画完成后按序
 * 补上；回放不因回合结束被截断，但新用户消息 / 出错 / 切线程时立刻放完。 */

let liveEls: HTMLElement[] = []; // 本条消息的临时节点，完整事件到达后整体移除
let liveBlock: LiveBlock | null = null; // 当前追加目标

/* 回合结束收尾：缓冲余量上屏，临时节点保留在页面上（中断时留住
 * 已生成的部分），只是不再跟踪、去掉光标 */
function streamSettle() {
  if (liveBlock && liveBlock.replay) return; // codex 回放自行收尾，不被回合结束截断
  liveDrain();
  for (const el of liveEls) el.classList.remove("streaming");
  liveEls = [];
  liveBlock = null;
  turnFooter?.update();
}

function liveClear() {
  if (liveBlock && liveBlock.raf) cancelAnimationFrame(liveBlock.raf);
  for (const el of liveEls) el.remove();
  liveEls = [];
  liveBlock = null;
}

function handleAgentEvent(ev: AgentEvent | undefined) {
  if (ev && ev.type === "stream_event") { handleStreamEvent(ev.event); return; }
  // 完整 assistant 事件带全部内容，先移除对应的增量节点再正式渲染
  if (ev && ev.type === "assistant") liveClear();
  workLabelFromEvent(ev);
  if (replayFeed(ev)) return; // codex 整段文本 → 打字机回放
  for (const node of renderEvent(ev)) replayAppend(node);
}

/** 回放队列项：带 node 的是排在动画之后补上的成品节点，否则是待打字的文本任务 */
interface ReplayJob {
  node?: HTMLElement;
  kind?: string;
  text?: string;
}

/* ---- codex 整段回放队列 ---- */

let replayQueue: ReplayJob[] = [];

function replayActive() { return !!(liveBlock && liveBlock.replay) || replayQueue.length > 0; }

/* codex 的文本事件（新旧两种结构）转打字任务；消费掉返回 true */
function replayFeed(ev: AgentEvent | undefined) {
  if (!ev || typeof ev !== "object") return false;
  let kind = "", text = "";
  if (ev.type === "item.completed" && ev.item) {
    if (ev.item.type === "agent_message") { kind = "text"; text = ev.item.text!; }
    else if (ev.item.type === "reasoning") { kind = "thinking"; text = ev.item.text!; }
  } else if (ev.msg && typeof ev.msg === "object") {
    if (ev.msg.type === "agent_message") { kind = "text"; text = ev.msg.message!; }
    else if (ev.msg.type === "agent_reasoning") { kind = "thinking"; text = ev.msg.text!; }
  }
  if (!kind || !text) return false;
  // 真流式（app-server 增量）已把这条消息现场打出来：清掉临时节点交回
  // 正式渲染（与 Claude 完整事件同一套路），不再回放
  if (liveEls.length) { liveClear(); return false; }
  replayQueue.push({ kind, text });
  replayPump();
  return true;
}

/* 回放进行中时后续事件的节点排队，保持时间线顺序；空闲时直接上屏 */
function replayAppend(node: HTMLElement) {
  if (replayActive()) replayQueue.push({ node });
  else appendChat(node);
}

function replayPump() {
  if (liveBlock) return; // 已有动画在放
  while (replayQueue.length) {
    const job = replayQueue.shift()!;
    if (job.node) { appendChat(job.node); continue; }
    setWorkingLabel(job.kind === "thinking" ? "推理中…" : "生成回复…");
    liveBlock = {
      ...liveNode(job.kind!), kind: job.kind!, shown: "", buf: "", carry: 0, tick: 0, raf: 0,
      replay: true, rate: Math.max(140, job.text!.length / 3),
    };
    liveBlock.el.classList.add("streaming");
    appendChat(liveBlock.el);
    liveFeed(job.text!);
    return;
  }
}

/* 一段回放放完：定格为与正式渲染一致的形态，接着放下一个排队项 */
function replayFinish() {
  const b = liveBlock;
  if (!b || !b.replay) return;
  if (b.raf) cancelAnimationFrame(b.raf);
  b.el.classList.remove("streaming", "live-text");
  if (b.kind === "thinking") (b.el as HTMLDetailsElement).open = false; // 与正式渲染一致：思考默认折叠
  liveBlock = null;
  turnFooter?.update();
  replayPump();
}

/* 立刻放完全部积压：新用户消息 / 出错时调用，保证时间线完整不乱序 */
function replayFlush() {
  while (liveBlock && liveBlock.replay) liveDrain();
}

/* 丢弃回放状态：切线程 / 切会话时调用，对话流马上要整体重建 */
function replayReset() {
  replayQueue = [];
  if (liveBlock && liveBlock.replay) {
    if (liveBlock.raf) cancelAnimationFrame(liveBlock.raf);
    liveBlock = null;
  }
}

/* 依据整包事件刷新执行指示的阶段文案（流式的文字/思考在 handleStreamEvent 里更新） */
function workLabelFromEvent(ev: AgentEvent | undefined) {
  if (!ev || S.chatState !== "running") return;
  if (ev.type === "assistant" && ev.message && Array.isArray(ev.message.content)) {
    for (const b of ev.message.content) {
      if (b.type === "tool_use") { setWorkingLabel("运行工具 " + b.name + "…"); return; }
    }
  } else if (ev.type === "item.started" && ev.item && ev.item.type === "command_execution") {
    setWorkingLabel("执行命令…");
  } else if (ev.type === "item.completed" && ev.item && ev.item.type === "reasoning") {
    setWorkingLabel("推理中…");
  }
}

function handleStreamEvent(e: StreamEvent | undefined) {
  if (!e || typeof e !== "object") return;
  switch (e.type) {
    case "message_start":
      // 正常流程上一条已被完整事件清掉（此处空转）；若断连错过了完整
      // 事件则保留残留文本，只停止跟踪，避免删掉用户已看到的内容
      streamSettle();
      break;
    case "content_block_start": {
      liveDrain(); // 上一块若有残余（丢了 stop 事件）先补齐
      const t = e.content_block && e.content_block.type;
      if (t === "text" || t === "thinking") {
        setWorkingLabel(t === "thinking" ? "思考中…" : "生成回复…");
        liveBlock = { ...liveNode(t), kind: t, shown: "", buf: "", carry: 0, tick: 0, raf: 0 };
        liveBlock.el.classList.add("streaming");
        appendChat(liveBlock.el);
        liveEls.push(liveBlock.el);
      } else {
        liveBlock = null; // tool_use 等交给完整事件渲染
      }
      break;
    }
    case "content_block_delta": {
      if (!liveBlock || !e.delta) break;
      const txt = e.delta.type === "text_delta" ? e.delta.text
        : e.delta.type === "thinking_delta" ? e.delta.thinking : "";
      if (txt) liveFeed(txt);
      break;
    }
    case "content_block_stop":
      if (liveBlock) {
        liveDrain();
        liveBlock.el.classList.remove("streaming");
      }
      liveBlock = null;
      break;
  }
}

/* ---- 打字机：缓冲 → rAF 匀速放出 ---- */

function liveFeed(text: string) {
  const b = liveBlock!;
  b.buf += text;
  if (document.hidden) { liveDrain(); return; } // 后台标签页 rAF 停摆，直接上屏
  if (!b.raf) {
    b.tick = performance.now();
    b.raf = requestAnimationFrame(liveTick);
  }
}

function liveTick(now: number) {
  const b = liveBlock;
  if (!b || !b.raf) return;
  b.raf = 0;
  const dt = Math.min(now - b.tick, 200);
  if (dt >= 33) { // Markdown 整段重渲染有成本，帧率封顶 ~30fps
    b.tick = now;
    // 真流式：基础 80 字/秒，按积压加速（约 0.4s 追平），滞后有上界；
    // codex 回放：全文早已到齐，按定速放（rate 已封顶总时长）
    b.carry += (dt / 1000) * (b.rate || Math.max(80, b.buf.length * 2.5));
    const n = Math.min(b.buf.length, Math.floor(b.carry));
    if (n > 0) {
      b.carry -= n;
      b.shown += b.buf.slice(0, n);
      b.buf = b.buf.slice(n);
      liveRender(b);
    }
  }
  if (b.buf) b.raf = requestAnimationFrame(liveTick);
  else if (b.replay) replayFinish();
  else b.carry = 0;
}

/* 缓冲余量一次性上屏（块结束/回合收尾/后台标签页时用） */
function liveDrain() {
  const b = liveBlock;
  if (!b) return;
  if (b.raf) { cancelAnimationFrame(b.raf); b.raf = 0; }
  if (b.buf) {
    b.shown += b.buf;
    b.buf = "";
    liveRender(b);
  }
  if (b.replay) replayFinish();
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





function liveRender(b: LiveBlock) {
  const log = $("chat-log");
  const stick = nearBottom(log);
  if (b.kind === "text") {
    b.body.replaceChildren(formatText(b.shown));
    answerSources.set(b.el, b.shown);
    if (turnFooter && b.shown) turnFooter.el.hidden = false;
  }
  else b.body.textContent = b.shown; // thinking 与最终渲染一致，保持纯文本
  if (stick) log.scrollTop = log.scrollHeight;
}

function handleChatMsg(msg: ChatMessage) {
  switch (msg.type) {
    case "user_message":
      replayFlush(); // 上一回合的回放立刻放完，不让新消息插到它前面
      noteThreadTitle(msg.text!);
      appendChat(renderUserMsg(msg.text));
      answerContext.turn = msg.turn;
      break;
    case "agent_event":
      observeAnswer(answerContext, msg.event, msg.ts);
      handleAgentEvent(msg.event);
      turnFooter?.update();
      break;
    case "agent_raw":
      replayAppend(chip(msg.text!)); // 回放中则排队，保持时间线顺序
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
      if (!answerContext.ts && msg.ts) answerContext.ts = msg.ts;
      setChatStatus(msg.state!, msg.error);
      if (msg.state === "error" && msg.retry_text) {
        const input = $<HTMLTextAreaElement>("chat-input");
        if (!input.value.trim()) { input.value = msg.retry_text; autoGrow(); }
        const retry = document.createElement("button");
        retry.className = "btn btn-sm"; actionButton(retry, "重试", "refresh", "恢复默认设置并重试本轮对话");
        retry.addEventListener("click", () => {
          S.pick.effort = ""; pendingPick = null; manualEffort = false; savePick(); renderPickPill();
          if (input.value.trim() && input.value.trim() !== msg.retry_text) {
            toast("已恢复默认，请确认当前输入后发送"); return;
          }
          input.value = msg.retry_text!; autoGrow(); retry.remove(); sendChat();
        }, { once: true });
        appendChat(retry);
      }
      if (msg.state === "idle" || msg.state === "error") refreshAll();
      break;
    case "error":
      appendChat(chip(msg.error!, "err"));
      break;
  }
}

/* 发送按钮双态：空闲=琥珀纸飞机发送，执行中=红色 ■ 中断。
 * 图标是按钮里的两个 SVG，靠 .stop class 切换显隐，别用 textContent 覆盖。
 * 执行进度不再用输入框上方的浮动胶囊，而是在对话流末尾挂一枚会动的品牌头像
 * + 阶段文案（详见 ensureWorking）。 */
export function setChatStatus(state: string, error?: string) {
  S.chatState = state === "running" ? "running" : "idle";
  if (state === "error") replayFlush(); // 出错立刻放完，错误提示紧随其后
  if (S.chatState !== "running") streamSettle();
  const send = $<HTMLButtonElement>("chat-send");
  if (state === "running") {
    // 思考指示接在末尾，若用户正跟读（贴底）就滚出来，免得藏在折叠线下方
    const log = $("chat-log");
    const stick = nearBottom(log);
    ensureWorking();
    if (stick) log.scrollTop = log.scrollHeight;
    send.classList.add("stop");
    setTip(send, "中断");
    send.ariaLabel = "中断"; // 纯图标按钮，可访问名称得跟着状态走
    send.disabled = false;
  } else {
    clearWorking();
    send.classList.remove("stop");
    setTip(send, "发送");
    send.ariaLabel = "发送";
    send.disabled = chatImgs.uploading > 0;
    if (state === "error" && error) appendChat(chip(error, "err"));
  }
  updateHero();
}



/* 占位提示：窄屏一行放不下长文案，且键盘快捷键提示在手机上无意义 */
function updateChatPlaceholder() {
  $<HTMLTextAreaElement>("chat-input").placeholder = isMobile() || enterInsertsNewline()
    ? "向 Agent 下达任务…"
    : "随心输入，向 Agent 下达任务…（Enter 发送，Shift+Enter 换行，可粘贴图片）";
}


/* 输入框随内容自动增高（1 行起步，封顶后内部滚动） */

export function autoGrow() {
  const t = $("chat-input");
  t.style.height = "auto";
  t.style.height = Math.min(t.scrollHeight, 220) + "px";
}

export function sendChat() {
  let text = $<HTMLTextAreaElement>("chat-input").value.trim();
  if (!text || S.chatState === "running") return;
  if (chatImgs.uploading > 0) {
    appendChat(chip("附件仍在上传中，请稍候…", "err"));
    return;
  }
  if (!connection.ready) {
    // 多半是会话空闲休眠后连接被断开。别让用户先去点启动：重新连一次即可，
    // 服务端的 chat 通道会幂等地把容器拉起来，连上后自动把这条消息发出去。
    wakeAndSend();
    return;
  }
  // [Image #N] / [File #N] 占位符替换为容器内真实路径，Agent 可直接读取
  for (const [n, info] of chatImgs.map) {
    if (!info.path) continue;
    text = text.split(`[Image #${n}]`).join(`[图片#${n} ${info.path}]`);
    text = text.split(`[File #${n}]`).join(`[附件#${n} ${info.path}]`);
  }
  connection.send(JSON.stringify({
    type: "user_message", text,
    model: S.pick.model, effort: S.pick.effort, effort_control: currentReasoning().control,
  }));
  $<HTMLTextAreaElement>("chat-input").value = "";
  autoGrow();
  resetChatImgs();
}

/* 唤醒并重试发送：立刻重连（服务端顺带拉起容器），连上后把输入框里的内容
 * 发出去。等待上限 60 秒——冷启容器 + 播种凭证通常几秒内完成。 */
let waking = false;
async function wakeAndSend() {
  if (waking) return;
  waking = true;
  setChatConn("waking");
  const send = $<HTMLButtonElement>("chat-send");
  send.disabled = true;
  chatAttempt = 0;
  connectChat();
  const epoch = chatEpoch;
  const deadline = Date.now() + 60000;
  try {
    while (Date.now() < deadline) {
      if (!S.current || epoch !== chatEpoch) return;
      if (connection.ready) {
        waking = false;
        send.disabled = false;
        sendChat(); // 连上了，把用户刚才那条发出去
        refreshAll(); // 状态从「休眠」翻回运行中
        return;
      }
      await new Promise((r) => setTimeout(r, 400));
    }
    appendChat(chip("唤醒工作空间超时，请稍后重试或手动启动工作空间", "err"));
  } finally {
    if (epoch !== chatEpoch) return;
    waking = false;
    send.disabled = chatImgs.uploading > 0;
  }
}

function sendInterrupt() {
  if (connection.ready) {
    connection.send(JSON.stringify({ type: "interrupt" }));
  }
}

/* ---------------- 附件（粘贴图片 / 上传文件） ---------------- */

const chatImgs = { seq: 0, map: new Map(), uploading: 0 }; // n -> {path, name, orig, kind}

export function resetChatImgs() {
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
    if (info.kind === "file") {
      const box = document.createElement("div");
      box.className = "attach-file mono";
      const name = document.createElement("span");
      name.className = "fn";
      name.textContent = info.orig || info.name || "附件";
      if (info.path) {
        box.append(document.createTextNode("📄"), name);
        setTip(box, `附件 #${n} · ${info.path}`);
      } else {
        box.append(spinEl(), name);
      }
      strip.appendChild(box);
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
      img.addEventListener("click", () => openLightbox(imgURLFromPath(info.path), `图片 #${n} · ${info.path}`));
      box.appendChild(img);
    } else {
      const up = document.createElement("span");
      up.className = "up";
      up.append(spinEl(), document.createTextNode("上传中"));
      box.appendChild(up);
    }
    strip.appendChild(box);
  }
  // 附件没传完不许发送（执行中按钮是「中断」，不能动）
  if (S.chatState !== "running") $<HTMLButtonElement>("chat-send").disabled = chatImgs.uploading > 0;
}

async function attachFile(file: File) {
  const isImg = (file.type || "").startsWith("image/");
  const tag = isImg ? "Image" : "File";
  const n = ++chatImgs.seq;
  chatImgs.map.set(n, { path: "", name: "", orig: file.name || "", kind: isImg ? "img" : "file" });
  insertAtCursor($("chat-input"), `[${tag} #${n}]`);
  autoGrow();
  chatImgs.uploading++;
  renderAttach();
  try {
    const res = await uploadAttachment(file);
    chatImgs.map.set(n, { ...res, kind: isImg ? "img" : "file" });
  } catch (e) {
    chatImgs.map.delete(n);
    $<HTMLTextAreaElement>("chat-input").value = $<HTMLTextAreaElement>("chat-input").value.replace(`[${tag} #${n}]`, "");
    appendChat(chip("附件上传失败：" + (e as Error).message, "err"));
  } finally {
    chatImgs.uploading--;
    renderAttach();
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
let pendingPick: { effort: string; control?: string } | null = null;
function currentReasoning(): ReasoningCapability {
  const capability = modelOpts().find(m => m.id === S.pick.model)?.reasoning || sessionModels?.default_reasoning;
  return { ...defaultReasoning(pickStyle()), ...capability };
}
function effortOpts(): EffortOpt[] {
  const r = currentReasoning();
  const opts: EffortOpt[] = [{ v: "", l: "默认强度", sub: "沿用模型和客户端的默认设置，不等于关闭推理" }];
  if (r.support === "unsupported" || (r.support === "unknown" && !manualEffort)) return opts;
  const levels = r.support === "supported" ? r.levels || [] : allowedLevels(pickStyle(), r.control);
  return [...opts, ...levels.map(v => ({ v, l: EFFORT_LABELS[v] || v, sub: r.control === "budget" ? `预算上限 ${BUDGETS[v].toLocaleString("en-US")} tokens` : v }))];
}
async function refreshModelCapabilities() {
  const id = S.current?.id, epoch = chatEpoch;
  if (!id) return;
  modelsAbort?.abort();
  const abort = new AbortController(); modelsAbort = abort;
  try {
    const value = await api<SessionModels>(`/sessions/${id}/models`, { signal: abort.signal });
    if (abort.signal.aborted || epoch !== chatEpoch || S.current?.id !== id) return;
    if (!Array.isArray(value.models)) throw new Error("模型能力响应无效");
    let previous = currentReasoning();
    sessionModels = value;
    if (pendingPick) {
      const saved = pendingPick; pendingPick = null;
      const r = currentReasoning();
      manualEffort = !!saved.effort && saved.control === r.control;
      if (manualEffort && effortOpts().some(o => o.v === saved.effort)) S.pick.effort = saved.effort;
      previous = r;
    }
    const next = currentReasoning();
    if (S.pick.effort && !(next.support === "unknown" && previous.control === next.control && manualEffort) && !canKeepEffort(previous, next, S.pick.effort)) {
      S.pick.effort = ""; manualEffort = false;
      toast("模型能力已变化，已恢复为默认强度");
    }
    savePick(); renderPickPill(); closePickMenu();
  } catch (error) {
    if (!abort.signal.aborted && epoch === chatEpoch) toast("读取模型能力失败：" + (error as Error).message, true);
  }
}
/* 尾部 [1m] 是 Claude Code 的 1M 上下文后缀（opus[1m] 等），与后端 modelRe 保持一致 */
export const MODEL_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(\[1m\])?$/;

function modelOpts(): ModelOption[] {
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
  if (currentReasoning().support === "unsupported") return "不支持调整";
  return (effortOpts().find(o => o.v === v) || effortOpts()[0]).l;
}

function workspaceModel() {
  return S.current?.default_model || (S.current?.agent === "codex" ? "gpt-5.5" : "claude-opus-5");
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
    textContent: `${modelLabel(S.pick.model)} · ${effortLabel(S.pick.effort)}`,
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
function effortTitle() { return currentReasoning().control === "budget" ? "思考预算" : "推理强度"; }
function customModel() {
  return S.pick.model && !modelOpts().some((o) => o.id === S.pick.model) ? S.pick.model : "";
}

function choose(kind: string, v: string) {
  pendingPick = null;
  if (kind === "model") {
    const before = currentReasoning();
    S.pick.model = v || workspaceModel();
    if (!canKeepEffort(before, currentReasoning(), S.pick.effort)) {
      S.pick.effort = "";
      toast("新模型的支持范围不同，已恢复为默认强度");
    }
    manualEffort = false;
  } else S.pick.effort = v;
  savePick();
  renderPickPill();
  closePickMenu();
}

async function askCustomModel() {
  const v = await askPrompt({
    title: "自定义模型",
    label: "模型 ID",
    value: customModel(),
    hint: `留空恢复为 ${modelLabel(workspaceModel())}。`,
    validate: (s) => (s.trim() && !MODEL_ID_RE.test(s.trim())
      ? "模型 ID 格式不合法（字母数字开头，可含 . _ -）" : ""),
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
      info.textContent = "此模型不支持调整。CLI 中已配置的强度可能仍需清除。";
      return [info];
    }
    const opts: HTMLElement[] = effortOpts().map(o => pickOpt(o.l, o.sub || "", S.pick.effort === o.v, () => choose("effort", o.v)));
    if (r.support === "unknown") {
      const note = document.createElement("p"); note.className = "muted";
      note.textContent = "支持情况未知，手动指定可能被模型或中转服务拒绝。";
      opts.unshift(note);
      if (!manualEffort) opts.push(pickOpt("手动指定…", "仅在确认服务支持时使用", false, () => {
        manualEffort = true; hideFly(); buildPickSub("effort");
      }));
    }
    return opts;
  }
  const out: HTMLElement[] = [];
  for (const o of modelOpts()) {
    out.push(pickOpt(o.label, o.id, S.pick.model === o.id, () => choose("model", o.id)));
  }
  const cur = customModel();
  out.push(pickOpt("自定义模型…", cur, !!cur, askCustomModel));
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
    head.textContent = kind === "model" ? "模型" : effortTitle();
    fly.replaceChildren(head, ...optList(kind));
    fly.dataset.kind = kind;
  }
  for (const r of document.querySelectorAll(".pick-row.open")) r.classList.remove("open");
  row.classList.add("open");
  fly.classList.remove("hidden");
  // 与悬停行顶端对齐，贴在面板左侧；超出视口时上下回拉
  fly.style.right = menu.offsetWidth + 6 + "px";
  const top = menu.offsetTop + row.offsetTop - menu.scrollTop;
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
  if (pickStyle() === "codex") {
    menu.append(
      pickRow("模型", modelLabel(S.pick.model), "model"),
      pickRow(effortTitle(), effortLabel(S.pick.effort), "effort"),
    );
    return;
  }
  const head = document.createElement("div");
  head.className = "pick-fly-h";
  head.textContent = "模型";
  const sep = document.createElement("div");
  sep.className = "pick-sep";
  menu.append(head, ...optList("model"), sep, pickRow(effortTitle(), effortLabel(S.pick.effort), "effort"));
}

/* 移动端二级面板（无悬停可用） */
function buildPickSub(kind: string) {
  const menu = $("pick-menu");
  menu.replaceChildren();
  const h = document.createElement("button");
  h.className = "pick-back";
  buttonLabel(h, kind === "model" ? "模型" : effortTitle(), "chevron-left");
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
  $("chat-loading-text").textContent = failed ? S.histError : "正在加载历史对话…";
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
    $<HTMLButtonElement>("chat-send").disabled = blocked || chatImgs.uploading > 0;
  }
}

for (const c of document.querySelectorAll<HTMLElement>(".hero-pill")) {
  c.addEventListener("click", () => {
    insertAtCursor($<HTMLTextAreaElement>("chat-input"), c.dataset.fill!);
    autoGrow();
  });
}

/* ---------------- 历史对话（当前线程） ---------------- */

const HISTORY_TIMEOUT = 15_000;
let histGen = 0;   // 并发重载守卫：作废在途的旧加载，防止内容追加进新视图
let histCtrl: AbortController | null = null;

function historyLoadIsStale(sessID: string, gen: number) {
  // refreshAll 每 8 秒会用接口返回的新对象刷新 S.current。这里只能比较稳定
  // 的会话 id；比较对象引用会把普通轮询误判成切换会话，并永远留下加载态。
  return !S.current || S.current.id !== sessID || gen !== histGen;
}

function cancelHistoryLoad() {
  histGen++;
  if (histCtrl) histCtrl.abort();
  histCtrl = null;
  S.histLoading = false;
  S.histError = "";
}

/* opts.silent：重连后的对齐式补拉——不显示加载态（页面已有内容，闪一下
 * 加载中反而像出了故障），拉到后整体替换对话流以补上断线期间错过的事件，
 * 并保持用户原本的阅读位置（除非本来就贴着底）。 */
export async function loadHistory(opts: { silent?: boolean } = {}) {
  const sess = S.current; if (!sess) return;
  const silent = !!opts.silent;
  if (histCtrl) histCtrl.abort();
  const gen = ++histGen;
  const ctrl = new AbortController();
  histCtrl = ctrl;
  let timedOut = false;
  let loaded = false;
  const timer = setTimeout(() => {
    timedOut = true;
    ctrl.abort();
  }, HISTORY_TIMEOUT);
  const log = $("chat-log");
  const wasAtBottom = silent ? nearBottom(log) : true;
  const prevScroll = log.scrollTop;
  if (!silent) {
    S.histLoading = true;
    S.histError = "";
    updateHero(); // 中央显示加载态，加载完一次性呈现，避免先闪新会话引导页
  }
  try {
    // 只返回当前对话线程的全文；其余线程在历史对话面板（chat-threads.js）
    // 里列表展示，切换后经 reloadThread 重新加载
    const { entries, thread, costs } = await api<History>(`/sessions/${sess.id}/history`, { signal: ctrl.signal });
    if (historyLoadIsStale(sess.id, gen)) return;
    setThreadBar(thread);
    replayReset();
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
    loaded = true;
  } catch (e) {
    if (historyLoadIsStale(sess.id, gen)) return;
    if (silent) return; // 补拉失败不打扰：下次重连或手动刷新还有机会
    S.histError = timedOut
      ? "历史对话加载超时"
      : "历史对话加载失败：" + ((e as Error).message || "未知错误");
  } finally {
    clearTimeout(timer);
    if (histCtrl === ctrl) histCtrl = null;
    if (!historyLoadIsStale(sess.id, gen)) {
      if (!silent) {
        S.histLoading = false;
        updateHero();
      }
      if (loaded) {
        log.scrollTop = wasAtBottom ? log.scrollHeight : prevScroll;
        updateScrollBottomButton();
      }
    }
  }
}

/* 对话线程发生切换（本端操作经 bus，或其它页面广播）后重载对话流 */
export async function reloadThread() {
  if (!S.current) return;
  histGen++; // 立刻作废在途加载，clear 之后它们不得再往里追加
  replayReset(); // 回放动画与积压一并丢弃，历史里有完整内容
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
    label.textContent = agent === "codex" ? "推理中…" : "思考中…";
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

function setWorkingLabel(text: string) {
  const l = workingEl && workingEl.querySelector(".work-label");
  if (l) l.textContent = text;
}

let disposeChat: (() => void) | undefined;
export function initChat() {
 disposeChat?.();
 const lifetime = new AbortController();
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
$("chat-send").addEventListener("click", () => {
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
 disposeChat = () => { lifetime.abort(); chatTeardown(); };
 return disposeChat;
}
