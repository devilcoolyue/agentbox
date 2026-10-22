/* 工具行：Codex 条目（codex-items.ts）与 Claude 工具调用（claude-tools.ts）共用的一行结构——
 * 图标 + 工具名 + 摘要 + 行尾信息（耗时、徽标、增删行数），有详情时整行是 <summary>，
 * 点开看命令、输出首尾或 diff。进行中的行转圈，并按 key 登记，完成事件到达时原地更新；
 * 回合结束仍挂着的行由 settleToolRows 停止转圈。模型与工具输出一律走 textContent。 */
import { buttonLabel, svgIcon, type IconSlot } from "../../icons.js";
import { setTip } from "../../tip.js";
import { setText, t as i18nText } from "../../i18n.js";
import { S } from "../../state.js";
import { openLightbox } from "../../util.js";

export const str = (v: unknown) => typeof v === "string" ? v : "";
export const num = (v: unknown) => typeof v === "number" && Number.isFinite(v) ? v : undefined;
/* 执行指示里只放得下一小段 */
export const clipLabel = (s: string) => s.length > 48 ? s.slice(0, 47) + "…" : s;
export const base = (p: string) => p.replace(/\/+$/, "").split("/").pop() || p;
export const oneLine = (s: string) => s.replace(/\s*\n\s*/g, " ⏎ ").trim();

// eslint-disable-next-line no-control-regex
const ANSI = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]/g;
export const clean = (s: string) => s.replace(ANSI, "").replace(/\r(?!\n)/g, "\n");

export function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls: string, text?: string) {
  const e = document.createElement(tag);
  e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function shortDuration(ms: number | undefined) {
  if (ms === undefined || ms < 100) return ""; // 瞬间完成的不标，耗时长的才值得看
  if (ms < 1000) return Math.round(ms) + "ms";
  if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
  const s = Math.round(ms / 1000);
  return Math.floor(s / 60) + "m " + (s % 60) + "s";
}

export interface Row { name: string; icon: IconSlot; sum: string; meta?: (HTMLElement | string)[]; body?: HTMLElement[]; tip?: string; htmlPath?: string }

export function fillRow(wrap: HTMLElement, row: Row, running: boolean) {
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
    buttonLabel(b, () => i18nText("预览"), "chat.preview");
    setTip(b, () => i18nText("在预览窗口渲染 ") + row.htmlPath);
    // 按钮在 <summary> 里：只打开预览（chat-log 上的委托处理），别顺带展开这一行
    b.addEventListener("click", e => e.preventDefault());
    line.append(b);
  }
  setTip(line, row.tip || row.sum || row.name);
  wrap.dataset.rowIcon = row.icon;
  if (running) wrap.dataset.status = "running";
  // 进行中就点开看过的行，完成时保持展开
  const wasOpen = !!wrap.querySelector(":scope > details[open]");
  if (row.body?.length) {
    const det = document.createElement("details");
    det.open = wasOpen;
    const body = el("div", "tc-body");
    body.append(...row.body);
    det.append(line, body);
    wrap.replaceChildren(det);
  } else wrap.replaceChildren(line);
  if (running) wrap.append(el("pre", "tc-live mono"));
}

export function newRow(row: Row, running: boolean, status = "") {
  const wrap = el("div", "tool-call");
  fillRow(wrap, row, running);
  if (status) wrap.dataset.status = status;
  return wrap;
}

export function badge(text: string, tone: "err" | "ok" | "warn" | "") {
  return el("span", "tc-badge" + (tone ? " " + tone : ""), text);
}

export function statusBadge(status: string, exit?: number) {
  if (status === "failed") return badge(exit !== undefined ? i18nText("✗ 退出码 {p0}", { p0: String(exit) }) : i18nText("✗ 失败"), "err");
  if (status === "declined") return badge(i18nText("已拒绝"), "warn");
  if (status === "interrupted") return badge(i18nText("已中断"), "warn");
  if (exit !== undefined && exit !== 0) return badge(i18nText("✗ 退出码 {p0}", { p0: String(exit) }), "err");
  return null;
}

export function textBlock(text: string, cls = "tc-out mono") {
  return el("pre", cls, text);
}

/* 输出首尾各留 20 行，中间标省略了几行 */
const SIDE_LINES = 20, SIDE_CHARS = 3000;
export interface OutputParts { head: string; tail: string; cut: boolean; total?: number }
export function clipOutput(full: string): OutputParts {
  const lines = full.split("\n");
  if (lines.length <= 2 * SIDE_LINES && full.length <= 2 * SIDE_CHARS) return { head: full, tail: "", cut: false, total: lines.length };
  const tailN = Math.min(SIDE_LINES, Math.max(0, lines.length - SIDE_LINES));
  return { head: lines.slice(0, SIDE_LINES).join("\n").slice(0, SIDE_CHARS), tail: tailN ? lines.slice(-tailN).join("\n").slice(-SIDE_CHARS) : "", cut: true, total: lines.length };
}

