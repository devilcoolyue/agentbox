import { settingsState } from "./features/settings/state.js";
import { actionButton, buttonLabel } from "./icons.js";
import { S } from "./state.js";
import type { ModelPrice, TokenRates, PricingView, PriceChange, PricingCatalogConfig } from "./types.js";
import { $, toast, askConfirm, fmtTime } from "./util.js";
import { api } from "./api.js";
import { setTip } from "./tip.js";

let view: PricingView | null = null;
let dirty = false;
let busy = false;
let generation = 0;
const customModels = new Set<string>();

const FALLBACK_KEYS = new Set(["claude", "codex"]);

/** 编辑中的表。进分区时从 settingsState.value 灌一次，保存前从 DOM 读回来。 */
let draft: Record<string, ModelPrice> = {};

const num = (v: number | undefined) => v === undefined ? "" : String(v);

function rateInput(cls: string, value: number | undefined, title: string) {
  const el = document.createElement("input");
  el.type = "text"; // 用 text 而不是 number：number 在中文输入法下会吃掉小数点
  el.className = cls;
  el.value = num(value);
  el.placeholder = "0";
  setTip(el, title);
  el.inputMode = "decimal";
  return el;
}

function renderRows() {
  const body = $("price-rows");
  body.replaceChildren();
  const keys = Object.keys(draft).sort();
  for (const key of keys) {
    const p = draft[key];
    const tr = document.createElement("tr");
    tr.dataset.key = key;

    const k = document.createElement("td");
    k.className = "pt-key" + (FALLBACK_KEYS.has(key) ? " fallback" : "");
    k.append(document.createTextNode(key));
    const meta = document.createElement("small");
    const origin = view?.active.managed[key];
    meta.textContent = origin && !customModels.has(key) ? `跟随目录 · ${origin.version}` : "自定义";
    k.appendChild(meta);
    if (origin && !customModels.has(key)) {
      const custom = document.createElement("button"); custom.type = "button"; custom.className = "price-mode";
      actionButton(custom, "自定义", "rename", "设为自定义价格");
      custom.addEventListener("click", () => { readDraft(); customModels.add(key); dirty = true; renderRows(); });
      k.appendChild(custom);
    }
    setTip(k, FALLBACK_KEYS.has(key)
      ? `${key} 的兜底价：这个 agent 下没有单独配价的模型都按它算`
      : key);
    tr.appendChild(k);

    for (const [field, label] of [
      ["input", "输入"], ["output", "输出"],
      ["cache_read", "缓存读取"], ["cache_write", "缓存写入"],
    ] as const) {
      const td = document.createElement("td");
      td.className = "num";
      td.appendChild(rateInput("rate " + field, p[field], `${label}：美元 / 百万 token`));
      tr.appendChild(td);
    }

    const over = document.createElement("td");
    over.className = "num";
    over.appendChild(rateInput("rate wide over", p.long_context_over,
      "超过这么多 token 的回合整体按右边那档计价（留空 = 没有长上下文档位）"));
    tr.appendChild(over);

    const long = document.createElement("td");
    long.className = "num";
    const wrap = document.createElement("div");
    wrap.className = "pt-long";
    const l: Partial<TokenRates> = p.long || {};
    for (const [field, label] of [
      ["input", "输入"], ["output", "输出"],
      ["cache_read", "缓存读取"], ["cache_write", "缓存写入"],
    ] as const) {
      wrap.appendChild(rateInput("rate long-" + field, l[field], `长上下文档的${label}`));
    }
    long.appendChild(wrap);
    tr.appendChild(long);

    const del = document.createElement("td");
    const btn = document.createElement("button");
    btn.className = "pt-del";
    btn.type = "button";
    buttonLabel(btn, "", "trash");
    btn.setAttribute("aria-label", "删除 " + key);
    setTip(btn, "删掉这一行");
    btn.addEventListener("click", () => {
      readDraft();          // 先把别的行的改动收进来，别让删除顺手回滚它们
      delete draft[key];
      dirty = true;
      renderRows();
    });
    del.appendChild(btn);
    tr.appendChild(del);

    body.appendChild(tr);
  }
  $("price-empty").classList.toggle("hidden", keys.length > 0);
  $("price-count").textContent = String(keys.length);
}

