/* shell：应用外壳 —— 抽屉、主区视图切换（工作台/设置）、顶栏标题、侧栏渲染。
 * 点击会话卡片 / 系统设置入口通过 bus 广播，由 sessions.js / settings.js 接管，
 * 保持 shell 不反向依赖任何功能模块。 */
"use strict";

import { S, bus, emit } from "./state.js";
import type { View } from "./state.js";
import { $ } from "./util.js";
import { agentIcon, agentAvatar, agentName } from "./brand.js";

/* ---- 抽屉（仅窄屏可见） ---- */

export function openDrawer() {
  $("sidebar").classList.add("open");
  $("scrim").classList.add("show");
}
export function closeDrawer() {
  $("sidebar").classList.remove("open");
  $("scrim").classList.remove("show");
}
$("btn-menu").addEventListener("click", openDrawer);
$("scrim").addEventListener("click", closeDrawer);
window.addEventListener("keydown", (e) => { if (e.key === "Escape") closeDrawer(); });

/* ---- 顶栏：设置视图显示标题，工作台视图显示 状态灯+会话名+⋯菜单 ---- */

const VIEW_TITLE: Record<string, string> = { settings: "系统设置", usage: "使用记录" };

export function updateTopbarTitle() {
  const inWork = S.view === "work" && !!S.current;
  $("topbar-title").textContent = VIEW_TITLE[S.view] || (S.current ? S.current.name : "");
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
  if (S.view === name) { closeDrawer(); updateTopbarTitle(); return; }
  S.view = name;
  $("view-work").classList.toggle("hidden", name !== "work");
  $("view-settings").classList.toggle("hidden", name !== "settings");
  $("view-usage").classList.toggle("hidden", name !== "usage");
  $("btn-settings").classList.toggle("active", name === "settings");
  $("btn-usagelog").classList.toggle("active", name === "usage");
  updateTopbarTitle();
  closeDrawer();
  renderSidebar();
}

$("btn-settings").addEventListener("click", () => emit("open-settings"));
/* btn-usagelog 而非 btn-usage：后者是工作台头部的「额度」按钮，见 index.html 注释。 */
$("btn-usagelog").addEventListener("click", () => emit("open-usage"));

/* ---- 侧栏：会话列表 ---- */

export function renderSidebar() {
  const list = $("session-list");
  list.replaceChildren();
  if (!S.sessions.length) {
    const p = document.createElement("p");
    p.className = "files-empty";
    p.textContent = "还没有会话。";
    list.appendChild(p);
  }
  for (const sess of S.sessions) {
    const card = document.createElement("div");
    card.className = "session-card" +
      (S.view === "work" && S.current && S.current.id === sess.id ? " active" : "");
    card.setAttribute("role", "button");
    card.tabIndex = 0;
    card.setAttribute("aria-label", `${sess.name}（${agentName(sess.agent)}）`);
    const av = agentAvatar(sess.agent, { led: true });
    av.title = agentName(sess.agent);
    if (sess.status === "running") av.querySelector(".led")!.classList.add("on");
    const body = document.createElement("div");
    body.className = "sc-body";
    const h = document.createElement("h3");
    h.textContent = sess.name;
    const meta = document.createElement("div");
    meta.className = "meta";
    meta.textContent = `${sess.account_label} · #${sess.id}`;
    body.append(h, meta);
    // 休眠 = 空闲自动停机（数据都在，发消息/开终端即自动唤醒）。与用户手动
    // 停止区分开，否则回来发现会话没了会以为服务出了故障。
    if (sess.stop_reason === "idle" && sess.status !== "running") {
      const zzz = document.createElement("span");
      zzz.className = "sc-sleep";
      zzz.textContent = "休眠";
      zzz.title = "空闲自动停机，发消息或打开终端会自动唤醒";
      meta.append(document.createTextNode(" · "), zzz);
    }
    card.append(av, body);
    const open = () => emit("open-session", sess);
    card.addEventListener("click", open);
    card.addEventListener("keydown", (e) => { if (e.key === "Enter") open(); });
    list.appendChild(card);
  }
}

bus.addEventListener("data-updated", renderSidebar);
