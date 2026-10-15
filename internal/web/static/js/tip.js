import { setAttrRender } from "./i18n.js";
/* tip：全站统一的悬浮提示。一个单例气泡 + document 上的事件委托，替掉原生 title。
 *
 * 换掉原生的理由：系统气泡不认设计令牌（浅色主题下尤其突兀）、冷启动要等半秒、
 * 键盘 focus 时根本不出现、移动端完全没有。
 *
 * 用法：静态结构写 data-tip="文本"，动态代码调 setTip(el, 文本)（传空串即清除）；
 * 可选 data-tip-pos="top|bottom|left|right" 指定首选方向，放不下会自动翻面；
 * 没有动作、只为展示详情的徽标加 data-tip-tap，触屏上点一下就浮出（见文末）。
 * 元素上残留的 title 在悬停时会被静默搬进 data-tip——聊天区渲染的用户内容也可能
 * 带 title，这条兜底保证全站不再冒出原生气泡；iframe 除外，那上面的 title 是
 * 无障碍名称不是提示。
 *
 * 气泡走 Popover API 进顶层：全站弹窗是原生 <dialog> + showModal()，处在 top layer
 * 里，任何 z-index 都压不过，只有同为顶层元素的 popover 才能浮在弹窗内容之上。
 * 老浏览器上属性失效，退化成 z-index 定位，弹窗外一切照常。 */
"use strict";
import { $ } from "./util.js";
import { keyboardInput } from "./modality.js";
const OPEN_DELAY = 90; // 冷启动延迟：够滤掉随手划过，又远快于原生的半秒起
const WARM_MS = 260; // 上一个刚收起，这段时间内换目标算「连读」，零延迟接上
const TOUCH_HOLD = 420; // 触屏长按多久浮出
const TOUCH_LINGER = 2600; // 触屏浮出后自己收起
const EDGE = 8; // 气泡离视口边缘的最小留白
const GAP = 9; // 气泡与锚点的间距，要容得下箭头（约 5.7px）
const TOUCH_SLOP = 10; // 长按期间手指挪动超过这个距离就当成滑动，取消
/** 只认这两种来源；iframe 的 title 是无障碍名称，不能当提示抢走 */
const SEL = "[data-tip],[title]:not(iframe)";
const SIDES = ["top", "bottom", "right", "left"];
let box;
const tipBox = () => (box ??= $("tip"));
/* Popover 只在支持时用，不支持就是个普通绝对定位的 div */
const canPop = typeof HTMLElement.prototype.showPopover === "function";
let anchor = null; // 当前锚点，含「已定下但还没浮出」的状态
let shown = false; // 气泡是否已经在屏幕上
let lastHide = 0; // 上次收起的时刻，用来判「连读」
let openTimer;
let holdTimer;
let lingerTimer;
let described = null; // 被写过 aria-describedby 的元素
let prevDescribedBy = null;
const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), Math.max(lo, hi));
/* ---- 锚点解析 ---- */
/** 把遗留的原生 title 搬进 data-tip，顺手掐掉浏览器自带的气泡 */
function adopt(el) {
    const t = el.getAttribute("title");
    if (t !== null) {
        el.removeAttribute("title");
        if (t.trim())
            el.dataset.tip = t;
    }
    return el.dataset.tip ? el : null;
}
/** 从节点往上找第一个真的有提示文案的祖先（title 为空的元素要继续往上翻） */
function fromNode(n) {
    for (let e = n?.closest(SEL); e; e = e.parentElement?.closest(SEL) ?? null) {
        const hit = adopt(e);
        if (hit)
            return hit;
    }
    return null;
}
/* 禁用控件不派发鼠标事件（事件目标会退到父元素上），但仍然参与命中测试，所以
 * 事件目标找不到时再按坐标兜一次。「为什么这个按钮是灰的」恰恰最需要提示，
 * 少了这条就是相对原生 title 的倒退。 */
