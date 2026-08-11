/* changes：Git 变更审查页 —— 列出 workspace 相对上次提交的改动，查看 diff，
 * 提交或丢弃。让「下发任务 → 审查改动 → 提交/回滚」的闭环不必切到终端。 */
"use strict";
import { S } from "./state.js";
import { $, spinEl, toast, btnBusy, btnDone } from "./util.js";
import { api } from "./api.js";
/* repo 是相对 workspace 的仓库路径（""=workspace 本身）。工作区根通常不是仓库，
 * 项目多半躺在子目录里，所以服务端会给出候选列表，这里记住当前选的那个。
 * view 是右侧看哪一种：diff（相对 HEAD 的差异）还是 full（当前完整文件）。 */
const CH = { files: [], selected: "", repo: "", repos: [], truncated: false, view: "diff" };
/** 会话切换时清掉上一个会话的仓库选择，别把它带进新会话。 */
export function resetChangesRepo() {
    CH.repo = "";
    CH.repos = [];
    CH.selected = "";
    CH.view = "diff";
}
function loadingRow(text) {
    const d = document.createElement("div");
    d.className = "loading-block";
    d.append(spinEl(), document.createTextNode(text));
    return d;
}
function listMsg(msg) {
    const p = document.createElement("p");
    p.className = "files-empty";
    p.textContent = msg;
    $("changes-list").replaceChildren(p);
}
function diffMsg(msg) {
    const p = document.createElement("p");
    p.className = "files-empty";
    p.textContent = msg;
    $("changes-diff").replaceChildren(p);
}
export async function loadChanges() {
    const sess = S.current;
    if (!sess)
        return;
    $("changes-branch").textContent = "";
    $("changes-list").replaceChildren(loadingRow("读取变更中…"));
    $("changes-diff").replaceChildren();
    $("changes-view-bar").classList.add("hidden");
    let data;
    try {
        const q = CH.repo ? "?repo=" + encodeURIComponent(CH.repo) : "";
        data = await api(`/sessions/${sess.id}/git/status${q}`);
    }
    catch (e) {
        listMsg("读取变更失败：" + e.message);
        setActions(false);
        return;
    }
    // 服务端返回的 repo 是权威值：本地记的仓库可能已经被删了，以它为准。
    CH.repo = data.repo || "";
    CH.repos = data.repos || [];
    renderRepoPick();
    if (!data.is_repo) {
        listMsg("工作区里没有 Git 仓库。可在终端里 git init，或把项目 clone 进来再回来审查改动。");
        setActions(false);
        return;
    }
    CH.files = data.files || [];
    CH.truncated = !!data.truncated;
    const branch = data.branch ? "分支 " + data.branch : "";
    // 仓库下拉已经显示路径时就不再重复；只有一个仓库且它在子目录里才带上路径。
    const prefix = CH.repos.length > 1 || !CH.repo ? "" : CH.repo + " · ";
    $("changes-branch").textContent = prefix + branch;
    renderList();
    if (CH.selected)
        renderView(); // 刷新后重新读一遍当前文件，别留着旧内容
}
/** 多个仓库时给出下拉；否则藏起来。 */
function renderRepoPick() {
    const sel = $("changes-repo");
    sel.classList.toggle("hidden", CH.repos.length < 2);
    if (CH.repos.length < 2) {
        sel.replaceChildren();
        return;
    }
    sel.replaceChildren(...CH.repos.map((r) => {
        const o = document.createElement("option");
        o.value = r;
        o.textContent = r || "（工作区根目录）";
        return o;
    }));
    sel.value = CH.repo;
}
function setActions(on) {
    $("btn-changes-commit").disabled = !on;
    $("btn-changes-discard-all").disabled = !on;
}
const STATUS_LABEL = {
    "??": "新增", "A ": "新增", "AM": "新增",
    " M": "修改", "M ": "已暂存", "MM": "修改",
    " D": "删除", "D ": "删除", "R ": "重命名", "RM": "重命名",
};
function statusText(xy) {
    return STATUS_LABEL[xy] || xy.trim() || "变更";
}
function statusKind(xy) {
    if (xy === "??" || xy.includes("A"))
        return "add";
    if (xy.includes("D"))
        return "del";
    if (xy.includes("R"))
        return "ren";
    return "mod";
}
function renderList() {
    // 上次选中的文件可能已经提交或被丢弃，右侧跟着一起清掉。
    if (CH.selected && !CH.files.some((f) => f.path === CH.selected)) {
        CH.selected = "";
        $("changes-diff").replaceChildren();
    }
    renderViewBar();
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
        row.addEventListener("keydown", (e) => { if (e.key === "Enter")
            open(); });
        frag.appendChild(row);
    }
    if (CH.truncated) {
        const more = document.createElement("p");
        more.className = "files-empty";
        more.textContent = `变更太多，只列出前 ${CH.files.length} 个。`;
        frag.appendChild(more);
    }
    $("changes-list").replaceChildren(frag);
}
function currentFile() {
    return CH.files.find((f) => f.path === CH.selected) || null;
}
const isNew = (f) => f.untracked || f.status.trim() === "A";
const isDeleted = (f) => !f.untracked && f.status.includes("D");
function selectFile(f) {
    CH.selected = f.path;
    // 按文件类型定默认视图，不沿用上一个文件的选择：新文件本来就没 diff 可看，
    // 改过的文件则是差异更有用。切文件时视图跟着重置，行为可预期。
    CH.view = isNew(f) ? "full" : "diff";
    renderList();
    renderView();
}
/** 右侧顶栏：当前文件 + 「差异 / 完整内容」切换。 */
function renderViewBar() {
    const f = currentFile();
    $("changes-view-bar").classList.toggle("hidden", !f);
    if (!f)
        return;
    $("changes-view-path").textContent = f.path;
    const diffBtn = $("btn-view-diff");
    const fullBtn = $("btn-view-full");
    diffBtn.classList.toggle("active", CH.view === "diff");
    fullBtn.classList.toggle("active", CH.view === "full");
    diffBtn.disabled = f.untracked; // 未跟踪的文件相对 HEAD 没有差异
    fullBtn.disabled = isDeleted(f); // 删掉的文件没有内容可读
    diffBtn.title = f.untracked ? "新文件没有可比对的版本" : "";
    fullBtn.title = isDeleted(f) ? "文件已删除" : "";
}
async function renderView() {
    const f = currentFile();
    renderViewBar();
    if (!f) {
        $("changes-diff").replaceChildren();
        return;
    }
    const full = CH.view === "full";
    $("changes-diff").replaceChildren(loadingRow(full ? "读取文件内容…" : "读取 diff…"));
    const stale = () => CH.selected !== f.path || (CH.view === "full") !== full;
    try {
        const text = await fetchText(full ? "file" : "diff", f.path);
        if (stale())
            return; // 读的过程中用户又点了别处
        if (full)
            renderText(text);
        else
            renderDiff(text);
    }
    catch (e) {
        if (stale())
            return;
        diffMsg((full ? "读取文件内容失败：" : "读取 diff 失败：") + e.message);
    }
}
/* diff / 文件内容都是纯文本，不走 api()（它只解析 JSON）。path 相对仓库根，
 * 不是工作区根。 */
