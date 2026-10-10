import { setAttrRender, setTextRender, t as i18nText } from "./i18n.js";
/* theme-editor：主题编辑器（主题管理弹窗里「编辑」「新建」打开的右侧面板，窄屏是底部抽屉）。
 *
 * 改动通过 theme.ts 的 setThemePreview 实时写到整页上——预览就是真实界面，不另做一套样张页；
 * 面板顶部的样张只补平时页面上看不到的按钮、状态色、代码块和终端。预览不写存储、不改用户的
 * 选择，保存才走 PUT，取消或退出登录都会撤掉预览。
 *
 * 三种改法共用同一份草稿：常用（十几个最影响观感的令牌）、全部令牌（按分组）、JSON（整份
 * 粘贴，适合把 AI 改好的结果贴回来）。取值规则只认服务端：草稿每次变动后去 /api/themes/validate
 * 校验一遍（不保存），预览这边只靠 theme.ts 的安全过滤兜底——坏值不生效，不会变成别的样式。
 * 「复制给 AI 的说明」把格式规则、每个令牌管什么和当前草稿拼成一段话，任何 AI 都能照着改。 */
"use strict";

import { api } from "./api.js";
import { $, askConfirm } from "./util.js";
import { setSelectValue } from "./select.js";
import { SKINS, SKIN_LABEL, effectiveTheme, setThemePreview } from "./theme.js";
import type { CustomTheme, Skin } from "./theme.js";
import { COMMON_KINDS, builtinTokens, contrastIssues, isSkin, modeLabel, problemText, themeButton, toRGBA } from "./theme-tools.js";
import type { Mode, RGBA } from "./theme-tools.js";
import { CORE_TOKENS, TOKEN_GROUPS, tokenLabel } from "./theme-tokens.js";
import type { ThemeManifest, ThemeToken, ThemeView } from "./types.js";

type Scope = "site" | "user";
type Tab = "core" | "all" | "json";
type Section = "common" | Mode;

export interface EditorOptions {
  scope: Scope;
  manifest: ThemeManifest;
  isNew: boolean;
  /** 管理员新建时可以选存成全站主题 */
  canChooseScope: boolean;
  /** 各范围已有的 ID：新建时不许撞上，免得悄悄覆盖别的主题 */
  takenIds: Record<Scope, string[]>;
  tokens: ThemeToken[];
  limitBytes: number;
  onClose(result: { saved?: ThemeView; scope: Scope }): void;
}

interface State {
  opts: EditorOptions;
  originalId: string;
  scope: Scope;
  draft: ThemeManifest;
  mode: Mode;
  tab: Tab;
  dirty: boolean;
  serverError: string;
  jsonError: string;
  issues: string;
  saving: boolean;
  validateSeq: number;
  validateTimer?: ReturnType<typeof setTimeout>;
  jsonTimer?: ReturnType<typeof setTimeout>;
  frame: number;
  baseCache: Map<string, Record<string, string>>;
}

let state: State | null = null;
const dialog = () => $<HTMLDialogElement>("dlg-theme-editor");
const ID_RE = /^[a-z0-9][a-z0-9-]{0,39}$/;

const clone = (m: ThemeManifest): ThemeManifest => JSON.parse(JSON.stringify(m)) as ThemeManifest;
const kindOf = (name: string) => state?.opts.tokens.find(t => t.name === name)?.kind ?? "color";
const sectionFor = (name: string): Section => COMMON_KINDS.has(kindOf(name)) ? "common" : state!.mode;
const baseSkin = (): Skin => isSkin(state!.draft.base) ? state!.draft.base : "amber";

/** 底子风格在当前明暗下的取值（读样式表规则，按 base|mode 缓存） */
function baseValues(): Record<string, string> {
  const s = state!, key = baseSkin() + "|" + s.mode;
  let values = s.baseCache.get(key);
  if (!values) {
    values = builtinTokens(baseSkin(), s.mode, s.opts.tokens.map(t => t.name));
    s.baseCache.set(key, values);
  }
  return values;
}
const overrideOf = (name: string) => state!.draft[sectionFor(name)]?.[name];
function inheritedOf(name: string) {
  const s = state!;
  return (sectionFor(name) !== "common" ? s.draft.common?.[name] : undefined) ?? baseValues()[name] ?? "";
}
const effectiveOf = (name: string) => overrideOf(name) ?? inheritedOf(name);

