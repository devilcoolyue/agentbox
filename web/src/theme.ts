import { setAttrRender, t as i18nText } from "./i18n.js";
/* theme：两个互不相干的维度。
 * - 明暗：跟随系统 / 浅色 / 深色三态。选择落在 <html data-theme-mode>，实际生效的
 *   明暗落在 <html data-theme>。
 * - 风格：琥珀（默认）/ 液态玻璃 / 赛博朋克 / 石墨极简 / 青野绿意 / 工程蓝图，
 *   落在 <html data-skin>；或者一套自定义主题（themes.ts 从服务端读来交给这里）。
 *   自定义主题 = 一个内置风格做底子（data-skin 照旧是它，形状与专属特效跟着它）
 *   + 一组令牌改写，写成 <html> 上的内联 CSS 变量，并记下 data-skin-custom。
 * 配色分支全在 css/base.css（琥珀）与 css/skins.css（其余风格）的令牌里。首屏那次由
 * index.html 头部的内联脚本抢在样式表之前落好属性，否则每次刷新都会闪一下默认配色；
 * 选中的自定义主题因此连同令牌一起缓存在 localStorage，首屏脚本照缓存先铺一遍。
 * 两项偏好都只存在当前浏览器的 localStorage；切过一次就以存下的选择为准。 */
"use strict";

import { setTip } from "./tip.js";

/* 改这三个键名或下面的取值时记得同步 index.html 头部那段内联脚本 */
const THEME_KEY = "agentbox_theme";
const SKIN_KEY = "agentbox_skin";
/** 选中的自定义主题整份缓存（CustomTheme），首屏脚本靠它在模块加载前铺好令牌 */
const CUSTOM_KEY = "agentbox_skin_custom";

/** 用户可选的三态；写在 <html data-theme-mode> */
type ThemeMode = "system" | "light" | "dark";
/** 实际生效的配色；写在 <html data-theme>，system 由系统偏好解析而来 */
type Theme = "light" | "dark";
/** 内置界面风格；写在 <html data-skin> */
export type Skin = "amber" | "glass" | "cyberpunk" | "graphite" | "verdant" | "blueprint";

/** 一套自定义主题在这里只需要的部分；key 是「site:<id>」或「user:<id>」 */
export interface CustomTheme {
  key: string;
  name: string;
  base: Skin;
  common?: Record<string, string>;
  dark?: Record<string, string>;
  light?: Record<string, string>;
}

const MODES: ThemeMode[] = ["system", "light", "dark"];
const MODE_LABEL: Record<ThemeMode, string> = { get system() { return i18nText("跟随系统"); }, get light() { return i18nText("浅色"); }, get dark() { return i18nText("深色"); } };
export const SKINS: Skin[] = ["amber", "glass", "cyberpunk", "graphite", "verdant", "blueprint"];
export const SKIN_LABEL: Record<Skin, () => string> = {
  amber: () => i18nText("琥珀"),
  glass: () => i18nText("液态玻璃"),
  cyberpunk: () => i18nText("赛博朋克"),
  graphite: () => i18nText("石墨极简"),
  verdant: () => i18nText("青野绿意"),
  blueprint: () => i18nText("工程蓝图"),
};

const darkMQ = window.matchMedia("(prefers-color-scheme: dark)");
const isSkin = (value: unknown): value is Skin => SKINS.includes(value as Skin);
export const isCustomKey = (value: unknown): value is string => typeof value === "string" && /^(site|user):[a-z0-9][a-z0-9-]{0,39}$/.test(value);

/* 令牌名与取值在服务端已按白名单和安全语法校验过；这里再挡一道，缓存被别的脚本
 * 改过或服务端出错时也只会落成无效变量，不会变成别的声明。与 index.html 同一条规则。 */