/** 空串按 0 算；填了非数字则返回 NaN，由 readDraft 的调用方拦下。 */
function parseRate(el: HTMLInputElement) {
  const t = el.value.trim();
  if (!t) return 0;
  return Number(t);
}

/** 从 DOM 读回整张表，覆盖 draft。 */
function readDraft() {
  const next: Record<string, ModelPrice> = {};
  for (const tr of document.querySelectorAll<HTMLElement>("#price-rows tr")) {
    const key = tr.dataset.key!;
    const get = (cls: string) => parseRate(tr.querySelector<HTMLInputElement>("input." + cls)!);
    const p: ModelPrice = {
      input: get("input"), output: get("output"),
      cache_read: get("cache_read"), cache_write: get("cache_write"),
    };
    const over = get("over");
    const long: TokenRates = {
      input: get("long-input"), output: get("long-output"),
      cache_read: get("long-cache_read"), cache_write: get("long-cache_write"),
    };
    // 保留显式 0 档与独立阈值；缺少阈值的长档交给服务端拒绝，不能静默丢价。
    if (over > 0) p.long_context_over = over;
    if (["long-input", "long-output", "long-cache_read", "long-cache_write"].some(cls => tr.querySelector<HTMLInputElement>("input." + cls)!.value.trim() !== "")) p.long = long;
    next[key] = p;
  }
  draft = next;
}

/** 找出所有不是合法数字的格子，返回给保存流程提示。 */
function badCells() {
  const bad: string[] = [];
  for (const tr of document.querySelectorAll<HTMLElement>("#price-rows tr")) {
    for (const el of tr.querySelectorAll<HTMLInputElement>("input.rate")) {
      const t = el.value.trim();
      if (t && (!Number.isFinite(Number(t)) || Number(t) < 0)) bad.push(tr.dataset.key!); // NaN / 负数都进这里
    }
  }
  return [...new Set(bad)];
}

$("price-add").addEventListener("click", () => {
  const el = $<HTMLInputElement>("price-new-key");
  const key = el.value.trim();
  if (!key) return;
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(key)) {
    toast("模型 ID 只能用字母、数字和 . _ -", true);
    return;
  }
  readDraft();
  if (draft[key]) { toast(`${key} 已经在表里了`, true); return; }
  dirty = true;
  draft[key] = { input: 0, output: 0, cache_read: 0, cache_write: 0 };
  el.value = "";
  renderRows();
  // 新行排序后可能在任何位置，滚过去并聚焦第一个单价框
  const row = document.querySelector<HTMLElement>(`#price-rows tr[data-key="${CSS.escape(key)}"]`);
  row?.scrollIntoView({ block: "nearest" });
  row?.querySelector<HTMLInputElement>("input.input")?.focus();
});

export function initPricing() {
  const reset = () => {
    generation++; busy = false; dirty = false; view = null; draft = {}; customModels.clear();
    for (const id of ["price-rows", "price-changes", "price-history", "price-warnings", "price-source-issues"]) $(id).replaceChildren();
    $("price-diff").classList.add("hidden");
    $("price-catalog-status").textContent = "读取中…";
    $("price-catalog-error").textContent = "";
    $<HTMLInputElement>("price-source").value = "";
    $<HTMLInputElement>("price-auto").checked = false;
    $<HTMLInputElement>("price-auto-apply").checked = false;
    setBusy(false);
  };
  reset();
  return reset;
}

function setBusy(value: boolean) {
  busy = value;
  for (const el of document.querySelectorAll<HTMLInputElement | HTMLButtonElement>("#sec-pricing button, #sec-pricing input")) el.disabled = value;
  // Retired catalog rows have no candidate to select.
  for (const el of document.querySelectorAll<HTMLInputElement>("#price-changes input[data-removed]")) el.disabled = true;
}

