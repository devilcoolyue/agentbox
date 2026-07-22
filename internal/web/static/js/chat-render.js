/* chat-render：对话流的纯渲染管线 —— 用户消息、agent 事件（Claude stream-json /
 * Codex --json 新旧两种结构）、轻量 Markdown、附件缩略图。无状态副作用。
 * 全部通过 DOM 构建输出，模型文本永远走 textContent，不存在注入面。 */
"use strict";

import { openLightbox, fmtTime } from "./util.js";
import { imgURLFromPath } from "./api.js";

export const USER_ATTACH_RE = /\[(图片|附件)#(\d+) (\/shared\/\.(?:images|file)\/[A-Za-z0-9._-]+)\]/g;

/* ---- 行内小图标：stroke 线稿，颜色随 currentColor ---- */
const ICONS = {
  tool: "M3.75 4.5 8.25 9l-4.5 4.5M9.75 13.5h4.5",
  copy: "M6.75 6V4.13c0-.62.5-1.13 1.13-1.13h6c.62 0 1.12.5 1.12 1.13v6c0 .62-.5 1.12-1.12 1.12H12M3 7.88c0-.63.5-1.13 1.13-1.13h6c.62 0 1.12.5 1.12 1.13v6c0 .62-.5 1.12-1.13 1.12h-6C3.5 15 3 14.5 3 13.88Z",
  check: "M3.75 9.75 7.5 13.5l6.75-8.25",
  caret: "M6.75 3.75 12 9l-5.25 5.25",
  chevron: "M3.75 6.75 9 12l5.25-5.25",
  /* 会话动作三件套：与侧栏（内网隧道/系统设置/退出登录）同为 24 视框、1.8 线宽，
   * 同尺寸渲染时观感才一致——18 视框的字形留白更多，会显得小一号。 */
  play: { d: "M6.5 3.5 20 12 6.5 20.5Z", box: 24, width: 1.8 },
  stop: { d: "M5.5 5.5h13v13h-13Z", box: 24, width: 1.8 },
  trash: { d: "M3 6h18M8 6V4.5A1.5 1.5 0 0 1 9.5 3h5A1.5 1.5 0 0 1 16 4.5V6M5.5 6l1 14.5h11L18.5 6", box: 24, width: 1.8 },
  /* 历史对话入口：表盘 + 指针 */
  clock: { d: "M12 21a9 9 0 1 1 0-18 9 9 0 0 1 0 18ZM12 7.2v5l3.4 2", box: 24, width: 1.8 },
};

export function svgIcon(name, size = 13) {
  const ico = ICONS[name];
  const { d, box = 18, width = 1.5 } = typeof ico === "string" ? { d: ico } : ico;
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", `0 0 ${box} ${box}`);
  svg.setAttribute("width", size);
  svg.setAttribute("height", size);
  svg.setAttribute("fill", "none");
  svg.setAttribute("aria-hidden", "true");
  const p = document.createElementNS(ns, "path");
  p.setAttribute("d", d);
  p.setAttribute("stroke", "currentColor");
  p.setAttribute("stroke-width", width);
  p.setAttribute("stroke-linecap", "round");
  p.setAttribute("stroke-linejoin", "round");
  svg.appendChild(p);
  return svg;
}

/* 复制到剪贴板：clipboard API 优先，非安全上下文回退 execCommand */
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (_) {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.cssText = "position:fixed;opacity:0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (_) {}
    ta.remove();
  }
}

export function chip(text, extra) {
  const c = document.createElement("div");
  c.className = "chip" + (extra ? " " + extra : "");
  c.textContent = text;
  return c;
}

export function toolChip(name, summary) {
  const c = document.createElement("div");
  c.className = "chip tool";
  const n = document.createElement("span");
  n.className = "name";
  n.append(svgIcon("tool", 12), document.createTextNode(name));
  c.appendChild(n);
  if (summary) {
    const s = document.createElement("span");
    s.className = "sum";
    s.textContent = summary;
    c.appendChild(s);
  }
  c.title = summary || name;
  return c;
}

/* 上下文分割线：divider 之前的消息不再带入后续对话 */
export function divider(label) {
  const d = document.createElement("div");
  d.className = "chat-divider mono";
  const s = document.createElement("span");
  s.textContent = label || "新对话";
  d.appendChild(s);
  return d;
}

