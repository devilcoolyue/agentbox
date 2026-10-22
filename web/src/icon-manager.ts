/* 系统设置 → 图标管理（仅管理员）。
 *
 * 两种视图：
 *   按位置：每个位置当前用哪个图标、跟随哪个语义位，可以逐个更换或恢复默认；
 *   按图标：每个图标用在哪些位置，可以整体换成另一个图标，或勾选它要用在哪些位置。
 * 改动先在本页实时预览，点「保存」才写到服务端，对全站生效。 */
import { $, toast, btnBusy, btnDone, askConfirm } from "./util.js";
import { t as i18nText, setText, setTextRender } from "./i18n.js";
import { api } from "./api.js";
import { S, bus } from "./state.js";
import { ICON_GROUPS, ICON_SLOTS, type IconGroup, type IconSlot, type IconSlotDef } from "./icon-slots.js";
import type { IconNode } from "./icon-builtin.js";
import {
  actionButton, applyIconConfig, defaultIcon, iconRefSvg, registerIconShapes, storeIconConfig, type IconConfig,
} from "./icons.js";

const SLOTS = ICON_SLOTS as Record<string, IconSlotDef>;
const IDS = Object.keys(SLOTS) as IconSlot[];
const SETS = ["tabler", "tabler-filled", "lucide"] as const;
type SetName = typeof SETS[number];

/** 默认跟随某个语义位的具体位置 */
const followers: Record<string, IconSlot[]> = {};
for (const id of IDS) {
  const base = SLOTS[id].base;
  if (base) (followers[base] ||= []).push(id);
}

let saved: Record<string, string> = {};
let draft: Record<string, string> = {};
let view: "slot" | "icon" = "slot";
let loaded = false;
let wired = false;

/* 语义位和具体位置有时同名（如「扩展」「文件夹」），同名的加上分组，免得列表里分不清 */
const sameName = new Set<string>();
{
  const seen = new Set<string>();
  for (const id of IDS) {
    if (seen.has(SLOTS[id].label)) sameName.add(SLOTS[id].label);
    seen.add(SLOTS[id].label);
  }
}
const label = (id: string) => {
  const def = SLOTS[id];
  return sameName.has(def.label) ? i18nText(ICON_GROUPS[def.group]) + " · " + i18nText(def.label) : i18nText(def.label);
};

/** 草稿下实际生效的图标 */
function effective(id: string): string {
  for (let cur = id, hops = 0; hops < 8; hops++) {
    if (draft[cur]) return draft[cur];
    const def = SLOTS[cur];
    if (def.icon) return def.icon;
    cur = def.base!;
  }
  return defaultIcon(id);
}

/** 不算自身覆盖时会得到的图标：自身默认，或所跟随语义位的当前图标 */
function inherited(id: string) {
  const def = SLOTS[id];
  return def.icon || effective(def.base!);
}

/** 正在跟随的语义位（自身没有覆盖、也没有独立默认时） */
function followedBase(id: string) {
  const def = SLOTS[id];
  return !draft[id] && !def.icon ? def.base : undefined;
}

/** 写入一个位置的图标；与继承来的相同就去掉覆盖，覆盖表保持最小 */
function assign(id: string, ref: string) {
  if (ref === inherited(id)) delete draft[id];
  else draft[id] = ref;
}

function dirtyCount() {
  const keys = new Set([...Object.keys(saved), ...Object.keys(draft)]);
  let n = 0;
  for (const k of keys) if (saved[k] !== draft[k]) n++;
  return n;
}

function changed() {
  applyIconConfig({ overrides: draft });
  render();
}

/* ---------------- 列表 ---------------- */

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls = "", text?: string) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

function smallButton(text: () => string, icon: IconSlot, run: () => void, cls = "btn-ghost") {
  const b = el("button", "btn btn-sm " + cls);
  b.type = "button";
  actionButton(b, text, icon);
  b.addEventListener("click", run);
  return b;
}

function matches(q: string, ...parts: string[]) {
  return !q || parts.some(p => p.toLowerCase().includes(q));
}

