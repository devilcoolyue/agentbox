/* 跑腿标签编辑器：管理员标注一个账号模型能不能当跑腿、能接哪些子任务、碰到哪些风险不接。
 * 委派时平台先按这些标签硬过滤，再交给规则或 Jev 选择；系统不会自动改标签。
 * 下方附上这个模型按任务类型统计的历史委派结果，作为调整标签的依据。 */
import { api } from "./api.js";
import { buttonLabel } from "./icons.js";
import { setText, setTextRender, t } from "./i18n.js";
import { S } from "./state.js";
import { setTip } from "./tip.js";
import { riskLabel, taskTypeLabel, tierLabel, usdLabel } from "./features/delegation/labels.js";
export const TASK_TYPES = ["search", "summarize", "mechanical_edit", "implement_spec", "write_tests", "fix_tests", "ui_copy", "docs", "refactor_local", "other"];
export const RISKS = ["auth", "billing", "migration", "concurrency", "security", "data_loss", "public_api", "cross_module", "ambiguous_spec"];
/** 按钮上的短文字；能接哪些任务放在 delegationTip 里，行里放不下。 */
export function delegationSummary(d) {
    if (!d?.enabled)
        return t("不当跑腿");
    return t("跑腿 · {tier}", { tier: tierLabel(d.tier) });
}
export function delegationTip(d, off) {
    if (!d?.enabled)
        return off;
    return [
        delegationSummary(d),
        t("可接：{list}", { list: (d.can || []).map(taskTypeLabel).join("、") }),
        (d.avoid || []).length ? t("不接：{list}", { list: (d.avoid || []).map(riskLabel).join("、") }) : "",
        t("点击修改"),
    ].filter(Boolean).join(" · ");
}
let stats = null;
let statsAt = 0;
async function loadStats() {
    if (S.role !== "admin")
        return [];
    if (stats && Date.now() - statsAt < 30_000)
        return stats;
    try {
        const res = await api("/delegation/stats");
        stats = Array.isArray(res?.stats) ? res.stats : [];
        statsAt = Date.now();
    }
    catch {
        stats = [];
    }
    return stats;
}
function checks(values, chosen = [], label) {
    const box = document.createElement("div");
    box.className = "dgt-checks";
    for (const v of values) {
        const item = document.createElement("label");
        item.className = "check dgt-check";
        const input = Object.assign(document.createElement("input"), { type: "checkbox", value: v, checked: chosen.includes(v) });
        const text = document.createElement("span");
        setTextRender(text, () => label(v));
        item.append(input, text);
        box.append(item);
    }
    return box;
}
const picked = (box) => [...box.querySelectorAll("input:checked")].map(i => i.value);
function statsTable(rows) {
    const wrap = document.createElement("div");
    wrap.className = "dgt-stats";
    if (!rows.length) {
        const p = document.createElement("p");
        p.className = "muted";
        setText(p, "还没有委派记录。开启跑腿后，这里会按任务类型显示主 Agent 的验收结果和花费。");
        wrap.append(p);
        return wrap;
    }
    const table = document.createElement("table");
    const head = document.createElement("tr");
    for (const h of ["任务类型", "次数", "通过", "小修", "重做", "放弃", "未完成", "平均花费"]) {
        const th = document.createElement("th");
        setText(th, h);
        head.append(th);
    }
    table.append(head);
    for (const r of rows) {
        const tr = document.createElement("tr");
        const cells = [taskTypeLabel(r.task_type), r.total, r.ok, r.minor_fix, r.redo, r.abandoned, r.failed, usdLabel(r.total ? Math.round(r.cost_micro_usd / r.total) : 0)];
        for (const c of cells)
            tr.append(Object.assign(document.createElement("td"), { textContent: String(c) }));
        table.append(tr);
    }
    wrap.append(table);
    return wrap;
}
let dialog = null;
/** 打开编辑器。返回新的标签（关闭跑腿时 enabled=false），取消返回 null。 */
export async function editDelegationTags(title, accountID, model, current) {
    if (!dialog) {
        dialog = document.createElement("dialog");
        dialog.className = "dlg-delegation-tags";
        document.body.append(dialog);
    }
    const d = dialog;
    const form = document.createElement("form");
    form.method = "dialog";
    const head = document.createElement("div");
    head.className = "dlg-head";
    const block = document.createElement("div");
    block.className = "dlg-title-block";
    const h = Object.assign(document.createElement("h3"), { textContent: title });
    const sub = document.createElement("p");
    sub.className = "muted";
    setText(sub, "主 Agent 委派子任务时，平台先按这里的标签过滤可用的跑腿；不会根据模型名字推断能力。");
    block.append(h, sub);
    head.append(block);
    const body = document.createElement("div");
    body.className = "dlg-body dgt-body";
    const enable = document.createElement("label");
    enable.className = "check dgt-enable";
    const enableBox = Object.assign(document.createElement("input"), { type: "checkbox", checked: !!current?.enabled });
    const enableText = document.createElement("span");
    setText(enableText, "可以当跑腿模型");
    enable.append(enableBox, enableText);
    const tierRow = document.createElement("label");
    tierRow.className = "dgt-field";
    const tierText = document.createElement("span");
    setText(tierText, "档位");
    const tier = document.createElement("select");
    for (const v of ["economy", "standard"]) {
        const o = Object.assign(document.createElement("option"), { value: v, selected: (current?.tier || "economy") === v });
        setTextRender(o, () => tierLabel(v));
        tier.append(o);
    }
    tierRow.append(tierText, tier);
    setTip(tierRow, () => t("多个跑腿都能接时，不用 Jev 的情况下优先选经济档，同档再比单价。"));
    const canTitle = document.createElement("div");
    canTitle.className = "dgt-title";
    setText(canTitle, "能接的任务类型");
    const can = checks(TASK_TYPES, current?.can, taskTypeLabel);
    const avoidTitle = document.createElement("div");
    avoidTitle.className = "dgt-title";
    setText(avoidTitle, "带这些风险时不接");
    const avoid = checks(RISKS, current?.avoid, riskLabel);
    const notesRow = document.createElement("label");
    notesRow.className = "dgt-field dgt-notes";
    const notesText = document.createElement("span");
    setText(notesText, "备注（交给 Jev 参考，最多 300 字）");
    const notes = Object.assign(document.createElement("textarea"), { rows: 2, maxLength: 300, value: current?.notes || "" });
    notesRow.append(notesText, notes);
    const statsTitle = document.createElement("div");
    statsTitle.className = "dgt-title";
    setText(statsTitle, "历史委派结果");
    const statsBox = document.createElement("div");
    statsBox.append(Object.assign(document.createElement("p"), { className: "muted", textContent: t("正在读取…") }));
    const error = document.createElement("p");
    error.className = "dgt-error hidden";
    body.append(enable, tierRow, canTitle, can, avoidTitle, avoid, notesRow, statsTitle, statsBox, error);
    const actions = document.createElement("div");
    actions.className = "dlg-actions";
    const cancel = Object.assign(document.createElement("button"), { type: "button", className: "btn" });
    buttonLabel(cancel, () => t("取消"), "close");
    const ok = Object.assign(document.createElement("button"), { type: "submit", className: "btn btn-primary" });
    buttonLabel(ok, () => t("确定"), "check");
    actions.append(cancel, ok);
    form.append(head, body, actions);
    d.replaceChildren(form);
    const sync = () => {
        const on = enableBox.checked;
        for (const el of [tier, ...can.querySelectorAll("input"), ...avoid.querySelectorAll("input"), notes])
            el.disabled = !on;
    };
    enableBox.addEventListener("change", sync);
    sync();
    void loadStats().then(all => {
        if (!d.open)
            return;
        statsBox.replaceChildren(statsTable(all.filter(s => s.account_id === accountID && s.model === model)));
    });
    return new Promise(resolve => {
        let result = null;
        form.addEventListener("submit", event => {
            const on = enableBox.checked, chosen = picked(can);
            if (on && !chosen.length) {
                event.preventDefault();
                setText(error, "开启跑腿时至少选择一种能接的任务类型。");
                error.classList.remove("hidden");
                return;
            }
            result = on
                ? { enabled: true, tier: tier.value, can: chosen, avoid: picked(avoid), ...(notes.value.trim() ? { notes: notes.value.trim() } : {}) }
                : { ...(current || {}), enabled: false };
        });
        cancel.addEventListener("click", () => d.close());
        d.addEventListener("close", () => resolve(result), { once: true });
        d.showModal();
    });
}
