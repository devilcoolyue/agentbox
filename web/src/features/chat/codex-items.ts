/* Codex 的工具条目：命令、改动、MCP / 动态工具、网页搜索、看图、生图、子 Agent。
 * 外观与 Claude 的工具行一致（图标 + 工具名 + 摘要的细行），多出 CLI 才有的那些：
 * 命令按 CLI 的解析显示成 Read / Search / List / Shell，行尾是耗时与退出码，点开
 * 看完整命令和输出首尾；改动显示增删行数，点开看 diff。
 *
 * item.started 先画一行「进行中」（转圈 + 实时输出最后几行），同一 id 的
 * item.completed 到达时原地改成完成态，不另起一行；没有 id 的旧记录按命令串配对。
 * 字段来自 agent/appserver_items.go，exec --json 回退路径只有其中一部分，缺的就不显示。
 * 行结构、进行中登记与收尾在 tool-rows.ts，与 Claude 的工具行共用。 */
import type { AgentEvent } from "../../types.js";
import { svgIcon, type IconSlot } from "../../icons.js";
import { setText, setTextRender, t as i18nText } from "../../i18n.js";
import {
  badge, base, clean, clipLabel, clipOutput, diffBlock, diffStat, el, fillRow, imageBody, newRow, num, oneLine,
  outputBlocks, pendingRow, shortDuration, statusBadge, str, takeRow, textBlock, trackRow, type Row,
} from "./tool-rows.js";

type Item = NonNullable<AgentEvent["item"]>;
interface Action { type?: string; command?: string; name?: string; path?: string; query?: string }
interface Change { path?: string; kind?: string | { type?: string }; move_path?: string; added?: number; removed?: number; diff?: string; diff_truncated?: boolean }

const TYPES = new Set(["command_execution", "file_change", "mcp_tool_call", "dynamic_tool_call", "web_search",
  "image_view", "image_generation", "context_compaction", "collab_agent_tool_call", "sub_agent_activity",
  "review_mode", "sleep"]);

/* 实时输出缓冲（只留末尾），随行元素回收 */
const liveText = new WeakMap<HTMLElement, string>();
const LIVE_KEEP = 4096, LIVE_LINES = 4;

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

/* 命令的显示名与摘要：全是读/列/搜时照 CLI 的 Explored 归类，否则就是 Shell */
function commandView(it: Item): { name: string; icon: IconSlot; sum: string; label: () => string } {
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
      if (t === "read") return { name: "Read", icon: "chat.tool-read", sum, label: () => i18nText("读取 {p0}…", { p0: what }) };
      if (t === "search") return { name: "Search", icon: "chat.tool-search", sum, label: () => i18nText("搜索 {p0}…", { p0: what }) };
      return { name: "List", icon: "chat.tool-list", sum, label: () => i18nText("列出 {p0}…", { p0: what }) };
    }
    const verb = (a: Action) => a.type === "read" ? "Read" : a.type === "search" ? "Search" : "List";
    return { name: "Explore", icon: "chat.tool-search", sum: acts.map(a => verb(a) + " " + part(a)).join(" · "), label: () => i18nText("浏览代码…") };
  }
  const what = clipLabel(oneLine(cmd));
  return { name: "Shell", icon: "chat.tool-shell", sum: oneLine(cmd), label: () => what ? i18nText("执行 {p0}…", { p0: what }) : i18nText("执行命令…") };
}

