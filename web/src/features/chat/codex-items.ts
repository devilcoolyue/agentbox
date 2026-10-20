/* Codex 的工具条目：命令、改动、MCP / 动态工具、网页搜索、看图、生图、子 Agent。
 * 外观与 Claude 的工具行一致（图标 + 工具名 + 摘要的细行），多出 CLI 才有的那些：
 * 命令按 CLI 的解析显示成 Read / Search / List / Shell，行尾是耗时与退出码，点开
 * 看完整命令和输出首尾；改动显示增删行数，点开看 diff。
 *
 * item.started 先画一行「进行中」（转圈 + 实时输出最后几行），同一 id 的
 * item.completed 到达时原地改成完成态，不另起一行；没有 id 的旧记录按命令串配对。
 * 字段来自 agent/appserver_items.go，exec --json 回退路径只有其中一部分，缺的就不显示。
 * 模型与命令输出一律走 textContent。 */
import type { AgentEvent } from "../../types.js";
import { buttonLabel, svgIcon } from "../../icons.js";
import { setTip } from "../../tip.js";
import { setText, setTextRender, t as i18nText } from "../../i18n.js";
import { S } from "../../state.js";
import { openLightbox } from "../../util.js";

type Item = NonNullable<AgentEvent["item"]>;
interface Action { type?: string; command?: string; name?: string; path?: string; query?: string }
interface Change { path?: string; kind?: string | { type?: string }; move_path?: string; added?: number; removed?: number; diff?: string; diff_truncated?: boolean }

const TYPES = new Set(["command_execution", "file_change", "mcp_tool_call", "dynamic_tool_call", "web_search",
  "image_view", "image_generation", "context_compaction", "collab_agent_tool_call", "sub_agent_activity",
  "review_mode", "sleep"]);

/* 进行中的条目：key → 行元素。完成时取出原地更新；上限防止中断的回合越攒越多 */
const pending = new Map<string, HTMLElement>();
const PENDING_MAX = 300;
/* 实时输出缓冲（只留末尾），随行元素回收 */
const liveText = new WeakMap<HTMLElement, string>();
const LIVE_KEEP = 4096, LIVE_LINES = 4;

const str = (v: unknown) => typeof v === "string" ? v : "";
const num = (v: unknown) => typeof v === "number" && Number.isFinite(v) ? v : undefined;

function itemKey(it: Item) {
  const id = str(it.id);
  if (id) return id;
  // 旧记录没有 id：同一条命令的开始与结束挨着落盘，按命令串配对
  return it.type === "command_execution" ? "cmd:" + str(it.command) : "";
}

