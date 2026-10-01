/* files：文件页 —— 工作区/共享目录切换、懒加载目录树、上传/下载。
 * 单文件预览/编辑弹窗在 preview.ts。 */
"use strict";

import { actionButton, buttonLabel, fileIconName } from "./icons.js";

import { S, emit } from "./state.js";
import type { FileScope } from "./state.js";
import type { FileEntry, UploadSummary } from "./types.js";
import { $, askConfirm, fmtDateTime, isImeEnter, isMobile, spinEl, btnBusy, btnDone, fmtSize, startDownload, toast } from "./util.js";
import { api, archiveDownloadURL, fileDownloadURL, scopeQS } from "./api.js";
import { openPreview } from "./preview.js";
import { svgIcon } from "./chat-render.js";
import { setTip } from "./tip.js";
import { bindMenu, moreButton } from "./menu.js";

/* 树形状态：expanded=已展开的目录（相对当前根），cache=已拉取的目录列表（"" 为当前根），
 * gen=竞态防护——快速连续导航时只让最后一次请求的结果上屏 */
const tree = {
  expanded: new Set<string>(),
  cache: new Map<string, FileEntry[]>(),
  gen: 0,
};

export function resetTree() {
  tree.expanded.clear();
  tree.cache.clear();
}

function filesLoadingRow() {
  const d = document.createElement("div");
  d.className = "loading-block";
  d.append(spinEl(), document.createTextNode("读取目录中…"));
  return d;
}

function filesEmptyRow() {
  const p = document.createElement("p");
  p.className = "files-empty";
  p.textContent = S.fileScope === "shared"
    ? "共享目录为空。这里的文件供你名下的所有工作空间读写（/shared）。"
    : "当前空间还没有文件。上传代码包，或在对话中让 Agent 创建项目。";
  return p;
}

function joinRel(base: string, name: string) {
  return base ? base + "/" + name : name;
}

async function fetchDir(rel: string) {
  const full = joinRel(S.filePath, rel);
  return api<FileEntry[]>(`/sessions/${S.current!.id}/files?path=${encodeURIComponent(full)}${scopeQS()}`);
}

export async function loadFiles() {
  const sess = S.current; if (!sess) return;
  const gen = ++tree.gen;
  tree.cache.clear(); // 每次都取最新内容，展开状态保留
  $("files-list").replaceChildren(filesLoadingRow());
  let entries: FileEntry[];
  try {
    entries = await api<FileEntry[]>(`/sessions/${sess.id}/files?path=${encodeURIComponent(S.filePath)}${scopeQS()}`);
  } catch (e) {
    if (gen !== tree.gen) return;
    $("files-head").classList.add("hidden");
    $("files-list").replaceChildren(Object.assign(document.createElement("p"), {
      className: "files-empty", textContent: "读取失败：" + (e as Error).message,
    }));
    return;
  }
  if (gen !== tree.gen) return; // 期间已发起新的导航，丢弃本次结果
  tree.cache.set("", entries);
  renderCrumb();
  const frag = document.createDocumentFragment();
  if (S.filePath) {
    frag.appendChild(upRow());
  }
  if (!entries.length && !S.filePath) {
    frag.appendChild(filesEmptyRow());
  }
  await buildRows(frag, entries, "", 0);
  if (gen !== tree.gen) return;
  $("files-head").classList.toggle("hidden", !entries.length && !S.filePath);
  $("files-list").replaceChildren(frag);
}

/* 递归拼装树：展开的目录懒加载子项后缩进列出 */
async function buildRows(frag: DocumentFragment, entries: FileEntry[], base: string, depth: number) {
  for (const ent of entries) {
    const rel = joinRel(base, ent.name);
    frag.appendChild(fileRow(ent, rel, depth));
    if (ent.is_dir && tree.expanded.has(rel)) {
      let kids = tree.cache.get(rel);
      if (!kids) {
        try { kids = await fetchDir(rel); } catch (_) { kids = []; }
        tree.cache.set(rel, kids);
      }
      if (!kids.length) {
        const p = document.createElement("div");
        p.className = "file-row empty-dir";
        p.style.setProperty("--depth", String(depth + 1));
        p.innerHTML = '<span class="fname muted">（空目录）</span>';
        frag.appendChild(p);
      } else {
        await buildRows(frag, kids, rel, depth + 1);
      }
    }
  }
}

