import { i18n, setTextRender, t as i18nText } from "./i18n.js";
/* themes：界面主题管理（用户弹层「风格 → 管理主题…」打开的弹窗）。
 *
 * 主题是一份 JSON 清单：选一个内置风格做底子，再改写白名单里的令牌（颜色、投影、
 * 圆角、字体）。服务端（internal/theme）严格校验令牌名与取值语法，这里只负责：
 * - 登录后读 /api/themes，把全站主题与个人主题交给 theme.ts，风格菜单随之出现；
 * - 导入（读本地文件原样提交，服务端校验）、导出、删除、启用；
 * - 把内置风格导出成写全两套令牌的模板：直接从已加载的 base.css / skins.css 规则里取值；
 * - 导入后按 WCAG 对比度提醒正文、按钮文字是否看不清（只提醒，不拦）。
 * 随登录由 app/lifecycle 挂载，退出时清掉个人主题列表、关掉弹窗、作废在途请求。 */
"use strict";

import { api } from "./api.js";
import { bus } from "./state.js";
import { $, askConfirm } from "./util.js";
import { actionButton } from "./icons.js";
import { APIError } from "./problems.js";
import { SKINS, SKIN_LABEL, currentSkin, customSwatch, setCustomThemes, setSkin } from "./theme.js";
import type { CustomTheme, Skin } from "./theme.js";
import type { ThemeManifest, ThemeProblem, ThemesResponse, ThemeToken, ThemeView } from "./types.js";

type Scope = "site" | "user";
type Mode = "dark" | "light";

const DEFAULT_LIMITS = { site: 50, user: 20, bytes: 64 << 10 };
/** 两种明暗都一样、放进 common 的令牌种类；颜色类按明暗各写一份 */
const COMMON_KINDS = new Set(["scale", "radius", "font"]);

let data: ThemesResponse | null = null;
/** 每次读列表 +1，只认最新一次的结果 */
let generation = 0;
/** 每次退出登录 +1：迟到的导入 / 删除结果不能落到下一个登录的用户身上 */
let epoch = 0;
let controller: AbortController | undefined;
let busy = false;
let pendingScope: Scope = "user";

const dialog = () => $<HTMLDialogElement>("dlg-themes");
const isSkin = (value: unknown): value is Skin => SKINS.includes(value as Skin);
const keyOf = (view: ThemeView) => `${view.scope}:${view.manifest.id}`;

function toCustom(view: ThemeView): CustomTheme | undefined {
  const m = view.manifest;
  if (!isSkin(m.base)) return undefined;
  return { key: keyOf(view), name: m.name, base: m.base, common: m.common, dark: m.dark, light: m.light };
}

function publish(authoritative: boolean) {
  const views = data ? [...data.site, ...data.user] : [];
  setCustomThemes(views.map(toCustom).filter((t): t is CustomTheme => !!t), authoritative);
}

/* ---- 读取 ---- */

async function load() {
  const gen = ++generation;
  controller?.abort();
  controller = new AbortController();
  try {
    const next = await api<ThemesResponse>("/themes", { signal: controller.signal });
    if (gen !== generation) return;
    data = { ...next, site: next.site || [], user: next.user || [], tokens: next.tokens || [], limits: next.limits || DEFAULT_LIMITS, bases: next.bases || [...SKINS] };
    publish(true);
    render();
  } catch (err) {
    if (gen !== generation || (err as Error).name === "AbortError") return;
    // 读不到不代表主题没了：保留旧列表与缓存里的选择，只在弹窗里说明
    publish(false);
    status(i18nText("读取主题失败：{p0}", { p0: (err as Error).message }), "error");
  }
}

/* ---- 内置风格的令牌：直接读已加载样式表里的 :root 规则 ----
 * 与浏览器层叠一致：琥珀的 :root → 琥珀浅色 → 该风格 → 该风格 × 明暗，
 * 同等特异度按样式表先后。var() 原样保留（服务端允许引用白名单令牌）。 */
