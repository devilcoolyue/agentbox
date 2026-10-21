/* Claude 的工具调用：照 Claude Code 终端的写法画——
 *
 *   ● Update(src/x.ts)                 工具名（CLI 的显示名）+ 括号里的主参数
 *     ⎿  Added 1 line, removed 1 line  结果摘要；Write/Edit 带行号的内容与 diff
 *
 * 连续的 Read / Grep / Glob 与只读、普通 Bash 命令像 CLI 一样折成一行
 * 「Read 3 files, ran 1 shell command」，进行中是「Running 1 shell command · 2s…」加一行
 * `⎿ $ 命令`；点开等于 CLI 的 ctrl+o，逐条列出 `● Bash(cmd)` 与完整输出。子 Agent 运行中
 * 列出它最近的工具调用，结束是「Done (3 tool uses · 15.7k tokens · 11s)」，点开看提示词、
 * 全部调用与回复。ToolSearch 与 CLI 一样不显示。
 *
 * tool_use（assistant 事件）先画进行中，user 事件里同一 id 的 tool_result 到达时原地更新；
 * 字段取自 tool_use 的 input 与 CLI 附在 user 事件上的 tool_use_result，旧记录缺哪样就
 * 退回 tool_result 的文字。进行中登记与回合结束的收尾在 tool-rows.ts。模型与工具输出一律
 * 走 textContent。 */
import type { AgentEvent, ContentBlock } from "../../types.js";
import { buttonLabel } from "../../icons.js";
import { setTip } from "../../tip.js";
import { setTextRender, t as i18nText } from "../../i18n.js";
import { clean, clipLabel, base, el, onSettle, pendingRow, str, num, takeRow, trackRow } from "./tool-rows.js";

type Input = Record<string, unknown>;
type Kind = "read" | "search" | "list" | "bash";
type Status = "running" | "completed" | "failed" | "unfinished";

/** omitted：历史页只带输出开头，服务端标出省略了几行（server/history_tools.go） */
interface Result { text: string; error: boolean; tur: Input | string | undefined; omitted: number }
interface Tool {
  id: string; name: string; input: Input;
  el: HTMLElement; status: Status; open: boolean;
  result?: Result;
  /** 实时到达时的开始时间，用来走秒；历史回放没有 */
  since?: number;
  group?: Group;
  parent?: Tool;
  /** 子 Agent：它自己的工具调用与 task_progress 的进度描述 */
  children?: Tool[];
  progress?: string;
}
interface Group { el: HTMLElement; tools: Tool[]; open: boolean; head: HTMLElement; hint: HTMLElement; items: HTMLElement }

const KEY = "cc:";
const tools = new WeakMap<HTMLElement, Tool>();
const AGENT_TOOLS = new Set(["Agent", "Task"]);
const HIDDEN = new Set(["ToolSearch"]);

/* 子 Agent 的回复是 Markdown；渲染函数由 chat-render 注入，免得两边互相 import */
let markdown: (text: string) => Node = text => document.createTextNode(text);
export function useMarkdown(fn: (text: string) => Node) { markdown = fn; }

const obj = (v: unknown) => v && typeof v === "object" && !Array.isArray(v) ? v as Input : undefined;
const plural = (n: number, one: () => string, many: () => string) => n === 1 ? one() : many();
const cap = (s: string) => s ? s[0].toUpperCase() + s.slice(1) : s;
const join = (parts: string[]) => parts.reduce((a, b) => a ? i18nText("{p0}，{p1}", { p0: a, p1: b }) : b, "");

/* 容器里的绝对路径按 CLI 的习惯显示成相对工作目录 */
export function relPath(p: unknown) {
  const s = str(p);
  if (s === "/workspace") return ".";
  return s.startsWith("/workspace/") ? s.slice(11) : s;
}

function secs(ms: number) {
  if (ms < 1000) return "<1s";
  const s = Math.round(ms / 1000);
  return s < 60 ? s + "s" : Math.floor(s / 60) + "m " + (s % 60) + "s";
}

function tokens(n: number) {
  return n < 1000 ? String(n) : (n / 1000).toFixed(1).replace(/\.0$/, "") + "k";
}

function bytes(n: number) {
  if (n < 1024) return n + " bytes";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + "KB";
  return (n / 1024 / 1024).toFixed(1) + "MB";
}

/* ---------------- 名称与参数：CLI 的 userFacingName ---------------- */