function upRow() {
  const row = document.createElement("div");
  row.className = "file-row dir";
  const name = document.createElement("span");
  name.className = "fname";
  const label = document.createElement("span");
  label.className = "flabel";
  buttonLabel(label, "上一级", "arrow-left");
  label.setAttribute("role", "button");
  label.tabIndex = 0;
  const up = () => {
    S.filePath = S.filePath.split("/").slice(0, -1).join("/");
    resetTree();
    loadFiles();
  };
  label.addEventListener("click", up);
  label.addEventListener("keydown", (e) => { if (e.key === "Enter") up(); });
  name.appendChild(label);
  row.appendChild(name);
  row.append(cell(""), cell("", "num"), cell(""), cell(""));
  return row;
}

function cell(text: string, extra?: string) {
  const s = document.createElement("span");
  s.className = extra || "";
  s.textContent = text;
  return s;
}

function fileRow(ent: FileEntry, rel: string, depth: number) {
  const row = document.createElement("div");
  row.className = "file-row" + (ent.is_dir ? " dir" : "");
  row.style.setProperty("--depth", String(depth));

  const name = document.createElement("span");
  name.className = "fname";

  if (ent.is_dir) {
    // 箭头：原地展开/收起下级
    const arrow = document.createElement("span");
    arrow.className = "farrow" + (tree.expanded.has(rel) ? " open" : "");
    arrow.append(svgIcon("chevron-right", 12));
    setTip(arrow, tree.expanded.has(rel) ? "收起" : "展开下级");
    arrow.setAttribute("role", "button");
    arrow.setAttribute("aria-label", tree.expanded.has(rel) ? "收起目录" : "展开目录");
    arrow.tabIndex = 0;
    const toggle = async () => {
      if (tree.expanded.has(rel)) {
        tree.expanded.delete(rel);
        rerenderTree(); // 收起走缓存，立即完成
        return;
      }
      tree.expanded.add(rel);
      if (!tree.cache.has(rel)) {
        arrow.classList.add("busy"); // 箭头原地转圈，等子目录列表返回
        arrow.replaceChildren(spinEl());
        try { tree.cache.set(rel, await fetchDir(rel)); }
        catch (_) { tree.cache.set(rel, []); }
      }
      rerenderTree();
    };
    arrow.addEventListener("click", toggle);
    arrow.addEventListener("keydown", (e) => { if (e.key === "Enter") toggle(); });
    name.appendChild(arrow);
  } else {
    const glyph = document.createElement("span");
    glyph.className = "fglyph";
    glyph.append(svgIcon(fileIconName(ent.name), 15));
    name.appendChild(glyph);
  }

  // 名称：目录点击进入，文件点击预览/编辑
  const label = document.createElement("span");
  label.className = "flabel";
  label.textContent = ent.name;
  label.setAttribute("role", "button");
  label.tabIndex = 0;
  const open = ent.is_dir
    ? () => {
        S.filePath = joinRel(S.filePath, rel);
        resetTree();
        loadFiles();
      }
    : () => openPreview(joinRel(S.filePath, rel), ent);
  setTip(label, ent.is_dir ? "进入目录" : "预览 / 编辑");
  label.addEventListener("click", open);
  label.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
  // 窄屏收掉大小列，大小改作名称下方的副标题（桌面端隐藏）
  const text = document.createElement("span");
  text.className = "ftext";
  text.appendChild(label);
  if (!ent.is_dir) text.appendChild(cell(fmtSize(ent.size), "fsub"));
  name.appendChild(text);
  row.appendChild(name);

  row.append(
    cell(ent.mode || "", "fperm"),
    cell(ent.is_dir ? "" : fmtSize(ent.size), "fsize num"),
    cell(ent.mtime ? fmtDateTime(ent.mtime) : "", "ftime"),
    fileActions(ent, joinRel(S.filePath, rel)),
  );
  return row;
}

function fileAction(label: string, icon: string, action: () => void, danger = false) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "file-act" + (danger ? " danger" : "");
  setTip(btn, label);
  btn.setAttribute("aria-label", label);
  btn.appendChild(svgIcon(icon, 16));
  btn.addEventListener("click", action);
  return btn;
}

/* 每行最多露出两个常用动作（预览、下载），改名/移动/删除收进 ⋯，删除放最后。
 * 窄屏连下载也收进去：44px 触控按钮一行放不下四五个，会压住大小列。 */