/** 颜色令牌的实际 sRGB：展开 var() 引用后交给 canvas 解析；渐变、无效值返回 undefined */
function resolveColor(name: string): RGBA | undefined {
  const expand = (value: string, depth = 0): string =>
    depth > 5 ? value : value.replace(/var\(\s*(--[a-z0-9-]+)\s*\)/gi, (_, ref: string) => expand(effectiveOf(ref), depth + 1));
  const value = expand(effectiveOf(name));
  return value && !/gradient\(/i.test(value) ? toRGBA(value) : undefined;
}
const hex = ([r, g, b]: RGBA) => "#" + [r, g, b].map(v => v.toString(16).padStart(2, "0")).join("");

function draftTheme(): CustomTheme {
  const d = state!.draft;
  return { key: "preview:" + (d.id || "draft"), name: d.name, base: baseSkin(), common: d.common, dark: d.dark, light: d.light };
}

/* ---- 草稿变动：预览、JSON、校验 ---- */

function changed(source: "form" | "json" | "meta") {
  const s = state!;
  s.dirty = true;
  if (!s.frame) s.frame = requestAnimationFrame(() => {
    if (!state) return;
    state.frame = 0;
    setThemePreview({ theme: draftTheme(), mode: state.mode });
  });
  if (source !== "json" && s.tab === "json") $<HTMLTextAreaElement>("te-json").value = JSON.stringify(s.draft, null, 2);
  scheduleValidate();
}

function setOverride(name: string, value: string) {
  const s = state!, section = sectionFor(name);
  const v = value.trim();
  const map = { ...s.draft[section] };
  if (v) map[name] = v;
  else delete map[name];
  s.draft[section] = Object.keys(map).length ? map : undefined;
  changed("form");
}

function clientError(): string {
  const s = state!, d = s.draft;
  if (!s.opts.isNew && d.id !== s.originalId) return i18nText("编辑已有主题时不能修改 ID");
  if (s.opts.isNew && ID_RE.test(d.id || "") && s.opts.takenIds[s.scope].includes(d.id)) return i18nText("这个 ID 已被同范围的另一个主题使用，换一个");
  return "";
}

function scheduleValidate() {
  const s = state!;
  clearTimeout(s.validateTimer);
  s.validateTimer = setTimeout(() => void validate(), 350);
}

async function validate() {
  const s = state;
  if (!s) return;
  const seq = ++s.validateSeq;
  let error = "";
  try {
    await api("/themes/validate", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(s.draft) });
  } catch (err) {
    error = problemText(err, s.opts.limitBytes);
  }
  if (state !== s || seq !== s.validateSeq) return;
  s.serverError = error;
  const issues = error ? [] : contrastIssues(s.draft, s.opts.tokens);
  s.issues = issues.map(i => i18nText("{p0} {p1} {p2}:1（建议至少 {p3}:1）", { p0: modeLabel(i.mode), p1: i.label, p2: String(i.ratio), p3: String(i.min) })).join("；");
  renderStatus();
}

function renderStatus(notice = "") {
  const s = state!, el = $("te-status");
  const error = s.jsonError || clientError() || s.serverError;
  const text = error || notice || (s.issues ? i18nText("对比度偏低：{p0}", { p0: s.issues }) : "");
  el.textContent = text;
  el.className = "themes-status " + (error ? "error" : notice ? "ok" : "warn");
  el.classList.toggle("hidden", !text);
  $<HTMLButtonElement>("te-save").disabled = s.saving || !!error;
}

/* ---- 渲染 ---- */

