import { openDiagnostics } from "./diagnostics.js";
import { setText, setTextRender, t as i18nText } from "./i18n.js";
/* sessions：会话的打开/切换、生命周期（启动/停止/删除，含窄屏 ⋯ 菜单）、
 * 新建会话弹窗、工作台标签页。 */
"use strict";
import { S, bus, emit } from "./state.js";
import { $, btnBusy, btnDone, wbBusy, wbIdle, toast, askPrompt } from "./util.js";
import { api } from "./api.js";
import { refreshAll } from "./data.js";
import { showView, renderSidebar, updateTopbarTitle } from "./shell.js";
import { chatTeardown, resetChatImgs, loadPick, updateHero, loadHistory, connectChat } from "./chat.js";
import { setThreadBar, closeThreadPanel } from "./chat-threads.js";
import { termTeardown, termDisconnect, openTerm, termSpendPolling } from "./term.js";
import { resetTree, loadFiles } from "./files.js";
import { loadChanges, resetChangesRepo } from "./changes.js";
import { loadSkills } from "./skills.js";
import { agentKey, agentName, agentIcon, agentAvatar, decorateAgentOpts } from "./brand.js";
import { openAcctUsage } from "./acct-usage.js";
import { showBrowser, browserDisconnect } from "./remote-browser.js";
import { setTip } from "./tip.js";
import { bindMenu } from "./menu.js";
import { sessionState } from "./session-state.js";
import { renderHome } from "./home.js";
import { actionButton } from "./icons.js";
/* ---------------- 打开 / 切换 ---------------- */
export async function openSession(sess, tab) {
    if (sess.agent !== "claude" && (tab === "skills" || tab === "mcp"))
        tab = "chat";
    if (S.current && S.current.id === sess.id) {
        showView("work");
        if (tab && tab !== S.tab)
            setTab(tab);
        return;
    }
    closeChannels();
    showView("work");
    S.current = sess;
    S.filePath = "";
    resetTree();
    resetChangesRepo();
    resetChatImgs();
    loadPick();
    $("empty").classList.add("hidden");
    $("workbench").classList.remove("hidden");
    $("chat-log").replaceChildren();
    setThreadBar(null); // 切换会话时先清掉上一个会话的线程标题
    S.histLoading = true; // loadHistory 还没跑之前也不要闪引导页
    updateHero();
    setTab(tab || "chat");
    renderSidebar();
    renderHead();
    await loadHistory();
    if (S.current?.id === sess.id)
        connectChat();
}
export function renderHead() {
    const sess = S.current;
    if (!sess)
        return;
    updateTopbarTitle();
    $("wb-name").textContent = sess.name;
    const av = agentAvatar(sess.agent, { size: 32, icon: 17, led: true });
    setTip(av, agentName(sess.agent));
    if (sess.status === "running")
        av.querySelector(".led").classList.add("on");
    $("wb-avatar").replaceChildren(av);
    const meta = $("wb-meta");
    meta.replaceChildren();
    const an = document.createElement("span");
    an.className = "agent-text agent-" + agentKey(sess.agent);
    an.textContent = agentName(sess.agent);
    const id = document.createElement("span");
    id.className = "mono";
    id.textContent = "#" + sess.id;
    meta.append(an, document.createTextNode(` · ${sess.account_label} · `), id);
    // 技能是 Claude Code 的机制，codex 会话没有对应目录，页签直接藏掉
    const claude = agentKey(sess.agent) === "claude";
    $("tab-btn-skills").classList.toggle("hidden", !claude);
    $("tab-btn-mcp").classList.toggle("hidden", !claude);
    if (!claude && (S.tab === "skills" || S.tab === "mcp"))
        setTab("chat");
    const running = sess.status === "running";
    const state = sessionState(sess);
    const pill = $("wb-state");
    pill.className = "state-pill " + state.cls;
    setTextRender(pill, () => sessionState(sess).label);
    setTip(pill, () => sessionState(sess).tip);
    if (!S.actionBusy) { // 启动/停止执行中由按钮自己管理，轮询刷新不得把另一个按钮换回来
        $("btn-start").classList.toggle("hidden", running);
        $("btn-stop").classList.toggle("hidden", !running);
    }
}
function closeChannels() {
    browserDisconnect();
    chatTeardown();
    termTeardown();
    closeThreadPanel();
}
bus.addEventListener("open-session", (e) => openSession(e.detail));
bus.addEventListener("data-updated", () => { if (S.current)
    renderHead(); });