function fileActions(ent: FileEntry, fullPath: string) {
  const actions = document.createElement("span");
  actions.className = "factions";
  // HTML / Markdown 直接给一个渲染入口：点名字进的也是同一个弹窗，但列表里
  // 一眼能看出哪些文件是能「看效果」的
  if (!ent.is_dir && /\.(html?|md|markdown|mdx)$/i.test(ent.name)) {
    const label = /\.html?$/i.test(ent.name) ? "预览页面" : "预览渲染";
    actions.append(fileAction(label, "eye", () => openPreview(fullPath, ent)));
  }
  // 目录没有单文件下载：整包下载走工具条的「下载 zip」
  const download = () => startDownload(fileDownloadURL(fullPath));
  if (!ent.is_dir) {
    const btn = fileAction("下载", "download", download);
    btn.classList.add("desktop-only");
    actions.append(btn);
  }
  const more = moreButton(() => [
    { label: "下载", icon: "download", run: download, hidden: ent.is_dir || !isMobile() },
    { label: "重命名", icon: "rename", run: () => openFileRename(ent, fullPath) },
    { label: "移动到…", icon: "move", run: () => openFileMove(ent, fullPath) },
    { label: "删除", icon: "trash", danger: true, sep: true, run: () => openFileDelete(ent, fullPath) },
  ], `${ent.name} 的更多操作`);
  more.className = "file-act file-more";
  actions.append(more);
  return actions;
}

/* ---------------- 移动 / 删除 ---------------- */

const fileOp: {
  entry: FileEntry | null;
  sourcePath: string;
  sourceScope: FileScope;
  moveScope: FileScope;
  movePath: string;
  moveGen: number;
  moveBusy: boolean;
  deleteBusy: boolean;
} = {
  entry: null,
  sourcePath: "",
  sourceScope: "workspace",
  moveScope: "workspace",
  movePath: "",
  moveGen: 0,
  moveBusy: false,
  deleteBusy: false,
};

function scopeRoot(scope: FileScope) {
  return scope === "shared" ? "/shared" : "/workspace";
}

function fullScopePath(scope: FileScope, rel: string) {
  return scopeRoot(scope) + (rel ? "/" + rel : "");
}

function scopeParam(scope: FileScope) {
  return scope === "shared" ? "&scope=shared" : "";
}

function parentRel(path: string) {
  return path.split("/").slice(0, -1).join("/");
}

/* 删除成功后只改命中节点所属目录的缓存。可见行一定来自 tree.cache，
 * 因而正常路径不会重新请求目录，也不会丢掉其它目录的展开状态。 */
function removeTreeEntry(fullPath: string, isDir: boolean) {
  const prefix = S.filePath ? S.filePath + "/" : "";
  if (prefix && !fullPath.startsWith(prefix)) return false;
  const rel = prefix ? fullPath.slice(prefix.length) : fullPath;
  if (!rel) return false;
  const parent = parentRel(rel);
  const entries = tree.cache.get(parent);
  if (!entries) return false;
  const name = rel.split("/").pop();
  const next = entries.filter((entry) => entry.name !== name);
  if (next.length === entries.length) return false;
  tree.cache.set(parent, next);

  if (isDir) {
    for (const key of [...tree.cache.keys()]) {
      if (key === rel || key.startsWith(rel + "/")) tree.cache.delete(key);
    }
    for (const key of [...tree.expanded]) {
      if (key === rel || key.startsWith(rel + "/")) tree.expanded.delete(key);
    }
  }
  rerenderTree();
  return true;
}

function setInlineError(id: string, message: string) {
  const el = $(id);
  el.textContent = message || "";
  el.classList.toggle("hidden", !message);
}

function moveIsNoop() {
  return fileOp.moveScope === fileOp.sourceScope && fileOp.movePath === parentRel(fileOp.sourcePath);
}

function updateMoveTarget() {
  $("file-move-target").textContent = fullScopePath(fileOp.moveScope, fileOp.movePath);
  $<HTMLButtonElement>("file-move-ok").disabled = fileOp.moveBusy || moveIsNoop();
  setTip($("file-move-ok"), moveIsNoop() ? "当前项目已在此目录" : "");
}

