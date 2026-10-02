/* menu：「⋯」更多操作菜单。工具栏里不该与主操作并排的动作（重命名、删除、清空后上传）
 * 以及一行放不下的次要动作收进这里。弹层挂在 body（或所在的模态框）上用 fixed 定位，
 * 列表行与卡片的 overflow 裁不到它；全站同一时刻只开一个。 */
"use strict";
import { svgIcon } from "./icons.js";
import { hideTip } from "./tip.js";
let current = null;
let dismissedAt = -Infinity;
export function closeMenu(focusButton = false) {
    if (!current)
        return;
    const { pop, btn } = current;
    current = null;
    pop.remove();
    btn?.setAttribute("aria-expanded", "false");
    if (focusButton)
        btn?.focus();
}
/** 刚刚有一下点在菜单外把它收起了：那一下只是收菜单，触摸处不该再当成点击 */
export function menuJustDismissed() { return performance.now() - dismissedAt < 600; }
function place(pop, btn) {
    const r = btn.getBoundingClientRect();
    const w = pop.offsetWidth, h = pop.offsetHeight, gap = 6, edge = 8;
    let left = r.right - w;
    left = Math.max(edge, Math.min(left, innerWidth - w - edge));
    let top = r.bottom + gap;
    if (top + h > innerHeight - edge && r.top - gap - h >= edge)
        top = r.top - gap - h;
    pop.style.left = left + "px";
    pop.style.top = Math.max(edge, top) + "px";
}
function buildMenu(items) {
    const pop = document.createElement("div");
    pop.className = "kebab-menu menu-pop";
    pop.setAttribute("role", "menu");
    const visible = items.filter((it) => !it.hidden);
    visible.forEach((it, i) => {
        if (it.sep && i > 0) {
            const sep = document.createElement("div");
            sep.className = "sep";
            sep.setAttribute("role", "separator");
            pop.append(sep);
        }
        const b = document.createElement("button");
        b.type = "button";
        b.setAttribute("role", "menuitem");
        if (it.danger)
            b.classList.add("danger");
        b.disabled = !!it.disabled;
        if (it.tip)
            b.dataset.tip = it.tip;
        const glyph = document.createElement("span");
        glyph.className = "glyph";
        glyph.append(svgIcon(it.icon, 17));
        b.append(glyph, it.label);
        b.addEventListener("click", (e) => {
            e.stopPropagation();
            closeMenu();
            it.run();
        });
        pop.append(b);
    });
    return pop;
}
export function openMenu(btn, items) {
    const reopen = current?.btn === btn;
    closeMenu();
    if (reopen)
        return; // 再点一次按钮 = 收起
    const pop = buildMenu(items);
    (btn.closest("dialog[open]") || document.body).append(pop);
    btn.setAttribute("aria-expanded", "true");
    hideTip(); // 菜单本身就写明了每一项，按钮的「更多操作」气泡会盖住第一行
    current = { pop, btn };
    place(pop, btn);
    pop.querySelector("button:not(:disabled)")?.focus({ preventScroll: true });
}
/* 在手指长按处弹出（终端里长按粘贴）：浮在手指上方居中，免得被手指挡住。不挪焦点，点菜单项
 * 也不让焦点离开原处——终端输入框一失焦，手机软键盘就收起了。 */
export function openMenuAt(x, y, items) {
    closeMenu();
    const pop = buildMenu(items);
    pop.addEventListener("mousedown", (e) => e.preventDefault());
    document.body.append(pop);
    hideTip();
    current = { pop, btn: null };
    const w = pop.offsetWidth, h = pop.offsetHeight, gap = 14, edge = 8;
    const top = y - gap - h >= edge ? y - gap - h : Math.min(y + gap, innerHeight - h - edge);
    pop.style.left = Math.max(edge, Math.min(x - w / 2, innerWidth - w - edge)) + "px";
    pop.style.top = Math.max(edge, top) + "px";
}
/* 「⋯」按钮：items 每次打开时重新取，禁用/隐藏状态随当前数据走。 */
export function moreButton(items, label = "更多操作") {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn-sm action-control action-icon more-btn";
    btn.setAttribute("aria-label", label);
    btn.setAttribute("aria-haspopup", "menu");
    btn.setAttribute("aria-expanded", "false");
    btn.dataset.tip = label;
    btn.append(svgIcon("more", 16));
    bindMenu(btn, items);
    return btn;
}
/* 给静态按钮（index.html 里写死的 ⋯）挂上同一套菜单。 */
export function bindMenu(btn, items) {
    btn.addEventListener("click", (e) => {
        e.stopPropagation();
        openMenu(btn, items());
    });
}
document.addEventListener("pointerdown", (e) => {
    if (!current)
        return;
    const t = e.target;
    if (current.pop.contains(t) || current.btn?.contains(t))
        return;
    closeMenu();
    dismissedAt = performance.now();
}, true);
window.addEventListener("keydown", (e) => {
    if (!current)
        return;
    if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        closeMenu(true);
        return;
    }
    if (e.key === "Tab") {
        closeMenu();
        return;
    }
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp" && e.key !== "Home" && e.key !== "End")
        return;
    const list = [...current.pop.querySelectorAll("button:not(:disabled)")];
    if (!list.length)
        return;
    e.preventDefault();
    const at = list.indexOf(document.activeElement);
    const next = e.key === "Home" ? 0
        : e.key === "End" ? list.length - 1
            : e.key === "ArrowDown" ? (at + 1) % list.length
                : (at - 1 + list.length) % list.length;
    list[next].focus();
}, true);
// 滚动与改窗口大小时按钮会挪走，菜单留在原处就对不上了，直接收起。
window.addEventListener("resize", () => closeMenu());
document.addEventListener("scroll", (e) => {
    if (current && !current.pop.contains(e.target))
        closeMenu();
}, true);
