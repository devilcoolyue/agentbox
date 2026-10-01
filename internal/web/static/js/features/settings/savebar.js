import { $, askConfirm } from "../../util.js";
import { setSelectValue } from "../../select.js";
const groups = [];
const baseline = new Map();
function valueOf(id) {
    const el = document.getElementById(id);
    if (!el)
        return "";
    if (el instanceof HTMLInputElement && el.type === "checkbox")
        return String(el.checked);
    return el.value;
}
export function defineGroups(list) {
    groups.splice(0, groups.length, ...list);
}
/** 以当前表单值为基准。不传 ids 时整页重置（载入、保存、放弃之后）。 */
export function rebaseline(ids) {
    const all = ids || groups.flatMap((g) => g.fields);
    for (const id of all)
        baseline.set(id, valueOf(id));
    renderBar();
}
/* 别处的即时保存（模型列表、默认模型）也会用服务端返回值重填整页表单。
 * 重填前先记下还没保存的改动，重填后放回去，不让它们被悄悄冲掉。 */
export function holdDirty() {
    const held = new Map();
    for (const g of dirtyGroups())
        for (const id of g.fields)
            held.set(id, valueOf(id));
    return () => {
        for (const [id, v] of held) {
            const el = document.getElementById(id);
            if (el instanceof HTMLInputElement && el.type === "checkbox")
                el.checked = v === "true";
            else if (el instanceof HTMLSelectElement)
                setSelectValue(el, v);
            else if (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement)
                el.value = v;
        }
        renderBar();
    };
}
export function dirtyGroups() {
    return groups.filter((g) => g.fields.some((id) => baseline.has(id) && valueOf(id) !== baseline.get(id)));
}
export function renderBar() {
    const dirty = dirtyGroups();
    const bar = $("set-savebar");
    bar.hidden = !dirty.length;
    $("set-savebar-text").textContent = dirty.length ? "未保存的修改：" + dirty.map((g) => g.label).join("、") : "";
}
/** 合并所有改动过的组，一次提交。put 由设置页提供（负责请求、重填表单与提示）。 */
export async function saveDirty(put) {
    const dirty = dirtyGroups();
    if (!dirty.length)
        return;
    const patch = {};
    for (const g of dirty) {
        const part = g.patch();
        if (!part)
            return; // 校验没过：控件已经弹出提示，整次不提交
        Object.assign(patch, part);
    }
    const saved = await put(patch, "已保存：" + dirty.map((g) => g.label).join("、"));
    if (saved)
        for (const g of dirty)
            g.done?.(saved);
}
/** 有未保存的改动时先确认；确认放弃则调 discard 还原表单。 */
export async function confirmDiscard(discard, action = "切换分区") {
    const dirty = dirtyGroups();
    if (!dirty.length)
        return true;
    const ok = await askConfirm(`「${dirty.map((g) => g.label).join("、")}」有未保存的修改，${action}后会丢失。`, {
        title: "放弃未保存的修改？", okLabel: "放弃修改", icon: "undo", danger: true,
    });
    if (ok)
        discard();
    return ok;
}
export function bindSaveBar(root, signal, onSave, onDiscard) {
    root.addEventListener("input", renderBar, { signal });
    root.addEventListener("change", renderBar, { signal });
    $("set-save").addEventListener("click", onSave, { signal });
    $("set-discard").addEventListener("click", onDiscard, { signal });
    window.addEventListener("beforeunload", (e) => {
        if (dirtyGroups().length)
            e.preventDefault();
    }, { signal });
}
