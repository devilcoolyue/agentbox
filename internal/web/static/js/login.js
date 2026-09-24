/* login：账号密码登录 / 登出 / 进入主界面。
 * 登录成功后服务端签发会话令牌，存 localStorage；/me 返回角色，
 * 普通用户隐藏侧栏「系统设置」入口（服务端接口同样有管理员校验）。 */
"use strict";
import { S, bus, emit } from "./state.js";
import { $, btnBusy, btnDone } from "./util.js";
import { api } from "./api.js";
import { refreshAll, startPolling } from "./data.js";
import { startPing } from "./ping.js";
import { renderMyQuota } from "./quota.js";
import { buttonLabel } from "./icons.js";
function setPasswordVisible(visible) {
    $("login-pass").type = visible ? "text" : "password";
    const toggle = $("login-password-toggle");
    const label = visible ? "隐藏密码" : "显示密码";
    toggle.setAttribute("aria-label", label);
    toggle.title = label;
    buttonLabel(toggle, "", visible ? "eye" : "eye-off");
}
$("login-password-toggle").addEventListener("click", () => {
    setPasswordVisible($("login-pass").type === "password");
});
export function showLogin(err) {
    setPasswordVisible(false);
    S.token = "";
    localStorage.removeItem("agentbox_token");
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
    let me;
    try {
        me = await api("/me", { signal: AbortSignal.timeout(15000) });
    }
    catch (error) {
        // Non-401 failures previously disappeared, leaving stored-token startup
        // blank or a submitted login waiting without a useful retry path.
        if (token === S.token)
            showLogin("读取登录状态失败，请重试：" + error.message);
        return;
    }
    if (token !== S.token)
        return;
    S.user = me.user;
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
    $("login-pass").value = "";
    $("app").classList.remove("hidden");
    btnDone($("login-btn")); // Authentication is complete; data loading is separate.
    await refreshAll(AbortSignal.timeout(15000));
    if (token !== S.token)
        return;
    emit("app-ready");
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
            signal: AbortSignal.timeout(15000),
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
                username: $("login-user").value.trim(),
                password: $("login-pass").value,
            }),
        });
        if (!res.ok) {
            let msg = "账号或密码错误";
            if (res.status !== 401) {
                try {
                    msg = (await res.json()).error || msg;
                }
                catch (_) { }
            }
            showLogin(msg);
            return;
        }
        const data = (await res.json());
        S.token = data.token;
        localStorage.setItem("agentbox_token", S.token);
        await tryEnter();
    }
    catch (_) {
        showLogin("无法连接服务器，请稍后重试");
    }
    finally {
        btnDone($("login-btn"));
    }
});
$("btn-logout").addEventListener("click", async () => {
    try {
        await api("/logout", { method: "POST" });
    }
    catch (_) { }
    localStorage.removeItem("agentbox_token");
    location.reload();
});