export function builtinTokens(skin: Skin, mode: Mode, names: readonly string[]): Record<string, string> {
  const wanted = [":root", `:root[data-theme="${mode}"]`, `:root[data-skin="${skin}"]`, `:root[data-skin="${skin}"][data-theme="${mode}"]`];
  const found: { rank: number; order: number; rule: CSSStyleRule }[] = [];
  let order = 0;
  for (const sheet of document.styleSheets) {
    let rules: CSSRuleList;
    try { rules = sheet.cssRules; } catch { continue; } // 跨源样式表读不了，也不含令牌
    for (const rule of rules) {
      if (!(rule instanceof CSSStyleRule)) continue;
      const rank = wanted.indexOf(rule.selectorText.replace(/\s+/g, "").replaceAll("'", '"'));
      if (rank >= 0) found.push({ rank, order: order++, rule });
    }
  }
  found.sort((a, b) => a.rank - b.rank || a.order - b.order);
  const out: Record<string, string> = {};
  for (const { rule } of found) {
    for (const name of names) {
      const value = rule.style.getPropertyValue(name).trim().replace(/\s+/g, " ");
      if (value) out[name] = value;
    }
  }
  return out;
}

/** 把一个内置风格写成可编辑的主题清单：common 放形状与字体，颜色按明暗各写全一套 */
export function builtinManifest(skin: Skin, tokens: readonly ThemeToken[]): ThemeManifest {
  const names = tokens.map(t => t.name);
  const commonNames = new Set(tokens.filter(t => COMMON_KINDS.has(t.kind)).map(t => t.name));
  const split = (all: Record<string, string>, common: boolean) =>
    Object.fromEntries(Object.entries(all).filter(([name]) => commonNames.has(name) === common));
  const dark = builtinTokens(skin, "dark", names), light = builtinTokens(skin, "light", names);
  return {
    agentbox_theme: 1,
    id: `my-${skin}`,
    name: i18nText("{p0}（自定义）", { p0: SKIN_LABEL[skin]() }),
    base: skin,
    author: "",
    description: "",
    common: split(dark, true),
    dark: split(dark, false),
    light: split(light, false),
  };
}

/* ---- 对比度提醒 ----
 * 取「底子风格 + 主题改写」后的实际取值，展开 var()，用 canvas 把任意 CSS 颜色
 * （oklch、color-mix 也行）换成 sRGB，半透明的前景叠在背景上再算 WCAG 对比度。 */
const PAIRS: [fg: string, bg: string, min: number, label: () => string][] = [
  ["--text", "--bg", 4.5, () => i18nText("正文 / 背景")],
  ["--text", "--panel", 4.5, () => i18nText("正文 / 面板")],
  ["--muted", "--panel", 3, () => i18nText("次要文字 / 面板")],
  ["--on-accent", "--accent", 3, () => i18nText("按钮文字 / 主按钮")],
  ["--amber", "--bg", 3, () => i18nText("强调色 / 背景")],
];
type RGBA = [number, number, number, number];
let paint: CanvasRenderingContext2D | null | undefined;

function toRGBA(value: string): RGBA | undefined {
  paint ??= document.createElement("canvas").getContext("2d", { willReadFrequently: true });
  if (!paint) return undefined;
  // 无效颜色不会改动 fillStyle：换两个哨兵值都没被覆盖才算无效
  for (const sentinel of ["#010203", "#040506"]) {
    paint.fillStyle = sentinel;
    paint.fillStyle = value;
    if (paint.fillStyle === sentinel) return undefined;
  }
  paint.clearRect(0, 0, 1, 1);
  paint.fillRect(0, 0, 1, 1);
  const d = paint.getImageData(0, 0, 1, 1).data;
  return [d[0]!, d[1]!, d[2]!, d[3]! / 255];
}
const over = (fg: RGBA, bg: RGBA): RGBA => [0, 1, 2].map(i => fg[i]! * fg[3] + bg[i]! * (1 - fg[3])).concat(1) as RGBA;
const luminance = ([r, g, b]: RGBA) => {
  const c = (v: number) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
  return 0.2126 * c(r) + 0.7152 * c(g) + 0.0722 * c(b);
};
const ratio = (a: RGBA, b: RGBA) => {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
};

export interface ContrastIssue { mode: Mode; label: string; ratio: number; min: number }