function tokenRow(name: string) {
  const row = document.createElement("div");
  row.className = "te-token";
  row.dataset.token = name;
  const id = "te-v" + name;
  const color = document.createElement("input");
  color.type = "color";
  color.className = "te-color";
  setAttrRender(color, "aria-label", () => i18nText("{p0} 取色", { p0: tokenLabel(name) }));
  const label = document.createElement("label");
  label.className = "te-token-text";
  label.htmlFor = id;
  const title = document.createElement("span");
  title.className = "te-token-name";
  setTextRender(title, () => tokenLabel(name));
  const code = document.createElement("code");
  code.textContent = name;
  label.append(title, code);
  const input = document.createElement("input");
  input.type = "text";
  input.id = id;
  input.className = "te-value mono";
  input.spellcheck = false;
  input.autocomplete = "off";
  const reset = themeButton(() => i18nText("恢复为底子风格的值"), "undo", () => {
    setOverride(name, "");
    sync();
    input.focus();
  }, true);
  reset.classList.add("te-reset");

  const sync = () => {
    const override = overrideOf(name);
    input.value = override ?? "";
    input.placeholder = inheritedOf(name);
    row.classList.toggle("set", override !== undefined);
    reset.disabled = override === undefined;
    const rgba = kindOf(name) === "color" ? resolveColor(name) : undefined;
    color.hidden = !rgba;
    if (rgba) color.value = hex(rgba);
  };
  input.addEventListener("input", () => {
    setOverride(name, input.value);
    const override = overrideOf(name);
    row.classList.toggle("set", override !== undefined);
    reset.disabled = override === undefined;
    const rgba = kindOf(name) === "color" ? resolveColor(name) : undefined;
    color.hidden = !rgba;
    if (rgba) color.value = hex(rgba);
  });
  color.addEventListener("input", () => {
    // 原来带透明度的颜色保留透明度，只换色相：玻璃面板改个颜色不该突然变成不透明
    const alpha = resolveColor(name)?.[3] ?? 1;
    const [r, g, b] = [1, 3, 5].map(i => parseInt(color.value.slice(i, i + 2), 16));
    const value = alpha < 0.995 ? `rgba(${r}, ${g}, ${b}, ${Math.round(alpha * 100) / 100})` : color.value;
    setOverride(name, value);
    input.value = value;
    row.classList.add("set");
    reset.disabled = false;
  });
  sync();
  row.append(color, label, input, reset);
  return row;
}

function scaleRow() {
  const row = document.createElement("div");
  row.className = "te-token te-scale";
  row.dataset.token = "--radius-scale";
  const label = document.createElement("label");
  label.className = "te-token-text";
  label.htmlFor = "te-scale";
  const title = document.createElement("span");
  title.className = "te-token-name";
  setTextRender(title, () => tokenLabel("--radius-scale"));
  const code = document.createElement("code");
  code.textContent = "--radius-scale";
  label.append(title, code);
  const range = document.createElement("input");
  range.type = "range";
  range.id = "te-scale";
  range.min = "0";
  range.max = "3";
  range.step = "0.05";
  const value = document.createElement("output");
  value.className = "te-scale-value mono";
  const sync = () => {
    const v = Number.parseFloat(effectiveOf("--radius-scale")) || 0;
    range.value = String(v);
    value.textContent = String(Math.round(v * 100) / 100);
  };
  range.addEventListener("input", () => {
    setOverride("--radius-scale", String(Math.round(Number(range.value) * 100) / 100));
    sync();
  });
  sync();
  row.append(label, range, value);
  return row;
}

function renderPanels() {
  const s = state!;
  for (const tab of ["core", "all", "json"] as const) $("te-panel-" + tab).hidden = s.tab !== tab;
  for (const b of document.querySelectorAll<HTMLElement>("#te-tabs .scope-btn")) {
    const on = b.dataset.tab === s.tab;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  }
  for (const b of document.querySelectorAll<HTMLElement>("#te-mode .scope-btn")) {
    const on = b.dataset.mode === s.mode;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  }
  if (s.tab === "core") $("te-panel-core").replaceChildren(...CORE_TOKENS.map(tokenRow), scaleRow());
  else if (s.tab === "all") {
    $("te-panel-all").replaceChildren(...TOKEN_GROUPS.map(group => {
      const section = document.createElement("section");
      section.className = "te-group";
      const h = document.createElement("h3");
      setTextRender(h, group.label);
      section.append(h, ...group.tokens.filter(name => s.opts.tokens.some(t => t.name === name)).map(tokenRow));
      return section;
    }));
  } else {
    const area = $<HTMLTextAreaElement>("te-json");
    if (document.activeElement !== area) area.value = JSON.stringify(s.draft, null, 2);
  }
}