function slotRow(id: IconSlot) {
  const def = SLOTS[id];
  const ref = effective(id);
  const row = el("div", "icon-row" + (draft[id] ? " changed" : ""));
  row.dataset.slot = id;
  const glyph = el("span", "ir-icon");
  glyph.append(iconRefSvg(ref, 20));
  const main = el("div", "ir-main");
  const title = el("span", "ir-label", label(id));
  const meta = el("span", "ir-meta mono", id + " · " + ref);
  main.append(title, meta);
  const base = followedBase(id);
  const note = el("span", "ir-note");
  if (draft[id]) {
    note.classList.add("tag", "live");
    setText(note, "已修改");
    note.title = i18nText("默认：{p0}", { p0: inherited(id) });
  } else if (base) {
    note.textContent = i18nText("跟随「{p0}」", { p0: label(base) });
  }
  if (note.textContent) title.append(note);
  const fans = followers[id];
  if (fans?.length) {
    main.append(el("span", "ir-followers", i18nText("还用在：{p0}", { p0: fans.map(label).join("、") })));
  }
  const acts = el("div", "ir-actions");
  acts.append(smallButton(() => i18nText("更换"), "edit", async () => {
    const pick = await pickIcon(ref, label(id));
    if (pick) { assign(id, pick); changed(); }
  }));
  if (draft[id]) acts.append(smallButton(() => i18nText("恢复默认"), "undo", () => { delete draft[id]; changed(); }));
  row.append(glyph, main, acts);
  return row;
}

function renderBySlot(box: HTMLElement, q: string, onlyChanged: boolean) {
  for (const [group, title] of Object.entries(ICON_GROUPS) as [IconGroup, string][]) {
    const ids = IDS.filter(id => SLOTS[id].group === group && (!onlyChanged || draft[id])
      && matches(q, id, SLOTS[id].label, label(id), effective(id)));
    if (!ids.length) continue;
    const sec = el("section", "icons-group");
    sec.append(el("h3", "", i18nText(title)));
    const rows = el("div", "icons-rows card");
    rows.append(...ids.map(slotRow));
    sec.append(rows);
    box.append(sec);
  }
}

function iconCard(ref: string, ids: IconSlot[]) {
  const card = el("div", "icon-row icon-card" + (ids.some(id => draft[id]) ? " changed" : ""));
  const glyph = el("span", "ir-icon");
  glyph.append(iconRefSvg(ref, 24));
  const main = el("div", "ir-main");
  main.append(el("span", "ir-label mono", ref));
  const chips = el("div", "ic-slots");
  for (const id of ids) {
    const chip = el("span", "ic-slot" + (draft[id] ? " changed" : followedBase(id) ? " follows" : ""), label(id));
    chip.title = id + (followedBase(id) ? " · " + i18nText("跟随「{p0}」", { p0: label(followedBase(id)!) }) : "");
    chips.append(chip);
  }
  main.append(chips);
  const acts = el("div", "ir-actions");
  acts.append(
    smallButton(() => i18nText("整体更换"), "edit", async () => {
      const pick = await pickIcon(ref, ref);
      if (!pick || pick === ref) return;
      // 跟随语义位的位置不单独改：语义位换了，它们自然跟着变
      for (const id of ids) if (!followedBase(id) || !ids.includes(followedBase(id) as IconSlot)) assign(id, pick);
      changed();
    }),
    smallButton(() => i18nText("设置到哪些位置…"), "list-check", async () => {
      const picked = await pickSlots(ref);
      if (!picked) return;
      for (const id of IDS) {
        const on = picked.has(id), uses = effective(id) === ref;
        if (on && !uses) assign(id, ref);
        else if (!on && uses && draft[id] === ref) delete draft[id];
      }
      changed();
    }),
  );
  card.append(glyph, main, acts);
  return card;
}