function renderMoveCrumb() {
  const crumb = $("file-move-crumb");
  crumb.replaceChildren();
  const root = document.createElement("button");
  root.type = "button";
  actionButton(root, scopeRoot(fileOp.moveScope), "folder", "进入 " + scopeRoot(fileOp.moveScope));
  root.addEventListener("click", () => { fileOp.movePath = ""; loadMoveDirs(); });
  crumb.appendChild(root);
  let acc = "";
  for (const part of fileOp.movePath.split("/").filter(Boolean)) {
    crumb.appendChild(document.createTextNode(" / "));
    acc = joinRel(acc, part);
    const next = document.createElement("button");
    const target = acc;
    next.type = "button";
    actionButton(next, part, "folder", "进入 " + fullScopePath(fileOp.moveScope, target));
    next.addEventListener("click", () => { fileOp.movePath = target; loadMoveDirs(); });
    crumb.appendChild(next);
  }
}

function moveDirDisabled(path: string) {
  if (!fileOp.entry?.is_dir || fileOp.moveScope !== fileOp.sourceScope) return false;
  return path === fileOp.sourcePath || path.startsWith(fileOp.sourcePath + "/");
}

async function loadMoveDirs() {
  const sess = S.current;
  if (!sess || !fileOp.entry) return;
  const gen = ++fileOp.moveGen;
  renderMoveCrumb();
  updateMoveTarget();
  setInlineError("file-move-error", "");
  $("file-move-dirs").replaceChildren(filesLoadingRow());
  let entries: FileEntry[];
  try {
    entries = await api<FileEntry[]>(`/sessions/${sess.id}/files?path=${encodeURIComponent(fileOp.movePath)}${scopeParam(fileOp.moveScope)}`);
  } catch (e) {
    if (gen !== fileOp.moveGen) return;
    $("file-move-dirs").replaceChildren();
    setInlineError("file-move-error", "读取目标目录失败：" + (e as Error).message);
    return;
  }
  if (gen !== fileOp.moveGen) return;
  const frag = document.createDocumentFragment();
  const dirs = entries.filter((entry) => entry.is_dir);
  for (const dir of dirs) {
    const path = joinRel(fileOp.movePath, dir.name);
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "file-move-dir mono";
    btn.disabled = moveDirDisabled(path);
    setTip(btn, btn.disabled ? "不能移动到自身或其子目录" : "进入目录");
    const name = document.createElement("span");
    name.className = "dir-name";
    name.textContent = dir.name;
    const arrow = document.createElement("span");
    arrow.className = "dir-arrow";
    arrow.appendChild(svgIcon("caret", 13));
    btn.append(svgIcon("folder", 17), name, arrow);
    btn.addEventListener("click", () => { fileOp.movePath = path; loadMoveDirs(); });
    frag.appendChild(btn);
  }
  if (!dirs.length) {
    const empty = document.createElement("p");
    empty.className = "file-move-empty";
    empty.textContent = "当前目录下没有子目录";
    frag.appendChild(empty);
  }
  $("file-move-dirs").replaceChildren(frag);
}

function setMoveScope(scope: FileScope) {
  if (fileOp.moveBusy || fileOp.moveScope === scope) return;
  fileOp.moveScope = scope;
  fileOp.movePath = "";
  $("file-move-ws").classList.toggle("active", scope === "workspace");
  $("file-move-shared").classList.toggle("active", scope === "shared");
  loadMoveDirs();
}

function openFileMove(ent: FileEntry, path: string) {
  fileOp.entry = ent;
  fileOp.sourcePath = path;
  fileOp.sourceScope = S.fileScope;
  fileOp.moveScope = S.fileScope;
  fileOp.movePath = parentRel(path);
  fileOp.moveBusy = false;
  $("file-move-title").textContent = ent.is_dir ? "移动文件夹" : "移动文件";
  $("file-move-source").textContent = fullScopePath(fileOp.sourceScope, path);
  $("file-move-ws").classList.toggle("active", fileOp.moveScope === "workspace");
  $("file-move-shared").classList.toggle("active", fileOp.moveScope === "shared");
  setInlineError("file-move-error", "");
  $<HTMLDialogElement>("dlg-file-move").showModal();
  loadMoveDirs();
}

function closeFileMove() {
  if (!fileOp.moveBusy) $<HTMLDialogElement>("dlg-file-move").close();
}

