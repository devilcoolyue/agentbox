/* 对话流里的委派卡片：主 Agent 每调用一次 delegate 就有一张，状态随服务端推送原地更新。
 * 历史里同一次委派会落多条（开始、结束、评价），按 ID 合并成一张卡片。
 * 跑腿模型的完整过程单独保存，点「查看跑腿过程」时按需读取，不进主对话的历史。 */
import { api } from "../../api.js";
import { problemMessages } from "../../problems.js";
import { renderEvent, formatText } from "../../chat-render.js";
import { settleToolRows } from "../chat/tool-rows.js";
import { buttonLabel } from "../../icons.js";
import { setText, setTextRender, t } from "../../i18n.js";
import { S } from "../../state.js";
import { setTip } from "../../tip.js";
import type { AgentEvent, DelegationView, HistoryEntry } from "../../types.js";
import { durationLabel, feedbackLabel, rejectionLabel, riskLabel, routeLabel, stateLabel, taskTypeLabel, usdLabel } from "./labels.js";

const cards = new Map<string, { el: HTMLElement; view: DelegationView }>();
const order: Record<string, number> = { routing: 0, running: 1, declined: 2, completed: 2, failed: 2, cancelled: 2, uncertain: 2 };

function firstLine(text: string) {
  const line = (text || "").split("\n").find(l => l.trim()) || "";
  return line.length > 90 ? line.slice(0, 90) + "…" : line;
}

function section(title: string, ...body: Node[]) {
  const box = document.createElement("div");
  box.className = "dg-section";
  const h = document.createElement("div");
  h.className = "dg-section-title";
  setText(h, title);
  box.append(h, ...body);
  return box;
}

function changesList(view: DelegationView) {
  const c = view.changes;
  const box = document.createElement("div");
  box.className = "dg-changes";
  if (!c) return null;
  if (!c.known) {
    setText(box, "工作区文件太多或读取超时，没能确定改动清单，请主 Agent 或你自己查看 git status。");
    return box;
  }
  if (!c.total) {
    setText(box, "没有文件改动");
    return box;
  }
  const list = document.createElement("ul");
  for (const [kind, paths] of [["added", c.added], ["modified", c.modified], ["deleted", c.deleted]] as const) {
    for (const path of paths || []) {
      const li = document.createElement("li");
      li.className = "dg-change " + kind;
      const tag = document.createElement("span");
      tag.className = "dg-change-kind";
      setText(tag, kind === "added" ? "新增" : kind === "modified" ? "修改" : "删除");
      li.append(tag, Object.assign(document.createElement("span"), { className: "mono", textContent: path }));
      list.append(li);
    }
  }
  box.append(list);
  if (c.truncated) {
    const more = document.createElement("p");
    more.className = "muted";
    setTextRender(more, () => t("共 {n} 个文件改动，只列出前一部分。", { n: String(c.total) }));
    box.append(more);
  }
  return box;
}

