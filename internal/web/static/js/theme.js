/* theme：深色 / 浅色主题切换。
 * 真值就是 <html data-theme>，配色分支全在 css/base.css 的令牌里；本模块只做
 * 三件事：落属性、记住选择、同步按钮文案。首屏那次由 index.html 头部的内联
 * 脚本抢在样式表之前落好属性，否则浅色用户每次刷新都会闪一下深色底。
 * 默认深色（品牌基调），用户切过一次就以 localStorage 里的选择为准。 */
"use strict";

import { $ } from "./util.js";

/* 改这个键名时记得同步 index.html 头部那段内联脚本 */
const THEME_KEY = "agentbox_theme";

const currentTheme = () => (document.documentElement.dataset.theme === "light" ? "light" : "dark");

function apply(theme) {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(THEME_KEY, theme);
  } catch {
    // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
  }
  syncUI(theme);
}

/* 按钮永远描述「点下去会得到什么」，与 shell.css 里日/月图标的显示逻辑一致 */
function syncUI(theme) {
  const next = theme === "light" ? "深色主题" : "浅色主题";
  $("theme-label").textContent = next;
  for (const b of document.querySelectorAll("[data-theme-toggle]")) {
    b.title = "切换到" + next;
    b.setAttribute("aria-label", "切换到" + next);
  }
  // 移动端浏览器地址栏跟着页面底色走
  const bg = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
  for (const m of document.querySelectorAll('meta[name="theme-color"]')) m.content = bg;
}

for (const b of document.querySelectorAll("[data-theme-toggle]")) {
  b.addEventListener("click", () => apply(currentTheme() === "light" ? "dark" : "light"));
}
syncUI(currentTheme());