const TOKEN_RE = /^--[a-z0-9-]+$/;
const safeValue = (value: unknown): value is string =>
  typeof value === "string" && value.length <= 1200 && !/[;{}<>\\@!*`]/.test(value) && !/(url|image|image-set|element|cross-fade|src)\s*\(/i.test(value);

function stored<T extends string>(key: string, choices: readonly T[], fallback: T): T {
  try {
    // 断言成 T 只为了让 includes 收下它：真值是 string | null，
    // 不在白名单（含 null）时照旧回落到默认值。
    const value = localStorage.getItem(key) as T;
    return choices.includes(value) ? value : fallback;
  } catch {
    return fallback;
  }
}
function storedSkin(): string {
  try {
    const value = localStorage.getItem(SKIN_KEY);
    return isSkin(value) || isCustomKey(value) ? value : "amber";
  } catch {
    return "amber";
  }
}
function save(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // 隐私模式下 localStorage 可能不可写：本次会话仍生效，只是记不住
  }
}
function forget(key: string) {
  try { localStorage.removeItem(key); } catch { /* 同上 */ }
}

const tokenMap = (value: unknown): Record<string, string> | undefined => {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const out: Record<string, string> = {};
  for (const [name, v] of Object.entries(value)) if (TOKEN_RE.test(name) && safeValue(v)) out[name] = v;
  return out;
};
/** 缓存里的自定义主题；只认 key 对得上的那份 */
function cachedCustom(key: string): CustomTheme | undefined {
  try {
    const raw: unknown = JSON.parse(localStorage.getItem(CUSTOM_KEY) || "null");
    if (!raw || typeof raw !== "object") return undefined;
    const c = raw as Record<string, unknown>;
    if (c.key !== key || !isSkin(c.base) || typeof c.name !== "string") return undefined;
    return { key, name: c.name, base: c.base, common: tokenMap(c.common), dark: tokenMap(c.dark), light: tokenMap(c.light) };
  } catch {
    return undefined;
  }
}

/* themes.ts 交来的自定义主题。authoritative = 这份列表是刚从服务端成功读到的：
 * 只有这时「选中的主题不在列表里」才说明它真的没了（被删、或换了账号），
 * 读失败或已退出登录时继续用缓存，不把用户的选择冲掉。 */
let customs: CustomTheme[] = [];
let customsAuthoritative = false;
/* 主题编辑器的预览：盖在当前选择之上，只写页面不写存储，也不改用户的选择与明暗偏好；
 * mode 让编辑器单独切深浅色检查两套令牌。结束预览（null）就回到原来的样子。 */
let preview: { theme: CustomTheme; mode?: Theme } | null = null;

const root = document.documentElement;
/** 当前选择：内置风格名或自定义主题的 key */
let selection = storedSkin();
/** 当前写在 <html> 上的内联令牌；首屏脚本按缓存写过的那些也算在内 */
let appliedProps: string[] = (() => {
  const c = isCustomKey(selection) ? cachedCustom(selection) : undefined;
  return c ? [...new Set(Object.keys({ ...c.common, ...c.dark, ...c.light }))] : [];
})();
let appliedSignature = "";

const currentMode = (): ThemeMode => (root.dataset.themeMode as ThemeMode | undefined) || stored(THEME_KEY, MODES, "system");
/** 当前选择（内置风格名或自定义主题 key），风格菜单用它标选中项 */
export const currentSkin = (): string => selection;
const effectiveFor = (mode: ThemeMode): Theme =>
  (mode === "system" ? (darkMQ.matches ? "dark" : "light") : mode);

function resolve(choice: string): { skin: Skin; custom?: CustomTheme } {
  if (isSkin(choice)) return { skin: choice };
  if (isCustomKey(choice)) {
    const found = customs.find(t => t.key === choice) ?? (customsAuthoritative ? undefined : cachedCustom(choice));
    if (found) return { skin: found.base, custom: found };
  }
  return { skin: "amber" };
}

function writeTokens(custom: CustomTheme | undefined, theme: Theme) {
  for (const name of appliedProps) root.style.removeProperty(name);
  appliedProps = [];
  if (!custom) {
    delete root.dataset.skinCustom;
    return "";
  }
  const vars = { ...custom.common, ...custom[theme] };
  for (const [name, value] of Object.entries(vars)) {
    if (!TOKEN_RE.test(name) || !safeValue(value)) continue;
    root.style.setProperty(name, value);
    appliedProps.push(name);
  }
  root.dataset.skinCustom = custom.key;
  return JSON.stringify(vars);
}

function apply(mode: ThemeMode, choice: string, persist = true) {
  if (!MODES.includes(mode)) mode = "system";
  let { skin, custom } = resolve(choice);
  // 选的是自定义主题却解析不到（已删除或换了账号）：落回琥珀
  if (!custom && !isSkin(choice)) choice = "amber";
  selection = choice;
  let theme = effectiveFor(mode);
  if (preview) {
    custom = preview.theme;
    skin = custom.base;
    theme = preview.mode ?? theme;
    persist = false;
  }
  root.dataset.themeMode = mode;
  root.dataset.theme = theme;
  root.dataset.skin = skin;
  const signature = [theme, skin, custom?.key ?? "", writeTokens(custom, theme)].join("|");
  // 模块加载时那一次只是接过首屏脚本铺好的状态，不算变化
  const changed = appliedSignature !== "" && signature !== appliedSignature;
  appliedSignature = signature;
  if (persist) {
    save(THEME_KEY, mode);
    save(SKIN_KEY, choice);
    if (custom) save(CUSTOM_KEY, JSON.stringify(custom));
    else forget(CUSTOM_KEY);
  }
  syncUI(mode);
  // 终端等按令牌取色、创建后不再读样式的组件靠这个事件刷新配色
  if (changed) window.dispatchEvent(new CustomEvent("agentbox-theme-change", { detail: { theme, skin, custom: custom?.key } }));
}

/* 底栏三态按钮反映已保存的选择，收起侧栏时用单个按钮循环切换；风格在用户弹层里选。 */
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
  const bg = getComputedStyle(root).getPropertyValue("--bg").trim();
  for (const m of document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')) m.content = bg;
}

/** 用户弹层的风格选择（shell.ts 的 pref-picker）与主题管理调用；选中态靠 agentbox-theme-change 回写 */
export function setSkin(choice: string) {
  apply(currentMode(), isSkin(choice) || isCustomKey(choice) ? choice : "amber");
}

/** 风格菜单的一项 */
export interface SkinChoice {
  value: string;
  label: () => string;
  swatch: string;
  group?: () => string;
}

/* 自定义主题的色卡取它深色（没有就浅色）的底色与强调色；取不到或引用了别的令牌
 * （var() 在色卡里会解析成当前页面的值，看着像别的主题）就用底子风格的色卡。 */
export function customSwatch(t: CustomTheme) {
  const pick = (name: string) => t.dark?.[name] ?? t.common?.[name] ?? t.light?.[name];
  const bg = pick("--bg"), accent = pick("--accent");
  if (!bg || !accent || /var\(/i.test(bg + accent)) return `var(--swatch-${t.base})`;
  return `linear-gradient(135deg, ${bg} 50%, ${accent} 51%)`;
}

/** 风格菜单的全部选项：内置风格在前，全站主题、我的主题各成一组 */
export function skinChoices(): SkinChoice[] {
  const builtin: SkinChoice[] = SKINS.map(skin => ({ value: skin, label: SKIN_LABEL[skin], swatch: `var(--swatch-${skin})` }));
  const group = (scope: string) => scope === "site" ? () => i18nText("全站主题") : () => i18nText("我的主题");
  return builtin.concat(customs.map(t => ({ value: t.key, label: () => t.name, swatch: customSwatch(t), group: group(t.key.split(":")[0]!) })));
}

/** 主题编辑器预览草稿（null 结束预览）；取值同样过 safeValue，草稿里的坏值只是不生效 */
export function setThemePreview(next: { theme: CustomTheme; mode?: Theme } | null) {
  preview = next && isSkin(next.theme.base) ? next : null;
  apply(currentMode(), selection, false);
}
/** 当前实际生效的明暗（预览时是预览的明暗） */
export const effectiveTheme = (): Theme => (root.dataset.theme === "light" ? "light" : "dark");

/** themes.ts 读到（或清空）自定义主题时调用；会按新内容重铺当前主题并通知风格菜单重建 */
export function setCustomThemes(list: CustomTheme[], authoritative: boolean) {
  customs = list.filter(t => isCustomKey(t.key) && isSkin(t.base));
  customsAuthoritative = authoritative;
  const current = isCustomKey(selection) ? customs.find(t => t.key === selection) : undefined;
  if (isCustomKey(selection) && authoritative) {
    // 选中的主题还在：内容可能被管理员改过，刷新缓存；不在了就落回琥珀并记住
    if (current) save(CUSTOM_KEY, JSON.stringify(current));
    apply(currentMode(), current ? selection : "amber", !current);
  } else {
    apply(currentMode(), selection, false);
  }
  window.dispatchEvent(new CustomEvent("agentbox-themes-change"));
}

for (const opt of document.querySelectorAll<HTMLElement>("[data-theme-option]")) {
  opt.addEventListener("click", () => apply(opt.dataset.themeOption as ThemeMode, selection));
}
for (const button of document.querySelectorAll<HTMLElement>("[data-theme-cycle]")) {
  button.addEventListener("click", () => apply(MODES[(MODES.indexOf(currentMode()) + 1) % MODES.length]!, selection));
}

// 系统深浅色变化时，跟随系统模式即时换实际主题；不改变用户已保存的选择。
const onDarkChange = (fn: () => void) => {
  if (darkMQ.addEventListener) darkMQ.addEventListener("change", fn);
  else darkMQ.addListener(fn); // 老 Safari 兜底
};
onDarkChange(() => {
  if (currentMode() === "system") apply("system", selection, false);
});
// 同一浏览器的其他标签页改了偏好，这里跟着换（storage 事件只发给别的页面）。
window.addEventListener("storage", e => {
  if (e.key === THEME_KEY || e.key === SKIN_KEY || e.key === CUSTOM_KEY || e.key === null) {
    apply(stored(THEME_KEY, MODES, "system"), storedSkin(), false);
  }
});

apply(currentMode(), selection, false);
