import { setAttrRender, setText, setTextRender, t as i18nText } from "./i18n.js";
import { skelBar } from "./skeleton.js";
/* tunnel：内网反向隧道 —— 侧栏状态入口 + 状态/接入指引页。
 * 状态随 data-updated 节流刷新（≥15s 一次），进入本页时立即刷新一次。
 * 普通用户在功能未启用时看不到入口；管理员始终可见（含「前往设置」引导）。 */
"use strict";
import { buttonLabel } from "./icons.js";
import { S, bus, emit } from "./state.js";
import { $, toast, fmtUptime } from "./util.js";
import { api } from "./api.js";
import { showView } from "./shell.js";
import { setTip } from "./tip.js";
let latestStatus = null; // GET /api/tunnel/status 缓存
let lastFetch = 0;
const onPage = () => S.view === "tunnel";
async function refreshStatus(force) {
    if (!force && Date.now() - lastFetch < 15000) {
        renderChatNetwork();
        return;
    }
    lastFetch = Date.now();
    try {
        latestStatus = await api("/tunnel/status");
    }
    catch (_) {
        return; // 网络抖动保持现状
    }
    renderSideItem();
    renderChatNetwork();
    if (onPage())
        renderPage();
}
/* ---- 侧栏入口 ---- */
function renderSideItem() {
    const st = latestStatus;
    const btn = $("btn-tunnel");
    if (!st || (!st.enabled && S.role !== "admin")) {
        btn.classList.add("hidden");
        return;
    }
    btn.classList.remove("hidden");
    const dot = $("tunnel-dot");
    dot.classList.toggle("on", !!st.connected);
    setTextRender($("tunnel-text"), () => !st.enabled ? i18nText("未启用") : (st.connected ? i18nText("在线") : i18nText("离线")));
    const label = i18nText("内网隧道：") + $("tunnel-text").textContent;
    setTip(btn, label);
    btn.setAttribute("aria-label", label);
}
function renderChatNetwork() {
    const st = latestStatus;
    const btn = $("chat-network");
    btn.classList.toggle("hidden", !st?.enabled);
    if (!st?.enabled)
        return;
    const current = st.workspaces?.find(w => w.session === S.current?.id);
    const state = !st.connected ? "offline" : !st.transparent || !st.client_transparent ? "proxy" : current?.ready ? "ready" : "pending";
    const states = {
        offline: { label: i18nText("离线"), hint: i18nText("本机客户端未连接，点击查看接入方式") },
        proxy: { label: i18nText("代理模式"), hint: i18nText("通过代理或端口映射访问内网，点击查看连接详情") },
        ready: { label: i18nText("已连接"), hint: i18nText("当前工作空间可直接访问已放行的内网目标，点击查看详情") },
        pending: { label: i18nText("待就绪"), hint: i18nText("客户端已连接，当前工作空间的网络尚未就绪，点击查看详情") },
    };
    btn.dataset.state = state;
    $("chat-network-status").textContent = states[state].label;
    setAttrRender(btn, "aria-label", () => i18nText("内网：{p0}，查看连接详情", { p0: String(states[state].label) }));
    setTip(btn, states[state].hint);
}
$("chat-network").addEventListener("click", openTunnelView);
$("tun-probe").addEventListener("click", async () => {
    const btn = $("tun-probe");
    btn.disabled = true;
    try {
        const result = await api("/tunnel/probe", { method: "POST", body: JSON.stringify({ target: $("tun-probe-target").value.trim() }) });
        setTextRender($("tun-probe-result"), () => i18nText("客户端到目标连接成功 · ") + result.elapsed_ms + i18nText(" ms（工作空间就绪状态见上方）"));
    }
    catch (err) {
        $("tun-probe-result").textContent = String(err.message);
    }
    finally {
        btn.disabled = false;
    }
});
/* ---- 页面 ---- */
function renderPage() {
    const st = latestStatus;
    if (!st)
        return;
    const conn = !!st.connected;
    $("tun-st-dot").classList.toggle("on", conn);
    setTextRender($("tun-st-title"), () => !st.enabled ? i18nText("功能未启用") : (conn ? i18nText("隧道在线") : i18nText("等待接入")));
    let sub = "";
    if (conn) {
        sub = i18nText("来自 ") + (st.remote || "?") + i18nText(" · 已连接 ") + fmtUptime(Date.now() - (st.since || Date.now()));
    }
    else if (st.enabled) {
        sub = i18nText("SOCKS5 代理 ") + st.proxy + (st.proxy_up ? i18nText(" 就绪") : i18nText(" 未监听"));
    }
    $("tun-st-sub").textContent = sub;
    setTextRender($("tun-mode-current"), () => !st.enabled ? i18nText("隧道未启用") : !st.transparent ? i18nText("服务端当前：兼容代理模式（暂不推荐）") : !conn ? i18nText("服务端默认：透明模式（推荐），等待客户端连接") : st.client_transparent ? i18nText("当前连接：透明模式（推荐）") : i18nText("当前连接：兼容代理模式（暂不推荐）；服务端已支持透明模式"));
    setTextRender($("tun-guide-mode"), () => st.transparent ? i18nText("新版客户端默认使用透明模式；旧配置请勾选「透明内网访问」，保存并启动。等待工作空间显示「网络就绪」后，对话与终端可直接使用放行的内网地址。") : i18nText("客户端在线后，Agent 可通过代理或端口映射访问放行目标；断开后暂停访问。"));
    $("tun-network").classList.toggle("hidden", !st.transparent);
    setTextRender($("tun-network-note"), () => st.network_error || (!st.connected ? i18nText("隧道离线，已配置的内网目标暂停访问。") : !st.client_transparent ? i18nText("客户端使用兼容模式；在新版 abox-link 中开启透明内网访问。") : i18nText("对话与终端可直接访问下列目标，无需配置代理。支持 IPv4 / TCP。")));
    setTextRender($("tun-network-rules"), () => (st.rules || []).join(" · ") || i18nText("尚未配置透明访问目标"));
    const workspaces = $("tun-network-workspaces");
    workspaces.replaceChildren();
    for (const item of st.workspaces || []) {
        const row = document.createElement("p");
        setTextRender(row, () => item.name + "：" + (item.ready ? i18nText("网络就绪") : item.error || i18nText("正在准备网络")));
        workspaces.appendChild(row);
    }
    // 端口映射列表
    const mapsBox = $("tun-maps");
    mapsBox.replaceChildren();
    const maps = st.maps || [];
    mapsBox.classList.toggle("hidden", !maps.length);
    for (const m of maps) {
        const row = document.createElement("div");
        row.className = "tun-map mono";
        row.textContent = m.listen + "  →  " + m.target;
        mapsBox.appendChild(row);
    }
    // 管理员：在线用户一览
    const adm = $("tun-admin-online");
    const online = st.online_users;
    adm.classList.toggle("hidden", !(S.role === "admin" && online));
    if (online)
        setTextRender(adm, () => i18nText("在线隧道用户：") + (online.length ? online.join("、") : i18nText("无")));
    $("tun-off-admin").classList.toggle("hidden", st.enabled);
    $("tun-guide").classList.toggle("hidden", !st.enabled);
}
/* 平台名映射：文件名里带 os-arch，展示成人话 */
function platformLabel(name) {
    const n = name.toLowerCase();
    if (n.includes("windows"))
        return "Windows";
    if (n.includes("darwin"))
        return n.includes("arm64") ? i18nText("macOS（Apple 芯片）") : "macOS（Intel）";
    if (n.includes("linux"))
        return n.includes("arm64") ? "Linux（arm64）" : "Linux";
    return name;
}
async function loadClients() {
    const box = $("tun-dl");
    // 第一次读取时先放几颗按钮大小的骨架，下载区不从空白突然撑开
    if (!box.children.length)
        box.replaceChildren(...Array.from({ length: 5 }, (_, i) => skelBar(7 + (i % 3) + "em", "btn-like")));
    let list = [];
    try {
        list = await api("/tunnel/clients");
    }
    catch (_) { }
    box.replaceChildren();
    $("tun-dl-empty").classList.toggle("hidden", !!list.length);
    for (const c of list) {
        const a = document.createElement("a");
        a.className = "btn btn-sm tun-dl-btn";
        buttonLabel(a, platformLabel(c.name), "download");
        setTip(a, c.name + " · " + (c.size / 1048576).toFixed(1) + " MB");
        a.href = "/api/tunnel/clients/" + encodeURIComponent(c.name) +
            "?token=" + encodeURIComponent(S.token);
        a.setAttribute("download", c.name);
        box.appendChild(a);
    }
}
let pairTimer = 0; // 配对码有效期倒计时
/* 进页面先清掉上次的配对码：码是一次性短时效的，隔一会儿回来那个多半已失效，
 * 留在页面上只会让人以为还能用。 */
