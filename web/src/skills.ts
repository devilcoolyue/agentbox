/* skills：技能（Claude Code Skill）页 —— 左侧是技能目录的文件树（SKILL.md、
 * scripts/、references/、assets/ 全部列出来），右侧看内容：.md 可切预览/源码，
 * 脚本和文本直接显示，图片内联，二进制只给下载。另外能安装、删除、在「本会话」
 * 与「我的模板」两个范围之间搬。
 * 「我的模板」写的是 data/users/<user>/home-template，每次会话启动铺进该用户
 * 的所有会话（服务端 agent.SeedHomeTemplate）。 */
"use strict";

import { actionButton, buttonLabel, fileIconName, svgIcon } from "./icons.js";

import { setSelectValue } from "./select.js";

import { S } from "./state.js";
import type {
  MarketCatalog, MarketPlugin, SkillDetail, SkillEntry, SkillFile, SkillInfo, SkillScope,
} from "./types.js";
import {
  $, spinEl, toast, askConfirm, fmtBytes, fmtSize, fmtTime, btnBusy, btnDone, startDownload,
} from "./util.js";
import { api, skillFileURL } from "./api.js";
import { formatText, splitFrontMatter, frontMatterChips } from "./chat-render.js";
import { setTip } from "./tip.js";

/* sel 是右侧显示什么：path 为空表示技能本身（SKILL.md 概览 + 操作按钮），
 * 否则是技能目录里的某个文件。open 装展开的节点键：技能名，或「技能名/子目录」。
 * cache 按技能存整棵树，刷新时清空、展开状态留着。 */
const SK: {
  scope: SkillScope;
  items: SkillInfo[];
  sel: { skill: string; path: string };
  view: "preview" | "source";
  open: Set<string>;
  cache: Map<string, SkillDetail>;
  loading: Set<string>;
} = {
  scope: "session",
  items: [],
  sel: { skill: "", path: "" },
  view: "preview", // 记住上次选的视图，切文件时不用反复点
  open: new Set(),
  cache: new Map(),
  loading: new Set(),
};

/* 右侧内容的竞态防护：连点几个文件时只让最后一次的结果上屏 */
let paneGen = 0;

/* 来源徽章：会话自装 / 用户模板 / 服务器模板。只有「本会话」范围才有意义，
 * 因为模板范围里的东西按定义都来自模板。 */
const SOURCE_LABEL: Record<string, string> = {
  session: "空间自装",
  template: "我的模板",
  global: "服务器模板",
};
const SOURCE_HINT: Record<string, string> = {
  session: "仅在当前工作空间使用。复制到「我的模板」后，可供其他空间复用。",
  template: "来自你的模板，工作空间启动时自动同步。",
  global: "来自服务器模板，由管理员统一下发给所有用户。",
};

function listMsg(msg: string) {
  const p = document.createElement("p");
  p.className = "files-empty";
  p.textContent = msg;
  $("skills-list").replaceChildren(p);
}

function detailMsg(msg: string) {
  const p = document.createElement("p");
  p.className = "files-empty";
  p.textContent = msg;
  $("skills-detail").replaceChildren(p);
}

function loadingRow(text: string) {
  const d = document.createElement("div");
  d.className = "loading-block";
  d.append(spinEl(), document.createTextNode(text));
  return d;
}

export async function loadSkills() {
  const sess = S.current;
  if (!sess) return;
  $("skills-list").replaceChildren(loadingRow("读取技能中…"));
  $("skills-detail").replaceChildren();
  $("skills-count").textContent = "";
  let data: { skills: SkillInfo[] };
  try {
    data = await api<{ skills: SkillInfo[] }>(`/sessions/${sess.id}/skills?scope=${SK.scope}`);
  } catch (e) {
    listMsg("读取技能失败：" + (e as Error).message);
    return;
  }
  SK.items = data.skills || [];
  SK.cache.clear(); // 刷新就是要看最新的目录内容
  renderList();

  // 选中的技能还在就把右侧恢复出来，没了就回到空白
  if (!SK.items.some((sk) => sk.name === SK.sel.skill)) {
    SK.sel = { skill: "", path: "" };
    detailMsg(SK.items.length ? "选择左侧技能查看说明与文件。" : "");
    return;
  }
  if (SK.sel.path) openFile(SK.sel.skill, SK.sel.path);
  else selectSkill(SK.sel.skill);
}

