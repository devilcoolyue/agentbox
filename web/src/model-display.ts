import { t as i18nText, setAttrRender, setText, setTextRender } from "./i18n.js";
/* 模型管理「账号模型」标签页：账号按协议（Claude / OpenAI）分组，每个账号一张可展开的卡片，
 * 收起时只显示摘要（显示几个、默认模型、待核对的价格）。展开后每个模型一行：对话下拉里的显示
 * 开关，以及实际计费的单价。关闭只是隐藏，模型仍属于账号，正在用它的空间照常可用。开关即时
 * 保存；同一账号的保存串行进行，保存期间再切换的结果记在 pending 里，渲染时叠加在账号数据上，
 * 不会被轮询刷新冲掉。价格来自全局价目表（按模型计价，与账号无关），点开可以直接设置。 */
import { api } from "./api.js";
import type { Account, ModelOption } from "./types.js";
import { $, toast } from "./util.js";
import { S, emit } from "./state.js";
import { refreshAll } from "./data.js";
import { setTip } from "./tip.js";
import { agentIcon } from "./brand.js";
import { actionButton } from "./icons.js";
import { effortSummary, openAccountModels } from "./account-models.js";
import { priceLookup, type PriceHit } from "./pricing.js";
import { openModelPrice, ratesText, usd } from "./model-price.js";

/** 账号 ID → 模型 ID → 待保存的 hidden 值 */
const pending = new Map<string, Map<string, boolean>>();
const saving = new Map<string, { again: boolean }>();
/** 账号卡片的展开状态（用户点过的才记）。没点过时默认收起；只有一个账号有模型列表时展开它。
 * 筛选时匹配的卡片一律展开。 */
const opened = new Map<string, boolean>();
const isOpen = (a: Account, only: string) => opened.get(a.id) ?? a.id === only;
const onlyListed = () => {
  const listed = S.accounts.filter(a => a.models?.length);
  return listed.length === 1 ? listed[0].id : "";
};
let wired = false;
let switchSeq = 0;

/** 按接入协议分组：Claude Code 账号走 Anthropic 协议，Codex 账号走 OpenAI 协议（含 DeepSeek 等中转）。 */
const GROUPS = [
  { type: "claude", title: () => i18nText("Claude 协议"), cli: "Claude Code" },
  { type: "codex", title: () => i18nText("OpenAI 协议"), cli: "Codex" },
];

function isHidden(a: Account, m: ModelOption) {
  const v = pending.get(a.id)?.get(m.id);
  return v === undefined ? !!m.hidden : v;
}
function filterText() { return $<HTMLInputElement>("mv-filter").value.trim().toLowerCase(); }
const found = (q: string, ...values: string[]) => values.some(v => v.toLowerCase().includes(q));
function countText(a: Account) {
  const models = a.models || [];
  return i18nText("显示 {n} / {total}", { n: String(models.filter(m => !isHidden(a, m)).length), total: String(models.length) });
}

function priceTip(hit: PriceHit): string {
  const action = hit.kind === "exact" || hit.kind === "dated" ? i18nText("点击修改") : i18nText("点击设置");
  const rates = hit.price ? ratesText(hit.price) + i18nText("（美元 / 百万 token）") : "";
  const what = hit.kind === "dated" ? rates + i18nText(" · 按 {key} 计价", { key: hit.key })
    : hit.kind === "fallback" ? i18nText("没有单独的价格，按 {agent} 兜底价计费：", { agent: hit.key }) + rates
    : hit.kind === "unpriced" ? i18nText("没有价格：只记用量，不扣额度") : rates;
  return what + " · " + action;
}

/** 价格列：按价目表的查找顺序显示实际计费的输入 / 输出单价，兜底价和未定价标成警示。 */
function priceButton(a: Account, m: ModelOption) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "btn btn-sm btn-ghost mv-price";
  const hit = S.role === "admin" ? priceLookup(a.type, m.id) : null;
  if (!hit) {
    button.classList.add("hidden"); // .btn's display beats the hidden attribute
    return button;
  }
  button.dataset.kind = hit.kind;
  button.classList.toggle("warn", hit.kind === "fallback" || hit.kind === "unpriced");
  const rates = hit.price ? `${usd(hit.price.input)} / ${usd(hit.price.output)}` : "";
  actionButton(button, () => hit.kind === "unpriced" ? i18nText("未定价") : hit.kind === "fallback" ? i18nText("兜底 {rates}", { rates }) : rates, "wallet", () => priceTip(hit));
  button.addEventListener("click", () => openModelPrice(a.type, m.id, m.label));
  return button;
}

