/* term：终端页 —— xterm 实例、PTY WebSocket（含自动重连）、连接状态与顶栏提示轮播、
 * 终端内粘贴图片上传。 */
"use strict";

import { S, bus } from "./state.js";
import type { TerminalTips, UsageEvents, UsageTotals } from "./types.js";
import { $, isMobile, openLightbox, toast } from "./util.js";
import { api, wsURL, imgURLFromPath, uploadAttachment } from "./api.js";
import { fmtUSD } from "./quota.js";
import { refreshAll } from "./data.js";
import { pastedImages } from "./chat.js";
import { setTip } from "./tip.js";
import { attachTermInputDebug, traceTermInput } from "./term-input-debug.js";
import { attachTouchScroll } from "./term-touch.js";
import { menuJustDismissed, openMenuAt } from "./menu.js";

const TerminalClass = window.Terminal && (window.Terminal.Terminal || window.Terminal);
const FitAddonClass = window.FitAddon && (window.FitAddon.FitAddon || window.FitAddon);
const WebglAddonClass = window.WebglAddon && (window.WebglAddon.WebglAddon || window.WebglAddon);

const ENC = new TextEncoder();
const IMG_PATH_RE = /\/shared\/\.images\/[A-Za-z0-9._-]+/g;

// 复用同一个 xterm 实例跨重连能保留回滚，但也把上一个程序（如 claude）开启的 DEC
// 私有模式一起带了过来：鼠标上报、备用屏、括号粘贴、焦点上报、应用光标键等。而服务端
// 每次重连都起一个全新的 bash（旧 claude 被强杀，来不及复位），于是新 shell 前台却还
// 开着鼠标上报——鼠标一动就把每次移动编码成 \x1b[<...M 发给 bash，回显成乱码、按回车
// 又被当命令执行（见 0723 报告）。故每次新连接建立时先复位这些输入相关的模式，让 xterm
// 与新 shell 对齐；首连时 xterm 本就是默认态，这串复位是空操作，安全。不清屏、不丢回滚。
const RESET_INPUT_MODES =
  "\x1b[?9l\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l" + // 各类鼠标追踪关
  "\x1b[?1004l" +                                          // 焦点上报关
  "\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l" +         // 鼠标编码关
  "\x1b[?2004l" +                                          // 括号粘贴关
  "\x1b[?1l\x1b>" +                                        // 光标键/小键盘改回普通模式
  "\x1b[?1049l\x1b[?1047l\x1b[?47l" +                      // 退出备用屏
  "\x1b[?25h\x1b[?7h";                                     // 显示光标、自动换行

const token = (name: string) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

/** 顶栏状态：connecting/connected/reconnecting 各有专属文案，其余一律按 closed 处理 */
type ConnState = "connecting" | "connected" | "reconnecting" | "closed";

/* ---------------- 连接状态展示（顶栏右侧：状态点 + 文案 + 重连钮） ---------------- */

function setConnStatus(state: ConnState) {
  syncTermKeys(state === "connected");
  const dot = $("term-conn-dot");
  const st = $("term-state");
  const btn = $("term-reconnect");
  if (!st) return;
  let cls, text;
  switch (state) {
    case "connecting":   cls = "warn"; text = "连接中…"; break;
    case "connected":    cls = "good"; text = "shell 已连接"; break;
    case "reconnecting": cls = "warn"; text = reconnectAttempt > 1 ? `重连中…（第 ${reconnectAttempt} 次）` : "重连中…"; break;
    default:             cls = "bad"; state = "closed"; text = "已断开，点右侧按钮重连"; break;
  }
  if (dot) dot.className = "t-dot " + cls;
  st.textContent = text;
  st.dataset.state = state;
  if (btn) btn.classList.toggle("busy", state === "connecting" || state === "reconnecting");
}

/* ---------------- 自动重连 ---------------- */