/* ---- 左侧文件树 ---- */

interface TreeNode {
  name: string;
  path: string; // 相对技能目录
  entry: SkillEntry;
  kids: TreeNode[];
}

/* 扁平清单拼成树。服务端保证父目录排在子项之前；万一没有（清单被截断），
 * 缺的父目录就地补一个占位，免得整条子树看不见。 */
function buildTree(entries: SkillEntry[]): TreeNode[] {
  const roots: TreeNode[] = [];
  const byPath = new Map<string, TreeNode>();

  const add = (path: string, entry: SkillEntry): TreeNode => {
    const cut = path.lastIndexOf("/");
    const node: TreeNode = { name: cut < 0 ? path : path.slice(cut + 1), path, entry, kids: [] };
    byPath.set(path, node);
    const parentPath = cut < 0 ? "" : path.slice(0, cut);
    const parent = parentPath
      ? byPath.get(parentPath) || add(parentPath, { path: parentPath, dir: true, size: 0, mtime: "" })
      : null;
    (parent ? parent.kids : roots).push(node);
    return node;
  };

  for (const e of entries) {
    const hit = byPath.get(e.path);
    if (hit) hit.entry = e; // 占位目录被真正的条目补全
    else add(e.path, e);
  }

  // SKILL.md 置顶（它才是技能的入口），然后目录，最后其它文件
  const rank = (n: TreeNode) => (n.path === "SKILL.md" ? 0 : n.entry.dir ? 1 : 2);
  const sortKids = (nodes: TreeNode[]) => {
    nodes.sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name));
    for (const n of nodes) sortKids(n.kids);
  };
  sortKids(roots);
  return roots;
}

function renderList() {
  const n = SK.items.length;
  $("skills-count").textContent = n ? `${n} 个技能` : "";
  if (!n) {
    listMsg(SK.scope === "session"
      ? "当前空间还没有技能。点击「安装技能」，或从「我的模板」中选择技能安装。"
      : "模板里还没有技能。安装到这里后，你的各个工作空间启动时都会自动同步。");
    detailMsg("");
    return;
  }
  const frag = document.createDocumentFragment();
  for (const sk of SK.items) {
    frag.appendChild(skillRow(sk));
    if (!SK.open.has(sk.name)) continue;
    const det = SK.cache.get(sk.name);
    if (!det) continue; // 还在拉，箭头上已经在转圈
    const roots = buildTree(det.entries || []);
    if (!roots.length) {
      frag.appendChild(hintRow("（空目录，Claude Code 不会加载它）", 1));
      continue;
    }
    appendNodes(frag, sk.name, roots, 1);
    if (det.more) frag.appendChild(hintRow("……文件太多，只列出了前面一部分", 1));
  }
  $("skills-list").replaceChildren(frag);
}

function baseRow(depth: number, active: boolean) {
  const row = document.createElement("div");
  row.className = "skill-row" + (active ? " active" : "");
  row.style.setProperty("--depth", String(depth));
  row.tabIndex = 0;
  return row;
}

/* 行主体的点击/回车都走同一个动作；箭头自己 stopPropagation，不牵连选中 */
function bindOpen(row: HTMLElement, action: () => void) {
  row.addEventListener("click", action);
  row.addEventListener("keydown", (e) => { if (e.key === "Enter") action(); });
}

function arrowEl(open: boolean, busy: boolean, toggle: () => void) {
  const a = document.createElement("span");
  a.className = "skill-arrow" + (open ? " open" : "") + (busy ? " busy" : "");
  if (busy) a.replaceChildren(spinEl());
  else a.append(svgIcon("chevron-right", 12));
  setTip(a, open ? "收起" : "展开目录");
  a.setAttribute("role", "button");
  a.setAttribute("aria-label", open ? "收起目录" : "展开目录");
  a.tabIndex = -1; // 行本身可聚焦就够了，别让 Tab 在树里走两遍
  a.addEventListener("click", (e) => { e.stopPropagation(); toggle(); });
  return a;
}

function tagEl(text: string, title: string) {
  const s = document.createElement("span");
  s.className = "skill-flag";
  s.textContent = text;
  setTip(s, title);
  return s;
}