/* 去掉 CLI 包的那层 `/bin/bash -lc '…'`，显示模型真正写的命令 */
export function unwrapShell(command: string) {
  const m = /^(?:\/usr)?(?:\/bin\/)?(?:bash|sh|zsh)\s+-l?c\s+([\s\S]+)$/.exec(command.trim());
  if (!m) return command;
  const arg = m[1].trim();
  if (arg.length >= 2 && arg[0] === "'" && arg.endsWith("'")) return arg.slice(1, -1).replaceAll("'\\''", "'");
  if (arg.length >= 2 && arg[0] === '"' && arg.endsWith('"')) return arg.slice(1, -1).replace(/\\(["\\$`])/g, "$1");
  return arg;
}

/* 执行指示里只放得下一小段 */
const clipLabel = (s: string) => s.length > 48 ? s.slice(0, 47) + "…" : s;
const base = (p: string) => p.replace(/\/+$/, "").split("/").pop() || p;
const oneLine = (s: string) => s.replace(/\s*\n\s*/g, " ⏎ ").trim();

/* 命令的显示名与摘要：全是读/列/搜时照 CLI 的 Explored 归类，否则就是 Shell */
function commandView(it: Item): { name: string; icon: string; sum: string; label: () => string } {
  const cmd = unwrapShell(str(it.command));
  const acts = (Array.isArray(it.actions) ? it.actions : []) as Action[];
  if (acts.length && acts.every(a => a.type === "read" || a.type === "list_files" || a.type === "search")) {
    const kinds = new Set(acts.map(a => a.type));
    const part = (a: Action) => a.type === "read" ? (a.name || base(a.path || a.command || ""))
      : a.type === "search" ? [a.query, a.path].filter(Boolean).join(" · ") || a.command || ""
      : a.path || ".";
    if (kinds.size === 1) {
      const t = acts[0].type;
      const sum = acts.map(part).join(", ");
      const what = clipLabel(sum);
      if (t === "read") return { name: "Read", icon: "file", sum, label: () => i18nText("读取 {p0}…", { p0: what }) };
      if (t === "search") return { name: "Search", icon: "search", sum, label: () => i18nText("搜索 {p0}…", { p0: what }) };
      return { name: "List", icon: "folder", sum, label: () => i18nText("列出 {p0}…", { p0: what }) };
    }
    const verb = (a: Action) => a.type === "read" ? "Read" : a.type === "search" ? "Search" : "List";
    return { name: "Explore", icon: "search", sum: acts.map(a => verb(a) + " " + part(a)).join(" · "), label: () => i18nText("浏览代码…") };
  }
  const what = clipLabel(oneLine(cmd));
  return { name: "Shell", icon: "tool", sum: oneLine(cmd), label: () => what ? i18nText("执行 {p0}…", { p0: what }) : i18nText("执行命令…") };
}

export function shortDuration(ms: number | undefined) {
  if (ms === undefined || ms < 100) return ""; // 瞬间完成的不标，耗时长的才值得看
  if (ms < 1000) return Math.round(ms) + "ms";
  if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
  const s = Math.round(ms / 1000);
  return Math.floor(s / 60) + "m " + (s % 60) + "s";
}

// eslint-disable-next-line no-control-regex
const ANSI = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]/g;
const clean = (s: string) => s.replace(ANSI, "").replace(/\r(?!\n)/g, "\n");

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls: string, text?: string) {
  const e = document.createElement(tag);
  e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

/* 一行工具：图标 + 名字 + 摘要 + 行尾信息；有详情时整行是 <summary>，可点开 */
interface Row { name: string; icon: string; sum: string; meta?: (HTMLElement | string)[]; body?: HTMLElement[]; tip?: string; htmlPath?: string }

function fillRow(wrap: HTMLElement, row: Row, running: boolean) {
  const line = el(row.body?.length ? "summary" : "div", "chip tool");
  const name = el("span", "name");
  name.append(running ? el("span", "tc-spin") : svgIcon(row.icon, 12), document.createTextNode(row.name));
  line.append(name);
  if (row.sum) line.append(el("span", "sum", row.sum));
  for (const m of row.meta || []) line.append(typeof m === "string" ? el("span", "tc-meta", m) : m);
  if (row.htmlPath) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "btn chip-open";
    b.dataset.htmlPreview = row.htmlPath;
    buttonLabel(b, () => i18nText("预览"), "eye");
    setTip(b, () => i18nText("在预览窗口渲染 ") + row.htmlPath);
    // 按钮在 <summary> 里：只打开预览（chat-log 上的委托处理），别顺带展开这一行
    b.addEventListener("click", e => e.preventDefault());
    line.append(b);
  }
  setTip(line, row.tip || row.sum || row.name);
  wrap.dataset.rowIcon = row.icon;
  if (running) wrap.dataset.status = "running";
  if (row.body?.length) {
    const det = document.createElement("details");
    const body = el("div", "tc-body");
    body.append(...row.body);
    det.append(line, body);
    wrap.replaceChildren(det);
  } else wrap.replaceChildren(line);
  if (running) wrap.append(el("pre", "tc-live mono"));
}

function badge(text: string, tone: "err" | "ok" | "warn" | "") {
  return el("span", "tc-badge" + (tone ? " " + tone : ""), text);
}

function statusBadge(status: string, exit?: number) {
  if (status === "failed") return badge(exit !== undefined ? i18nText("✗ 退出码 {p0}", { p0: String(exit) }) : i18nText("✗ 失败"), "err");
  if (status === "declined") return badge(i18nText("已拒绝"), "warn");
  if (status === "interrupted") return badge(i18nText("已中断"), "warn");
  if (exit !== undefined && exit !== 0) return badge(i18nText("✗ 退出码 {p0}", { p0: String(exit) }), "err");
  return null;
}

/* 首尾各留 20 行：服务端已截好的直接用；exec 回退路径给的是全文，这里照同样口径截 */
const SIDE_LINES = 20, SIDE_CHARS = 3000;
function outputParts(it: Item) {
  const full = clean(str(it.aggregated_output)).replace(/\n+$/, "");
  if (it.output_tail !== undefined || it.output_truncated) return { head: full, tail: clean(str(it.output_tail)).replace(/\n+$/, ""), cut: true, total: num(it.output_lines) };
  const lines = full.split("\n");
  if (lines.length <= 2 * SIDE_LINES && full.length <= 2 * SIDE_CHARS) return { head: full, tail: "", cut: false, total: lines.length };
  const tailN = Math.min(SIDE_LINES, Math.max(0, lines.length - SIDE_LINES));
  return { head: lines.slice(0, SIDE_LINES).join("\n").slice(0, SIDE_CHARS), tail: tailN ? lines.slice(-tailN).join("\n").slice(-SIDE_CHARS) : "", cut: true, total: lines.length };
}

function outputBlocks(it: Item) {
  const out: HTMLElement[] = [];
  const { head, tail, cut, total } = outputParts(it);
  if (!head && !tail) return out;
  out.push(el("pre", "tc-out mono", head));
  if (cut) {
    const shown = (head ? head.split("\n").length : 0) + (tail ? tail.split("\n").length : 0);
    const gap = el("div", "tc-gap");
    if (total && total > shown) setText(gap, "… 省略 {p0} 行 …", { p0: String(total - shown) });
    else setText(gap, "… 输出过长，中间已省略 …");
    out.push(gap);
    if (tail) out.push(el("pre", "tc-out mono", tail));
  }
  return out;
}

function commandRow(it: Item, completed: boolean): Row {
  const v = commandView(it);
  const cmd = unwrapShell(str(it.command));
  const status = str(it.status), exit = num(it.exit_code), dur = num(it.duration_ms);
  const meta: (HTMLElement | string)[] = [];
  if (completed) {
    const d = shortDuration(dur);
    if (d) meta.push(d);
    const b = statusBadge(status, exit);
    if (b) meta.push(b);
  }
  const body: HTMLElement[] = [];
  const line = el("div", "tc-cmd mono");
  line.append(el("span", "tc-prompt", "$"), document.createTextNode(cmd));
  body.push(line);
  const cwd = str(it.cwd);
  if (cwd && cwd !== "/workspace") body.push(el("div", "tc-note mono", "cwd " + cwd));
  if (completed) {
    const outs = outputBlocks(it);
    if (outs.length) body.push(...outs);
    // 只有明确记了「没输出」才这么说；旧记录根本没存输出
    else if (it.output_lines === 0 || it.aggregated_output === "") {
      const none = el("div", "tc-note");
      setText(none, "（无输出）");
      body.push(none);
    }
  }
  return { name: v.name, icon: v.icon, sum: v.sum, meta, body, tip: cmd };
}

function changeKind(c: Change) {
  return typeof c.kind === "string" ? c.kind : str(c.kind?.type);
}

function diffBlock(c: Change, kind: string) {
  const pre = el("pre", "tc-diff mono");
  const text = str(c.diff).replace(/\n$/, "");
  for (const l of text.split("\n")) {
    const cls = kind === "add" ? "add" : kind === "delete" ? "del"
      : l.startsWith("+++") || l.startsWith("---") ? "file" : l.startsWith("@@") ? "hunk"
      : l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : "";
    const span = el("span", cls ? "dl " + cls : "dl", (kind === "add" ? "+" : kind === "delete" ? "-" : "") + l);
    pre.append(span, "\n");
  }
  const out: HTMLElement[] = [pre];
  if (c.diff_truncated) {
    const gap = el("div", "tc-gap");
    setText(gap, "… 改动过长，后面已省略 …");
    out.push(gap);
  }
  return out;
}

function changeRows(it: Item): Row[] {
  const changes = (Array.isArray(it.changes) ? it.changes : []) as Change[];
  const status = str(it.status);
  return changes.map(c => {
    const kind = changeKind(c), path = str(c.path), move = str(c.move_path);
    const name = kind === "add" ? "Write" : kind === "delete" ? "Delete" : move ? "Move" : "Edit";
    const icon = kind === "add" ? "file-code" : kind === "delete" ? "trash" : move ? "move" : "edit";
    const meta: (HTMLElement | string)[] = [];
    const added = num(c.added), removed = num(c.removed);
    if (added || removed) {
      const stat = el("span", "tc-stat");
      if (added) stat.append(el("span", "add", "+" + added));
      if (removed) stat.append(el("span", "del", "−" + removed));
      meta.push(stat);
    }
    const b = statusBadge(status);
    if (b) meta.push(b);
    const htmlPath = changes.length === 1 && kind !== "delete" && /\.html?$/i.test(move || path) ? move || path : "";
    return { name, icon, sum: move ? path + " → " + move : path, meta, body: c.diff ? diffBlock(c, kind) : [], htmlPath };
  });
}

function textBlock(text: string, cls = "tc-out mono") {
  return el("pre", cls, text);
}

function toolRow(it: Item, completed: boolean): Row {
  const mcp = it.type === "mcp_tool_call";
  const name = mcp ? [str(it.server), str(it.tool)].filter(Boolean).join(".") : [str(it.namespace), str(it.tool)].filter(Boolean).join(".");
  const args = str(it.arguments);
  const meta: (HTMLElement | string)[] = [];
  const body: HTMLElement[] = [];
  if (args) body.push(textBlock(args, "tc-cmd mono"));
  if (completed) {
    const d = shortDuration(num(it.duration_ms));
    if (d) meta.push(d);
    const err = it.error && typeof it.error === "object" ? str((it.error as { message?: unknown }).message) : "";
    const failed = str(it.status) === "failed" || it.success === false || !!err;
    if (failed) meta.push(badge(i18nText("✗ 失败"), "err"));
    if (err) body.push(textBlock(err, "tc-out mono err"));
    const res = str(it.result_text);
    if (res) body.push(textBlock(res));
  }
  return { name: name || (mcp ? "MCP" : "Tool"), icon: mcp ? "plug" : "puzzle", sum: oneLine(args).slice(0, 160), meta, body };
}

function searchRow(it: Item, completed: boolean): Row {
  const act = (it.action && typeof it.action === "object" ? it.action : {}) as { type?: string; query?: string; queries?: unknown; url?: string; pattern?: string };
  const queries = Array.isArray(act.queries) ? act.queries.filter((q): q is string => typeof q === "string") : [];
  let sum = str(it.query) || str(act.query) || queries.join(" · ");
  let name = "WebSearch";
  if (act.type === "open_page") { name = "WebFetch"; sum = str(act.url) || sum; }
  if (act.type === "find_in_page") { name = "FindInPage"; sum = [str(act.pattern), str(act.url)].filter(Boolean).join(" @ ") || sum; }
  const body: HTMLElement[] = [];
  const meta: (HTMLElement | string)[] = [];
  if (queries.length > 1) body.push(textBlock(queries.join("\n"), "tc-cmd mono"));
  const results = (Array.isArray(it.results) ? it.results : []) as { title?: string; url?: string; domain?: string }[];
  if (results.length) {
    const list = el("ul", "tc-results");
    for (const r of results) {
      const li = document.createElement("li");
      const url = str(r.url);
      if (/^https?:\/\//i.test(url)) {
        const a = el("a", "", str(r.title) || url);
        a.href = url;
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        li.append(a);
      } else li.append(el("span", "", str(r.title) || url));
      if (r.domain) li.append(el("span", "tc-domain", str(r.domain)));
      list.append(li);
    }
    body.push(list);
  }
  const count = num(it.result_count);
  if (completed && count) {
    const c = el("span", "tc-meta");
    setText(c, "{p0} 条结果", { p0: String(count) });
    meta.push(c);
  }
  return { name, icon: "globe", sum: sum || (completed ? "" : i18nText("搜索中…")), meta, body };
}

/* 容器里的路径 → 文件直链（/workspace 与 /shared 两个范围），其它路径不给图 */
function fileURL(path: string) {
  if (!S.current) return "";
  const p = path.replace(/^file:\/\//, "");
  const [rel, scope] = p.startsWith("/workspace/") ? [p.slice(11), ""] : p.startsWith("/shared/") ? [p.slice(8), "&scope=shared"] : ["", ""];
  if (!rel) return "";
  return `/api/sessions/${encodeURIComponent(S.current.id)}/file?path=${encodeURIComponent(rel)}&token=${encodeURIComponent(S.token)}${scope}`;
}

function imageBody(path: string) {
  const url = fileURL(path);
  if (!url) return [];
  const img = el("img", "tc-image");
  img.src = url;
  img.alt = base(path);
  img.loading = "lazy";
  img.addEventListener("click", () => openLightbox(url, path));
  img.addEventListener("error", () => {
    const gone = el("div", "tc-note");
    setText(gone, "图片已不在原位置");
    img.replaceWith(gone);
  });
  return [img];
}

function collabRow(it: Item, completed: boolean): Row {
  const tool = str(it.tool);
  const sum = [str(it.model), oneLine(str(it.prompt)).slice(0, 160)].filter(Boolean).join(" · ");
  const meta: (HTMLElement | string)[] = [];
  const receivers = num(it.receivers);
  if (receivers && receivers > 1) {
    const r = el("span", "tc-meta");
    setText(r, "{p0} 个 Agent", { p0: String(receivers) });
    meta.push(r);
  }
  if (completed) {
    const b = statusBadge(str(it.status));
    if (b) meta.push(b);
  }
  const body = str(it.prompt) ? [textBlock(str(it.prompt), "tc-cmd mono")] : [];
  return { name: "Agent." + (tool || "call"), icon: "users", sum, meta, body };
}

function noteChip(icon: string, text: () => string, cls = "") {
  const c = el("div", "chip tc-note-chip" + (cls ? " " + cls : ""));
  const s = document.createElement("span");
  setTextRender(s, text);
  c.append(svgIcon(icon, 12), s);
  return c;
}

/* 只有一行、没有进行中状态的条目 */
function simpleNodes(it: Item): HTMLElement[] {
  switch (it.type) {
    case "image_view": {
      const path = str(it.path);
      const wrap = el("div", "tool-call");
      fillRow(wrap, { name: "ViewImage", icon: "file-image", sum: path, body: imageBody(path) }, false);
      return [wrap];
    }
    case "image_generation": {
      const saved = str(it.saved_path), failed = str(it.status) === "failed" || !!str(it.failure);
      const wrap = el("div", "tool-call");
      const meta = failed ? [badge(str(it.failure) === "usage_limit_exceeded" ? i18nText("✗ 额度已用完") : i18nText("✗ 失败"), "err")] : [];
      const body = [...(str(it.revised_prompt) ? [textBlock(str(it.revised_prompt), "tc-cmd mono")] : []), ...(saved ? imageBody(saved) : [])];
      fillRow(wrap, { name: "ImageGen", icon: "sparkles", sum: saved || oneLine(str(it.revised_prompt)).slice(0, 160), meta, body }, false);
      return [wrap];
    }
    case "context_compaction":
      return [noteChip("sparkles", () => i18nText("已压缩上下文"), "compact")];
    case "sub_agent_activity": {
      const kind = str(it.kind), who = str(it.agent_path) || str(it.model);
      const label: Record<string, () => string> = {
        started: () => i18nText("子 Agent 已启动"), interacted: () => i18nText("子 Agent 收到消息"),
        interrupted: () => i18nText("子 Agent 已中断"), completed: () => i18nText("子 Agent 已完成"),
      };
      return [noteChip("users", () => [label[kind]?.() || i18nText("子 Agent"), who].filter(Boolean).join(" · "))];
    }
    case "review_mode":
      return [noteChip("eye", () => it.entered ? i18nText("进入代码审查") : i18nText("代码审查结束"))];
    case "sleep": {
      const d = shortDuration(num(it.duration_ms));
      return [noteChip("clock", () => d ? i18nText("等待了 {p0}", { p0: d }) : i18nText("等待"))];
    }
  }
  return [];
}

function rowFor(it: Item, completed: boolean): Row[] {
  switch (it.type) {
    case "command_execution": return [commandRow(it, completed)];
    case "file_change": return changeRows(it);
    case "mcp_tool_call": case "dynamic_tool_call": return [toolRow(it, completed)];
    case "web_search": return [searchRow(it, completed)];
    case "collab_agent_tool_call": return [collabRow(it, completed)];
  }
  return [];
}

/** Codex 的工具条目 → 节点；不归这里管的条目（文字、思考、计划表…）返回 undefined。
 * 完成事件若对上了进行中的那一行，原地更新并返回空数组。 */
export function codexItemNodes(ev: AgentEvent): HTMLElement[] | undefined {
  const it = ev.item;
  if (!it || !TYPES.has(str(it.type))) return undefined;
  if (ev.type === "item.updated") return [];
  const completed = ev.type === "item.completed";
  if (!completed && ev.type !== "item.started") return [];
  if (completed && !rowFor(it, true).length) return simpleNodes(it);
  const key = itemKey(it);
  if (completed) {
    const open = key ? pending.get(key) : undefined;
    if (open) pending.delete(key);
    const rows = rowFor(it, true);
    if (open && rows.length === 1) {
      delete open.dataset.status;
      fillRow(open, rows[0], false);
      open.dataset.status = str(it.status);
      return [];
    }
    return rows.map(r => {
      const wrap = el("div", "tool-call");
      fillRow(wrap, r, false);
      wrap.dataset.status = str(it.status);
      return wrap;
    });
  }
  const rows = rowFor(it, false);
  if (rows.length !== 1) return [];
  const wrap = el("div", "tool-call");
  fillRow(wrap, rows[0], true);
  if (key) {
    const prev = pending.get(key); // 同一条目又开始了（上一次没收尾）：旧行停在未完成
    if (prev) settleRow(prev);
    pending.set(key, wrap);
    if (pending.size > PENDING_MAX) pending.delete(pending.keys().next().value!);
  }
  return [wrap];
}

/** 进行中条目对应的执行指示文案 */
export function codexWorkLabel(ev: AgentEvent): (() => string) | undefined {
  const it = ev.item;
  if (ev.type !== "item.started" || !it) return undefined;
  switch (it.type) {
    case "command_execution": return commandView(it).label;
    case "mcp_tool_call": case "dynamic_tool_call": {
      const name = [str(it.server) || str(it.namespace), str(it.tool)].filter(Boolean).join(".");
      return () => i18nText("运行工具 ") + name + "…";
    }
    case "web_search": return () => i18nText("搜索网页…");
    case "collab_agent_tool_call": return () => i18nText("调度子 Agent…");
  }
  return undefined;
}

/** 不落盘的实时增量：命令输出、MCP 进度。只更新进行中的那一行，返回是否认领 */
export function codexStreamEvent(e: { type?: string; item_id?: unknown; delta?: unknown; message?: unknown }) {
  if (e.type !== "item_output_delta" && e.type !== "item_progress") return false;
  const row = pending.get(str(e.item_id));
  const live = row?.querySelector<HTMLElement>(".tc-live");
  if (!row || !live) return true;
  const prev = e.type === "item_progress" ? "" : liveText.get(row) || "";
  const next = (prev + clean(e.type === "item_progress" ? str(e.message) : str(e.delta))).slice(-LIVE_KEEP);
  liveText.set(row, next);
  live.textContent = next.replace(/\n+$/, "").split("\n").slice(-LIVE_LINES).join("\n");
  return true;
}

/** 回合结束：还挂着「进行中」的行（中断、断线）停止转圈，收起实时输出 */
function settleRow(row: HTMLElement) {
  row.dataset.status = "unfinished";
  row.querySelector(".tc-live")?.remove();
  row.querySelector(".tc-spin")?.replaceWith(svgIcon(row.dataset.rowIcon || "tool", 12));
  for (const [k, v] of pending) if (v === row) pending.delete(k);
}

export function settleCodexItems(root: ParentNode, keep?: Element | null) {
  for (const row of root.querySelectorAll<HTMLElement>('.tool-call[data-status="running"]')) {
    if (!keep?.contains(row)) settleRow(row);
  }
}

/** 切线程、整体重载：对话流要重建，丢掉登记 */
export function resetCodexItems() {
  pending.clear();
}
