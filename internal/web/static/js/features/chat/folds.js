import { svgIcon } from "../../icons.js";
import { setText, setTextRender, t as i18nText } from "../../i18n.js";
import { el } from "./tool-rows.js";
const labels = new WeakMap();
function newFold(kind, icon, text) {
    const wrap = el("div", "fold fold-" + kind);
    const head = el("div", "fold-head");
    head.tabIndex = 0;
    head.setAttribute("role", "button");
    head.setAttribute("aria-expanded", "false");
    const label = el("span", "fold-text");
    const items = el("div", "fold-items");
    head.append(svgIcon(icon, 13), label);
    wrap.append(head, items);
    labels.set(wrap, text);
    const toggle = () => {
        const open = !wrap.classList.contains("fold-open");
        wrap.classList.toggle("fold-open", open);
        head.setAttribute("aria-expanded", String(open));
    };
    head.addEventListener("click", toggle);
    head.addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        toggle();
    } });
    return wrap;
}
/* 条数变了重新绑定标题（重新绑定会立即重画，语言切换时也按它重算） */
function recount(fold) {
    const label = fold.querySelector(":scope > .fold-head > .fold-text"), text = labels.get(fold);
    const n = fold.querySelector(":scope > .fold-items").childElementCount;
    if (label && text)
        setTextRender(label, () => text(n));
}
/* ---------------- 思考过程 ---------------- */
const isThink = (n) => !!n && n.matches("details.think:not(.streaming)");
/* 列表里每段用它的第一行当标题，点开看全文 */
function asItem(think) {
    think.open = false;
    const sum = think.querySelector(":scope > summary");
    if (sum && !sum.querySelector(".think-peek")) {
        const first = (think.querySelector(".think-body")?.textContent || "").trim().split("\n")[0].replace(/^\*\*(.+?)\*\*$/, "$1").trim();
        if (first)
            sum.append(el("span", "think-peek", first));
    }
    return think;
}
function thinkFold(first) {
    const fold = newFold("think", "brain", n => i18nText("思考过程 · {p0} 段", { p0: String(n) }));
    first.replaceWith(fold);
    fold.querySelector(".fold-items").append(asItem(first));
    recount(fold);
    return fold;
}
/** 回放放完的思考块：前面紧挨着思考就并进去 */
export function foldThink(node) {
    if (!isThink(node))
        return;
    const prev = node.previousElementSibling;
    if (isThink(prev))
        thinkFold(prev);
    const group = node.previousElementSibling;
    if (group?.classList.contains("fold-think")) {
        group.querySelector(".fold-items").append(asItem(node));
        relabel(group);
    }
}
/* ---------------- 工具心跳 ---------------- */
function secs(s) {
    return s < 60 ? s + "s" : Math.floor(s / 60) + "m " + (s % 60) + "s";
}
/** tool_progress → 只有一条的心跳组；紧跟在另一个心跳组后面时由 foldInto 并进去 */
export function heartbeatNode(ev, title) {
    const fold = newFold("beat", "heart-pulse", n => i18nText("心跳检查 · {p0} 次", { p0: String(n) }));
    const item = document.createElement("details");
    item.className = "fold-item beat";
    const sum = document.createElement("summary");
    const elapsed = typeof ev.elapsed_time_seconds === "number" ? ev.elapsed_time_seconds : undefined;
    const what = el("span", "beat-what", title);
    const time = el("span", "beat-time");
    if (elapsed !== undefined)
        setText(time, "已运行 {p0}", { p0: secs(elapsed) });
    sum.append(what, time);
    const pre = el("pre", "beat-raw mono", JSON.stringify(ev, null, 2));
    item.append(sum, pre);
    item.dataset.elapsed = elapsed !== undefined ? String(elapsed) : "";
    fold.querySelector(".fold-items").append(item);
    relabel(fold);
    return fold;
}
/* 心跳组标题后跟最近一次的已运行时长 */
function relabel(fold) {
    recount(fold);
    if (!fold.classList.contains("fold-beat"))
        return;
    const head = fold.querySelector(":scope > .fold-head");
    const last = fold.querySelector(":scope > .fold-items > .fold-item:last-child");
    const elapsed = Number(last?.dataset.elapsed || NaN);
    let meta = head.querySelector(".fold-meta");
    if (!Number.isFinite(elapsed)) {
        meta?.remove();
        return;
    }
    if (!meta) {
        meta = el("span", "fold-meta");
        head.append(meta);
    }
    const what = last?.querySelector(".beat-what")?.textContent || "";
    setTextRender(meta, () => " · " + (what ? what + " " : "") + i18nText("已运行 {p0}", { p0: secs(elapsed) }));
}
/* ---------------- 入口 ---------------- */
/** 新节点能并进紧挨着的前一个节点就并进去，返回 true（调用方不再插入它） */
export function foldInto(prev, node) {
    if (!prev)
        return false;
    if (node.classList.contains("fold-beat") && prev.classList.contains("fold-beat")) {
        prev.querySelector(".fold-items").append(...node.querySelector(".fold-items").children);
        relabel(prev);
        return true;
    }
    if (!isThink(node))
        return false;
    if (isThink(prev))
        prev = thinkFold(prev);
    if (!prev.classList.contains("fold-think"))
        return false;
    prev.querySelector(".fold-items").append(asItem(node));
    relabel(prev);
    return true;
}