function hintRow(text: string, depth: number) {
  const row = baseRow(depth, false);
  row.classList.add("skill-hint");
  row.tabIndex = -1;
  row.textContent = text;
  return row;
}

function skillRow(sk: SkillInfo) {
  const row = baseRow(0, SK.sel.skill === sk.name && !SK.sel.path);
  const line = document.createElement("div");
  line.className = "skill-main";
  line.appendChild(arrowEl(SK.open.has(sk.name), SK.loading.has(sk.name), () => toggleSkill(sk.name)));

  const name = document.createElement("span");
  name.className = "skill-name mono";
  name.textContent = sk.name;
  line.appendChild(name);
  if (SK.scope === "session" && sk.source !== "session") {
    const badge = document.createElement("span");
    badge.className = "skill-badge s-" + sk.source;
    badge.textContent = SOURCE_LABEL[sk.source] || sk.source;
    line.appendChild(badge);
  }

  const desc = document.createElement("div");
  desc.className = "skill-desc";
  desc.textContent = sk.description || "（无描述）";

  row.append(line, desc);
  bindOpen(row, () => selectSkill(sk.name));
  return row;
}

function appendNodes(frag: DocumentFragment, skill: string, nodes: TreeNode[], depth: number) {
  for (const node of nodes) {
    frag.appendChild(nodeRow(skill, node, depth));
    if (!node.entry.dir || !SK.open.has(skill + "/" + node.path)) continue;
    if (node.kids.length) appendNodes(frag, skill, node.kids, depth + 1);
    else frag.appendChild(hintRow("（空目录）", depth + 1));
  }
}

function nodeRow(skill: string, node: TreeNode, depth: number) {
  const key = skill + "/" + node.path;
  const isDir = !!node.entry.dir;
  const row = baseRow(depth, SK.sel.skill === skill && SK.sel.path === node.path);
  row.classList.add(isDir ? "is-dir" : "is-file");

  const line = document.createElement("div");
  line.className = "skill-main";
  if (isDir) {
    line.appendChild(arrowEl(SK.open.has(key), false, () => toggleDir(key)));
  } else {
    const glyph = document.createElement("span");
    glyph.className = "skill-glyph";
    glyph.append(svgIcon(fileIconName(node.name), 14));
    line.appendChild(glyph);
  }

  const name = document.createElement("span");
  name.className = "skill-name mono";
  name.textContent = node.name;
  line.appendChild(name);

  if (node.entry.link) line.appendChild(tagEl("链接", "符号链接：装技能时不会产生，也不支持预览"));
  else if (node.entry.exec) line.appendChild(tagEl("+x", "有可执行位，脚本能直接跑"));
  if (!isDir) {
    const size = document.createElement("span");
    size.className = "skill-size";
    size.textContent = fmtSize(node.entry.size);
    line.appendChild(size);
  }

  row.appendChild(line);
  bindOpen(row, () => {
    if (isDir) toggleDir(key);
    else if (node.entry.link) toast("符号链接不支持预览", true);
    else openFile(skill, node.path);
  });
  return row;
}

function isImage(path: string) {
  return /\.(png|jpe?g|gif|svg|webp|bmp|ico)$/i.test(path);
}

/* ---- 拉取与选中 ---- */

/* 技能详情连整棵文件树一起回来（技能就几十个文件的量级），拉一次缓存住，
 * 展开子目录不再打服务端。 */
async function ensureDetail(name: string): Promise<SkillDetail | null> {
  const hit = SK.cache.get(name);
  if (hit) return hit;
  const sess = S.current;
  if (!sess) return null;
  SK.loading.add(name);
  renderList(); // 箭头原地转圈
  try {
    const det = await api<SkillDetail>(
      `/sessions/${sess.id}/skills/${encodeURIComponent(name)}?scope=${SK.scope}`);
    SK.cache.set(name, det);
    return det;
  } catch (e) {
    toast("读取技能失败：" + (e as Error).message, true);
    SK.open.delete(name);
    return null;
  } finally {
    SK.loading.delete(name);
  }
}

async function toggleSkill(name: string) {
  if (SK.open.has(name)) {
    SK.open.delete(name);
    renderList();
    return;
  }
  SK.open.add(name);
  await ensureDetail(name);
  renderList();
}

function toggleDir(key: string) {
  if (SK.open.has(key)) SK.open.delete(key);
  else SK.open.add(key);
  renderList();
}

