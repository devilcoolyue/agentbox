/* login：账号密码登录 / 登出 / 进入主界面。
 * 登录成功后服务端签发会话令牌，存 localStorage；/me 返回角色，
 * 普通用户隐藏侧栏「系统设置」入口（服务端接口同样有管理员校验）。 */
"use strict";

import { S, bus, emit } from "./state.js";
import { $, btnBusy, btnDone } from "./util.js";
import { api } from "./api.js";
import type { Me } from "./types.js";
import { refreshAll, startPolling } from "./data.js";
import { startPing } from "./ping.js";
import { renderMyQuota } from "./quota.js";

export function showLogin(err?: string) {
  $("app").classList.add("hidden");
  $("login").classList.remove("hidden");
  if (err) {
    $("login-error").textContent = err;
    $("login-error").classList.remove("hidden");
  }
}

export async function tryEnter() {
  let me: Me;
  try {
    me = await api<Me>("/me");
  } catch (_) {
    return; // unauthorized 事件已触发 showLogin
  }
  S.user = me.user;
  S.role = me.role;
  S.models = me.models || null;
  S.termTips = me.terminal_tips || null;
  S.quota = me.quota || null;
  renderMyQuota();
  emit("tips-updated"); // 让 term.js 用下发的提示语初始化顶栏轮播
  $("btn-settings").classList.toggle("hidden", me.role !== "admin");
  $("login").classList.add("hidden");
  $("app").classList.remove("hidden");
  await refreshAll();
  startPolling();
  startPing();
}

bus.addEventListener("unauthorized", () => showLogin("登录已过期，请重新登录"));

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("login-error").classList.add("hidden");
  btnBusy($("login-btn"), "登录中…");
  try {
    const res = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: $<HTMLInputElement>("login-user").value.trim(),
        password: $<HTMLInputElement>("login-pass").value,
      }),
    });
    if (!res.ok) {
      let msg = "账号或密码错误";
      if (res.status !== 401) {
        try { msg = ((await res.json()) as { error?: string }).error || msg; } catch (_) {}
      }
      showLogin(msg);
      return;
    }
    const data = (await res.json()) as { token: string };
    S.token = data.token;
    localStorage.setItem("agentbox_token", S.token);
    await tryEnter();
  } catch (_) {
    showLogin("无法连接服务器，请稍后重试");
  } finally {
    btnDone($("login-btn"));
  }
});

$("btn-logout").addEventListener("click", async () => {
  try { await api("/logout", { method: "POST" }); } catch (_) {}
  localStorage.removeItem("agentbox_token");
  location.reload();
});
