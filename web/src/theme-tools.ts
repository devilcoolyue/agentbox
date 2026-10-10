import { i18n, t as i18nText } from "./i18n.js";
/* theme-tools：主题管理弹窗（themes.ts）与主题编辑器（theme-editor.ts）共用的部分——
 * 从已加载样式表读内置风格的令牌、生成模板、对比度计算、校验错误文案。
 * 不持有状态，也不碰 DOM 以外的东西；令牌白名单由调用方传入（/api/themes 下发）。 */
"use strict";

import { actionButton } from "./icons.js";
import { APIError } from "./problems.js";
import { SKINS, SKIN_LABEL } from "./theme.js";
import type { Skin } from "./theme.js";
import type { ThemeManifest, ThemeProblem, ThemeToken } from "./types.js";

export type Mode = "dark" | "light";
/** 两种明暗都一样、放进 common 的令牌种类；颜色类按明暗各写一份 */
export const COMMON_KINDS = new Set(["scale", "radius", "font"]);
export const isSkin = (value: unknown): value is Skin => SKINS.includes(value as Skin);

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
export type RGBA = [number, number, number, number];
let paint: CanvasRenderingContext2D | null | undefined;

export function toRGBA(value: string): RGBA | undefined {
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

export const modeLabel = (mode: string | undefined) =>
  mode === "dark" ? i18nText("深色") : mode === "light" ? i18nText("浅色") : i18nText("通用");

export function problemText(err: unknown, limitBytes = 64 << 10): string {
  const problem = err instanceof APIError ? err.problem as { error?: unknown; theme_error?: ThemeProblem } : undefined;
  const p = problem?.theme_error;
  // 其他失败（网络、权限、服务端错误）保留通用格式，含操作编号便于排查
  if (!p) return err instanceof Error ? err.message : String(err);
  // 校验失败是文件内容的问题，不带操作编号。服务端说明是简体中文写的、带具体原因；
  // 其他语言按稳定 code 翻成模板
  if (i18n.locale === "zh-CN" && typeof problem?.error === "string" && problem.error) return problem.error;
  const token = p.token || "", section = modeLabel(p.section);
  switch (p.code) {
    case "too_large": return i18nText("主题文件超过 {p0} KiB", { p0: String(limitBytes >> 10) });
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

export function themeButton(label: () => string, icon: string, onClick: () => void, iconOnly = false) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "btn btn-sm";
  actionButton(b, iconOnly ? "" : label, icon, label);
  b.addEventListener("click", onClick);
  return b;
}