function fill(el: HTMLElement, view: DelegationView) {
  el.dataset.state = view.state;
  const head = el.querySelector<HTMLElement>(".dg-summary")!;
  const badge = document.createElement("span");
  badge.className = "dg-badge";
  buttonLabel(badge, () => t("委派"), "users");
  const worker = document.createElement("span");
  worker.className = "dg-worker";
  if (view.worker_label) worker.textContent = view.worker_label;
  else setText(worker, view.state === "declined" ? "主 Agent" : "跑腿");
  const task = Object.assign(document.createElement("span"), { className: "dg-task", textContent: firstLine(view.task) || taskTypeLabel(view.task_type) });
  const state = document.createElement("span");
  state.className = "dg-state";
  setTextRender(state, () => stateLabel(view.state));
  const meta = document.createElement("span");
  meta.className = "dg-meta";
  setTextRender(meta, () => [durationLabel(view.duration_ms), view.cost_micro_usd ? usdLabel(view.cost_micro_usd) : ""].filter(Boolean).join(" · "));
  head.replaceChildren(badge, worker, task, state, meta);

  const body = el.querySelector<HTMLElement>(".dg-detail")!;
  const parts: Node[] = [];
  const facts = document.createElement("p");
  facts.className = "dg-facts";
  setTextRender(facts, () => [
    taskTypeLabel(view.task_type),
    view.risks?.length ? t("风险：{list}", { list: view.risks.map(riskLabel).join("、") }) : t("无标注风险"),
    view.access === "read" ? t("只读") : t("可修改文件"),
    view.account_label ? `${view.account_label}${view.model ? " · " + view.model : ""}` : "",
  ].filter(Boolean).join(" · "));
  parts.push(facts);
  if (view.route) {
    const route = document.createElement("p");
    route.className = "dg-route";
    setTextRender(route, () => t("路由：") + routeLabel(view));
    parts.push(route);
    const rejected = Object.entries(view.route.rejected || {});
    if (rejected.length && view.state === "declined") {
      const why = document.createElement("p");
      why.className = "dg-route muted";
      setTextRender(why, () => rejected.map(([, reason]) => rejectionLabel(reason)).join("；"));
      parts.push(why);
    }
  }
  if (view.task) {
    const pre = Object.assign(document.createElement("pre"), { className: "dg-task-text", textContent: view.task });
    parts.push(section("交给跑腿的任务", pre));
  }
  const changes = changesList(view);
  if (changes) parts.push(section("改动的文件（平台实测）", changes));
  if (view.report) {
    const report = document.createElement("div");
    report.className = "dg-report md";
    report.append(formatText(view.report));
    parts.push(section("跑腿报告", report));
  }
  if (view.error_code && view.state !== "completed") {
    // A known problem code reads as its message; others stay as the raw code.
    const code = view.error_code, known = problemMessages[code];
    const err = Object.assign(document.createElement("p"), { className: "dg-error" });
    setTextRender(err, () => known ? t(known[0]) : code);
    parts.push(err);
  }
  if (view.feedback) {
    const fb = document.createElement("p");
    fb.className = "dg-feedback " + view.feedback;
    setTextRender(fb, () => feedbackLabel(view.feedback!) + (view.feedback_note ? "：" + view.feedback_note : ""));
    parts.push(fb);
  }
  const live = el.querySelector<HTMLElement>(".dg-live");
  const keepLive = !!live && (view.state === "running" || view.state === "routing");
  if (view.state !== "declined" && view.state !== "routing") {
    const log = document.createElement("button");
    log.type = "button";
    log.className = "btn btn-sm btn-ghost dg-log";
    buttonLabel(log, () => t("查看跑腿过程"), "history");
    log.addEventListener("click", () => void openLog(view));
    parts.push(log);
  }
  body.replaceChildren(...parts);
  if (keepLive) body.append(live!);
}

/** 新建或原地更新一张卡片。已在页面上的卡片更新后返回 null，调用方不必再放置。 */
export function upsertDelegationCard(view: DelegationView): HTMLElement | null {
  if (!view?.id) return null;
  const known = cards.get(view.id);
  if (known && known.el.isConnected) {
    // 推送可能乱序：较早的阶段不覆盖已经结束的状态，评价等补充信息照常合并。
    if ((order[view.state] ?? 0) < (order[known.view.state] ?? 0)) return null;
    known.view = view;
    fill(known.el, view);
    return null;
  }
  const el = document.createElement("details");
  el.className = "delegation-card";
  el.dataset.delegation = view.id;
  const summary = document.createElement("summary");
  summary.className = "dg-summary";
  const detail = document.createElement("div");
  detail.className = "dg-detail";
  el.append(summary, detail);
  cards.set(view.id, { el, view });
  fill(el, view);
  return el;
}

