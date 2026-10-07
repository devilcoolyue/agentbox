import { SettingsRequests } from "./requests.js";
import { initSettingsMaintenance } from "./maintenance.js";
import { setAttrRender, setText, setTextRender, t as i18nText } from "../../i18n.js";
import { initImageUpdates, fillImageUpdateSettings, startImageUpdates, stopImageUpdates } from "./image-updates.js";
import { loadSystem, stopSystem, startMonitor, stopMonitor } from "./operations.js";
import { settingsState } from "./state.js";
import { bindSaveBar, confirmDiscard, defineGroups, dirtyGroups, holdDirty, rebaseline, saveDirty } from "./savebar.js";
import type { SettingsPatch } from "./savebar.js";
/* settings：系统设置视图 —— 账号池维护（含 OAuth / API Key 登录弹窗）、
 * 容器与资源、模型管理、安全与访问、关于。 */
"use strict";

import { actionButton } from "../../icons.js";

import { setSelectValue } from "../../select.js";

import { S, bus, emit } from "../../state.js";
import type {
  Account, ApiKeyTest, OAuthFinish, OAuthStart, Settings, User,
} from "../../types.js";
import { $, btnBusy as baseBusy, btnDone as baseDone, toast as baseToast, fmtTime, askConfirm, askPrompt } from "../../util.js";
import { api as httpApi } from "../../api.js";
import { refreshAll } from "../../data.js";
import { showView } from "../../shell.js";
import { MODEL_ID_RE } from "../../chat.js";
import { agentKey, agentName, agentIcon, decorateAgentOpts } from "../../brand.js";
import { quotaChip, openQuota } from "../../quota.js";
import { loadProxies, mountProxyPicker, openProxiesSection, refreshProxyCount } from "../../proxies.js";
import type { ProxyPicker } from "../../proxies.js";
import { openPricingSection, refreshPriceCount } from "../../pricing.js";
import { setTip } from "../../tip.js";
import { editAccountReasoning, editReasoning, reasoningLabel } from "../../reasoning-editor.js";
import { accessLabel, editAccountAccess } from "../../account-access.js";
import { moreButton } from "../../menu.js";

/* 静态标识装饰：添加账号弹窗的类型选择卡、模型管理卡片标题 */
decorateAgentOpts($("acct-form"));
for (const h of document.querySelectorAll<HTMLElement>("h3[data-agent]")) {
  h.classList.add("agent-h3", "agent-" + agentKey(h.dataset.agent!));
  h.prepend(agentIcon(h.dataset.agent!, 15));
}

export const SET_SECS = ["accounts", "proxies", "container", "models", "pricing", "interface", "security", "monitor", "about"];