/* ---------------- 标签页 ---------------- */
export function setTab(name) {
    if (S.tab === "browser" && name !== "browser")
        browserDisconnect();
    S.tab = name;
    emit("navigation-changed");
    for (const t of document.querySelectorAll(".tab")) {
        t.classList.toggle("active", t.dataset.tab === name);
    }
    $("tab-chat").classList.toggle("hidden", name !== "chat");
    $("tab-term").classList.toggle("hidden", name !== "term");
    $("tab-files").classList.toggle("hidden", name !== "files");
    $("tab-changes").classList.toggle("hidden", name !== "changes");
    $("tab-skills").classList.toggle("hidden", name !== "skills");
    $("tab-mcp").classList.toggle("hidden", name !== "mcp");
    $("tab-browser").classList.toggle("hidden", name !== "browser");
    if (name === "browser")
        void showBrowser();
    if (name === "files")
        loadFiles();
    if (name === "changes")
        loadChanges();
    if (name === "skills")
        loadSkills();
    if (name === "term")
        openTerm(); // 进入即自动拉起 shell；已连上则只重排尺寸
    // 「本会话已花」只在终端页轮询：切走了没人看，没必要一直问服务端。
    termSpendPolling(name === "term");
}
for (const t of document.querySelectorAll(".tab")) {
    t.addEventListener("click", () => setTab(t.dataset.tab));
}
/* ---------------- 生命周期：启动 / 停止 / 删除 ---------------- */
async function doStart() {
    const s = S.current;
    if (!s || S.actionBusy)
        return;
    S.actionBusy = true;
    wbBusy("start");
    btnBusy($("btn-start"), () => i18nText("启动中…"));
    try {
        const res = await api(`/sessions/${s.id}/start`, { method: "POST" });
        if (S.current && S.current.id === s.id)
            S.current = res; // 期间切换了会话则不覆盖
    }
    catch (e) {
        toast(i18nText("启动失败：") + e.message, true);
    }
    S.actionBusy = false;
    wbIdle();
    btnDone($("btn-start"));
    renderHead();
    refreshAll();
}
async function doStop() {
    const s = S.current;
    if (!s || S.actionBusy)
        return;
    S.actionBusy = true;
    wbBusy("stop");
    btnBusy($("btn-stop"), () => i18nText("停止中…"));
    try {
        const res = await api(`/sessions/${s.id}/stop`, { method: "POST" });
        if (S.current && S.current.id === s.id)
            S.current = res;
        browserDisconnect();
        termDisconnect(); // 容器停了收掉连接，但保留终端画面
    }
    catch (e) {
        toast(i18nText("停止失败：") + e.message, true);
    }
    S.actionBusy = false;
    wbIdle();
    btnDone($("btn-stop"));
    renderHead();
    refreshAll();
}
function openDeleteDlg() {
    if (!S.current)
        return;
    setText($("del-text"), "确认删除工作空间「{p0}」？空间将从列表移除，容器被删除。文件、配置和对话记录默认保留在服务器磁盘上。", { p0: String(S.current.name) });
    $("del-purge").checked = false;
    syncDeleteLabel();
    $("dlg-del").showModal();
}
/* 勾选「同时清除」后按钮写明后果，不让同一个「删除」承担两种轻重 */
function syncDeleteLabel() {
    const purge = $("del-purge").checked;
    actionButton($("del-ok"), () => purge ? i18nText("删除并清除文件") : i18nText("删除"), "trash");
}
$("del-purge").addEventListener("change", syncDeleteLabel);
$("btn-start").addEventListener("click", doStart);
$("btn-stop").addEventListener("click", doStop);
/* 工作台头部 ⋯ 与窄屏顶栏 ⋯ 共用一份菜单；窄屏没有头部，启动/停止也放进去。
 * 删除与启停不在同一层级：删除只在菜单最后一项。 */
