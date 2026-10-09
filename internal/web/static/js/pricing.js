import { setAttrRender, setText, setTextRender, t as i18nText } from "./i18n.js";
import { settingsState } from "./features/settings/state.js";
import { actionButton, buttonLabel } from "./icons.js";
import { S, emit } from "./state.js";
import { $, toast, askConfirm, fmtTime } from "./util.js";
import { api } from "./api.js";
import { setTip } from "./tip.js";
let view = null;
let dirty = false;
let busy = false;
let generation = 0;
const customModels = new Set();
const FALLBACK_KEYS = new Set(["claude", "codex"]);
/** 编辑中的表。进分区时从 settingsState.value 灌一次，保存前从 DOM 读回来。 */
let draft = {};
const num = (v) => v === undefined ? "" : String(v);
function rateInput(cls, value, title) {
    const el = document.createElement("input");
    el.type = "text"; // 用 text 而不是 number：number 在中文输入法下会吃掉小数点
    el.className = cls;
    el.value = num(value);
    el.placeholder = "0";
    setTip(el, title);
    el.inputMode = "decimal";
    return el;
}
/** 只看在用的模型：服务端算出在用模型命中的键；这次新加、还没保存的行也留着。 */
function shownKeys(keys) {
    const inUse = view?.in_use;
    if (!inUse || !$("price-inuse").checked)
        return keys;
    const keep = new Set(inUse);
    return keys.filter(key => keep.has(key) || !view.active.prices[key]);
}
function renderRows() {
    const body = $("price-rows");
    body.replaceChildren();
    const all = Object.keys(draft).sort(), keys = shownKeys(all);
    const hidden = all.length - keys.length;
    setTextRender($("price-inuse-note"), () => hidden ? i18nText("另有 {n} 个未使用的价格已隐藏", { n: String(hidden) }) : "");
    $("price-inuse").closest(".price-filter").classList.toggle("hidden", !view?.in_use);
    for (const key of keys) {
        const p = draft[key];
        const tr = document.createElement("tr");
        tr.dataset.key = key;
        const k = document.createElement("td");
        k.className = "pt-key" + (FALLBACK_KEYS.has(key) ? " fallback" : "");
        k.append(document.createTextNode(key));
        const meta = document.createElement("small");
        const origin = view?.active.managed[key];
        setTextRender(meta, () => origin && !customModels.has(key) ? i18nText("跟随目录 · {p0}", { p0: String(origin.version) }) : i18nText("自定义"));
        k.appendChild(meta);
        if (origin && !customModels.has(key)) {
            const custom = document.createElement("button");
            custom.type = "button";
            custom.className = "price-mode";
            actionButton(custom, () => i18nText("自定义"), "rename", () => i18nText("设为自定义价格"));
            custom.addEventListener("click", () => { readDraft(); customModels.add(key); dirty = true; renderRows(); });
            k.appendChild(custom);
        }
        setTip(k, () => FALLBACK_KEYS.has(key)
            ? i18nText("{p0} 的兜底价：这个 agent 下没有单独配价的模型都按它算", { p0: String(key) })
            : key);
        tr.appendChild(k);
        for (const [field, label] of [
            ["input", i18nText("输入")], ["output", i18nText("输出")],
            ["cache_read", i18nText("缓存读取")], ["cache_write", i18nText("缓存写入")],
        ]) {
            const td = document.createElement("td");
            td.className = "num";
            td.appendChild(rateInput("rate " + field, p[field], i18nText("{p0}：美元 / 百万 token", { p0: String(label) })));
            tr.appendChild(td);
        }
        const over = document.createElement("td");
        over.className = "num";
        over.appendChild(rateInput("rate wide over", p.long_context_over, i18nText("超过这么多 token 的回合整体按右边那档计价（留空 = 没有长上下文档位）")));
        tr.appendChild(over);
        const long = document.createElement("td");
        long.className = "num";
        const wrap = document.createElement("div");
        wrap.className = "pt-long";
        const l = p.long || {};
        for (const [field, label] of [
            ["input", i18nText("输入")], ["output", i18nText("输出")],
            ["cache_read", i18nText("缓存读取")], ["cache_write", i18nText("缓存写入")],
        ]) {
            wrap.appendChild(rateInput("rate long-" + field, l[field], i18nText("长上下文档的{p0}", { p0: String(label) })));
        }
        long.appendChild(wrap);
        tr.appendChild(long);
        const del = document.createElement("td");
        const btn = document.createElement("button");
        btn.className = "pt-del";
        btn.type = "button";
        buttonLabel(btn, "", "trash");
        setAttrRender(btn, "aria-label", () => i18nText("删除 ") + key);
        setTip(btn, () => i18nText("删掉这一行"));
        btn.addEventListener("click", () => {
            readDraft(); // 先把别的行的改动收进来，别让删除顺手回滚它们
            delete draft[key];
            dirty = true;
            renderRows();
        });
        del.appendChild(btn);
        tr.appendChild(del);
        body.appendChild(tr);
    }
    $("price-empty").classList.toggle("hidden", all.length > 0);
}
/** 空串按 0 算；填了非数字则返回 NaN，由 readDraft 的调用方拦下。 */
function parseRate(el) {
    const t = el.value.trim();
    if (!t)
        return 0;
    return Number(t);
}
/** 从 DOM 读回显示中的行，覆盖 draft 里的对应项；筛选隐藏的行保持原值。 */
function readDraft() {
    const next = { ...draft };
    for (const tr of document.querySelectorAll("#price-rows tr")) {
        const key = tr.dataset.key;
        const get = (cls) => parseRate(tr.querySelector("input." + cls));
        const p = {
            input: get("input"), output: get("output"),
            cache_read: get("cache_read"), cache_write: get("cache_write"),
        };
        const over = get("over");
        const long = {
            input: get("long-input"), output: get("long-output"),
            cache_read: get("long-cache_read"), cache_write: get("long-cache_write"),
        };
        // 保留显式 0 档与独立阈值；缺少阈值的长档交给服务端拒绝，不能静默丢价。
        if (over > 0)
            p.long_context_over = over;
        if (["long-input", "long-output", "long-cache_read", "long-cache_write"].some(cls => tr.querySelector("input." + cls).value.trim() !== ""))
            p.long = long;
        next[key] = p;
    }
    draft = next;
}
/** 找出所有不是合法数字的格子，返回给保存流程提示。 */
function badCells() {
    const bad = [];
    for (const tr of document.querySelectorAll("#price-rows tr")) {
        for (const el of tr.querySelectorAll("input.rate")) {
            const t = el.value.trim();
            if (t && (!Number.isFinite(Number(t)) || Number(t) < 0))
                bad.push(tr.dataset.key); // NaN / 负数都进这里
        }
    }
    return [...new Set(bad)];
}
// Keep edits in rows the filter is about to hide.
$("price-inuse").addEventListener("change", () => { readDraft(); renderRows(); });
$("price-add").addEventListener("click", () => {
    const el = $("price-new-key");
    const key = el.value.trim();
    if (!key)
        return;
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(key)) {
        toast(i18nText("模型 ID 只能用字母、数字和 . _ -"), true);
        return;
    }
    readDraft();
    if (draft[key]) {
        toast(i18nText("{p0} 已经在表里了", { p0: String(key) }), true);
        return;
    }
    dirty = true;
    draft[key] = { input: 0, output: 0, cache_read: 0, cache_write: 0 };
    el.value = "";
    renderRows();
    // 新行排序后可能在任何位置，滚过去并聚焦第一个单价框
    const row = document.querySelector(`#price-rows tr[data-key="${CSS.escape(key)}"]`);
    row?.scrollIntoView({ block: "nearest" });
    row?.querySelector("input.input")?.focus();
});
export function initPricing() {
    const reset = () => {
        generation++;
        busy = false;
        dirty = false;
        view = null;
        draft = {};
        customModels.clear();
        for (const id of ["price-rows", "price-changes", "price-history", "price-warnings", "price-source-issues"])
            $(id).replaceChildren();
        $("price-diff").classList.add("hidden");
        $("price-catalog-box").open = false;
        setText($("price-catalog-status"), "读取中…");
        $("price-catalog-error").textContent = "";
        $("price-source").value = "";
        $("price-auto").checked = false;
        $("price-auto-apply").checked = false;
        setBusy(false);
    };
    reset();
    return reset;
}
function setBusy(value) {
    busy = value;
    for (const el of document.querySelectorAll("#mdl-panel-pricing button, #mdl-panel-pricing input"))
        el.disabled = value;
    // Retired catalog rows have no candidate to select.
    for (const el of document.querySelectorAll("#price-changes input[data-removed]"))
        el.disabled = true;
}
function accept(next) {
    view = next;
    dirty = false;
    customModels.clear();
    draft = structuredClone(next.active.prices);
    if (settingsState.value)
        settingsState.value.pricing = structuredClone(draft);
    $("price-source").value = next.active.catalog.url;
    $("price-auto").checked = next.active.catalog.auto_check;
    $("price-auto-apply").checked = !!next.active.catalog.auto_apply;
    renderRows();
    renderCatalog();
    emit("pricing-updated");
}
export async function openPricingSection(force = false) {
    if (S.role !== "admin" || busy || (!force && (dirty || sourceDirty())))
        return;
    const ticket = ++generation;
    setBusy(true);
    try {
        const next = await api("/pricing");
        if (ticket === generation)
            accept(next);
    }
    catch (e) {
        if (ticket === generation)
            toast(i18nText("读取价目表失败：") + e.message, true);
    }
    finally {
        if (ticket === generation)
            setBusy(false);
    }
}
function renderCatalog() {
    if (!view)
        return;
    const c = view.candidate;
    const count = view.changes.filter(row => row.kind === "new" || row.kind === "update").length;
    setTextRender($("price-catalog-status"), () => i18nText("{p0} · {p1} · {p2} 个新增 / 调价候选", { p0: String(c.bundled ? i18nText("内置旧快照（尚未重新核验）") : c.catalog.source === "models.dev" ? i18nText("models.dev 第三方候选（未人工核验）") : i18nText("远程候选目录")), p1: String(c.catalog.version), p2: String(count) }) +
        (c.checked_at ? i18nText(" · 最近成功检查 {p0}", { p0: String(fmtTime(c.checked_at)) }) : i18nText(" · 尚未成功联网检查")));
    const error = c.error || (!view.active.catalog.url ? i18nText("尚未配置远程目录。内置数据仅供核对，不代表最新官方价格。") : "");
    $("price-catalog-error").textContent = error;
    $("price-catalog-error").classList.toggle("hidden", !error);
    // The catalog card is folded; a failed check must not stay out of sight.
    if (c.error)
        $("price-catalog-box").open = true;
    const issues = $("price-source-issues");
    issues.replaceChildren();
    if (c.catalog.issues?.length) {
        const title = document.createElement("b");
        setText(title, "以下模型暂未导入，保留现价");
        issues.appendChild(title);
        const list = document.createElement("ul");
        for (const issue of c.catalog.issues) {
            const row = document.createElement("li");
            row.textContent = `${issue.model}：${issue.reason}`;
            list.appendChild(row);
        }
        issues.appendChild(list);
    }
    issues.classList.toggle("hidden", !issues.childElementCount);
    renderChanges();
    const warnings = $("price-warnings");
    warnings.replaceChildren();
    if (view.warnings.length) {
        const title = document.createElement("b");
        setText(title, "需要核对的模型价格");
        warnings.appendChild(title);
        const hint = document.createElement("p");
        setText(hint, "来自账号可用模型、默认模型、现有空间及最近 30 天使用记录；以下为当前定价状态，不会改写历史账单。");
        warnings.appendChild(hint);
        const list = document.createElement("ul");
        for (const row of view.warnings) {
            const item = document.createElement("li");
            setTextRender(item, () => `${row.agent} / ${row.model || i18nText("未提供模型名")}：${row.kind === "fallback" ? i18nText("使用 {p0} 兜底价", { p0: String(row.key) }) : i18nText("未定价（费用记 0）")}`);
            list.appendChild(item);
        }
        warnings.appendChild(list);
    }
    if (view.warnings_truncated || view.warning_error) {
        const warningError = view.warning_error;
        const note = document.createElement("p");
        setTextRender(note, () => warningError || i18nText("最近使用模型超过 200 种，仅检查前 200 种。"));
        warnings.appendChild(note);
    }
    warnings.classList.toggle("hidden", !warnings.childElementCount);
    const history = $("price-history");
    history.replaceChildren();
    if (!view.active.history.length)
        setText(history, "尚无价格变更记录。");
    for (const item of view.active.history) {
        const row = document.createElement("div");
        row.className = "price-history-row";
        const text = document.createElement("span");
        setText(text, "{p0} · {p1}前 · {p2} 条", { p0: String(fmtTime(item.saved_at)), p1: String(item.reason), p2: String(Object.keys(item.prices).length) });
        const restore = document.createElement("button");
        restore.type = "button";
        restore.className = "btn btn-sm";
        actionButton(restore, () => i18nText("恢复"), "undo", () => i18nText("恢复此价格版本"));
        restore.addEventListener("click", async () => {
            if (!view || busy || !requireClean())
                return;
            const revision = view.active.revision;
            if (!await askConfirm(() => i18nText("将恢复该次修改前的全部价格及跟随状态。自动跟随将暂停，避免下次检查再次覆盖。历史账单和已开始的网页回合保持原价。"), { get title() { return i18nText("恢复价格版本"); }, get okLabel() { return i18nText("恢复"); }, icon: "undo" }))
                return;
            await mutate("/pricing/restore", "POST", { revision, id: item.id }, i18nText("价格版本已恢复"));
        });
        row.append(text, restore);
        history.appendChild(row);
    }
}
const labels = { get new() { return i18nText("新增"); }, get update() { return i18nText("调价"); }, get custom() { return i18nText("自定义 · 默认保留"); }, get current() { return i18nText("价格一致"); }, get removed() { return i18nText("目录已移除 · 保留现价"); } };
const buckets = () => [["input", i18nText("输入")], ["output", i18nText("输出")], ["cache_read", i18nText("缓存读")], ["cache_write", i18nText("缓存写")]];
function rateChange(old, next) {
    if (old === undefined)
        return String(next);
    const percent = old > 0 && old !== next ? ` (${next > old ? "+" : ""}${((next / old - 1) * 100).toFixed(1)}%)` : "";
    return `${old} → ${next}${percent}`;
}
function renderChanges() {
    if (!view)
        return;
    const box = $("price-changes");
    box.replaceChildren();
    for (const row of view.changes) {
        const item = document.createElement("div");
        item.className = "price-change";
        const label = document.createElement("label");
        const check = document.createElement("input");
        check.type = "checkbox";
        check.value = row.model;
        check.checked = row.kind === "new" || row.kind === "update";
        if (!row.candidate) {
            check.disabled = true;
            check.dataset.removed = "true";
        }
        const name = document.createElement("strong");
        name.textContent = row.model;
        const kind = document.createElement("span");
        setTextRender(kind, () => labels[row.kind]);
        label.append(check, name, kind);
        item.appendChild(label);
        if (row.auto_block_reason) {
            const note = document.createElement("p");
            note.className = "card-desc";
            setTextRender(note, () => i18nText("暂不自动应用：") + row.auto_block_reason);
            item.appendChild(note);
        }
        if (row.candidate) {
            const candidate = row.candidate;
            const rates = document.createElement("p");
            rates.className = "price-change-rates";
            setTextRender(rates, () => buckets().map(([key, label]) => `${label} ${rateChange(row.current?.[key], candidate.price[key])}`).join(" · "));
            item.appendChild(rates);
            if (row.current?.long || candidate.price.long || row.current?.long_context_over || candidate.price.long_context_over) {
                const long = document.createElement("p");
                long.className = "price-change-rates";
                setTextRender(long, () => i18nText("长上下文阈值 {p0} → {p1}；", { p0: String(row.current?.long_context_over || i18nText("无")), p1: String(candidate.price.long_context_over || i18nText("无")) }) +
                    (candidate.price.long ? buckets().map(([key, label]) => `${label} ${rateChange(row.current?.long?.[key], candidate.price.long[key])}`).join(" · ") : i18nText("取消长上下文档")));
                item.appendChild(long);
            }
            const source = document.createElement("p");
            source.className = "card-desc";
            const link = document.createElement("a");
            link.href = candidate.source_url;
            link.target = "_blank";
            link.rel = "noopener noreferrer";
            setText(link, "价格来源");
            source.append(link, document.createTextNode(` · ${candidate.verified_at ? i18nText("核验于 ") + fmtTime(Date.parse(candidate.verified_at)) : i18nText("尚未重新核验")}${candidate.notes ? " · " + candidate.notes : ""}`));
            item.appendChild(source);
        }
        box.appendChild(item);
    }
}
function sourceDirty() {
    return !!view && (sourceDraft().url !== view.active.catalog.url || sourceDraft().auto_check !== view.active.catalog.auto_check || sourceDraft().auto_apply !== !!view.active.catalog.auto_apply);
}
function requireClean() {
    if (dirty || sourceDirty()) {
        toast(i18nText("请先保存编辑中的价格或目录来源，再执行此操作"), true);
        return false;
    }
    return true;
}
async function mutate(path, method, body, message) {
    if (busy)
        return;
    const ticket = ++generation;
    setBusy(true);
    try {
        const next = await api(path, { method, body: JSON.stringify(body) });
        if (ticket === generation) {
            accept(next);
            toast(message);
        }
    }
    catch (e) {
        if (ticket === generation)
            toast(e.message, true);
    }
    finally {
        if (ticket === generation)
            setBusy(false);
    }
}
$("price-rows").addEventListener("input", () => { dirty = true; });
$("price-save").addEventListener("click", async () => {
    if (!view || busy)
        return;
    const bad = badCells();
    if (bad.length) {
        toast(i18nText("这些行的单价不是合法数字：{p0}", { p0: String(bad.join("、")) }), true);
        return;
    }
    if (!await confirmAutoApply())
        return;
    readDraft();
    await mutate("/pricing", "PUT", { revision: view.active.revision, prices: draft, custom_models: [...customModels], catalog: sourceDraft() }, i18nText("价目表已保存，修改的模型已设为自定义"));
});
$("price-source-save").addEventListener("click", async () => {
    if (!view || busy)
        return;
    if (dirty) {
        toast(i18nText("请先保存价目表"), true);
        return;
    }
    if (!await confirmAutoApply())
        return;
    await mutate("/pricing", "PUT", { revision: view.active.revision, catalog: sourceDraft() }, i18nText("目录来源已保存"));
});
$("price-refresh").addEventListener("click", async () => {
    if (busy)
        return;
    if ((dirty || sourceDirty()) && !await askConfirm(() => i18nText("将丢弃当前尚未保存的价格和来源编辑。"), { get title() { return i18nText("重新读取价目表"); }, get okLabel() { return i18nText("放弃编辑并重新读取"); }, icon: "undo", danger: true }))
        return;
    await openPricingSection(true);
});
$("price-preview").addEventListener("click", () => { $("price-diff").classList.toggle("hidden"); });
$("price-check").addEventListener("click", async () => {
    if (!view || busy || !requireClean())
        return;
    const ticket = ++generation;
    setBusy(true);
    try {
        const next = await api("/pricing/check", { method: "POST" });
        if (ticket === generation) {
            accept(next);
            $("price-diff").classList.remove("hidden");
            $("price-catalog-box").open = true;
        }
    }
    catch (e) {
        if (ticket === generation)
            toast(e.message, true);
    }
    finally {
        if (ticket === generation)
            setBusy(false);
    }
});
$("price-apply").addEventListener("click", async () => {
    if (!view || busy || !requireClean())
        return;
    const models = [...document.querySelectorAll("#price-changes input:checked")].map(el => el.value);
    if (!models.length) {
        toast(i18nText("请选择需要应用的模型"), true);
        return;
    }
    const adopt = models.filter(key => !!view.active.prices[key] && !view.active.managed[key]);
    const request = { revision: view.active.revision, catalog_revision: view.candidate.revision, models, adopt_custom: adopt };
    const note = i18nText("将应用 {p0} 个模型的候选价格。", { p0: String(models.length) }) + (adopt.length ? i18nText("其中 {p0} 个自定义模型将替换价格并恢复跟随目录。", { p0: String(adopt.length) }) : "") +
        (view.candidate.bundled ? i18nText("当前为尚未重新核验的旧快照，请先核对来源。") : "") + i18nText("历史账单及已开始的网页回合保持原价。");
    if (!await askConfirm(note, { get title() { return i18nText("应用价格变更"); }, get okLabel() { return i18nText("应用"); } }))
        return;
    await mutate("/pricing/apply", "POST", request, i18nText("所选价格已应用"));
});
function sourceDraft() {
    return { url: $("price-source").value.trim(), auto_check: $("price-auto").checked, auto_apply: $("price-auto-apply").checked };
}
async function confirmAutoApply() {
    if (!sourceDraft().auto_apply || view?.active.catalog.auto_apply)
        return true;
    return askConfirm(() => i18nText("每日检查后，将自动调整已明确跟随当前来源的模型价格，仅影响新回合。自定义价格和新模型保持不变；单价变化超过 25%、零价格及长上下文规则变化需要手动核对。"), { get title() { return i18nText("开启自动跟随"); }, get okLabel() { return i18nText("开启"); } });
}
$("price-modelsdev").addEventListener("click", () => {
    if (busy)
        return;
    $("price-source").value = "https://models.dev/api.json";
    $("price-auto").checked = true;
    $("price-auto-apply").checked = false;
});
$("price-auto").addEventListener("change", () => {
    if (!$("price-auto").checked)
        $("price-auto-apply").checked = false;
});
$("price-auto-apply").addEventListener("change", () => {
    if ($("price-auto-apply").checked)
        $("price-auto").checked = true;
});
/** 与服务端 config.LookupPrice 同序：精确模型 ID → 去掉 -YYYYMMDD → agent 名。价目表还没读到时返回 null。 */
export function priceLookup(agent, model) {
    const prices = view?.active.prices;
    if (!prices)
        return null;
    const has = (key) => key !== "" && Object.hasOwn(prices, key);
    if (has(model))
        return { kind: "exact", key: model, price: prices[model] };
    const base = model.replace(/-\d{8}$/, "");
    if (base !== model && has(base))
        return { kind: "dated", key: base, price: prices[base] };
    if (has(agent))
        return { kind: "fallback", key: agent, price: prices[agent] };
    return { kind: "unpriced", key: "" };
}
/** 当前价格目录（如 models.dev）里这个键的候选价。 */
export function catalogPrice(key) {
    const entries = view?.candidate?.catalog.entries;
    return entries && Object.hasOwn(entries, key) ? entries[key] : undefined;
}
export function followsCatalog(key) {
    return !!view && Object.hasOwn(view.active.managed, key);
}
/** 读最新的价目表再提交一次修改；「价格」标签页有未保存的编辑时拒绝，免得两边互相覆盖。 */
async function editOne(change) {
    if (busy)
        throw new Error(i18nText("价目表正在保存，请稍后再试"));
    if (dirty || sourceDirty())
        throw new Error(i18nText("「价格」标签页有未保存的修改，请先保存或放弃"));
    const ticket = ++generation;
    setBusy(true);
    try {
        const next = await change(await api("/pricing"));
        if (ticket === generation)
            accept(next);
    }
    finally {
        if (ticket === generation)
            setBusy(false);
    }
}
/** 设置一个模型的四档单价，保留它已有的长上下文档，存为自定义价格。 */
export function saveModelPrice(key, rates) {
    return editOne(fresh => api("/pricing", { method: "PUT", body: JSON.stringify({
            revision: fresh.active.revision,
            prices: { ...fresh.active.prices, [key]: { ...fresh.active.prices[key], ...rates } },
            custom_models: [key],
        }) }));
}
/** 按价格目录设置这个模型并跟随目录（与「价格」页勾选应用同一个接口）。 */
export function followCatalogPrice(key) {
    return editOne(fresh => {
        if (!Object.hasOwn(fresh.candidate.catalog.entries || {}, key))
            throw new Error(i18nText("价格目录里已经没有这个模型"));
        const custom = Object.hasOwn(fresh.active.prices, key) && !Object.hasOwn(fresh.active.managed, key);
        return api("/pricing/apply", { method: "POST", body: JSON.stringify({
                revision: fresh.active.revision, catalog_revision: fresh.candidate.revision, models: [key], adopt_custom: custom ? [key] : [],
            }) });
    });
}
/** 价格目录来源的简称，用在「账号模型」的改价弹窗里。 */
export function catalogName() {
    return view?.candidate?.catalog.source === "models.dev" ? "models.dev" : i18nText("价格目录");
}