function accept(next: PricingView) {
  view = next; dirty = false; customModels.clear();
  draft = structuredClone(next.active.prices);
  if (settingsState.value) settingsState.value.pricing = structuredClone(draft);
  $<HTMLInputElement>("price-source").value = next.active.catalog.url;
  $<HTMLInputElement>("price-auto").checked = next.active.catalog.auto_check;
  $<HTMLInputElement>("price-auto-apply").checked = !!next.active.catalog.auto_apply;
  renderRows(); renderCatalog();
}

export async function openPricingSection(force = false) {
  if (S.role !== "admin" || busy || (!force && (dirty || sourceDirty()))) return;
  const ticket = ++generation;
  setBusy(true);
  try {
    const next = await api<PricingView>("/pricing");
    if (ticket === generation) accept(next);
  } catch (e) { if (ticket === generation) toast("读取价目表失败：" + (e as Error).message, true); }
  finally { if (ticket === generation) setBusy(false); }
}

export function refreshPriceCount() {
  $("price-count").textContent = String(Object.keys(view?.active.prices || settingsState.value?.pricing || {}).length);
}

function renderCatalog() {
  if (!view) return;
  const c = view.candidate;
  const count = view.changes.filter(row => row.kind === "new" || row.kind === "update").length;
  $("price-catalog-status").textContent = `${c.bundled ? "内置旧快照（尚未重新核验）" : c.catalog.source === "models.dev" ? "models.dev 第三方候选（未人工核验）" : "远程候选目录"} · ${c.catalog.version} · ${count} 个新增 / 调价候选` +
    (c.checked_at ? ` · 最近成功检查 ${fmtTime(c.checked_at)}` : " · 尚未成功联网检查");
  const error = c.error || (!view.active.catalog.url ? "尚未配置远程目录。内置数据仅供核对，不代表最新官方价格。" : "");
  $("price-catalog-error").textContent = error;
  $("price-catalog-error").classList.toggle("hidden", !error);
  const issues = $("price-source-issues"); issues.replaceChildren();
  if (c.catalog.issues?.length) {
    const title = document.createElement("b"); title.textContent = "以下模型暂未导入，保留现价"; issues.appendChild(title);
    const list = document.createElement("ul");
    for (const issue of c.catalog.issues) {
      const row = document.createElement("li"); row.textContent = `${issue.model}：${issue.reason}`; list.appendChild(row);
    }
    issues.appendChild(list);
  }
  issues.classList.toggle("hidden", !issues.childElementCount);
  renderChanges();
  const warnings = $("price-warnings"); warnings.replaceChildren();
  if (view.warnings.length) {
    const title = document.createElement("b"); title.textContent = "需要核对的模型价格"; warnings.appendChild(title);
    const hint = document.createElement("p"); hint.textContent = "来自默认模型、现有空间及最近 30 天使用记录；以下为当前定价状态，不会改写历史账单。"; warnings.appendChild(hint);
    const list = document.createElement("ul");
    for (const row of view.warnings) {
      const item = document.createElement("li");
      item.textContent = `${row.agent} / ${row.model || "未提供模型名"}：${row.kind === "fallback" ? `使用 ${row.key} 兜底价` : "未定价（费用记 0）"}`;
      list.appendChild(item);
    }
    warnings.appendChild(list);
  }
  if (view.warnings_truncated || view.warning_error) {
    const note = document.createElement("p"); note.textContent = view.warning_error || "最近使用模型超过 200 种，仅检查前 200 种。"; warnings.appendChild(note);
  }
  warnings.classList.toggle("hidden", !warnings.childElementCount);
  const history = $("price-history"); history.replaceChildren();
  if (!view.active.history.length) history.textContent = "尚无价格变更记录。";
  for (const item of view.active.history) {
    const row = document.createElement("div"); row.className = "price-history-row";
    const text = document.createElement("span"); text.textContent = `${fmtTime(item.saved_at)} · ${item.reason}前 · ${Object.keys(item.prices).length} 条`;
    const restore = document.createElement("button"); restore.type = "button"; restore.className = "btn btn-sm"; actionButton(restore, "恢复", "undo", "恢复此价格版本");
    restore.addEventListener("click", async () => {
      if (!view || busy || !requireClean()) return;
      const revision = view.active.revision;
      if (!await askConfirm("将恢复该次修改前的全部价格及跟随状态。自动跟随将暂停，避免下次检查再次覆盖。历史账单和已开始的网页回合保持原价。", { title: "恢复价格版本", okLabel: "恢复", icon: "undo" })) return;
      await mutate("/pricing/restore", "POST", { revision, id: item.id }, "价格版本已恢复");
    });
    row.append(text, restore); history.appendChild(row);
  }
}

