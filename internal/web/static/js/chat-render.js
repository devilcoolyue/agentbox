/* chat-render：对话流的纯渲染管线 —— 用户消息、agent 事件（Claude stream-json /
 * Codex --json 新旧两种结构）、轻量 Markdown、附件缩略图。无状态副作用。
 * 全部通过 DOM 构建输出，模型文本永远走 textContent，不存在注入面。 */
"use strict";
import { openLightbox, fmtTime, toast } from "./util.js";
import { imgURLFromPath } from "./api.js";
import { svgIcon, buttonLabel, actionButton } from "./icons.js";
export { svgIcon } from "./icons.js";
import { setTip } from "./tip.js";
export const USER_ATTACH_RE = /\[(图片|附件)#(\d+) (\/shared\/\.(?:images|file)\/[A-Za-z0-9._-]+)\]/g;
/* 复制到剪贴板：clipboard API 优先，非安全上下文回退 execCommand */
export async function copyText(text) {
    try {
        await navigator.clipboard.writeText(text);
    }
    catch (_) {
        const focused = document.activeElement;
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.style.cssText = "position:fixed;opacity:0";
        document.body.appendChild(ta);
        ta.select();
        try {
            if (!document.execCommand("copy"))
                throw new Error("copy failed");
        }
        finally {
            ta.remove();
            focused?.focus({ preventScroll: true });
        }
    }
}
export function chip(text, extra) {
    const c = document.createElement("div");
    c.className = "chip" + (extra ? " " + extra : "");
    c.textContent = text;
    return c;
}
/* 工具 chip；带 htmlPath 时在末尾挂一个「预览」按钮，产品改原型不用再切到文件页
 * 翻目录。真正的打开逻辑在 preview.ts 用事件委托接管（此处保持纯渲染，也避开
 * chat-render ↔ preview ↔ files 的环形依赖）。 */
export function toolChip(name, summary, htmlPath = "") {
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
    if (htmlPath) {
        // 改成 flex 行：路径再长也只压缩 .sum，「预览」按钮不会被 ellipsis 裁掉
        c.classList.add("has-open");
        const b = document.createElement("button");
        b.type = "button";
        b.className = "btn chip-open";
        b.dataset.htmlPreview = htmlPath;
        buttonLabel(b, "预览", "eye");
        setTip(b, "在预览窗口渲染 " + htmlPath);
        c.appendChild(b);
    }
    setTip(c, summary || name);
    return c;
}
/* 上下文分割线：divider 之前的消息不再带入后续对话 */
export function divider(label) {
    const d = document.createElement("div");
    d.className = "chat-divider";
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
        if (m.index > last)
            d.appendChild(document.createTextNode(str.slice(last, m.index)));
        if (m[1] === "图片")
            d.appendChild(chatThumb(m[3], "图片 #" + m[2]));
        else
            d.appendChild(fileLink(m[3], "附件 #" + m[2]));
        last = m.index + m[0].length;
    }
    if (last < str.length)
        d.appendChild(document.createTextNode(str.slice(last)));
    return d;
}
/* 消息气泡里的图片缩略图 */
export function chatThumb(containerPath, title) {
    const img = document.createElement("img");
    img.className = "chat-thumb";
    img.src = imgURLFromPath(containerPath);
    img.alt = title;
    setTip(img, title + "（点击查看大图）");
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
    setTip(a, title + " · " + containerPath);
    a.href = imgURLFromPath(containerPath);
    a.target = "_blank";
    return a;
}
// Keep source Markdown independently of rendered DOM (code line numbers,
// toolbar labels and KaTeX accessibility text must never enter the clipboard).
export const answerSources = new WeakMap();
function agentText(text) {
    const d = document.createElement("div");
    d.className = "msg agent";
    d.appendChild(formatText(text));
    answerSources.set(d, text);
    return d;
}
/* ================= 轻量 Markdown =================
 * 代码围栏 → 带语言头和复制按钮的代码块；围栏外逐行解析块级结构
 * （标题/列表/引用/分隔线/管道表格），行内解析 code/粗斜体/删除线/链接。
 * 流式渲染也复用本函数（chat.ts 打字机逐帧整段重跑），未闭合围栏
 * 天然渲染成打开的代码块，生成中即可呈现。
 *
 * 例外：语言标为 math / latex / tex 的围栏当展示式交给 KaTeX——Codex/GPT 惯用
 * ```math 围栏承载公式（Claude 惯用 $$…$$），不特判就会渲染成代码块而非公式。 */