export function createSettingsController() {
 const owner=S.token,role=S.role,lifetime=new AbortController();
 const scope=new SettingsRequests(httpApi,()=>S.token===owner&&S.role===role);
 const requests=scope;
 const busyButtons=new Set<Parameters<typeof baseBusy>[0]>();
 const toast:typeof baseToast=(...args)=>{if(scope.current())baseToast(...args);};
 const btnBusy:typeof baseBusy=(button,...args)=>{if(scope.current()){busyButtons.add(button);baseBusy(button,...args);}};
 const btnDone:typeof baseDone=(button)=>{if(scope.current()){busyButtons.delete(button);baseDone(button);}};
 async function api<T=any>(path:string,options?:RequestInit):Promise<T>{
  const result=await scope.request<T>(path,options);
  if(!scope.current())throw new DOMException('Settings owner disposed','AbortError');
  return result as T;
 }
/* ---------------- 视图入口与二级导航 ---------------- */

async function openSettingsView() {
  if (S.role !== "admin") return; // 普通用户无入口，这里再兜一道
  showView("settings");
  setSec(S.sec || "accounts");
  renderSettingsAccounts();
  // 停在别的分区时也要把 IP 代理的条数拉出来：左侧计数是给管理员扫一眼用的，
  // 停在 0 上等人点开就是假信息。分区本身进来时自己会拉。
  if (S.sec !== "proxies") refreshProxyCount();
  const scope=requests;if(!scope)return;
  try {
    const value=await scope.read<Settings>();if(!value)return;
    settingsState.value=value;
    // 离开设置页时没保存的改动还留在表单里：回来时接着显示保存条，不用服务端的值盖掉
    const restore = holdDirty();
    fillSettingsForms();
    restore();
  } catch (e) { if(!scope.current())return;
    if(scope.current())toast(i18nText("读取设置失败：") + (e as Error).message, true);
  }
}



function setSec(name: string) {
  S.sec = name;
  emit("navigation-changed");
  for (const b of document.querySelectorAll<HTMLElement>("#set-nav button")) {
    b.classList.toggle("active", b.dataset.sec === name);
  }
  for (const sec of SET_SECS) {
    $("sec-" + sec).classList.toggle("hidden", sec !== name);
  }
  $("set-content").scrollTop = 0;
  stopImageUpdates();
  if (name === "container") startImageUpdates();
  stopMonitor(); // 离开监控页就停轮询，任何切换都先关掉
  stopSystem();if (name === "about") void loadSystem(lifetime.signal);
  if (name === "security") loadUsers();
  if (name === "monitor") startMonitor();
  if (name === "proxies") openProxiesSection();
  if (name === "pricing") openPricingSection();
}






/* ---------------- 账号池 ---------------- */

function acctStateSeg(a: Account): [string, string, string] {
  // 返回 [状态class, 状态文案, 附加说明]
  const via = a.base_url ? a.base_url.replace(/^https?:\/\//, "") : i18nText("官方接口");
  if (a.type === "claude") {
    if (a.auth_mode === "apikey") return ["ok", i18nText("Key 已配置"), via];
    if (a.cred_status === "ok") {
      return ["ok", i18nText("凭证正常"), a.expires_at ? i18nText("令牌 ") + fmtTime(a.expires_at) + i18nText(" 到期（自动续期）") : ""];
    }
    if (a.cred_status === "norefresh") return ["warn", i18nText("需重新登录"), i18nText("refresh token 已失效")];
    return ["", i18nText("未登录"), i18nText("凭证目录已就绪，等待授权")];
  }
  if (a.auth_mode === "oauth" && a.cred_status === "ok") return ["ok", i18nText("订阅已授权"), i18nText("ChatGPT 订阅 · CLI 自动续期")];
  if (a.cred_status === "norefresh") return ["warn", i18nText("需重新授权"), i18nText("缺少刷新令牌")];
  if (a.cred_status === "ok") return ["ok", i18nText("Key 已配置"), via];
  return ["", i18nText("未认证"), i18nText("选择订阅授权或 API Key")];
}

function renderSettingsAccounts() {
  if(!scope.current())return;
  $("set-acct-count").textContent = String(S.accounts.length);
  const box = $("acct-list-box");
  box.replaceChildren();
  if (!S.accounts.length) {
    const p = document.createElement("p");
    p.className = "acct-empty";
    setText(p, "账号池为空。点击右上角「添加账号」创建第一个账号。");
    box.appendChild(p);
    return;
  }
  for (const a of S.accounts) box.appendChild(acctRow(a));
}

function acctRow(a: Account) {
  const [stCls, , stExtra] = acctStateSeg(a);
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
  const statusText = document.createTextNode("");
  setTextRender(statusText, () => acctStateSeg(a)[1]);
  st.append(dot, statusText);
  state.appendChild(st);
  if (stExtra) {
    const extra = document.createElement("span");
    setTextRender(extra, () => acctStateSeg(a)[2]);
    state.append(extra);
  }
  const spaces = document.createElement("span");
  setTextRender(spaces, () => a.sessions > 0 ? a.sessions + i18nText(" 个工作空间在用") : i18nText("暂无工作空间使用"));
  state.append(spaces);
  // 出口 IP 直接标在账号行上：它决定官方那边看到的是谁，排查封号时第一眼要看的
  // 就是这个，藏进编辑弹窗里等于没有。
  const px = document.createElement("span");
  px.className = "acct-proxy" + (a.proxy_id ? "" : " none");
  setTextRender(px, () => a.proxy_id ? "⇄ " + (a.proxy_label || a.proxy_id) : i18nText("⇄ 直连"));
  setTip(px, () => a.proxy_id ? i18nText("该账号的请求经此代理出网") : i18nText("该账号的请求从服务器自身 IP 发出"));
  state.appendChild(px);

  // 行内只露两个带字的常用动作，其余收进 ⋯，删除放最后。
  const acts = document.createElement("div");
  acts.className = "acct-actions";
  const auth = document.createElement("button");
  const needAuth = a.cred_status !== "ok";
  auth.className = "btn btn-sm" + (needAuth ? " btn-primary" : " btn-ghost");
  actionButton(auth, () => i18nText("认证"), "key", () => needAuth ? i18nText("认证账号：登录订阅或填写 API Key") : i18nText("管理认证：重新登录或更换 API Key"));
  auth.addEventListener("click", () => openAuthDlg(a));
  const edit = document.createElement("button");
  edit.className = "btn btn-sm btn-ghost";
  actionButton(edit, () => i18nText("编辑"), "edit", () => i18nText("编辑名称、环境变量与出口代理"));
  edit.addEventListener("click", () => openAcctEdit(a));
  const more = moreButton(() => [
    { label: i18nText("使用范围 · ") + accessLabel(a), icon: "users", run: () => editAccountAccess(a) },
    { label: i18nText("模型能力"), icon: "sliders", run: () => editAccountReasoning(a), tip: i18nText("为这个账号覆盖模型的推理强度能力") },
    { label: i18nText("删除账号"), icon: "trash", danger: true, sep: true, run: () => openAcctDel(a),
      disabled: a.sessions > 0, tip: a.sessions > 0 ? i18nText("有工作空间在用，请先删除对应工作空间") : undefined },
  ], () => i18nText("{p0} 的更多操作", { p0: String(a.label) }));
  acts.append(auth, edit, more);

  row.append(idBox, state, acts);
  return row;
}

/* 环境变量 <-> 文本（每行 KEY=VALUE） */
function envToText(env: Record<string, string> | undefined) {
  return Object.entries(env || {}).map(([k, v]) => k + "=" + v).join("\n");
}
function parseEnvText(text: string) {
  const env: Record<string, string> = {};
  for (const line of String(text).split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    const key = i > 0 ? t.slice(0, i).trim() : "";
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      throw new Error(i18nText("环境变量格式错误：「") + t + i18nText("」应为 KEY=VALUE"));
    }
    env[key] = t.slice(i + 1);
  }
  return env;
}

/* ---- 添加账号 ---- */






/* ---- 编辑账号 ---- */

let editAcct: Account | null = null;
let acctPicker: ProxyPicker | null = null;

async function openAcctEdit(a: Account) {
  editAcct = a;
  setTextRender($("acct-edit-title"), () => i18nText("编辑账号 · ") + a.id);
  $<HTMLInputElement>("acct-edit-label").value = a.label;
  $<HTMLTextAreaElement>("acct-edit-env").value = envToText(a.env);
  $("acct-edit-error").classList.add("hidden");
  if (!acctPicker) acctPicker = mountProxyPicker($("acct-edit-proxy"));
  acctPicker.set(a.proxy_id || "");
  $<HTMLDialogElement>("dlg-acct-edit").showModal();
  // 代理列表后到也不挡开弹窗：选择器先用当前缓存画，拉到新列表再刷一遍选中项
  // 的文案（否则刚在 IP 代理页加的代理，这里要等下次开弹窗才看得见）。
  try {
    await loadProxies();if(!scope.current())return;
    acctPicker.set(a.proxy_id || "");
  } catch (_) { if(!scope.current())return; /* 列表拉不到就只能选「无代理」，不影响改名字/环境变量 */ }
}




/* ---- 删除账号 ---- */

let delAcct: Account | null = null;
function openAcctDel(a: Account) {
  delAcct = a;
  setTextRender($("acct-del-text"), () => i18nText("确认从账号池删除「") + a.label + "」（" + a.id + "）？");
  $<HTMLDialogElement>("dlg-acct-del").showModal();
}




/* ---- 账号创建与认证：同一弹窗，失败保留账号以便重试 ---- */
let authAcct: Account | null = null;
let authCreating = false;
let authBusy = false;
let authMode = "oauth";
let createPicker: ProxyPicker | null = null;

function authMsg(text: string, isErr?: boolean) {
  if(!scope.current())return;
  const m = $("auth-msg");
  m.textContent = text;
  m.classList.toggle("hidden", !text);
  m.classList.toggle("err", !!isErr);
}

function authType() {
  return authAcct?.type || document.querySelector<HTMLInputElement>('#acct-form input[name="atype"]:checked')!.value;
}

function resetAuthCode() {
  $("auth-linkrow").classList.add("hidden");
  $("auth-code-label").classList.add("hidden");
  $("auth-finish").classList.add("hidden");
  $<HTMLInputElement>("auth-code").value = "";
  $<HTMLAnchorElement>("auth-link").removeAttribute("href");
}

function updateAuthType() {
  const claude = authType() === "claude";
  setTextRender($("auth-oauth-hint"), () => claude
    ? i18nText("打开授权链接，使用 Claude 订阅账号登录，再把页面显示的授权码粘贴回来。")
    : i18nText("打开授权链接，使用 ChatGPT 账号登录。授权后复制地址栏中完整的 localhost:1455 回调地址并粘贴回来；本地页面无法打开也不影响完成授权。"));
  setTextRender($("auth-code-title"), () => claude ? i18nText("授权码") : i18nText("完整回调地址"));
  setAttrRender($<HTMLInputElement>("auth-code"), "placeholder", () => claude ? i18nText("粘贴授权码（xxxx#yyyy）") : "http://localhost:1455/auth/callback?code=…&state=…");
  $<HTMLSelectElement>("auth-wire").hidden = claude;
  setText($("auth-key-hint"), "填写官方或中转服务的 API Key。Base URL 留空使用官方接口，填写时请使用服务商提供的完整 API 地址。");
  $("auth-test").classList.toggle("hidden", !authAcct);
  $("auth-clearkey").classList.toggle("hidden", !(claude && authAcct?.auth_mode === "apikey"));
  actionButton($("auth-gen"), () => i18nText("生成链接"), "link", () => authCreating && !authAcct ? i18nText("保存账号并生成授权链接") : i18nText("生成授权链接"));
  actionButton($("auth-savekey"), () => i18nText("保存"), "save", () => authCreating ? i18nText("保存账号并完成认证") : i18nText("保存认证"));
}

function openAuthDlg(a: Account) {
  if(!scope.current())return;
  if (authBusy) return;
  authCreating = false;
  authAcct = a;
  $("acct-form").classList.add("hidden");
  setTextRender($("auth-title"), () => a.label + i18nText(" · 账号认证"));
  resetAuthCode();
  $<HTMLInputElement>("auth-apikey").value = "";
  $<HTMLInputElement>("auth-baseurl").value = a.base_url || "";
  setSelectValue($<HTMLSelectElement>("auth-wire"), a.wire_api === "chat" ? "chat" : "responses");
  renderAuthModels(null);
  authMsg("");
  updateAuthType();
  setAuthMode(a.auth_mode === "apikey" || (!a.auth_mode && a.cred_status === "ok") ? "key" : "oauth");
  $<HTMLDialogElement>("dlg-auth").showModal();
}

function setAuthMode(mode: string) {
  if (authBusy) return;
  authMode = mode;
  $("auth-oauth").classList.toggle("hidden", mode !== "oauth");
  $("auth-key").classList.toggle("hidden", mode !== "key");
  for (const m of ["oauth", "key"]) {
    $("auth-mode-" + m).classList.toggle("active", mode === m);
    $("auth-mode-" + m).setAttribute("aria-pressed", String(mode === m));
  }
  authMsg("");
}

async function ensureAuthAccount(): Promise<Account> {
  if (authAcct) return authAcct;
  const env = parseEnvText($<HTMLTextAreaElement>("acct-env").value);
  const mode = $<HTMLSelectElement>("acct-access").value;
  const users = [...new Set($<HTMLTextAreaElement>("acct-users").value.split(/[\s,，]+/).filter(Boolean))];
  // The request only happens on an authentication action. Retain its result on
  // errors so retry never creates a second account or loses the configured proxy.
  authAcct = await api<Account>("/accounts", {method: "POST", body: JSON.stringify({
    id: $<HTMLInputElement>("acct-id").value.trim(), type: authType(),
    label: $<HTMLInputElement>("acct-label").value.trim(), env,
    proxy_id: createPicker?.get() || "", access: {mode, ...(mode === "users" ? {users} : {})},
  })});
  $("acct-created").classList.remove("hidden");
  updateAuthType();
  void refreshAll().catch(e => toast((e as Error).message, true));
  return authAcct;
}

async function authAction(button: string, work: () => Promise<void>) {
  if (authBusy) return;
  if (!authAcct && !$<HTMLFormElement>("acct-form").reportValidity()) return;
  authBusy = true;
  authMsg("");
  btnBusy($(button), () => i18nText("处理中…"));
  const controls = [...$("dlg-auth").querySelectorAll<HTMLInputElement | HTMLButtonElement | HTMLSelectElement | HTMLTextAreaElement>("button, input, select, textarea")];
  const disabled = controls.map(el => el.disabled);
  controls.forEach(el => { el.disabled = true; });
  const untrack=scope.cleanup(()=>controls.forEach((el,i)=>{el.disabled=disabled[i];}));
  try { await work(); }
  catch (e) { if(!scope.current())return; authMsg((e as Error).message, true); }
  finally {
    untrack();if(!scope.current())return;
    controls.forEach((el, i) => { el.disabled = disabled[i]; });
    btnDone($(button));
    authBusy = false;
    $<HTMLFieldSetElement>("acct-fields").disabled = authCreating && !!authAcct;
    updateAuthType();
  }
}

function finishAuth() {
  $<HTMLInputElement>("auth-code").value = "";
  $<HTMLInputElement>("auth-apikey").value = "";
  resetAuthCode();
  if (authCreating) {
    $<HTMLDialogElement>("dlg-auth").close();
    toast(i18nText("账号已添加并完成认证"));
  } else {
    authMsg(i18nText("认证已保存，下次对话或重新打开终端时使用新配置。"));
  }
  void refreshAll().catch(e => toast((e as Error).message, true));
}

/* 测试连接 = 拉一次 /v1/models：通就显示延迟 + 模型列表，点模型直接入库 */


function renderAuthModels(models: string[] | null) {
  const box = $("auth-models");
  box.replaceChildren();
  box.classList.toggle("hidden", !models || !models.length);
  if (!models) return;
  const agent = authAcct!.type; // chips 加进当前账号类型对应的模型列表
  const have = new Set((((settingsState.value && settingsState.value.models) || {})[agent] || []).map((m) => m.id));
  for (const id of models) {
    const chip = document.createElement("button");
    chip.type = "button";
    chip.className = "auth-model-chip mono" + (have.has(id) ? " in" : "");
    actionButton(chip, id, have.has(id) ? "check" : "plus", () => have.has(id) ? i18nText("{p0} · 已在模型列表中", { p0: String(id) }) : i18nText("将 {p0} 加入模型列表", { p0: String(id) }));
    setTip(chip, () => have.has(id) ? i18nText("已在模型列表中") : i18nText("点击加入模型列表"));
    chip.addEventListener("click", () => {
      const models = (settingsState.value && settingsState.value.models) || {};
      if ((models[agent] || []).some((x) => x.id === id)) {
        toast(i18nText("模型 ") + id + i18nText(" 已在列表中"));
        return;
      }
      const next = { ...models, [agent]: [...(models[agent] || []), { id, label: id }] };
      putSettings({ models: next }, chip, i18nText("已添加 ") + id).then((ok) => {
        if (ok) { chip.classList.add("in"); actionButton(chip, id, "check", () => i18nText("{p0} · 已在模型列表中", { p0: String(id) })); }
      });
    });
    box.appendChild(chip);
  }
}





/* ---------------- 容器与资源 / 安全与访问 表单 ---------------- */

function fillSettingsForms() {
  const st = settingsState.value;
  if (!st) return;
  fillImageUpdateSettings(st.image_updates);
  $<HTMLInputElement>("set-image").value = st.agent_image;
  $<HTMLInputElement>("set-mem").value = String(st.container.memory_mb);
  $<HTMLInputElement>("set-cpus").value = String(st.container.cpus);
  $<HTMLInputElement>("set-pids").value = String(st.container.pids_limit);
  const net = $<HTMLSelectElement>("set-net");
  if (![...net.options].some((o) => o.value === st.container.network)) {
    // 配置里出现了自定义 docker 网络名，补一个选项而不是悄悄丢掉
    net.appendChild(new Option(st.container.network + i18nText(" — 自定义网络"), st.container.network));
  }
  setSelectValue(net, st.container.network);
  $<HTMLInputElement>("set-running").value = String(st.resources?.max_running || 0);
  $<HTMLInputElement>("set-user-running").value = String(st.resources?.max_running_per_user || 0);
  $<HTMLInputElement>("set-free-gib").value = String((st.resources?.min_free_bytes || 0) / (1024 ** 3));
  $<HTMLInputElement>("set-idle").value = String(st.idle_timeout_min);
  const tz = $<HTMLSelectElement>("set-timezone");
  if (![...tz.options].some((o) => o.value === st.timezone)) {
    tz.appendChild(new Option(st.timezone + i18nText("（自定义）"), st.timezone));
  }
  setSelectValue(tz, st.timezone);
  setSelectValue($<HTMLSelectElement>("set-perm"), st.permission_mode);
  $<HTMLInputElement>("set-upload").value = String(st.max_upload_mb);
  $<HTMLInputElement>("set-listen").value = st.listen;
  setTextRender($("security-note"), () => st.restart_required
    ? i18nText("监听地址已修改，重启 agentbox 服务后生效")
    : i18nText("权限模式与上传上限即时生效；监听地址需重启 agentbox 服务"));
  $<HTMLInputElement>("set-tunnel-on").checked = !!(st.tunnel && st.tunnel.enabled);
 $<HTMLInputElement>("set-tunnel-transparent").checked = !!st.tunnel?.transparent;
 $<HTMLInputElement>("set-network-bind").value = st.tunnel?.network_bind || "";
 $<HTMLInputElement>("set-network-image").value = st.tunnel?.network_image || "";
  $<HTMLInputElement>("set-tunnel-bind").value = (st.tunnel && st.tunnel.proxy_bind) || "";
  $<HTMLInputElement>("set-tunnel-host").value = (st.tunnel && st.tunnel.proxy_host) || "";
  $<HTMLInputElement>("set-bridge-bind").value = (st.proxy_bridge && st.proxy_bridge.bind) || "";
  $<HTMLInputElement>("set-bridge-host").value = (st.proxy_bridge && st.proxy_bridge.host) || "";
  setTextRender($("tunnel-note"), () => st.tunnel && st.tunnel.enabled
    ? (st.tunnel_active ? i18nText("隧道已启用，SOCKS5 代理监听中") : i18nText("隧道已启用，但代理未在监听（检查绑定地址）"))
    : i18nText("默认绑定 docker 网桥网关 172.17.0.1，仅容器与本机可达"));
  const tips: Partial<Settings["terminal_tips"]> = st.terminal_tips || {};
  $<HTMLTextAreaElement>("set-tips").value = (tips.tips || []).join("\n");
  $<HTMLInputElement>("set-tips-interval").value = String(tips.interval_sec != null ? tips.interval_sec : 4);
  setSelectValue($<HTMLSelectElement>("set-tips-anim"), tips.animation || "scroll");
  renderModels();
  refreshPriceCount();
  rebaseline();
}

async function putSettings(
  patch: SettingsPatch, btn: HTMLButtonElement | null, okMsg?: string, keepDirty = true,
) {
  const scope=requests;if(!scope?.current())return false;
  if (btn) btnBusy(btn, () => i18nText("保存中…"));
  const untrack=scope.cleanup(()=>{if(btn)btnDone(btn);});
  try {
    const value=await scope.save<Settings>(JSON.stringify(patch));if(!value)return false;
    settingsState.value=value;
    // 即时保存（模型列表等）只提交了自己的字段，其余卡片里没保存的改动重填后放回去
    const restore = keepDirty ? holdDirty() : null;
    fillSettingsForms();
    restore?.();
    if (settingsState.value.models) { S.models = settingsState.value.models; emit("models-updated"); } // 对话框的模型菜单同步更新
    if (settingsState.value.terminal_tips) { // 终端顶栏轮播实时刷新
      S.termTips = settingsState.value.terminal_tips;
      emit("tips-updated");
    }
    if (settingsState.value.timezone && settingsState.value.timezone !== S.timeZone) {
      S.timeZone = settingsState.value.timezone;
      emit("timezone-updated");
    }
    toast(okMsg || i18nText("已保存"));
    return true;
  } catch (e) { if(!scope.current())return;
    if(scope.current())toast(i18nText("保存失败：") + (e as Error).message, true);
    return false;
  } finally {
    untrack();if(scope.current()&&btn)btnDone(btn);
  }
}















/* ---------------- 模型管理 ---------------- */

function renderModels() {
  const models = (settingsState.value && settingsState.value.models) || {};
  for (const agent of ["claude", "codex"]) {
    const box = document.querySelector<HTMLElement>(`.model-rows[data-agent="${agent}"]`);
    if (!box) continue;
    box.replaceChildren();
    const selected = settingsState.value?.default_models[agent] || "";
    const picker = $<HTMLSelectElement>(`mdl-${agent}-default`);
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
      id.title = m.id;
      const rm = document.createElement("button");
      rm.className = "btn btn-sm btn-ghost m-remove";
      actionButton(rm, () => m.id === selected ? i18nText("默认") : i18nText("移除"), m.id === selected ? "check" : "close", () => m.id === selected ? i18nText("当前默认模型") : i18nText("移除模型"));
      rm.disabled = m.id === selected;
      setTip(rm, () => m.id === selected ? i18nText("移除前请先选择其他默认模型") : i18nText("移除模型"));
      rm.addEventListener("click", () => {
        const next = { ...models, [agent]: (models[agent] || []).filter((x) => x.id !== m.id) };
        putSettings({ models: next }, rm, i18nText("已移除 ") + m.label);
      });
      const reasoning = document.createElement("button");
      reasoning.className = "btn btn-sm btn-ghost m-reasoning";
      actionButton(reasoning, () => i18nText("模型能力 · ") + reasoningLabel(m.reasoning), "sliders");
      reasoning.addEventListener("click", async () => {
        const policy = await editReasoning(agent, m.label + i18nText(" · 模型能力"), m.reasoning);
        if (policy === null) return;
        try {
          const fresh = await api<Settings>("/settings");
          const next = { ...fresh.models, [agent]: fresh.models[agent].map(x => x.id === m.id ? { ...x, reasoning: policy } : x) };
          await putSettings({ models: next }, reasoning, i18nText("已保存模型能力"));
        } catch (error) { if(!scope.current())return; toast((error as Error).message, true); }
      });
      row.append(label, id, reasoning, rm);
      box.appendChild(row);
    }
  }
}

