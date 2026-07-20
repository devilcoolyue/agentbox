/* files：文件页 —— 工作区/共享目录切换、懒加载目录树、上传/下载。
 * 单文件预览/编辑弹窗在 preview.js。 */
"use strict";

import { S } from "./state.js";
import { $, spinEl, btnBusy, btnDone, fmtSize, isMobile, onMobileChange } from "./util.js";
import { api, scopeQS } from "./api.js";
import { openPreview } from "./preview.js";

/* 树形状态：expanded=已展开的目录（相对当前根），cache=已拉取的目录列表（"" 为当前根），
 * gen=竞态防护——快速连续导航时只让最后一次请求的结果上屏 */
const tree = { expanded: new Set(), cache: new Map(), gen: 0 };

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

function joinRel(base, name) {
  return base ? base + "/" + name : name;
}

async function fetchDir(rel) {
  const full = joinRel(S.filePath, rel);
  return api(`/sessions/${S.current.id}/files?path=${encodeURIComponent(full)}${scopeQS()}`);
}

export async function loadFiles() {
  const sess = S.current; if (!sess) return;
  const gen = ++tree.gen;
  tree.cache.clear(); // 每次都取最新内容，展开状态保留
  $("files-list").replaceChildren(filesLoadingRow());
  let entries;
  try {
    entries = await api(`/sessions/${sess.id}/files?path=${encodeURIComponent(S.filePath)}${scopeQS()}`);
  } catch (e) {
    if (gen !== tree.gen) return;
    $("files-head").classList.add("hidden");
    $("files-list").replaceChildren(Object.assign(document.createElement("p"), {
      className: "files-empty", textContent: "读取失败：" + e.message,
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
    const p = document.createElement("p");
    p.className = "files-empty";
    p.textContent = S.fileScope === "shared"
      ? "共享目录为空。放到这里的文件对本账号所有会话可见（容器内路径 /shared）。"
      : "工作区为空。上传代码包，或直接在对话里让 Agent 创建项目。";
    frag.appendChild(p);
  }
  await buildRows(frag, entries, "", 0);
  if (gen !== tree.gen) return;
  $("files-head").classList.toggle("hidden", !entries.length && !S.filePath);
  $("files-list").replaceChildren(frag);
}

/* 递归拼装树：展开的目录懒加载子项后缩进列出 */
async function buildRows(frag, entries, base, depth) {
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
        p.style.setProperty("--depth", depth + 1);
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
  label.textContent = "‹ 上一级";
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
  row.append(cell(""), cell("", "num"), cell(""));
  return row;
}

function cell(text, extra) {
  const s = document.createElement("span");
  s.className = extra || "";
  s.textContent = text;
  return s;
}

function fileRow(ent, rel, depth) {
  const row = document.createElement("div");
  row.className = "file-row" + (ent.is_dir ? " dir" : "");
  row.style.setProperty("--depth", depth);

  const name = document.createElement("span");
  name.className = "fname";

  if (ent.is_dir) {
    // 箭头：原地展开/收起下级
    const arrow = document.createElement("span");
    arrow.className = "farrow" + (tree.expanded.has(rel) ? " open" : "");
    arrow.textContent = "▸";
    arrow.title = tree.expanded.has(rel) ? "收起" : "展开下级";
    arrow.setAttribute("role", "button");
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
    glyph.textContent = fileGlyph(ent.name);
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
  label.title = ent.is_dir ? "进入目录" : "预览 / 编辑";
  label.addEventListener("click", open);
  label.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
  name.appendChild(label);
  row.appendChild(name);

  row.append(
    cell(ent.mode || "", "fperm"),
    cell(ent.is_dir ? "" : fmtSize(ent.size), "fsize num"),
    cell(ent.mtime ? new Date(ent.mtime).toLocaleString() : "", "ftime"),
  );
  return row;
}

/* 收起/展开时用缓存重绘，不重新请求 */
async function rerenderTree() {
  const sess = S.current; if (!sess) return;
  const gen = ++tree.gen;
  let entries = tree.cache.get("");
  if (!entries) {
    try {
      entries = await api(`/sessions/${sess.id}/files?path=${encodeURIComponent(S.filePath)}${scopeQS()}`);
    } catch (_) { return; }
    if (gen !== tree.gen) return;
    tree.cache.set("", entries);
  }
  const frag = document.createDocumentFragment();
  if (S.filePath) frag.appendChild(upRow());
  await buildRows(frag, entries, "", 0);
  if (gen !== tree.gen) return;
  $("files-list").replaceChildren(frag);
}

function fileGlyph(name) {
  const ext = (name.split(".").pop() || "").toLowerCase();
  if (["png", "jpg", "jpeg", "gif", "svg", "webp", "ico", "bmp"].includes(ext)) return "◨";
  if (["zip", "tar", "gz", "tgz", "7z", "rar"].includes(ext)) return "▣";
  if (["sh", "py", "js", "ts", "go", "rs", "c", "h", "cpp", "java", "rb", "php"].includes(ext)) return "⌘";
  return "·";
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
  return S.fileScope === "shared" ? "共享目录" : "工作区";
}

/* 工具条按钮文案：窄屏用短词（上传 / 下载 zip / 上传前清空） */
function updateFileLabels() {
  const short = isMobile();
  $("btn-upload").textContent = short ? "上传" : "上传代码包";
  $("btn-download").textContent = short ? "下载 zip" : `下载${scopeLabel()} (zip)`;
  $("upload-clear-label").textContent = short ? "上传前清空" : "上传前清空" + scopeLabel();
}
onMobileChange(updateFileLabels);
updateFileLabels();

function setFileScope(scope) {
  if (S.fileScope === scope) return;
  S.fileScope = scope;
  S.filePath = "";
  resetTree();
  $("scope-ws").classList.toggle("active", scope === "workspace");
  $("scope-shared").classList.toggle("active", scope === "shared");
  updateFileLabels();
  $("upload-clear").checked = false;
  loadFiles();
}

$("scope-ws").addEventListener("click", () => setFileScope("workspace"));
$("scope-shared").addEventListener("click", () => setFileScope("shared"));

$("btn-upload").addEventListener("click", () => $("upload-input").click());
$("upload-input").addEventListener("change", async () => {
  const file = $("upload-input").files[0];
  if (!file || !S.current) return;
  const fd = new FormData();
  fd.append("file", file);
  if ($("upload-clear").checked) fd.append("clear", "1");
  btnBusy($("btn-upload"), "上传中…");
  $("btn-download").disabled = true;   // 上传期间锁住相关操作
  $("scope-ws").disabled = true;
  $("scope-shared").disabled = true;
  try {
    const res = await api(`/sessions/${S.current.id}/upload?x=1${scopeQS()}`, { method: "POST", body: fd });
    alert(res.mode === "archive" ? `已解压 ${res.files} 个文件到${scopeLabel()}` : "文件已上传到" + scopeLabel());
    loadFiles();
  } catch (e) {
    alert("上传失败：" + e.message);
  } finally {
    btnDone($("btn-upload"));
    updateFileLabels(); // btnDone 恢复的是点击时文案，断点可能已变
    $("btn-download").disabled = false;
    $("scope-ws").disabled = false;
    $("scope-shared").disabled = false;
    $("upload-input").value = "";
  }
});

$("btn-download").addEventListener("click", () => {
  if (!S.current) return;
  const a = document.createElement("a");
  a.href = `/api/sessions/${S.current.id}/archive?token=${encodeURIComponent(S.token)}${scopeQS()}`;
  a.download = "";
  a.click();
});