$("file-move-ws").addEventListener("click", () => setMoveScope("workspace"));
$("file-move-shared").addEventListener("click", () => setMoveScope("shared"));
$("file-move-close").addEventListener("click", closeFileMove);
$("file-move-cancel").addEventListener("click", closeFileMove);
$("dlg-file-move").addEventListener("cancel", (e) => { if (fileOp.moveBusy) e.preventDefault(); });
$("dlg-file-move").addEventListener("close", () => { fileOp.moveGen++; fileOp.entry = null; });
$("file-move-ok").addEventListener("click", async () => {
  const sess = S.current;
  if (!sess || !fileOp.entry || fileOp.moveBusy || moveIsNoop()) return;
  fileOp.moveBusy = true;
  setInlineError("file-move-error", "");
  btnBusy($("file-move-ok"), "移动中…");
  $<HTMLButtonElement>("file-move-close").disabled = true;
  $<HTMLButtonElement>("file-move-cancel").disabled = true;
  $<HTMLButtonElement>("file-move-ws").disabled = true;
  $<HTMLButtonElement>("file-move-shared").disabled = true;
  try {
    const wasDir = fileOp.entry.is_dir;
    const destinationLabel = fullScopePath(fileOp.moveScope, fileOp.movePath);
    await api(`/sessions/${sess.id}/files/move`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        source_scope: fileOp.sourceScope,
        source_path: fileOp.sourcePath,
        destination_scope: fileOp.moveScope,
        destination_dir: fileOp.movePath,
      }),
    });
    $<HTMLDialogElement>("dlg-file-move").close();
    toast(`${wasDir ? "文件夹" : "文件"}已移动到 ${destinationLabel}`);
    resetTree();
    loadFiles();
  } catch (e) {
    setInlineError("file-move-error", "移动失败：" + (e as Error).message);
  } finally {
    fileOp.moveBusy = false;
    btnDone($("file-move-ok"));
    $<HTMLButtonElement>("file-move-close").disabled = false;
    $<HTMLButtonElement>("file-move-cancel").disabled = false;
    $<HTMLButtonElement>("file-move-ws").disabled = false;
    $<HTMLButtonElement>("file-move-shared").disabled = false;
    updateMoveTarget();
  }
});

function openFileDelete(ent: FileEntry, path: string) {
  fileOp.entry = ent;
  fileOp.sourcePath = path;
  fileOp.sourceScope = S.fileScope;
  fileOp.deleteBusy = false;
  $("file-delete-title").textContent = ent.is_dir ? "删除文件夹" : "删除文件";
  $("file-delete-text").textContent = `确认删除「${fullScopePath(fileOp.sourceScope, path)}」？`;
  $("file-delete-warning").textContent = ent.is_dir
    ? "该文件夹及其中所有内容都会被删除，此操作不可恢复。"
    : "此操作不可恢复。";
  setInlineError("file-delete-error", "");
  $<HTMLDialogElement>("dlg-file-delete").showModal();
}

function closeFileDelete() {
  if (!fileOp.deleteBusy) $<HTMLDialogElement>("dlg-file-delete").close();
}

$("file-delete-close").addEventListener("click", closeFileDelete);
$("file-delete-cancel").addEventListener("click", closeFileDelete);
$("dlg-file-delete").addEventListener("cancel", (e) => { if (fileOp.deleteBusy) e.preventDefault(); });
$("dlg-file-delete").addEventListener("close", () => { if (!fileOp.moveBusy) fileOp.entry = null; });
$("file-delete-ok").addEventListener("click", async () => {
  const sess = S.current;
  if (!sess || !fileOp.entry || fileOp.deleteBusy) return;
  const wasDir = fileOp.entry.is_dir;
  const sourcePath = fileOp.sourcePath;
  const sourceScope = fileOp.sourceScope;
  fileOp.deleteBusy = true;
  setInlineError("file-delete-error", "");
  btnBusy($("file-delete-ok"), "删除中…");
  $<HTMLButtonElement>("file-delete-close").disabled = true;
  $<HTMLButtonElement>("file-delete-cancel").disabled = true;
  try {
    await api(`/sessions/${sess.id}/files?path=${encodeURIComponent(sourcePath)}${scopeParam(sourceScope)}`, { method: "DELETE" });
    $<HTMLDialogElement>("dlg-file-delete").close();
    toast(wasDir ? "文件夹已删除" : "文件已删除");
    if (sourceScope !== S.fileScope || !removeTreeEntry(sourcePath, wasDir)) loadFiles();
  } catch (e) {
    setInlineError("file-delete-error", "删除失败：" + (e as Error).message);
  } finally {
    fileOp.deleteBusy = false;
    btnDone($("file-delete-ok"));
    $<HTMLButtonElement>("file-delete-close").disabled = false;
    $<HTMLButtonElement>("file-delete-cancel").disabled = false;
  }
});