for (const agent of ["claude", "codex"]) {
  const picker = $<HTMLSelectElement>(`mdl-${agent}-default`);
  picker.addEventListener("change", async () => {
    picker.disabled = true;
    const saved = await putSettings({ default_models: { [agent]: picker.value } }, null, i18nText("已设置，新建工作空间时生效"));
    if (!scope.current())return;
    if (!saved) setSelectValue(picker, settingsState.value?.default_models[agent] || "");
    picker.disabled = false;
  });
}

for (const btn of document.querySelectorAll<HTMLButtonElement>(".mdl-add")) {
  btn.addEventListener("click", () => {
    const agent = btn.dataset.agent!;
    const label = $<HTMLInputElement>(`mdl-${agent}-label`).value.trim();
    const id = $<HTMLInputElement>(`mdl-${agent}-id`).value.trim();
    if (!label || !MODEL_ID_RE.test(id)) {
      toast(i18nText("请填写显示名称和合法的模型 ID（字母数字开头，可含 . _ -）"), true);
      return;
    }
    const models = (settingsState.value && settingsState.value.models) || {};
    if ((models[agent] || []).some((x) => x.id === id)) {
      toast(i18nText("模型 ") + id + i18nText(" 已在列表中"), true);
      return;
    }
    const next = { ...models, [agent]: [...(models[agent] || []), { id, label }] };
    putSettings({ models: next }, btn, i18nText("已添加 ") + label).then((ok) => {
      if (ok) {
        $<HTMLInputElement>(`mdl-${agent}-label`).value = "";
        $<HTMLInputElement>(`mdl-${agent}-id`).value = "";
      }
    });
  });
}

