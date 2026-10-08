import { setAttrRender, setTextRender, t as i18nText } from "./i18n.js";
/* theme：两个互不相干的维度。
 * - 明暗：跟随系统 / 浅色 / 深色三态。选择落在 <html data-theme-mode>，实际生效的
 *   明暗落在 <html data-theme>。
 * - 风格：琥珀（默认）/ 液态玻璃 / 赛博朋克 / 石墨极简 / 青野绿意 / 工程蓝图，
 *   落在 <html data-skin>。
 * 配色分支全在 css/base.css（琥珀）与 css/skins.css（其余风格）的令牌里。首屏那次由
 * index.html 头部的内联脚本抢在样式表之前落好属性，否则每次刷新都会闪一下默认配色。
 * 两项偏好都只存在当前浏览器的 localStorage；切过一次就以存下的选择为准。 */
"use strict";
import { setTip } from "./tip.js";
/* 改这两个键名或下面的取值时记得同步 index.html 头部那段内联脚本 */
const THEME_KEY = "agentbox_theme";
const SKIN_KEY = "agentbox_skin";
const MODES = ["system", "light", "dark"];
const MODE_LABEL = { get system() { return i18nText("跟随系统"); }, get light() { return i18nText("浅色"); }, get dark() { return i18nText("深色"); } };
const SKINS = ["amber", "glass", "cyberpunk", "graphite", "verdant", "blueprint"];
const SKIN_LABEL = {
    amber: () => i18nText("琥珀"),
    glass: () => i18nText("液态玻璃"),
    cyberpunk: () => i18nText("赛博朋克"),
    graphite: () => i18nText("石墨极简"),
    verdant: () => i18nText("青野绿意"),
    blueprint: () => i18nText("工程蓝图"),
};
const darkMQ = window.matchMedia("(prefers-color-scheme: dark)");
function stored(key, choices, fallback) {
    try {
        // 断言成 T 只为了让 includes 收下它：真值是 string | null，
        // 不在白名单（含 null）时照旧回落到默认值。
        const value = localStorage.getItem(key);
        return choices.includes(value) ? value : fallback;
    }
    catch {
        return fallback;
    }
}
function save(key, value) {
    try {
        localStorage.setItem(key, value);
    }
    catch {
        // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
    }
}
const root = document.documentElement;
const currentMode = () => root.dataset.themeMode || stored(THEME_KEY, MODES, "system");
const currentSkin = () => {
    const skin = root.dataset.skin;
    return skin && SKINS.includes(skin) ? skin : stored(SKIN_KEY, SKINS, "amber");
};
const effectiveFor = (mode) => (mode === "system" ? (darkMQ.matches ? "dark" : "light") : mode);
function apply(mode, skin, persist = true) {
    if (!MODES.includes(mode))
        mode = "system";
    if (!SKINS.includes(skin))
        skin = "amber";
    const theme = effectiveFor(mode);
    const changed = root.dataset.theme !== theme || root.dataset.skin !== skin;
    root.dataset.themeMode = mode;
    root.dataset.theme = theme;
    root.dataset.skin = skin;
    if (persist) {
        save(THEME_KEY, mode);
        save(SKIN_KEY, skin);
    }
    syncUI(mode, skin);
    // 终端等按令牌取色、创建后不再读样式的组件靠这个事件刷新配色
    if (changed)
        window.dispatchEvent(new CustomEvent("agentbox-theme-change", { detail: { theme, skin } }));
}
/* 底栏三态按钮反映已保存的选择，收起侧栏时用单个按钮循环切换；风格下拉在用户菜单里。 */
function syncUI(mode, skin) {
    for (const opt of document.querySelectorAll("[data-theme-option]")) {
        const active = opt.dataset.themeOption === mode;
        opt.classList.toggle("active", active);
        opt.setAttribute("aria-pressed", String(active));
        setAttrRender(opt, "aria-label", () => i18nText("切换到") + MODE_LABEL[opt.dataset.themeOption]);
    }
    for (const button of document.querySelectorAll("[data-theme-cycle]")) {
        const next = MODES[(MODES.indexOf(mode) + 1) % MODES.length];
        const label = () => i18nText("主题：{p0}，切换到{p1}", { p0: String(MODE_LABEL[mode]), p1: String(MODE_LABEL[next]) });
        setAttrRender(button, "aria-label", label);
        setTip(button, label);
    }
    for (const select of skinSelects) {
        if (select.value === skin)
            continue;
        select.value = skin;
        // 只让统一下拉组件刷新显示，不派发 change（那会又调一次 apply）
        select.dispatchEvent(new Event("input", { bubbles: true }));
    }
    // 移动端浏览器地址栏跟着页面底色走
    const bg = getComputedStyle(root).getPropertyValue("--bg").trim();
    for (const m of document.querySelectorAll('meta[name="theme-color"]'))
        m.content = bg;
}
const skinSelects = [...document.querySelectorAll("[data-skin-select]")];
for (const select of skinSelects) {
    for (const skin of SKINS) {
        const option = document.createElement("option");
        option.value = skin;
        setTextRender(option, SKIN_LABEL[skin]);
        select.append(option);
    }
    select.addEventListener("change", () => apply(currentMode(), select.value));
}
for (const opt of document.querySelectorAll("[data-theme-option]")) {
    opt.addEventListener("click", () => apply(opt.dataset.themeOption, currentSkin()));
}
for (const button of document.querySelectorAll("[data-theme-cycle]")) {
    button.addEventListener("click", () => apply(MODES[(MODES.indexOf(currentMode()) + 1) % MODES.length], currentSkin()));
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
        apply("system", currentSkin(), false);
});
// 同一浏览器的其他标签页改了偏好，这里跟着换（storage 事件只发给别的页面）。
window.addEventListener("storage", e => {
    if (e.key === THEME_KEY || e.key === SKIN_KEY || e.key === null) {
        apply(stored(THEME_KEY, MODES, "system"), stored(SKIN_KEY, SKINS, "amber"), false);
    }
});
apply(currentMode(), currentSkin(), false);