// 退避序列（毫秒），封顶后一直用最后一档；成功连上或手动重连时归零。
const RECONNECT_BACKOFF = [1000, 2000, 3000, 5000, 8000, 10000];
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let reconnectAttempt = 0;

function clearReconnect() {
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
}

// 断线后排一次重连——仅当用户仍停留在终端页时才自动重连，切走了就停在「已断开」，
// 回到终端页（setTab → openTerm）会重新拉起。
function scheduleReconnect() {
  if (S.tab !== "term" || !S.current) { setConnStatus("closed"); return; }
  const delay = RECONNECT_BACKOFF[Math.min(reconnectAttempt, RECONNECT_BACKOFF.length - 1)];
  reconnectAttempt++;
  setConnStatus("reconnecting");
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    if (S.tab !== "term" || !S.current) { setConnStatus("closed"); return; }
    connectTermWS(true);
  }, delay);
}

/* ---------------- 终端实例与 WebSocket ---------------- */

// 仅断开连接（停止容器时）：作废重连、关连接，保留终端画面与回滚缓冲。
export function termDisconnect() {
  S.termWSGen++;
  clearReconnect();
  reconnectAttempt = 0;
  if (S.termWS) { S.termWS.close(); S.termWS = null; }
  $("term-loading").classList.add("hidden");
  setConnStatus("closed");
}

// 完全收尾（切换/删除会话时）：连实例一起销毁。
export function termTeardown() {
  termDisconnect();
  if (S.term) { S.term.dispose(); S.term = null; S.fit = null; }
  fitTermViewport();
}

// 手动重连：作废旧连接与在途重连，保留已有终端实例（不丢回滚），立即重新连接。
export function termReconnect() {
  if (!S.current) return;
  S.termWSGen++;
  clearReconnect();
  reconnectAttempt = 0;
  if (S.termWS) { try { S.termWS.close(); } catch (_) {} S.termWS = null; }
  ensureTerm();
  if (!S.term) return;
  connectTermWS(false);
}

// Chrome（macOS 实测）对聚焦中的 textarea 切换过 readOnly（disableStdin 的底层实现）
// 后，IME 可能就此失灵——打拼音直接上屏字母，点页面别处再点回终端才恢复。这里主动做
// 一轮 blur→focus 刷新 IME 上下文；textarea 本就没聚焦时直接 focus，与原行为一致。
function refocusTerm() {
  if (!S.term) return;
  const ta = S.term.textarea;
  if (ta && document.activeElement === ta) {
    ta.blur();
    requestAnimationFrame(() => { if (S.term) S.term.focus(); });
  } else {
    S.term.focus();
  }
}

/* iOS Safari 的中文键盘直接上屏的标点（「，」等）会被 xterm 6 丢掉。xterm 只在三处发送：
 * keydown（要求可打印键码）、keypress、以及键码 229 时 setTimeout(0) 里的 textarea 差分；
 * 而 iOS 可能不给可打印键码、不发 keypress，且把字写进 textarea 晚于那次差分；最后到达的
 * insertText 输入事件又因「已见过 keydown」被 _inputEvent 跳过（上游 xtermjs/xterm.js#3070，
 * 修复 PR #5614 未合并）。这里等 xterm 所有发送时机都过去再看：从这次按键开始 xterm 一个字
 * 都没发、也不在组字，才由我们补发，因此不会重复。监听挂在外层捕获阶段，先于 xterm 记数。
 *
 * 连按同一个标点键时 iOS 在「，。？！」间循环：不发按键事件，直接把刚上屏的符号删掉或替换
 * 成下一个。xterm 只认插入、不认删除，终端里就剩下「，。」。所以补发前先比对 textarea 改动
 * 前后的内容，删了几个字就先发几个退格。 */
let dataSeq = 0;
const MAX_BRIDGE_DELETE = 8; // 超出就不是循环标点这类小改动，宁可少删也不误删用户的输入