function displayName(t: Tool) {
  switch (t.name) {
    case "Edit": case "MultiEdit": return "Update";
    case "WebFetch": return "Fetch";
    case "WebSearch": return "Web Search";
    case "Grep": case "Glob": return "Search";
    case "NotebookEdit": return "Edit Notebook";
  }
  if (t.name.startsWith("mcp__")) {
    const rest = t.name.slice(5), i = rest.indexOf("__");
    return i > 0 ? `${rest.slice(0, i)} - ${rest.slice(i + 2)} (MCP)` : rest + " (MCP)";
  }
  return t.name;
}

function kv(input: Input, keys?: string[]) {
  const out: string[] = [];
  for (const [k, v] of Object.entries(input)) {
    if (keys && !keys.includes(k)) continue;
    if (v === undefined || v === null || v === "") continue;
    out.push(`${k}: ${JSON.stringify(k === "path" ? relPath(v) : v)}`);
  }
  const s = out.join(", ");
  return s.length > 240 ? s.slice(0, 239) + "…" : s;
}

function headArgs(t: Tool) {
  const i = t.input;
  switch (t.name) {
    case "Bash": return str(i.command);
    case "Read": case "Write": case "Edit": case "MultiEdit": return relPath(i.file_path);
    case "NotebookEdit": return relPath(i.notebook_path);
    case "WebFetch": return str(i.url);
    case "WebSearch": return `"${str(i.query)}"`;
    case "Agent": case "Task": return str(i.description);
    case "Grep": return kv(i, ["pattern", "path", "glob", "type"]);
    case "Glob": return kv(i, ["pattern", "path"]);
    case "Skill": return str(i.skill) || str(i.command);
  }
  return kv(i);
}

/* ---------------- 折叠组：CLI 把只读/搜索与普通命令收成一行 ---------------- */

const READ_CMDS = new Set(["cat", "head", "tail", "less", "more", "bat", "wc", "nl", "stat", "file", "sed", "jq", "awk", "cut", "sort", "uniq"]);
const LIST_CMDS = new Set(["ls", "tree", "find", "fd", "du", "pwd"]);
const SEARCH_CMDS = new Set(["grep", "rg", "ag", "ack", "egrep", "fgrep"]);
const NEUTRAL_CMDS = new Set(["echo", "printf", "true", "cd"]);

/* 命令里每一段的程序名：全是读就算 Read，全是列目录就算 List，含搜索的只读组合算 Search */
function bashKind(command: string): Kind {
  const kinds = new Set<Kind>();
  for (const seg of command.split(/&&|\|\||[;|\n]/)) {
    const words = seg.trim().split(/\s+/).filter(w => !/^[A-Za-z_][A-Za-z0-9_]*=/.test(w));
    const cmd = base(words[0] || "");
    if (!cmd || NEUTRAL_CMDS.has(cmd)) continue;
    if (cmd === "sed" && words.some(w => /^-[a-zA-Z]*i/.test(w))) return "bash";
    if (cmd === "find" && words.some(w => w === "-delete" || w === "-exec")) return "bash";
    if (READ_CMDS.has(cmd)) kinds.add("read");
    else if (LIST_CMDS.has(cmd)) kinds.add("list");
    else if (SEARCH_CMDS.has(cmd)) kinds.add("search");
    else return "bash";
  }
  if (kinds.has("search")) return "search";
  if (kinds.has("read")) return "read";
  if (kinds.has("list")) return "list";
  return "bash";
}

function groupKind(t: Tool): Kind | "" {
  if (t.name === "Read") return "read";
  if (t.name === "Grep" || t.name === "Glob") return "search";
  if (t.name === "Bash" && t.input.run_in_background !== true) return bashKind(str(t.input.command));
  return "";
}

const PHRASES: Record<Kind, (n: number, running: boolean) => string> = {
  read: (n, running) => running ? plural(n, () => i18nText("正在读取 1 个文件"), () => i18nText("正在读取 {p0} 个文件", { p0: String(n) }))
    : plural(n, () => i18nText("读取 1 个文件"), () => i18nText("读取 {p0} 个文件", { p0: String(n) })),
  search: (n, running) => running ? plural(n, () => i18nText("正在搜索 1 个模式"), () => i18nText("正在搜索 {p0} 个模式", { p0: String(n) }))
    : plural(n, () => i18nText("搜索 1 个模式"), () => i18nText("搜索 {p0} 个模式", { p0: String(n) })),
  list: (n, running) => running ? plural(n, () => i18nText("正在列出 1 个目录"), () => i18nText("正在列出 {p0} 个目录", { p0: String(n) }))
    : plural(n, () => i18nText("列出 1 个目录"), () => i18nText("列出 {p0} 个目录", { p0: String(n) })),
  bash: (n, running) => running ? plural(n, () => i18nText("正在运行 1 条命令"), () => i18nText("正在运行 {p0} 条命令", { p0: String(n) }))
    : plural(n, () => i18nText("运行 1 条命令"), () => i18nText("运行 {p0} 条命令", { p0: String(n) })),
};