/** 跑腿运行中推送来的完整事件：在卡片里滚动显示最近几条，结束后收起。 */
export function delegationLiveEntry(id: string, entry: HistoryEntry | undefined) {
  const card = cards.get(id);
  if (!card?.el.isConnected || !entry || entry.kind !== "event") return;
  const nodes = renderEvent(entry.event as AgentEvent);
  if (!nodes.length) return;
  let live = card.el.querySelector<HTMLElement>(".dg-live");
  if (!live) {
    live = document.createElement("div");
    live.className = "dg-live";
    card.el.querySelector(".dg-detail")!.append(live);
  }
  live.append(...nodes);
  while (live.childElementCount > 6) live.firstElementChild!.remove();
  setTip(card.el.querySelector(".dg-summary")!, () => t("跑腿正在执行，展开查看最近的输出"));
}

/** 切换空间或重载对话时清空登记，旧节点随对话流一起丢弃。 */
export function resetDelegationCards() { cards.clear(); }

let dialog: HTMLDialogElement | null = null;
async function openLog(view: DelegationView) {
  const session = S.current?.id;
  if (!session) return;
  if (!dialog) {
    dialog = document.createElement("dialog");
    dialog.className = "dlg-delegation-log";
    dialog.innerHTML = `<div class="dlg-head"><div class="dlg-title-block"><h3 class="dg-log-title"></h3><p class="dg-log-sub muted"></p></div><button type="button" class="icon-btn dg-log-close"></button></div><div class="dlg-body dg-log-body"></div><div class="dlg-actions"><button type="button" class="btn dg-log-ok"></button></div>`;
    document.body.append(dialog);
    const close = dialog.querySelector<HTMLButtonElement>(".dg-log-close")!;
    buttonLabel(close, "", "close");
    setTip(close, () => t("关闭"));
    close.setAttribute("aria-label", t("关闭"));
    buttonLabel(dialog.querySelector<HTMLButtonElement>(".dg-log-ok")!, () => t("关闭"), "check");
    for (const b of dialog.querySelectorAll("button")) b.addEventListener("click", () => dialog?.close());
  }
  const title = dialog.querySelector<HTMLElement>(".dg-log-title")!, sub = dialog.querySelector<HTMLElement>(".dg-log-sub")!, body = dialog.querySelector<HTMLElement>(".dg-log-body")!;
  setTextRender(title, () => t("跑腿过程 · {worker}", { worker: view.worker_label || view.model || "" }));
  sub.textContent = firstLine(view.task);
  body.replaceChildren(Object.assign(document.createElement("p"), { className: "muted", textContent: t("正在读取…") }));
  dialog.showModal();
  try {
    const entries: HistoryEntry[] = [];
    let after = 0;
    for (let page = 0; page < 20; page++) {
      const res = await api<{ entries: HistoryEntry[]; next: number; has_more: boolean }>(`/sessions/${encodeURIComponent(session)}/delegations/${encodeURIComponent(view.id)}/log?after=${after}`);
      entries.push(...(res.entries || []));
      if (!res.has_more) break;
      after = res.next;
    }
    const nodes: Node[] = [];
    for (const entry of entries) {
      if (entry.kind === "user" && entry.text) {
        const pre = Object.assign(document.createElement("pre"), { className: "dg-task-text", textContent: entry.text });
        nodes.push(section("派给跑腿的输入", pre));
      } else if (entry.kind === "event") nodes.push(...renderEvent(entry.event as AgentEvent));
    }
    if (!nodes.length) nodes.push(Object.assign(document.createElement("p"), { className: "muted", textContent: t("没有可显示的过程记录") }));
    body.replaceChildren(...nodes);
    settleToolRows(body); // 过程记录是事后回看，没收尾的工具行不再转圈
  } catch (error) {
    body.replaceChildren(Object.assign(document.createElement("p"), { className: "dg-error", textContent: t("读取跑腿过程失败：") + (error as Error).message }));
  }
}