/** 两段文本的差异：从末尾起删掉几个字符（按码点）、插入了什么 */
function textEdit(before: string, after: string) {
  const a = [...before], b = [...after];
  let p = 0;
  while (p < a.length && p < b.length && a[p] === b[p]) p++;
  let s = 0;
  while (s < a.length - p && s < b.length - p && a[a.length - 1 - s] === b[b.length - 1 - s]) s++;
  return { removed: a.length - p - s, inserted: b.slice(p, b.length - s).join("") };
}

function bridgeDroppedInput(host: HTMLElement, ta: HTMLTextAreaElement) {
  let mark = -1; // 本次按键开始时的 dataSeq，-1 表示没有待确认的按键
  let keyCode = 0;
  let composing = false;
  let before: string | null = null; // beforeinput 时的 textarea 内容
  let queued = 0;
  const send = (out: string) => { traceTermInput("bridge", out); sendTermInput(modifiedTermInput(out)); };
  // 晚于 xterm 在 keydown/compositionend 时排下的 setTimeout(0)，同延迟的定时器按登记顺序触发。
  const later = (from: number, out: string) => {
    queued++;
    setTimeout(() => {
      queued--;
      if (dataSeq === from && !composing) send(out);
    }, 0);
  };
  const on = <K extends keyof HTMLElementEventMap>(type: K, fn: (e: HTMLElementEventMap[K]) => void) =>
    host.addEventListener(type, (e) => { if (e.target === ta) fn(e); }, true);
  on("keydown", (e) => { mark = dataSeq; keyCode = e.keyCode; });
  // xterm 已为这次按键发过数据就结束；还没发（iOS 晚到的输入）则留着等输入事件。
  on("keyup", () => { if (mark !== dataSeq) mark = -1; });
  on("compositionstart", () => { composing = true; });
  on("compositionend", () => { composing = false; });
  on("beforeinput", () => { before = ta.value; });
  on("input", (e) => {
    const keyed = mark >= 0;
    const from = keyed ? mark : dataSeq;
    const prev = before;
    mark = -1;
    before = null;
    if (e.isComposing || composing) return;
    const edit = prev === null ? null : textEdit(prev, ta.value);
    let removed = edit && edit.removed <= MAX_BRIDGE_DELETE ? edit.removed : 0;
    let data = "";
    if (e.inputType === "insertText" && e.data) {
      data = e.data;
      if (edit?.inserted !== data) removed = 0;
    } else if (e.inputType !== "deleteContentBackward" || edit?.inserted !== "") return;
    const del = "\x7f".repeat(removed);
    if (!del && !data) return;
    // 键码 229 时 xterm 的差分定时器可能也为这次改动发退格或文字，整体等它跑完再决定。
    if (keyed && keyCode === 229) { later(from, del + data); return; }
    // xterm 从不发删除：立即发出，赶在它可能同步发出的这次插入之前（循环标点的删除与插入
    // 常是相邻两个输入事件）。插入照旧等 xterm 的时机都过去再看要不要补。
    if (del && dataSeq === from) {
      if (queued) later(from, del);
      else send(del);
    }
    if (data) later(from, data);
  });
}