function renderMeta() {
  const s = state!, d = s.draft;
  $<HTMLInputElement>("te-name").value = d.name ?? "";
  const id = $<HTMLInputElement>("te-id");
  id.value = d.id ?? "";
  id.readOnly = !s.opts.isNew;
  setSelectValue($<HTMLSelectElement>("te-base"), baseSkin());
  $("te-scope-field").classList.toggle("hidden", !s.opts.canChooseScope);
  setSelectValue($<HTMLSelectElement>("te-scope"), s.scope);
  setTextRender($("te-title"), () => s.opts.isNew ? i18nText("新建主题") : i18nText("编辑主题"));
}

/* ---- 给 AI 的说明 ---- */

export function aiPrompt(draft: ThemeManifest, tokens: readonly ThemeToken[]): string {
  const bases = SKINS.map(skin => `${skin}（${SKIN_LABEL[skin]()}）`).join(" / ");
  return [
    i18nText("请帮我修改下面这份 agentbox 界面主题。只输出修改后的完整 JSON，不要任何解释。"),
    "",
    i18nText("规则："),
    i18nText("1. agentbox_theme 固定为 1；id 只能用小写字母、数字和短横线（最多 40 个字符）；name 最多 40 个字符。"),
    i18nText("2. base 是底子风格，决定圆角、半透明和特效，只能取：{p0}。", { p0: bases }),
    i18nText("3. common 里的令牌两种明暗都生效，dark、light 分别覆盖深色和浅色；没写的令牌沿用 base 的取值。"),
    i18nText("4. 颜色用 #十六进制、rgb()、rgba()、hsl()、oklch()、color-mix()，或 var(--另一个令牌)；投影写长度加颜色；--app-canvas 可以用 linear-gradient() 和 radial-gradient()；--radius-scale 是 0 到 3 的数字；含空格的字体名加双引号。"),
    i18nText("5. 不能使用 url()、分号、花括号、注释、反斜杠和 !important，也不能出现下面列表以外的令牌或字段。"),
    i18nText("6. --panel-solid 必须不透明；正文 --text 与背景 --bg 的对比度至少 4.5:1，主按钮文字 --on-accent 与 --accent 至少 3:1。"),
    "",
    i18nText("可用令牌："),
    ...tokens.map(t => `- ${t.name}：${tokenLabel(t.name)}`),
    "",
    i18nText("当前主题："),
    "```json",
    JSON.stringify(draft, null, 2),
    "```",
    "",
    i18nText("我的要求：（在这里写，比如「改成紫色赛博风，深色更暗一点」）"),
  ].join("\n");
}

async function copyPrompt() {
  const s = state!;
  const text = aiPrompt(s.draft, s.opts.tokens);
  const fallback = $<HTMLTextAreaElement>("te-ai-fallback");
  try {
    await navigator.clipboard.writeText(text);
    fallback.classList.add("hidden");
    renderStatus(i18nText("已复制。把它发给 AI 并写上你的要求，再把 AI 回复的 JSON 粘贴到「JSON」页签。"));
  } catch {
    // 非 HTTPS 或浏览器拒绝写剪贴板：把文字摆出来让用户自己复制
    fallback.value = text;
    fallback.classList.remove("hidden");
    fallback.focus();
    fallback.select();
    renderStatus(i18nText("浏览器不允许直接写入剪贴板，请手动复制下面的文字。"));
  }
}

/* ---- 打开 / 关闭 / 保存 ---- */

function finish(saved?: ThemeView) {
  const s = state;
  if (!s) return;
  clearTimeout(s.validateTimer);
  clearTimeout(s.jsonTimer);
  if (s.frame) cancelAnimationFrame(s.frame);
  state = null;
  setThemePreview(null);
  $<HTMLTextAreaElement>("te-ai-fallback").classList.add("hidden");
  if (dialog().open) dialog().close();
  s.opts.onClose({ saved, scope: s.scope });
}

async function cancel() {
  const s = state;
  if (!s || s.saving) return;
  if (s.dirty && !await askConfirm(() => i18nText("放弃未保存的修改？"), { title: i18nText("关闭编辑器"), okLabel: i18nText("放弃修改"), icon: "close", danger: true })) return;
  if (state === s) finish();
}

