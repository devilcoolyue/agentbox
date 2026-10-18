/* loading-bar：页面顶部的加载细线。
 *
 * 只跟踪「人刚做了操作」引起的请求：点击、回车、提交、切换路由后 1 秒内发起的
 * api()，以及页面刚打开时的首批请求。后台轮询（会话列表、扩展状态、延迟测量…）
 * 不在这个窗口里，不会让细线隔几秒闪一下。请求 150ms 内就回来的也不显示。
 * 多个请求同时在途时共用一条线，全部结束才收尾。样式在 base.css 的 #top-loader。 */
"use strict";
const ACTION_WINDOW = 1000, SHOW_DELAY = 150, FINISH = 350;
let lastAction = typeof performance === "undefined" ? 0 : performance.now(); // 首次加载算一次操作
let active = 0, showTimer = 0, hideTimer = 0;
let bar = null;
function element() {
    if (!bar) {
        bar = document.createElement("div");
        bar.id = "top-loader";
        bar.setAttribute("aria-hidden", "true"); // 纯视觉提示；各处按钮自己有忙碌状态
        bar.append(document.createElement("span"));
        document.body.append(bar);
    }
    return bar;
}
export function initLoadingBar() {
    const mark = () => { lastAction = performance.now(); };
    for (const type of ["click", "submit", "hashchange", "popstate"])
        window.addEventListener(type, mark, { capture: true, passive: true });
    window.addEventListener("keydown", e => { if (e.key === "Enter")
        mark(); }, { capture: true, passive: true });
}
/** 一个请求开始；返回的函数在请求结束时调用（重复调用无副作用）。 */
export function beginRequest() {
    if (typeof document === "undefined" || performance.now() - lastAction > ACTION_WINDOW)
        return () => { };
    if (++active === 1) {
        window.clearTimeout(hideTimer);
        const el = element();
        el.classList.remove("done");
        if (!el.classList.contains("on"))
            showTimer = window.setTimeout(() => el.classList.add("on"), SHOW_DELAY);
    }
    let ended = false;
    return () => {
        if (ended)
            return;
        ended = true;
        if (--active > 0)
            return;
        window.clearTimeout(showTimer);
        if (!bar?.classList.contains("on"))
            return;
        bar.classList.add("done"); // 走满再淡出
        hideTimer = window.setTimeout(() => bar?.classList.remove("on", "done"), FINISH);
    };
}