/* 当前可以续上的组：之后没有别的内容插进来（思考块除外，CLI 普通视图里不显示思考） */
let openGroup: Group | null = null;

/** 文字、别的工具、提示行……任何正文内容都结束当前的折叠组 */
export function breakClaudeGroup() { openGroup = null; }

function groupContinues(g: Group | null): g is Group {
  if (!g) return false;
  for (let n = g.el.nextElementSibling; n; n = n.nextElementSibling) {
    if (n.classList.contains("think") || n.classList.contains("answer-footer")) continue;
    return false;
  }
  return true;
}

function newGroup(): Group {
  const wrap = el("div", "tool-call cc-group");
  const head = el("div", "cc-gsum");
  const hint = el("div", "cc-res cc-hint");
  const items = el("div", "cc-gitems");
  const g: Group = { el: wrap, tools: [], open: false, head, hint, items };
  head.tabIndex = 0;
  head.setAttribute("role", "button");
  const toggle = () => { g.open = !g.open; renderGroup(g); };
  head.addEventListener("click", toggle);
  head.addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); } });
  wrap.append(head, hint, items);
  onSettle(wrap, () => {
    for (const t of g.tools) if (t.status === "running") settleTool(t);
    renderGroup(g);
  });
  return g;
}

function runningSince(g: Group) {
  let since = 0;
  for (const t of g.tools) if (t.status === "running" && t.since) since = since ? Math.min(since, t.since) : t.since;
  return since;
}

function renderGroup(g: Group) {
  const running = g.tools.some(t => t.status === "running");
  const counts = new Map<Kind, number>();
  for (const t of g.tools) {
    const k = groupKind(t) || "bash";
    counts.set(k, (counts.get(k) || 0) + 1);
  }
  const failed = g.tools.filter(t => t.status === "failed").length;
  const since = running ? runningSince(g) : 0;
  g.el.dataset.status = running ? "running" : g.tools.some(t => t.status === "unfinished") ? "unfinished" : failed ? "failed" : "completed";
  g.el.classList.toggle("cc-open", g.open);
  g.head.setAttribute("aria-expanded", String(g.open));
  const dot = el("span", "cc-dot", "●");
  const text = el("span", "cc-gtext");
  setTextRender(text, () => cap(join([...counts].map(([k, n]) => PHRASES[k](n, running))))
    + (since && Date.now() - since >= 1000 ? " · " + secs(Date.now() - since) : "") + (running ? "…" : ""));
  g.head.replaceChildren(dot, text);
  if (failed && !running) {
    const err = el("span", "cc-gerr");
    setTextRender(err, () => " · " + plural(failed, () => i18nText("1 处出错"), () => i18nText("{p0} 处出错", { p0: String(failed) })));
    g.head.append(err);
  }
  // 进行中：CLI 在下面跟一行正在做的那一条
  const cur = [...g.tools].reverse().find(t => t.status === "running");
  if (cur) {
    const what = cur.name === "Bash" ? "$ " + str(cur.input.command).split("\n")[0]
      : cur.name === "Read" ? relPath(cur.input.file_path) : str(cur.input.pattern);
    g.hint.replaceChildren(el("span", "cc-elbow", "⎿"), el("span", "cc-line", what + (cur.since && Date.now() - cur.since >= 1000 ? ` (${secs(Date.now() - cur.since)})` : "")));
    g.hint.hidden = false;
  } else g.hint.hidden = true;
  if (g.items.childElementCount !== g.tools.length) g.items.replaceChildren(...g.tools.map(t => t.el));
}

/* ---------------- 走秒：进行中的折叠组每秒刷新一次 ---------------- */

const ticking = new Set<Group>();
let timer = 0;
function tick(g: Group) {
  ticking.add(g);
  if (timer) return;
  timer = window.setInterval(() => {
    for (const x of ticking) {
      if (!x.el.isConnected || !x.tools.some(t => t.status === "running")) ticking.delete(x);
      else renderGroup(x);
    }
    if (!ticking.size) { clearInterval(timer); timer = 0; }
  }, 1000);
}