/* ---------------- 新建文件夹 / 重命名（复用 dlg-file-name） ---------------- */

const nameOp: {
  /** "rename" | "mkdir" */
  mode: string;
  entry: FileEntry | null;
  sourcePath: string;
  scope: FileScope;
  busy: boolean;
} = { mode: "", entry: null, sourcePath: "", scope: "workspace", busy: false };

function openMkdir() {
  if (!S.current) return;
  nameOp.mode = "mkdir";
  nameOp.entry = null;
  nameOp.scope = S.fileScope;
  nameOp.busy = false;
  $("file-name-title").textContent = "新建文件夹";
  $("file-name-label").textContent = `在 ${fullScopePath(S.fileScope, S.filePath)} 下新建`;
  $<HTMLInputElement>("file-name-input").value = "";
  setInlineError("file-name-error", "");
  $<HTMLDialogElement>("dlg-file-name").showModal();
  $("file-name-input").focus();
}

function openFileRename(ent: FileEntry, path: string) {
  nameOp.mode = "rename";
  nameOp.entry = ent;
  nameOp.sourcePath = path;
  nameOp.scope = S.fileScope;
  nameOp.busy = false;
  $("file-name-title").textContent = ent.is_dir ? "重命名文件夹" : "重命名文件";
  $("file-name-label").textContent = "新名称";
  $<HTMLInputElement>("file-name-input").value = ent.name;
  setInlineError("file-name-error", "");
  $<HTMLDialogElement>("dlg-file-name").showModal();
  const input = $<HTMLInputElement>("file-name-input");
  input.focus();
  // 选中主名部分（不含扩展名），改名更顺手
  const dot = ent.is_dir ? -1 : ent.name.lastIndexOf(".");
  input.setSelectionRange(0, dot > 0 ? dot : ent.name.length);
}

function closeFileName() {
  if (!nameOp.busy) $<HTMLDialogElement>("dlg-file-name").close();
}

async function submitFileName() {
  const sess = S.current;
  if (!sess || nameOp.busy) return;
  const name = $<HTMLInputElement>("file-name-input").value.trim();
  if (!name) { setInlineError("file-name-error", "名称不能为空"); return; }
  if (/[/\\]/.test(name)) { setInlineError("file-name-error", "名称不能包含斜杠"); return; }
  nameOp.busy = true;
  setInlineError("file-name-error", "");
  btnBusy($("file-name-ok"), "提交中…");
  try {
    if (nameOp.mode === "mkdir") {
      await api(`/sessions/${sess.id}/files/mkdir`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ scope: nameOp.scope, dir: S.filePath, name }),
      });
      toast("文件夹已创建");
    } else {
      await api(`/sessions/${sess.id}/files/rename`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ scope: nameOp.scope, path: nameOp.sourcePath, name }),
      });
      toast("已重命名");
    }
    $<HTMLDialogElement>("dlg-file-name").close();
    resetTree();
    loadFiles();
  } catch (e) {
    setInlineError("file-name-error", (nameOp.mode === "mkdir" ? "创建失败：" : "重命名失败：") + (e as Error).message);
  } finally {
    nameOp.busy = false;
    btnDone($("file-name-ok"));
  }
}

$("btn-mkdir").addEventListener("click", openMkdir);
$("file-name-close").addEventListener("click", closeFileName);
$("file-name-cancel").addEventListener("click", closeFileName);
$("file-name-ok").addEventListener("click", submitFileName);
$("file-name-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !isImeEnter(e)) { e.preventDefault(); submitFileName(); }
});
$("dlg-file-name").addEventListener("cancel", (e) => { if (nameOp.busy) e.preventDefault(); });

