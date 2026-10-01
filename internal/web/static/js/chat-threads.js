/* chat-threads：对话线程 —— 一个会话里可开多条对话，各自独立上下文，随时
 * 切回继续。顶部切换栏显示当前对话标题（也是历史面板入口），右侧滑出面板
 * 列出全部对话：点击切换续聊、可新建 / 删除。服务端按线程记录 provider
 * 会话 id，切回旧对话自动携带原上下文；早期迁移数据挖不出 id 的，继续时
 * 从空白上下文开始并给出提示。切换成功后服务端广播给同会话的其它页面，
 * 本端与广播都汇入 bus 的 thread-changed 事件，由 chat.ts 重载对话流。 */
"use strict";
import { S, emit } from "./state.js";
import { $, fmtTime, toast, withSpin, askConfirm, askPrompt } from "./util.js";
import { api } from "./api.js";
import { svgIcon, USER_ATTACH_RE } from "./chat-render.js";
import { setTip } from "./tip.js";
/* 预览文案：附件占位符只留类别名，不展示容器内路径 */
function previewText(s) {
    return String(s || "").replace(USER_ATTACH_RE, "[$1]") || "（无文字消息）";
}
/* ---- 顶部切换栏 ---- */
/* 切换栏上挂的是不是正式标题（模型总结好的 / 用户重命名的）。新线程是服务端
 * 在首条消息时静默建的、不广播，前端要等下一次历史加载才登记 S.thread，在那
 * 之前 S.thread 一直为 null，单看它分不清「还没标题」和「标题已经来了」——
 * 于是单独记一笔，置上之后 noteThreadTitle 就不再拿后续消息覆盖它。 */
let titled = false;
/* 由 chat.ts 的 loadHistory 用服务端返回的线程元数据刷新（空对话传 null） */
export function setThreadBar(thread) {
    S.thread = thread && thread.id ? thread : null;
    titled = false; // 已登记 S.thread 后改由下面的 id 比对把关
    $("thread-title").textContent = S.thread ? previewText(S.thread.title) : "新对话";
}
/* 空的新对话发出首条消息后，标题立即跟上，不必等下一次历史加载。标题只由开场
 * 消息决定，之后每轮消息都不该再动它 —— 已有正式标题时让位。 */
export function noteThreadTitle(text) {
    if (!S.thread && !titled)
        $("thread-title").textContent = previewText(text);
}
/* 服务端异步生成好线程标题后广播过来，即时替换切换栏标题。
 * 新线程发首条消息时前端还没登记 S.thread（仍为 null），但 chat ws 按会话建立，
 * 广播只会在正查看该会话时到达，此刻显示的就是这条刚起好标题的线程——直接更新。
 * 已登记 S.thread 时则要 id 对上，避免改到已切走的其它线程。
 * 面板若开着，其列表会在下次打开时按最新元数据刷新。 */
export function applyThreadTitle(id, title) {
    if (S.thread && S.thread.id !== id)
        return;
    if (S.thread)
        S.thread.title = title;
    titled = true;
    $("thread-title").textContent = previewText(title);
}
/* ---- 面板开合 ---- */
function panelOpen() { return $("thread-panel").classList.contains("open"); }
export function closeThreadPanel() {
    $("thread-panel").classList.remove("open");
    $("thread-panel").setAttribute("aria-hidden", "true");
    $("tp-scrim").classList.remove("show");
}
async function openThreadPanel() {
    if (!S.current)
        return;
    $("thread-panel").classList.add("open");
    $("thread-panel").setAttribute("aria-hidden", "false");
    $("tp-scrim").classList.add("show");
    await refreshList();
}
/* 最近一次拉到的列表：搜索过滤在本地做，重度用户攒下几十条也不必每敲一个字
 * 就打一次接口 */
