import { openGitBranches } from "./git-branches.js";
/* changes：Git 变更审查页 —— 列出 workspace 相对上次提交的改动，查看 diff，
 * 提交或丢弃。让「下发任务 → 审查改动 → 提交/回滚」的闭环不必切到终端。 */
"use strict";
import { buttonLabel } from "./icons.js";
import { setSelectValue } from "./select.js";
import { S, bus } from "./state.js";
import "./git-profile.js";
import { openGitRemote, openGitClone } from "./git-connections.js";
import { $, spinEl, toast, btnBusy, btnDone, fmtTime } from "./util.js";
import { api } from "./api.js";
import { setTip } from "./tip.js";
/* repo 是相对 workspace 的仓库路径（""=workspace 本身）。工作区根通常不是仓库，
 * 项目多半躺在子目录里，所以服务端会给出候选列表，这里记住当前选的那个。
 * view 是右侧看哪一种：diff（相对 HEAD 的差异）还是 full（当前完整文件）。 */
const CH = { files: [], selected: "", repo: "", repos: [], truncated: false, view: "diff" };
let loadGeneration = 0;
let gitStatus = null;
/** 会话切换时清掉上一个会话的仓库选择，别把它带进新会话。 */
export function resetChangesRepo() {
    $("tab-changes").classList.remove("show-diff");
    gitStatus = null;
    loadGeneration++;
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
    const generation = ++loadGeneration;
    const token = S.token;
    const stale = () => generation !== loadGeneration || sess.id !== S.current?.id || token !== S.token;
    $("tab-changes").classList.add("changes-empty");
    setActions(false);
    $("btn-changes-remote").disabled = $("btn-changes-branches").disabled = true;
    $("changes-remote-state").classList.add("hidden");
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
        if (stale())
            return;
        listMsg("读取变更失败：" + e.message);
        setActions(false);
        return;
    }
    if (stale())
        return;
    gitStatus = data;
    // 服务端返回的 repo 是权威值：本地记的仓库可能已经被删了，以它为准。
    CH.repo = data.repo || "";
    CH.repos = data.repos || [];
    renderRepoPick();
    if (!data.is_repo) {
        listMsg("当前空间没有 Git 仓库。在终端中初始化或克隆项目后，即可查看改动。");
        const clone = document.createElement("button");
        clone.className = "btn btn-primary";
        buttonLabel(clone, "克隆仓库", "git-clone");
        clone.addEventListener("click", () => $("btn-changes-clone").click());
        $("changes-list").append(clone);
        setActions(false);
        return;
    }
    CH.files = data.files || [];
    $("btn-changes-remote").disabled = $("btn-changes-branches").disabled = false;
    CH.truncated = !!data.truncated;
    const branch = data.detached ? "游离 HEAD · " + (data.head || "").slice(0, 12) : "分支 " + (data.branch || "—") + (data.unborn ? "（尚无提交）" : "");
    // 仓库下拉已经显示路径时就不再重复；只有一个仓库且它在子目录里才带上路径。
    const prefix = CH.repos.length > 1 || !CH.repo ? "" : CH.repo + " · ";
    $("changes-branch").textContent = prefix + branch;
    const remoteState = $("changes-remote-state");
    const tracking = data.upstream
        ? `${data.upstream} · ${data.tracking_known ? `本地领先 ${data.ahead} / 落后 ${data.behind}` : "跟踪分支尚未获取或已删除"}（本地缓存）`
        : "当前分支未设置上游";
    const remotes = (data.remotes || []).map(r => `${r.name}${r.push ? "（推送）" : ""}: ${r.url}`);
    $("changes-tracking").textContent = tracking;
    $("changes-remote-detail").textContent = [...remotes, ...(data.last_fetch ? [`上次网页获取 ${data.last_fetch.target} · ${fmtTime(Date.parse(data.last_fetch.at))}`] : []), "刷新仅检查本地状态，不会获取或推送远程提交。"].join("\n");
    remoteState.classList.remove("hidden");
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
        o.textContent = r || "（空间文件根目录）";
        return o;
    }));
    setSelectValue(sel, CH.repo);
}
function setActions(on) {
    $("btn-changes-commit").disabled = !on;
    $("btn-changes-discard-all").disabled = !on;
}
/* 标签只表示改动类型；是否已暂存单独用一个小圆点表示。旧版把两种分类混进同一个
 * 标签（暂存的修改显示「已暂存」，暂存的新文件却显示「新增」），用户看不出改了什么。
 * 提交时服务端会 add -A，暂存与否不影响这次提交包含哪些文件。 */