/* ---------------- 用户管理 / 登录密码 ---------------- */

async function loadUsers() {
  let users: User[];
  try { users = await api<User[]>("/users"); } catch (e) { if(!scope.current())return;
    toast(i18nText("读取用户列表失败：") + (e as Error).message, true);
    return;
  }
  const box = $("user-list");
  box.replaceChildren();
  for (const u of users) box.appendChild(userRow(u));
}

function userRow(u: User) {
  const row = document.createElement("div");
  row.className = "user-row";

  const name = document.createElement("span");
  name.className = "u-name mono";
  name.textContent = u.name;
  const role = document.createElement("span");
  role.className = "u-role" + (u.role === "admin" ? " admin" : "");
  setTextRender(role, () => u.role === "admin" ? i18nText("管理员") : i18nText("普通用户"));
  const meta = document.createElement("span");
  meta.className = "u-meta";
  setTextRender(meta, () => u.sessions > 0 ? u.sessions + i18nText(" 个工作空间") : i18nText("暂无工作空间"));
  const quota = quotaChip(u.quota);

  const acts = document.createElement("div");
  acts.className = "u-actions"; // 只有三个动作：全部带字，删除放最后
  const credit = document.createElement("button");
  credit.className = "btn btn-sm btn-ghost";
  actionButton(credit, () => i18nText("额度"), "wallet", () => i18nText("查看和管理用户额度"));
  credit.addEventListener("click", () => openQuota(u.name, loadUsers));
  acts.appendChild(credit);
  const pw = document.createElement("button");
  pw.className = "btn btn-sm btn-ghost";
  actionButton(pw, () => i18nText("密码"), "key", () => i18nText("重置用户密码"));
  pw.addEventListener("click", async () => {
    const next = await askPrompt({
      get title() { return i18nText("重置密码"); },
      get label() { return i18nText("为用户「") + u.name + i18nText("」设置新密码"); },
      get hint() { return i18nText("至少 8 位。重置后该用户其他已登录端会立即失效。"); },
      password: true,
      validate: (v) => (v.length < 8 ? i18nText("密码至少 8 位") : ""),
    });
    if (next === null) return;
    try {
      await api("/users/" + u.name + "/password", {
        method: "POST", body: JSON.stringify({ password: next }),
      });
      toast(i18nText("已重置「") + u.name + i18nText("」的密码，其已登录端将失效"));
    } catch (e) { if(!scope.current())return;
      toast(i18nText("重置失败：") + (e as Error).message, true);
    }
  });
  acts.appendChild(pw);
  if (u.role !== "admin") {
    const del = document.createElement("button");
    del.className = "btn btn-sm btn-ghost btn-danger";
    actionButton(del, () => i18nText("删除"), "trash", () => i18nText("删除用户及其全部工作空间"));
    del.addEventListener("click", async () => {
      const ok = await askConfirm(() => i18nText("删除用户「") + u.name + "」？", {
        get title() { return i18nText("删除用户"); },
        get hint() { return i18nText("该用户的全部工作空间和容器将一并移除，文件保留在服务器磁盘上。"); },
        get okLabel() { return i18nText("删除"); }, icon: "trash", danger: true,
      });
      if (!ok) return;
      btnBusy(del, () => i18nText("删除中…"));
      try {
        await api("/users/" + u.name, { method: "DELETE" });
        toast(i18nText("用户已删除"));
        loadUsers();
        refreshAll();
      } catch (e) { if(!scope.current())return;
        toast(i18nText("删除失败：") + (e as Error).message, true);
        btnDone(del);
      }
    });
    acts.appendChild(del);
  }

  row.append(name, role, quota, meta, acts);
  return row;
}