export function renderUserMsg(text) {
  const d = document.createElement("div");
  d.className = "msg user";
  const str = String(text);
  let last = 0;
  for (const m of str.matchAll(USER_ATTACH_RE)) {
    if (m.index > last) d.appendChild(document.createTextNode(str.slice(last, m.index)));
    if (m[1] === "图片") d.appendChild(chatThumb(m[3], "图片 #" + m[2]));
    else d.appendChild(fileLink(m[3], "附件 #" + m[2]));
    last = m.index + m[0].length;
  }
  if (last < str.length) d.appendChild(document.createTextNode(str.slice(last)));
  return d;
}

/* 消息气泡里的图片缩略图 */
export function chatThumb(containerPath, title) {
  const img = document.createElement("img");
  img.className = "chat-thumb";
  img.src = imgURLFromPath(containerPath);
  img.alt = title;
  img.title = title + "（点击查看大图）";
  img.addEventListener("click", () => openLightbox(imgURLFromPath(containerPath), title + " · " + containerPath));
  img.addEventListener("error", () => {
    const gone = document.createElement("span");
    gone.className = "img-gone";
    gone.textContent = `[${title} 已过期]`;
    img.replaceWith(gone);
  });
  return img;
}

/* 消息里的附件：可点击下载的文件条 */
function fileLink(containerPath, title) {
  const a = document.createElement("a");
  a.className = "file-link mono";
  a.textContent = "📄 " + containerPath.split("/").pop();
  a.title = title + " · " + containerPath;
  a.href = imgURLFromPath(containerPath);
  a.target = "_blank";
  return a;
}

function agentText(text) {
  const d = document.createElement("div");
  d.className = "msg agent";
  d.appendChild(formatText(text));
  return d;
}

/* ================= 轻量 Markdown =================
 * 代码围栏 → 带语言头和复制按钮的代码块；围栏外逐行解析块级结构
 * （标题/列表/引用/分隔线/管道表格），行内解析 code/粗斜体/删除线/链接。
 * 流式渲染也复用本函数（chat.js 打字机逐帧整段重跑），未闭合围栏
 * 天然渲染成打开的代码块，生成中即可呈现。
 *
 * 例外：语言标为 math / latex / tex 的围栏当展示式交给 KaTeX——Codex/GPT 惯用
 * ```math 围栏承载公式（Claude 惯用 $$…$$），不特判就会渲染成代码块而非公式。 */
const MATH_FENCE = new Set(["math", "latex", "tex"]);

