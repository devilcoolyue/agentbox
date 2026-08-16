/* sessions：会话的打开/切换、生命周期（启动/停止/删除，含窄屏 ⋯ 菜单）、
 * 新建会话弹窗、工作台标签页。 */
"use strict";
import { S, bus } from "./state.js";
import { $, btnBusy, btnDone, wbBusy, wbIdle, toast, askPrompt } from "./util.js";
import { api } from "./api.js";
import { refreshAll } from "./data.js";
import { showView, renderSidebar, updateTopbarTitle } from "./shell.js";
import { chatTeardown, resetChatImgs, loadPick, updateHero, loadHistory, connectChat } from "./chat.js";
import { setThreadBar, closeThreadPanel } from "./chat-threads.js";
import { svgIcon } from "./chat-render.js";
import { termTeardown, termDisconnect, openTerm, termSpendPolling } from "./term.js";
import { resetTree, loadFiles } from "./files.js";
import { loadChanges, resetChangesRepo } from "./changes.js";
import { loadSkills } from "./skills.js";
import { agentKey, agentName, agentIcon, agentAvatar, decorateAgentOpts } from "./brand.js";
import { openAcctUsage, syncUsageBtn } from "./acct-usage.js";
import { setTip } from "./tip.js";
/* ---------------- 打开 / 切换 ---------------- */
export async function openSession(sess) {
    if (S.current && S.current.id === sess.id) {
        showView("work");
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
    setTab("chat");
    renderSidebar();
    renderHead();
    await loadHistory();
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
    meta.append(an, document.createTextNode(` · ${sess.account_label} · #${sess.id}`));
    // 技能是 Claude Code 的机制，codex 会话没有对应目录，页签直接藏掉
    const claude = agentKey(sess.agent) === "claude";
    $("tab-btn-skills").classList.toggle("hidden", !claude);
    if (!claude && S.tab === "skills")
        setTab("chat");
    syncUsageBtn(sess.agent);
    if (sess.stop_reason === "idle" && sess.status !== "running") {
        const zzz = document.createElement("span");
        zzz.className = "sc-sleep";
        zzz.textContent = "休眠中";
        setTip(zzz, "空闲自动停机，发消息或打开终端会自动唤醒");
        meta.append(document.createTextNode(" · "), zzz);
    }
    if (!S.actionBusy) { // 启动/停止执行中由按钮自己管理禁用态，轮询刷新不得复活
        const running = sess.status === "running";
        $("btn-start").disabled = running;
        $("btn-stop").disabled = !running;
        $("kb-start").disabled = running;
        $("kb-stop").disabled = !running;
    }
}
function closeChannels() {
    chatTeardown();
    termTeardown();
    closeThreadPanel();
}
bus.addEventListener("open-session", (e) => openSession(e.detail));
bus.addEventListener("data-updated", () => { if (S.current)
    renderHead(); });
/* ---------------- 标签页 ---------------- */
export function setTab(name) {
    S.tab = name;
    for (const t of document.querySelectorAll(".tab")) {
        t.classList.toggle("active", t.dataset.tab === name);
    }
    $("tab-chat").classList.toggle("hidden", name !== "chat");
    $("tab-term").classList.toggle("hidden", name !== "term");
    $("tab-files").classList.toggle("hidden", name !== "files");
    $("tab-changes").classList.toggle("hidden", name !== "changes");
    $("tab-skills").classList.toggle("hidden", name !== "skills");
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
    btnBusy($("btn-start"), "启动中…");
    $("btn-stop").disabled = true;
    $("btn-delete").disabled = true;
    $("kb-start").disabled = true;
    $("kb-stop").disabled = true;
    try {
        const res = await api(`/sessions/${s.id}/start`, { method: "POST" });
        if (S.current && S.current.id === s.id)
            S.current = res; // 期间切换了会话则不覆盖
    }
    catch (e) {
        toast("启动失败：" + e.message, true);
    }
    S.actionBusy = false;
    wbIdle();
    btnDone($("btn-start"));
    $("btn-delete").disabled = false;
    renderHead();
    refreshAll();
}
async function doStop() {
    const s = S.current;
    if (!s || S.actionBusy)
        return;
    S.actionBusy = true;
    wbBusy("stop");
    btnBusy($("btn-stop"), "停止中…");
    $("btn-start").disabled = true;
    $("btn-delete").disabled = true;
    $("kb-start").disabled = true;
    $("kb-stop").disabled = true;
    try {
        const res = await api(`/sessions/${s.id}/stop`, { method: "POST" });
        if (S.current && S.current.id === s.id)
            S.current = res;
        termDisconnect(); // 容器停了收掉连接，但保留终端画面
    }
    catch (e) {
        toast("停止失败：" + e.message, true);
    }
    S.actionBusy = false;
    wbIdle();
    btnDone($("btn-stop"));
    $("btn-delete").disabled = false;
    renderHead();
    refreshAll();
}
function openDeleteDlg() {
    if (!S.current)
        return;
    $("del-text").textContent = `确认删除会话「${S.current.name}」？容器会被移除。`;
    $("del-purge").checked = false;
    $("dlg-del").showModal();
}
/* 静态装饰：工作台按钮与 ⋯ 菜单共用同一组图标。工作台直接前置（btnBusy 换成转圈后
 * 仍能原样还原）；菜单放进 .glyph 定宽槽位，保证各行文字左边缘对齐。 */
for (const [act, ico] of [["start", "play"], ["stop", "stop"], ["usage", "gauge"], ["delete", "trash"]]) {
    $("btn-" + act).prepend(svgIcon(ico, 17));
    $("kb-" + act).querySelector(".glyph").appendChild(svgIcon(ico, 17));
}
/* 重命名只在 ⋯ 菜单里有，工作台头部没有对应按钮，所以不能并进上面那轮。 */
$("kb-rename").querySelector(".glyph").appendChild(svgIcon("rename", 17));
$("btn-start").addEventListener("click", doStart);
$("btn-stop").addEventListener("click", doStop);
$("btn-usage").addEventListener("click", openAcctUsage);
$("btn-delete").addEventListener("click", openDeleteDlg);
/* 窄屏顶栏 ⋯ 菜单：与工作台头部按钮共用同一套动作 */
$("btn-kebab").addEventListener("click", (e) => {
    e.stopPropagation();
    $("kebab-menu").classList.toggle("hidden");
});
document.addEventListener("click", (e) => {
    if (!e.target.closest(".kebab-wrap"))
        $("kebab-menu").classList.add("hidden");
});
window.addEventListener("keydown", (e) => {
    if (e.key === "Escape")
        $("kebab-menu").classList.add("hidden");
});
/* 会话改名：只改展示名，id / 目录 / 容器名都从 id 派生，不受影响 */
async function renameSession() {
    const sess = S.current;
    if (!sess)
        return;
    const name = await askPrompt({
        title: "重命名会话",
        label: "会话名称",
        value: sess.name,
        hint: "1-64 个字符。工作区、对话记录都不受影响。",
        validate: (v) => {
            const t = v.trim();
            if (!t)
                return "名称不能为空";
            if ([...t].length > 64)
                return "名称最多 64 个字符";
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
        toast("会话已重命名");
    }
    catch (e) {
        toast("重命名失败：" + e.message, true);
    }
}
const kebabDo = (fn) => () => { $("kebab-menu").classList.add("hidden"); fn(); };
$("kb-start").addEventListener("click", kebabDo(doStart));
$("kb-stop").addEventListener("click", kebabDo(doStop));
$("kb-usage").addEventListener("click", kebabDo(openAcctUsage));
$("kb-rename").addEventListener("click", kebabDo(renameSession));
$("kb-delete").addEventListener("click", kebabDo(openDeleteDlg));
$("del-cancel").addEventListener("click", () => $("dlg-del").close());
let delBusy = false;
$("dlg-del").addEventListener("cancel", (e) => { if (delBusy)
    e.preventDefault(); }); // 删除中禁止 Esc 关闭
$("del-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const s = S.current;
    if (!s || delBusy)
        return;
    delBusy = true;
    btnBusy($("del-ok"), "删除中…");
    $("del-cancel").disabled = true;
    const purge = $("del-purge").checked ? "?purge=1" : "";
    try {
        await api(`/sessions/${s.id}${purge}`, { method: "DELETE" });
        $("dlg-del").close();
        closeChannels();
        S.current = null;
        $("workbench").classList.add("hidden");
        $("empty").classList.remove("hidden");
        updateTopbarTitle();
        refreshAll();
    }
    catch (err) {
        toast("删除失败：" + err.message, true);
    }
    delBusy = false;
    btnDone($("del-ok"));
    $("del-cancel").disabled = false;
});
/* ---------------- 新建会话 ---------------- */
decorateAgentOpts($("new-form"));
/* 空状态：亮明本箱预装的两家 Agent CLI；CTA 与侧栏「新建会话」同一入口 */
for (const a of ["claude", "codex"]) {
    const chip = document.createElement("span");
    chip.className = "brand-chip agent-" + a;
    chip.append(agentIcon(a, 14), agentName(a));
    $("empty-brands").append(chip);
}
$("empty-new").addEventListener("click", () => $("btn-new").click());
/* 侧栏字标 = 首页入口：放下当前会话回到空状态（只断前端通道，容器不动） */
$("btn-home").addEventListener("click", () => {
    if (S.current) {
        closeChannels();
        S.current = null;
        $("workbench").classList.add("hidden");
        $("empty").classList.remove("hidden");
    }
    showView("work"); /* 已在工作台时走早退分支，仍会收抽屉 */
    updateTopbarTitle();
    renderSidebar();
});
$("btn-new").addEventListener("click", () => {
    fillAccountSelect();
    $("new-error").classList.add("hidden");
    $("dlg-new").showModal();
});
$("new-cancel").addEventListener("click", () => $("dlg-new").close());
for (const r of document.querySelectorAll('#new-form input[name="agent"]')) {
    r.addEventListener("change", fillAccountSelect);
}
function fillAccountSelect() {
    const agent = document.querySelector('#new-form input[name="agent"]:checked').value;
    const sel = $("new-account");
    sel.replaceChildren();
    for (const a of S.accounts.filter((x) => x.type === agent)) {
        const o = document.createElement("option");
        o.value = a.id;
        o.textContent = `${a.label}（${a.sessions} 个会话在用）`;
        sel.appendChild(o);
    }
    if (!sel.children.length) {
        const o = document.createElement("option");
        o.value = "";
        o.textContent = "该类型下没有可用账号";
        sel.appendChild(o);
    }
}
$("new-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const agent = document.querySelector('#new-form input[name="agent"]:checked').value;
    const body = {
        name: $("new-name").value.trim(),
        agent,
        account_id: $("new-account").value,
    };
    btnBusy($("new-ok"), "创建中…");
    $("new-cancel").disabled = true;
    try {
        const sess = await api("/sessions", { method: "POST", body: JSON.stringify(body) });
        $("dlg-new").close();
        $("new-name").value = "";
        await refreshAll();
        openSession(sess);
    }
    catch (err) {
        $("new-error").textContent = err.message;
        $("new-error").classList.remove("hidden");
    }
    finally {
        btnDone($("new-ok"));
        $("new-cancel").disabled = false;
    }
});