function sessionMenu(withPower) {
    const s = S.current;
    if (!s)
        return [];
    const running = s.status === "running";
    return [
        { label: i18nText("启动"), icon: "play", run: doStart, hidden: !withPower || running, disabled: S.actionBusy },
        { label: i18nText("停止"), icon: "stop", run: doStop, hidden: !withPower || !running, disabled: S.actionBusy, tip: i18nText("停止工作空间，保留文件与对话") },
        { label: i18nText("使用指引"), icon: "bulb", run: () => emit("workspace-guide-open"), sep: true },
        { label: i18nText("环境检查"), icon: "activity", run: () => openDiagnostics(s.id), sep: true },
        { label: i18nText("查看账号额度"), icon: "gauge", run: openAcctUsage, hidden: agentKey(s.agent) !== "claude", sep: true, tip: i18nText("该账号订阅的 5 小时 / 每周用量窗口") },
        { label: i18nText("重命名"), icon: "rename", run: renameSession, sep: agentKey(s.agent) !== "claude" },
        { label: i18nText("删除工作空间…"), icon: "trash", danger: true, sep: true, run: openDeleteDlg, disabled: S.actionBusy },
    ];
}
bindMenu($("btn-wb-more"), () => sessionMenu(false));
bindMenu($("btn-kebab"), () => sessionMenu(true));
/* 会话改名：只改展示名，id / 目录 / 容器名都从 id 派生，不受影响 */
async function renameSession() {
    const sess = S.current;
    if (!sess)
        return;
    const name = await askPrompt({
        get title() { return i18nText("重命名工作空间"); },
        get label() { return i18nText("工作空间名称"); },
        value: sess.name,
        get hint() { return i18nText("1–64 个字符，仅修改空间名称，文件与对话记录保留。"); },
        validate: (v) => {
            const t = v.trim();
            if (!t)
                return i18nText("名称不能为空");
            if ([...t].length > 64)
                return i18nText("名称最多 64 个字符");
            return "";
        },
    });
    if (name === null)
        return;
    try {
        const res = await api(`/sessions/${sess.id}`, {
            method: "PATCH",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ name: name.trim() }),
        });
        if (S.current && S.current.id === sess.id)
            S.current = res;
        renderHead();
        renderSidebar();
        refreshAll();
        toast(i18nText("工作空间已重命名"));
    }
    catch (e) {
        toast(i18nText("重命名失败：") + e.message, true);
    }
}
$("del-cancel").addEventListener("click", () => $("dlg-del").close());
$("del-close").addEventListener("click", () => $("dlg-del").close());
let delBusy = false;
$("dlg-del").addEventListener("cancel", (e) => { if (delBusy)
    e.preventDefault(); }); // 删除中禁止 Esc 关闭
$("del-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const s = S.current;
    if (!s || delBusy)
        return;
    delBusy = true;
    btnBusy($("del-ok"), () => i18nText("删除中…"));
    $("del-cancel").disabled = true;
    $("del-close").disabled = true;
    const purge = $("del-purge").checked ? "?purge=1" : "";
    try {
        await api(`/sessions/${s.id}${purge}`, { method: "DELETE" });
        $("dlg-del").close();
        closeChannels();
        S.current = null;
        $("workbench").classList.add("hidden");
        $("empty").classList.remove("hidden");
        updateTopbarTitle();
        renderHome();
        emit("navigation-changed");
        refreshAll();
    }
    catch (err) {
        toast(i18nText("删除失败：") + err.message, true);
    }
    delBusy = false;
    btnDone($("del-ok"));
    $("del-cancel").disabled = false;
    $("del-close").disabled = false;
});
/* ---------------- 新建会话 ---------------- */
decorateAgentOpts($("new-form"));
/* 空状态：列出支持的 Agent 类型，不代表镜像或账号已验证；CTA 与侧栏「新建会话」同一入口 */
for (const a of ["claude", "codex"]) {
    const chip = document.createElement("span");
    chip.className = "brand-chip agent-" + a;
    chip.append(agentIcon(a, 14), agentName(a));
    $("empty-brands").append(chip);
}
$("empty-new").addEventListener("click", () => $("btn-new").click());
/* 侧栏字标 = 首页入口：放下当前会话回到空状态（只断前端通道，容器不动） */
export function openHome() {
    if (S.current) {
        closeChannels();
        S.current = null;
    }
    $("workbench").classList.add("hidden");
    $("empty").classList.remove("hidden");
    showView("work"); /* 已在工作台时走早退分支，仍会收抽屉 */
    updateTopbarTitle();
    renderSidebar();
    renderHome();
}
$("btn-home").addEventListener("click", openHome);
