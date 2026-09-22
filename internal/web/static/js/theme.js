/* theme：跟随系统 / 浅色 / 深色三态主题切换。
 * 选择模式落在 <html data-theme-mode>，实际生效主题落在 <html data-theme>；
 * 配色分支全在 css/base.css 的令牌里。首屏那次由 index.html 头部的内联脚本
 * 抢在样式表之前落好属性，否则浅色/系统用户每次刷新都会闪一下深色底。
 * 默认跟随系统；用户切过一次就以 localStorage 里的选择为准。 */
"use strict";
import { $ } from "./util.js";
import { setTip } from "./tip.js";
/* 改这个键名时记得同步 index.html 头部那段内联脚本 */
const THEME_KEY = "agentbox_theme";
const MODES = ["system", "light", "dark"];
const MODE_LABEL = { system: "跟随系统", light: "浅色", dark: "深色" };
const darkMQ = window.matchMedia("(prefers-color-scheme: dark)");
const hoverMQ = window.matchMedia("(hover: hover)");
const narrowMQ = window.matchMedia("(max-width: 760px)");
const usesHoverMenu = () => hoverMQ.matches && !narrowMQ.matches;
function storedMode() {
    try {
        // 断言成 ThemeMode 只为了让 includes 收下它：真值是 string | null，
        // 不在白名单（含 null）时下面照旧回落到 system。
        const mode = localStorage.getItem(THEME_KEY);
        return MODES.includes(mode) ? mode : "system";
    }
    catch {
        return "system";
    }
}
const currentMode = () => document.documentElement.dataset.themeMode || storedMode();
const effectiveFor = (mode) => (mode === "system" ? (darkMQ.matches ? "dark" : "light") : mode);
function apply(mode, persist = true) {
    if (!MODES.includes(mode))
        mode = "system";
    const effective = effectiveFor(mode);
    document.documentElement.dataset.themeMode = mode;
    document.documentElement.dataset.theme = effective;
    if (persist) {
        try {
            localStorage.setItem(THEME_KEY, mode);
        }
        catch {
            // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
        }
    }
    syncUI(mode, effective);
}
/* 主钮显示当前模式；菜单三行全列出，当前项加 active（CSS 打勾）。 */
function syncUI(mode, effective) {
    const label = MODE_LABEL[mode];
    const current = mode === "system" ? `${label}（当前${MODE_LABEL[effective]}）` : label;
    const themeLabel = $("theme-label");
    if (themeLabel)
        themeLabel.textContent = label;
    for (const b of document.querySelectorAll("[data-theme-toggle]")) {
        setTip(b, "主题：" + current);
        b.setAttribute("aria-label", "主题：" + current + "，展开主题选项");
    }
    for (const opt of document.querySelectorAll("[data-theme-option]")) {
        const active = opt.dataset.themeOption === mode;
        opt.classList.toggle("active", active);
        opt.setAttribute("aria-current", active ? "true" : "false");
        opt.setAttribute("aria-label", "切换到" + MODE_LABEL[opt.dataset.themeOption]);
        setTip(opt, "切换到" + MODE_LABEL[opt.dataset.themeOption]);
    }
    // 移动端浏览器地址栏跟着页面底色走
    const bg = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
    for (const m of document.querySelectorAll('meta[name="theme-color"]'))
        m.content = bg;
}
/* ---- 弹出菜单：桌面悬停展开，触屏点按展开；移开/点外部/Esc 关闭 ---- */
const switches = [...document.querySelectorAll("[data-theme-switch]")];
function positionMenu(sw) {
    const menu = sw.querySelector(".theme-menu");
    const btn = sw.querySelector("[data-theme-toggle]");
    if (!menu || !btn)
        return;
    if (narrowMQ.matches) {
        menu.style.removeProperty("left");
        menu.style.removeProperty("top");
        return;
    }
    const rect = btn.getBoundingClientRect();
    const gap = 8;
    const mw = menu.offsetWidth;
    const mh = menu.offsetHeight;
    // 侧栏主题钮：菜单默认浮到右侧；贴到屏幕右缘时回落到左侧
    let left = rect.right + gap;
    if (left + mw > window.innerWidth - gap)
        left = rect.left - mw - gap;
    left = Math.max(gap, Math.min(left, window.innerWidth - mw - gap));
    let top = rect.top + rect.height / 2;
    top = Math.max(gap + mh / 2, Math.min(top, window.innerHeight - gap - mh / 2));
    menu.style.left = Math.round(left) + "px";
    menu.style.top = Math.round(top) + "px";
}
function setExpanded(sw, expanded) {
    const btn = sw.querySelector("[data-theme-toggle]");
    if (btn)
        btn.setAttribute("aria-expanded", expanded ? "true" : "false");
}
function closeSwitch(sw) {
    sw.classList.remove("open");
    setExpanded(sw, false);
}
function closeAll(except = null) {
    for (const sw of switches) {
        if (sw !== except)
            closeSwitch(sw);
    }
}
/** 外壳收起/抽屉关闭时同步清除浮层状态。 */
export function closeThemeMenus() { closeAll(); }
function openSwitch(sw) {
    closeAll(sw);
    sw.classList.add("open");
    setExpanded(sw, true);
    positionMenu(sw);
}
function toggleSwitch(sw) {
    if (sw.classList.contains("open"))
        closeSwitch(sw);
    else
        openSwitch(sw);
}
for (const sw of switches) {
    const btn = sw.querySelector("[data-theme-toggle]");
    if (!btn)
        continue;
    // 只有真正具备悬停能力的设备才用 hover 展开；触屏走 click，避免
    // 移动浏览器“先 synthesized hover 再 click”导致刚展开又被点掉。
    sw.addEventListener("pointerenter", () => { if (usesHoverMenu())
        openSwitch(sw); });
    sw.addEventListener("pointerleave", () => { if (usesHoverMenu())
        closeSwitch(sw); });
    sw.addEventListener("focusin", () => { if (usesHoverMenu())
        openSwitch(sw); });
    sw.addEventListener("focusout", (e) => { if (!sw.contains(e.relatedTarget))
        closeSwitch(sw); });
    btn.addEventListener("click", (e) => {
        e.stopPropagation();
        if (usesHoverMenu())
            openSwitch(sw);
        else
            toggleSwitch(sw);
    });
    for (const opt of sw.querySelectorAll("[data-theme-option]")) {
        opt.addEventListener("click", () => {
            apply(opt.dataset.themeOption);
            // 点完选项按钮会留着焦点，:focus-within 会把菜单钉住不消失——
            // 主动失焦，之后只剩 :hover 撑着，鼠标一移出即收起。
            opt.blur();
            closeSwitch(sw);
        });
    }
}
document.addEventListener("pointerdown", (e) => {
    if (!e.target.closest("[data-theme-switch]"))
        closeAll();
});
window.addEventListener("keydown", (e) => { if (e.key === "Escape")
    closeAll(); });
function repositionOpenMenus() {
    for (const sw of switches) {
        if (sw.classList.contains("open"))
            positionMenu(sw);
    }
}
window.addEventListener("resize", repositionOpenMenus);
window.addEventListener("scroll", repositionOpenMenus, true);
// 桌面菜单有内联坐标；切成抽屉时即使菜单已关闭，也必须清掉，
// 否则这些坐标会撑大侧栏的滚动范围。
narrowMQ.addEventListener("change", () => {
    closeAll();
    for (const sw of switches)
        positionMenu(sw);
});
// 系统深浅色变化时，跟随系统模式即时换实际主题；不改变用户已保存的选择。
const onDarkChange = (fn) => {
    if (darkMQ.addEventListener)
        darkMQ.addEventListener("change", fn);
    else
        darkMQ.addListener(fn); // 老 Safari 兜底
};
onDarkChange(() => {
    if (currentMode() === "system")
        apply("system", false);
});
apply(currentMode(), false);