const labels: Record<PriceChange["kind"], string> = { new: "新增", update: "调价", custom: "自定义 · 默认保留", current: "价格一致", removed: "目录已移除 · 保留现价" };
const buckets = [["input", "输入"], ["output", "输出"], ["cache_read", "缓存读"], ["cache_write", "缓存写"]] as const;
function rateChange(old: number | undefined, next: number) {
  if (old === undefined) return String(next);
  const percent = old > 0 && old !== next ? ` (${next > old ? "+" : ""}${((next / old - 1) * 100).toFixed(1)}%)` : "";
  return `${old} → ${next}${percent}`;
}
function renderChanges() {
  if (!view) return;
  const box = $("price-changes"); box.replaceChildren();
  for (const row of view.changes) {
    const item = document.createElement("div"); item.className = "price-change";
    const label = document.createElement("label");
    const check = document.createElement("input"); check.type = "checkbox"; check.value = row.model;
    check.checked = row.kind === "new" || row.kind === "update";
    if (!row.candidate) { check.disabled = true; check.dataset.removed = "true"; }
    const name = document.createElement("strong"); name.textContent = row.model;
    const kind = document.createElement("span"); kind.textContent = labels[row.kind];
    label.append(check, name, kind); item.appendChild(label);
    if (row.auto_block_reason) {
      const note = document.createElement("p"); note.className = "card-desc";
      note.textContent = "暂不自动应用：" + row.auto_block_reason; item.appendChild(note);
    }
    if (row.candidate) {
      const candidate = row.candidate;
      const rates = document.createElement("p"); rates.className = "price-change-rates";
      rates.textContent = buckets.map(([key, label]) => `${label} ${rateChange(row.current?.[key], candidate.price[key])}`).join(" · ");
      item.appendChild(rates);
      if (row.current?.long || candidate.price.long || row.current?.long_context_over || candidate.price.long_context_over) {
        const long = document.createElement("p"); long.className = "price-change-rates";
        long.textContent = `长上下文阈值 ${row.current?.long_context_over || "无"} → ${candidate.price.long_context_over || "无"}；` +
          (candidate.price.long ? buckets.map(([key, label]) => `${label} ${rateChange(row.current?.long?.[key], candidate.price.long![key])}`).join(" · ") : "取消长上下文档");
        item.appendChild(long);
      }
      const source = document.createElement("p"); source.className = "card-desc";
      const link = document.createElement("a"); link.href = candidate.source_url; link.target = "_blank"; link.rel = "noopener noreferrer"; link.textContent = "价格来源";
      source.append(link, document.createTextNode(` · ${candidate.verified_at ? "核验于 " + fmtTime(Date.parse(candidate.verified_at)) : "尚未重新核验"}${candidate.notes ? " · " + candidate.notes : ""}`));
      item.appendChild(source);
    }
    box.appendChild(item);
  }
}

function sourceDirty() {
  return !!view && (sourceDraft().url !== view.active.catalog.url || sourceDraft().auto_check !== view.active.catalog.auto_check || sourceDraft().auto_apply !== !!view.active.catalog.auto_apply);
}
function requireClean() {
  if (dirty || sourceDirty()) { toast("请先保存编辑中的价格或目录来源，再执行此操作", true); return false; }
  return true;
}
async function mutate(path: string, method: string, body: unknown, message: string) {
  if (busy) return;
  const ticket = ++generation;
  setBusy(true);
  try { const next = await api<PricingView>(path, { method, body: JSON.stringify(body) }); if (ticket === generation) { accept(next); toast(message); } }
  catch (e) { if (ticket === generation) toast((e as Error).message, true); }
  finally { if (ticket === generation) setBusy(false); }
}