function resetPair() {
    clearInterval(pairTimer);
    $("tun-pair-result").classList.add("hidden");
    $("tun-pair-code").textContent = "";
    setText($("tun-pair-hint"), "10 分钟内有效，只能用一次");
}
export function openTunnelView() {
    showView("tunnel");
    resetPair();
    renderPage();
    refreshStatus(true);
    loadClients();
}
$("tun-goto-settings").addEventListener("click", () => {
    S.sec = "security";
    emit("open-settings");
});
/* 配对码：一次性、短时效，粘进 abox-link 控制台即完成接入。
 * 生成后顺手复制到剪贴板，并倒计时显示剩余有效期。 */
$("tun-pair").addEventListener("click", async () => {
    const btn = $("tun-pair");
    btn.disabled = true;
    try {
        const res = await api("/tunnel/pair", {
            method: "POST",
            body: JSON.stringify({ origin: location.origin }),
        });
        const box = $("tun-pair-code");
        box.textContent = res.code;
        $("tun-pair-result").classList.remove("hidden");
        try {
            await navigator.clipboard.writeText(res.code);
            toast(i18nText("配对码已复制，粘贴到 abox-link 控制台"));
        }
        catch (_) {
            toast(i18nText("配对码已生成，请手动选中复制"));
        }
        startPairCountdown(res.expires_in);
    }
    catch (e) {
        toast(e.message || i18nText("生成配对码失败"), true);
    }
    finally {
        btn.disabled = false;
    }
});
$("tun-pair-copy").addEventListener("click", async () => {
    const code = $("tun-pair-code").textContent;
    if (!code || $("tun-pair-result").classList.contains("hidden"))
        return;
    try {
        await navigator.clipboard.writeText(code);
        toast(i18nText("配对码已复制，粘贴到 abox-link 控制台"));
    }
    catch (_) {
        toast(i18nText("复制失败，请手动选中配对码复制"), true);
    }
});
function startPairCountdown(seconds) {
    clearInterval(pairTimer);
    const hint = $("tun-pair-hint");
    const deadline = Date.now() + seconds * 1000;
    const tick = () => {
        const left = Math.round((deadline - Date.now()) / 1000);
        if (left <= 0) {
            clearInterval(pairTimer);
            setText(hint, "已过期，请重新生成");
            $("tun-pair-result").classList.add("hidden");
            $("tun-pair-code").textContent = "";
            return;
        }
        setTextRender(hint, () => i18nText("剩余 ") + Math.floor(left / 60) + ":" +
            String(left % 60).padStart(2, "0") + i18nText(" 内有效，只能用一次"));
    };
    tick();
    pairTimer = setInterval(tick, 1000);
}
/* 停留在本页期间 5s 一刷，让「等待接入 → 在线」立刻可见 */
setInterval(() => {
    if (onPage())
        refreshStatus(true);
}, 5000);
bus.addEventListener("data-updated", () => refreshStatus(false));
bus.addEventListener("open-tunnel", openTunnelView);
bus.addEventListener("open-session", () => { renderChatNetwork(); void refreshStatus(true); });
window.addEventListener("agentbox-language-change", () => {
    renderSideItem();
    renderChatNetwork();
});
