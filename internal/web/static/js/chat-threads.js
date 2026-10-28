import { setAttrRender, setText, setTextRender, t as i18nText } from "./i18n.js";
/* chat-threads：对话线程 —— 一个会话里可开多条对话，各自独立上下文，随时
 * 切回继续。顶部切换栏显示当前对话标题（也是历史面板入口），右侧滑出面板
 * 列出全部对话：点击切换续聊、可新建 / 删除。服务端按线程记录 provider
 * 会话 id，切回旧对话自动携带原上下文；早期迁移数据挖不出 id 的，继续时
 * 从空白上下文开始并给出提示。本端切换经 bus 的 thread-changed 事件由 chat.ts
 * 重载对话流。同一空间的几个对话可以同时运行（features/chat/running.ts）：运行中
 * 照样能切换、新建，列表和切换栏标出正在跑的对话；别的页面切换不会把本页带走。 */
"use strict";
import { S, emit } from "./state.js";
import { $, fmtSize, fmtTime, toast, askConfirm, askPrompt } from "./util.js";
import { api } from "./api.js";
import { svgIcon, USER_ATTACH_RE } from "./chat-render.js";
import { setTip } from "./tip.js";
import { keepCount, skelBar, skelShell } from "./skeleton.js";
import { onRunningChange, runningIDs, runningThread } from "./features/chat/running.js";
/* 预览文案：附件占位符只留类别名，不展示容器内路径 */
function previewText(s) {
    return String(s || "").replace(USER_ATTACH_RE, "[$1]") || i18nText("（无文字消息）");
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
    setTextRender($("thread-title"), () => S.thread?.title ? previewText(S.thread.title) : i18nText("新对话"));
    renderRunningBadge();
}
/* 空的新对话发出首条消息后，标题立即跟上，不必等下一次历史加载。标题只由开场
 * 消息决定，之后每轮消息都不该再动它 —— 已有正式标题时让位。 */
export function noteThreadTitle(text) {
    if ((!S.thread || S.thread.turns === 0 && !S.thread.title) && !titled) {
        if (S.thread) {
            S.thread.title = text;
            S.thread.turns = 1;
        }
        $("thread-title").textContent = previewText(text);
    }
}
/* 服务端异步生成好线程标题后广播过来，即时替换切换栏标题。
 * 新线程发首条消息时前端还没登记 S.thread（仍为 null），但 chat ws 按会话建立，
 * 广播只会在正查看该会话时到达，此刻显示的就是这条刚起好标题的线程——直接更新。
 * 已登记 S.thread 时则要 id 对上，避免改到已切走的其它线程。
 * 面板若开着，其列表会在下次打开时按最新元数据刷新。 */