/* ---------------- 统一保存条：各卡片的配置项分组登记 ---------------- */

const inputVal = (id: string) => $<HTMLInputElement>(id).value;
const valid = (...ids: string[]) => ids.every((id) => $<HTMLInputElement>(id).reportValidity());

function initSaveBar(signal: AbortSignal) {
  defineGroups([
    { id: "container", get label() { return i18nText("Agent 镜像与资源限制"); }, fields: ["set-image", "set-mem", "set-cpus", "set-pids", "set-net"],
      patch: () => ({
        agent_image: inputVal("set-image").trim(),
        expected_agent_image: settingsState.value?.agent_image,
        container: {
          memory_mb: Number(inputVal("set-mem")), cpus: Number(inputVal("set-cpus")),
          pids_limit: Number(inputVal("set-pids")), network: $<HTMLSelectElement>("set-net").value,
        },
      }) },
    { id: "image-updates", get label() { return i18nText("Agent 镜像自动更新"); }, fields: ["image-update-enabled", "image-update-channel", "image-update-time", "image-update-codex"],
      patch: () => valid("image-update-time") ? {
        image_updates: {
          enabled: $<HTMLSelectElement>("image-update-enabled").value === "on",
          channel: $<HTMLSelectElement>("image-update-channel").value as "stable" | "latest",
          time: inputVal("image-update-time"),
          update_codex: $<HTMLSelectElement>("image-update-codex").value === "on",
        },
      } : null },
    { id: "resources", get label() { return i18nText("容量与磁盘"); }, fields: ["set-running", "set-user-running", "set-free-gib"],
      patch: () => valid("set-running", "set-user-running", "set-free-gib") ? {
        resources: {
          max_running: Number(inputVal("set-running")), max_running_per_user: Number(inputVal("set-user-running")),
          min_free_bytes: Math.round(Number(inputVal("set-free-gib")) * 1024 ** 3),
        },
      } : null },
    { id: "idle", get label() { return i18nText("空闲自动停机"); }, fields: ["set-idle"],
      patch: () => ({ idle_timeout_min: Number(inputVal("set-idle")) }) },
    { id: "timezone", get label() { return i18nText("界面时区"); }, fields: ["set-timezone"],
      patch: () => ({ timezone: $<HTMLSelectElement>("set-timezone").value }) },
    { id: "tips", get label() { return i18nText("终端提示语"); }, fields: ["set-tips", "set-tips-interval", "set-tips-anim"],
      patch: () => ({
        terminal_tips: {
          tips: $<HTMLTextAreaElement>("set-tips").value.split("\n").map((t) => t.trim()).filter(Boolean),
          interval_sec: Number(inputVal("set-tips-interval")) || 0,
          animation: $<HTMLSelectElement>("set-tips-anim").value,
        },
      }) },
    { id: "security", get label() { return i18nText("权限模式与上传监听"); }, fields: ["set-perm", "set-upload", "set-listen"],
      patch: () => ({
        permission_mode: $<HTMLSelectElement>("set-perm").value,
        max_upload_mb: Number(inputVal("set-upload")),
        listen: inputVal("set-listen").trim(),
      }),
      done: (st) => { if (st.restart_required) toast(i18nText("监听地址改动需重启 agentbox 服务后生效")); } },
    { id: "tunnel", get label() { return i18nText("内网隧道"); },
      fields: ["set-tunnel-on", "set-tunnel-transparent", "set-network-bind", "set-network-image", "set-tunnel-bind", "set-tunnel-host"],
      patch: () => ({
        tunnel: {
          enabled: $<HTMLInputElement>("set-tunnel-on").checked,
          transparent: $<HTMLInputElement>("set-tunnel-transparent").checked,
          network_bind: inputVal("set-network-bind").trim(),
          network_image: inputVal("set-network-image").trim(),
          proxy_bind: inputVal("set-tunnel-bind").trim(),
          proxy_host: inputVal("set-tunnel-host").trim(),
        },
      }),
      done: (st) => { if (st.tunnel_error) toast(i18nText("隧道启动失败：") + st.tunnel_error, true); } },
    { id: "bridge", get label() { return i18nText("代理桥接地址"); }, fields: ["set-bridge-bind", "set-bridge-host"],
      patch: () => ({ proxy_bridge: { bind: inputVal("set-bridge-bind").trim(), host: inputVal("set-bridge-host").trim() } }),
      done: () => openProxiesSection() }, // 重绑结果（成功/失败）由列表接口回报
  ]);
  bindSaveBar($("set-content"), signal, () => void saveDirty(async (patch, okMsg) =>
    await putSettings(patch, $<HTMLButtonElement>("set-save"), okMsg, false) ? settingsState.value : null,
  ), () => { fillSettingsForms(); toast(i18nText("已放弃未保存的修改")); });
  bus.addEventListener("view-changed", () => {
    if (S.view !== "settings" && dirtyGroups().length) toast(i18nText("系统设置里有未保存的修改，回到设置页可继续保存"));
  }, { signal });
}