export function contrastIssues(m: ThemeManifest, tokens: readonly ThemeToken[]): ContrastIssue[] {
  if (!isSkin(m.base)) return [];
  const names = tokens.map(t => t.name);
  const issues: ContrastIssue[] = [];
  for (const mode of ["dark", "light"] as const) {
    const vars: Record<string, string> = { ...builtinTokens(m.base, mode, names), ...m.common, ...m[mode] };
    const expand = (value: string, depth = 0): string =>
      depth > 5 ? value : value.replace(/var\(\s*(--[a-z0-9-]+)\s*\)/gi, (_, ref: string) => expand(vars[ref] ?? "", depth + 1));
    const color = (name: string) => (vars[name] ? toRGBA(expand(vars[name]!)) : undefined);
    const canvas: RGBA = mode === "dark" ? [0, 0, 0, 1] : [255, 255, 255, 1];
    const bg = color("--bg"), panel = color("--panel");
    const ground = bg ? over(bg, canvas) : undefined;
    const surface: Record<string, RGBA | undefined> = {
      "--bg": ground,
      "--panel": panel && ground ? over(panel, ground) : undefined,
    };
    for (const [fgName, bgName, min, label] of PAIRS) {
      const fg = color(fgName);
      const back = surface[bgName] ?? (() => { const c = color(bgName); return c && ground ? over(c, ground) : undefined; })();
      if (!fg || !back) continue;
      const r = ratio(over(fg, back), back);
      if (r < min) issues.push({ mode, label: label(), ratio: Math.round(r * 10) / 10, min });
    }
  }
  return issues;
}

/* ---- 渲染 ---- */

function status(text: string, kind: "ok" | "warn" | "error" = "ok") {
  const el = $("themes-status");
  el.textContent = text;
  el.className = "themes-status " + kind;
  el.classList.toggle("hidden", !text);
}

const modeLabel = (mode: string | undefined) =>
  mode === "dark" ? i18nText("深色") : mode === "light" ? i18nText("浅色") : i18nText("通用");

function problemText(err: unknown): string {
  const problem = err instanceof APIError ? err.problem as { error?: unknown; theme_error?: ThemeProblem } : undefined;
  const p = problem?.theme_error;
  // 其他失败（网络、权限、服务端错误）保留通用格式，含操作编号便于排查
  if (!p) return err instanceof Error ? err.message : String(err);
  // 校验失败是文件内容的问题，不带操作编号。服务端说明是简体中文写的、带具体原因；
  // 其他语言按稳定 code 翻成模板
  if (i18n.locale === "zh-CN" && typeof problem?.error === "string" && problem.error) return problem.error;
  const token = p.token || "", section = modeLabel(p.section);
  switch (p.code) {
    case "too_large": return i18nText("主题文件超过 {p0} KiB", { p0: String((data?.limits.bytes ?? DEFAULT_LIMITS.bytes) >> 10) });
    case "bad_json": return i18nText("主题文件不是有效的 JSON 对象");
    case "bad_type": return i18nText("主题文件里 {p0} 的类型不对", { p0: token });
    case "unknown_field": return i18nText("主题文件含不认识的字段 {p0}", { p0: token });
    case "format": return i18nText("不支持的主题格式版本（需要 agentbox_theme: 1）");
    case "id": return i18nText("主题 ID 只能包含小写字母、数字和短横线，最多 40 个字符，且以字母或数字开头");
    case "name": return i18nText("主题名称不能为空，最多 40 个字符，且不能含控制字符");
    case "base": return i18nText("base 必须是内置风格之一：{p0}", { p0: SKINS.join(" / ") });
    case "author": return i18nText("作者最多 60 个字符");
    case "description": return i18nText("说明最多 200 个字符");
    case "unknown_token": return i18nText("{p0}里的 {p1} 不是可改写的主题令牌", { p0: section, p1: token });
    case "bad_value": return i18nText("{p0}里 {p1} 的取值无效", { p0: section, p1: token });
    case "limit": return i18nText("主题数量已达上限");
    default: return err instanceof Error ? err.message : String(err);
  }
}

function button(label: () => string, icon: string, onClick: () => void, iconOnly = false) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "btn btn-sm";
  actionButton(b, iconOnly ? "" : label, icon, label);
  b.addEventListener("click", onClick);
  return b;
}

function swatch(value: string) {
  const s = document.createElement("span");
  s.className = "pref-swatch themes-swatch";
  s.setAttribute("aria-hidden", "true");
  s.style.setProperty("--swatch", value);
  return s;
}

function row(opts: { key: string; name: () => string; meta: () => string; swatch: string; actions: HTMLElement[] }) {
  const li = document.createElement("li");
  li.className = "themes-row";
  li.dataset.key = opts.key;
  const text = document.createElement("span");
  text.className = "themes-row-text";
  const name = document.createElement("span");
  name.className = "themes-row-name";
  setTextRender(name, opts.name);
  const meta = document.createElement("span");
  meta.className = "themes-row-meta";
  setTextRender(meta, opts.meta);
  text.append(name, meta);
  const actions = document.createElement("span");
  actions.className = "themes-row-actions";
  const active = currentSkin() === opts.key;
  const use = button(() => active ? i18nText("使用中") : i18nText("使用"), "check", () => choose(opts.key));
  use.disabled = active || busy;
  use.classList.add("themes-use");
  li.classList.toggle("active", active);
  actions.append(use, ...opts.actions);
  li.append(swatch(opts.swatch), text, actions);
  return li;
}