/* ---------------- 结果 ---------------- */

function resultOf(block: ContentBlock, tur: unknown): Result {
  const c = (block as { content?: unknown }).content;
  let text = typeof c === "string" ? c
    : Array.isArray(c) ? c.map(b => b && typeof b === "object" && (b as { type?: string }).type === "text" ? str((b as { text?: unknown }).text) : "").filter(Boolean).join("\n") : "";
  text = clean(text).replace(/^<tool_use_error>([\s\S]*)<\/tool_use_error>$/, "$1");
  return { text, error: (block as { is_error?: unknown }).is_error === true, tur: typeof tur === "string" ? tur : obj(tur), omitted: num((block as { omitted_lines?: unknown }).omitted_lines) ?? 0 };
}

const INTERRUPTED = "[Request interrupted by user for tool use]";
const REJECTED = "The user doesn't want to proceed with this tool use";

/* ---- 结果区的构件：⎿ 开头，后面是摘要行或成段的输出 ---- */

function res(...parts: Node[]) {
  const box = el("div", "cc-res");
  const out = el("div", "cc-out");
  out.append(...parts);
  box.append(el("span", "cc-elbow", "⎿"), out);
  return box;
}

function line(text: () => string, cls = "") {
  const d = el("div", "cc-line" + (cls ? " " + cls : ""));
  setTextRender(d, text);
  return d;
}

function more(t: Tool, n: number) {
  const b = el("button", "cc-more");
  b.type = "button";
  setTextRender(b, () => plural(n, () => i18nText("… 还有 1 行"), () => i18nText("… 还有 {p0} 行", { p0: String(n) })));
  b.addEventListener("click", e => { e.stopPropagation(); t.open = true; renderTool(t); });
  return b;
}

/* 一段输出：普通视图只留前几行，展开（或折叠组 / 子 Agent 里的详细视图）给全文，再长也封顶 */
const FULL_CAP = 400;
function textLines(t: Tool, text: string, limit: number, cls = "", omitted = 0) {
  const all = text.replace(/\n+$/, "").split("\n");
  const cap = Math.min(all.length, limit);
  const pre = el("pre", "cc-pre mono" + (cls ? " " + cls : ""), all.slice(0, cap).join("\n"));
  const out: Node[] = [pre];
  const rest = all.length - cap + omitted;
  // 还有没画出来的行就给「展开」；剩下的只是历史页没带的，照实说一句
  if (rest > 0) out.push(cap < all.length && limit < FULL_CAP ? more(t, rest)
    : line(() => plural(rest, () => i18nText("… 还有 1 行"), () => i18nText("… 还有 {p0} 行", { p0: String(rest) })), "cc-dim"));
  return out;
}

function errorLines(t: Tool, text: string, limit: number, omitted = 0) {
  const s = text.trim() || i18nText("回合失败");
  return textLines(t, /^error/i.test(s) ? s : "Error: " + s, limit, "err", omitted);
}

/* 带行号的内容（Write）；行号列与 CLI 一样右对齐 */
function numbered(t: Tool, content: string, limit: number) {
  const all = content.replace(/\n$/, "").split("\n");
  const n = Math.min(all.length, limit);
  const w = String(n).length;
  const pre = el("pre", "cc-pre cc-num mono");
  for (let i = 0; i < n; i++) {
    pre.append(el("span", "ln", String(i + 1).padStart(w)), el("span", "dl", " " + all[i]), "\n");
  }
  const out: Node[] = [pre];
  if (all.length > n) out.push(more(t, all.length - n));
  return out;
}

interface Hunk { oldStart?: number; newStart?: number; lines?: unknown }

/* structuredPatch → 带行号的 diff；删除行标旧行号，新增与上下文行标新行号，与 CLI 一致 */
function patchLines(t: Tool, hunks: Hunk[], limit: number) {
  const rows: [string, string, string][] = [];
  hunks.forEach((h, i) => {
    if (i) rows.push(["", "", "..."]);
    let o = num(h.oldStart) ?? 1, n = num(h.newStart) ?? 1;
    for (const l of Array.isArray(h.lines) ? h.lines as unknown[] : []) {
      const s = str(l);
      if (s.startsWith("-")) rows.push([String(o++), "del", s]);
      else if (s.startsWith("+")) rows.push([String(n++), "add", s]);
      else { rows.push([String(n), "", s]); o++; n++; }
    }
  });
  const cap = Math.min(rows.length, limit);
  const w = Math.max(...rows.slice(0, cap).map(r => r[0].length), 1);
  const pre = el("pre", "cc-pre cc-num cc-diff mono");
  for (const [no, cls, s] of rows.slice(0, cap)) {
    pre.append(el("span", "ln", no.padStart(w)), el("span", "dl" + (cls ? " " + cls : ""), " " + s), "\n");
  }
  const out: Node[] = [pre];
  if (rows.length > cap) out.push(more(t, rows.length - cap));
  return out;
}