const MATH_FENCE = new Set(["math", "latex", "tex"]);
/* 拆 YAML front matter：只认最简单的 key: value 单行形式，与服务端取技能
 * description 的口径一致。没有 front matter 时原样返回正文。
 * 渲染 .md 的地方（技能文档、文件预览）都得先剥掉它，否则开头的 --- 会被当成
 * 一条分隔线加几行文字。 */
export function splitFrontMatter(text) {
    const norm = text.replace(/\r\n/g, "\n");
    if (!norm.startsWith("---\n"))
        return { meta: [], body: norm };
    const end = norm.indexOf("\n---", 3);
    if (end < 0)
        return { meta: [], body: norm };
    const head = norm.slice(4, end);
    const body = norm.slice(end + 4).replace(/^\n+/, "");
    const meta = [];
    for (const line of head.split("\n")) {
        const i = line.indexOf(":");
        if (i <= 0 || /^\s/.test(line))
            continue; // 缩进行是上一个键的续行，跳过
        meta.push([line.slice(0, i).trim(), line.slice(i + 1).trim().replace(/^["']|["']$/g, "")]);
    }
    return { meta, body };
}
/* front matter 渲染成一排灰色小标签，信息不丢又不占正文位置 */
export function frontMatterChips(meta) {
    const fm = document.createElement("div");
    fm.className = "skill-fm";
    for (const [k, v] of meta) {
        const item = document.createElement("span");
        item.className = "skill-fm-item mono";
        item.textContent = `${k}: ${v}`;
        fm.appendChild(item);
    }
    return fm;
}
/* 渲染全程同步，用一个模块级变量就够，不必把 opts 一路穿到行内解析里 */
let imgResolver = null;
export function formatText(text, opts = {}) {
    imgResolver = opts.img || null;
    try {
        const frag = document.createDocumentFragment();
        // split 产物：[文本, 语言, 段, 语言, 段, ...]——奇数下标是围栏行的语言捕获，
        // 偶数下标的段在 文本/代码体 之间交替（开栏后是代码体，闭栏后回到文本）
        const parts = String(text).split(/```([\w+-]*)[ \t]*\n?/);
        for (let i = 0; i < parts.length; i += 2) {
            const isCode = (i / 2) % 2 === 1;
            if (!isCode)
                renderBlocks(frag, parts[i]);
            else if (MATH_FENCE.has((parts[i - 1] || "").toLowerCase()))
                frag.appendChild(renderMath(parts[i].replace(/\n$/, "").trim(), true));
            else
                frag.appendChild(codeBlock(parts[i - 1] || "", parts[i].replace(/\n$/, "")));
        }
        return frag;
    }
    finally {
        imgResolver = null;
    }
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
    setTip(toggle, "折叠 / 展开代码");
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
    btn.className = "btn cb-copy";
    actionButton(btn, "复制", "copy", "复制代码");
    btn.addEventListener("click", async () => {
        try {
            await copyText(body);
        }
        catch (_) {
            toast("复制失败，请选择内容后手动复制", true);
            return;
        }
        actionButton(btn, "已复制", "check", "代码已复制");
        btn.disabled = true;
        setTimeout(() => {
            actionButton(btn, "复制", "copy", "复制代码");
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
        }
        catch (_) { /* 回退到原样文本 */ }
    }
    el.textContent = (display ? "$$" : "$") + tex + (display ? "$$" : "$");
    return el;
}
/* lines[i] 若是块级公式起始（$$ 或 \[），渲染并返回消费到的下一行下标；
 * 否则返回 -1，交回普通块处理。未找到闭合分隔符时同样返回 -1（按普通文本处理）。 */
function tryDisplayMath(parent, lines, i, flush) {
    const trimmed = lines[i].trim();
    const open = trimmed.startsWith("$$") ? "$$" : trimmed.startsWith("\\[") ? "\\[" : "";
    if (!open)
        return -1;
    const close = open === "$$" ? "$$" : "\\]";
    const rest = trimmed.slice(open.length);
    const inline = rest.indexOf(close);
    if (inline !== -1) { // 单行 $$ … $$
        const tex = rest.slice(0, inline).trim();
        if (tex) {
            flush();
            parent.appendChild(renderMath(tex, true));
        }
        return i + 1;
    }
    const buf = rest ? [rest] : [];
    for (let j = i + 1; j < lines.length; j++) {
        const ci = lines[j].indexOf(close);
        if (ci === -1) {
            buf.push(lines[j]);
            continue;
        }
        const head = lines[j].slice(0, ci);
        if (head.trim())
            buf.push(head);
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
        if (!para.length)
            return;
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
        if (dm !== -1) {
            i = dm;
            continue;
        }
        let m;
        if ((m = line.match(/^(#{1,4})\s+(.*)$/))) {
            flush();
            const h = document.createElement("div");
            h.className = "md-h md-h" + m[1].length;
            appendInline(h, m[2]);
            parent.appendChild(h);
            i++;
        }
        else if (/^\s*(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
            flush();
            parent.appendChild(Object.assign(document.createElement("div"), { className: "md-hr" }));
            i++;
        }
        else if (/^\s*>\s?/.test(line)) {
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
        }
        else if (LIST_ITEM_RE.test(line)) {
            flush();
            i = renderList(parent, lines, i);
        }
        else if (isTableStart(lines, i)) {
            flush();
            i = renderTable(parent, lines, i);
        }
        else if (line.trim() === "") {
            flush();
            i++;
        }
        else {
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
        }
        else if (LIST_ITEM_RE.test(lines[i])) {
            break; // 有序/无序切换，交回上层重开列表
        }
        else if (li && /^\s+\S/.test(lines[i])) {
            // 缩进续行并入当前项
            li.appendChild(document.createTextNode("\n"));
            appendInline(li, lines[i].trim());
            i++;
        }
        else
            break;
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
    while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) {
        rows.push(lines[i]);
        i++;
    }
    const wrap = document.createElement("div");
    wrap.className = "md-tablewrap";
    const table = document.createElement("table");
    table.className = "md-table";
    rows.forEach((row, idx) => {
        if (idx === 1)
            return; // 对齐分隔行
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
/* 行内：`code`、**粗**、*斜*、~~删除~~、[文字](http://…)、裸链接、
 * ![alt](src) 与裸 <img …>（README 里常拿它控制 logo 尺寸，纯 md 语法做不到）。
 * 图片两支只在有 imgResolver 时成图，否则退回原样文字。 */
const INLINE_RE = /`([^`\n]+)`|\*\*([^*\n]+)\*\*|__([^_\n]+)__|\*([^*\n]+)\*|~~([^~\n]+)~~|\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>()"']+)|!\[([^\]\n]*)\]\(\s*([^\s)]+)[^)\n]*\)|<img\s([^<>]*?)\/?>/g;
function mdLink(text, href) {
    const a = document.createElement("a");
    a.className = "md-link";
    a.textContent = text;
    a.href = href;
    a.target = "_blank";
    a.rel = "noopener";
    return a;
}
/* 裸 <img> 标签的属性表：只按名取值，标签本身永远不进 innerHTML，
 * 所以 onerror 之类的事件属性天然被丢在门外。 */
function htmlAttrs(raw) {
    const out = {};
    for (const m of raw.matchAll(/([a-zA-Z-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))/g)) {
        out[m[1].toLowerCase()] = m[2] ?? m[3] ?? m[4] ?? "";
    }
    return out;
}
/* 只认 src / alt / width / height / title 这几个属性，尺寸还得是纯数字或百分比。
 * 解析器拿不出 URL（没传解析器、或路径落在够不着的地方）时返回 null，
 * 由调用方退回成原样文字。 */
function mdImage(src, at) {
    const url = imgResolver ? imgResolver(src) : "";
    if (!url)
        return null;
    const img = document.createElement("img");
    img.className = "md-img";
    img.src = url;
    img.alt = at.alt || "";
    if (at.title)
        setTip(img, at.title);
    for (const k of ["width", "height"]) {
        const v = (at[k] || "").trim();
        if (/^\d+(px|%)?$/.test(v))
            img.style[k] = /^\d+$/.test(v) ? v + "px" : v;
    }
    return img;
}
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
        if (m.index > last)
            appendInlineFmt(parent, str.slice(last, m.index));
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
        if (m.index > last)
            parent.appendChild(document.createTextNode(str.slice(last, m.index)));
        if (m[1]) {
            const code = document.createElement("code");
            code.textContent = m[1];
            parent.appendChild(code);
        }
        else if (m[2] || m[3]) {
            const b = document.createElement("strong");
            b.textContent = m[2] || m[3];
            parent.appendChild(b);
        }
        else if (m[4]) {
            const em = document.createElement("em");
            em.textContent = m[4];
            parent.appendChild(em);
        }
        else if (m[5]) {
            const del = document.createElement("del");
            del.textContent = m[5];
            parent.appendChild(del);
        }
        else if (m[10] != null) {
            // 成不了图时（对话流没有解析器）：远程图退成链接，本地路径退成原样文字
            parent.appendChild(mdImage(m[10], { alt: m[9] || "" }) ||
                (/^https?:\/\//i.test(m[10]) ? mdLink(m[9] || m[10], m[10]) : document.createTextNode(m[0])));
        }
        else if (m[11] != null) {
            const at = htmlAttrs(m[11]);
            parent.appendChild(mdImage(at.src || "", at) || document.createTextNode(m[0]));
        }
        else {
            parent.appendChild(mdLink(m[6] || m[8], m[7] || m[8]));
        }
        last = m.index + m[0].length;
    }
    if (last < str.length)
        parent.appendChild(document.createTextNode(str.slice(last)));
}
/* 一条落盘的 logEntry（线程 jsonl 行）→ 节点数组（历史加载入口）。
 * chat_session 等簿记类条目对用户不可见，落到兜底分支返回空。 */
export function renderEntry(raw) {
    if (!raw || typeof raw !== "object")
        return [];
    if (raw.kind === "user")
        return [renderUserMsg(raw.text)];
    if (raw.kind === "event")
        return renderEvent(raw.event);
    if (raw.kind === "divider")
        return [divider(raw.ts ? "新对话 · " + fmtTime(raw.ts) : "")];
    if (raw.kind === "status" && raw.state === "error")
        return [chip(raw.error, "err")];
    return [];
}
/* 工具入参里指向 HTML 的落盘路径 —— 有它才给 chip 挂「预览」。只认写文件类的
 * 入参名，Read/Grep 命中的 .html 不算「刚做出来的东西」，不必给入口。 */
function htmlOutputPath(input) {
    if (!input || typeof input !== "object")
        return "";
    const o = input;
    const p = String(o.file_path || o.path || o.notebook_path || "");
    return /\.html?$/i.test(p) ? p : "";
}
function summarizeInput(input) {
    if (!input || typeof input !== "object")
        return "";
    const o = input;
    const cand = o.command || o.file_path || o.path || o.pattern ||
        o.query || o.url || o.description;
    const s = cand ? String(cand) : JSON.stringify(input);
    return s.length > 100 ? s.slice(0, 100) + "…" : s;
}
/* 流式生成中的临时节点：body 由 chat.ts 的打字机填充（text 走 Markdown
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
    if (!sec)
        return "";
    return `${fmtTime(sec * 1000)} 重置`;
}
/* 限流事件：allowed=正常不展示，allowed_warning=接近上限（黄），rejected=已被拦（红） */
function rateLimitChip(info) {
    if (!info || !info.status || info.status === "allowed")
        return null;
    const label = RATE_LIMIT_LABEL[info.rateLimitType] || "用量额度";
    const reset = resetsAt(info.resetsAt);
    if (info.status === "rejected") {
        return chip(["⛔ 已用尽" + label, reset].filter(Boolean).join(" · "), "err");
    }
    const pct = typeof info.utilization === "number" ? "已用 " + Math.round(info.utilization * 100) + "%" : "";
    // allowed_warning 之外的未知状态照原样带出，免得又变成看不懂的提示
    const head = (info.status === "allowed_warning" ? "接近" : info.status + " · ") + label;
    const parts = ["⚠ " + head, pct, reset];
    if (info.isUsingOverage)
        parts.push("正在使用用量积分");
    return chip(parts.filter(Boolean).join(" · "), "warn");
}
/* 渲染一条 agent 事件（Claude stream-json / Codex --json），返回节点数组 */
export function renderEvent(ev) {
    if (!ev || typeof ev !== "object")
        return [];
    const out = [];
    // Claude Code 事件
    if (ev.type === "system") {
        return out; // 所有 system 事件（init 及其它 subtype）均属系统信息，对用户无意义，一律不展示
    }
    if (ev.type === "assistant" && ev.message && Array.isArray(ev.message.content)) {
        for (const block of ev.message.content) {
            if (block.type === "text" && block.text)
                out.push(agentText(block.text));
            else if (block.type === "tool_use") {
                out.push(toolChip(block.name, summarizeInput(block.input), htmlOutputPath(block.input)));
            }
            else if (block.type === "thinking" && block.thinking)
                out.push(thinkBlock(block.thinking));
        }
        return out;
    }
    if (ev.type === "user") {
        return out; // 工具结果回填，不展示（终端里能看到实际效果）
    }
    if (ev.type === "rate_limit_event") {
        const c = rateLimitChip(ev.rate_limit_info);
        if (c)
            out.push(c);
        return out;
    }
    if (ev.type === "result") {
        if (ev.subtype === "success") {
            const secs = ev.duration_ms ? (ev.duration_ms / 1000).toFixed(1) + "s" : "";
            // Cost is shown in the footer from committed usage, including table-priced
            // Codex/Claude fallbacks. Raw CLI totals could disagree with settlement.
            out.push(chip(["✓ 回合完成", secs].filter(Boolean).join(" · "), "result"));
        }
        else {
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
            if (it.type === "agent_message" && it.text)
                out.push(agentText(it.text));
            else if (it.type === "reasoning" && it.text)
                out.push(thinkBlock(it.text));
            else if (it.type === "file_change" && Array.isArray(it.changes)) {
                // 一次改了好几个文件时不猜要预览哪个，只有单个 HTML 才给入口
                const only = it.changes.length === 1 ? htmlOutputPath(it.changes[0]) : "";
                out.push(toolChip("edit", it.changes.map((c) => c.path).join(" ").slice(0, 100), only));
            }
            else if (it.type === "web_search" && it.query)
                out.push(toolChip("search", it.query));
        }
        return out;
    }
    if (ev.type === "turn.started")
        return out;
    if (ev.type === "turn.completed") {
        const u = ev.usage || {};
        const tokens = u.input_tokens ? ` · 入 ${u.input_tokens} · 出 ${u.output_tokens || 0} tokens` : "";
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
    // 没有 msg 包层时就把整个事件当它用（旧版结构宽松匹配）；两者字段名一致，
    // 断言成 msg 的形状即可，下面每个分支都会自己再判一次 type 与取值。
    const m = (ev.msg || ev);
    if (m && typeof m === "object" && typeof m.type === "string") {
        if (m.type === "agent_message" && m.message) {
            out.push(agentText(m.message));
            return out;
        }
        if (m.type === "agent_reasoning" && m.text) {
            out.push(thinkBlock(m.text));
            return out;
        }
        if (m.type.startsWith("exec_command_begin")) {
            out.push(toolChip("exec", Array.isArray(m.command) ? m.command.join(" ") : m.command || ""));
            return out;
        }
        if (m.type === "task_complete") {
            out.push(chip("✓ 回合完成", "result"));
            return out;
        }
        if (m.type === "error" && m.message) {
            out.push(chip("✗ " + m.message, "err"));
            return out;
        }
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