function choose(key: string) {
  setSkin(key);
  render();
}

function customRows(scope: Scope, views: ThemeView[], canDelete: boolean) {
  const list = $(scope === "site" ? "themes-site" : "themes-user");
  if (!views.length) {
    const empty = document.createElement("li");
    empty.className = "themes-empty";
    setTextRender(empty, () => !data ? i18nText("正在读取…") : scope === "site" ? i18nText("还没有全站主题。") : i18nText("还没有个人主题。导入一个主题文件，或先导出内置风格当模板。"));
    list.replaceChildren(empty);
    return;
  }
  list.replaceChildren(...views.map(view => {
    const m = view.manifest, custom = toCustom(view);
    const actions = [button(() => i18nText("导出"), "download", () => exportManifest(m), true)];
    if (canDelete) actions.push(button(() => i18nText("删除"), "trash", () => remove(scope, view), true));
    for (const b of actions) b.disabled = busy;
    return row({
      key: keyOf(view),
      name: () => m.name,
      meta: () => [i18nText("基于{p0}", { p0: isSkin(m.base) ? SKIN_LABEL[m.base]() : m.base }), m.author, m.description].filter(Boolean).join(" · "),
      swatch: custom ? customSwatch(custom) : "var(--panel-2)",
      actions,
    });
  }));
}

function render() {
  if (!dialog().open) return;
  // 整列重建会换掉按钮：记下焦点在哪一行、哪个按钮，重建后放回去
  const focused = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const focusKey = focused?.closest<HTMLElement>("#dlg-themes .themes-row")?.dataset.key;
  const focusLabel = focused?.getAttribute("aria-label");
  const limits = data?.limits ?? DEFAULT_LIMITS;
  const site = data?.site ?? [], user = data?.user ?? [];
  const admin = !!data?.can_manage_site;
  $("themes-user-count").textContent = `${user.length} / ${limits.user}`;
  $("themes-site-count").textContent = `${site.length} / ${limits.site}`;
  const importUser = $<HTMLButtonElement>("themes-import-user"), importSite = $<HTMLButtonElement>("themes-import-site");
  importUser.disabled = busy || !data;
  importSite.disabled = busy || !data;
  importSite.classList.toggle("hidden", !admin);
  customRows("user", user, true);
  customRows("site", site, admin);
  $("themes-builtin").replaceChildren(...SKINS.map(skin => {
    const exp = button(() => i18nText("导出为模板"), "download", () => exportBuiltin(skin), true);
    exp.disabled = busy || !data;
    return row({ key: skin, name: SKIN_LABEL[skin], meta: () => i18nText("内置风格"), swatch: `var(--swatch-${skin})`, actions: [exp] });
  }));
  if (focusKey) {
    const buttons = [...dialog().querySelectorAll<HTMLButtonElement>(`.themes-row[data-key="${CSS.escape(focusKey)}"] button`)];
    (buttons.find(b => b.getAttribute("aria-label") === focusLabel && !b.disabled) ?? buttons.find(b => !b.disabled))?.focus();
  }
}

/* ---- 导出 ---- */