/* 首尾各留 20 行：服务端已截好的直接用；exec 回退路径给的是全文，这里照同样口径截 */
function outputParts(it: Item) {
  const full = clean(str(it.aggregated_output)).replace(/\n+$/, "");
  if (it.output_tail !== undefined || it.output_truncated) return { head: full, tail: clean(str(it.output_tail)).replace(/\n+$/, ""), cut: true, total: num(it.output_lines) };
  return clipOutput(full);
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
    const outs = outputBlocks(outputParts(it));
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

function changeRows(it: Item): Row[] {
  const changes = (Array.isArray(it.changes) ? it.changes : []) as Change[];
  const status = str(it.status);
  return changes.map(c => {
    const kind = changeKind(c), path = str(c.path), move = str(c.move_path);
    const name = kind === "add" ? "Write" : kind === "delete" ? "Delete" : move ? "Move" : "Edit";
    const icon: IconSlot = kind === "add" ? "chat.tool-add" : kind === "delete" ? "chat.tool-delete" : move ? "chat.tool-move" : "chat.tool-edit";
    const meta: (HTMLElement | string)[] = [];
    const stat = diffStat(num(c.added), num(c.removed));
    if (stat) meta.push(stat);
    const b = statusBadge(status);
    if (b) meta.push(b);
    const htmlPath = changes.length === 1 && kind !== "delete" && /\.html?$/i.test(move || path) ? move || path : "";
    return { name, icon, sum: move ? path + " → " + move : path, meta, body: c.diff ? diffBlock(str(c.diff), kind, !!c.diff_truncated) : [], htmlPath };
  });
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
  return { name: name || (mcp ? "MCP" : "Tool"), icon: mcp ? "chat.tool-mcp" : "chat.tool-custom", sum: oneLine(args).slice(0, 160), meta, body };
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
  return { name, icon: "chat.tool-web", sum: sum || (completed ? "" : i18nText("搜索中…")), meta, body };
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
  return { name: "Agent." + (tool || "call"), icon: "chat.tool-agent", sum, meta, body };
}

function noteChip(icon: IconSlot, text: () => string, cls = "") {
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
      fillRow(wrap, { name: "ViewImage", icon: "chat.tool-image", sum: path, body: imageBody(path) }, false);
      return [wrap];
    }
    case "image_generation": {
      const saved = str(it.saved_path), failed = str(it.status) === "failed" || !!str(it.failure);
      const wrap = el("div", "tool-call");
      const meta = failed ? [badge(str(it.failure) === "usage_limit_exceeded" ? i18nText("✗ 额度已用完") : i18nText("✗ 失败"), "err")] : [];
      const body = [...(str(it.revised_prompt) ? [textBlock(str(it.revised_prompt), "tc-cmd mono")] : []), ...(saved ? imageBody(saved) : [])];
      fillRow(wrap, { name: "ImageGen", icon: "chat.tool-image-gen", sum: saved || oneLine(str(it.revised_prompt)).slice(0, 160), meta, body }, false);
      return [wrap];
    }
    case "context_compaction":
      return [noteChip("chat.compact", () => i18nText("已压缩上下文"), "compact")];
    case "sub_agent_activity": {
      const kind = str(it.kind), who = str(it.agent_path) || str(it.model);
      const label: Record<string, () => string> = {
        started: () => i18nText("子 Agent 已启动"), interacted: () => i18nText("子 Agent 收到消息"),
        interrupted: () => i18nText("子 Agent 已中断"), completed: () => i18nText("子 Agent 已完成"),
      };
      return [noteChip("chat.tool-agent", () => [label[kind]?.() || i18nText("子 Agent"), who].filter(Boolean).join(" · "))];
    }
    case "review_mode":
      return [noteChip("chat.review", () => it.entered ? i18nText("进入代码审查") : i18nText("代码审查结束"))];
    case "sleep": {
      const d = shortDuration(num(it.duration_ms));
      return [noteChip("chat.wait", () => d ? i18nText("等待了 {p0}", { p0: d }) : i18nText("等待"))];
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
    const open = takeRow(key);
    const rows = rowFor(it, true);
    if (open && rows.length === 1) {
      delete open.dataset.status;
      fillRow(open, rows[0], false);
      open.dataset.status = str(it.status);
      return [];
    }
    return rows.map(r => newRow(r, false, str(it.status)));
  }
  const rows = rowFor(it, false);
  if (rows.length !== 1) return [];
  const wrap = newRow(rows[0], true);
  if (key) trackRow(key, wrap);
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
  const row = pendingRow(str(e.item_id));
  const live = row?.querySelector<HTMLElement>(".tc-live");
  if (!row || !live) return true;
  const prev = e.type === "item_progress" ? "" : liveText.get(row) || "";
  const next = (prev + clean(e.type === "item_progress" ? str(e.message) : str(e.delta))).slice(-LIVE_KEEP);
  liveText.set(row, next);
  live.textContent = next.replace(/\n+$/, "").split("\n").slice(-LIVE_LINES).join("\n");
  return true;
}