function renderByIcon(box: HTMLElement, q: string, onlyChanged: boolean) {
  const byRef = new Map<string, IconSlot[]>();
  for (const id of IDS) {
    const ref = effective(id);
    if (!byRef.has(ref)) byRef.set(ref, []);
    byRef.get(ref)!.push(id);
  }
  const cards = [...byRef].filter(([ref, ids]) => (!onlyChanged || ids.some(id => draft[id]))
      && (matches(q, ref) || ids.some(id => matches(q, id, SLOTS[id].label, label(id)))))
    .sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]));
  if (!cards.length) return;
  const list = el("div", "icons-rows card");
  list.append(...cards.map(([ref, ids]) => iconCard(ref, ids)));
  box.append(list);
}

function render() {
  if (!loaded) return;
  const box = $("icons-list");
  const q = $<HTMLInputElement>("icons-filter").value.trim().toLowerCase();
  const onlyChanged = $<HTMLInputElement>("icons-changed-only").checked;
  box.replaceChildren();
  if (view === "slot") renderBySlot(box, q, onlyChanged);
  else renderByIcon(box, q, onlyChanged);
  if (!box.childElementCount) box.append(el("p", "acct-empty", i18nText("没有符合条件的位置")));
  for (const [id, on] of [["icons-by-slot", view === "slot"], ["icons-by-icon", view === "icon"]] as const) {
    $(id).classList.toggle("active", on);
    $(id).setAttribute("aria-selected", String(on));
  }
  const dirty = dirtyCount();
  $("icons-savebar").hidden = !dirty;
  setTextRender($("icons-dirty"), () => i18nText("{p0} 处修改尚未保存，页面上已经是预览效果", { p0: String(dirty) }));
  $<HTMLButtonElement>("icons-reset-all").disabled = !Object.keys(draft).length;
}

/* ---------------- 图标选择器 ---------------- */

interface IconSetFile { style: "stroke" | "fill"; icons: Record<string, IconNode[]>; tags?: Record<string, string> }
const sets = new Map<SetName, Promise<{ names: string[]; tags: Record<string, string> }>>();

function loadSet(name: SetName) {
  let p = sets.get(name);
  if (!p) {
    p = fetch(new URL(`../vendor/icons/${name}.json`, import.meta.url)).then(async res => {
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const doc = await res.json() as IconSetFile;
      const shapes: Record<string, IconNode[]> = {};
      for (const [k, v] of Object.entries(doc.icons)) shapes[name + ":" + k] = v;
      registerIconShapes(shapes);
      return { names: Object.keys(doc.icons), tags: doc.tags || {} };
    });
    p.catch(() => sets.delete(name));
    sets.set(name, p);
  }
  return p;
}

const PAGE = 240;