function download(filename: string, value: unknown) {
  const blob = new Blob([JSON.stringify(value, null, 2) + "\n"], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  // 下载已经开始后再释放；Safari 在同一任务里释放会拿到空文件
  setTimeout(() => URL.revokeObjectURL(url), 30_000);
}

function exportManifest(m: ThemeManifest) {
  download(`${m.id}.agentbox-theme.json`, m);
}

function exportBuiltin(skin: Skin) {
  if (!data) return;
  const m = builtinManifest(skin, data.tokens);
  download(`${m.id}.agentbox-theme.json`, m);
  status(i18nText("已导出模板：改好 id、name 和令牌取值后，用「导入」加回来。"));
}

/* ---- 导入 / 删除 ---- */

async function mutate(work: () => Promise<void>) {
  if (busy) return;
  busy = true;
  render();
  const started = epoch;
  try {
    await work();
  } catch (err) {
    if (started === epoch) status(problemText(err), "error");
  } finally {
    busy = false;
    render();
  }
}

function pickFile(scope: Scope) {
  pendingScope = scope;
  const input = $<HTMLInputElement>("themes-file");
  input.value = "";
  input.click();
}

async function importFile(file: File, scope: Scope) {
  const limits = data?.limits ?? DEFAULT_LIMITS;
  if (file.size > limits.bytes) {
    status(i18nText("主题文件超过 {p0} KiB", { p0: String(limits.bytes >> 10) }), "error");
    return;
  }
  const text = await file.text();
  let parsed: unknown;
  try { parsed = JSON.parse(text); } catch { parsed = undefined; }
  const id = parsed && typeof parsed === "object" ? (parsed as { id?: unknown }).id : undefined;
  if (typeof id !== "string" || !/^[a-z0-9][a-z0-9-]{0,39}$/.test(id)) {
    status(parsed === undefined ? i18nText("主题文件不是有效的 JSON 对象") : i18nText("主题 ID 只能包含小写字母、数字和短横线，最多 40 个字符，且以字母或数字开头"), "error");
    return;
  }
  const existing = (scope === "site" ? data?.site : data?.user)?.find(v => v.manifest.id === id);
  if (existing) {
    const ok = await askConfirm(() => i18nText("「{p0}」已存在，用导入的文件替换它？", { p0: existing.manifest.name }), { title: i18nText("替换主题"), okLabel: i18nText("替换"), icon: "upload" });
    if (!ok) return;
  }
  await mutate(async () => {
    const started = epoch;
    const saved = await api<ThemeView>(`/themes/${scope}/${encodeURIComponent(id)}`, {
      method: "PUT", headers: { "Content-Type": "application/json" }, body: text,
    });
    if (started !== epoch) return;
    await load();
    if (started !== epoch) return;
    // 列表没读回来（网络抖动）时不切：theme.ts 解析不到新主题会落回琥珀
    if (data?.[scope].some(v => v.manifest.id === saved.manifest.id)) setSkin(keyOf(saved));
    const issues = data ? contrastIssues(saved.manifest, data.tokens) : [];
    const done = i18nText("已导入并启用「{p0}」。", { p0: saved.manifest.name });
    if (!issues.length) { status(done); return; }
    const details = issues.map(i => i18nText("{p0} {p1} {p2}:1（建议至少 {p3}:1）", { p0: modeLabel(i.mode), p1: i.label, p2: String(i.ratio), p3: String(i.min) })).join("；");
    status(done + " " + i18nText("对比度偏低：{p0}", { p0: details }), "warn");
  });
}

async function remove(scope: Scope, view: ThemeView) {
  const ok = await askConfirm(() => i18nText("删除主题「{p0}」？", { p0: view.manifest.name }), {
    title: i18nText("删除主题"), okLabel: i18nText("删除"), icon: "trash", danger: true,
    hint: scope === "site" ? i18nText("正在使用它的用户会回到琥珀风格。") : "",
  });
  if (!ok) return;
  await mutate(async () => {
    const started = epoch;
    await api(`/themes/${scope}/${encodeURIComponent(view.manifest.id)}`, { method: "DELETE" });
    if (started !== epoch) return;
    await load();
    status(i18nText("已删除「{p0}」。", { p0: view.manifest.name }));
  });
}

function open() {
  const dlg = dialog();
  status("");
  if (!dlg.open) dlg.showModal();
  render();
  void load();
}

/** 登录后挂载；返回的清理函数幂等，退出登录时作废请求、清掉个人主题、关掉弹窗 */
export function initThemes() {
  const lifetime = new AbortController();
  const { signal } = lifetime;
  bus.addEventListener("themes-open", open, { signal });
  $("themes-close").addEventListener("click", () => dialog().close(), { signal });
  $("themes-import-user").addEventListener("click", () => pickFile("user"), { signal });
  $("themes-import-site").addEventListener("click", () => pickFile("site"), { signal });
  $<HTMLInputElement>("themes-file").addEventListener("change", e => {
    const file = (e.target as HTMLInputElement).files?.[0];
    if (file) void importFile(file, pendingScope);
  }, { signal });
  // 在别处（其他标签页、风格菜单）换了主题时，弹窗里的「使用中」跟着变
  window.addEventListener("agentbox-theme-change", () => render(), { signal });
  void load();
  return () => {
    if (signal.aborted) return;
    lifetime.abort();
    generation++;
    epoch++;
    controller?.abort();
    controller = undefined;
    data = null;
    busy = false;
    if (dialog().open) dialog().close();
    // 不是权威列表：当前选中的主题继续按缓存显示，下一个登录的用户读到列表后再核对
    setCustomThemes([], false);
  };
}
