import { t as i18nText, setText, setTextRender } from "./i18n.js";
/* 账号可用模型：从账号的上游读取模型目录，管理员勾选全部或部分模型，并按模型
 * 设置思考强度。保存后使用这个账号的工作空间只能在这些模型里选择。 */
import { api } from "./api.js";
import { $, askConfirm, btnBusy, btnDone, toast } from "./util.js";
import { editReasoning } from "./reasoning-editor.js";
import { BUDGETS, EFFORT_LABELS } from "./reasoning.js";
import { emit } from "./state.js";
import { refreshAll } from "./data.js";
import { setTip } from "./tip.js";
import { agentName } from "./brand.js";
import { actionButton } from "./icons.js";
const MODEL_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(\[1m\])?$/;
let account = null;
let rows = [];
let defaultId = "";
let summary = "";
let busy = false;
let epoch = 0;
let wired = false;
const dialog = () => $("dlg-acct-models");
export function modelCountLabel(a) {
    if (!a.models?.length)
        return i18nText("沿用系统模型列表");
    const hidden = a.models.filter(m => m.hidden).length;
    return hidden
        ? i18nText("{n} 个可用模型（{h} 个隐藏）", { n: String(a.models.length), h: String(hidden) })
        : i18nText("{n} 个可用模型", { n: String(a.models.length) });
}
export function effortSummary(r) {
    if (!r)
        return i18nText("强度：未设置");
    if (r.support === "unsupported")
        return i18nText("强度：不支持调整");
    if (r.support === "unknown")
        return i18nText("强度：未知");
    return i18nText("强度：") + (r.levels || []).map(l => r.control === "budget" ? `${EFFORT_LABELS[l] || l} (${BUDGETS[l]})` : EFFORT_LABELS[l] || l).join(" / ");
}
function effortTip(row) {
    const from = row.source === "upstream" ? i18nText("由上游模型目录提供")
        : row.source === "global" ? i18nText("沿用系统模型管理中的配置")
            : row.source === "account" ? i18nText("沿用这个账号已有的配置")
                : row.source === "official" ? i18nText("由官方模型目录提供，中转站需转发该参数才生效") : "";
    const what = row.reasoning?.support === "supported" ? i18nText("对话时只显示这些档位")
        : i18nText("对话时不显示思考强度选项，使用模型默认值");
    return [effortSummary(row.reasoning), what, from, i18nText("点击修改")].filter(Boolean).join(" · ");
}
/** 官方数据来自哪里：Agent 镜像里 CLI 自带的目录，或随服务端发布的快照。 */
function officialWhere(agent, o) {
    if (o.kind === "cli")
        return i18nText("{cli} {version} 内置目录", { cli: agent === "claude" ? "Claude Code" : "Codex CLI", version: o.cli_version || "" });
    return i18nText("内置官方快照（{date} 核对）", { date: o.verified_at || "" });
}
function setError(text) {
    const el = $("acct-models-error");
    el.textContent = text;
    el.classList.toggle("hidden", !text);
}
function filterText() { return $("acct-models-filter").value.trim().toLowerCase(); }
function visible(row) {
    const q = filterText();
    return !q || row.id.toLowerCase().includes(q) || row.label.toLowerCase().includes(q);
}
/** 批量设置强度只作用于筛选后仍可见、且已勾选的模型。 */
function bulkTargets() { return rows.filter(r => r.checked && visible(r)); }
function effectiveDefault() {
    const chosen = rows.filter(r => r.checked && !r.hidden);
    return chosen.some(r => r.id === defaultId) ? defaultId : chosen[0]?.id || "";
}
function renderStatus() {
    const chosen = rows.filter(r => r.checked).length;
    const lines = [rows.length
            ? i18nText("已选 {n} / {total} 个模型", { n: String(chosen), total: String(rows.length) })
            : i18nText("还没有模型。点击「从上游读取」获取这个账号实际可用的模型；中转站读不到时可用「官方目录」，或手动添加模型 ID。")];
    if (!account?.models?.length)
        lines.push(i18nText("当前沿用系统模型列表，保存后改为只用勾选的模型。"));
    if (summary)
        lines.push(summary);
    $("acct-models-status").textContent = lines.join("\n");
    const shown = rows.filter(visible);
    const all = $("acct-models-all");
    all.disabled = !shown.length || busy;
    all.checked = !!shown.length && shown.every(r => r.checked);
    all.indeterminate = !all.checked && shown.some(r => r.checked);
    $("acct-models-bulk").disabled = busy || !bulkTargets().length;
}
function renderRows() {
    const list = $("acct-models-list");
    const def = effectiveDefault();
    list.replaceChildren(...rows.filter(visible).map(row => {
        const el = document.createElement("div");
        el.className = "am-row" + (row.checked ? "" : " off");
        el.dataset.id = row.id;
        const pick = document.createElement("label");
        pick.className = "check am-pick";
        const box = Object.assign(document.createElement("input"), { type: "checkbox", checked: row.checked, disabled: busy });
        box.addEventListener("change", () => { row.checked = box.checked; renderAll(); });
        const name = document.createElement("span");
        name.className = "am-name";
        const label = Object.assign(document.createElement("span"), { className: "am-label", textContent: row.label });
        name.append(label);
        if (row.label !== row.id)
            name.append(Object.assign(document.createElement("span"), { className: "am-id mono", textContent: row.id }));
        pick.append(box, name);
        const tag = document.createElement("span");
        tag.className = "am-tag";
        const tagText = row.state === "new" ? i18nText("新") : row.state === "missing" ? i18nText("上游未列出") : row.state === "manual" ? i18nText("手动")
            : row.state === "official" ? i18nText("官方目录") : row.hidden ? i18nText("已隐藏") : "";
        tag.textContent = tagText;
        tag.hidden = !tagText;
        if (row.state === "missing")
            setTip(tag, () => i18nText("这次读取的模型目录里没有它，可能已下线或账号无权使用"));
        else if (row.state === "official")
            setTip(tag, () => i18nText("来自官方模型目录，不代表这个账号的上游一定提供"));
        else if (row.hidden)
            setTip(tag, () => i18nText("已在「模型管理」里关闭显示，对话下拉里看不到它"));
        const effort = document.createElement("button");
        effort.type = "button";
        effort.className = "btn btn-sm btn-ghost am-effort" + (row.reasoning?.support === "supported" ? " on" : "");
        actionButton(effort, () => effortSummary(row.reasoning), "sliders", () => effortTip(row));
        effort.disabled = busy;
        effort.addEventListener("click", async () => {
            if (!account)
                return;
            const next = await editReasoning(account.type, row.label + i18nText(" · 思考强度"), row.reasoning);
            if (next === null)
                return;
            row.reasoning = next;
            row.source = "";
            renderAll();
        });
        const dflt = document.createElement("label");
        dflt.className = "check am-default";
        const radio = Object.assign(document.createElement("input"), { type: "radio", name: "am-default", checked: row.id === def, disabled: !row.checked || !!row.hidden || busy });
        radio.addEventListener("change", () => { if (radio.checked) {
            defaultId = row.id;
            renderAll();
        } });
        const caption = document.createTextNode("");
        setTextRender(caption, () => " " + i18nText("默认"));
        dflt.append(radio, caption);
        setTip(dflt, () => i18nText("新建工作空间和未选择模型时使用的模型"));
        el.append(pick, tag, effort, dflt);
        return el;
    }));
    if (rows.length && !list.childElementCount) {
        const empty = document.createElement("p");
        empty.className = "field-hint am-empty";
        setText(empty, "没有匹配的模型");
        list.append(empty);
    }
}
function renderAll() { renderRows(); renderStatus(); }
function setBusy(next) {
    busy = next;
    for (const id of ["acct-models-fetch", "acct-models-official", "acct-models-add", "acct-models-ok"])
        $(id).disabled = next;
    $("acct-models-new").disabled = next;
    renderAll();
}
/** official：列出官方模型目录（不访问账号上游，也不需要订阅登录）。 */
async function discover(mode = "upstream") {
    if (!account || busy)
        return;
    const id = account.id, agent = account.type, run = ++epoch;
    const button = $(mode === "official" ? "acct-models-official" : "acct-models-fetch");
    setError("");
    btnBusy(button, () => i18nText("读取中…"));
    setBusy(true);
    try {
        const query = mode === "official" ? "?source=official" : "";
        const res = await api(`/accounts/${encodeURIComponent(id)}/models/discover${query}`, { method: "POST" });
        if (run !== epoch || account?.id !== id)
            return;
        if (!Array.isArray(res.models))
            throw new Error(i18nText("模型列表响应无效"));
        const official = res.source === "official";
        const firstTime = rows.every(r => r.state === "manual");
        const listed = new Set(res.models.map(m => m.id));
        const known = new Map(rows.map(r => [r.id, r]));
        for (const m of res.models) {
            const row = known.get(m.id);
            if (row) {
                if (row.state === "missing")
                    row.state = "saved";
                if (row.label === row.id && m.label)
                    row.label = m.label;
                if (!row.reasoning && m.reasoning) {
                    row.reasoning = m.reasoning;
                    row.source = m.reasoning_source || "";
                }
                continue;
            }
            // First read: everything is selected, so "全选" is the default and the
            // administrator unticks what this account should not offer. The official
            // catalog is not the account's own list: only the CLI's current models.
            const checked = firstTime && (!official || !!m.recommended);
            rows.push({ id: m.id, label: m.label || m.id, reasoning: m.reasoning, source: m.reasoning_source || "", checked, state: official ? "official" : firstTime ? "" : "new" });
        }
        // Only the account's own list can show that a saved model went away.
        if (!official)
            for (const row of rows)
                if (row.state === "saved" && !listed.has(row.id))
                    row.state = "missing";
        const from = res.official ? officialWhere(agent, res.official) : "";
        const where = res.source === "codex_subscription" ? i18nText("ChatGPT 订阅模型目录")
            : res.source === "claude_subscription" ? i18nText("Claude 订阅接口") : official ? from : res.endpoint;
        const lines = [];
        if (res.upstream_error)
            lines.push(i18nText("上游没有返回模型列表（{error}），已改为列出官方模型目录。", { error: res.upstream_error }));
        lines.push(i18nText("已从 {where} 读取 {n} 个模型（{ms}ms）", { where, n: String(res.models.length), ms: String(res.latency_ms) }));
        if (official)
            lines.push(i18nText("官方目录不代表这个账号的上游一定提供，请只保留实际可用的模型。"));
        const filled = res.models.filter(m => m.reasoning_source === "official").length;
        if (!official && filled)
            lines.push(i18nText("{n} 个模型的思考强度按 {where} 补全。", { n: String(filled), where: from }));
        if (res.official?.cli_unavailable)
            lines.push(i18nText("暂时读不到 Agent 镜像里的 CLI 模型目录，已使用内置快照。"));
        summary = lines.join("\n");
        if (res.skipped?.length) {
            const sample = res.skipped.slice(0, 3).join("、") + (res.skipped.length > 3 ? "…" : "");
            summary += "\n" + i18nText("另有 {n} 个模型 ID 含有 CLI 不支持的字符，未列出：{ids}", { n: String(res.skipped.length), ids: sample });
        }
    }
    catch (error) {
        if (run === epoch)
            setError(i18nText("读取模型列表失败：") + error.message);
    }
    finally {
        if (run === epoch) {
            btnDone(button);
            setBusy(false);
        }
    }
}
async function bulkEffort() {
    const targets = bulkTargets();
    if (!account || busy || !targets.length)
        return;
    const run = epoch, same = targets.every(r => JSON.stringify(r.reasoning) === JSON.stringify(targets[0].reasoning));
    const next = await editReasoning(account.type, i18nText("已选的 {n} 个模型 · 思考强度", { n: String(targets.length) }), same ? targets[0].reasoning : undefined);
    if (next === null || run !== epoch)
        return;
    for (const row of targets) {
        row.reasoning = next && { ...next, ...(next.levels ? { levels: [...next.levels] } : {}) };
        row.source = "";
    }
    renderAll();
}
function addManual() {
    const input = $("acct-models-new");
    const id = input.value.trim();
    if (!id)
        return;
    if (!MODEL_ID.test(id)) {
        setError(i18nText("模型 ID 格式不合法（字母数字开头，可含 . _ -）"));
        return;
    }
    setError("");
    const existing = rows.find(r => r.id === id);
    if (existing)
        existing.checked = true;
    else
        rows.push({ id, label: id, source: "", checked: true, state: "manual" });
    input.value = "";
    $("acct-models-filter").value = "";
    renderAll();
    $("acct-models-list").querySelector(`[data-id="${CSS.escape(id)}"]`)?.scrollIntoView({ block: "nearest" });
}
async function save(event) {
    event.preventDefault();
    if (!account || busy)
        return;
    const chosen = rows.filter(r => r.checked);
    if (!chosen.length) {
        if (!account.models?.length) {
            dialog().close();
            return;
        }
        const ok = await askConfirm(() => i18nText("没有勾选任何模型。保存后「{name}」恢复使用系统模型列表，确定吗？", { name: account?.label || "" }), { title: i18nText("恢复系统模型列表"), okLabel: i18nText("恢复") });
        if (!ok || !account)
            return;
    }
    const id = account.id, run = ++epoch, button = $("acct-models-ok");
    setError("");
    btnBusy(button, () => i18nText("保存中…"));
    setBusy(true);
    try {
        const body = {
            models: chosen.map(r => ({ id: r.id, label: r.label, ...(r.reasoning ? { reasoning: r.reasoning } : {}), ...(r.hidden ? { hidden: true } : {}) })),
            default_model: chosen.length ? effectiveDefault() : "",
        };
        // Account overrides were folded into the rows above; keep one source of truth.
        if (chosen.length)
            body.model_reasoning = {};
        await api(`/accounts/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(body) });
        if (run !== epoch)
            return;
        dialog().close();
        toast(chosen.length ? i18nText("已保存 {n} 个可用模型", { n: String(chosen.length) }) : i18nText("已恢复使用系统模型列表"));
        emit("models-updated");
        void refreshAll().catch(e => toast(e.message, true));
    }
    catch (error) {
        if (run === epoch)
            setError(i18nText("保存失败：") + error.message);
    }
    finally {
        if (run === epoch) {
            btnDone(button);
            setBusy(false);
        }
    }
}
function wire() {
    if (wired)
        return;
    wired = true;
    $("acct-models-fetch").addEventListener("click", () => void discover());
    $("acct-models-official").addEventListener("click", () => void discover("official"));
    $("acct-models-add").addEventListener("click", addManual);
    $("acct-models-bulk").addEventListener("click", () => void bulkEffort());
    setTip($("acct-models-bulk"), () => i18nText("把同一套思考强度应用到列表中已勾选的 {n} 个模型，覆盖它们原来的设置。可先用筛选缩小范围。", { n: String(bulkTargets().length) }));
    $("acct-models-new").addEventListener("keydown", e => { if (e.key === "Enter" && !e.isComposing) {
        e.preventDefault();
        addManual();
    } });
    $("acct-models-filter").addEventListener("input", renderAll);
    $("acct-models-all").addEventListener("change", () => {
        const on = $("acct-models-all").checked;
        for (const row of rows)
            if (visible(row))
                row.checked = on;
        renderAll();
    });
    $("acct-models-form").addEventListener("submit", e => void save(e));
    for (const id of ["acct-models-close", "acct-models-cancel"])
        $(id).addEventListener("click", () => { if (!busy)
            dialog().close(); });
    dialog().addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    dialog().addEventListener("close", () => { epoch++; account = null; rows = []; busy = false; });
}
/** discover：打开后立即从上游读取（新建账号认证成功后用）。没有保存过列表时也会自动读取。 */
export function openAccountModels(a, opts = {}) {
    wire();
    epoch++;
    account = a;
    defaultId = a.default_model || "";
    summary = "";
    busy = false;
    rows = (a.models || []).map(m => {
        const override = a.model_reasoning?.[m.id];
        return { id: m.id, label: m.label || m.id, reasoning: override || m.reasoning, source: "", checked: true, state: "saved", hidden: !!m.hidden };
    });
    setTextRender($("acct-models-title"), () => i18nText("{name} · 可用模型", { name: a.label }));
    $("acct-models-filter").value = "";
    $("acct-models-new").value = "";
    setTip($("acct-models-fetch"), () => i18nText("读取 {agent} 账号当前可用的模型，不会改动已保存的选择", { agent: agentName(a.type) }));
    setTip($("acct-models-official"), () => i18nText("列出 {agent} 的官方模型和思考强度：读取 Agent 镜像里 CLI 自带的目录，不需要订阅登录，也不访问上游。已有模型缺少强度时一并补全。", { agent: agentName(a.type) }));
    setError("");
    for (const id of ["acct-models-fetch", "acct-models-official", "acct-models-add", "acct-models-ok"])
        btnDone($(id));
    setBusy(false);
    if (!dialog().open)
        dialog().showModal();
    if (opts.discover || !rows.length)
        void discover();
}
