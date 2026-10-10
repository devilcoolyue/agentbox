import { t as i18nText } from "./i18n.js";
/* theme-tokens：可改写令牌的界面说明——编辑器里的分组与名称、给 AI 的说明里每个令牌管什么。
 * 白名单本身以服务端 internal/theme 的 tokenList 为准（/api/themes 下发）；这里只补展示用的
 * 文字，浏览器回归会核对两边一致：服务端加了令牌，这里也要补一行。 */
"use strict";

export interface TokenGroup {
  id: string;
  label: () => string;
  tokens: string[];
}

export const TOKEN_GROUPS: TokenGroup[] = [
  { id: "surface", label: () => i18nText("表面与文字"), tokens: ["--bg", "--panel", "--panel-2", "--panel-solid", "--line", "--line-soft", "--text", "--text-hi", "--muted", "--field", "--page"] },
  { id: "accent", label: () => i18nText("强调与状态"), tokens: ["--amber", "--amber-dim", "--accent", "--accent-hi", "--on-accent", "--green", "--green-line", "--red", "--red-hi", "--red-line", "--on-red", "--warn", "--warn-line", "--info", "--diff-add"] },
  { id: "code", label: () => i18nText("代码与终端"), tokens: ["--code-bg", "--code-head", "--code-line", "--syntax-keyword", "--syntax-function", "--term-bg", "--term-fg", "--term-cursor", "--term-sel"] },
  { id: "overlay", label: () => i18nText("遮罩、投影与滚动条"), tokens: ["--scrim", "--backdrop", "--backdrop-strong", "--overlay-chip", "--busy-veil", "--checker-a", "--checker-b", "--scroll-thumb", "--scroll-thumb-hover", "--scroll-thumb-active", "--shadow-sm", "--shadow-md", "--shadow-lg"] },
  { id: "effects", label: () => i18nText("背景与风格效果"), tokens: ["--app-canvas", "--skin-detail", "--glass-glint", "--glass-edge"] },
  { id: "shape", label: () => i18nText("形状与字体"), tokens: ["--radius-scale", "--radius-pill", "--radius-round", "--sans", "--mono"] },
];

export const TOKEN_LABEL: Record<string, () => string> = {
  "--bg": () => i18nText("页面背景"),
  "--panel": () => i18nText("卡片、侧栏等面板"),
  "--panel-2": () => i18nText("悬停与次级填充"),
  "--panel-solid": () => i18nText("弹窗、菜单等浮层（须不透明）"),
  "--line": () => i18nText("边框与分隔线"),
  "--line-soft": () => i18nText("列表行分隔线"),
  "--text": () => i18nText("正文文字"),
  "--text-hi": () => i18nText("最亮文字（悬停）"),
  "--muted": () => i18nText("次要文字"),
  "--field": () => i18nText("输入框底色"),
  "--page": () => i18nText("页面头部底色"),
  "--amber": () => i18nText("强调文字与描边"),
  "--amber-dim": () => i18nText("弱强调描边"),
  "--accent": () => i18nText("主按钮底色"),
  "--accent-hi": () => i18nText("主按钮悬停"),
  "--on-accent": () => i18nText("主按钮上的文字"),
  "--green": () => i18nText("成功、运行中"),
  "--green-line": () => i18nText("成功描边"),
  "--red": () => i18nText("错误、危险操作"),
  "--red-hi": () => i18nText("危险按钮悬停"),
  "--red-line": () => i18nText("错误描边"),
  "--on-red": () => i18nText("危险按钮上的文字"),
  "--warn": () => i18nText("警告"),
  "--warn-line": () => i18nText("警告描边"),
  "--info": () => i18nText("提示信息"),
  "--diff-add": () => i18nText("变更里的新增行"),
  "--code-bg": () => i18nText("代码块背景"),
  "--code-head": () => i18nText("代码块标题栏"),
  "--code-line": () => i18nText("代码块边框"),
  "--syntax-keyword": () => i18nText("代码关键字"),
  "--syntax-function": () => i18nText("代码函数名"),
  "--term-bg": () => i18nText("终端背景"),
  "--term-fg": () => i18nText("终端文字"),
  "--term-cursor": () => i18nText("终端光标"),
  "--term-sel": () => i18nText("终端选区"),
  "--scrim": () => i18nText("抽屉遮罩"),
  "--backdrop": () => i18nText("弹窗背板"),
  "--backdrop-strong": () => i18nText("图片灯箱背板"),
  "--overlay-chip": () => i18nText("压在图片上的角标"),
  "--busy-veil": () => i18nText("启动、停止时的遮罩"),
  "--checker-a": () => i18nText("透明棋盘格（深格）"),
  "--checker-b": () => i18nText("透明棋盘格（浅格）"),
  "--scroll-thumb": () => i18nText("滚动条"),
  "--scroll-thumb-hover": () => i18nText("滚动条悬停"),
  "--scroll-thumb-active": () => i18nText("滚动条拖动"),
  "--shadow-sm": () => i18nText("小投影"),
  "--shadow-md": () => i18nText("中投影"),
  "--shadow-lg": () => i18nText("大投影"),
  "--app-canvas": () => i18nText("页面底层背景，可用渐变"),
  "--skin-detail": () => i18nText("点缀色（赛博朋克、工程蓝图）"),
  "--glass-glint": () => i18nText("玻璃高光（液态玻璃）"),
  "--glass-edge": () => i18nText("玻璃描边（液态玻璃）"),
  "--radius-scale": () => i18nText("圆角倍数，0 为直角"),
  "--radius-pill": () => i18nText("胶囊圆角"),
  "--radius-round": () => i18nText("圆形按钮圆角"),
  "--sans": () => i18nText("界面字体"),
  "--mono": () => i18nText("等宽字体"),
};

/** 「常用」页签：最影响整体观感的那几项，先改它们就能换出一套主题 */
export const CORE_TOKENS = ["--bg", "--panel", "--panel-solid", "--panel-2", "--line", "--text", "--muted", "--accent", "--on-accent", "--amber", "--green", "--red", "--warn", "--code-bg", "--term-bg"];

export const tokenLabel = (name: string) => TOKEN_LABEL[name]?.() ?? name;