function modelRow(a: Account, m: ModelOption, card: HTMLElement) {
  const row = document.createElement("label");
  row.className = "mv-row";
  row.dataset.model = m.id;
  const name = document.createElement("span");
  name.className = "mv-name";
  name.append(Object.assign(document.createElement("span"), { className: "mv-label", textContent: m.label }));
  if (m.label !== m.id) name.append(Object.assign(document.createElement("span"), { className: "mv-id mono", textContent: m.id }));
  const effort = Object.assign(document.createElement("span"), { className: "mv-effort", textContent: effortSummary(m.reasoning) });
  const tag = document.createElement("span");
  tag.className = "mv-tag";
  const isDefault = m.id === a.default_model;
  if (isDefault) setText(tag, "默认");
  tag.hidden = !isDefault;
  const box = Object.assign(document.createElement("input"), { type: "checkbox", className: "mv-switch", checked: !isHidden(a, m), disabled: isDefault });
  // The price button comes first and is labelable too: bind the row to the switch.
  box.id = "mv-switch-" + ++switchSeq;
  row.htmlFor = box.id;
  box.setAttribute("role", "switch");
  box.dataset.key = a.id + "\n" + m.id;
  setAttrRender(box, "aria-label", () => i18nText("在对话中显示 {model}", { model: m.label }));
  if (isDefault) setTip(row, () => i18nText("默认模型始终显示。要隐藏它，先在「可用模型」里换一个默认模型。"));
  box.addEventListener("change", () => {
    let changes = pending.get(a.id);
    if (!changes) pending.set(a.id, changes = new Map());
    changes.set(m.id, !box.checked);
    row.classList.toggle("off", !box.checked);
    setTextRender(card.querySelector<HTMLElement>(".mv-count")!, () => countText(a));
    void save(a.id);
  });
  row.classList.toggle("off", !box.checked);
  row.append(name, effort, priceButton(a, m), tag, box);
  return row;
}

/** 价格要核对的模型数：按 agent 兜底价计费或没有价格。价目表没读到时不算。 */
function priceIssues(a: Account) {
  if (S.role !== "admin") return 0;
  return (a.models || []).filter(m => {
    const kind = priceLookup(a.type, m.id)?.kind;
    return kind === "fallback" || kind === "unpriced";
  }).length;
}

function span(className: string, text = "") {
  return Object.assign(document.createElement("span"), { className, textContent: text });
}

/** open：展开显示 rows（筛选时只是匹配的那几行）。 */
function accountCard(a: Account, open: boolean, rows: ModelOption[]) {
  const card = document.createElement("div");
  card.className = "card mv-card" + (open ? " open" : "");
  card.dataset.account = a.id;
  const head = document.createElement("div");
  head.className = "mv-head";
  const name = span("mv-title");
  name.append(span("mv-account", a.label));
  if (a.label !== a.id) name.append(span("mv-slug mono", a.id));
  const manage = document.createElement("button");
  manage.type = "button";
  manage.className = "btn btn-sm btn-ghost mv-manage";
  manage.addEventListener("click", () => openAccountModels(a, { discover: !a.models?.length }));
  card.append(head);
  if (!a.models?.length) {
    const note = span("mv-summary");
    setText(note, "沿用系统模型列表");
    setTip(note, () => i18nText("对话中显示「系统模型列表」里的全部模型。为账号配置可用模型后，可以在这里逐个控制显示和价格。"));
    actionButton(manage, () => i18nText("配置可用模型…"), "cpu", () => i18nText("从上游读取这个账号的模型并选择可用模型"));
    head.append(span("mv-chev mv-chev-none"), name, note, manage);
    return card;
  }
  const toggle = document.createElement("button");
  toggle.type = "button";
  toggle.className = "mv-toggle";
  toggle.dataset.key = "card\n" + a.id;
  toggle.setAttribute("aria-expanded", String(open));
  toggle.addEventListener("click", () => {
    if (filterText()) return; // matches stay open while filtering
    opened.set(a.id, !open);
    renderModelDisplay();
  });
  const summary = span("mv-summary");
  const count = span("mv-count");
  setTextRender(count, () => countText(a));
  summary.append(count);
  const def = a.models.find(m => m.id === a.default_model);
  if (def) {
    const label = span("mv-default");
    setTextRender(label, () => i18nText("默认 {model}", { model: def.label }));
    summary.append(label);
  }
  const issues = priceIssues(a);
  if (issues) {
    const warn = span("mv-warn");
    setTextRender(warn, () => i18nText("{n} 个价格待核对", { n: String(issues) }));
    summary.append(warn);
  }
  toggle.append(span("mv-chev"), name, summary);
  actionButton(manage, () => i18nText("管理可用模型…"), "cpu", () => i18nText("从上游读取、增删这个账号的模型，设置思考强度与默认模型"));
  head.append(toggle, manage);
  if (open) {
    const list = document.createElement("div");
    list.className = "mv-rows";
    list.append(...rows.map(m => modelRow(a, m, card)));
    card.append(list);
  }
  return card;
}