async function fetchText(kind, path) {
    const q = new URLSearchParams();
    if (path)
        q.set("path", path);
    if (CH.repo)
        q.set("repo", CH.repo);
    const qs = q.toString();
    const url = `/api/sessions/${S.current.id}/git/${kind}${qs ? "?" + qs : ""}`;
    const res = await fetch(url, { headers: { Authorization: "Bearer " + S.token } });
    if (!res.ok) {
        let m = res.statusText;
        try {
            m = (await res.json()).error || m;
        }
        catch (_) { /* 保持 statusText */ }
        throw new Error(m);
    }
    return res.text();
}
function renderText(text) {
    if (!text) {
        diffMsg("（空文件）");
        return;
    }
    const pre = document.createElement("pre");
    pre.className = "filetext mono";
    pre.textContent = text;
    $("changes-diff").replaceChildren(pre);
}
function renderDiff(text) {
    if (!text.trim()) {
        diffMsg("（无文本差异）");
        return;
    }
    const pre = document.createElement("pre");
    pre.className = "diff mono";
    for (const line of text.split("\n")) {
        const span = document.createElement("span");
        let cls = "d-ctx";
        if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("diff ") || line.startsWith("index "))
            cls = "d-meta";
        else if (line.startsWith("@@"))
            cls = "d-hunk";
        else if (line.startsWith("+"))
            cls = "d-add";
        else if (line.startsWith("-"))
            cls = "d-del";
        span.className = cls;
        span.textContent = line + "\n";
        pre.appendChild(span);
    }
    $("changes-diff").replaceChildren(pre);
}
function setErr(id, msg) {
    const el = $(id);
    el.textContent = msg || "";
    el.classList.toggle("hidden", !msg);
}
$("btn-changes-refresh").addEventListener("click", loadChanges);
$("changes-repo").addEventListener("change", (e) => {
    CH.repo = e.target.value;
    CH.selected = "";
    loadChanges();
});
function setView(v) {
    if (CH.view === v)
        return;
    CH.view = v;
    renderView();
}
$("btn-view-diff").addEventListener("click", () => setView("diff"));
$("btn-view-full").addEventListener("click", () => setView("full"));
/* ---- 提交 ---- */
$("btn-changes-commit").addEventListener("click", () => {
    $("git-commit-msg").value = "";
    setErr("git-commit-error", "");
    $("dlg-git-commit").showModal();
    $("git-commit-msg").focus();
});
$("git-commit-close").addEventListener("click", () => $("dlg-git-commit").close());
$("git-commit-cancel").addEventListener("click", () => $("dlg-git-commit").close());
$("git-commit-ok").addEventListener("click", async () => {
    const sess = S.current;
    if (!sess)
        return;
    const msg = $("git-commit-msg").value.trim();
    if (!msg) {
        setErr("git-commit-error", "提交信息不能为空");
        return;
    }
    setErr("git-commit-error", "");
    btnBusy($("git-commit-ok"), "提交中…");
    try {
        const res = await api(`/sessions/${sess.id}/git/commit`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ message: msg, repo: CH.repo }),
        });
        $("dlg-git-commit").close();
        toast(res.output ? "已提交：" + res.output.split("\n")[0] : "已提交");
        loadChanges();
    }
    catch (e) {
        setErr("git-commit-error", "提交失败：" + e.message);
    }
    finally {
        btnDone($("git-commit-ok"));
    }
});
/* ---- 丢弃 ---- */
let discardPath = "";
function openDiscard(path) {
    discardPath = path || "";
    $("git-discard-title").textContent = path ? "丢弃文件改动" : "丢弃全部改动";
    const where = CH.repo ? `仓库「${CH.repo}」` : "工作区";
    $("git-discard-text").textContent = path
        ? `确认丢弃「${path}」的改动？`
        : `确认丢弃${where}里所有未提交的改动？`;
    $("dlg-git-discard").showModal();
}
$("btn-changes-discard-all").addEventListener("click", () => openDiscard(""));
$("git-discard-close").addEventListener("click", () => $("dlg-git-discard").close());
$("git-discard-cancel").addEventListener("click", () => $("dlg-git-discard").close());
$("git-discard-ok").addEventListener("click", async () => {
    const sess = S.current;
    if (!sess)
        return;
    btnBusy($("git-discard-ok"), "处理中…");
    try {
        await api(`/sessions/${sess.id}/git/discard`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ path: discardPath, repo: CH.repo }),
        });
        $("dlg-git-discard").close();
        toast("已丢弃改动");
        CH.selected = "";
        loadChanges();
    }
    catch (e) {
        toast("丢弃失败：" + e.message, true);
    }
    finally {
        btnDone($("git-discard-ok"));
    }
});
