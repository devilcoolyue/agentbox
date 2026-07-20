/* term：终端页 —— xterm 实例、PTY WebSocket、终端内粘贴图片上传。 */
"use strict";

import { S } from "./state.js";
import { $, isMobile, openLightbox } from "./util.js";
import { wsURL, imgURLFromPath, uploadAttachment } from "./api.js";
import { refreshAll } from "./data.js";
import { pastedImages } from "./chat.js";

const TerminalClass = window.Terminal && (window.Terminal.Terminal || window.Terminal);
const FitAddonClass = window.FitAddon && (window.FitAddon.FitAddon || window.FitAddon);

const ENC = new TextEncoder();
const IMG_PATH_RE = /\/shared\/\.images\/[A-Za-z0-9._-]+/g;

const token = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

$("btn-term-shell").addEventListener("click", () => openTerm("shell"));
$("btn-term-agent").addEventListener("click", () => openTerm("agent"));

/* 连接建立前锁住两个入口按钮，避免连点反复拉起 */
function setTermBtns(disabled) {
  $("btn-term-shell").disabled = disabled;
  $("btn-term-agent").disabled = disabled;
}

/* 仅断开连接（停止容器时）：保留终端画面与回滚缓冲 */
export function termDisconnect() {
  if (S.termWS) { S.termWS.close(); S.termWS = null; }
  setTermBtns(false);
}

/* 完全收尾（切换/删除会话时）：连实例一起销毁 */
export function termTeardown() {
  termDisconnect();
  if (S.term) { S.term.dispose(); S.term = null; S.fit = null; }
}

function openTerm(mode) {
  const sess = S.current; if (!sess) return;
  if (S.termWS) { S.termWS.close(); S.termWS = null; }
  if (S.term) { S.term.dispose(); S.term = null; S.fit = null; }
  if (!TerminalClass) { $("term-state").textContent = "xterm.js 加载失败"; return; }

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
  const fit = new FitAddonClass();
  term.loadAddon(fit);
  $("term-mount").replaceChildren();
  term.open($("term-mount"));
  fit.fit();
  S.term = term;
  S.fit = fit;

  // 加载态显示在下方终端显示区（盖在挂载点上），按钮条只锁按钮不放转圈
  $("term-state").textContent = "";
  $("term-loading").classList.remove("hidden");
  setTermBtns(true);
  const ws = new WebSocket(wsURL(`/sessions/${sess.id}/term`) + `&mode=${mode}`);
  ws.binaryType = "arraybuffer";
  S.termWS = ws;

  ws.onopen = () => {
    $("term-loading").classList.add("hidden");
    $("term-state").textContent = mode === "agent" ? `${sess.agent} · 交互终端` : "shell";
    setTermBtns(false);
    ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    term.focus();
    refreshAll(); // 终端会自动拉起容器，刷新状态灯
  };
  ws.onmessage = (e) => {
    term.write(typeof e.data === "string" ? e.data : new Uint8Array(e.data));
  };
  ws.onclose = () => {
    $("term-loading").classList.add("hidden"); // 连接失败时不能留着转圈
    setTermBtns(false);
    if (S.termWS === ws) {
      $("term-state").textContent = "已断开（进程退出或连接中断）";
      S.termWS = null;
    }
  };
  term.onData((d) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(ENC.encode(d));
  });
  term.onResize(({ cols, rows }) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols, rows }));
  });

  // 终端里出现的 /shared/.images/ 路径可点击弹出图片预览
  if (term.registerLinkProvider) {
    term.registerLinkProvider({
      provideLinks(y, cb) {
        const line = term.buffer.active.getLine(y - 1);
        if (!line) return cb(undefined);
        const text = line.translateToString(true);
        const links = [];
        for (const m of text.matchAll(IMG_PATH_RE)) {
          links.push({
            range: { start: { x: m.index + 1, y }, end: { x: m.index + m[0].length, y } },
            text: m[0],
            activate: (_e, p) => openLightbox(imgURLFromPath(p), p),
          });
        }
        cb(links.length ? links : undefined);
      },
    });
  }
}

window.addEventListener("resize", () => {
  if (S.fit && S.tab === "term") S.fit.fit();
});

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
    if (!on) S.term.focus();
  }
}

$("term-mount").addEventListener("paste", (e) => {
  const files = pastedImages(e);
  if (!files.length || !S.current) return;
  e.preventDefault();
  e.stopPropagation();
  (async () => {
    termUpload.total += files.length;
    termUploadUI();
    for (const f of files) {
      try {
        const res = await uploadAttachment(f);
        if (S.termWS && S.termWS.readyState === WebSocket.OPEN) {
          S.termWS.send(ENC.encode(res.path + " "));
        }
      } catch (err) {
        if (S.term) S.term.write(`\r\n\x1b[31m图片上传失败: ${err.message}\x1b[0m\r\n`);
      }
      termUpload.done++;
      if (termUpload.done === termUpload.total) { termUpload.total = 0; termUpload.done = 0; }
      termUploadUI();
    }
  })();
}, true);