function patchStat(hunks: Hunk[]) {
  let added = 0, removed = 0;
  for (const h of hunks) for (const l of Array.isArray(h.lines) ? h.lines as unknown[] : []) {
    if (str(l).startsWith("+")) added++;
    else if (str(l).startsWith("-")) removed++;
  }
  return { added, removed };
}

function addedRemoved(added: number, removed: number) {
  const parts: string[] = [];
  if (added) parts.push(plural(added, () => i18nText("新增 1 行"), () => i18nText("新增 {p0} 行", { p0: String(added) })));
  if (removed) parts.push(plural(removed, () => i18nText("删除 1 行"), () => i18nText("删除 {p0} 行", { p0: String(removed) })));
  return cap(join(parts));
}

/* 旧记录没有 structuredPatch：用 old_string / new_string 拼出 -/+ */
function inputPatch(t: Tool): Hunk[] {
  const edits = Array.isArray(t.input.edits) ? t.input.edits as Input[] : [t.input];
  return edits.filter(e => typeof e.old_string === "string" || typeof e.new_string === "string").map(e => ({
    lines: [...str(e.old_string).split("\n").map(l => "-" + l), ...str(e.new_string).split("\n").map(l => "+" + l)],
  }));
}

/* ---------------- 各工具的结果：compact 是普通视图，full 是 ctrl+o 详细视图 ---------------- */

interface View { compact: Node[]; full?: Node[] }

/* 实时事件带完整 stdout/stderr；历史页只有截过的 tool_result 正文（两者内容相同） */
function bashOutput(r: Result) {
  const tur = obj(r.tur);
  if (tur && typeof tur.stdout === "string") return [str(tur.stdout), str(tur.stderr)].filter(Boolean).join("\n");
  return r.text;
}

function agentText(r: Result) {
  const tur = obj(r.tur);
  const content = tur && Array.isArray(tur.content) ? tur.content as unknown[] : [];
  const text = content.map(b => obj(b)?.type === "text" ? str(obj(b)!.text) : "").filter(Boolean).join("\n\n");
  return text || r.text.replace(/^\[Subagent hand-back\][^\n]*\n+/, "");
}

function childLine(c: Tool) {
  const d = el("div", "cc-line cc-child");
  d.dataset.status = c.status;
  d.append(el("b", "", displayName(c)), el("span", "cc-args", headArgs(c) ? `(${clipLabel(headArgs(c).split("\n")[0])})` : ""));
  return d;
}

function agentView(t: Tool): View {
  const r = t.result, tur = obj(r?.tur);
  const kids = t.children || [];
  const full: Node[] = [];
  const prompt = str(t.input.prompt);
  if (prompt) full.push(res(line(() => i18nText("提示词："), "cc-label"), el("pre", "cc-pre cc-wrap", prompt)));
  for (const c of kids) { renderTool(c, true); full.push(c.el); }
  let compact: Node[];
  if (!r) {
    // 运行中：最近三条子调用，更早的折成「还有 N 次」
    const shown = kids.slice(-3).map(childLine);
    const rest = kids.length - shown.length;
    if (rest > 0) shown.push(line(() => plural(rest, () => i18nText("还有 1 次工具调用"), () => i18nText("还有 {p0} 次工具调用", { p0: String(rest) }))));
    if (!shown.length && t.progress) shown.push(line(() => t.progress!, "cc-dim"));
    compact = shown.length ? [res(...shown)] : [];
  } else if (r.error) {
    compact = [res(...errorLines(t, r.text, 3))];
  } else if (t.input.run_in_background === true || tur?.status === "async_launched") {
    compact = [res(line(() => i18nText("已转入后台")))];
  } else {
    const uses = num(tur?.totalToolUseCount) ?? kids.length, toks = num(tur?.totalTokens), ms = num(tur?.totalDurationMs);
    compact = [res(line(() => i18nText("完成（{p0}）", { p0: [
      plural(uses, () => i18nText("1 次工具调用"), () => i18nText("{p0} 次工具调用", { p0: String(uses) })),
      toks !== undefined ? tokens(toks) + " tokens" : "", ms !== undefined ? secs(ms) : "",
    ].filter(Boolean).join(" · ") })))];
    const reply = agentText(r);
    if (reply) {
      const md = el("div", "msg agent cc-reply");
      md.append(markdown(reply));
      full.push(res(line(() => i18nText("回复："), "cc-label"), md));
    }
  }
  if (r) full.push(...compact);
  return { compact, full };
}