export function outputBlocks({ head, tail, cut, total }: OutputParts, cls = "tc-out mono") {
  const out: HTMLElement[] = [];
  if (!head && !tail) return out;
  out.push(el("pre", cls, head));
  if (cut) {
    const shown = (head ? head.split("\n").length : 0) + (tail ? tail.split("\n").length : 0);
    const gap = el("div", "tc-gap");
    if (total && total > shown) setText(gap, "… 省略 {p0} 行 …", { p0: String(total - shown) });
    else setText(gap, "… 输出过长，中间已省略 …");
    out.push(gap);
    if (tail) out.push(el("pre", cls, tail));
  }
  return out;
}

/* diff 文本 → 着色的行；kind 为 add / delete 时整段是新增 / 删除的内容，逐行补上符号 */
export function diffBlock(text: string, kind = "", truncated = false) {
  const pre = el("pre", "tc-diff mono");
  for (const l of text.replace(/\n$/, "").split("\n")) {
    const cls = kind === "add" ? "add" : kind === "delete" ? "del"
      : l.startsWith("+++") || l.startsWith("---") ? "file" : l.startsWith("@@") ? "hunk"
      : l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : "";
    const span = el("span", cls ? "dl " + cls : "dl", (kind === "add" ? "+" : kind === "delete" ? "-" : "") + l);
    pre.append(span, "\n");
  }
  const out: HTMLElement[] = [pre];
  if (truncated) {
    const gap = el("div", "tc-gap");
    setText(gap, "… 改动过长，后面已省略 …");
    out.push(gap);
  }
  return out;
}

export function diffStat(added?: number, removed?: number) {
  if (!added && !removed) return null;
  const stat = el("span", "tc-stat");
  if (added) stat.append(el("span", "add", "+" + added));
  if (removed) stat.append(el("span", "del", "−" + removed));
  return stat;
}

/* 容器里的路径 → 文件直链（/workspace 与 /shared 两个范围），其它路径不给图 */
function fileURL(path: string) {
  if (!S.current) return "";
  const p = path.replace(/^file:\/\//, "");
  const [rel, scope] = p.startsWith("/workspace/") ? [p.slice(11), ""] : p.startsWith("/shared/") ? [p.slice(8), "&scope=shared"] : ["", ""];
  if (!rel) return "";
  return `/api/sessions/${encodeURIComponent(S.current.id)}/file?path=${encodeURIComponent(rel)}&token=${encodeURIComponent(S.token)}${scope}`;
}

export function imageBody(path: string) {
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

/* ---- 进行中的行：key → 行元素。完成时取出原地更新；上限防止中断的回合越攒越多 ---- */
const pending = new Map<string, HTMLElement>();
const PENDING_MAX = 300;

export function trackRow(key: string, wrap: HTMLElement) {
  const prev = pending.get(key); // 同一条目又开始了（上一次没收尾）：旧行停在未完成
  if (prev && prev !== wrap) settleRow(prev);
  pending.set(key, wrap);
  if (pending.size > PENDING_MAX) pending.delete(pending.keys().next().value!);
}

export function pendingRow(key: string) {
  return key ? pending.get(key) : undefined;
}

export function takeRow(key: string) {
  const row = pendingRow(key);
  if (row) pending.delete(key);
  return row;
}

/* 自己画进行中状态的行（Claude 工具行）登记收尾回调，停止时由它重画 */
const settlers = new WeakMap<HTMLElement, () => void>();
export function onSettle(row: HTMLElement, fn: () => void) {
  settlers.set(row, fn);
}

/** 回合结束：还挂着「进行中」的行（中断、断线）停止转圈，收起实时输出 */
function settleRow(row: HTMLElement) {
  row.dataset.status = "unfinished";
  settlers.get(row)?.();
  row.querySelector(":scope > .tc-live")?.remove();
  row.querySelector(":scope > details > summary .tc-spin, :scope > .chip .tc-spin")?.replaceWith(svgIcon((row.dataset.rowIcon || "chat.tool") as IconSlot, 12));
  for (const [k, v] of pending) if (v === row) pending.delete(k);
}

export function settleToolRows(root: ParentNode, keep?: Element | null) {
  for (const row of root.querySelectorAll<HTMLElement>('.tool-call[data-status="running"]')) {
    if (!keep?.contains(row)) settleRow(row);
  }
}

/** 切线程、整体重载：对话流要重建，丢掉登记 */
export function resetToolRows() {
  pending.clear();
}