// 创建 xterm 实例（若尚不存在）：渲染器、图片路径链接、输入/尺寸回调都在这里挂一次，
// 之后跨重连复用同一实例，回调动态读取 S.termWS，避免重复叠加监听。
function ensureTerm() {
  if (S.term) return;
  if (!TerminalClass) {
    $("term-loading").classList.add("hidden");
    setConnStatus("closed");
    $("term-state").textContent = "xterm.js 加载失败";
    return;
  }
  const term = new TerminalClass({
    fontFamily: "JetBrains Mono, Menlo, Consolas, monospace",
    fontSize: isMobile() ? 12 : 13, // 窄屏降一号，约 46 列
    cursorBlink: true,
    // 配色取 css/base.css 的 --term-* 令牌（深浅主题下都是深色，见那里的说明）
    theme: {
      background: token("--term-bg"),
      foreground: token("--term-fg"),
      cursor: token("--term-cursor"),
      selectionBackground: token("--term-sel"),
    },
  });
  const fit = new FitAddonClass!();
  term.loadAddon(fit);
  $("term-mount").replaceChildren();
  term.open($("term-mount"));
  // GPU 渲染器：方块/半格字形按整格实心填充（customGlyphs 默认开），
  // 像素图（如 Claude 小人）与制表线严丝合缝，接近原生终端的细腻度。
  // 必须在 term.open 之后加载；WebGL 不可用或上下文丢失时自动回退默认 DOM 渲染。
  if (WebglAddonClass) {
    try {
      const webgl = new WebglAddonClass();
      webgl.onContextLoss(() => webgl.dispose());
      term.loadAddon(webgl);
    } catch (_) { /* 保持默认 DOM 渲染 */ }
  }
  fit.fit();
  S.term = term;
  S.fit = fit;
  fitTermViewport();

  if (term.element && term.textarea) {
    bridgeDroppedInput(term.element, term.textarea);
    attachTermInputDebug(term.element, term.textarea);
  }
  attachTouchScroll(term, {
    longPress: (x, y) => {
      if (termKeys.disabled) return;
      openMenuAt(x, y, [{ label: "粘贴", icon: "paste", run: () => void pasteClipboard() }]);
    },
    swallowTap: menuJustDismissed,
  });
  term.onData((d) => {
    dataSeq++;
    traceTermInput("xterm", d);
    // 焦点上报不是用户按键，不能消耗一次性的 Ctrl / Alt。
    if (d === "\x1b[I" || d === "\x1b[O") {
      sendTermInput(d);
    } else {
      sendTermInput(modifiedTermInput(d));
    }
  });
  term.onResize(({ cols, rows }) => {
    if (S.termWS && S.termWS.readyState === WebSocket.OPEN) {
      S.termWS.send(JSON.stringify({ type: "resize", cols, rows }));
    }
  });

  // 终端里出现的 /shared/.images/ 路径可点击弹出图片预览
  if (term.registerLinkProvider) {
    term.registerLinkProvider({
      provideLinks(y, cb) {
        const line = term.buffer.active.getLine(y - 1);
        if (!line) return cb(undefined);
        const text = line.translateToString(true);
        const links: XtermLink[] = [];
        for (const m of text.matchAll(IMG_PATH_RE)) {
          links.push({
            range: { start: { x: m.index + 1, y }, end: { x: m.index + m[0].length, y } },
            text: m[0],
            activate: (_e: MouseEvent, p: string) => openLightbox(imgURLFromPath(p), p),
          });
        }
        cb(links.length ? links : undefined);
      },
    });
  }
}