function pickIcon(current: string, subject: string): Promise<string | null> {
  const dlg = $<HTMLDialogElement>("dlg-icon-picker");
  const grid = $("icon-picker-grid");
  const filter = $<HTMLInputElement>("icon-picker-filter");
  const status = $("icon-picker-status");
  const ok = $<HTMLButtonElement>("icon-picker-ok");
  const choice = $("icon-picker-choice");
  setTextRender($("icon-picker-title"), () => i18nText("选择图标 · {p0}", { p0: subject }));
  let set: SetName = (SETS as readonly string[]).includes(current.split(":")[0]) ? current.split(":")[0] as SetName : "tabler";
  let chosen = "";
  let observer: IntersectionObserver | null = null;
  let token = 0;
  filter.value = "";

  const select = (ref: string) => {
    chosen = ref;
    ok.disabled = !ref || ref === current;
    choice.replaceChildren(...(ref ? [iconRefSvg(ref, 18), document.createTextNode(ref)] : []));
    for (const b of grid.querySelectorAll<HTMLElement>(".ip-item")) {
      const on = b.dataset.ref === ref;
      b.classList.toggle("on", on);
      b.setAttribute("aria-selected", String(on));
    }
  };

  const draw = async () => {
    const mine = ++token;
    observer?.disconnect();
    for (const b of $("icon-picker-sets").querySelectorAll<HTMLElement>("[data-set]")) b.classList.toggle("active", b.dataset.set === set);
    grid.replaceChildren();
    setText(status, "正在加载图标库…");
    let data;
    try { data = await loadSet(set); } catch (e) {
      if (mine === token) status.textContent = i18nText("图标库加载失败：") + (e as Error).message;
      return;
    }
    if (mine !== token) return;
    const q = filter.value.trim().toLowerCase();
    const words = q.split(/\s+/).filter(Boolean);
    const names = data.names.filter(n => words.every(w => n.includes(w) || data.tags[n]?.includes(w)));
    // 精确或前缀命中的排前面，搜 git 时 git-branch 不会被埋在一堆标签命中后面
    if (q) names.sort((a, b) => Number(!a.startsWith(words[0])) - Number(!b.startsWith(words[0])) || a.localeCompare(b));
    status.textContent = i18nText("{p0} 个图标", { p0: String(names.length) });
    let shown = 0;
    const sentinel = el("div", "ip-more");
    const more = () => {
      const frag = document.createDocumentFragment();
      for (const n of names.slice(shown, shown + PAGE)) {
        const ref = set + ":" + n;
        const b = el("button", "ip-item" + (ref === chosen ? " on" : "") + (ref === current ? " current" : ""));
        b.type = "button";
        b.dataset.ref = ref;
        b.title = n;
        b.setAttribute("role", "option");
        b.setAttribute("aria-label", n);
        b.setAttribute("aria-selected", String(ref === chosen));
        b.append(iconRefSvg(ref, 22));
        frag.append(b);
      }
      shown += PAGE;
      sentinel.before(frag);
      if (shown >= names.length) { observer?.disconnect(); sentinel.remove(); }
    };
    grid.append(sentinel);
    observer = new IntersectionObserver(entries => { if (entries.some(e => e.isIntersecting)) more(); }, { root: grid.parentElement, rootMargin: "200px" });
    more();
    if (shown < names.length) observer.observe(sentinel);
  };

  return new Promise(resolve => {
    const ac = new AbortController();
    const signal = ac.signal;
    const finish = (v: string | null) => { ac.abort(); observer?.disconnect(); if (dlg.open) dlg.close(); resolve(v); };
    grid.addEventListener("click", e => {
      const b = (e.target as Element).closest<HTMLElement>(".ip-item");
      if (b) select(b.dataset.ref!);
    }, { signal });
    grid.addEventListener("dblclick", e => {
      const b = (e.target as Element).closest<HTMLElement>(".ip-item");
      if (b && b.dataset.ref !== current) finish(b.dataset.ref!);
    }, { signal });
    $("icon-picker-sets").addEventListener("click", e => {
      const b = (e.target as Element).closest<HTMLElement>("[data-set]");
      if (b && b.dataset.set !== set) { set = b.dataset.set as SetName; void draw(); }
    }, { signal });
    let timer = 0;
    filter.addEventListener("input", () => { clearTimeout(timer); timer = window.setTimeout(draw, 150); }, { signal });
    ok.addEventListener("click", e => { e.preventDefault(); if (chosen) finish(chosen); }, { signal });
    for (const id of ["icon-picker-cancel", "icon-picker-close"]) $(id).addEventListener("click", () => finish(null), { signal });
    dlg.addEventListener("cancel", () => finish(null), { signal });
    select("");
    choice.replaceChildren(iconRefSvg(current, 18), document.createTextNode(i18nText("当前：{p0}", { p0: current })));
    dlg.showModal();
    filter.focus();
    void draw();
  });
}

/* ---------------- 一个图标设置到哪些位置 ---------------- */