export function applyThreadTitle(id, title) {
    const known = allThreads.find((t) => t.id === id);
    if (known)
        known.title = title;
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
let listSeq = 0;
/* 提示里用的对话名：列表里有就用标题，没有（还没打开过列表）就说「另一个对话」 */
export function threadTitleOf(id) {
    if (S.thread?.id === id && S.thread.title)
        return previewText(S.thread.title);
    const t = allThreads.find((x) => x.id === id);
    return t?.title ? previewText(t.title) : i18nText("另一个对话");
}
/* 切换栏：别的对话还在跑时，标题旁标出有几个 */
function renderRunningBadge() {
    const others = runningIDs().filter((id) => id !== S.thread?.id).length;
    const badge = $("thread-running");
    badge.classList.toggle("hidden", others === 0);
    setTextRender($("thread-running-text"), () => i18nText("{p0} 个运行中", { p0: String(others) }));
    setTip(badge, others ? () => i18nText("另有 {p0} 个对话正在运行，打开对话列表查看", { p0: String(others) }) : null);
}
onRunningChange(() => {
    renderRunningBadge();
    if (panelOpen())
        renderList();
});
/* 只认最后一次请求的结果。会话按 id 比对：8 秒一次的轮询会把 S.current 换成
 * 新对象，按对象比对时请求一旦跨过轮询就被当成过期丢掉，骨架屏永远不退。 */
async function refreshList() {
    const sess = S.current;
    if (!sess)
        return;
    const seq = ++listSeq;
    const list = $("tp-list");
    list.replaceChildren(threadsSkeleton(keepCount(list, ".tp-item", 5)));
    $("tp-count").textContent = "";
    try {
        const { threads } = await api(`/sessions/${sess.id}/chat/threads`, { signal: AbortSignal.timeout(15000) });
        if (seq !== listSeq || S.current?.id !== sess.id || !panelOpen())
            return;
        allThreads = threads;
        renderList();
    }
    catch (e) {
        if (seq !== listSeq || !panelOpen())
            return;
        const err = e;
        const d = document.createElement("div");
        d.className = "tp-empty";
        d.textContent = err.name === "TimeoutError" ? i18nText("历史对话加载超时") : i18nText("加载历史对话失败：") + err.message;
        const retry = document.createElement("button");
        retry.type = "button";
        retry.className = "btn btn-sm";
        setText(retry, "重试");
        retry.addEventListener("click", () => { void refreshList(); });
        d.append(document.createElement("br"), retry);
        list.replaceChildren(d);
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
        setText(d, "还没有对话记录。发出第一条消息后，这里会出现历史对话。");
        list.replaceChildren(d);
        return;
    }
    if (!shown.length) {
        const d = document.createElement("div");
        d.className = "tp-empty";
        setText(d, "没有标题匹配「{p0}」的对话。", { p0: String(q) });
        list.replaceChildren(d);
        setText($("tp-count"), "0 / {p0} 条", { p0: String(allThreads.length) });
        return;
    }
    setTextRender($("tp-count"), () => q
        ? i18nText("{p0} / {p1} 条", { p0: String(shown.length), p1: String(allThreads.length) })
        : allThreads.length + i18nText(" 条"));
    // 「当前」是本页正在看的对话：别的页面可能看着另一个
    list.replaceChildren(...shown.map((t) => threadItem(t, t.id === S.thread?.id)));
}
$("tp-search").addEventListener("input", renderList);
/* 历史对话骨架：图标位、标题、时间一行一条，与 threadItem 同高 */
function threadsSkeleton(count) {
    const wrap = skelShell(i18nText("加载中…"), "tp-skeleton");
    for (let i = 0; i < count; i++) {
        const row = document.createElement("div");
        row.className = "tp-item";
        row.setAttribute("aria-hidden", "true");
        const open = document.createElement("div");
        open.className = "tp-open";
        const glyph = skelBar("15px", "text");
        glyph.style.gridRow = "1 / 3";
        open.append(glyph, skelBar(45 + (i * 23) % 40, "text tp-title"), skelBar(30 + (i * 11) % 20, "text tp-meta"));
        row.append(open);
        wrap.append(row);
    }
    return wrap;
}
function threadItem(t, on) {
    const run = runningThread(t.id);
    const row = document.createElement("div");
    row.className = "tp-item" + (on ? " on" : "") + (run ? " running" : "");
    const open = document.createElement("button");
    open.type = "button";
    open.className = "tp-open";
    // 正在运行的对话用转圈的图标，一眼看出哪几条在跑
    const glyph = svgIcon(run ? "chat.thread-running" : "clock", 15);
    if (run)
        glyph.classList.add("tp-spin");
    open.appendChild(glyph);
    setTip(open, () => on ? i18nText("当前对话") : run ? i18nText("正在运行，切过去查看进度") : i18nText("切换到这条对话继续"));
    const title = document.createElement("span");
    title.className = "tp-title";
    title.textContent = previewText(t.title);
    const meta = document.createElement("span");
    meta.className = "tp-meta";
    setTextRender(meta, () => [run ? i18nText("运行中") : "", fmtTime(t.updated || t.ts), i18nText("{p0} 轮", { p0: String(t.turns || 0) }), t.size ? fmtSize(t.size) : "", on ? i18nText("当前") : ""]
        .filter(Boolean).join(" · "));
    open.append(title, meta);
    open.addEventListener("click", () => { if (on)
        closeThreadPanel();
    else
        switchThread(t); });
    const ren = document.createElement("button");
    ren.type = "button";
    ren.className = "tp-del"; // 与删除同一套图标按钮样式
    setAttrRender(ren, "aria-label", () => i18nText("重命名这条对话"));
    setTip(ren, () => i18nText("重命名这条对话"));
    ren.appendChild(svgIcon("rename", 15));
    ren.addEventListener("click", (e) => { e.stopPropagation(); renameThread(t); });
    const del = document.createElement("button");
    del.type = "button";
    del.className = "tp-del";
    setAttrRender(del, "aria-label", () => i18nText("删除这条对话"));
    setTip(del, () => i18nText("删除这条对话"));
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
        get title() { return i18nText("重命名对话"); },
        get label() { return i18nText("对话标题"); },
        value: previewText(t.title),
        get hint() { return i18nText("留作辨认用；不会影响对话内容与上下文。"); },
        validate: (v) => (v.trim() ? "" : i18nText("标题不能为空")),
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
        toast(i18nText("已重命名"));
    }
    catch (e) {
        toast(i18nText("重命名失败：") + e.message, true);
    }
}
/* ---- 动作 ---- */
/* 运行中的对话不能删：它的回合还在往记录里写 */
function busy(id) {
    if (!runningThread(id) && !(S.thread?.id === id && S.chatState === "running"))
        return false;
    toast(i18nText("这个对话正在运行，请先等待完成或中断"), true);
    return true;
}
async function switchThread(t) {
    const sess = S.current;
    if (!sess)
        return;
    try {
        const res = await api(`/sessions/${sess.id}/chat/threads/${t.id}/activate`, { method: "POST" });
        closeThreadPanel();
        emit("thread-changed");
        if (!res.resumable) {
            toast(i18nText("这条对话较早，没有可续聊的上下文标识；继续将从全新上下文开始（记录仍完整保留）"));
        }
    }
    catch (e) {
        toast(i18nText("切换对话失败：") + e.message, true);
    }
}
export async function newThread() {
    const sess = S.current;
    if (!sess)
        return;
    try {
        const res = await api(`/sessions/${sess.id}/chat/threads`, { method: "POST" });
        closeThreadPanel();
        if (res.created) {
            emit("thread-changed");
            toast(i18nText("已新建对话，之前的对话可在历史列表中继续"));
        }
        else {
            toast(i18nText("当前已是新对话"));
        }
    }
    catch (e) {
        toast(i18nText("新建对话失败：") + e.message, true);
    }
}
async function delThread(t) {
    const sess = S.current;
    if (!sess)
        return;
    const isCur = !!(S.thread && S.thread.id === t.id);
    if (busy(t.id))
        return;
    const name = previewText(t.title);
    const ok = await askConfirm(() => i18nText("删除对话「{p0}」？", { p0: String(name.length > 24 ? name.slice(0, 24) + "…" : name) }), {
        get title() { return i18nText("删除对话"); }, get hint() { return i18nText("该对话的全部消息记录不可恢复。"); }, get okLabel() { return i18nText("删除"); }, icon: "trash", danger: true,
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
        toast(i18nText("删除失败：") + e.message, true);
    }
}
/* ---- 静态装饰与事件挂载 ---- */
$("btn-threads").prepend(svgIcon("clock", 15));
$("btn-threads").append(svgIcon("chevron", 11));
$("btn-threads").addEventListener("click", openThreadPanel);
$("thread-running").prepend(svgIcon("chat.thread-running", 13));
$("thread-running").addEventListener("click", openThreadPanel);
$("btn-thread-new").addEventListener("click", newThread);
$("tp-new").addEventListener("click", newThread);
$("tp-close").addEventListener("click", closeThreadPanel);
$("tp-scrim").addEventListener("click", closeThreadPanel);
window.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && panelOpen())
        closeThreadPanel();
});
window.addEventListener("agentbox-language-change", () => {
    if (!S.thread && !titled)
        setText($("thread-title"), "新对话");
    if (panelOpen())
        renderList();
});