function resolve(target, x, y) {
    return fromNode(target instanceof Element ? target : null)
        ?? fromNode(document.elementFromPoint(x, y));
}
/* ---- 显示 / 收起 ---- */
function describe(el) {
    if (described === el)
        return;
    if (described) {
        if (prevDescribedBy === null)
            described.removeAttribute("aria-describedby");
        else
            described.setAttribute("aria-describedby", prevDescribedBy);
    }
    described = el;
    prevDescribedBy = el ? el.getAttribute("aria-describedby") : null;
    if (el)
        el.setAttribute("aria-describedby", "tip");
}
function show(el) {
    const text = el.dataset.tip;
    if (!text)
        return;
    // 展开着菜单 / 弹层的按钮不再提示「更多操作」，气泡会盖住刚弹出的第一项
    if (el.getAttribute("aria-expanded") === "true")
        return;
    const b = tipBox();
    b.textContent = text;
    anchor = el;
    describe(el);
    const first = !shown;
    shown = true;
    b.classList.add("show"); // 先出 display，才量得到尺寸
    /* 关掉一个 <dialog> 时，UA 会把压在它上面的顶层元素一起收掉，所以别信自己的
     * 标志位，按实际状态补开一次。 */
    if (canPop && !b.matches(":popover-open")) {
        try {
            b.showPopover();
        }
        catch { /* 忽略 */ }
    }
    if (first)
        requestAnimationFrame(() => { if (shown)
            b.classList.add("in"); });
    place();
}
function place() {
    const el = anchor;
    if (!el || !shown)
        return;
    const b = tipBox();
    const r = el.getBoundingClientRect();
    const w = b.offsetWidth;
    const h = b.offsetHeight;
    const vw = document.documentElement.clientWidth;
    const vh = document.documentElement.clientHeight;
    /* 首选方向放不下就按 上→下→右→左 顺次找，全都放不下挑最宽裕的那个 */
    const room = { top: r.top, bottom: vh - r.bottom, left: r.left, right: vw - r.right };
    const need = { top: h, bottom: h, left: w, right: w };
    const want = (el.dataset.tipPos || "top");
    const order = SIDES.includes(want) ? [want, ...SIDES.filter((s) => s !== want)] : SIDES;
    const side = order.find((s) => room[s] >= need[s] + GAP + EDGE)
        ?? order.reduce((a, s) => (room[s] - need[s] > room[a] - need[a] ? s : a), order[0]);
    const vertical = side === "top" || side === "bottom";
    const x = vertical
        ? r.left + r.width / 2 - w / 2
        : side === "left" ? r.left - w - GAP : r.right + GAP;
    const y = vertical
        ? (side === "top" ? r.top - h - GAP : r.bottom + GAP)
        : r.top + r.height / 2 - h / 2;
    const cx = clamp(x, EDGE, vw - w - EDGE);
    const cy = clamp(y, EDGE, vh - h - EDGE);
    b.dataset.side = side;
    b.style.left = Math.round(cx) + "px";
    b.style.top = Math.round(cy) + "px";
    /* 箭头跟着锚点中心走：气泡被夹到视口边缘时也还指得对 */
    b.style.setProperty("--tip-ax", Math.round(clamp(r.left + r.width / 2 - cx, 12, w - 12)) + "px");
    b.style.setProperty("--tip-ay", Math.round(clamp(r.top + r.height / 2 - cy, 12, h - 12)) + "px");
}
function hide() {
    clearTimeout(openTimer);
    openTimer = undefined;
    clearTimeout(lingerTimer);
    lingerTimer = undefined;
    anchor = null;
    describe(null);
    if (!shown)
        return;
    shown = false;
    lastHide = Date.now();
    const b = tipBox();
    b.classList.remove("show", "in");
    if (canPop) {
        try {
            b.hidePopover();
        }
        catch { /* 本来就没开，忽略 */ }
    }
}
/** 悬停目标变了：没提示就收起，有提示就按冷/热启动决定等不等 */
function hover(el) {
    if (el === anchor)
        return;
    clearTimeout(openTimer);
    openTimer = undefined;
    if (!el) {
        hide();
        return;
    }
    anchor = el;
    if (shown || Date.now() - lastHide < WARM_MS)
        show(el);
    else
        openTimer = setTimeout(() => { openTimer = undefined; if (anchor)
            show(anchor); }, OPEN_DELAY);
}
/* ---- 对外 API ---- */
/** 设置/清除元素的提示文案，替代 `el.title = …`；文案为空即清除 */
export function setTip(el, text) {
    const h = el;
    if (typeof text === "function")
        setAttrRender(h, "data-tip", text);
    else if (text)
        h.dataset.tip = text;
    else
        delete h.dataset.tip;
    if (h.hasAttribute("title"))
        h.removeAttribute("title"); // 两套提示别打架
    if (anchor !== h)
        return;
    if (!text)
        hide();
    else if (shown)
        show(h);
}
/** 强制收起。元素被移除、菜单收起等时机可以主动调一下 */
export function hideTip() { hide(); }
/* ---- 事件接线 ---- */
/* 鼠标：pointermove + rAF 节流。用 move 而不是 over，是因为禁用按钮不发事件，
 * 光靠 over 只能在跨过父容器边界那一下才有机会命中。原地不动或目标没变时直接
 * 短路，真正干活的只有「换了元素」或「挪了一小段」这两种情况。 */