let allThreads = [];
let activeID = "";
async function refreshList() {
    const sess = S.current;
    if (!sess)
        return;
    const list = $("tp-list");
    const load = document.createElement("div");
    load.className = "tp-empty";
    load.append(withSpin("加载中…"));
    list.replaceChildren(load);
    $("tp-count").textContent = "";
    try {
        const { threads, active } = await api(`/sessions/${sess.id}/chat/threads`);
        if (S.current !== sess || !panelOpen())
            return;
        allThreads = threads;
        activeID = active;
        renderList();
    }
    catch (e) {
        if (panelOpen())
            toast("加载历史对话失败：" + e.message, true);
    }
}
function renderList() {
    const list = $("tp-list");
    const q = $("tp-search").value.trim().toLowerCase();
    const shown = q
        ? allThreads.filter((t) => previewText(t.title).toLowerCase().includes(q))
        : allThreads;
    $("tp-search").classList.toggle("hidden", allThreads.length < 2 && !q);
    if (!allThreads.length) {
        const d = document.createElement("div");
        d.className = "tp-empty";
        d.textContent = "还没有对话记录。发出第一条消息后，这里会出现历史对话。";
        list.replaceChildren(d);
        return;
    }
    if (!shown.length) {
        const d = document.createElement("div");
        d.className = "tp-empty";
        d.textContent = `没有标题匹配「${q}」的对话。`;
        list.replaceChildren(d);
        $("tp-count").textContent = `0 / ${allThreads.length} 条`;
        return;
    }
    $("tp-count").textContent = q
        ? `${shown.length} / ${allThreads.length} 条`
        : allThreads.length + " 条";
    list.replaceChildren(...shown.map((t) => threadItem(t, t.id === activeID)));
}
$("tp-search").addEventListener("input", renderList);
function threadItem(t, on) {
    const row = document.createElement("div");
    row.className = "tp-item" + (on ? " on" : "");
    const open = document.createElement("button");
    open.type = "button";
    open.className = "tp-open";
    open.appendChild(svgIcon("clock", 15));
    setTip(open, on ? "当前对话" : "切换到这条对话继续");
    const title = document.createElement("span");
    title.className = "tp-title";
    title.textContent = previewText(t.title);
    const meta = document.createElement("span");
    meta.className = "tp-meta";
    meta.textContent = [fmtTime(t.updated || t.ts), `${t.turns || 0} 轮`, on ? "当前" : ""]
        .filter(Boolean).join(" · ");
    open.append(title, meta);
    open.addEventListener("click", () => { if (on)
        closeThreadPanel();
    else
        switchThread(t); });
    const ren = document.createElement("button");
    ren.type = "button";
    ren.className = "tp-del"; // 与删除同一套图标按钮样式
    ren.setAttribute("aria-label", "重命名这条对话");
    setTip(ren, "重命名这条对话");
    ren.appendChild(svgIcon("rename", 15));
    ren.addEventListener("click", (e) => { e.stopPropagation(); renameThread(t); });
    const del = document.createElement("button");
    del.type = "button";
    del.className = "tp-del";
    del.setAttribute("aria-label", "删除这条对话");
    setTip(del, "删除这条对话");
    del.appendChild(svgIcon("trash", 15));
    del.addEventListener("click", (e) => { e.stopPropagation(); delThread(t); });
    row.append(open, ren, del);
    return row;
}
async function renameThread(t) {
    const sess = S.current;
    if (!sess)
        return;
    const name = await askPrompt({
        title: "重命名对话",
        label: "对话标题",
        value: previewText(t.title),
        hint: "留作辨认用；不会影响对话内容与上下文。",
        validate: (v) => (v.trim() ? "" : "标题不能为空"),
    });
    if (name === null)
        return;
    try {
        const res = await api(`/sessions/${sess.id}/chat/threads/${t.id}`, {
            method: "PATCH",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ title: name.trim() }),
        });
        t.title = res.title;
        if (S.thread && S.thread.id === t.id)
            applyThreadTitle(t.id, res.title);
        renderList();
        toast("已重命名");
    }
    catch (e) {
        toast("重命名失败：" + e.message, true);
    }
}
/* ---- 动作 ---- */
function busy() {
    if (S.chatState !== "running")
        return false;
    toast("Agent 执行中，请先等待完成或中断", true);
    return true;
}
async function switchThread(t) {
    const sess = S.current;
    if (!sess || busy())
        return;
    try {
        const res = await api(`/sessions/${sess.id}/chat/threads/${t.id}/activate`, { method: "POST" });
        closeThreadPanel();
        emit("thread-changed");
        if (!res.resumable) {
            toast("这条对话较早，没有可续聊的上下文标识；继续将从全新上下文开始（记录仍完整保留）");
        }
    }
    catch (e) {
        toast("切换对话失败：" + e.message, true);
    }
}
export async function newThread() {
    const sess = S.current;
    if (!sess || busy())
        return;
    try {
        const res = await api(`/sessions/${sess.id}/chat/threads`, { method: "POST" });
        closeThreadPanel();
        if (res.created) {
            emit("thread-changed");
            toast("已新建对话，之前的对话可在历史列表中继续");
        }
        else {
            toast("当前已是新对话");
        }
    }
    catch (e) {
        toast("新建对话失败：" + e.message, true);
    }
}
async function delThread(t) {
    const sess = S.current;
    if (!sess)
        return;
    const isCur = !!(S.thread && S.thread.id === t.id);
    if (isCur && busy())
        return;
    const name = previewText(t.title);
    const ok = await askConfirm(`删除对话「${name.length > 24 ? name.slice(0, 24) + "…" : name}」？`, {
        title: "删除对话", hint: "该对话的全部消息记录不可恢复。", okLabel: "删除", icon: "trash", danger: true,
    });
    if (!ok)
        return;
    try {
        await api(`/sessions/${sess.id}/chat/threads/${t.id}`, { method: "DELETE" });
        if (isCur)
            emit("thread-changed"); // 服务端已自动切到最近一条
        await refreshList();
    }
    catch (e) {
        toast("删除失败：" + e.message, true);
    }
}
/* ---- 静态装饰与事件挂载 ---- */
$("btn-threads").prepend(svgIcon("clock", 15));
$("btn-threads").append(svgIcon("chevron", 11));
$("btn-threads").addEventListener("click", openThreadPanel);
$("btn-thread-new").addEventListener("click", newThread);
$("tp-new").addEventListener("click", newThread);
$("tp-close").addEventListener("click", closeThreadPanel);
$("tp-scrim").addEventListener("click", closeThreadPanel);
window.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && panelOpen())
        closeThreadPanel();
});