/* 收起/展开时用缓存重绘，不重新请求 */
async function rerenderTree() {
  const sess = S.current; if (!sess) return;
  const gen = ++tree.gen;
  let entries = tree.cache.get("");
  if (!entries) {
    try {
      entries = await api<FileEntry[]>(`/sessions/${sess.id}/files?path=${encodeURIComponent(S.filePath)}${scopeQS()}`);
    } catch (_) { return; }
    if (gen !== tree.gen) return;
    tree.cache.set("", entries);
  }
  const frag = document.createDocumentFragment();
  if (S.filePath) frag.appendChild(upRow());
  if (!entries.length && !S.filePath) frag.appendChild(filesEmptyRow());
  await buildRows(frag, entries, "", 0);
  if (gen !== tree.gen) return;
  $("files-head").classList.toggle("hidden", !entries.length && !S.filePath);
  $("files-list").replaceChildren(frag);
}

function renderCrumb() {
  const crumb = $("files-crumb");
  crumb.replaceChildren();
  const root = document.createElement("a");
  root.textContent = S.fileScope === "shared" ? "/shared" : "/workspace";
  root.addEventListener("click", () => { S.filePath = ""; resetTree(); loadFiles(); });
  crumb.appendChild(root);
  let acc = "";
  for (const part of S.filePath.split("/").filter(Boolean)) {
    crumb.appendChild(document.createTextNode(" / "));
    acc = acc ? acc + "/" + part : part;
    const a = document.createElement("a");
    const target = acc;
    a.textContent = part;
    a.addEventListener("click", () => { S.filePath = target; resetTree(); loadFiles(); });
    crumb.appendChild(a);
  }
}

export function scopeLabel() {
  return S.fileScope === "shared" ? "共享目录" : "空间文件";
}

function updateFileLabels() {
  actionButton($("btn-upload"), "上传", "upload", "上传文件或代码包到当前目录，压缩包会自动解压");
  actionButton($("btn-download"), "下载", "download", `下载全部${scopeLabel()}（ZIP）`);
}
updateFileLabels();

function setFileScope(scope: FileScope) {
  if (S.fileScope === scope) return;
  S.fileScope = scope;
  S.filePath = "";
  resetTree();
  $("scope-ws").classList.toggle("active", scope === "workspace");
  $("scope-shared").classList.toggle("active", scope === "shared");
  updateFileLabels();
  loadFiles();
}

$("scope-ws").addEventListener("click", () => setFileScope("workspace"));
$("scope-shared").addEventListener("click", () => setFileScope("shared"));

/* 「清空后上传」是一次性的显式动作：从 ⋯ 菜单进入，选完文件后再确认一次，
 * 不留常驻勾选状态——旧版勾选框会让之后每一次上传和拖拽都先清空整个根目录。 */
let clearNextUpload = false;
function pickUpload(clear: boolean) {
  clearNextUpload = clear;
  $("upload-input").click();
}
$("btn-upload").addEventListener("click", () => pickUpload(false));
$("upload-input").addEventListener("change", async () => {
  const input = $<HTMLInputElement>("upload-input");
  const files = [...(input.files || [])];
  const clear = clearNextUpload;
  clearNextUpload = false;
  input.value = "";
  if (!files.length) return;
  if (clear && !(await confirmClearUpload(files.length))) return;
  uploadFiles(files, clear);
});
$("upload-input").addEventListener("cancel", () => { clearNextUpload = false; });
actionButton($("btn-files-more"), "", "more", "更多操作");
bindMenu($("btn-files-more"), () => [
  { label: "清空当前目录后上传…", icon: "upload", danger: true, run: () => pickUpload(true),
    tip: "先删除当前目录下的全部内容，再上传" },
]);

async function confirmClearUpload(count: number) {
  const sess = S.current;
  if (!sess) return false;
  const dir = fullScopePath(S.fileScope, S.filePath);
  let existing = 0;
  try {
    existing = (await api<FileEntry[]>(`/sessions/${sess.id}/files?path=${encodeURIComponent(S.filePath)}${scopeQS()}`)).length;
  } catch (e) {
    toast("读取当前目录失败：" + (e as Error).message, true);
    return false;
  }
  const what = existing ? `会先删除 ${dir} 下的 ${existing} 项（含子目录），` : `${dir} 目前是空的，`;
  return askConfirm(`${what}再上传 ${count} 个文件。`, {
    title: "清空后上传",
    hint: S.fileScope === "shared" && !S.filePath
      ? "共享目录供你名下所有工作空间使用，对话里粘贴的图片和附件也存在这里，会一起删除。删除后无法恢复。"
      : "删除后无法恢复。",
    okLabel: existing ? "清空并上传" : "上传",
    icon: existing ? "trash" : "upload",
    danger: existing > 0,
  });
}