function viewOf(t: Tool, verbose: boolean): View {
  if (AGENT_TOOLS.has(t.name)) return agentView(t);
  const r = t.result;
  const lim = (n: number) => verbose ? FULL_CAP : n;
  if (t.name === "Bash" && t.input.run_in_background === true && (!r || !r.error)) return { compact: [res(line(() => i18nText("在后台运行")))] };
  if (!r) return { compact: [] };
  if (r.text.includes(INTERRUPTED)) return { compact: [res(line(() => i18nText("已被用户中断"), "err"))] };
  if (r.text.startsWith(REJECTED)) return { compact: [res(line(() => i18nText("已拒绝"), "warn"))] };
  const tur = obj(r.tur);
  if (r.error) return { compact: [res(...errorLines(t, r.text, lim(3), r.omitted))], full: [res(...errorLines(t, r.text, FULL_CAP, r.omitted))] };
  switch (t.name) {
    case "Bash": {
      const out = bashOutput(r), omitted = out === r.text ? r.omitted : 0;
      if (!out.trim()) return { compact: [res(line(() => i18nText("（无输出）"), "cc-dim"))] };
      return { compact: [res(...textLines(t, out, lim(3), "", omitted))], full: [res(...textLines(t, out, FULL_CAP, "", omitted))] };
    }
    case "Read": {
      const file = obj(tur?.file);
      if (tur?.type === "image") return { compact: [res(line(() => i18nText("读取图片")))] };
      const n = num(file?.numLines) ?? (r.text ? r.text.replace(/\n$/, "").split("\n").length : 0);
      return { compact: [res(line(() => plural(n, () => i18nText("读取 1 行"), () => i18nText("读取 {p0} 行", { p0: String(n) }))))] };
    }
    case "Write": {
      const patch = Array.isArray(tur?.structuredPatch) ? tur!.structuredPatch as Hunk[] : [];
      if (tur?.type === "update" && patch.length) {
        const { added, removed } = patchStat(patch);
        return { compact: [res(line(() => addedRemoved(added, removed)), ...patchLines(t, patch, lim(40)))], full: [res(line(() => addedRemoved(added, removed)), ...patchLines(t, patch, FULL_CAP))] };
      }
      const content = str(t.input.content), n = content ? content.replace(/\n$/, "").split("\n").length : 0, path = relPath(t.input.file_path);
      const head = () => plural(n, () => i18nText("已写入 1 行到 {p0}", { p0: path }), () => i18nText("已写入 {p0} 行到 {p1}", { p0: String(n), p1: path }));
      return { compact: [res(line(head), ...numbered(t, content, lim(10)))], full: [res(line(head), ...numbered(t, content, FULL_CAP))] };
    }
    case "Edit": case "MultiEdit": {
      const patch = Array.isArray(tur?.structuredPatch) && (tur!.structuredPatch as Hunk[]).length ? tur!.structuredPatch as Hunk[] : inputPatch(t);
      const { added, removed } = patchStat(patch);
      return { compact: [res(line(() => addedRemoved(added, removed)), ...patchLines(t, patch, lim(40)))], full: [res(line(() => addedRemoved(added, removed)), ...patchLines(t, patch, FULL_CAP))] };
    }
    case "Grep": case "Glob": {
      const files = Array.isArray(tur?.filenames) ? (tur!.filenames as unknown[]).map(relPath) : [];
      const content = str(tur?.content);
      const lines = tur?.mode === "content" ? num(tur?.numLines) : undefined;
      const n = num(tur?.numFiles) ?? files.length;
      const sum = () => lines !== undefined ? plural(lines, () => i18nText("找到 1 行"), () => i18nText("找到 {p0} 行", { p0: String(lines) }))
        : plural(n, () => i18nText("找到 1 个文件"), () => i18nText("找到 {p0} 个文件", { p0: String(n) }));
      const body = content || files.join("\n") || r.text;
      return { compact: [res(line(sum))], full: [res(line(sum), ...(body ? textLines(t, body, FULL_CAP) : []))] };
    }
    case "WebFetch": {
      const size = num(tur?.bytes), code = num(tur?.code), codeText = str(tur?.codeText);
      const head = size !== undefined ? () => i18nText("收到 {p0}（{p1}）", { p0: bytes(size), p1: [code, codeText].filter(v => v !== undefined && v !== "").join(" ") }) : undefined;
      const body = str(tur?.result) || r.text;
      if (!head) return { compact: [res(...textLines(t, body, lim(3)))], full: [res(...textLines(t, body, FULL_CAP))] };
      return { compact: [res(line(head))], full: [res(line(head), el("pre", "cc-pre cc-wrap", body))] };
    }
    case "WebSearch": {
      const count = num(tur?.searchCount) ?? 1, dur = num(tur?.durationSeconds);
      const head = () => dur !== undefined
        ? plural(count, () => i18nText("搜索 1 次，用时 {p0}", { p0: secs(dur * 1000) }), () => i18nText("搜索 {p0} 次，用时 {p1}", { p0: String(count), p1: secs(dur * 1000) }))
        : plural(count, () => i18nText("搜索 1 次"), () => i18nText("搜索 {p0} 次", { p0: String(count) }));
      const links = el("ul", "cc-links");
      for (const item of Array.isArray(tur?.results) ? tur!.results as unknown[] : []) {
        for (const hit of Array.isArray(obj(item)?.content) ? obj(item)!.content as unknown[] : []) {
          const url = str(obj(hit)?.url), title = str(obj(hit)?.title) || url;
          const li = document.createElement("li");
          if (/^https?:\/\//i.test(url)) {
            const a = el("a", "", title);
            a.href = url;
            a.target = "_blank";
            a.rel = "noopener noreferrer";
            li.append(a);
          } else li.append(el("span", "", title));
          links.append(li);
        }
      }
      return { compact: [res(line(head))], full: [res(line(head), ...(links.childElementCount ? [links] : []))] };
    }
  }
  if (!r.text.trim()) return { compact: [res(line(() => i18nText("（无输出）"), "cc-dim"))] };
  return { compact: [res(...textLines(t, r.text, lim(3), "", r.omitted))], full: [res(...textLines(t, r.text, FULL_CAP, "", r.omitted))] };
}