// 拉起 PTY WebSocket。isReconnect 为 true 时不弹大遮罩（终端画面还在），只在顶栏提示重连中。
function connectTermWS(isReconnect: boolean) {
  const sess = S.current;
  if (!sess || !S.term) return;
  clearReconnect();
  const gen = ++S.termWSGen; // 本次连接的代际；旧连接的 onopen/onclose 据此作废

  if (!isReconnect) {
    $("term-loading").classList.remove("hidden"); // 大遮罩仅首次/手动连接时出现
  }
  setConnStatus(isReconnect ? "reconnecting" : "connecting");

  const ws = new WebSocket(wsURL(`/sessions/${sess.id}/term`));
  ws.binaryType = "arraybuffer";
  S.termWS = ws;

  ws.onopen = () => {
    if (gen !== S.termWSGen) { ws.close(); return; } // 已被更新的连接取代
    reconnectAttempt = 0;
    S.term!.write(RESET_INPUT_MODES); // 复位上一个程序遗留的鼠标上报/备用屏等模式，再接新 bash
    $("term-loading").classList.add("hidden");
    setConnStatus("connected");
    ws.send(JSON.stringify({ type: "resize", cols: S.term!.cols, rows: S.term!.rows }));
    if (S.fit) S.fit.fit();
    refocusTerm(); // 重连场景顺带刷新 IME 上下文
    refreshAll(); // 终端会自动拉起容器，刷新状态灯
  };
  ws.onmessage = (e) => {
    if (!S.term) return;
    S.term.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data));
  };
  ws.onclose = (e) => {
    if (gen !== S.termWSGen) return; // 被 teardown/disconnect/手动重连接管，忽略
    S.termWS = null;
    $("term-loading").classList.add("hidden"); // 连接失败时不能留着转圈
    // 4000–4999 是服务端的应用私有关闭码（目前只有额度用尽一种）：reason 是写给
    // 人看的一句话，写进终端画面并顶到状态栏。绝不自动重连——理由不会因为重试
    // 而改变，只会被同一句话再挡一次。
    if (e && e.code >= 4000 && e.code < 5000) {
      const why = e.reason || "连接已被服务端拒绝";
      if (S.term) S.term.write("\r\n\x1b[31m" + why + "\x1b[0m\r\n");
      setConnStatus("closed");
      $("term-state").textContent = why;
      return;
    }
    // 干净关闭（1000，服务端在 shell 进程退出时发）意味着：用户主动 exit，
    // 或本终端被另一处打开的终端顶掉（tmux attach -D）。这两种都不该自动重连
    // ——尤其后者，自动重连会把对方再顶掉，两个页面无限拉锯。停在「已断开」，
    // 想连点右侧按钮。网络类异常断开（1006 等）才走自动重连。
    if (e && e.code === 1000) { setConnStatus("closed"); return; }
    scheduleReconnect();
  };
}

// 进入终端页时调用：已有活动连接则只重排字形，否则（新开 / 断开后重进）拉起 shell。
export function openTerm() {
  const sess = S.current; if (!sess) return;
  // 连接中或已连上：不重建，避免切标签页把正在跑的会话打断，只需重新贴合尺寸
  if (S.termWS && S.termWS.readyState <= WebSocket.OPEN) {
    if (S.fit) requestAnimationFrame(() => S.fit!.fit());
    return;
  }
  clearReconnect();
  reconnectAttempt = 0;
  ensureTerm();
  if (!S.term) return; // xterm 加载失败，ensureTerm 已提示
  connectTermWS(false);
}

$("term-reconnect").addEventListener("click", termReconnect);

// 侧栏收起也会改变终端宽度，但不会触发 window.resize。
new ResizeObserver(([entry]) => {
  if (S.fit && S.tab === "term" && entry.contentRect.width && entry.contentRect.height) S.fit.fit();
}).observe($("term-mount"));

/* ---------------- 手机辅助键 ---------------- */
const termKeys = $("term-keys") as HTMLFieldSetElement;
const termModifiers = { ctrl: false, alt: false };
let keysConnected = false;

function resetTermModifiers() {
  termModifiers.ctrl = termModifiers.alt = false;
  renderTermModifiers();
}

function renderTermModifiers() {
  for (const button of termKeys.querySelectorAll<HTMLButtonElement>("[data-term-mod]")) {
    const mod = button.dataset.termMod as keyof typeof termModifiers;
    button.setAttribute("aria-pressed", String(termModifiers[mod]));
  }
}

function syncTermKeys(connected = keysConnected) {
  keysConnected = connected;
  termKeys.disabled = !connected || !!S.term?.options.disableStdin;
  if (termKeys.disabled) resetTermModifiers();
}

function sendTermInput(data: string) {
  if (!S.term || S.term.options.disableStdin || S.termWS?.readyState !== WebSocket.OPEN) return;
  S.termWS.send(ENC.encode(data));
}