function groupSection(group: typeof GROUPS[number], accounts: number, cards: HTMLElement[]) {
  const section = document.createElement("section");
  section.className = "mv-group";
  section.dataset.type = group.type;
  const head = document.createElement("h3");
  head.className = "mv-group-head";
  const title = span("mv-group-title");
  setTextRender(title, group.title);
  const sub = span("mv-group-sub");
  setTextRender(sub, () => i18nText("{cli} · {n} 个账号", { cli: group.cli, n: String(accounts) }));
  head.append(agentIcon(group.type, 15), title, sub);
  const list = document.createElement("div");
  list.className = "mv-cards";
  list.append(...cards);
  section.append(head, list);
  return section;
}

/** 系统模型列表只给没有自己可用模型的账号兜底：标出谁在用。 */
function renderSystemState() {
  const users = S.accounts.filter(a => !a.models?.length);
  setTextRender($("mdl-system-state"), () => users.length
    ? i18nText("{n} 个账号在用：{names}", { n: String(users.length), names: users.map(a => a.label).join(" / ") })
    : S.accounts.length ? i18nText("所有账号都已配置可用模型，目前不影响对话") : "");
}

export function renderModelDisplay() {
  wire();
  renderSystemState();
  const box = $("mv-list");
  const focused = (document.activeElement as HTMLElement | null)?.dataset?.key;
  const q = filterText();
  const listed = S.accounts.filter(a => a.models?.length);
  const only = onlyListed();
  const groups: HTMLElement[] = [];
  for (const group of GROUPS) {
    const accounts = S.accounts.filter(a => a.type === group.type);
    const cards: HTMLElement[] = [];
    for (const a of accounts) {
      const models = a.models || [];
      if (!q) { cards.push(accountCard(a, models.length > 0 && isOpen(a, only), models)); continue; }
      // An account name match shows all its models; otherwise only matching models.
      const rows = found(q, a.label, a.id) ? models : models.filter(m => found(q, m.id, m.label));
      if (rows.length || (!models.length && found(q, a.label, a.id))) cards.push(accountCard(a, rows.length > 0, rows));
    }
    if (cards.length) groups.push(groupSection(group, accounts.length, cards));
  }
  box.replaceChildren(...groups);
  if (!groups.length) {
    const empty = document.createElement("p");
    empty.className = "acct-empty";
    setText(empty, S.accounts.length ? "没有匹配的账号或模型" : "账号池为空。先在「账号池」添加账号。");
    box.append(empty);
  }
  const all = $<HTMLButtonElement>("mv-expand");
  const allOpen = listed.every(a => isOpen(a, only));
  all.classList.toggle("hidden", !!q || listed.length < 2);
  actionButton(all, () => allOpen ? i18nText("全部收起") : i18nText("全部展开"), allOpen ? "collapse" : "expand");
  if (focused) box.querySelector<HTMLElement>(`[data-key="${CSS.escape(focused)}"]`)?.focus();
}

async function save(id: string) {
  const state = saving.get(id);
  if (state) { state.again = true; return; }
  const run = { again: false };
  saving.set(id, run);
  try {
    do {
      run.again = false;
      const changes = pending.get(id), acct = S.accounts.find(a => a.id === id);
      if (!changes?.size || !acct?.models?.length) break;
      const sent = new Map(changes);
      const models = acct.models.map(m => {
        const { hidden: _, ...rest } = m;
        return (sent.has(m.id) ? sent.get(m.id) : m.hidden) ? { ...rest, hidden: true } : rest;
      });
      const updated = await api<Account>(`/accounts/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify({ models }) });
      const i = S.accounts.findIndex(a => a.id === id);
      if (i >= 0 && Array.isArray(updated?.models)) S.accounts[i] = { ...S.accounts[i], models: updated.models, default_model: updated.default_model };
      // 保存期间又切换过的模型保留待保存值，下一轮再发。
      for (const [model, hidden] of sent) if (changes.get(model) === hidden) changes.delete(model);
    } while (run.again);
    emit("models-updated");
    void refreshAll().catch(() => {});
  } catch (error) {
    pending.delete(id);
    toast(i18nText("保存显示设置失败：") + (error as Error).message, true);
    await refreshAll().catch(() => {});
    if (S.sec === "models") renderModelDisplay();
  } finally {
    saving.delete(id);
  }
}

/** 正在保存时不重绘，避免开关在请求往返中跳动；保存完成后的刷新会再画一次。 */
export function refreshModelDisplay() {
  if (S.view === "settings" && S.sec === "models" && !saving.size) renderModelDisplay();
}

function wire() {
  if (wired) return;
  wired = true;
  $("mv-filter").addEventListener("input", () => renderModelDisplay());
  $("mv-expand").addEventListener("click", () => {
    const listed = S.accounts.filter(a => a.models?.length), only = onlyListed();
    const next = !listed.every(a => isOpen(a, only));
    for (const a of listed) opened.set(a.id, next);
    renderModelDisplay();
  });
}