function initSettings() {
 initSettingsMaintenance(lifetime.signal,scope);
 initImageUpdates(lifetime.signal);
 initSaveBar(lifetime.signal);
 bus.addEventListener("view-changed", () => { if (S.view !== "settings") { stopMonitor();stopSystem(); stopImageUpdates(); } }, { signal: lifetime.signal });
 $("set-nav").addEventListener("click", async (e) => {
  const btn = (e.target as Element).closest<HTMLElement>("button[data-sec]");
  if (!btn) return;
  if (btn.dataset.sec !== S.sec && !await confirmDiscard(fillSettingsForms)) {
    emit("navigation-changed"); // 让窄屏分区下拉回到当前分区
    return;
  }
  if(scope.current())setSec(btn.dataset.sec!);
}, { signal: lifetime.signal });
bus.addEventListener("open-settings", openSettingsView, { signal: lifetime.signal });
bus.addEventListener("data-updated", () => {
  if (S.view === "settings") renderSettingsAccounts();
}, { signal: lifetime.signal });
$("btn-acct-add").addEventListener("click", () => {
  if (authBusy) return;
  authAcct = null;
  authCreating = true;
  $<HTMLFormElement>("acct-form").reset();
  $<HTMLFieldSetElement>("acct-fields").disabled = false;
  $("acct-form").classList.remove("hidden");
  $("acct-created").classList.add("hidden");
  $("acct-users-row").classList.add("hidden");
  setSelectValue($<HTMLSelectElement>("acct-access"), "all");
  if (!createPicker) createPicker = mountProxyPicker($("acct-proxy"));
  createPicker.set("");
  void loadProxies().then(() => {if(scope.current())createPicker?.set(createPicker.get());}).catch(() => {});
  setText($("auth-title"), "添加账号");
  resetAuthCode();
  $<HTMLInputElement>("auth-apikey").value = "";
  $<HTMLInputElement>("auth-baseurl").value = "";
  setSelectValue($<HTMLSelectElement>("auth-wire"), "responses");
  renderAuthModels(null);
  updateAuthType();
  setAuthMode("oauth");
  $<HTMLDialogElement>("dlg-auth").showModal();
}, {signal: lifetime.signal});
$("acct-form").addEventListener("submit", e => {
  e.preventDefault();
  if (!authBusy) $(authMode === "oauth" ? "auth-gen" : "auth-savekey").click();
}, {signal: lifetime.signal});
$("acct-form").addEventListener("change", e => {
  if ((e.target as HTMLInputElement).name === "atype") { resetAuthCode(); updateAuthType(); }
}, {signal: lifetime.signal});
$("acct-access").addEventListener("change", () => {
  $("acct-users-row").classList.toggle("hidden", $<HTMLSelectElement>("acct-access").value !== "users");
}, {signal: lifetime.signal});
$("dlg-auth").addEventListener("cancel", e => { if (authBusy) e.preventDefault(); }, {signal: lifetime.signal});
$("dlg-auth").addEventListener("close", () => {
  $<HTMLInputElement>("auth-apikey").value = "";
  $<HTMLTextAreaElement>("acct-env").value = "";
  resetAuthCode();
}, {signal: lifetime.signal});
$("acct-edit-cancel").addEventListener("click", () => $<HTMLDialogElement>("dlg-acct-edit").close(), { signal: lifetime.signal });
$("acct-edit-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!editAcct) return;
  const errEl = $("acct-edit-error");
  errEl.classList.add("hidden");
  let env;
  try { env = parseEnvText($<HTMLTextAreaElement>("acct-edit-env").value); } catch (err) { if(!scope.current())return;
    errEl.textContent = (err as Error).message;
    errEl.classList.remove("hidden");
    return;
  }
  btnBusy($("acct-edit-ok"), () => i18nText("保存中…"));
  try {
    await api("/accounts/" + editAcct.id, {
      method: "PATCH",
      body: JSON.stringify({
        label: $<HTMLInputElement>("acct-edit-label").value,
        env,
        proxy_id: acctPicker ? acctPicker.get() : "",
      }),
    });
    $<HTMLDialogElement>("dlg-acct-edit").close();
    toast(i18nText("账号已更新"));
    refreshAll();
  } catch (err) { if(!scope.current())return;
    errEl.textContent = (err as Error).message;
    errEl.classList.remove("hidden");
  } finally {
    btnDone($("acct-edit-ok"));
  }
}, { signal: lifetime.signal });
$("acct-del-cancel").addEventListener("click", () => $<HTMLDialogElement>("dlg-acct-del").close(), { signal: lifetime.signal });
$("acct-del-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!delAcct) return;
  btnBusy($("acct-del-ok"), () => i18nText("删除中…"));
  try {
    await api("/accounts/" + delAcct.id, { method: "DELETE" });
    $<HTMLDialogElement>("dlg-acct-del").close();
    toast(i18nText("账号已删除，凭证目录保留在磁盘上"));
    refreshAll();
  } catch (err) { if(!scope.current())return;
    toast(i18nText("删除失败：") + (err as Error).message, true);
  } finally {
    btnDone($("acct-del-ok"));
  }
}, { signal: lifetime.signal });
$("auth-mode-oauth").addEventListener("click", () => setAuthMode("oauth"), { signal: lifetime.signal });
$("auth-mode-key").addEventListener("click", () => setAuthMode("key"), { signal: lifetime.signal });
$("auth-gen").addEventListener("click", () => authAction("auth-gen", async () => {
  const acct = await ensureAuthAccount();
  const {url} = await api<OAuthStart>(`/accounts/${acct.id}/oauth/start`, {method: "POST"});
  resetAuthCode();
  const link = $<HTMLAnchorElement>("auth-link");
  link.href = url;
  setText(link, "打开授权页面 ↗");
  $("auth-linkrow").classList.remove("hidden");
  $("auth-code-label").classList.remove("hidden");
  $("auth-finish").classList.remove("hidden");
  authMsg(i18nText("授权链接已生成，15 分钟内有效。请打开链接完成授权。"));
}), {signal: lifetime.signal});
$("auth-copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText($<HTMLAnchorElement>("auth-link").href);
    authMsg(i18nText("链接已复制"));
  } catch (_) { if(!scope.current())return;
    authMsg(i18nText("复制失败，请手动选中链接复制"), true);
  }
}, { signal: lifetime.signal });
$("auth-finish").addEventListener("click", () => authAction("auth-finish", async () => {
  if (!authAcct) return;
  const code = $<HTMLInputElement>("auth-code").value.trim();
  if (!code) throw new Error(authType() === "codex" ? i18nText("请粘贴完整回调地址") : i18nText("请粘贴授权码"));
  await api<OAuthFinish>(`/accounts/${authAcct.id}/oauth/finish`, {method: "POST", body: JSON.stringify({code})});
  authAcct.auth_mode = "oauth";
  authAcct.base_url = "";
  $<HTMLInputElement>("auth-baseurl").value = "";
  finishAuth();
}), {signal: lifetime.signal});
$("auth-savekey").addEventListener("click", () => authAction("auth-savekey", async () => {
  const key = $<HTMLInputElement>("auth-apikey").value.trim();
  const base = $<HTMLInputElement>("auth-baseurl").value.trim();
  if (!key) throw new Error(i18nText("请输入 API Key"));
  if (base) {
    let parsed: URL;
    try { parsed = new URL(base); } catch { throw new Error(i18nText("Base URL 必须是完整的 http(s) 地址")); }
    if (!["http:", "https:"].includes(parsed.protocol)) throw new Error(i18nText("Base URL 必须以 http:// 或 https:// 开头"));
  }
  const acct = await ensureAuthAccount();
  await api(`/accounts/${acct.id}/apikey`, {method: "POST", body: JSON.stringify({api_key: key, base_url: base, wire_api: $<HTMLSelectElement>("auth-wire").value})});
  acct.auth_mode = "apikey";
  acct.base_url = base;
  finishAuth();
}), {signal: lifetime.signal});
$("auth-test").addEventListener("click", () => authAction("auth-test", async () => {
  if (!authAcct) return;
  renderAuthModels(null);
  const res = await api<ApiKeyTest>(`/accounts/${authAcct.id}/apikey/test`, {
    method: "POST", body: JSON.stringify({api_key: $<HTMLInputElement>("auth-apikey").value.trim(), base_url: $<HTMLInputElement>("auth-baseurl").value.trim()}),
  });
  renderAuthModels(res.models);
  authMsg(i18nText("连接正常 · {p0}ms · {p1} 个模型，可点击加入模型列表", { p0: String(res.latency_ms), p1: String(res.models.length) }));
}), {signal: lifetime.signal});
$("auth-clearkey").addEventListener("click", () => authAction("auth-clearkey", async () => {
  if (!authAcct) return;
  await api(`/accounts/${authAcct.id}/apikey`, {method: "DELETE"});
  authAcct.auth_mode = "oauth";
  authAcct.base_url = "";
  $<HTMLInputElement>("auth-baseurl").value = "";
  renderAuthModels(null);
  authMsg(i18nText("已清除中转配置，切回保存的订阅凭证；如未授权，请先完成订阅授权。"));
  void refreshAll();
}), {signal: lifetime.signal});
$("auth-close").addEventListener("click", () => { if (!authBusy) $<HTMLDialogElement>("dlg-auth").close(); }, { signal: lifetime.signal });
$("btn-user-add").addEventListener("click", async () => {
  const username = $<HTMLInputElement>("user-new-name").value.trim();
  const password = $<HTMLInputElement>("user-new-pass").value;
  if (!/^[a-z0-9][a-z0-9_-]{1,31}$/.test(username)) {
    toast(i18nText("用户名不合法：小写字母或数字开头，可含 - 和 _，长度 2-32"), true);
    return;
  }
  if (password.length < 8) {
    toast(i18nText("初始密码至少 8 位"), true);
    return;
  }
  btnBusy($("btn-user-add"), () => i18nText("创建中…"));
  try {
    await api("/users", { method: "POST", body: JSON.stringify({ username, password }) });
    $<HTMLInputElement>("user-new-name").value = "";
    $<HTMLInputElement>("user-new-pass").value = "";
    toast(i18nText("用户 ") + username + i18nText(" 已创建，可用该账号密码登录"));
    loadUsers();
  } catch (e) { if(!scope.current())return;
    toast(i18nText("创建失败：") + (e as Error).message, true);
  } finally {
    btnDone($("btn-user-add"));
  }
}, { signal: lifetime.signal });
$("btn-pw-save").addEventListener("click", async () => {
  const oldPw = $<HTMLInputElement>("pw-old").value;
  const newPw = $<HTMLInputElement>("pw-new").value;
  if (newPw.length < 8) { toast(i18nText("新密码至少 8 位"), true); return; }
  btnBusy($("btn-pw-save"), () => i18nText("保存中…"));
  try {
    await api("/me/password", {
      method: "POST",
      body: JSON.stringify({ old_password: oldPw, new_password: newPw }),
    });
    $<HTMLInputElement>("pw-old").value = "";
    $<HTMLInputElement>("pw-new").value = "";
    toast(i18nText("密码已修改；其他已登录端将失效，本端保持登录"));
  } catch (e) { if(!scope.current())return;
    toast(i18nText("修改失败：") + (e as Error).message, true);
  } finally {
    btnDone($("btn-pw-save"));
  }
}, { signal: lifetime.signal });
 return () => {
  if(lifetime.signal.aborted)return;
  scope.dispose();lifetime.abort();stopMonitor();stopSystem();stopImageUpdates();
  for(const button of busyButtons)baseDone(button);busyButtons.clear();
  for(const id of ['dlg-auth','dlg-acct-edit','dlg-acct-del'])$<HTMLDialogElement>(id).close();
  $<HTMLInputElement>('auth-apikey').value='';$<HTMLTextAreaElement>('acct-env').value='';
  resetAuthCode();authAcct=null;editAcct=null;delAcct=null;
 };
}

 const dispose=initSettings();
 return {dispose,openSettingsView,renderSettingsAccounts,openAuthDlg,putSettings};
}