/* ---------------- 一条工具调用 ---------------- */

function htmlPath(t: Tool) {
  if (!["Write", "Edit", "MultiEdit"].includes(t.name)) return "";
  const p = str(t.input.file_path);
  return /\.html?$/i.test(p) ? p : "";
}

/** verbose：折叠组与子 Agent 里的详细视图，全文展开、标题不再可点 */
function renderTool(t: Tool, verbose = !!t.group || !!t.parent) {
  const view = viewOf(t, verbose);
  const expandable = !verbose && !!view.full;
  const head = el("div", "cc-head");
  head.append(el("span", "cc-dot", "●"), el("b", "cc-name", displayName(t)));
  const args = headArgs(t);
  if (args) {
    const a = el("span", "cc-args", "(" + (verbose || t.open ? args : args.split("\n")[0] + (args.includes("\n") ? " …" : "")) + ")");
    head.append(a);
  }
  setTip(head, args || displayName(t));
  const html = htmlPath(t);
  if (html) {
    const b = el("button", "btn chip-open");
    b.type = "button";
    b.dataset.htmlPreview = html;
    buttonLabel(b, () => i18nText("预览"), "eye");
    setTip(b, () => i18nText("在预览窗口渲染 ") + html);
    b.addEventListener("click", e => e.stopPropagation());
    head.append(b);
  }
  if (expandable) {
    head.classList.add("cc-toggle");
    head.tabIndex = 0;
    head.setAttribute("role", "button");
    head.setAttribute("aria-expanded", String(t.open));
    const toggle = () => { t.open = !t.open; renderTool(t); };
    head.addEventListener("click", toggle);
    head.addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); } });
  }
  t.el.dataset.status = t.status;
  t.el.dataset.tool = t.name;
  t.el.replaceChildren(head, ...(verbose || t.open ? view.full ?? view.compact : view.compact));
}

function settleTool(t: Tool) {
  t.status = "unfinished";
  t.el.dataset.status = "unfinished";
  for (const c of t.children || []) if (c.status === "running") settleTool(c);
  takeRow(KEY + t.id);
}

/* 子调用变了重画它所在的子 Agent 那一行 */
function refresh(t: Tool): void {
  if (t.parent) return refresh(t.parent);
  renderTool(t);
  if (t.group) renderGroup(t.group);
}

