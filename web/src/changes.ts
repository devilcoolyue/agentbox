/* changes：Git 变更审查页 —— 列出 workspace 相对上次提交的改动，查看 diff，
 * 提交或丢弃。让「下发任务 → 审查改动 → 提交/回滚」的闭环不必切到终端。 */
"use strict";

import { S } from "./state.js";
import type { ChangeEntry, GitCommitResult, GitStatus } from "./types.js";
import { $, spinEl, toast, btnBusy, btnDone } from "./util.js";
import { api } from "./api.js";

const CH: { files: ChangeEntry[]; selected: string } = { files: [], selected: "" };

function loadingRow(text: string) {
  const d = document.createElement("div");
  d.className = "loading-block";
  d.append(spinEl(), document.createTextNode(text));
  return d;
}

function listMsg(msg: string) {
  const p = document.createElement("p");
  p.className = "files-empty";
  p.textContent = msg;
  $("changes-list").replaceChildren(p);
}

function diffMsg(msg: string) {
  const p = document.createElement("p");
  p.className = "files-empty";
  p.textContent = msg;
  $("changes-diff").replaceChildren(p);
}

export async function loadChanges() {
  const sess = S.current;
  if (!sess) return;
  $("changes-branch").textContent = "";
  $("changes-list").replaceChildren(loadingRow("读取变更中…"));
  $("changes-diff").replaceChildren();
  let data: GitStatus;
  try {
    data = await api<GitStatus>(`/sessions/${sess.id}/git/status`);
  } catch (e) {
    listMsg("读取变更失败：" + (e as Error).message);
    setActions(false);
    return;
  }
  if (!data.is_repo) {
    listMsg("工作区不是 Git 仓库。可在终端里 git init，或让 Agent 初始化后再来审查改动。");
    setActions(false);
    return;
  }
  CH.files = data.files || [];
  $("changes-branch").textContent = data.branch ? "分支 " + data.branch : "";
  renderList();
}

function setActions(on: boolean) {
  $<HTMLButtonElement>("btn-changes-commit").disabled = !on;
  $<HTMLButtonElement>("btn-changes-discard-all").disabled = !on;
}

const STATUS_LABEL: Record<string, string> = {
  "??": "新增", "A ": "新增", "AM": "新增",
  " M": "修改", "M ": "已暂存", "MM": "修改",
  " D": "删除", "D ": "删除", "R ": "重命名", "RM": "重命名",
};
function statusText(xy: string) {
  return STATUS_LABEL[xy] || xy.trim() || "变更";
}
function statusKind(xy: string) {
  if (xy === "??" || xy.includes("A")) return "add";
  if (xy.includes("D")) return "del";
  if (xy.includes("R")) return "ren";
  return "mod";
}

function renderList() {
  if (!CH.files.length) {
    listMsg("没有未提交的改动。");
    setActions(false);
    diffMsg("");
    return;
  }
  setActions(true);
  const frag = document.createDocumentFragment();
  for (const f of CH.files) {
    const row = document.createElement("div");
    row.className = "change-row" + (f.path === CH.selected ? " active" : "");
    row.tabIndex = 0;
    const badge = document.createElement("span");
    badge.className = "change-badge k-" + statusKind(f.status);
    badge.textContent = statusText(f.status);
    const name = document.createElement("span");
    name.className = "change-path mono";
    name.textContent = f.path;
    const disc = document.createElement("button");
    disc.type = "button";
    disc.className = "change-discard";
    disc.title = "丢弃此文件的改动";
    disc.setAttribute("aria-label", "丢弃此文件的改动");
    disc.textContent = "⟲";
    disc.addEventListener("click", (e) => { e.stopPropagation(); openDiscard(f.path); });
    row.append(badge, name, disc);
    const open = () => selectFile(f);
    row.addEventListener("click", open);
    row.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
    frag.appendChild(row);
  }
  $("changes-list").replaceChildren(frag);
}

async function selectFile(f: ChangeEntry) {
  CH.selected = f.path;
  renderList();
  if (f.untracked) {
    diffMsg("新增文件（未跟踪）。可在「文件」页预览其内容。");
    return;
  }
  $("changes-diff").replaceChildren(loadingRow("读取 diff…"));
  try {
    const text = await fetchDiff(f.path);
    renderDiff(text);
  } catch (e) {
    diffMsg("读取 diff 失败：" + (e as Error).message);
  }
}