async function selectSkill(name: string) {
  SK.sel = { skill: name, path: "" };
  SK.open.add(name);
  const gen = ++paneGen;
  renderList();
  $("skills-detail").replaceChildren(loadingRow("读取 SKILL.md…"));
  const det = await ensureDetail(name);
  renderList();
  if (gen !== paneGen) return; // 期间点了别的
  if (!det) {
    detailMsg("读取失败");
    return;
  }
  renderDetail(det);
}

async function openFile(skill: string, path: string) {
  const sess = S.current;
  if (!sess) return;
  SK.sel = { skill, path };
  const gen = ++paneGen;
  renderList();
  $("skills-detail").replaceChildren(loadingRow("读取 " + path + "…"));
  try {
    const f = await api<SkillFile>(
      `/sessions/${sess.id}/skills/${encodeURIComponent(skill)}/file` +
      `?scope=${SK.scope}&path=${encodeURIComponent(path)}`);
    if (gen !== paneGen) return;
    renderFile(skill, f);
  } catch (e) {
    if (gen === paneGen) detailMsg("读取失败：" + (e as Error).message);
  }
}

function renderDetail(det: SkillDetail) {
  const head = document.createElement("div");
  head.className = "skill-head";

  const title = document.createElement("h3");
  title.className = "skill-title mono";
  title.textContent = det.name;
  head.appendChild(title);

  if (det.description) {
    const d = document.createElement("p");
    d.className = "skill-head-desc";
    d.textContent = det.description;
    head.appendChild(d);
  }

  const meta = document.createElement("p");
  meta.className = "skill-meta";
  const bits = [`${det.files} 个文件`, fmtBytes(det.bytes)];
  // 空目录没有任何文件时 updated_at 是零值时间，别拿它渲染出个 0001 年
  if (det.files && det.updated_at) bits.push("更新于 " + fmtTime(det.updated_at));
  meta.textContent = bits.join(" · ");
  head.appendChild(meta);

  if (SK.scope === "session" && SOURCE_HINT[det.source]) {
    const src = document.createElement("p");
    src.className = "skill-source";
    src.textContent = SOURCE_HINT[det.source];
    head.appendChild(src);
  }

  const actions = document.createElement("div");
  actions.className = "skill-head-actions";
  const move = document.createElement("button");
  move.className = "btn btn-sm";
  actionButton(move, SK.scope === "session" ? "复制" : "安装", SK.scope === "session" ? "copy" : "package-plus", SK.scope === "session" ? "复制技能到我的模板" : "安装技能到本空间");
  setTip(move, SK.scope === "session"
    ? "复制进模板后，你名下每个工作空间启动时都会带上它"
    : "把模板里的这个技能立刻装进当前工作空间，不必等下次启动");
  move.addEventListener("click", () => copySkill(det.name, move));
  const del = document.createElement("button");
  del.className = "btn btn-sm btn-danger";
  actionButton(del, "", "trash", "删除技能");
  del.addEventListener("click", () => removeSkill(det.name));
  actions.append(move, del);
  head.appendChild(actions);

  const body = document.createElement("div");
  body.className = "skill-doc";
  if (det.content) {
    body.append(...docView("SKILL.md", det.content, det.truncated, true));
  } else {
    const p = document.createElement("p");
    p.className = "files-empty";
    p.textContent = "这个技能目录里没有 SKILL.md —— Claude Code 不会加载它。";
    body.appendChild(p);
  }

  $("skills-detail").replaceChildren(head, body);
}

/* 文件树里点开的单个文件：.md 走预览/源码，图片内联，二进制只给下载，
 * 其它一律当文本原样显示（scripts/ 下的脚本就是这一支）。 */
