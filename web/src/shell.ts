import { htmlText as trHTML, i18n, languages, setAttrRender, setTextRender, t as i18nText } from "./i18n.js";
/* shell：应用外壳 —— 抽屉、主区视图切换（工作台/设置）、顶栏标题、侧栏渲染。
 * 点击会话卡片 / 系统设置入口通过 bus 广播，由 sessions.js / settings.js 接管，
 * 保持 shell 不反向依赖任何功能模块。 */
"use strict";

import { S, bus, emit } from "./state.js";
import type { View } from "./state.js";
import { $ } from "./util.js";
import { agentIcon, agentAvatar, agentName } from "./brand.js";
import { hideTip, setTip } from "./tip.js";
import { sessionState } from "./session-state.js";
import { filterWorkspaces, workspaceFilterActive } from "./features/workspaces/filter.js";
import { mountPrefPicker } from "./pref-picker.js";
import { SKINS, SKIN_LABEL, currentSkin, setSkin } from "./theme.js";
import { ListMotion } from "./motion.js";

/* ---- 侧栏：桌面收起偏好与移动抽屉各自独立 ---- */

const narrowMQ = window.matchMedia("(max-width: 760px)");
const sidebar = $("sidebar");
const main = document.querySelector<HTMLElement>(".main")!;
const topbar = document.querySelector<HTMLElement>(".topbar")!;
const toggle = $("btn-sidebar-toggle");
// index.html 在首屏恢复同一个键，避免刷新时宽度跳动。
const SIDEBAR_KEY = "agentbox_sidebar_collapsed";

/* 用户弹层：展开时向上贴齐头像，图标栏时在侧栏右侧展开。 */
const userButton = $("btn-user-menu");
const userMenu = $("sidebar-user-menu");
const hoverMQ = window.matchMedia("(hover: hover) and (pointer: fine)");
let userMenuCloseTimer: ReturnType<typeof setTimeout> | undefined;
/** 在弹层里点过东西（选语言、风格等）、或从展开的语言/风格菜单移出后，不再随鼠标移出
 * 收起，点别处或 Esc 才关；不靠焦点判断，Safari 点按钮不获焦，结果会与 Chrome 不同。 */
let userMenuEngaged = false;

function cancelUserMenuClose() {
  clearTimeout(userMenuCloseTimer);
  userMenuCloseTimer = undefined;
}

function renderUserMenu() {
  const role = () => S.role === "admin" ? i18nText("管理员") : i18nText("普通用户");
  $("sidebar-user-name").textContent = S.user;
  setTextRender($("sidebar-user-role"), role);
  $("side-user-label").textContent = S.user;
  setAttrRender(userButton, "aria-label", () => i18nText("{p0} · {p1}，用户菜单", { p0: String(S.user), p1: String(role()) }));
}

/* 弹层里的语言、风格各占一行：右侧是当前选择和箭头，展开后在上方列出全部选项
 * （pref-picker.ts）。菜单是弹层的子节点，鼠标移进去不算离开弹层，选完弹层还在。 */
const prefPickers = [
  mountPrefPicker($("pref-language"), {
    title: () => i18nText("语言"),
    // 语言名用各自的写法，只有「跟随系统」随界面翻译
    options: languages.map(({ value, label }) => ({ value, label: value === "system" ? () => i18nText(label) : () => label })),
    value: () => i18n.preference,
    select: value => i18n.setLanguage(value as typeof i18n.preference),
  }),
  mountPrefPicker($("pref-skin"), {
    title: () => i18nText("风格"),
    options: SKINS.map(skin => ({ value: skin, label: SKIN_LABEL[skin], swatch: `var(--swatch-${skin})` })),
    value: currentSkin,
    select: setSkin,
  }),
];
// 其他标签页改了偏好、或系统语言变化时跟着换选中态
for (const event of ["agentbox-language-change", "agentbox-theme-change"]) {
  window.addEventListener(event, () => { for (const picker of prefPickers) picker.sync(); });
}

function closeUserMenu(restoreFocus = false) {
  cancelUserMenuClose();
  userMenuEngaged = false;
  for (const picker of prefPickers) picker.close();
  userMenu.classList.remove("open");
  userMenu.inert = true;
  userButton.setAttribute("aria-expanded", "false");
  renderUserMenu();
  if (restoreFocus) userButton.focus();
}

function positionUserMenu() {
  userMenu.style.removeProperty("left");
  userMenu.style.removeProperty("top");
  if (narrowMQ.matches) return; // 抽屉有 transform，改由 CSS 在底栏上方定位。
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
  if (focus) userMenu.focus({ preventScroll: true });
}