/* diff 是纯文本，不走 api()（它只解析 JSON） */
async function fetchDiff(path: string) {
  const url = `/api/sessions/${S.current!.id}/git/diff${path ? "?path=" + encodeURIComponent(path) : ""}`;
  const res = await fetch(url, { headers: { Authorization: "Bearer " + S.token } });
  if (!res.ok) {
    let m = res.statusText;
    try { m = ((await res.json()) as { error?: string }).error || m; } catch (_) { /* 保持 statusText */ }
    throw new Error(m);
  }
  return res.text();
}

function renderDiff(text: string) {
  if (!text.trim()) { diffMsg("（无文本差异）"); return; }
  const pre = document.createElement("pre");
  pre.className = "diff mono";
  for (const line of text.split("\n")) {
    const span = document.createElement("span");
    let cls = "d-ctx";
    if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("diff ") || line.startsWith("index ")) cls = "d-meta";
    else if (line.startsWith("@@")) cls = "d-hunk";
    else if (line.startsWith("+")) cls = "d-add";
    else if (line.startsWith("-")) cls = "d-del";
    span.className = cls;
    span.textContent = line + "\n";
    pre.appendChild(span);
  }
  $("changes-diff").replaceChildren(pre);
}

function setErr(id: string, msg: string) {
  const el = $(id);
  el.textContent = msg || "";
  el.classList.toggle("hidden", !msg);
}

/* ---- 提交 ---- */

$("btn-changes-refresh").addEventListener("click", loadChanges);
$("btn-changes-commit").addEventListener("click", () => {
  $<HTMLTextAreaElement>("git-commit-msg").value = "";
  setErr("git-commit-error", "");
  $<HTMLDialogElement>("dlg-git-commit").showModal();
  $("git-commit-msg").focus();
});
$("git-commit-close").addEventListener("click", () => $<HTMLDialogElement>("dlg-git-commit").close());
$("git-commit-cancel").addEventListener("click", () => $<HTMLDialogElement>("dlg-git-commit").close());
$("git-commit-ok").addEventListener("click", async () => {
  const sess = S.current;
  if (!sess) return;
  const msg = $<HTMLTextAreaElement>("git-commit-msg").value.trim();
  if (!msg) { setErr("git-commit-error", "提交信息不能为空"); return; }
  setErr("git-commit-error", "");
  btnBusy($<HTMLButtonElement>("git-commit-ok"), "提交中…");
  try {
    const res = await api<GitCommitResult>(`/sessions/${sess.id}/git/commit`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ message: msg }),
    });
    $<HTMLDialogElement>("dlg-git-commit").close();
    toast(res.output ? "已提交：" + res.output.split("\n")[0] : "已提交");
    loadChanges();
  } catch (e) {
    setErr("git-commit-error", "提交失败：" + (e as Error).message);
  } finally {
    btnDone($<HTMLButtonElement>("git-commit-ok"));
  }
});

/* ---- 丢弃 ---- */

let discardPath = "";
function openDiscard(path: string) {
  discardPath = path || "";
  $("git-discard-title").textContent = path ? "丢弃文件改动" : "丢弃全部改动";
  $("git-discard-text").textContent = path
    ? `确认丢弃「${path}」的改动？`
    : "确认丢弃工作区里所有未提交的改动？";
  $<HTMLDialogElement>("dlg-git-discard").showModal();
}
$("btn-changes-discard-all").addEventListener("click", () => openDiscard(""));
$("git-discard-close").addEventListener("click", () => $<HTMLDialogElement>("dlg-git-discard").close());
$("git-discard-cancel").addEventListener("click", () => $<HTMLDialogElement>("dlg-git-discard").close());
$("git-discard-ok").addEventListener("click", async () => {
  const sess = S.current;
  if (!sess) return;
  btnBusy($<HTMLButtonElement>("git-discard-ok"), "处理中…");
  try {
    await api(`/sessions/${sess.id}/git/discard`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: discardPath }),
    });
    $<HTMLDialogElement>("dlg-git-discard").close();
    toast("已丢弃改动");
    CH.selected = "";
    loadChanges();
  } catch (e) {
    toast("丢弃失败：" + (e as Error).message, true);
  } finally {
    btnDone($<HTMLButtonElement>("git-discard-ok"));
  }
});
