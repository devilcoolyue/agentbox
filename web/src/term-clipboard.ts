/* term-clipboard：终端里的程序复制文字 → 本机剪贴板（OSC 52）。
 *
 * Claude Code 在 tmux 里选中文字后执行 `tmux load-buffer -w`，tmux 再用 OSC 52（ESC ] 52 ; 选区 ;
 * base64 BEL）把内容交给外层终端，也就是这个 xterm。xterm.js 自己不认 OSC 52，内容以前就停在
 * tmux 缓冲区里——CLI 还提示「prefix + ]」粘贴，而我们的 tmux 连前缀键都没有。这里接住它写进
 * 系统剪贴板，CLI 那句提示改不了，所以另弹一条「已复制」。
 *
 * 只写不读：查询剪贴板（Pd 为「?」）一律不回应，否则容器里的程序能读走本机剪贴板。程序直接
 * 打印的 OSC 52 被 tmux（set-clipboard off）挡在外面，cat 一个文件改不了剪贴板；而
 * `tmux load-buffer -w` 谁都能跑，所以还要求用户刚在终端里点过、拖过或按过键（3 秒内），
 * 免得 Agent 在用户不注意时改写本机剪贴板、等用户粘贴进自己电脑的终端。
 *
 * Chrome / Firefox 允许页面在获焦时直接写剪贴板；Safari 只认点击当时写，请求从 WebSocket
 * 过来已经不在手势里，于是终端底部浮出一条「复制到剪贴板」，点一下在手势里写。 */
"use strict";

import { setText, t as i18nText } from "./i18n.js";
import { enter, leave } from "./motion.js";
import { $, toast } from "./util.js";

const GRACE_MS = 3000;            // 交互后多久内的复制请求算用户发起的
const MAX_BASE64 = 4 * 1024 * 1024; // 约 3 MB 文字；再大多半不是用户选的
const BASE64_RE = /^[A-Za-z0-9+/]*={0,2}$/;

let lastInteraction = -Infinity;
let pressedInTerm = false; // 拖选可能在终端外松手，松手那一下也算
let pending = "";          // 等用户点按钮写入的内容
let leaving: Animation | null = null;

document.addEventListener("pointerup", () => {
  if (!pressedInTerm) return;
  pressedInTerm = false;
  lastInteraction = performance.now();
}, true);

/** 解出 OSC 52 要写的文字；查询、清空、格式不对都返回 null */
export function parseOsc52(data: string): string | null {
  const sep = data.indexOf(";");
  if (sep < 0) return null;
  const payload = data.slice(sep + 1).replace(/\s+/g, "");
  if (!payload || payload === "?" || payload.length > MAX_BASE64 || !BASE64_RE.test(payload)) return null;
  try {
    const bytes = Uint8Array.from(atob(payload), (c) => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return null;
  }
}

const chars = (text: string) => [...text].length;

function copied(text: string) {
  toast(i18nText("已复制到剪贴板（{p0} 个字符）", { p0: String(chars(text)) }));
}

/** 不在安全上下文（HTTP）时没有异步剪贴板，退回 execCommand；两者都只在点击当时可靠 */
function legacyCopy(text: string) {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.cssText = "position:fixed;top:0;left:0;opacity:0;pointer-events:none";
  const focused = document.activeElement as HTMLElement | null;
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { ok = false; }
  ta.remove();
  focused?.focus?.();
  return ok;
}

function hideOffer() {
  pending = "";
  const bar = $("term-copy");
  if (bar.classList.contains("hidden") || leaving) return;
  leaving = leave(bar, () => {
    leaving?.cancel(); // 撤掉保持终态的淡出，下次显示才看得见
    leaving = null;
    bar.classList.add("hidden");
    bar.inert = false;
  }, 4);
}

function offer(text: string) {
  pending = text;
  const bar = $("term-copy");
  setText($("term-copy-label"), "终端复制了 {p0} 个字符", { p0: String(chars(text)) });
  $("term-copy-preview").textContent = text.slice(0, 200).replace(/\s+/g, " ").trim();
  leaving?.cancel();
  leaving = null;
  bar.inert = false;
  if (bar.classList.contains("hidden")) {
    bar.classList.remove("hidden");
    enter(bar, 6);
  }
}

async function write(text: string) {
  try {
    if (!navigator.clipboard?.writeText) throw new Error("no async clipboard");
    await navigator.clipboard.writeText(text);
    hideOffer();
    copied(text);
  } catch {
    offer(text);
  }
}

let wired = false;
function wireOffer(refocusTerm: () => void) {
  if (wired) return;
  wired = true;
  // 桌面上点完按钮接着打字；触屏上把焦点还给终端会弹出软键盘，不还
  const refocus = () => { if (!matchMedia("(pointer: coarse)").matches) refocusTerm(); };
  $("term-copy-btn").addEventListener("click", () => {
    const text = pending;
    if (!text) return;
    const done = () => { hideOffer(); copied(text); refocus(); };
    const fail = () => toast(i18nText("浏览器拒绝写入剪贴板"), true);
    if (navigator.clipboard?.writeText) navigator.clipboard.writeText(text).then(done, () => (legacyCopy(text) ? done() : fail()));
    else if (legacyCopy(text)) done();
    else fail();
  });
  $("term-copy-close").addEventListener("click", () => { hideOffer(); refocus(); });
}

/** 挂到一个 xterm 实例上；监听随 term.element 一起销毁 */
export function attachTermClipboard(term: XtermTerminal, refocus: () => void) {
  wireOffer(refocus);
  const el = term.element;
  if (el) {
    const mark = () => { lastInteraction = performance.now(); };
    el.addEventListener("pointerdown", () => { pressedInTerm = true; mark(); }, true);
    el.addEventListener("keydown", mark, true);
  }
  term.parser?.registerOscHandler(52, (data) => {
    if (performance.now() - lastInteraction > GRACE_MS) return true;
    const text = parseOsc52(data);
    if (text === null && data.length > MAX_BASE64) toast(i18nText("终端复制的内容太大，没有写入剪贴板"), true);
    if (!text) return true;
    void write(text);
    return true;
  });
}

/** 换空间、销毁终端时收起，别把上一个空间复制的内容留着等人点 */
export function resetTermClipboard() {
  hideOffer();
  lastInteraction = -Infinity;
  pressedInTerm = false;
}