let queued = false;
let mx = 0, my = 0, mtarget = null;
let lastX = -1, lastY = -1, lastTarget = null;
document.addEventListener("pointermove", (e) => {
    if (e.pointerType === "touch")
        return;
    mx = e.clientX;
    my = e.clientY;
    mtarget = e.target;
    if (queued)
        return;
    queued = true;
    requestAnimationFrame(() => {
        queued = false;
        if (anchor && !anchor.isConnected)
            hide(); // 锚点被重绘掉了
        const still = mtarget === lastTarget && Math.abs(mx - lastX) < 6 && Math.abs(my - lastY) < 6;
        if (still)
            return;
        lastTarget = mtarget;
        lastX = mx;
        lastY = my;
        hover(resolve(mtarget, mx, my));
    });
}, { passive: true });
/* 鼠标移出窗口就收。手指抬起时浏览器也会对 document 发一次 pointerleave，那不算离开，
 * 不排除的话长按、点按浮出的气泡一松手就没了 */
document.addEventListener("pointerleave", (e) => { if (e.pointerType !== "touch")
    hide(); });
/* 键盘走到带提示的控件上立即显示——原生 title 做不到这件事。手指点开弹窗时 iOS 会把
 * showModal() 给关闭按钮的焦点也算成 :focus-visible，所以还要核对最近一次是不是键盘操作。 */
document.addEventListener("focusin", (e) => {
    const t = e.target;
    if (!keyboardInput() || !t?.matches?.(":focus-visible"))
        return;
    const el = fromNode(t);
    if (!el)
        return;
    clearTimeout(openTimer);
    openTimer = undefined;
    anchor = el;
    show(el);
});
document.addEventListener("focusout", (e) => { if (e.target === anchor)
    hide(); });
/* 只有锚点跟着滚走了才收：对话流式输出时消息列表一直在自动滚，输入框上的气泡不受影响 */
addEventListener("scroll", (e) => {
    if (anchor && e.target instanceof Node && !e.target.contains(anchor))
        return;
    hide();
}, { capture: true, passive: true });
addEventListener("resize", () => hide());
addEventListener("blur", () => hide());
document.addEventListener("keydown", (e) => { if (e.key === "Escape")
    hide(); }, true);
/* 触屏：原生 title 在移动端完全不显示，这里补一条长按通道 */
let tx = 0, ty = 0;
let moved = false;
let wasOpen = null; // 按下前已经浮着的锚点：再点一下是要收起
const cancelHold = () => { clearTimeout(holdTimer); holdTimer = undefined; };
const tapTarget = (t) => t instanceof Element ? t.closest("[data-tip-tap]") : null;
document.addEventListener("pointerdown", (e) => {
    wasOpen = shown ? anchor : null;
    hide(); // 点下去就收起，别让气泡挂在操作结果上面
    cancelHold();
    if (e.pointerType === "mouse")
        return;
    tx = e.clientX;
    ty = e.clientY;
    moved = false;
    if (e.pointerType !== "touch")
        return;
    const el = resolve(e.target, e.clientX, e.clientY);
    if (!el)
        return;
    holdTimer = setTimeout(() => {
        holdTimer = undefined;
        anchor = el;
        show(el);
        lingerTimer = setTimeout(() => hide(), TOUCH_LINGER);
    }, TOUCH_HOLD);
}, true);
/* 点按浮出：上下文占用这类纯信息徽标点了没有反应，长按又没人想得到，所以带
 * data-tip-tap 的元素用手指或笔点一下就浮出、再点一下收起，点别处或页面滚动照常收起，
 * 不自动消失（要留时间读完）。鼠标仍走悬停。用 pointerup 而不是 click：iOS 只给它
 * 认为可点的元素派发 click。 */
document.addEventListener("pointerup", (e) => {
    cancelHold();
    if (e.pointerType === "mouse" || moved)
        return;
    const el = tapTarget(e.target);
    if (!el?.dataset.tip)
        return;
    if (shown && anchor === el) {
        clearTimeout(lingerTimer);
        lingerTimer = undefined;
        return;
    } // 长按已经浮出，松手后留着
    if (wasOpen === el)
        return; // 第二下：按下时已经收起了
    anchor = el;
    show(el);
}, true);
/* 不抢焦点：手机上正开着软键盘打字时点一下看详情，键盘不该收起（收起会重排、把气泡挤走） */
document.addEventListener("mousedown", (e) => { if (tapTarget(e.target))
    e.preventDefault(); }, true);
document.addEventListener("pointercancel", cancelHold, true);
document.addEventListener("pointermove", (e) => {
    if (e.pointerType === "mouse" || Math.abs(e.clientX - tx) + Math.abs(e.clientY - ty) <= TOUCH_SLOP)
        return;
    moved = true;
    cancelHold();
}, { capture: true, passive: true });
window.addEventListener("agentbox-language-change", () => { if (shown && anchor)
    show(anchor); });