$("price-rows").addEventListener("input", () => { dirty = true; });
$("price-save").addEventListener("click", async () => {
  if (!view || busy) return;
  const bad = badCells();
  if (bad.length) { toast(`这些行的单价不是合法数字：${bad.join("、")}`, true); return; }
  if (!await confirmAutoApply()) return;
  readDraft();
  await mutate("/pricing", "PUT", { revision: view.active.revision, prices: draft, custom_models: [...customModels], catalog: sourceDraft() }, "价目表已保存，修改的模型已设为自定义");
});
$("price-source-save").addEventListener("click", async () => {
  if (!view || busy) return;
  if (dirty) { toast("请先保存价目表", true); return; }
  if (!await confirmAutoApply()) return;
  await mutate("/pricing", "PUT", { revision: view.active.revision, catalog: sourceDraft() }, "目录来源已保存");
});
$("price-refresh").addEventListener("click", async () => {
  if (busy) return;
  if ((dirty || sourceDirty()) && !await askConfirm("将丢弃当前尚未保存的价格和来源编辑。", { title: "重新读取价目表", okLabel: "放弃编辑并重新读取", icon: "undo", danger: true })) return;
  await openPricingSection(true);
});
$("price-preview").addEventListener("click", () => { $("price-diff").classList.toggle("hidden"); });
$("price-check").addEventListener("click", async () => {
  if (!view || busy || !requireClean()) return;
  const ticket = ++generation;
  setBusy(true);
  try { const next = await api<PricingView>("/pricing/check", { method: "POST" }); if (ticket === generation) { accept(next); $("price-diff").classList.remove("hidden"); } }
  catch (e) { if (ticket === generation) toast((e as Error).message, true); }
  finally { if (ticket === generation) setBusy(false); }
});
$("price-apply").addEventListener("click", async () => {
  if (!view || busy || !requireClean()) return;
  const models = [...document.querySelectorAll<HTMLInputElement>("#price-changes input:checked")].map(el => el.value);
  if (!models.length) { toast("请选择需要应用的模型", true); return; }
  const adopt = models.filter(key => !!view!.active.prices[key] && !view!.active.managed[key]);
  const request = { revision: view.active.revision, catalog_revision: view.candidate.revision, models, adopt_custom: adopt };
  const note = `将应用 ${models.length} 个模型的候选价格。` + (adopt.length ? `其中 ${adopt.length} 个自定义模型将替换价格并恢复跟随目录。` : "") +
    (view.candidate.bundled ? "当前为尚未重新核验的旧快照，请先核对来源。" : "") + "历史账单及已开始的网页回合保持原价。";
  if (!await askConfirm(note, { title: "应用价格变更", okLabel: "应用" })) return;
  await mutate("/pricing/apply", "POST", request, "所选价格已应用");
});

function sourceDraft(): PricingCatalogConfig {
  return { url: $<HTMLInputElement>("price-source").value.trim(), auto_check: $<HTMLInputElement>("price-auto").checked, auto_apply: $<HTMLInputElement>("price-auto-apply").checked };
}
async function confirmAutoApply() {
  if (!sourceDraft().auto_apply || view?.active.catalog.auto_apply) return true;
  return askConfirm("每日检查后，将自动调整已明确跟随当前来源的模型价格，仅影响新回合。自定义价格和新模型保持不变；单价变化超过 25%、零价格及长上下文规则变化需要手动核对。", { title: "开启自动跟随", okLabel: "开启" });
}
$("price-modelsdev").addEventListener("click", () => {
  if (busy) return;
  $<HTMLInputElement>("price-source").value = "https://models.dev/api.json";
  $<HTMLInputElement>("price-auto").checked = true;
  $<HTMLInputElement>("price-auto-apply").checked = false;
});
$("price-auto").addEventListener("change", () => {
  if (!$<HTMLInputElement>("price-auto").checked) $<HTMLInputElement>("price-auto-apply").checked = false;
});
$("price-auto-apply").addEventListener("change", () => {
  if ($<HTMLInputElement>("price-auto-apply").checked) $<HTMLInputElement>("price-auto").checked = true;
});
