/* theme：跟随系统 / 浅色 / 深色三态主题切换。
 * 选择模式落在 <html data-theme-mode>，实际生效主题落在 <html data-theme>；
 * 配色分支全在 css/base.css 的令牌里。首屏那次由 index.html 头部的内联脚本
 * 抢在样式表之前落好属性，否则浅色/系统用户每次刷新都会闪一下深色底。
 * 默认跟随系统；用户切过一次就以 localStorage 里的选择为准。 */
"use strict";
import { setTip } from "./tip.js";
/* 改这个键名时记得同步 index.html 头部那段内联脚本 */
const THEME_KEY = "agentbox_theme";
const MODES = ["system", "light", "dark"];
const MODE_LABEL = { system: "跟随系统", light: "浅色", dark: "深色" };
const darkMQ = window.matchMedia("(prefers-color-scheme: dark)");
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
    syncUI(mode);
}
/* 底栏三态按钮反映已保存的选择，收起侧栏时用单个按钮循环切换。 */
function syncUI(mode) {
    for (const opt of document.querySelectorAll("[data-theme-option]")) {
        const active = opt.dataset.themeOption === mode;
        opt.classList.toggle("active", active);
        opt.setAttribute("aria-pressed", String(active));
        opt.setAttribute("aria-label", "切换到" + MODE_LABEL[opt.dataset.themeOption]);
    }
    for (const button of document.querySelectorAll("[data-theme-cycle]")) {
        const next = MODES[(MODES.indexOf(mode) + 1) % MODES.length];
        const label = `主题：${MODE_LABEL[mode]}，切换到${MODE_LABEL[next]}`;
        button.setAttribute("aria-label", label);
        setTip(button, label);
    }
    // 移动端浏览器地址栏跟着页面底色走
    const bg = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
    for (const m of document.querySelectorAll('meta[name="theme-color"]'))
        m.content = bg;
}
for (const opt of document.querySelectorAll("[data-theme-option]")) {
    opt.addEventListener("click", () => apply(opt.dataset.themeOption));
}
for (const button of document.querySelectorAll("[data-theme-cycle]")) {
    button.addEventListener("click", () => apply(MODES[(MODES.indexOf(currentMode()) + 1) % MODES.length]));
}
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