/** assistant 事件里的一个 tool_use → 要放进对话流的节点；续进折叠组、子 Agent 里的调用
 * 与不显示的工具返回 null。不归这里管（任务、提问、委派等）的由调用方先挑走。 */
export function claudeToolUse(block: ContentBlock, ev: AgentEvent, live = false): HTMLElement | null {
  const id = str(block.id), name = str(block.name);
  if (!name || HIDDEN.has(name)) return null;
  const t: Tool = { id, name, input: obj(block.input) || {}, el: el("div", "tool-call cc-tool"), status: "running", open: false, since: live ? Date.now() : undefined };
  tools.set(t.el, t);
  if (AGENT_TOOLS.has(name)) t.children = [];
  if (id) trackRow(KEY + id, t.el);
  onSettle(t.el, () => { settleTool(t); refresh(t); });
  // 子 Agent 自己的调用：收进它那一行
  const parentID = str(ev.parent_tool_use_id);
  const parent = parentID ? tools.get(pendingRow(KEY + parentID) as HTMLElement) : undefined;
  if (parent?.children) {
    t.parent = parent;
    parent.children.push(t);
    renderTool(t, true);
    refresh(t);
    return null;
  }
  if (groupKind(t)) {
    if (!groupContinues(openGroup)) openGroup = null;
    const g = openGroup || newGroup();
    t.group = g;
    g.tools.push(t);
    renderTool(t, true);
    renderGroup(g);
    if (live) tick(g);
    if (openGroup === g) return null;
    openGroup = g;
    return g.el;
  }
  openGroup = null;
  renderTool(t);
  return t.el;
}

/** user 事件：tool_result 原地更新对应的那一行；找不到（换页、旧记录）就不显示 */
export function claudeToolResults(ev: AgentEvent) {
  const content = ev.message?.content;
  if (!Array.isArray(content)) return;
  const results = content.filter(b => b.type === "tool_result");
  for (const block of results) {
    const row = takeRow(KEY + str((block as { tool_use_id?: unknown }).tool_use_id));
    const t = row && tools.get(row);
    if (!t) continue;
    t.result = resultOf(block, results.length === 1 ? ev.tool_use_result : undefined);
    t.status = t.result.error ? "failed" : "completed";
    refresh(t);
  }
}

/** 子 Agent 的事件能否收进它的工具行（那一行还在，且是 Agent 调用） */
export function subAgentKnown(ev: AgentEvent) {
  const row = pendingRow(KEY + str(ev.parent_tool_use_id));
  return !!(row && tools.get(row)?.children);
}

/** system task_progress：子 Agent 还没有调用工具时，显示它正在做什么 */
export function claudeTaskProgress(ev: AgentEvent) {
  const row = pendingRow(KEY + str(ev.tool_use_id));
  const t = row && tools.get(row);
  if (!t?.children || t.result) return;
  t.progress = str(ev.description);
  refresh(t);
}

/** 执行指示：正在调用的工具 */
export function claudeWorkLabel(block: ContentBlock): () => string {
  const i = obj(block.input) || {}, name = str(block.name);
  const what = (s: string) => clipLabel(s.split("\n")[0]);
  switch (name) {
    case "Bash": {
      const cmd = what(str(i.command));
      const kind = bashKind(str(i.command));
      if (kind === "read") return () => i18nText("读取 {p0}…", { p0: cmd });
      if (kind === "search") return () => i18nText("搜索 {p0}…", { p0: cmd });
      if (kind === "list") return () => i18nText("列出 {p0}…", { p0: cmd });
      return () => i18nText("执行 {p0}…", { p0: cmd });
    }
    case "Read": return () => i18nText("读取 {p0}…", { p0: what(relPath(i.file_path)) });
    case "Write": return () => i18nText("写入 {p0}…", { p0: what(relPath(i.file_path)) });
    case "Edit": case "MultiEdit": case "NotebookEdit": return () => i18nText("修改 {p0}…", { p0: what(relPath(i.file_path || i.notebook_path)) });
    case "Grep": case "Glob": return () => i18nText("搜索 {p0}…", { p0: what(str(i.pattern)) });
    case "WebSearch": return () => i18nText("搜索网页…");
    case "WebFetch": return () => i18nText("读取网页 {p0}…", { p0: what(str(i.url).replace(/^https?:\/\//, "")) });
    case "Agent": case "Task": return () => i18nText("调度子 Agent…");
  }
  return () => i18nText("运行工具 ") + name + "…";
}
