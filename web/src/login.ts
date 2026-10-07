/* login：账号密码登录 / 登出 / 进入主界面。
 * 登录成功后服务端签发会话令牌，存 localStorage；/me 返回角色，
 * 普通用户隐藏侧栏「系统设置」入口（服务端接口同样有管理员校验）。 */
import { t as i18nText } from "./i18n.js";
"use strict";

import { S, bus, emit } from "./state.js";
import { $, btnBusy, btnDone } from "./util.js";
import { responseError } from "./problems.js";
import { api } from "./api.js";
import type { Me } from "./types.js";
import { refreshAll, startPolling } from "./data.js";
import { startPing } from "./ping.js";
import { renderMyQuota } from "./quota.js";
import { buttonLabel } from "./icons.js";
import {clearPreviousChatIdentity,rememberChatIdentity} from "./features/chat/local-identity.js";

function setPasswordVisible(visible: boolean) {
  $<HTMLInputElement>("login-pass").type = visible ? "text" : "password";
  const toggle = $("login-password-toggle");
  const label = visible ? i18nText("隐藏密码") : i18nText("显示密码");
  toggle.setAttribute("aria-label", label);
  toggle.dataset.tip = label;
  buttonLabel(toggle, "", visible ? "eye-off" : "eye");
}

$("login-password-toggle").addEventListener("click", () => {
  setPasswordVisible($<HTMLInputElement>("login-pass").type === "password");
});

export function showLogin(err?: string, keepStoredToken=false) {
  setPasswordVisible(false);
  // A delayed 401/storage event from an old page must neither remove a newer
  // token nor clear drafts/outgoing copies saved by that new login.
  const stored=localStorage.getItem("agentbox_token");
  if(stored&&stored!==S.token)keepStoredToken=true;
  S.preserveChatCopiesOnSignout=keepStoredToken;
  S.token = "";
  if(!keepStoredToken){localStorage.removeItem("agentbox_token");clearPreviousChatIdentity(localStorage,sessionStorage);}
  emit("signed-out");
  $("app").classList.add("hidden");
  $("login").classList.remove("hidden");
  if (err) {
    $("login-error").textContent = err;
    $("login-error").classList.remove("hidden");
  }
}

export async function tryEnter() {
  const token = S.token;
  let me: Me;
  try {
    me = await api<Me>("/me", { signal: AbortSignal.timeout(15000) });
  } catch (error) {
    // Non-401 failures previously disappeared, leaving stored-token startup
    // blank or a submitted login waiting without a useful retry path.
    if (token === S.token) showLogin(i18nText("读取登录状态失败，请重试：") + (error as Error).message);
    return;
  }
  if (token !== S.token || localStorage.getItem("agentbox_token")!==token) return;
  S.user = me.user;
  S.draftScope=me.draft_protocol===1&&/^[a-f0-9]{64}$/.test(me.draft_scope||"")?me.draft_scope!:"";
  S.draftProtocol=me.draft_protocol===1?1:0;
  S.chatScope=me.chat_protocol===1&&/^[a-f0-9]{64}$/.test(me.chat_scope||"")?me.chat_scope!:"";
  S.chatProtocol=me.chat_protocol===1?1:me.chat_protocol===undefined||me.chat_protocol===0?0:-1;
  S.preserveChatCopiesOnSignout=false;
  rememberChatIdentity(localStorage,S.draftScope,me.chat_scope||"");
  S.role = me.role;
  S.models = me.models || null;
  S.termTips = me.terminal_tips || null;
  S.timeZone = me.timezone || "Asia/Shanghai";
  S.quota = me.quota || null;
  // Features initialized by this event need the authenticated role and timezone.
  emit("signed-in");
  renderMyQuota();
  emit("tips-updated"); // 让 term.js 用下发的提示语初始化顶栏轮播
  emit("timezone-updated");
  $("btn-settings").classList.toggle("hidden", me.role !== "admin");
  $("login").classList.add("hidden");
  setPasswordVisible(false);
  $<HTMLInputElement>("login-pass").value = "";
  $("app").classList.remove("hidden");
  btnDone($("login-btn")); // Authentication is complete; data loading is separate.
  await refreshAll(AbortSignal.timeout(15000));
  if (token !== S.token) return;
  emit("app-ready");
  startPolling();
  startPing();
}

bus.addEventListener("unauthorized", event => {
  const message = (event as CustomEvent<unknown>).detail;
  showLogin(typeof message === "string" ? message : i18nText("登录已过期，请重新登录"));
});

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("login-error").classList.add("hidden");
  btnBusy($("login-btn"), () => i18nText("登录中…"));
  try {
    const res = await fetch("/api/login", {
      method: "POST",
      signal: AbortSignal.timeout(15000),
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: $<HTMLInputElement>("login-user").value.trim(),
        password: $<HTMLInputElement>("login-pass").value,
      }),
    });
    if (!res.ok) {
      const error = await responseError(res, i18nText, i18nText("账号或密码错误"));
      showLogin(error.message);
      return;
    }
    const data = (await res.json()) as { token: string };
    S.token = data.token;
    localStorage.setItem("agentbox_token", S.token);
    clearPreviousChatIdentity(localStorage,sessionStorage);
    await tryEnter();
  } catch (_) {
    showLogin(i18nText("无法连接服务器，请稍后重试"));
  } finally {
    btnDone($("login-btn"));
  }
});

$("btn-logout").addEventListener("click", async () => {
  try { await api("/logout", { method: "POST" }); } catch (_) {}
  showLogin();
  location.reload();
});

window.addEventListener("storage",event=>{
  if(event.key==="agentbox_token"&&S.token&&event.newValue!==S.token)showLogin(i18nText("其他页面的登录状态已变化，请重新登录。"),true);
});

window.addEventListener("agentbox-language-change", () => setPasswordVisible($<HTMLInputElement>("login-pass").type === "text"));
