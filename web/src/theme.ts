import { setAttrRender, t as i18nText } from "./i18n.js";
/* theme：跟随系统 / 浅色 / 深色三态主题切换。
 * 选择模式落在 <html data-theme-mode>，实际生效主题落在 <html data-theme>；
 * 配色分支全在 css/base.css 的令牌里。首屏那次由 index.html 头部的内联脚本
 * 抢在样式表之前落好属性，否则浅色/系统用户每次刷新都会闪一下深色底。
 * 默认跟随系统；用户切过一次就以 localStorage 里的选择为准。 */
"use strict";

import { setTip } from "./tip.js";

/* 改这个键名时记得同步 index.html 头部那段内联脚本 */
const THEME_KEY = "agentbox_theme";

/** 用户可选的三态；写在 <html data-theme-mode> */
type ThemeMode = "system" | "light" | "dark";
/** 实际生效的配色；写在 <html data-theme>，system 由系统偏好解析而来 */
type Theme = "light" | "dark";

const MODES: ThemeMode[] = ["system", "light", "dark"];
const MODE_LABEL: Record<ThemeMode, string> = { get system() { return i18nText("跟随系统"); }, get light() { return i18nText("浅色"); }, get dark() { return i18nText("深色"); } };

const darkMQ = window.matchMedia("(prefers-color-scheme: dark)");

function storedMode(): ThemeMode {
  try {
    // 断言成 ThemeMode 只为了让 includes 收下它：真值是 string | null，
    // 不在白名单（含 null）时下面照旧回落到 system。
    const mode = localStorage.getItem(THEME_KEY) as ThemeMode;
    return MODES.includes(mode) ? mode : "system";
  } catch {
    return "system";
  }
}

const currentMode = (): ThemeMode =>
  (document.documentElement.dataset.themeMode as ThemeMode | undefined) || storedMode();
const effectiveFor = (mode: ThemeMode): Theme =>
  (mode === "system" ? (darkMQ.matches ? "dark" : "light") : mode);

function apply(mode: ThemeMode, persist = true) {
  if (!MODES.includes(mode)) mode = "system";
  const effective = effectiveFor(mode);
  document.documentElement.dataset.themeMode = mode;
  document.documentElement.dataset.theme = effective;
  if (persist) {
    try {
      localStorage.setItem(THEME_KEY, mode);
    } catch {
      // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
    }
  }
  syncUI(mode);
}

/* 底栏三态按钮反映已保存的选择，收起侧栏时用单个按钮循环切换。 */
function syncUI(mode: ThemeMode) {
  for (const opt of document.querySelectorAll<HTMLElement>("[data-theme-option]")) {
    const active = opt.dataset.themeOption === mode;
    opt.classList.toggle("active", active);
    opt.setAttribute("aria-pressed", String(active));
    setAttrRender(opt, "aria-label", () => i18nText("切换到") + MODE_LABEL[opt.dataset.themeOption as ThemeMode]);
  }
  for (const button of document.querySelectorAll<HTMLElement>("[data-theme-cycle]")) {
    const next = MODES[(MODES.indexOf(mode) + 1) % MODES.length]!;
    const label = () => i18nText("主题：{p0}，切换到{p1}", { p0: String(MODE_LABEL[mode]), p1: String(MODE_LABEL[next]) });
    setAttrRender(button, "aria-label", label);
    setTip(button, label);
  }

  // 移动端浏览器地址栏跟着页面底色走
  const bg = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
  for (const m of document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')) m.content = bg;
}

for (const opt of document.querySelectorAll<HTMLElement>("[data-theme-option]")) {
  opt.addEventListener("click", () => apply(opt.dataset.themeOption as ThemeMode));
}
for (const button of document.querySelectorAll<HTMLElement>("[data-theme-cycle]")) {
  button.addEventListener("click", () => apply(MODES[(MODES.indexOf(currentMode()) + 1) % MODES.length]!));
}

// 系统深浅色变化时，跟随系统模式即时换实际主题；不改变用户已保存的选择。
const onDarkChange = (fn: () => void) => {
  if (darkMQ.addEventListener) darkMQ.addEventListener("change", fn);
  else darkMQ.addListener(fn); // 老 Safari 兜底
};
onDarkChange(() => {
  if (currentMode() === "system") apply("system", false);
});

apply(currentMode(), false);