async function save() {
  const s = state;
  if (!s || s.saving) return;
  if (s.jsonError || clientError()) { renderStatus(); return; }
  s.saving = true;
  renderStatus();
  try {
    const saved = await api<ThemeView>(`/themes/${s.scope}/${encodeURIComponent(s.draft.id)}`, {
      method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(s.draft),
    });
    if (state === s) finish(saved);
  } catch (err) {
    if (state !== s) return;
    s.saving = false;
    s.serverError = problemText(err, s.opts.limitBytes);
    renderStatus();
  }
}

export function openThemeEditor(opts: EditorOptions) {
  if (state) finish();
  state = {
    opts, originalId: opts.manifest.id, scope: opts.scope, draft: clone(opts.manifest),
    mode: effectiveTheme(), tab: "core", dirty: false, serverError: "", jsonError: "", issues: "",
    saving: false, validateSeq: 0, frame: 0, baseCache: new Map(),
  };
  $<HTMLTextAreaElement>("te-ai-fallback").classList.add("hidden");
  renderMeta();
  renderPanels();
  renderStatus();
  dialog().showModal();
  setThemePreview({ theme: draftTheme(), mode: state.mode });
  void validate();
}

/** 退出登录等场合强制关闭：不询问、不保存，撤掉预览 */
export function closeThemeEditor() {
  if (!state) return;
  const s = state;
  s.opts = { ...s.opts, onClose: () => {} };
  finish();
}

/** 只挂一次的控件事件；处理函数都以当前 state 为准，没有打开的编辑器时什么也不做 */
let wired = false;
export function wireThemeEditor() {
  if (wired) return;
  wired = true;
  $("te-close").addEventListener("click", () => void cancel());
  $("te-cancel").addEventListener("click", () => void cancel());
  $("te-save").addEventListener("click", () => void save());
  $("te-ai").addEventListener("click", () => { if (state) void copyPrompt(); });
  dialog().addEventListener("cancel", e => { e.preventDefault(); void cancel(); });
  $<HTMLInputElement>("te-name").addEventListener("input", e => {
    if (!state) return;
    state.draft.name = (e.target as HTMLInputElement).value;
    changed("meta");
  });
  $<HTMLInputElement>("te-id").addEventListener("input", e => {
    if (!state || !state.opts.isNew) return;
    state.draft.id = (e.target as HTMLInputElement).value.trim();
    changed("meta");
    renderStatus();
  });
  $<HTMLSelectElement>("te-base").addEventListener("change", e => {
    if (!state) return;
    state.draft.base = (e.target as HTMLSelectElement).value;
    changed("meta");
    renderPanels();
  });
  $<HTMLSelectElement>("te-scope").addEventListener("change", e => {
    if (!state) return;
    state.scope = (e.target as HTMLSelectElement).value === "site" ? "site" : "user";
    renderStatus();
  });
  for (const b of document.querySelectorAll<HTMLElement>("#te-mode .scope-btn")) b.addEventListener("click", () => {
    if (!state || state.mode === b.dataset.mode) return;
    state.mode = b.dataset.mode === "light" ? "light" : "dark";
    setThemePreview({ theme: draftTheme(), mode: state.mode });
    renderPanels();
  });
  for (const b of document.querySelectorAll<HTMLElement>("#te-tabs .scope-btn")) b.addEventListener("click", () => {
    if (!state) return;
    state.tab = (b.dataset.tab as Tab) || "core";
    renderPanels();
  });
  $<HTMLTextAreaElement>("te-json").addEventListener("input", e => {
    const s = state;
    if (!s) return;
    clearTimeout(s.jsonTimer);
    s.jsonTimer = setTimeout(() => {
      if (state !== s) return;
      const text = (e.target as HTMLTextAreaElement).value;
      let parsed: unknown;
      try {
        parsed = JSON.parse(text);
      } catch (err) {
        s.jsonError = i18nText("JSON 格式有误：{p0}", { p0: (err as Error).message });
        renderStatus();
        return;
      }
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
        s.jsonError = i18nText("主题文件不是有效的 JSON 对象");
        renderStatus();
        return;
      }
      s.jsonError = "";
      s.draft = parsed as ThemeManifest;
      renderMeta();
      changed("json");
      renderStatus();
    }, 250);
  });
}