function pickSlots(ref: string): Promise<Set<string> | null> {
  const dlg = $<HTMLDialogElement>("dlg-icon-slots");
  const list = $("icon-slots-list");
  const filter = $<HTMLInputElement>("icon-slots-filter");
  $("icon-slots-ref").replaceChildren(iconRefSvg(ref, 18), document.createTextNode(ref));
  filter.value = "";
  const checked = new Set<string>(IDS.filter(id => effective(id) === ref));
  const draw = () => {
    const q = filter.value.trim().toLowerCase();
    list.replaceChildren();
    for (const [group, title] of Object.entries(ICON_GROUPS) as [IconGroup, string][]) {
      const ids = IDS.filter(id => SLOTS[id].group === group && matches(q, id, SLOTS[id].label, label(id)));
      if (!ids.length) continue;
      list.append(el("h3", "", i18nText(title)));
      for (const id of ids) {
        const row = el("label", "check is-row");
        const box = el("input");
        box.type = "checkbox";
        box.checked = checked.has(id);
        box.addEventListener("change", () => { if (box.checked) checked.add(id); else checked.delete(id); });
        const now = el("span", "is-now");
        now.append(iconRefSvg(effective(id), 15));
        row.append(box, now, el("span", "", label(id)), el("span", "ir-meta mono", id));
        list.append(row);
      }
    }
  };
  return new Promise(resolve => {
    const ac = new AbortController();
    const signal = ac.signal;
    const finish = (v: Set<string> | null) => { ac.abort(); if (dlg.open) dlg.close(); resolve(v); };
    filter.addEventListener("input", draw, { signal });
    $("icon-slots-ok").addEventListener("click", e => { e.preventDefault(); finish(checked); }, { signal });
    for (const id of ["icon-slots-cancel", "icon-slots-close"]) $(id).addEventListener("click", () => finish(null), { signal });
    dlg.addEventListener("cancel", () => finish(null), { signal });
    draw();
    dlg.showModal();
    filter.focus();
  });
}

/* ---------------- 读写与入口 ---------------- */

async function load() {
  const cfg = await api<IconConfig & { overrides: Record<string, string> }>("/icons");
  storeIconConfig(cfg);
  saved = { ...cfg.overrides };
  draft = { ...saved };
  loaded = true;
}

async function save() {
  const btn = $<HTMLButtonElement>("icons-save");
  btnBusy(btn, () => i18nText("保存中…"));
  try {
    const cfg = await api<IconConfig & { overrides: Record<string, string> }>("/icons", { method: "PUT", body: JSON.stringify({ overrides: draft }) });
    storeIconConfig(cfg);
    saved = { ...cfg.overrides };
    draft = { ...saved };
    toast(i18nText("图标已保存，全站生效"));
  } catch (e) {
    toast(i18nText("保存图标失败：") + (e as Error).message, true);
  } finally {
    btnDone(btn);
    render();
  }
}

function wire(signal: AbortSignal) {
  if (wired) return;
  wired = true;
  signal.addEventListener("abort", () => { wired = false; });
  // 切到别的视图时预览也要撤掉，否则全站看到的是没保存的图标
  bus.addEventListener("view-changed", () => { if (S.view !== "settings") leaveIconManager(); }, { signal });
  $("icons-by-slot").addEventListener("click", () => { view = "slot"; render(); }, { signal });
  $("icons-by-icon").addEventListener("click", () => { view = "icon"; render(); }, { signal });
  $("icons-filter").addEventListener("input", render, { signal });
  $("icons-changed-only").addEventListener("change", render, { signal });
  $("icons-save").addEventListener("click", () => void save(), { signal });
  $("icons-discard").addEventListener("click", () => { draft = { ...saved }; changed(); }, { signal });
  $("icons-reset-all").addEventListener("click", async () => {
    if (!await askConfirm(() => i18nText("把所有位置恢复成默认图标？保存后对全站生效。"), { okLabel: i18nText("全部恢复"), icon: "undo" })) return;
    draft = {};
    changed();
  }, { signal });
}

/** 进入图标管理分区 */
export async function openIconManager(signal: AbortSignal) {
  wire(signal);
  if (!loaded) {
    $("icons-list").replaceChildren(el("p", "acct-empty", i18nText("正在读取图标配置…")));
    try { await load(); } catch (e) {
      $("icons-list").replaceChildren(el("p", "acct-empty", i18nText("读取图标配置失败：") + (e as Error).message));
      return;
    }
  }
  applyIconConfig({ overrides: draft });
  render();
}

/** 离开分区：未保存的草稿留着，页面图标先恢复成已保存的样子 */
export function leaveIconManager() {
  if (loaded && dirtyCount()) applyIconConfig({ overrides: saved });
}