function modifiedTermInput(data: string): string {
  const { ctrl, alt } = termModifiers;
  if (!ctrl && !alt) return data;
  resetTermModifiers();
  // 只组合单个 ASCII 按键，中文输入和整段粘贴原样通过。
  if (data.length !== 1 || data.charCodeAt(0) > 127) return data;
  if (ctrl) {
    const code = data.toUpperCase().charCodeAt(0);
    if (code >= 64 && code <= 95) data = String.fromCharCode(code - 64);
    else if (data === " ") data = "\x00";
    else if (data === "?") data = "\x7f";
  }
  return (alt ? "\x1b" : "") + data;
}

// 阻止按钮抢走 textarea 焦点；触摸仍允许横向滚动，点击后同步 focus 可唤起 iOS 键盘。
termKeys.addEventListener("mousedown", (event) => event.preventDefault());
termKeys.addEventListener("click", (event) => {
  const button = (event.target as Element).closest<HTMLButtonElement>("button");
  if (!button || termKeys.disabled || !S.term) return;
  if (button.id === "term-keyboard") {
    resetTermModifiers();
    if (document.activeElement === S.term.textarea) S.term.textarea?.blur();
    else S.term.focus();
    return;
  }
  const mod = button.dataset.termMod;
  if (mod === "ctrl" || mod === "alt") {
    termModifiers[mod] = !termModifiers[mod];
    renderTermModifiers();
  } else {
    const key = button.dataset.termKey!;
    const arrows: Record<string, string> = { up: "A", down: "B", right: "C", left: "D" };
    const fixed: Record<string, string> = { escape: "\x1b", tab: "\t", "shift-tab": "\x1b[Z", interrupt: "\x03" };
    let data: string;
    if (arrows[key]) {
      const modifier = 1 + (termModifiers.alt ? 2 : 0) + (termModifiers.ctrl ? 4 : 0);
      data = modifier > 1 ? `\x1b[1;${modifier}${arrows[key]}`
        : `\x1b${S.term.modes.applicationCursorKeysMode ? "O" : "["}${arrows[key]}`;
      resetTermModifiers();
    } else if (key === "interrupt" || key === "shift-tab") {
      data = fixed[key]!;
      resetTermModifiers();
    } else {
      data = modifiedTermInput(fixed[key] ?? key);
    }
    sendTermInput(data);
  }
  S.term.focus();
});

const touchTerminal = matchMedia("(max-width: 760px), (pointer: coarse)");
function fitTermViewport() {
  const viewport = window.visualViewport;
  const active = !!S.term && S.view === "work" && S.tab === "term" && touchTerminal.matches;
  const app = $("app");
  // 放大页面时保留浏览器的平移与缩放行为。
  app.classList.toggle("term-viewport", active && !!viewport && viewport.scale === 1);
  if (active && viewport) {
    app.style.setProperty("--term-viewport-height", `${viewport.height}px`);
    app.style.setProperty("--term-viewport-top", `${viewport.offsetTop}px`);
  }
  if (!active) resetTermModifiers();
}
window.visualViewport?.addEventListener("resize", fitTermViewport);
window.visualViewport?.addEventListener("scroll", fitTermViewport);
touchTerminal.addEventListener("change", fitTermViewport);
const termVisibility = new MutationObserver(() => {
  resetTermModifiers();
  fitTermViewport();
});
for (const id of ["tab-term", "view-work"]) {
  termVisibility.observe($(id), { attributes: true, attributeFilter: ["class"] });
}

/* ---------------- 顶栏提示轮播 ----------------
 * 提示语与频率/动画在系统设置「界面与提示」里配置，经 /me 下发给所有用户（S.termTips）。
 * 单行视窗 + 纵向轨道：每隔 interval 逐条上移一行，末尾追加首条克隆做无缝回卷。
 * animation 目前只有 "scroll"（滚动），作为字段保留以便日后扩展滑动/翻转等。 */
let tipsTimer: ReturnType<typeof setInterval> | null = null;

