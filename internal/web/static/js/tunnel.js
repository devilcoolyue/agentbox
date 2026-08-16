/* tunnel：内网反向隧道 —— 侧栏状态入口 + 状态/接入指引页。
 * 状态随 data-updated 节流刷新（≥15s 一次），进入本页时立即刷新一次。
 * 普通用户在功能未启用时看不到入口；管理员始终可见（含「前往设置」引导）。 */
"use strict";
import { S, bus, emit } from "./state.js";
import { $, toast, fmtUptime } from "./util.js";
import { api } from "./api.js";
import { showView } from "./shell.js";
import { setTip } from "./tip.js";
let st = null; // GET /api/tunnel/status 缓存
let lastFetch = 0;
const onPage = () => S.view === "tunnel";
async function refreshStatus(force) {
    if (!force && Date.now() - lastFetch < 15000)
        return;
    lastFetch = Date.now();
    try {
        st = await api("/tunnel/status");
    }
    catch (_) {
        return; // 网络抖动保持现状
    }
    renderSideItem();
    if (onPage())
        renderPage();
}
/* ---- 侧栏入口 ---- */
function renderSideItem() {
    const btn = $("btn-tunnel");
    if (!st || (!st.enabled && S.role !== "admin")) {
        btn.classList.add("hidden");
        return;
    }
    btn.classList.remove("hidden");
    const dot = $("tunnel-dot");
    dot.classList.toggle("on", !!st.connected);
    $("tunnel-text").textContent = !st.enabled ? "未启用" : (st.connected ? "在线" : "离线");
}
/* ---- 页面 ---- */
function renderPage() {
    if (!st)
        return;
    const conn = !!st.connected;
    $("tun-st-dot").classList.toggle("on", conn);
    $("tun-st-title").textContent = !st.enabled ? "功能未启用" : (conn ? "隧道在线" : "等待接入");
    let sub = "";
    if (conn) {
        sub = "来自 " + (st.remote || "?") + " · 已连接 " + fmtUptime(Date.now() - st.since);
    }
    else if (st.enabled) {
        sub = "SOCKS5 代理 " + st.proxy + (st.proxy_up ? " 就绪" : " 未监听");
    }
    $("tun-st-sub").textContent = sub;
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
        adm.textContent = "在线隧道用户：" + (online.length ? online.join("、") : "无");
    $("tun-off-admin").classList.toggle("hidden", st.enabled);
    $("tun-guide").classList.toggle("hidden", !st.enabled);
}
/* 平台名映射：文件名里带 os-arch，展示成人话 */
function platformLabel(name) {
    const n = name.toLowerCase();
    if (n.includes("windows"))
        return "Windows";
    if (n.includes("darwin"))
        return n.includes("arm64") ? "macOS（Apple 芯片）" : "macOS（Intel）";
    if (n.includes("linux"))
        return n.includes("arm64") ? "Linux（arm64）" : "Linux";
    return name;
}
async function loadClients() {
    let list = [];
    try {
        list = await api("/tunnel/clients");
    }
    catch (_) { }
    const box = $("tun-dl");
    box.replaceChildren();
    $("tun-dl-empty").classList.toggle("hidden", !!list.length);
    for (const c of list) {
        const a = document.createElement("a");
        a.className = "btn btn-sm tun-dl-btn";
        a.textContent = platformLabel(c.name);
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
    $("tun-pair-code").classList.add("hidden");
    $("tun-pair-code").textContent = "";
    $("tun-pair-hint").textContent = "10 分钟内有效，只能用一次";
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
        box.classList.remove("hidden");
        try {
            await navigator.clipboard.writeText(res.code);
            toast("配对码已复制，粘贴到 abox-link 控制台");
        }
        catch (_) {
            toast("配对码已生成，请手动选中复制");
        }
        startPairCountdown(res.expires_in);
    }
    catch (e) {
        toast(e.message || "生成配对码失败", true);
    }
    finally {
        btn.disabled = false;
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
            hint.textContent = "已过期，请重新生成";
            $("tun-pair-code").classList.add("hidden");
            return;
        }
        hint.textContent = "剩余 " + Math.floor(left / 60) + ":" +
            String(left % 60).padStart(2, "0") + " 内有效，只能用一次";
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
