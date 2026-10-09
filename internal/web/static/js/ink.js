/* ink：滑动指示条。标签下划线、二级导航与分段按钮的选中底块，切换时从旧位置滑到新位置。
 * 选中态仍由各模块自己切 .active，这里只盯着宿主子树的 class 变化，量出选中项的位置
 * 写进宿主的 --ink-x/y/w/h；条的外观（下划线还是底块）由各组件的样式决定，见 base.css。
 * 宿主隐藏时量不到尺寸，条先收起；重新显示时 ResizeObserver 再量一次，这一次直接落位
 * 不做过渡，免得条从角落滑进来。 */
const HOSTS = ".tabs, .set-nav, .scope-switch, .changes-view-tabs, .auth-modes";
export function trackInk(host) {
    if (host.hasAttribute("data-ink"))
        return;
    host.setAttribute("data-ink", "");
    const ink = document.createElement("span");
    ink.className = "ink";
    ink.setAttribute("aria-hidden", "true");
    host.append(ink);
    let placed = false; // 上一次量到了有效位置：只有这时才让条滑过去
    const update = () => {
        const item = host.querySelector(":scope > .active");
        if (!item || !item.offsetWidth || !host.offsetWidth) {
            placed = false;
            host.removeAttribute("data-ink-ready");
            return;
        }
        if (!placed)
            host.setAttribute("data-ink-still", "");
        host.style.setProperty("--ink-x", item.offsetLeft + "px");
        host.style.setProperty("--ink-y", item.offsetTop + "px");
        host.style.setProperty("--ink-w", item.offsetWidth + "px");
        host.style.setProperty("--ink-h", item.offsetHeight + "px");
        host.setAttribute("data-ink-ready", "");
        if (!placed) {
            void ink.offsetWidth; // 先在无过渡状态下落位，再恢复过渡
            host.removeAttribute("data-ink-still");
            placed = true;
        }
    };
    // 文字随语言、计数变宽变窄，宿主不一定跟着变：每个选项都要盯
    const sizes = new ResizeObserver(update);
    const observeSizes = () => {
        sizes.disconnect();
        sizes.observe(host);
        for (const child of host.children)
            if (child !== ink)
                sizes.observe(child);
    };
    new MutationObserver((records) => {
        if (records.some((r) => r.type === "childList" && r.target === host))
            observeSizes();
        update();
    }).observe(host, { subtree: true, childList: true, attributes: true, attributeFilter: ["class"] });
    observeSizes();
    update();
}
/** 给页面里写死的宿主统一挂上；脚本动态生成的分段按钮在生成处调 trackInk。 */
export function initInk(root = document) {
    for (const host of root.querySelectorAll(HOSTS))
        trackInk(host);
}