const KIND_LABEL = { new: "未跟踪", add: "新增", mod: "修改", del: "删除", ren: "重命名", conflict: "冲突" };
function statusKind(xy) {
    if (xy === "??")
        return "new";
    if (xy.includes("U") || xy === "AA" || xy === "DD")
        return "conflict";
    if (xy.includes("R"))
        return "ren";
    if (xy.includes("D"))
        return "del";
    if (xy.includes("A") || xy.includes("C"))
        return "add";
    return "mod";
}
function stagedState(xy) {
    const x = xy[0] || " ", y = xy[1] || " ";
    if (x === " " || x === "?" || x === "!")
        return "";
    return y === " " ? "full" : "part";
}
function renderList() {
    $("tab-changes").classList.toggle("changes-empty", !CH.files.length);
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
    // 右侧空着时给一句提示，别留一大块空白让人以为没加载出来
    if (!CH.selected)
        diffMsg(`共 ${CH.files.length} 个文件有改动。选择左侧文件查看差异。`);
    const frag = document.createDocumentFragment();
    for (const f of CH.files) {
        const row = document.createElement("div");
        row.className = "change-row" + (f.path === CH.selected ? " active" : "");
        row.tabIndex = 0;
        const kind = statusKind(f.status);
        const badge = document.createElement("span");
        badge.className = "change-badge k-" + kind;
        badge.textContent = KIND_LABEL[kind];
        const staged = stagedState(f.status);
        const dot = document.createElement("span");
        dot.className = "change-staged" + (staged ? " " + staged : "");
        if (staged) {
            const tip = staged === "full" ? "已暂存" : "部分暂存：工作区里还有未暂存的改动";
            setTip(dot, tip);
            dot.setAttribute("aria-label", tip);
            dot.setAttribute("role", "img");
        }
        else
            dot.setAttribute("aria-hidden", "true");
        const name = document.createElement("span");
        name.className = "change-path mono";
        name.textContent = f.path;
        const disc = document.createElement("button");
        disc.type = "button";
        disc.className = "change-discard";
        setTip(disc, "丢弃此文件的改动");
        disc.setAttribute("aria-label", "丢弃此文件的改动");
        buttonLabel(disc, "", "undo");
        disc.addEventListener("click", (e) => { e.stopPropagation(); openDiscard(f.path); });
        row.append(badge, dot, name, disc);
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
    $("tab-changes").classList.add("show-diff");
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
    if (!f) {
        $("tab-changes").classList.remove("show-diff");
        return;
    }
    $("changes-view-path").textContent = f.path;
    const diffBtn = $("btn-view-diff");
    const fullBtn = $("btn-view-full");
    diffBtn.classList.toggle("active", CH.view === "diff");
    fullBtn.classList.toggle("active", CH.view === "full");
    diffBtn.disabled = f.untracked; // 未跟踪的文件相对 HEAD 没有差异
    fullBtn.disabled = isDeleted(f); // 删掉的文件没有内容可读
    setTip(diffBtn, f.untracked ? "新文件没有可比对的版本" : "");
    setTip(fullBtn, isDeleted(f) ? "文件已删除" : "");
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
$("btn-changes-branches").addEventListener("click", () => openGitBranches(CH.repo, loadChanges));
$("btn-changes-refresh").addEventListener("click", loadChanges);
$("btn-changes-clone").addEventListener("click", () => openGitClone(async (repo) => { CH.repo = repo; await loadChanges(); }));
$("btn-changes-remote").addEventListener("click", () => { if (gitStatus)
    void openGitRemote(CH.repo, gitStatus, loadChanges); });
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
let commitTarget = null;
let committing = false;
$("btn-changes-commit").addEventListener("click", async () => {
    if (!S.current || committing)
        return;
    const target = { session: S.current.id, repo: CH.repo, token: S.token };
    commitTarget = target;
    $("git-commit-msg").value = "";
    $("git-commit-target").textContent = `${S.current.name} · ${CH.repo || "空间文件根目录"}`;
    $("git-commit-identity").textContent = "读取提交身份中…";
    $("git-commit-ok").disabled = true;
    setErr("git-commit-error", "");
    $("dlg-git-commit").showModal();
    $("git-commit-msg").focus();
    try {
        const profile = await api("/me/git");
        if (commitTarget !== target || target.token !== S.token)
            return;
        $("git-commit-identity").textContent = `提交身份：${profile.name} <${profile.email}>`;
        $("git-commit-ok").disabled = false;
    }
    catch (e) {
        if (commitTarget === target)
            setErr("git-commit-error", "读取提交身份失败：" + e.message);
    }
});
$("dlg-git-commit").addEventListener("close", () => { commitTarget = null; });
$("dlg-git-commit").addEventListener("cancel", e => { if (committing)
    e.preventDefault(); });
$("git-commit-close").addEventListener("click", () => $("dlg-git-commit").close());
$("git-commit-cancel").addEventListener("click", () => $("dlg-git-commit").close());
$("git-commit-ok").addEventListener("click", async () => {
    const target = commitTarget;
    if (!target || committing || target.token !== S.token)
        return;
    const msg = $("git-commit-msg").value.trim();
    if (!msg) {
        setErr("git-commit-error", "提交信息不能为空");
        return;
    }
    setErr("git-commit-error", "");
    committing = true;
    $("git-commit-cancel").disabled = $("git-commit-close").disabled = true;
    btnBusy($("git-commit-ok"), "提交中…");
    try {
        const res = await api(`/sessions/${target.session}/git/commit`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ message: msg, repo: target.repo }),
        });
        $("dlg-git-commit").close();
        if (target.token !== S.token)
            return;
        toast(res.warning || `已提交到本地${res.sha ? " · " + res.sha.slice(0, 12) : ""}，未推送到远程`);
        if (target.session === S.current?.id && target.repo === CH.repo)
            loadChanges();
    }
    catch (e) {
        if (target.token === S.token)
            setErr("git-commit-error", "提交失败：" + e.message);
    }
    finally {
        committing = false;
        $("git-commit-cancel").disabled = $("git-commit-close").disabled = false;
        btnDone($("git-commit-ok"));
    }
});
bus.addEventListener("signed-out", () => {
    resetChangesRepo();
    $("dlg-git-commit").close();
    $("dlg-git-discard").close();
});
/* ---- 丢弃 ---- */
let discardPath = "";
function openDiscard(path) {
    discardPath = path || "";
    $("git-discard-title").textContent = path ? "丢弃文件改动" : "丢弃全部改动";
    const where = CH.repo ? `仓库「${CH.repo}」` : "空间文件根目录";
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
$("changes-back").addEventListener("click", () => {
    $("tab-changes").classList.remove("show-diff");
    $("changes-list").querySelector(".change-row.active")?.focus();
});