export function formatText(text) {
  const frag = document.createDocumentFragment();
  // split 产物：[文本, 语言, 段, 语言, 段, ...]——奇数下标是围栏行的语言捕获，
  // 偶数下标的段在 文本/代码体 之间交替（开栏后是代码体，闭栏后回到文本）
  const parts = String(text).split(/```([\w+-]*)[ \t]*\n?/);
  for (let i = 0; i < parts.length; i += 2) {
    const isCode = (i / 2) % 2 === 1;
    if (!isCode) renderBlocks(frag, parts[i]);
    else if (MATH_FENCE.has((parts[i - 1] || "").toLowerCase()))
      frag.appendChild(renderMath(parts[i].replace(/\n$/, "").trim(), true));
    else frag.appendChild(codeBlock(parts[i - 1] || "", parts[i].replace(/\n$/, "")));
  }
  return frag;
}

function codeBlock(lang, body) {
  const lines = String(body).split("\n");
  const box = document.createElement("div");
  box.className = "codeblock";
  const head = document.createElement("div");
  head.className = "cb-head mono";

  // 左侧整体是折叠开关：箭头 + 语言 + 行数
  const toggle = document.createElement("button");
  toggle.type = "button";
  toggle.className = "cb-toggle";
  toggle.title = "折叠 / 展开代码";
  toggle.setAttribute("aria-expanded", "true");
  const caret = document.createElement("span");
  caret.className = "cb-caret";
  caret.appendChild(svgIcon("caret", 11));
  const l = document.createElement("span");
  l.className = "cb-lang";
  l.textContent = lang || "code";
  const cnt = document.createElement("span");
  cnt.className = "cb-count";
  cnt.textContent = lines.length + " 行";
  toggle.append(caret, l, cnt);
  toggle.addEventListener("click", () => {
    const folded = box.classList.toggle("folded");
    toggle.setAttribute("aria-expanded", String(!folded));
  });

  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "cb-copy";
  btn.append(svgIcon("copy", 12), document.createTextNode("复制"));
  btn.addEventListener("click", async () => {
    await copyText(body);
    btn.replaceChildren(svgIcon("check", 12), document.createTextNode("已复制"));
    btn.disabled = true;
    setTimeout(() => {
      btn.replaceChildren(svgIcon("copy", 12), document.createTextNode("复制"));
      btn.disabled = false;
    }, 1400);
  });
  head.append(toggle, btn);

  // 主体：行号列固定，代码区单独横向滚动（行高/字号与行号列严格一致）
  const wrap = document.createElement("div");
  wrap.className = "cb-body";
  const gutter = document.createElement("div");
  gutter.className = "cb-gutter mono";
  gutter.setAttribute("aria-hidden", "true");
  gutter.textContent = lines.map((_, i) => i + 1).join("\n");
  const pre = document.createElement("pre");
  const code = document.createElement("code");
  code.textContent = body;
  pre.appendChild(code);
  wrap.append(gutter, pre);

  box.append(head, wrap);
  return box;
}

/* ================= 数学公式（KaTeX） =================
 * 块级 $$…$$ / \[…\]（可跨行）在 renderBlocks 里成段渲染，行内 $…$ / \(…\)
 * 在 appendInline 里先于其它行内语法抽出。KaTeX 未加载或解析失败时回退成
 * 原样文本（throwOnError:false 让它把错误红字画在原位而不抛异常）。 */
function renderMath(tex, display) {
  const el = document.createElement(display ? "div" : "span");
  el.className = display ? "math-display" : "math-inline";
  const k = typeof window !== "undefined" ? window.katex : null;
  if (k) {
    try {
      k.render(tex, el, { displayMode: display, throwOnError: false });
      return el;
    } catch (_) { /* 回退到原样文本 */ }
  }
  el.textContent = (display ? "$$" : "$") + tex + (display ? "$$" : "$");
  return el;
}

/* lines[i] 若是块级公式起始（$$ 或 \[），渲染并返回消费到的下一行下标；
 * 否则返回 -1，交回普通块处理。未找到闭合分隔符时同样返回 -1（按普通文本处理）。 */
function tryDisplayMath(parent, lines, i, flush) {
  const trimmed = lines[i].trim();
  const open = trimmed.startsWith("$$") ? "$$" : trimmed.startsWith("\\[") ? "\\[" : "";
  if (!open) return -1;
  const close = open === "$$" ? "$$" : "\\]";
  const rest = trimmed.slice(open.length);
  const inline = rest.indexOf(close);
  if (inline !== -1) { // 单行 $$ … $$
    const tex = rest.slice(0, inline).trim();
    if (tex) { flush(); parent.appendChild(renderMath(tex, true)); }
    return i + 1;
  }
  const buf = rest ? [rest] : [];
  for (let j = i + 1; j < lines.length; j++) {
    const ci = lines[j].indexOf(close);
    if (ci === -1) { buf.push(lines[j]); continue; }
    const head = lines[j].slice(0, ci);
    if (head.trim()) buf.push(head);
    flush();
    parent.appendChild(renderMath(buf.join("\n").trim(), true));
    return j + 1;
  }
  return -1; // 没有闭合，当普通文本
}

const LIST_ITEM_RE = /^\s*(?:[-*+]|\d+[.)])\s+/;

function renderBlocks(parent, text) {
  const lines = String(text).split("\n");
  const para = [];
  const flush = () => {
    if (!para.length) return;
    const p = document.createElement("div");
    p.className = "md-p";
    appendInline(p, para.join("\n"));
    parent.appendChild(p);
    para.length = 0;
  };
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const dm = tryDisplayMath(parent, lines, i, flush);
    if (dm !== -1) { i = dm; continue; }
    let m;
    if ((m = line.match(/^(#{1,4})\s+(.*)$/))) {
      flush();
      const h = document.createElement("div");
      h.className = "md-h md-h" + m[1].length;
      appendInline(h, m[2]);
      parent.appendChild(h);
      i++;
    } else if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
      flush();
      parent.appendChild(Object.assign(document.createElement("div"), { className: "md-hr" }));
      i++;
    } else if (/^\s*>\s?/.test(line)) {
      flush();
      const buf = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        buf.push(lines[i].replace(/^\s*>\s?/, ""));
        i++;
      }
      const bq = document.createElement("blockquote");
      bq.className = "md-quote";
      appendInline(bq, buf.join("\n"));
      parent.appendChild(bq);
    } else if (LIST_ITEM_RE.test(line)) {
      flush();
      i = renderList(parent, lines, i);
    } else if (isTableStart(lines, i)) {
      flush();
      i = renderTable(parent, lines, i);
    } else if (line.trim() === "") {
      flush();
      i++;
    } else {
      para.push(line);
      i++;
    }
  }
  flush();
}

function renderList(parent, lines, i) {
  const ordered = /^\s*\d+[.)]\s+/.test(lines[i]);
  const itemRe = ordered ? /^\s*\d+[.)]\s+(.*)$/ : /^\s*[-*+]\s+(.*)$/;
  const list = document.createElement(ordered ? "ol" : "ul");
  list.className = "md-list";
  let li = null;
  while (i < lines.length) {
    const m = lines[i].match(itemRe);
    if (m) {
      li = document.createElement("li");
      appendInline(li, m[1]);
      list.appendChild(li);
      i++;
    } else if (LIST_ITEM_RE.test(lines[i])) {
      break; // 有序/无序切换，交回上层重开列表
    } else if (li && /^\s+\S/.test(lines[i])) {
      // 缩进续行并入当前项
      li.appendChild(document.createTextNode("\n"));
      appendInline(li, lines[i].trim());
      i++;
    } else break;
  }
  parent.appendChild(list);
  return i;
}

function isTableStart(lines, i) {
  return /^\s*\|.*\|\s*$/.test(lines[i] || "") &&
    /^\s*\|?[\s:|-]+\|?\s*$/.test(lines[i + 1] || "") &&
    (lines[i + 1] || "").includes("-");
}

function renderTable(parent, lines, i) {
  const rows = [];
  while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) { rows.push(lines[i]); i++; }
  const wrap = document.createElement("div");
  wrap.className = "md-tablewrap";
  const table = document.createElement("table");
  table.className = "md-table";
  rows.forEach((row, idx) => {
    if (idx === 1) return; // 对齐分隔行
    const tr = document.createElement("tr");
    for (const cell of row.trim().replace(/^\|/, "").replace(/\|$/, "").split("|")) {
      const td = document.createElement(idx === 0 ? "th" : "td");
      appendInline(td, cell.trim());
      tr.appendChild(td);
    }
    table.appendChild(tr);
  });
  wrap.appendChild(table);
  parent.appendChild(wrap);
  return i;
}

/* 行内：`code`、**粗**、*斜*、~~删除~~、[文字](http://…)、裸链接 */
const INLINE_RE = /`([^`\n]+)`|\*\*([^*\n]+)\*\*|__([^_\n]+)__|\*([^*\n]+)\*|~~([^~\n]+)~~|\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>()"']+)/g;

/* 行内数学：\(…\)、$…$ 是行内式；\[…\]、$$…$$ 本是展示式，但只有「独占一行」
 * 时才由 renderBlocks/tryDisplayMath 成段渲染——Codex/GPT 常把 \[…\] 写在句中或
 * 列表项里（Claude 惯用 $ / 独占行的 $$，天然走块级），这类嵌在文内的展示式若不
 * 在此兜住就会漏成原样文本。故这里一并抽出，展示式分隔符也按行内式呈现（不断行、
 * 不丢渲染）。$…$ 规则：$ 后不接空白、闭合 $ 前不接空白、闭合后不接数字，且避开
 * $$ 与转义 \$，以降低把货币金额误判成公式的概率。展示式分隔符更长，排在前面先匹配。
 * 抽出公式后，其余文字再交给 appendInlineFmt 走常规行内语法。 */
const INLINE_MATH_RE = /\\\[([\s\S]+?)\\\]|\$\$([\s\S]+?)\$\$|\\\(([\s\S]+?)\\\)|(?<![\\$])\$(?!\s)([^$\n]+?)(?<!\s)\$(?!\$)(?!\d)/g;

function appendInline(parent, text) {
  const str = String(text);
  let last = 0;
  for (const m of str.matchAll(INLINE_MATH_RE)) {
    if (m.index > last) appendInlineFmt(parent, str.slice(last, m.index));
    const tex = m[1] != null ? m[1] : m[2] != null ? m[2] : m[3] != null ? m[3] : m[4];
    parent.appendChild(renderMath(tex, false));
    last = m.index + m[0].length;
  }
  appendInlineFmt(parent, str.slice(last));
}

function appendInlineFmt(parent, text) {
  const str = String(text);
  let last = 0;
  for (const m of str.matchAll(INLINE_RE)) {
    if (m.index > last) parent.appendChild(document.createTextNode(str.slice(last, m.index)));
    if (m[1]) {
      const code = document.createElement("code");
      code.textContent = m[1];
      parent.appendChild(code);
    } else if (m[2] || m[3]) {
      const b = document.createElement("strong");
      b.textContent = m[2] || m[3];
      parent.appendChild(b);
    } else if (m[4]) {
      const em = document.createElement("em");
      em.textContent = m[4];
      parent.appendChild(em);
    } else if (m[5]) {
      const del = document.createElement("del");
      del.textContent = m[5];
      parent.appendChild(del);
    } else {
      const a = document.createElement("a");
      a.className = "md-link";
      a.textContent = m[6] || m[8];
      a.href = m[7] || m[8];
      a.target = "_blank";
      a.rel = "noopener";
      parent.appendChild(a);
    }
    last = m.index + m[0].length;
  }
  if (last < str.length) parent.appendChild(document.createTextNode(str.slice(last)));
}

/* 一条落盘的 logEntry（线程 jsonl 行）→ 节点数组（历史加载入口）。
 * chat_session 等簿记类条目对用户不可见，落到兜底分支返回空。 */
export function renderEntry(raw) {
  if (!raw || typeof raw !== "object") return [];
  if (raw.kind === "user") return [renderUserMsg(raw.text)];
  if (raw.kind === "event") return renderEvent(raw.event);
  if (raw.kind === "divider") return [divider(raw.ts ? "新对话 · " + fmtTime(raw.ts) : "")];
  if (raw.kind === "status" && raw.state === "error") return [chip(raw.error, "err")];
  return [];
}

function summarizeInput(input) {
  if (!input || typeof input !== "object") return "";
  const cand = input.command || input.file_path || input.path || input.pattern ||
    input.query || input.url || input.description;
  const s = cand ? String(cand) : JSON.stringify(input);
  return s.length > 100 ? s.slice(0, 100) + "…" : s;
}

/* 流式生成中的临时节点：body 由 chat.js 的打字机填充（text 走 Markdown
 * 重渲染，thinking 纯文本），完整事件到达后整体移除交给正式渲染。
 * thinking 生成中默认展开。 */
export function liveNode(kind) {
  if (kind === "thinking") {
    const det = document.createElement("details");
    det.className = "think";
    det.open = true;
    const sum = document.createElement("summary");
    sum.textContent = "思考过程";
    const body = document.createElement("div");
    body.className = "think-body";
    det.append(sum, body);
    return { el: det, body };
  }
  const d = document.createElement("div");
  d.className = "msg agent live-text";
  return { el: d, body: d };
}

function thinkBlock(text) {
  const { el, body } = liveNode("thinking");
  el.open = false;
  body.textContent = text;
  return el;
}

/* rateLimitType → 额度名称，对齐原生 Claude Code 的 session/weekly limit 措辞 */
const RATE_LIMIT_LABEL = {
  five_hour: "5 小时额度",
  seven_day: "周额度",
  seven_day_opus: "Opus 周额度",
  seven_day_sonnet: "Sonnet 周额度",
  seven_day_overage_included: "周额度",
  overage: "用量积分额度",
};

function resetsAt(sec) {
  if (!sec) return "";
  const d = new Date(sec * 1000);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getMonth() + 1}/${d.getDate()} ${p(d.getHours())}:${p(d.getMinutes())} 重置`;
}

/* 限流事件：allowed=正常不展示，allowed_warning=接近上限（黄），rejected=已被拦（红） */
function rateLimitChip(info) {
  if (!info || !info.status || info.status === "allowed") return null;
  const label = RATE_LIMIT_LABEL[info.rateLimitType] || "用量额度";
  const reset = resetsAt(info.resetsAt);
  if (info.status === "rejected") {
    return chip(["⛔ 已用尽" + label, reset].filter(Boolean).join(" · "), "err");
  }
  const pct = typeof info.utilization === "number" ? "已用 " + Math.round(info.utilization * 100) + "%" : "";
  // allowed_warning 之外的未知状态照原样带出，免得又变成看不懂的提示
  const head = (info.status === "allowed_warning" ? "接近" : info.status + " · ") + label;
  const parts = ["⚠ " + head, pct, reset];
  if (info.isUsingOverage) parts.push("正在使用用量积分");
  return chip(parts.filter(Boolean).join(" · "), "warn");
}

/* 渲染一条 agent 事件（Claude stream-json / Codex --json），返回节点数组 */
export function renderEvent(ev) {
  if (!ev || typeof ev !== "object") return [];
  const out = [];

  // Claude Code 事件
  if (ev.type === "system") {
    return out; // 所有 system 事件（init 及其它 subtype）均属系统信息，对用户无意义，一律不展示
  }
  if (ev.type === "assistant" && ev.message && Array.isArray(ev.message.content)) {
    for (const block of ev.message.content) {
      if (block.type === "text" && block.text) out.push(agentText(block.text));
      else if (block.type === "tool_use") out.push(toolChip(block.name, summarizeInput(block.input)));
      else if (block.type === "thinking" && block.thinking) out.push(thinkBlock(block.thinking));
    }
    return out;
  }
  if (ev.type === "user") {
    return out; // 工具结果回填，不展示（终端里能看到实际效果）
  }
  if (ev.type === "rate_limit_event") {
    const c = rateLimitChip(ev.rate_limit_info);
    if (c) out.push(c);
    return out;
  }
  if (ev.type === "result") {
    if (ev.subtype === "success") {
      const secs = ev.duration_ms ? (ev.duration_ms / 1000).toFixed(1) + "s" : "";
      const cost = typeof ev.total_cost_usd === "number" ? "$" + ev.total_cost_usd.toFixed(4) : "";
      out.push(chip(["✓ 回合完成", secs, cost].filter(Boolean).join(" · "), "result"));
    } else {
      out.push(chip("✗ " + (ev.result || ev.subtype || "回合失败"), "err"));
    }
    return out;
  }

  // Codex 事件（0.14x 的 thread/turn/item 结构）
  if (ev.type === "thread.started") {
    return out; // 同 Claude init：会话就绪属 system 信息，不展示
  }
  if (ev.type === "item.started" && ev.item) {
    if (ev.item.type === "command_execution") {
      out.push(toolChip("exec", String(ev.item.command || "").slice(0, 100)));
    }
    return out;
  }
  if ((ev.type === "item.completed" || ev.type === "item.updated") && ev.item) {
    const it = ev.item;
    if (ev.type === "item.completed") {
      if (it.type === "agent_message" && it.text) out.push(agentText(it.text));
      else if (it.type === "reasoning" && it.text) out.push(thinkBlock(it.text));
      else if (it.type === "file_change" && Array.isArray(it.changes)) {
        out.push(toolChip("edit", it.changes.map((c) => c.path).join(" ").slice(0, 100)));
      } else if (it.type === "web_search" && it.query) out.push(toolChip("search", it.query));
    }
    return out;
  }
  if (ev.type === "turn.started") return out;
  if (ev.type === "turn.completed") {
    const u = ev.usage || {};
    const tokens = u.input_tokens ? ` · ${u.input_tokens}↑ ${u.output_tokens || 0}↓` : "";
    out.push(chip("✓ 回合完成" + tokens, "result"));
    return out;
  }
  if (ev.type === "turn.failed") {
    out.push(chip("✗ " + (ev.error && ev.error.message || "回合失败"), "err"));
    return out;
  }
  if (ev.type === "error" && ev.message) {
    out.push(chip("✗ " + ev.message, "err"));
    return out;
  }

  // Codex 旧版事件（{"msg":{...}} 结构，宽松匹配）
  const m = ev.msg || ev;
  if (m && typeof m === "object" && typeof m.type === "string") {
    if (m.type === "agent_message" && m.message) { out.push(agentText(m.message)); return out; }
    if (m.type === "agent_reasoning" && m.text) { out.push(thinkBlock(m.text)); return out; }
    if (m.type.startsWith("exec_command_begin")) {
      out.push(toolChip("exec", Array.isArray(m.command) ? m.command.join(" ") : m.command || ""));
      return out;
    }
    if (m.type === "task_complete") { out.push(chip("✓ 回合完成", "result")); return out; }
    if (m.type === "error" && m.message) { out.push(chip("✗ " + m.message, "err")); return out; }
    if (m.type.startsWith("exec_command_output") || m.type === "task_started" ||
        m.type === "session_configured" || m.type === "token_count") {
      return out; // 噪音事件不展示
    }
  }

  // 未识别事件：折叠原始 JSON，方便排查
  const det = document.createElement("details");
  det.className = "raw";
  const sum = document.createElement("summary");
  sum.textContent = "· " + (ev.type || "event");
  const pre = document.createElement("pre");
  pre.textContent = JSON.stringify(ev, null, 2);
  det.append(sum, pre);
  out.push(det);
  return out;
}