function initTermTips() {
  const host = $("term-tips");
  const wrap = $("term-tip-wrap");
  if (!host || !wrap) return;
  if (tipsTimer) { clearInterval(tipsTimer); tipsTimer = null; }
  host.replaceChildren();

  const cfg: Partial<TerminalTips> = S.termTips || {};
  const tips = Array.isArray(cfg.tips) ? cfg.tips.filter((t) => t && t.trim()) : [];
  wrap.classList.toggle("hidden", tips.length === 0);
  if (!tips.length) return;

  const track = document.createElement("div");
  track.className = "term-tips-track anim-" + (cfg.animation || "scroll");
  for (const t of tips) {
    track.appendChild(Object.assign(document.createElement("div"), { className: "term-tip", textContent: t }));
  }
  host.appendChild(track);

  const interval = Math.floor(cfg.interval_sec || 0) * 1000;
  if (tips.length <= 1 || interval <= 0) return; // 单条或频率 0：静止显示第一条

  // 末尾克隆首条，滚到它时无缝跳回顶部
  track.appendChild(track.firstElementChild!.cloneNode(true));
  let idx = 0;
  tipsTimer = setInterval(() => {
    const h = host.clientHeight;
    if (!h) return; // 窄屏隐藏（display:none）时不动
    idx++;
    track.style.transition = "transform .5s ease";
    track.style.transform = `translateY(${-idx * h}px)`;
    if (idx >= tips.length) {
      setTimeout(() => {
        track.style.transition = "none";
        track.style.transform = "translateY(0)";
        idx = 0;
      }, 520);
    }
  }, interval);
}

bus.addEventListener("tips-updated", initTermTips);

/* ---------------- 顶栏「本会话已花」 ----------------
 * 会话内的全部消耗：网页对话 + 终端手敲 + 起标题，也就是使用记录里按会话筛出来的
 * 那份合计。数据走 /usage/events 的 total（服务端按筛选条件算，与翻页无关），
 * 顺带触发一次终端消耗补记，所以在终端里刚花完的量这里也是最新的。
 *
 * 订阅账号下这个金额是「按 API 价折算的等价金额」而非真实支出；终端行还可能因为
 * 价目表里没配 claude 而算不出钱——所以 token 也一并显示，金额为 0 时它才是唯一
 * 有信息量的那个数。 */
const SPEND_POLL_MS = 15000;
let spendTimer: ReturnType<typeof setInterval> | null = null;
let spendFor = ""; // 当前显示的是哪个工作空间的数，切工作空间时先清空免得串台

