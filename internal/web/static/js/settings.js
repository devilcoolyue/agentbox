/* settings：系统设置视图 —— 账号池维护（含 OAuth / API Key 登录弹窗）、
 * 容器与资源、模型管理、安全与访问、关于。 */
"use strict";
import { setSelectValue } from "./select.js";
import { S, bus, emit } from "./state.js";
import { $, btnBusy, btnDone, toast, fmtTime, fmtUptime, fmtBytes, askConfirm, askPrompt } from "./util.js";
import { api } from "./api.js";
import { refreshAll } from "./data.js";
import { showView } from "./shell.js";
import { MODEL_ID_RE } from "./chat.js";
import { agentKey, agentName, agentIcon, decorateAgentOpts } from "./brand.js";
import { quotaChip, openQuota } from "./quota.js";
import { loadProxies, mountProxyPicker, openProxiesSection, refreshProxyCount } from "./proxies.js";
import { openPricingSection, refreshPriceCount } from "./pricing.js";
import { setTip } from "./tip.js";
import { accountAccessButton } from "./account-access.js";
/* 静态标识装饰：添加账号弹窗的类型选择卡、模型管理卡片标题 */
decorateAgentOpts($("acct-form"));
for (const h of document.querySelectorAll("h3[data-agent]")) {
    h.classList.add("agent-h3", "agent-" + agentKey(h.dataset.agent));
    h.prepend(agentIcon(h.dataset.agent, 15));
}
/* ---------------- 视图入口与二级导航 ---------------- */
export async function openSettingsView() {
    if (S.role !== "admin")
        return; // 普通用户无入口，这里再兜一道
    showView("settings");
    setSec(S.sec || "accounts");
    renderSettingsAccounts();
    // 停在别的分区时也要把 IP 代理的条数拉出来：左侧计数是给管理员扫一眼用的，
    // 停在 0 上等人点开就是假信息。分区本身进来时自己会拉。
    if (S.sec !== "proxies")
        refreshProxyCount();
    try {
        S.settings = await api("/settings");
        fillSettingsForms();
    }
    catch (e) {
        toast("读取设置失败：" + e.message, true);
    }
}
const SET_SECS = ["accounts", "proxies", "container", "models", "pricing", "interface", "security", "monitor", "about"];
function setSec(name) {
    S.sec = name;
    for (const b of document.querySelectorAll("#set-nav button")) {
        b.classList.toggle("active", b.dataset.sec === name);
    }
    for (const sec of SET_SECS) {
        $("sec-" + sec).classList.toggle("hidden", sec !== name);
    }
    $("set-content").scrollTop = 0;
    stopMonitor(); // 离开监控页就停轮询，任何切换都先关掉
    if (name === "about")
        loadSystem();
    if (name === "security")
        loadUsers();
    if (name === "monitor")
        startMonitor();
    if (name === "proxies")
        openProxiesSection();
    if (name === "pricing")
        openPricingSection();
}
$("set-nav").addEventListener("click", (e) => {
    const btn = e.target.closest("button[data-sec]");
    if (btn)
        setSec(btn.dataset.sec);
});
bus.addEventListener("open-settings", openSettingsView);
bus.addEventListener("data-updated", () => {
    if (S.view === "settings")
        renderSettingsAccounts();
});
/* ---------------- 账号池 ---------------- */
function acctStateSeg(a) {
    // 返回 [状态class, 状态文案, 附加说明]
    const via = a.base_url ? a.base_url.replace(/^https?:\/\//, "") : "官方接口";
    if (a.type === "claude") {
        if (a.auth_mode === "apikey")
            return ["ok", "Key 已配置", via];
        if (a.cred_status === "ok") {
            return ["ok", "凭证正常", a.expires_at ? "令牌 " + fmtTime(a.expires_at) + " 到期（自动续期）" : ""];
        }
        if (a.cred_status === "norefresh")
            return ["warn", "需重新登录", "refresh token 已失效"];
        return ["", "未登录", "凭证目录已就绪，等待授权"];
    }
    if (a.cred_status === "ok")
        return ["ok", "Key 已配置", via];
    return ["", "未配置 Key", a.base_url ? via : ""];
}
export function renderSettingsAccounts() {
    $("set-acct-count").textContent = String(S.accounts.length);
    const box = $("acct-list-box");
    box.replaceChildren();
    if (!S.accounts.length) {
        const p = document.createElement("p");
        p.className = "acct-empty";
        p.textContent = "账号池为空。点击右上角「添加账号」创建第一个账号。";
        box.appendChild(p);
        return;
    }
    for (const a of S.accounts)
        box.appendChild(acctRow(a));
}
function acctRow(a) {
    const [stCls, stText, stExtra] = acctStateSeg(a);
    const row = document.createElement("div");
    row.className = "acct-row" + (a.cred_status === "norefresh" ? " stale" : "");
    const idBox = document.createElement("div");
    idBox.className = "acct-id";
    const type = document.createElement("span");
    type.className = "acct-type agent-" + agentKey(a.type);
    type.append(agentIcon(a.type, 12), document.createTextNode(a.type === "codex" ? "Codex" : "Claude"));
    const name = document.createElement("span");
    name.className = "acct-name";
    name.textContent = a.label;
    const slug = document.createElement("span");
    slug.className = "acct-slug";
    slug.textContent = a.id;
    idBox.append(type, name, slug);
    const state = document.createElement("div");
    state.className = "acct-state";
    const st = document.createElement("span");
    st.className = "st" + (stCls ? " " + stCls : "");
    const dot = document.createElement("span");
    dot.className = "dot";
    st.append(dot, document.createTextNode(stText));
    state.appendChild(st);
    if (stExtra)
        state.appendChild(Object.assign(document.createElement("span"), { textContent: stExtra }));
    state.appendChild(Object.assign(document.createElement("span"), {
        textContent: a.sessions > 0 ? a.sessions + " 个工作空间在用" : "暂无工作空间使用",
    }));
    // 出口 IP 直接标在账号行上：它决定官方那边看到的是谁，排查封号时第一眼要看的
    // 就是这个，藏进编辑弹窗里等于没有。
    const px = document.createElement("span");
    px.className = "acct-proxy" + (a.proxy_id ? "" : " none");
    px.textContent = a.proxy_id ? "⇄ " + (a.proxy_label || a.proxy_id) : "⇄ 直连";
    setTip(px, a.proxy_id ? "该账号的请求经此代理出网" : "该账号的请求从服务器自身 IP 发出");
    state.appendChild(px);
    const acts = document.createElement("div");
    acts.className = "acct-actions";
    const auth = document.createElement("button");
    const needAuth = a.cred_status !== "ok";
    auth.className = "btn btn-sm" + (needAuth ? " btn-primary" : "");
    auth.textContent = a.type === "claude"
        ? (a.auth_mode === "apikey" ? "更换 Key" : (a.cred_status === "missing" ? "登录" : "重新授权"))
        : (a.cred_status === "ok" ? "更换 Key" : "配置 Key");
    auth.addEventListener("click", () => openAuthDlg(a));
    const edit = document.createElement("button");
    edit.className = "btn btn-sm btn-ghost";
    edit.textContent = "编辑";
    edit.addEventListener("click", () => openAcctEdit(a));
    const del = document.createElement("button");
    del.className = "btn btn-sm btn-danger";
    del.textContent = "删除";
    if (a.sessions > 0) {
        del.disabled = true;
        setTip(del, "有工作空间在用，请先删除对应工作空间");
    }
    else {
        del.addEventListener("click", () => openAcctDel(a));
    }
    acts.append(auth, edit, accountAccessButton(a), del);
    row.append(idBox, state, acts);
    return row;
}
/* 环境变量 <-> 文本（每行 KEY=VALUE） */
function envToText(env) {
    return Object.entries(env || {}).map(([k, v]) => k + "=" + v).join("\n");
}
function parseEnvText(text) {
    const env = {};
    for (const line of String(text).split("\n")) {
        const t = line.trim();
        if (!t)
            continue;
        const i = t.indexOf("=");
        const key = i > 0 ? t.slice(0, i).trim() : "";
        if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
            throw new Error("环境变量格式错误：「" + t + "」应为 KEY=VALUE");
        }
        env[key] = t.slice(i + 1);
    }
    return env;
}
/* ---- 添加账号 ---- */
$("btn-acct-add").addEventListener("click", () => {
    $("acct-form").reset();
    $("acct-error").classList.add("hidden");
    $("dlg-acct").showModal();
});
$("acct-cancel").addEventListener("click", () => $("dlg-acct").close());
$("acct-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const errEl = $("acct-error");
    errEl.classList.add("hidden");
    let env;
    try {
        env = parseEnvText($("acct-env").value);
    }
    catch (err) {
        errEl.textContent = err.message;
        errEl.classList.remove("hidden");
        return;
    }
    const body = {
        id: $("acct-id").value.trim(),
        type: document.querySelector('#acct-form input[name="atype"]:checked').value,
        label: $("acct-label").value.trim(),
        env,
    };
    btnBusy($("acct-ok"), "创建中…");
    $("acct-cancel").disabled = true;
    try {
        const acct = await api("/accounts", { method: "POST", body: JSON.stringify(body) });
        $("dlg-acct").close();
        toast("账号 " + acct.label + " 已创建");
        await refreshAll();
        openAuthDlg(acct); // 创建后直接进入登录流程
    }
    catch (err) {
        errEl.textContent = err.message;
        errEl.classList.remove("hidden");
    }
    finally {
        btnDone($("acct-ok"));
        $("acct-cancel").disabled = false;
    }
});
/* ---- 编辑账号 ---- */
let editAcct = null;
let acctPicker = null;
async function openAcctEdit(a) {
    editAcct = a;
    $("acct-edit-title").textContent = "编辑账号 · " + a.id;
    $("acct-edit-label").value = a.label;
    $("acct-edit-env").value = envToText(a.env);
    $("acct-edit-error").classList.add("hidden");
    if (!acctPicker)
        acctPicker = mountProxyPicker($("acct-edit-proxy"));
    acctPicker.set(a.proxy_id || "");
    $("dlg-acct-edit").showModal();
    // 代理列表后到也不挡开弹窗：选择器先用当前缓存画，拉到新列表再刷一遍选中项
    // 的文案（否则刚在 IP 代理页加的代理，这里要等下次开弹窗才看得见）。
    try {
        await loadProxies();
        acctPicker.set(a.proxy_id || "");
    }
    catch (_) { /* 列表拉不到就只能选「无代理」，不影响改名字/环境变量 */ }
}
$("acct-edit-cancel").addEventListener("click", () => $("dlg-acct-edit").close());
$("acct-edit-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!editAcct)
        return;
    const errEl = $("acct-edit-error");
    errEl.classList.add("hidden");
    let env;
    try {
        env = parseEnvText($("acct-edit-env").value);
    }
    catch (err) {
        errEl.textContent = err.message;
        errEl.classList.remove("hidden");
        return;
    }
    btnBusy($("acct-edit-ok"), "保存中…");
    try {
        await api("/accounts/" + editAcct.id, {
            method: "PATCH",
            body: JSON.stringify({
                label: $("acct-edit-label").value,
                env,
                proxy_id: acctPicker ? acctPicker.get() : "",
            }),
        });
        $("dlg-acct-edit").close();
        toast("账号已更新");
        refreshAll();
    }
    catch (err) {
        errEl.textContent = err.message;
        errEl.classList.remove("hidden");
    }
    finally {
        btnDone($("acct-edit-ok"));
    }
});
/* ---- 删除账号 ---- */
let delAcct = null;
function openAcctDel(a) {
    delAcct = a;
    $("acct-del-text").textContent = "确认从账号池删除「" + a.label + "」（" + a.id + "）？";
    $("dlg-acct-del").showModal();
}
$("acct-del-cancel").addEventListener("click", () => $("dlg-acct-del").close());
$("acct-del-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!delAcct)
        return;
    btnBusy($("acct-del-ok"), "删除中…");
    try {
        await api("/accounts/" + delAcct.id, { method: "DELETE" });
        $("dlg-acct-del").close();
        toast("账号已删除，凭证目录保留在磁盘上");
        refreshAll();
    }
    catch (err) {
        toast("删除失败：" + err.message, true);
    }
    finally {
        btnDone($("acct-del-ok"));
    }
});
/* ---- 账号登录 / 配置弹窗（claude OAuth / codex API Key） ---- */
let authAcct = null;
function authMsg(text, isErr) {
    const m = $("auth-msg");
    m.textContent = text || "";
    m.classList.toggle("hidden", !text);
    m.classList.toggle("err", !!isErr);
}
export function openAuthDlg(a) {
    authAcct = a;
    const isClaude = a.type === "claude";
    $("auth-title").textContent = a.label + (isClaude ? " · 账号认证" : " · API Key");
    $("auth-modes").classList.toggle("hidden", !isClaude);
    $("auth-linkrow").classList.add("hidden");
    $("auth-code").classList.add("hidden");
    $("auth-finish").classList.add("hidden");
    $("auth-code").value = "";
    $("auth-apikey").value = "";
    $("auth-baseurl").value = a.base_url || "";
    setSelectValue($("auth-wire"), a.wire_api === "chat" ? "chat" : "responses");
    $("auth-wire").classList.toggle("hidden", isClaude); // wire_api 仅 codex 有意义
    $("auth-clearkey").classList.toggle("hidden", !(isClaude && a.auth_mode === "apikey"));
    $("auth-key-hint").textContent = isClaude
        ? "中转站 API Key + Base URL，保存进账号环境变量（ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN），对新对话和新开终端立即生效，优先于订阅凭证。Base URL 留空 = 官方 api.anthropic.com。"
        : "中转站或官方 API Key + Base URL。保存后写入账号池 auth.json 与 config.toml（base_url），容器下次拉起生效。Base URL 留空 = 官方接口/保持现有 config.toml 不动。";
    renderAuthModels(null);
    authMsg("");
    setAuthMode(isClaude && a.auth_mode !== "apikey" ? "oauth" : "key");
    $("dlg-auth").showModal();
}
function setAuthMode(mode) {
    const isClaude = authAcct && authAcct.type === "claude";
    $("auth-oauth").classList.toggle("hidden", !(isClaude && mode === "oauth"));
    $("auth-key").classList.toggle("hidden", mode !== "key");
    $("auth-mode-oauth").classList.toggle("active", mode === "oauth");
    $("auth-mode-key").classList.toggle("active", mode === "key");
}
$("auth-mode-oauth").addEventListener("click", () => setAuthMode("oauth"));
$("auth-mode-key").addEventListener("click", () => setAuthMode("key"));
$("auth-gen").addEventListener("click", async () => {
    if (!authAcct)
        return;
    btnBusy($("auth-gen"), "生成中…");
    try {
        const { url } = await api(`/accounts/${authAcct.id}/oauth/start`, { method: "POST" });
        const link = $("auth-link");
        link.href = url;
        link.textContent = url;
        $("auth-linkrow").classList.remove("hidden");
        $("auth-code").classList.remove("hidden");
        $("auth-finish").classList.remove("hidden");
        authMsg("在浏览器完成授权后，把回显的授权码粘贴到下方输入框");
    }
    catch (e) {
        authMsg("生成失败：" + e.message, true);
    }
    btnDone($("auth-gen"));
});
$("auth-copy").addEventListener("click", async () => {
    try {
        await navigator.clipboard.writeText($("auth-link").href);
        authMsg("链接已复制");
    }
    catch (_) {
        authMsg("复制失败，请手动选中链接复制", true);
    }
});
$("auth-finish").addEventListener("click", async () => {
    if (!authAcct)
        return;
    const code = $("auth-code").value.trim();
    if (!code) {
        authMsg("请先粘贴授权码", true);
        return;
    }
    btnBusy($("auth-finish"), "换取令牌…");
    try {
        const res = await api(`/accounts/${authAcct.id}/oauth/finish`, {
            method: "POST", body: JSON.stringify({ code }),
        });
        authMsg(`登录成功${res.subscription_type ? "（" + res.subscription_type + " 订阅）" : ""}，工作空间将在下次对话或拉起时使用新凭证`);
        refreshAll();
    }
    catch (e) {
        authMsg("登录失败：" + e.message, true);
    }
    btnDone($("auth-finish"));
});
$("auth-savekey").addEventListener("click", async () => {
    if (!authAcct)
        return;
    const key = $("auth-apikey").value.trim();
    const base = $("auth-baseurl").value.trim();
    if (!key) {
        authMsg("请输入 API Key", true);
        return;
    }
    btnBusy($("auth-savekey"), "保存中…");
    try {
        await api(`/accounts/${authAcct.id}/apikey`, {
            method: "POST",
            body: JSON.stringify({ api_key: key, base_url: base, wire_api: $("auth-wire").value }),
        });
        if (authAcct.type === "claude") {
            authMsg(base
                ? "已保存到账号环境变量，对新对话和新开终端立即生效"
                : "已保存（走官方 api.anthropic.com），对新对话和新开终端立即生效");
            $("auth-clearkey").classList.remove("hidden");
        }
        else {
            authMsg(base
                ? "已保存（auth.json + config.toml），容器下次拉起生效"
                : "已保存（auth.json，config.toml 未改动），容器下次拉起生效");
        }
        $("auth-apikey").value = "";
        refreshAll();
    }
    catch (e) {
        authMsg("保存失败：" + e.message, true);
    }
    btnDone($("auth-savekey"));
});
/* 测试连接 = 拉一次 /v1/models：通就显示延迟 + 模型列表，点模型直接入库 */
$("auth-test").addEventListener("click", async () => {
    if (!authAcct)
        return;
    btnBusy($("auth-test"), "探测中…");
    try {
        const res = await api(`/accounts/${authAcct.id}/apikey/test`, {
            method: "POST",
            body: JSON.stringify({
                api_key: $("auth-apikey").value.trim(),
                base_url: $("auth-baseurl").value.trim(),
            }),
        });
        renderAuthModels(res.models);
        authMsg(`连接正常 · ${res.latency_ms}ms · ${res.endpoint} · ${res.models.length} 个模型`
            + (res.models.length ? "，点击模型可加入下拉菜单" : ""));
    }
    catch (e) {
        renderAuthModels(null);
        authMsg("连接失败：" + e.message, true);
    }
    btnDone($("auth-test"));
});
function renderAuthModels(models) {
    const box = $("auth-models");
    box.replaceChildren();
    box.classList.toggle("hidden", !models || !models.length);
    if (!models)
        return;
    const agent = authAcct.type; // chips 加进当前账号类型对应的模型列表
    const have = new Set((((S.settings && S.settings.models) || {})[agent] || []).map((m) => m.id));
    for (const id of models) {
        const chip = document.createElement("button");
        chip.type = "button";
        chip.className = "auth-model-chip mono" + (have.has(id) ? " in" : "");
        chip.textContent = id;
        setTip(chip, have.has(id) ? "已在模型列表中" : "点击加入模型列表");
        chip.addEventListener("click", () => {
            const models = (S.settings && S.settings.models) || {};
            if ((models[agent] || []).some((x) => x.id === id)) {
                toast("模型 " + id + " 已在列表中");
                return;
            }
            const next = { ...models, [agent]: [...(models[agent] || []), { id, label: id }] };
            putSettings({ models: next }, chip, "已添加 " + id).then((ok) => {
                if (ok) {
                    chip.classList.add("in");
                    setTip(chip, "已在模型列表中");
                }
            });
        });
        box.appendChild(chip);
    }
}
$("auth-clearkey").addEventListener("click", async () => {
    if (!authAcct)
        return;
    btnBusy($("auth-clearkey"), "清除中…");
    try {
        await api(`/accounts/${authAcct.id}/apikey`, { method: "DELETE" });
        authMsg("已清除中转站配置，该账号切回订阅 OAuth 凭证");
        $("auth-baseurl").value = "";
        $("auth-clearkey").classList.add("hidden");
        renderAuthModels(null);
        refreshAll();
    }
    catch (e) {
        authMsg("清除失败：" + e.message, true);
    }
    btnDone($("auth-clearkey"));
});
$("auth-close").addEventListener("click", () => $("dlg-auth").close());
/* ---------------- 容器与资源 / 安全与访问 表单 ---------------- */
function fillSettingsForms() {
    const st = S.settings;
    if (!st)
        return;
    $("set-image").value = st.agent_image;
    $("set-mem").value = String(st.container.memory_mb);
    $("set-cpus").value = String(st.container.cpus);
    $("set-pids").value = String(st.container.pids_limit);
    const net = $("set-net");
    if (![...net.options].some((o) => o.value === st.container.network)) {
        // 配置里出现了自定义 docker 网络名，补一个选项而不是悄悄丢掉
        net.appendChild(new Option(st.container.network + " — 自定义网络", st.container.network));
    }
    setSelectValue(net, st.container.network);
    $("set-idle").value = String(st.idle_timeout_min);
    const tz = $("set-timezone");
    if (![...tz.options].some((o) => o.value === st.timezone)) {
        tz.appendChild(new Option(st.timezone + "（自定义）", st.timezone));
    }
    setSelectValue(tz, st.timezone);
    setSelectValue($("set-perm"), st.permission_mode);
    $("set-upload").value = String(st.max_upload_mb);
    $("set-listen").value = st.listen;
    $("security-note").textContent = st.restart_required
        ? "监听地址已修改，重启 agentbox 服务后生效"
        : "权限模式与上传上限即时生效；监听地址需重启 agentbox 服务";
    $("set-tunnel-on").checked = !!(st.tunnel && st.tunnel.enabled);
    $("set-tunnel-bind").value = (st.tunnel && st.tunnel.proxy_bind) || "";
    $("set-tunnel-host").value = (st.tunnel && st.tunnel.proxy_host) || "";
    $("set-bridge-bind").value = (st.proxy_bridge && st.proxy_bridge.bind) || "";
    $("set-bridge-host").value = (st.proxy_bridge && st.proxy_bridge.host) || "";
    $("tunnel-note").textContent = st.tunnel && st.tunnel.enabled
        ? (st.tunnel_active ? "隧道已启用，SOCKS5 代理监听中" : "隧道已启用，但代理未在监听（检查绑定地址）")
        : "默认绑定 docker 网桥网关 172.17.0.1，仅容器与本机可达";
    const tips = st.terminal_tips || {};
    $("set-tips").value = (tips.tips || []).join("\n");
    $("set-tips-interval").value = String(tips.interval_sec != null ? tips.interval_sec : 4);
    setSelectValue($("set-tips-anim"), tips.animation || "scroll");
    renderModels();
    refreshPriceCount();
}
export async function putSettings(patch, btn, okMsg) {
    if (btn)
        btnBusy(btn, "保存中…");
    try {
        S.settings = await api("/settings", { method: "PUT", body: JSON.stringify(patch) });
        fillSettingsForms();
        if (S.settings.models)
            S.models = S.settings.models; // 对话框的模型菜单同步更新
        if (S.settings.terminal_tips) { // 终端顶栏轮播实时刷新
            S.termTips = S.settings.terminal_tips;
            emit("tips-updated");
        }
        if (S.settings.timezone && S.settings.timezone !== S.timeZone) {
            S.timeZone = S.settings.timezone;
            emit("timezone-updated");
        }
        toast(okMsg || "已保存");
        return true;
    }
    catch (e) {
        toast("保存失败：" + e.message, true);
        return false;
    }
    finally {
        if (btn)
            btnDone(btn);
    }
}
$("btn-save-container").addEventListener("click", () => {
    putSettings({
        agent_image: $("set-image").value.trim(),
        container: {
            memory_mb: Number($("set-mem").value),
            cpus: Number($("set-cpus").value),
            pids_limit: Number($("set-pids").value),
            network: $("set-net").value,
        },
    }, $("btn-save-container"), "已保存，对新启动的容器生效");
});
$("btn-save-idle").addEventListener("click", () => {
    putSettings({
        idle_timeout_min: Number($("set-idle").value),
    }, $("btn-save-idle"), "空闲停机设置已保存并即时生效");
});
$("btn-save-timezone").addEventListener("click", () => {
    putSettings({
        timezone: $("set-timezone").value,
    }, $("btn-save-timezone"), "界面时区已保存并即时生效");
});
$("btn-save-tips").addEventListener("click", () => {
    const tips = $("set-tips").value.split("\n").map((t) => t.trim()).filter(Boolean);
    putSettings({
        terminal_tips: {
            tips,
            interval_sec: Number($("set-tips-interval").value) || 0,
            animation: $("set-tips-anim").value,
        },
    }, $("btn-save-tips"), "终端提示已保存并即时生效");
});
$("btn-save-tunnel").addEventListener("click", async () => {
    const ok = await putSettings({
        tunnel: {
            enabled: $("set-tunnel-on").checked,
            proxy_bind: $("set-tunnel-bind").value.trim(),
            proxy_host: $("set-tunnel-host").value.trim(),
        },
    }, $("btn-save-tunnel"), "隧道设置已保存并生效");
    if (ok && S.settings.tunnel_error) {
        toast("隧道启动失败：" + S.settings.tunnel_error, true);
    }
});
$("btn-save-bridge").addEventListener("click", async () => {
    const ok = await putSettings({
        proxy_bridge: {
            bind: $("set-bridge-bind").value.trim(),
            host: $("set-bridge-host").value.trim(),
        },
    }, $("btn-save-bridge"), "桥接地址已保存并重新绑定");
    if (ok)
        openProxiesSection(); // 重绑结果（成功/失败）由列表接口回报
});
$("btn-save-security").addEventListener("click", async () => {
    const ok = await putSettings({
        permission_mode: $("set-perm").value,
        max_upload_mb: Number($("set-upload").value),
        listen: $("set-listen").value.trim(),
    }, $("btn-save-security"));
    if (ok && S.settings.restart_required) {
        toast("已保存；监听地址改动需重启 agentbox 服务后生效");
    }
});
/* ---------------- 模型管理 ---------------- */
function renderModels() {
    const models = (S.settings && S.settings.models) || {};
    for (const agent of ["claude", "codex"]) {
        const box = document.querySelector(`.model-rows[data-agent="${agent}"]`);
        if (!box)
            continue;
        box.replaceChildren();
        const selected = S.settings?.default_models[agent] || "";
        const picker = $(`mdl-${agent}-default`);
        picker.replaceChildren(...(models[agent] || []).map((m) => new Option(`${m.label} · ${m.id}`, m.id)));
        setSelectValue(picker, selected);
        for (const m of models[agent] || []) {
            const row = document.createElement("div");
            row.className = "model-row";
            const label = document.createElement("span");
            label.className = "m-label";
            label.textContent = m.label;
            const id = document.createElement("span");
            id.className = "m-id";
            id.textContent = m.id;
            const rm = document.createElement("button");
            rm.className = "btn btn-sm btn-ghost";
            rm.textContent = m.id === selected ? "默认" : "移除";
            rm.disabled = m.id === selected;
            rm.title = m.id === selected ? "移除前请先选择其他默认模型" : "";
            rm.addEventListener("click", () => {
                const next = { ...models, [agent]: (models[agent] || []).filter((x) => x.id !== m.id) };
                putSettings({ models: next }, rm, "已移除 " + m.label);
            });
            row.append(label, id, rm);
            box.appendChild(row);
        }
    }
}
for (const agent of ["claude", "codex"]) {
    const picker = $(`mdl-${agent}-default`);
    picker.addEventListener("change", async () => {
        picker.disabled = true;
        const saved = await putSettings({ default_models: { [agent]: picker.value } }, null, "已设置，新建工作空间时生效");
        if (!saved)
            setSelectValue(picker, S.settings?.default_models[agent] || "");
        picker.disabled = false;
    });
}
for (const btn of document.querySelectorAll(".mdl-add")) {
    btn.addEventListener("click", () => {
        const agent = btn.dataset.agent;
        const label = $(`mdl-${agent}-label`).value.trim();
        const id = $(`mdl-${agent}-id`).value.trim();
        if (!label || !MODEL_ID_RE.test(id)) {
            toast("请填写显示名称和合法的模型 ID（字母数字开头，可含 . _ -）", true);
            return;
        }
        const models = (S.settings && S.settings.models) || {};
        if ((models[agent] || []).some((x) => x.id === id)) {
            toast("模型 " + id + " 已在列表中", true);
            return;
        }
        const next = { ...models, [agent]: [...(models[agent] || []), { id, label }] };
        putSettings({ models: next }, btn, "已添加 " + label).then((ok) => {
            if (ok) {
                $(`mdl-${agent}-label`).value = "";
                $(`mdl-${agent}-id`).value = "";
            }
        });
    });
}
/* ---------------- 用户管理 / 登录密码 ---------------- */
async function loadUsers() {
    let users;
    try {
        users = await api("/users");
    }
    catch (e) {
        toast("读取用户列表失败：" + e.message, true);
        return;
    }
    const box = $("user-list");
    box.replaceChildren();
    for (const u of users)
        box.appendChild(userRow(u));
}
function userRow(u) {
    const row = document.createElement("div");
    row.className = "user-row";
    const name = document.createElement("span");
    name.className = "u-name mono";
    name.textContent = u.name;
    const role = document.createElement("span");
    role.className = "u-role" + (u.role === "admin" ? " admin" : "");
    role.textContent = u.role === "admin" ? "管理员" : "普通用户";
    const meta = document.createElement("span");
    meta.className = "u-meta";
    meta.textContent = u.sessions > 0 ? u.sessions + " 个工作空间" : "暂无工作空间";
    const quota = quotaChip(u.quota);
    const acts = document.createElement("div");
    acts.className = "u-actions";
    const credit = document.createElement("button");
    credit.className = "btn btn-sm btn-ghost";
    credit.textContent = "额度";
    credit.addEventListener("click", () => openQuota(u.name, loadUsers));
    acts.appendChild(credit);
    const pw = document.createElement("button");
    pw.className = "btn btn-sm btn-ghost";
    pw.textContent = "重置密码";
    pw.addEventListener("click", async () => {
        const next = await askPrompt({
            title: "重置密码",
            label: "为用户「" + u.name + "」设置新密码",
            hint: "至少 8 位。重置后该用户其他已登录端会立即失效。",
            password: true,
            validate: (v) => (v.length < 8 ? "密码至少 8 位" : ""),
        });
        if (next === null)
            return;
        try {
            await api("/users/" + u.name + "/password", {
                method: "POST", body: JSON.stringify({ password: next }),
            });
            toast("已重置「" + u.name + "」的密码，其已登录端将失效");
        }
        catch (e) {
            toast("重置失败：" + e.message, true);
        }
    });
    acts.appendChild(pw);
    if (u.role !== "admin") {
        const del = document.createElement("button");
        del.className = "btn btn-sm btn-danger";
        del.textContent = "删除";
        del.addEventListener("click", async () => {
            const ok = await askConfirm("删除用户「" + u.name + "」？", {
                title: "删除用户",
                hint: "该用户的全部工作空间和容器将一并移除，文件保留在服务器磁盘上。",
                okLabel: "删除", danger: true,
            });
            if (!ok)
                return;
            btnBusy(del, "删除中…");
            try {
                await api("/users/" + u.name, { method: "DELETE" });
                toast("用户已删除");
                loadUsers();
                refreshAll();
            }
            catch (e) {
                toast("删除失败：" + e.message, true);
                btnDone(del);
            }
        });
        acts.appendChild(del);
    }
    row.append(name, role, quota, meta, acts);
    return row;
}
$("btn-user-add").addEventListener("click", async () => {
    const username = $("user-new-name").value.trim();
    const password = $("user-new-pass").value;
    if (!/^[a-z0-9][a-z0-9_-]{1,31}$/.test(username)) {
        toast("用户名不合法：小写字母或数字开头，可含 - 和 _，长度 2-32", true);
        return;
    }
    if (password.length < 8) {
        toast("初始密码至少 8 位", true);
        return;
    }
    btnBusy($("btn-user-add"), "创建中…");
    try {
        await api("/users", { method: "POST", body: JSON.stringify({ username, password }) });
        $("user-new-name").value = "";
        $("user-new-pass").value = "";
        toast("用户 " + username + " 已创建，可用该账号密码登录");
        loadUsers();
    }
    catch (e) {
        toast("创建失败：" + e.message, true);
    }
    finally {
        btnDone($("btn-user-add"));
    }
});
$("btn-pw-save").addEventListener("click", async () => {
    const oldPw = $("pw-old").value;
    const newPw = $("pw-new").value;
    if (newPw.length < 8) {
        toast("新密码至少 8 位", true);
        return;
    }
    btnBusy($("btn-pw-save"), "保存中…");
    try {
        await api("/me/password", {
            method: "POST",
            body: JSON.stringify({ old_password: oldPw, new_password: newPw }),
        });
        $("pw-old").value = "";
        $("pw-new").value = "";
        toast("密码已修改；其他已登录端将失效，本端保持登录");
    }
    catch (e) {
        toast("修改失败：" + e.message, true);
    }
    finally {
        btnDone($("btn-pw-save"));
    }
});
/* ---------------- 关于 ---------------- */
async function loadSystem() {
    const kv = $("about-kv");
    kv.replaceChildren();
    kv.classList.add("hidden");
    $("about-loading").classList.remove("hidden");
    let sys;
    try {
        sys = await api("/system");
    }
    catch (e) {
        toast("读取系统信息失败：" + e.message, true);
        return;
    }
    finally {
        $("about-loading").classList.add("hidden"); // 失败时也别留着转圈
    }
    kv.classList.remove("hidden");
    const add = (k, v) => {
        const dt = document.createElement("dt");
        dt.textContent = k;
        const dd = document.createElement("dd");
        dd.textContent = v;
        kv.append(dt, dd);
    };
    add("运行时", "agentbox · " + sys.go_version);
    add("Docker", sys.docker_version ? sys.docker_version : "无法连接");
    add("监听地址", sys.listen);
    add("工作空间", `${sys.sessions_running} 个运行中 / 共 ${sys.sessions_total} 个`);
    add("账号池", sys.accounts + " 个账号");
    add("用户", sys.users + " 个（含管理员）");
    add("数据目录", sys.data_dir);
    add("配置文件", sys.config_path);
    add("运行时长", fmtUptime(Date.now() - sys.started_at) + "（自 " + new Date(sys.started_at).toLocaleString() + "）");
}
/* ---------------- 运维监控 ---------------- */
let monTimer = null;
function startMonitor() {
    stopMonitor();
    $("mon-tiles").replaceChildren();
    $("mon-tbody").replaceChildren();
    $("mon-loading").classList.remove("hidden");
    loadMonitor(true); // 初次加载才弹错，之后的自动刷新失败静默重试
    // 后端每次请求会阻塞一个采样窗口（~300ms），5 秒一轮足够跟手又不压服务。
    monTimer = setInterval(() => {
        if (S.view !== "settings" || S.sec !== "monitor") {
            stopMonitor();
            return;
        }
        if (!$("mon-auto").checked)
            return;
        loadMonitor(false);
    }, 5000);
}
function stopMonitor() {
    if (monTimer) {
        clearInterval(monTimer);
        monTimer = null;
    }
}
async function loadMonitor(surfaceErr) {
    let m;
    try {
        m = await api("/monitor");
    }
    catch (e) {
        if (surfaceErr && S.sec === "monitor")
            toast("读取监控失败：" + e.message, true);
        return;
    }
    if (S.sec !== "monitor")
        return; // 请求在途中切走了页，丢弃这帧
    $("mon-loading").classList.add("hidden");
    renderMonitorTiles(m);
    renderMonitorTable(m);
}
function monTile(label, value, sub, pct) {
    const el = document.createElement("div");
    el.className = "mon-tile";
    el.append(Object.assign(document.createElement("div"), { className: "mt-label", textContent: label }), Object.assign(document.createElement("div"), { className: "mt-value", textContent: value }));
    if (typeof pct === "number") {
        const bar = document.createElement("div");
        bar.className = "mt-bar";
        const fill = document.createElement("span");
        const p = Math.max(0, Math.min(100, pct));
        fill.style.width = p + "%";
        if (p >= 90)
            fill.classList.add("hot");
        else if (p >= 70)
            fill.classList.add("warn");
        bar.appendChild(fill);
        el.appendChild(bar);
    }
    if (sub)
        el.appendChild(Object.assign(document.createElement("div"), { className: "mt-sub", textContent: sub }));
    return el;
}
function renderMonitorTiles(m) {
    const box = $("mon-tiles");
    const p = m.process, h = m.host, su = m.summary;
    const memPct = h.mem_total ? (h.mem_used / h.mem_total) * 100 : 0;
    const diskPct = h.disk_total ? (h.disk_used / h.disk_total) * 100 : 0;
    // 首帧还没有上一帧可做差，CPU 速率标「测量中」而不是误导的 0.0%。
    const cpu = (v) => (m.window_ms ? v.toFixed(1) + "%" : "测量中");
    const cpuPct = (v) => (m.window_ms ? v : undefined);
    const tiles = [
        monTile("后端 CPU", cpu(p.cpu_percent), "已运行 " + fmtUptime(p.uptime_ms), cpuPct(p.cpu_percent)),
        monTile("后端内存", fmtBytes(p.rss), "Go 堆 " + fmtBytes(p.heap_alloc) + " · " + p.goroutines + " 协程"),
        monTile("主机 CPU", cpu(h.cpu_percent), h.cpu_count + " 核 · 负载 " + h.load1.toFixed(2), cpuPct(h.cpu_percent)),
        monTile("主机内存", fmtBytes(h.mem_used) + " / " + fmtBytes(h.mem_total), memPct.toFixed(0) + "% 已用", memPct),
    ];
    // 数据盘写满会连带拖垮 SQLite 与所有会话，水位单独给一格（读不到则不显示）。
    if (h.disk_total) {
        const hint = diskPct >= 90 ? "数据盘将满，尽快清理" : "数据目录所在磁盘";
        tiles.push(monTile("磁盘水位", fmtBytes(h.disk_used) + " / " + fmtBytes(h.disk_total), diskPct.toFixed(0) + "% 已用 · " + hint, diskPct));
    }
    tiles.push(monTile("运行容器", su.running + " / " + su.total, "个工作空间容器在运行"), monTile("容器合计", cpu(su.cpu_percent), "内存 " + fmtBytes(su.mem_usage)));
    box.replaceChildren(...tiles);
}
function renderMonitorTable(m) {
    const win = m.window_ms
        ? "采样窗口 " + (m.window_ms / 1000).toFixed(1) + " 秒"
        : "首次采样中";
    $("mon-sub").textContent =
        `共 ${m.summary.total} 个工作空间 · ${m.summary.running} 个运行中 · ${win}`;
    const tb = $("mon-tbody");
    tb.replaceChildren();
    if (!m.containers.length) {
        const tr = document.createElement("tr");
        const td = document.createElement("td");
        td.colSpan = 9;
        td.className = "mon-empty";
        td.textContent = "暂无工作空间容器";
        tr.appendChild(td);
        tb.appendChild(tr);
        return;
    }
    for (const c of m.containers)
        tb.appendChild(monRow(c, m.now, !!m.window_ms));
}
function monCell(text, cls) {
    const el = document.createElement("td");
    el.textContent = text;
    if (cls)
        el.className = cls;
    return el;
}
function monRow(c, now, rate) {
    const tr = document.createElement("tr");
    if (!c.running)
        tr.className = "off";
    const name = document.createElement("td");
    name.className = "mon-name";
    name.append(agentIcon(c.agent, 13), document.createTextNode(c.name || c.session_id));
    const status = document.createElement("td");
    status.className = "mon-status";
    const dot = document.createElement("span");
    dot.className = "mon-dot" + (c.running ? " on" : "");
    status.append(dot, document.createTextNode(c.running ? "运行中" : "已停止"));
    const mem = monCell(c.running ? fmtBytes(c.mem_usage) : "—", "num");
    if (c.running && c.mem_limit)
        setTip(mem, fmtBytes(c.mem_usage) + " / " + fmtBytes(c.mem_limit) + " 上限");
    tr.append(name, monCell(c.user), monCell(agentName(c.agent)), status, monCell(c.running && c.started_at ? fmtTime(c.started_at) : "—"), monCell(c.running && c.started_at ? fmtUptime(now - c.started_at) : "—"), monCell(c.running ? (rate ? c.cpu_percent.toFixed(1) + "%" : "…") : "—", "num"), mem, monCell(c.running && c.pids ? String(c.pids) : "—", "num"));
    return tr;
}