/* fetch 拿不到上传进度，改用 XHR 手动带令牌与进度回调 */
function uploadWithProgress(
  url: string, formData: FormData, onProgress: (frac: number) => void,
): Promise<UploadSummary> {
  return new Promise<UploadSummary>((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", "/api" + url);
    xhr.setRequestHeader("Authorization", "Bearer " + S.token);
    xhr.upload.addEventListener("progress", (e) => {
      if (e.lengthComputable) onProgress(e.loaded / e.total);
    });
    xhr.addEventListener("load", () => {
      if (xhr.status === 401) { emit("unauthorized"); reject(new Error("unauthorized")); return; }
      let data: Partial<UploadSummary> & { error?: string } = {};
      try { data = JSON.parse(xhr.responseText); } catch (_) { /* 非 JSON 保持空 */ }
      if (xhr.status >= 200 && xhr.status < 300) resolve(data as UploadSummary);
      else reject(new Error(data.error || xhr.statusText || "上传失败"));
    });
    xhr.addEventListener("error", () => reject(new Error("网络错误")));
    xhr.send(formData);
  });
}

function setUploadFill(frac: number) {
  $("upload-progress-fill").style.width = Math.round(frac * 100) + "%";
}

async function uploadFiles(files: File[], clearFirst = false) {
  if (!files.length || !S.current) return;
  const lock = ["btn-download", "scope-ws", "scope-shared", "btn-mkdir", "btn-files-more"];
  // 目标在开始时定下：上传过程中用户点进别的目录，后续文件也不会换地方
  const dir = S.filePath, dirLabel = fullScopePath(S.fileScope, dir);
  $("upload-progress").classList.remove("hidden");
  btnBusy($("btn-upload"), "上传中…");
  lock.forEach((id) => { $<HTMLButtonElement>(id).disabled = true; });
  let plain = 0, archived = 0;
  try {
    for (let i = 0; i < files.length; i++) {
      const file = files[i];
      const fd = new FormData();
      fd.append("file", file);
      if (clearFirst && i === 0) fd.append("clear", "1"); // 只让第一个清空，避免逐个清掉前一个
      $("upload-progress-text").textContent = `${file.name}（${i + 1}/${files.length}）`;
      setUploadFill(0);
      try {
        const res = await uploadWithProgress(`/sessions/${S.current!.id}/upload?path=${encodeURIComponent(dir)}${scopeQS()}`, fd, setUploadFill);
        if (res.mode === "archive") archived += res.files || 0; else plain++;
      } catch (e) {
        toast(`${file.name} 上传失败：${(e as Error).message}`, true);
      }
    }
    const parts = [];
    if (plain) parts.push(`${plain} 个文件`);
    if (archived) parts.push(`解压 ${archived} 个文件`);
    if (parts.length) toast(`已上传${parts.join("，")}到 ${dirLabel}`);
    loadFiles();
  } finally {
    $("upload-progress").classList.add("hidden");
    setUploadFill(0);
    btnDone($("btn-upload"));
    updateFileLabels(); // btnDone 恢复的是点击时文案，断点可能已变
    lock.forEach((id) => { $<HTMLButtonElement>(id).disabled = false; });
  }
}

/* 拖拽上传：把文件拖到文件页任意处即可 */
const filesTab = $("tab-files");
["dragenter", "dragover"].forEach((ev) => filesTab.addEventListener(ev, (e) => {
  if (!S.current || !(e as DragEvent).dataTransfer?.types?.includes("Files")) return;
  e.preventDefault();
  filesTab.classList.add("drag-over");
}));
filesTab.addEventListener("dragleave", (e) => {
  if (!filesTab.contains(e.relatedTarget as Node | null)) filesTab.classList.remove("drag-over");
});
filesTab.addEventListener("drop", (e) => {
  filesTab.classList.remove("drag-over");
  const files = e.dataTransfer?.files;
  if (!S.current || !files || !files.length) return;
  e.preventDefault();
  uploadFiles([...files]);
});

$("btn-download").addEventListener("click", () => {
  if (!S.current) return;
  startDownload(archiveDownloadURL());
});