userButton.addEventListener("click", e => {
  // 悬停已打开时，点击进入弹层；触屏仍可再次点击关闭。
  if (!userMenu.inert && (!hoverMQ.matches || e.pointerType === "touch")) { closeUserMenu(); return; }
  openUserMenu(true);
});
for (const el of [userButton, userMenu]) {
  el.addEventListener("pointerenter", e => {
    if (!hoverMQ.matches || e.pointerType === "touch") return;
    openUserMenu();
  });
  el.addEventListener("pointerleave", () => {
    if (!hoverMQ.matches) return;
    cancelUserMenuClose();
    // 从展开的语言/风格菜单移出（它可能伸到弹层外）：只收那个菜单，弹层留着
    if (prefPickers.some(picker => picker.isOpen())) { userMenuEngaged = true; return; }
    // 留出跨越图标和弹层间隙的时间；键盘操作期间保持打开。
    userMenuCloseTimer = setTimeout(() => {
      if (!userMenuEngaged && !userMenu.contains(document.activeElement)) closeUserMenu();
    }, 200);
  });
}
userMenu.addEventListener("pointerdown", () => { userMenuEngaged = true; });
for (const event of ["pointerdown", "focusin"]) {
  document.addEventListener(event, e => {
    if (userMenu.inert || userMenu.contains(e.target as Node) || userButton.contains(e.target as Node)) return;
    closeUserMenu(event === "pointerdown" && userMenu.contains(document.activeElement));
  });
}
document.addEventListener("keydown", e => {
  if (e.key !== "Escape" || userMenu.inert) return;
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
  const label = narrowMQ.matches ? i18nText("关闭菜单") : collapsed ? i18nText("展开侧栏") : i18nText("收起侧栏");
  toggle.setAttribute("aria-expanded", String(narrowMQ.matches ? open : !collapsed));
  toggle.setAttribute("aria-label", label);
  setTip(toggle, label);
}

export function openDrawer() {
  if (!narrowMQ.matches) return;
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
  if (restoreFocus) $("btn-menu").focus();
}
$("btn-menu").addEventListener("click", openDrawer);
$("btn-sidebar-close").addEventListener("click", closeDrawer);
$("scrim").addEventListener("click", closeDrawer);
toggle.addEventListener("click", () => {
  if (narrowMQ.matches) { closeDrawer(); return; }
  closeUserMenu();
  hideTip();
  const collapsed = document.documentElement.dataset.sidebarCollapsed !== "true";
  document.documentElement.dataset.sidebarCollapsed = String(collapsed);
  try { localStorage.setItem(SIDEBAR_KEY, collapsed ? "1" : "0"); } catch { /* 本次仍生效 */ }
  syncSidebar();
});
narrowMQ.addEventListener("change", closeDrawer);
window.addEventListener("keydown", (e) => {
  if (!sidebar.classList.contains("open") || document.querySelector("dialog[open]")) return;
  if (e.key === "Escape") { e.preventDefault(); closeDrawer(); }
  if (e.key !== "Tab") return;
  const targets = [...sidebar.querySelectorAll<HTMLElement>("button:not(:disabled), input:not(:disabled), select:not(:disabled), [tabindex='0']")]
    .filter(el => !el.closest("[inert]") && el.getClientRects().length && getComputedStyle(el).visibility !== "hidden");
  const first = targets[0], last = targets[targets.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus(); }
});
syncSidebar();

/* ---- 顶栏：设置视图显示标题，工作台视图显示 状态灯+会话名+⋯菜单 ---- */

const VIEW_TITLE: Record<string, string> = {
  get git() { return i18nText("Git 管理"); }, get settings() { return i18nText("系统设置"); }, get usage() { return i18nText("使用记录"); }, get tunnel() { return i18nText("内网隧道"); },
};

export function updateTopbarTitle() {
  const inWork = S.view === "work" && !!S.current;
  setTextRender($("topbar-title"),()=>VIEW_TITLE[S.view] || (S.current ? S.current.name : ""));
  const led = $("tb-led");
  led.classList.toggle("hidden", !inWork);
  led.classList.toggle("on", inWork && S.current!.status === "running");
  const ico = $("tb-ico");
  ico.classList.toggle("hidden", !inWork);
  ico.replaceChildren();
  if (inWork) ico.appendChild(agentIcon(S.current!.agent, 15));
  $("kebab-wrap").classList.toggle("hidden", !inWork);
}

/* ---- 主区视图切换 ---- */

export function showView(name: View) {
  emit("navigation-changed");
  if (S.view === name) { closeDrawer(); updateTopbarTitle(); return; }
  S.view = name;
  emit("view-changed", name);
  $("view-work").classList.toggle("hidden", name !== "work");
  $("view-git").classList.toggle("hidden", name !== "git");
  $("view-settings").classList.toggle("hidden", name !== "settings");
  $("view-usage").classList.toggle("hidden", name !== "usage");
  $("view-tunnel").classList.toggle("hidden", name !== "tunnel");
  for (const [id, view] of [["btn-git-management", "git"], ["btn-settings", "settings"], ["btn-usagelog", "usage"], ["btn-tunnel", "tunnel"]]) {
    $(id).classList.toggle("active", name === view);
    if (name === view) $(id).setAttribute("aria-current", "page");
    else $(id).removeAttribute("aria-current");
  }
  updateTopbarTitle();
  closeDrawer();
  renderSidebar();
}

$("btn-settings").addEventListener("click", () => emit("open-settings"));
/* 账号额度在工作台头部的 ⋯ 菜单里；这里是侧栏的使用记录入口。 */
$("btn-usagelog").addEventListener("click", () => emit("open-usage"));
$("btn-tunnel").addEventListener("click", () => emit("open-tunnel"));

/* ---- 侧栏：会话列表 ---- */

/* 侧栏里新出现的空间（刚新建、别处新建后轮询到）底色闪一下，告诉你它落在了哪。
 * 登录后第一次拿到的列表只记下、不闪；退出登录后重新记。 */
const newCard = new ListMotion("flash", "card-flash", 1200);
let cardsSettled = false;

export function renderSidebar() {
  renderUserMenu();
  const list = $("session-list");
  const scrollTop = list.scrollTop;
  const focusedID = (document.activeElement as HTMLElement | null)?.closest<HTMLElement>(".session-card")?.dataset.sessionId;
  list.replaceChildren();
  const sessions = filterWorkspaces(S.sessions);
  const filtered = workspaceFilterActive();
  const count = $("session-count");
  const countText = filtered ? `${sessions.length}/${S.sessions.length}` : String(S.sessions.length);
  if (count.textContent !== countText) count.textContent = countText;
  setAttrRender(count, "aria-label", () => i18nText("显示 {shown} / {total} 个工作空间", { shown: String(sessions.length), total: String(S.sessions.length) }));
  $("session-filter-clear").classList.toggle("hidden", !filtered);
  $("session-search-open").classList.toggle("active", filtered);
  if (S.sessions.length && !sessions.length) {
    const p = document.createElement("p");
    p.className = "session-empty session-empty-text";
    setTextRender(p, () => i18nText("没有匹配的工作空间"));
    list.appendChild(p);
  }
  if (!S.sessions.length) {
    const p = document.createElement("p");
    p.className = "session-empty";
    p.innerHTML = `<span class="session-empty-icon" aria-hidden="true">—</span><span class="session-empty-text"><span data-i18n="还没有工作空间，点击上方新建。">${trHTML("还没有工作空间，点击上方新建。")}</span></span>`;
    setTip(p, () => i18nText("还没有工作空间，点击上方新建"));
    list.appendChild(p);
  }
  for (const sess of sessions) {
    const card = document.createElement("button");
    const active = S.view === "work" && S.current?.id === sess.id;
    card.type = "button";
    card.className = "session-card" + (active ? " active" : "");
    card.dataset.sessionId = sess.id;
    if (active) card.setAttribute("aria-current", "page");
    const state = sessionState(sess);
    card.classList.add("st-" + state.cls);
    setAttrRender(card, "aria-label", () => `${sess.name} (${agentName(sess.agent)}, ${sessionState(sess).label})`);
    setTip(card, () => `${sess.name}\n${sess.account_label} · ${agentName(sess.agent)} · ${sessionState(sess).label}\n#${sess.id}`);
    const av = agentAvatar(sess.agent, { icon: 18, led: true });
    if (sess.status === "running") av.querySelector(".led")!.classList.add("on");
    // 收起侧栏时只剩头像，同一种 Agent 的空间图标一模一样：改显示空间名首字
    const initial = document.createElement("span");
    initial.className = "sc-initial";
    initial.setAttribute("aria-hidden", "true");
    initial.textContent = Array.from(sess.name.trim())[0]?.toUpperCase() || "?";
    av.append(initial);
    const body = document.createElement("span");
    body.className = "sc-body";
    const h = document.createElement("span");
    h.className = "sc-name";
    h.textContent = sess.name;
    const meta = document.createElement("span");
    meta.className = "meta";
    meta.textContent = sess.account_label || agentName(sess.agent);
    body.append(h, meta);
    // 运行中 / 休眠写成文字，不只靠颜色区分。休眠 = 空闲自动停机
    // （数据都在，发消息/开终端即自动唤醒），与用户手动停止区分开。
    if (state.cls !== "off") {
      const st = document.createElement("span");
      st.className = "sc-state " + state.cls;
      setTextRender(st, () => sessionState(sess).label);
      meta.append(document.createTextNode(" · "), st);
    }
    // 行尾圆点同桌面版：运行中=绿、休眠=黄、已停止=灰；收起侧栏时改由头像角灯表示。
    const dot = document.createElement("span");
    dot.className = "sc-dot " + state.cls;
    dot.setAttribute("aria-hidden", "true");
    card.append(av, body, dot);
    const open = () => emit("open-session", sess);
    card.addEventListener("click", open);
    list.appendChild(card);
    if (cardsSettled) newCard.play(card, sess.id);
    if (focusedID === sess.id) card.focus({ preventScroll: true });
  }
  if (focusedID && !sessions.some(sess => sess.id === focusedID)) {
    const search = $("session-search");
    (search.getClientRects().length ? search : $("session-search-open")).focus({ preventScroll: true });
  }
  list.scrollTop = scrollTop;
}

bus.addEventListener("data-updated", () => {
  if (!cardsSettled) { newCard.settle(S.sessions.map((s) => s.id)); cardsSettled = true; }
  renderSidebar();
});
bus.addEventListener("signed-out", () => { newCard.reset(); cardsSettled = false; });
