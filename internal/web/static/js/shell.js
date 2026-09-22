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
/* 主题与退出共用一个轻量弹层；桌面图标栏向右展开，完整侧栏向上展开。 */
const moreButton = $("btn-sidebar-more");
const morePanel = $("sidebar-more");
function closeSidebarMore(restoreFocus = false) {
    morePanel.classList.remove("open");
    morePanel.inert = true;
    moreButton.setAttribute("aria-expanded", "false");
    setTip(moreButton, "更多选项");
    if (restoreFocus)
        moreButton.focus();
}
function positionSidebarMore() {
    morePanel.style.removeProperty("left");
    morePanel.style.removeProperty("top");
    if (narrowMQ.matches)
        return; // 抽屉内走绝对定位，不受侧栏 transform 的影响。
    const foot = moreButton.closest("footer").getBoundingClientRect();
    const collapsed = document.documentElement.dataset.sidebarCollapsed === "true";
    const left = collapsed ? sidebar.getBoundingClientRect().right + 8 : foot.left + 12;
    const top = (collapsed ? moreButton.getBoundingClientRect().bottom : foot.top - 4) - morePanel.offsetHeight;
    morePanel.style.left = Math.max(8, Math.min(left, innerWidth - morePanel.offsetWidth - 8)) + "px";
    morePanel.style.top = Math.max(8, Math.min(top, innerHeight - morePanel.offsetHeight - 8)) + "px";
}
moreButton.addEventListener("click", () => {
    if (morePanel.classList.contains("open")) {
        closeSidebarMore();
        return;
    }
    hideTip();
    setTip(moreButton, null);
    positionSidebarMore();
    morePanel.inert = false;
    morePanel.classList.add("open");
    moreButton.setAttribute("aria-expanded", "true");
    requestAnimationFrame(() => {
        if (!morePanel.inert)
            morePanel.querySelector("[data-theme-option].active")?.focus();
    });
});
for (const event of ["pointerdown", "focusin"]) {
    document.addEventListener(event, e => {
        if (!morePanel.contains(e.target) && !moreButton.contains(e.target))
            closeSidebarMore();
    });
}
document.addEventListener("keydown", e => {
    if (e.key !== "Escape" || !morePanel.classList.contains("open"))
        return;
    e.preventDefault();
    e.stopPropagation(); // 先关弹层，再按一次 Esc 才关移动抽屉。
    closeSidebarMore(true);
});
window.addEventListener("resize", () => {
    closeSidebarMore(morePanel.contains(document.activeElement));
    positionSidebarMore();
});
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
    closeSidebarMore();
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
    closeSidebarMore();
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
    if (S.view === name) {
        closeDrawer();
        updateTopbarTitle();
        return;
    }
    S.view = name;
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
    const list = $("session-list");
    const scrollTop = list.scrollTop;
    const focusedID = document.activeElement?.closest(".session-card")?.dataset.sessionId;
    list.replaceChildren();
    $("session-count").textContent = String(S.sessions.length);
    if (!S.sessions.length) {
        const p = document.createElement("p");
        p.className = "session-empty";
        p.innerHTML = '<span class="session-empty-icon" aria-hidden="true">—</span><span class="session-empty-text">还没有会话，点击上方新建。</span>';
        setTip(p, "还没有会话，点击上方新建");
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