function fmtTok(n: number) {
  if (n >= 1e6) return (n / 1e6).toFixed(n >= 1e7 ? 0 : 1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + "K";
  return String(n);
}

async function loadSpend() {
  const sess = S.current;
  const box = $("term-spend");
  if (!sess) { box.classList.add("hidden"); return; }
  if (spendFor !== sess.id) { box.replaceChildren(); spendFor = sess.id; }
  let total: UsageTotals;
  try {
    const data = await api<UsageEvents>(
      "/usage/events?limit=1&session=" + encodeURIComponent(sess.id));
    total = data.total;
  } catch (_) {
    return; // 拉不到就保持上一次的数，顶栏不该因为这个闪
  }
  if (S.current?.id !== sess.id) return; // 请求在途时用户切走了
  const tokens = total.input_tokens + total.output_tokens
    + total.cache_read_tokens + total.cache_write_tokens;
  if (!total.rows) { box.classList.add("hidden"); return; }

  const label = Object.assign(document.createElement("span"), {
    className: "ts-label", textContent: "本空间已花",
  });
  const cost = Object.assign(document.createElement("span"), {
    className: "ts-cost", textContent: fmtUSD(total.cost_micro_usd, 2),
  });
  const tok = Object.assign(document.createElement("span"), {
    className: "ts-tok", textContent: fmtTok(tokens) + " tok",
  });
  box.replaceChildren(label, cost, tok);
  setTip(box, `${total.turns} 个回合 / ${total.rows} 条记录，含网页对话、终端和起标题。`
    + "金额按价目表与 provider 报价折算，订阅账号下只是等价估算；终端消耗计入这里但不扣额度。");
  box.classList.remove("hidden");
}

/** 进出终端页时开关轮询：不在终端页就没必要一直问。 */
export function termSpendPolling(on: boolean) {
  if (spendTimer) { clearInterval(spendTimer); spendTimer = null; }
  if (!on) return;
  void loadSpend();
  spendTimer = setInterval(() => void loadSpend(), SPEND_POLL_MS);
}

/* 终端粘贴图片：上传后把容器内路径写入 PTY（capture 阶段拦截，避免 xterm 处理）。
 * 上传期间整个终端页盖遮罩转圈，并通过 disableStdin 禁止键入，防止用户不知道发生了什么。 */
const termUpload = { total: 0, done: 0 };

function termUploadUI() {
  const on = termUpload.total > 0;
  $("term-overlay").classList.toggle("hidden", !on);
  if (on) {
    $("term-overlay-text").textContent = termUpload.total > 1
      ? `图片上传中… (${termUpload.done + 1}/${termUpload.total})`
      : "图片上传中…";
  }
  if (S.term) {
    S.term.options.disableStdin = on;
    if (!on) refocusTerm(); // disableStdin 切过 readOnly，须刷新 IME 上下文
  }
  syncTermKeys();
}

async function uploadTermImages(files: Blob[]) {
  termUpload.total += files.length;
  termUploadUI();
  for (const f of files) {
    try {
      const res = await uploadAttachment(f);
      if (S.termWS && S.termWS.readyState === WebSocket.OPEN) {
        S.termWS.send(ENC.encode(res.path + " "));
      }
    } catch (err) {
      if (S.term) S.term.write(`\r\n\x1b[31m图片上传失败: ${(err as Error).message}\x1b[0m\r\n`);
    }
    termUpload.done++;
    if (termUpload.done === termUpload.total) { termUpload.total = 0; termUpload.done = 0; }
    termUploadUI();
  }
}

$("term-mount").addEventListener("paste", (e) => {
  const files = pastedImages(e);
  if (!files.length || !S.current) return;
  e.preventDefault();
  e.stopPropagation();
  void uploadTermImages(files);
}, true);

/* 手机上长按终端的「粘贴」。触屏上 xterm 的输入框只是光标处看不见的一个点，长按别处出不来
 * 系统的粘贴菜单，只能自己读剪贴板（要 HTTPS）：iOS 会再弹一个系统「粘贴」气泡确认，Android
 * 第一次会问剪贴板权限。图片与桌面粘贴一样上传后写入路径；文字走 xterm 的 paste，程序开着
 * 括号粘贴（Claude 会开）时多行文字整段送达，不会被当成一行行回车。 */
async function pasteClipboard() {
  const clip = navigator.clipboard;
  if (!clip?.read && !clip?.readText) { toast("浏览器不允许网页读取剪贴板（需要 HTTPS 访问）", true); return; }
  let text = "";
  const images: Blob[] = [];
  try {
    if (clip.read) {
      for (const item of await clip.read()) {
        const image = item.types.find((t) => t.startsWith("image/"));
        if (image) images.push(await item.getType(image));
        else if (item.types.includes("text/plain")) text += await (await item.getType("text/plain")).text();
      }
    } else {
      text = await clip.readText();
    }
  } catch {
    toast("没能读取剪贴板：请允许浏览器访问剪贴板后再试", true);
    return;
  }
  if (!S.term || !S.current || termKeys.disabled) return;
  if (images.length) void uploadTermImages(images);
  else if (text) S.term.paste(text);
  else toast("剪贴板里没有文字或图片");
}