function renderFile(skill: string, f: SkillFile) {
  const head = document.createElement("div");
  head.className = "skill-head";

  const back = document.createElement("button");
  back.type = "button";
  back.className = "skill-back";
  buttonLabel(back, skill, "arrow-left");
  setTip(back, "回到技能概览");
  back.addEventListener("click", () => selectSkill(skill));
  head.appendChild(back);

  const title = document.createElement("h3");
  title.className = "skill-title mono";
  title.textContent = f.path;
  head.appendChild(title);

  const meta = document.createElement("p");
  meta.className = "skill-meta";
  meta.textContent = [
    fmtSize(f.size),
    f.mode,
    f.exec ? "可执行" : "",
    f.mtime ? "更新于 " + fmtTime(f.mtime) : "",
  ].filter(Boolean).join(" · ");
  head.appendChild(meta);

  const actions = document.createElement("div");
  actions.className = "skill-head-actions";
  const dl = document.createElement("button");
  dl.className = "btn btn-sm";
  actionButton(dl, "", "download", "下载技能文件");
  dl.addEventListener("click", () => startDownload(skillFileURL(skill, f.path, SK.scope, true)));
  actions.appendChild(dl);
  head.appendChild(actions);

  const body = document.createElement("div");
  body.className = "skill-doc";
  if (isImage(f.path)) {
    const img = document.createElement("img");
    img.className = "skill-img";
    img.src = skillFileURL(skill, f.path, SK.scope);
    img.alt = f.path;
    body.appendChild(img);
  } else if (f.binary) {
    const p = document.createElement("p");
    p.className = "files-empty";
    p.textContent = `二进制文件（${fmtSize(f.size)}），没法在页面里看，点上面的「下载」拿到本地。`;
    body.appendChild(p);
  } else if (/\.md$/i.test(f.path)) {
    body.append(...docView(f.path, f.content, f.truncated, false));
  } else {
    const pre = document.createElement("pre");
    pre.className = "skill-md mono";
    pre.textContent = f.content + (f.truncated ? "\n…（内容过长，已截断）" : "");
    body.appendChild(pre);
  }

  $("skills-detail").replaceChildren(head, body);
}

/* Markdown 正文区：一条「预览 / 源码」切换栏 + 内容。预览走对话那套轻量
 * Markdown 渲染器，样式与消息气泡一致，不必再养第二套。
 * skipMeta 用于技能概览：name/description 上方已经显示过，不再重复列出来。 */
function docView(label: string, content: string, truncated: boolean, skipMeta: boolean) {
  const bar = document.createElement("div");
  bar.className = "skill-doc-bar";
  const labelEl = document.createElement("span");
  labelEl.className = "skill-doc-label mono";
  labelEl.textContent = label;
  const sw = document.createElement("div");
  sw.className = "scope-switch";
  const buttons: Record<string, HTMLButtonElement> = {};
  for (const [mode, text] of [["preview", "预览"], ["source", "源码"]] as const) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "scope-btn" + (SK.view === mode ? " active" : "");
    buttonLabel(b, text, mode === "preview" ? "eye" : "code");
    b.addEventListener("click", () => {
      if (SK.view === mode) return;
      SK.view = mode;
      for (const [k, el] of Object.entries(buttons)) el.classList.toggle("active", k === mode);
      paint();
    });
    buttons[mode] = b;
    sw.appendChild(b);
  }
  bar.append(labelEl, sw);

  const view = document.createElement("div");
  const tail = truncated ? "\n…（内容过长，已截断）" : "";
  const paint = () => {
    if (SK.view === "source") {
      view.className = "skill-view";
      const pre = document.createElement("pre");
      pre.className = "skill-md mono";
      pre.textContent = content + tail;
      view.replaceChildren(pre);
      return;
    }
    // 预览：front matter 里的键单独列出来，免得渲染时被当成一条分隔线加一段
    // 文字，也免得直接丢掉丢了信息。
    const { meta, body } = splitFrontMatter(content);
    view.className = "skill-view msg agent";
    const parts: Node[] = [];
    const rest = skipMeta ? meta.filter(([k]) => k !== "name" && k !== "description") : meta;
    if (rest.length) parts.push(frontMatterChips(rest));
    parts.push(formatText(body + tail));
    view.replaceChildren(...parts);
  };
  paint();
  return [bar, view];
}

