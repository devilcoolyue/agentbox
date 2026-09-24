/* shell：应用外壳 —— 抽屉、主区视图切换（工作台/设置）、顶栏标题、侧栏渲染。
 * 点击会话卡片 / 系统设置入口通过 bus 广播，由 sessions.js / settings.js 接管，
 * 保持 shell 不反向依赖任何功能模块。 */
"use strict";
import { S, bus, emit } from "./state.js";
import { $ } from "./util.js";
import { agentIcon, agentAvatar, agentName } from "./brand.js";
import { hideTip, setTip } from "./tip.js";
/* ---- 侧栏：桌面收起偏好与移动抽屉各自独立 ---- */
const narrowMQ = window.matchMedia("(max-width: 760px)");
const sidebar = $("sidebar");
const main = document.querySelector(".main");
const topbar = document.querySelector(".topbar");
const toggle = $("btn-sidebar-toggle");
// index.html 在首屏恢复同一个键，避免刷新时宽度跳动。
const SIDEBAR_KEY = "agentbox_sidebar_collapsed";
/* 用户弹层：展开时向上贴齐头像，图标栏时在侧栏右侧展开。 */
const userButton = $("btn-user-menu");
const userMenu = $("sidebar-user-menu");
const hoverMQ = window.matchMedia("(hover: hover) and (pointer: fine)");
let userMenuCloseTimer;
function cancelUserMenuClose() {
    clearTimeout(userMenuCloseTimer);
    userMenuCloseTimer = undefined;
}
function renderUserMenu() {
    const role = S.role === "admin" ? "管理员" : "普通用户";
    $("sidebar-user-name").textContent = S.user;
    $("sidebar-user-role").textContent = role;
    userButton.setAttribute("aria-label", `${S.user} · ${role}，用户菜单`);
}
function closeUserMenu(restoreFocus = false) {
    cancelUserMenuClose();
    userMenu.classList.remove("open");
    userMenu.inert = true;
    userButton.setAttribute("aria-expanded", "false");
    renderUserMenu();
    if (restoreFocus)
        userButton.focus();
}
function positionUserMenu() {
    userMenu.style.removeProperty("left");
    userMenu.style.removeProperty("top");
    if (narrowMQ.matches)
        return; // 抽屉有 transform，改由 CSS 在底栏上方定位。
    const anchor = userButton.getBoundingClientRect();
    const collapsed = document.documentElement.dataset.sidebarCollapsed === "true";
    const left = collapsed ? sidebar.getBoundingClientRect().right + 8 : anchor.right - userMenu.offsetWidth;
    const top = (collapsed ? anchor.bottom : anchor.top - 8) - userMenu.offsetHeight;
    userMenu.style.left = Math.max(8, Math.min(left, innerWidth - userMenu.offsetWidth - 8)) + "px";
    userMenu.style.top = Math.max(8, Math.min(top, innerHeight - userMenu.offsetHeight - 8)) + "px";
}
function openUserMenu(focus = false) {
    cancelUserMenuClose();
    hideTip();
    userMenu.inert = false;
    renderUserMenu();
    positionUserMenu();
    userMenu.classList.add("open");
    userButton.setAttribute("aria-expanded", "true");
    if (focus)
        userMenu.focus({ preventScroll: true });
}
userButton.addEventListener("click", e => {
    // 悬停已打开时，点击进入弹层；触屏仍可再次点击关闭。
    if (!userMenu.inert && (!hoverMQ.matches || e.pointerType === "touch")) {
        closeUserMenu();
        return;
    }
    openUserMenu(true);
});
for (const el of [userButton, userMenu]) {
    el.addEventListener("pointerenter", e => {
        if (!hoverMQ.matches || e.pointerType === "touch")
            return;
        openUserMenu();
    });
    el.addEventListener("pointerleave", () => {
        if (!hoverMQ.matches)
            return;
        cancelUserMenuClose();
        // 留出跨越图标和弹层间隙的时间；键盘操作期间保持打开。
        userMenuCloseTimer = setTimeout(() => {
            if (!userMenu.contains(document.activeElement))
                closeUserMenu();
        }, 200);
    });
}
for (const event of ["pointerdown", "focusin"]) {
    document.addEventListener(event, e => {
        if (userMenu.inert || userMenu.contains(e.target) || userButton.contains(e.target))
            return;
        closeUserMenu(event === "pointerdown" && userMenu.contains(document.activeElement));
    });
}
document.addEventListener("keydown", e => {
    if (e.key !== "Escape" || userMenu.inert)
        return;
    e.preventDefault();
    e.stopPropagation(); // 第一次 Esc 关闭用户弹层，第二次再关闭抽屉。
    closeUserMenu(true);
});
window.addEventListener("resize", () => closeUserMenu(userMenu.contains(document.activeElement)));
bus.addEventListener("unauthorized", () => closeUserMenu());
function syncSidebar() {
    const open = narrowMQ.matches && sidebar.classList.contains("open");
    sidebar.inert = narrowMQ.matches && !open;
    main.inert = topbar.inert = open;
    $("btn-menu").setAttribute("aria-expanded", String(open));
    const collapsed = !narrowMQ.matches && document.documentElement.dataset.sidebarCollapsed === "true";
    const label = narrowMQ.matches ? "关闭菜单" : collapsed ? "展开侧栏" : "收起侧栏";
    toggle.setAttribute("aria-expanded", String(narrowMQ.matches ? open : !collapsed));
    toggle.setAttribute("aria-label", label);
    setTip(toggle, label);
}
export function openDrawer() {
    if (!narrowMQ.matches)
        return;
    sidebar.classList.add("open");
    $("scrim").classList.add("show");
    syncSidebar();
    $("btn-sidebar-close").focus();
}
export function closeDrawer() {
    const restoreFocus = narrowMQ.matches && sidebar.classList.contains("open") && !document.querySelector("dialog[open]");
    sidebar.classList.remove("open");
    $("scrim").classList.remove("show");
    closeUserMenu();
    hideTip();
    syncSidebar();
    if (restoreFocus)
        $("btn-menu").focus();
}
$("btn-menu").addEventListener("click", openDrawer);
$("btn-sidebar-close").addEventListener("click", closeDrawer);
$("scrim").addEventListener("click", closeDrawer);
toggle.addEventListener("click", () => {
    if (narrowMQ.matches) {
        closeDrawer();
        return;
    }
    closeUserMenu();
    hideTip();
    const collapsed = document.documentElement.dataset.sidebarCollapsed !== "true";
    document.documentElement.dataset.sidebarCollapsed = String(collapsed);
    try {
        localStorage.setItem(SIDEBAR_KEY, collapsed ? "1" : "0");
    }
    catch { /* 本次仍生效 */ }
    syncSidebar();
});
narrowMQ.addEventListener("change", closeDrawer);
window.addEventListener("keydown", (e) => {
    if (!sidebar.classList.contains("open") || document.querySelector("dialog[open]"))
        return;
    if (e.key === "Escape") {
        e.preventDefault();
        closeDrawer();
    }
    if (e.key !== "Tab")
        return;
    const targets = [...sidebar.querySelectorAll("button:not(:disabled), [tabindex='0']")]
        .filter(el => !el.closest("[inert]") && el.getClientRects().length && getComputedStyle(el).visibility !== "hidden");
    const first = targets[0], last = targets[targets.length - 1];
    if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last?.focus();
    }
    else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first?.focus();
    }
});
syncSidebar();
/* ---- 顶栏：设置视图显示标题，工作台视图显示 状态灯+会话名+⋯菜单 ---- */
const VIEW_TITLE = {
    settings: "系统设置", usage: "使用记录", tunnel: "内网隧道",
};
export function updateTopbarTitle() {
    const inWork = S.view === "work" && !!S.current;
    $("topbar-title").textContent = VIEW_TITLE[S.view] || (S.current ? S.current.name : "");
    const led = $("tb-led");
    led.classList.toggle("hidden", !inWork);
    led.classList.toggle("on", inWork && S.current.status === "running");
    const ico = $("tb-ico");
    ico.classList.toggle("hidden", !inWork);
    ico.replaceChildren();
    if (inWork)
        ico.appendChild(agentIcon(S.current.agent, 15));
    $("kebab-wrap").classList.toggle("hidden", !inWork);
}
/* ---- 主区视图切换 ---- */
export function showView(name) {
    emit("navigation-changed");
    if (S.view === name) {
        closeDrawer();
        updateTopbarTitle();
        return;
    }
    S.view = name;
    emit("view-changed", name);
    $("view-work").classList.toggle("hidden", name !== "work");
    $("view-settings").classList.toggle("hidden", name !== "settings");
    $("view-usage").classList.toggle("hidden", name !== "usage");
    $("view-tunnel").classList.toggle("hidden", name !== "tunnel");
    for (const [id, view] of [["btn-settings", "settings"], ["btn-usagelog", "usage"], ["btn-tunnel", "tunnel"]]) {
        $(id).classList.toggle("active", name === view);
        if (name === view)
            $(id).setAttribute("aria-current", "page");
        else
            $(id).removeAttribute("aria-current");
    }
    updateTopbarTitle();
    closeDrawer();
    renderSidebar();
}
$("btn-settings").addEventListener("click", () => emit("open-settings"));
/* btn-usagelog 而非 btn-usage：后者是工作台头部的「额度」按钮，见 index.html 注释。 */
$("btn-usagelog").addEventListener("click", () => emit("open-usage"));
$("btn-tunnel").addEventListener("click", () => emit("open-tunnel"));
/* ---- 侧栏：会话列表 ---- */
export function renderSidebar() {
    renderUserMenu();
    const list = $("session-list");
    const scrollTop = list.scrollTop;
    const focusedID = document.activeElement?.closest(".session-card")?.dataset.sessionId;
    list.replaceChildren();
    $("session-count").textContent = String(S.sessions.length);
    if (!S.sessions.length) {
        const p = document.createElement("p");
        p.className = "session-empty";
        p.innerHTML = '<span class="session-empty-icon" aria-hidden="true">—</span><span class="session-empty-text">还没有工作空间，点击上方新建。</span>';
        setTip(p, "还没有工作空间，点击上方新建");
        list.appendChild(p);
    }
    for (const sess of S.sessions) {
        const card = document.createElement("button");
        const active = S.view === "work" && S.current?.id === sess.id;
        card.type = "button";
        card.className = "session-card" + (active ? " active" : "");
        card.dataset.sessionId = sess.id;
        if (active)
            card.setAttribute("aria-current", "page");
        const status = sess.status === "running" ? "运行中" : sess.stop_reason === "idle" ? "休眠" : "已停止";
        card.setAttribute("aria-label", `${sess.name}（${agentName(sess.agent)}，${status}）`);
        setTip(card, `${sess.name}\n${sess.account_label} · ${agentName(sess.agent)} · ${status}\n#${sess.id}`);
        const av = agentAvatar(sess.agent, { led: true });
        if (sess.status === "running")
            av.querySelector(".led").classList.add("on");
        const body = document.createElement("span");
        body.className = "sc-body";
        const h = document.createElement("span");
        h.className = "sc-name";
        h.textContent = sess.name;
        const meta = document.createElement("span");
        meta.className = "meta";
        meta.textContent = sess.account_label || agentName(sess.agent);
        body.append(h, meta);
        // 休眠 = 空闲自动停机（数据都在，发消息/开终端即自动唤醒）。与用户手动
        // 停止区分开，否则回来发现会话没了会以为服务出了故障。
        if (sess.stop_reason === "idle" && sess.status !== "running") {
            const zzz = document.createElement("span");
            zzz.className = "sc-sleep";
            zzz.textContent = "休眠";
            meta.append(document.createTextNode(" · "), zzz);
        }
        card.append(av, body);
        const open = () => emit("open-session", sess);
        card.addEventListener("click", open);
        list.appendChild(card);
        if (focusedID === sess.id)
            card.focus({ preventScroll: true });
    }
    list.scrollTop = scrollTop;
}
bus.addEventListener("data-updated", renderSidebar);
