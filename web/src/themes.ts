import { i18n, setTextRender, t as i18nText } from "./i18n.js";
/* themes：界面主题管理（用户弹层「风格 → 管理主题…」打开的弹窗）。
 *
 * 主题是一份 JSON 清单：选一个内置风格做底子，再改写白名单里的令牌（颜色、投影、
 * 圆角、字体）。服务端（internal/theme）严格校验令牌名与取值语法，这里只负责：
 * - 登录后读 /api/themes，把全站主题与个人主题交给 theme.ts，风格菜单随之出现；
 * - 导入（读本地文件原样提交，服务端校验）、导出、删除、启用；
 * - 把内置风格导出成写全两套令牌的模板：直接从已加载的 base.css / skins.css 规则里取值；
 * - 导入后按 WCAG 对比度提醒正文、按钮文字是否看不清（只提醒，不拦）；
 * - 「编辑」「新建」打开带实时预览的编辑器（theme-editor.ts），关掉后回到这里。
 * 随登录由 app/lifecycle 挂载，退出时清掉个人主题列表、关掉弹窗、作废在途请求。 */
"use strict";

import { api } from "./api.js";
import { bus } from "./state.js";
import { $, askConfirm } from "./util.js";
import { hideTip } from "./tip.js";
import { SKINS, SKIN_LABEL, currentSkin, customSwatch, isCustomKey, setCustomThemes, setSkin } from "./theme.js";
import type { CustomTheme, Skin } from "./theme.js";
import { builtinManifest, contrastIssues, isSkin, modeLabel, problemText, themeButton } from "./theme-tools.js";
import { closeThemeEditor, openThemeEditor, wireThemeEditor } from "./theme-editor.js";
import type { ThemeManifest, ThemesResponse, ThemeView } from "./types.js";

type Scope = "site" | "user";

const DEFAULT_LIMITS = { site: 50, user: 20, bytes: 64 << 10 };

let data: ThemesResponse | null = null;
/** 每次读列表 +1，只认最新一次的结果 */
let generation = 0;
/** 每次退出登录 +1：迟到的导入 / 删除结果不能落到下一个登录的用户身上 */
let epoch = 0;
let controller: AbortController | undefined;
let busy = false;
let pendingScope: Scope = "user";

const dialog = () => $<HTMLDialogElement>("dlg-themes");
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

/* ---- 渲染 ---- */

function status(text: string, kind: "ok" | "warn" | "error" = "ok") {
  const el = $("themes-status");
  el.textContent = text;
  el.className = "themes-status " + kind;
  el.classList.toggle("hidden", !text);
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
  const use = themeButton(() => active ? i18nText("使用中") : i18nText("使用"), "check", () => choose(opts.key));
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
    const actions = [themeButton(() => i18nText("导出"), "download", () => exportManifest(m), true)];
    // 能删的就能改：个人主题归本人，全站主题归管理员
    if (canDelete) actions.unshift(themeButton(() => i18nText("编辑"), "edit", () => edit(scope, m, false), true));
    if (canDelete) actions.push(themeButton(() => i18nText("删除"), "trash", () => remove(scope, view), true));
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
  $<HTMLButtonElement>("themes-new").disabled = busy || !data;
  $("themes-builtin").replaceChildren(...SKINS.map(skin => {
    const create = themeButton(() => i18nText("基于它新建"), "plus", () => createFrom(skin), true);
    const exp = themeButton(() => i18nText("导出为模板"), "download", () => exportBuiltin(skin), true);
    create.disabled = exp.disabled = busy || !data;
    return row({ key: skin, name: SKIN_LABEL[skin], meta: () => i18nText("内置风格"), swatch: `var(--swatch-${skin})`, actions: [create, exp] });
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

/* ---- 编辑 / 新建 ---- */

/** 新主题的 ID：my-<底子>，撞上了就加 -2、-3… */
function uniqueId(stem: string, scope: Scope) {
  const taken = new Set((data?.[scope] ?? []).map(v => v.manifest.id));
  const base = stem.slice(0, 36).replace(/-+$/, "") || "my-theme";
  if (!taken.has(base)) return base;
  for (let n = 2; ; n++) if (!taken.has(`${base}-${n}`)) return `${base}-${n}`;
}

function edit(scope: Scope, manifest: ThemeManifest, isNew: boolean) {
  if (!data) return;
  // 管理弹窗收起时，刚点的那个按钮的提示气泡会失去锚点停在角落里
  hideTip();
  dialog().close();
  openThemeEditor({
    scope, manifest, isNew,
    canChooseScope: isNew && !!data.can_manage_site,
    takenIds: { site: data.site.map(v => v.manifest.id), user: data.user.map(v => v.manifest.id) },
    tokens: data.tokens,
    limitBytes: data.limits.bytes,
    onClose: ({ saved, scope: savedScope }) => {
      // 保存成功的结果就是服务端现状：先并进本地列表并启用，和编辑器撤预览在同一个任务里完成，
      // 页面不会闪回旧主题；随后 open() 再从服务端读一遍对齐
      if (saved && data) {
        data = { ...data, [savedScope]: data[savedScope].filter(v => v.manifest.id !== saved.manifest.id).concat(saved) };
        publish(true);
        setSkin(keyOf(saved));
      }
      open();
      if (saved) status(i18nText("已保存并启用「{p0}」。", { p0: saved.manifest.name }));
    },
  });
}

/** 新主题只带名称、ID 和底子，令牌一个不写：保存下来的是「改了哪些」，没改的跟着底子走 */
function createFrom(skin: Skin) {
  edit("user", { agentbox_theme: 1, id: uniqueId("my-" + skin, "user"), name: i18nText("{p0}（自定义）", { p0: SKIN_LABEL[skin]() }), base: skin }, true);
}

/** 「新建」：以当前在用的风格为起点；在用的是自定义主题就复制它的改动 */
function createFromCurrent() {
  const current = currentSkin();
  const view = isCustomKey(current) ? [...(data?.site ?? []), ...(data?.user ?? [])].find(v => keyOf(v) === current) : undefined;
  if (!view) { createFrom(isSkin(current) ? current : "amber"); return; }
  const copy = JSON.parse(JSON.stringify(view.manifest)) as ThemeManifest;
  copy.id = uniqueId(copy.id, "user");
  copy.name = i18nText("{p0}（副本）", { p0: copy.name }).slice(0, 40);
  edit("user", copy, true);
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
    if (started === epoch) status(problemText(err, data?.limits.bytes), "error");
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
  wireThemeEditor();
  bus.addEventListener("themes-open", open, { signal });
  $("themes-new").addEventListener("click", createFromCurrent, { signal });
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
    closeThemeEditor();
    // 不是权威列表：当前选中的主题继续按缓存显示，下一个登录的用户读到列表后再核对
    setCustomThemes([], false);
  };
}