async function copySkill(name: string, btn: HTMLButtonElement) {
  const sess = S.current;
  if (!sess) return;
  const to: SkillScope = SK.scope === "session" ? "template" : "session";
  btnBusy(btn, "处理中…");
  try {
    await api(`/sessions/${sess.id}/skills/${encodeURIComponent(name)}/copy?scope=${SK.scope}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ to }),
    });
    toast(to === "template" ? "已复制到模板，各工作空间下次启动时同步" : "已安装到本空间");
    loadSkills();
  } catch (e) {
    toast("操作失败：" + (e as Error).message, true);
  } finally {
    btnDone(btn);
  }
}

async function removeSkill(name: string) {
  const sess = S.current;
  if (!sess) return;
  const where = SK.scope === "session" ? "当前工作空间" : "你的模板";
  const ok = await askConfirm(`确认从${where}删除技能「${name}」？`, {
    title: "删除技能",
    danger: true,
    okLabel: "删除", icon: "trash",
    hint: SK.scope === "session"
      ? "若它来自模板，下次工作空间启动还会被铺回来；要彻底去掉请到「我的模板」里删。"
      : "已经铺进各个工作空间的副本不会跟着消失，需要各自删除。",
  });
  if (!ok) return;
  try {
    await api(`/sessions/${sess.id}/skills/${encodeURIComponent(name)}?scope=${SK.scope}`,
      { method: "DELETE" });
    toast("已删除");
    SK.sel = { skill: "", path: "" };
    SK.open.delete(name);
    loadSkills();
  } catch (e) {
    toast("删除失败：" + (e as Error).message, true);
  }
}

/* ---- 范围切换与安装 ---- */

function setSkillScope(scope: SkillScope) {
  if (SK.scope === scope) return;
  SK.scope = scope;
  SK.sel = { skill: "", path: "" };
  SK.open.clear(); // 两个范围的目录内容不是一回事，展开状态不跟着走
  $("skill-scope-session").classList.toggle("active", scope === "session");
  $("skill-scope-template").classList.toggle("active", scope === "template");
  loadSkills();
}

$("skill-scope-session").addEventListener("click", () => setSkillScope("session"));
$("skill-scope-template").addEventListener("click", () => setSkillScope("template"));
$("btn-skills-refresh").addEventListener("click", loadSkills);

/* ---- 安装弹窗：本地上传 / 官方市场 ---- */

const dlgInstall = () => $<HTMLDialogElement>("dlg-skill-install");

function scopeLabel() {
  return SK.scope === "session" ? "本空间" : "我的模板";
}

$("btn-skill-install").addEventListener("click", () => {
  $("skill-install-target").textContent = scopeLabel();
  setInstallSource("local");
  dlgInstall().showModal();
});
$("skill-install-close").addEventListener("click", () => dlgInstall().close());

function setInstallSource(src: "local" | "market") {
  $("skill-src-local").classList.toggle("active", src === "local");
  $("skill-src-market").classList.toggle("active", src === "market");
  $("skill-src-local-pane").classList.toggle("hidden", src !== "local");
  $("skill-src-market-pane").classList.toggle("hidden", src !== "market");
  if (src === "market") loadMarket(false);
}
$("skill-src-local").addEventListener("click", () => setInstallSource("local"));
$("skill-src-market").addEventListener("click", () => setInstallSource("market"));

/* 装完统一收尾：关弹窗、刷新列表、选中并展开新装的那个（loadSkills 会按 sel
 * 把右侧恢复出来） */
async function afterInstall(name: string, msg: string) {
  dlgInstall().close();
  toast(msg);
  SK.sel = { skill: name, path: "" };
  if (name) SK.open.add(name);
  await loadSkills();
}

async function installFile(file: File) {
  const sess = S.current;
  if (!sess) return;
  const drop = $("skill-drop");
  drop.classList.add("over");
  const fd = new FormData();
  fd.append("file", file, file.name);
  try {
    const res = await api<{ name: string }>(`/sessions/${sess.id}/skills?scope=${SK.scope}`,
      { method: "POST", body: fd });
    await afterInstall(res.name, `已安装技能「${res.name}」`);
  } catch (err) {
    toast("安装失败：" + (err as Error).message, true);
  } finally {
    drop.classList.remove("over");
  }
}

$("skill-drop").addEventListener("click", () => $<HTMLInputElement>("skill-install-input").click());
$("skill-install-input").addEventListener("change", (e) => {
  const input = e.target as HTMLInputElement;
  const file = input.files && input.files[0];
  input.value = ""; // 同一个文件连续选两次也要触发
  if (file) installFile(file);
});
for (const ev of ["dragenter", "dragover"] as const) {
  $("skill-drop").addEventListener(ev, (e) => {
    e.preventDefault();
    $("skill-drop").classList.add("over");
  });
}
$("skill-drop").addEventListener("dragleave", () => $("skill-drop").classList.remove("over"));
$("skill-drop").addEventListener("drop", (e) => {
  e.preventDefault();
  $("skill-drop").classList.remove("over");
  const file = (e as DragEvent).dataTransfer?.files?.[0];
  if (file) installFile(file);
});

/* ---- 官方市场 ---- */

/* 整份目录一次拉完（不到 200KB）缓存在内存里，搜索和分类筛选纯前端做，
 * 敲字不打服务端。 */
const MK: { plugins: MarketPlugin[]; loaded: boolean } = { plugins: [], loaded: false };

function marketMsg(msg: string) {
  const p = document.createElement("p");
  p.className = "market-empty";
  p.textContent = msg;
  $("market-list").replaceChildren(p);
}

async function loadMarket(force: boolean) {
  if (MK.loaded && !force) { renderMarket(); return; }
  $("market-list").replaceChildren(loadingRow("拉取官方目录中…"));
  try {
    const data = await api<MarketCatalog>("/marketplace" + (force ? "?refresh=1" : ""));
    MK.plugins = data.plugins || [];
    MK.loaded = true;
    const sel = $<HTMLSelectElement>("market-cat");
    const cur = sel.value;
    const opts = [{ v: "", t: "全部分类" }, ...(data.categories || []).map((c) => ({ v: c, t: c }))];
    sel.replaceChildren(...opts.map(({ v, t }) => {
      const o = document.createElement("option");
      o.value = v;
      o.textContent = t;
      return o;
    }));
    setSelectValue(sel, cur);
    renderMarket();
  } catch (e) {
    marketMsg("拉取失败：" + (e as Error).message);
  }
}

function renderMarket() {
  const q = $<HTMLInputElement>("market-q").value.trim().toLowerCase();
  const cat = $<HTMLSelectElement>("market-cat").value;
  const hit = MK.plugins.filter((p) => {
    if (cat && p.category !== cat) return false;
    if (!q) return true;
    return [p.name, p.display_name, p.description, p.author, ...(p.keywords || [])]
      .some((s) => (s || "").toLowerCase().includes(q));
  });
  if (!hit.length) {
    marketMsg(MK.plugins.length ? "没有匹配的插件。" : "目录是空的。");
    return;
  }
  const frag = document.createDocumentFragment();
  for (const p of hit.slice(0, 300)) {
    const row = document.createElement("div");
    row.className = "market-row";

    const info = document.createElement("div");
    info.className = "market-info";
    const line = document.createElement("div");
    line.className = "market-name";
    const nm = document.createElement("b");
    nm.textContent = p.display_name || p.name;
    line.appendChild(nm);
    for (const [text, cls] of [
      [p.author, ""],
      [p.category, ""],
      [p.skills ? `含 ${p.skills} 个技能` : "", "has-skills"],
    ] as [string, string][]) {
      if (!text) continue;
      const tag = document.createElement("span");
      tag.className = "market-tag" + (cls ? " " + cls : "");
      tag.textContent = text;
      line.appendChild(tag);
    }
    const desc = document.createElement("div");
    desc.className = "market-desc";
    desc.textContent = p.description || "";
    info.append(line, desc);

    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn-sm";
    buttonLabel(btn, "安装", "package-plus");
    btn.addEventListener("click", () => installFromMarket(p.name, btn));

    row.append(info, btn);
    frag.appendChild(row);
  }
  $("market-list").replaceChildren(frag);
}

async function installFromMarket(name: string, btn: HTMLButtonElement) {
  const sess = S.current;
  if (!sess) return;
  btnBusy(btn, "安装中…");
  try {
    const res = await api<{ installed: string[] }>(
      `/sessions/${sess.id}/skills/market?scope=${SK.scope}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      });
    const list = res.installed || [];
    await afterInstall(list[0] || "", list.length > 1
      ? `已装 ${list.length} 个技能：${list.join("、")}`
      : `已安装技能「${list[0]}」`);
  } catch (e) {
    // 纯命令/MCP 插件会走到这里，服务端给的文案已经说明该怎么办
    toast((e as Error).message, true);
  } finally {
    btnDone(btn);
  }
}

$("market-q").addEventListener("input", renderMarket);
$("market-cat").addEventListener("change", renderMarket);
$("btn-market-refresh").addEventListener("click", () => loadMarket(true));
