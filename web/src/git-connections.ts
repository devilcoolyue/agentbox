import { htmlText as trHTML, setAttrRender, setText, setTextRender, t as i18nText } from "./i18n.js";
import { skelRows } from "./skeleton.js";
import { openGitManagement } from "./git-management.js";
import { createGitSurface } from "./git-surface.js";
import { openGitTerminal } from "./git-terminal.js";
import { openGitShares } from "./git-shares.js";
import { openGitReviews } from "./git-reviews.js";
import { openGitOAuth, openGitOAuthApps } from "./git-oauth.js";
import { gitRequest, openGitOperations } from "./git-operations.js";
import { api } from "./api.js";
import { S, bus } from "./state.js";
import { askConfirm, askPrompt, toast } from "./util.js";
import { actionButton, decorateIcons } from "./icons.js";
import { moreButton } from "./menu.js";
import type { MenuItem } from "./menu.js";
import { enhanceSelects, setSelectValue } from "./select.js";
import type { GitConnection, GitBinding, GitStatus, GitPushPreview } from "./types.js";

const dialogs = new Set<HTMLDialogElement>();
function dialog(title: string | (() => string), markup: string) {
  const d = document.createElement("dialog");
  d.className = "dlg-git-connections";
  d.innerHTML = `<div class="dlg-head"><h2></h2><button type="button" class="dlg-x" aria-label="${trHTML("关闭")}" data-close data-icon="close" data-i18n-attrs="{&quot;aria-label&quot;:&quot;关闭&quot;}"></button></div><div class="dlg-body">${markup}</div>`;
  if (typeof title === "function") setTextRender(d.querySelector("h2")!, title);
  else d.querySelector("h2")!.textContent = title;
  d.querySelector("[data-close]")!.addEventListener("click", () => d.close());
  d.addEventListener("close", () => { dialogs.delete(d); d.remove(); });
  document.body.append(d); dialogs.add(d); decorateIcons(d); enhanceSelects(d); d.showModal();
  return d;
}
function errorText(d: HTMLElement, error: unknown) {
  d.querySelector<HTMLElement>("[data-error]")!.textContent = (error as Error).message;
  for (const skeleton of d.querySelectorAll(".skeleton-list")) skeleton.remove(); // 读失败就别再装作在读
}
function option(value: string, label: string) { return Object.assign(document.createElement("option"), {value, textContent: label}); }
function action(label: string, icon: string, run: () => Promise<void>, tip = label) {
  const b = document.createElement("button"); b.type = "button"; b.className = "btn btn-sm"; actionButton(b, label, icon, tip);
  b.addEventListener("click", async () => { b.disabled = true; try { await run(); } catch (e) { toast((e as Error).message, true); } finally { b.disabled = false; } });
  return b;
}

export function openGitConnections() { openGitManagement("connections"); }

function renderGitConnections(host: HTMLElement) {
  const token = S.token;
  const d = document.createElement("section");
  d.className = "git-surface";
  d.id = "git-connections";
  d.innerHTML = `<div class="sec-intro"><div><h2><span data-i18n="仓库连接">${trHTML("仓库连接")}</span></h2><p><span data-i18n="连接可在你的多个工作空间复用。添加后，到空间「变更 → 远程」绑定仓库。">${trHTML("连接可在你的多个工作空间复用。添加后，到空间「变更 → 远程」绑定仓库。")}</span></p></div></div>
    <div class="git-connection-tools"><button class="btn btn-primary btn-sm" data-add data-icon="plus"><span class="action-label" data-i18n="添加连接">${trHTML("添加连接")}</span></button><button class="btn btn-sm" data-oauth data-icon="login"><span class="action-label" data-i18n="网页授权">${trHTML("网页授权")}</span></button><button class="btn btn-sm" data-oauth-apps data-icon="key"><span class="action-label" data-i18n="OAuth 应用">${trHTML("OAuth 应用")}</span></button><button class="btn btn-sm" data-refresh data-icon="refresh"><span class="action-label" data-i18n="刷新">${trHTML("刷新")}</span></button><button class="btn btn-sm" data-operations data-icon="history"><span class="action-label" data-i18n="操作记录">${trHTML("操作记录")}</span></button></div>
    <p class="git-note"><span data-i18n="默认连接用于新空间和未绑定仓库的默认选择；已有绑定保持不变。保存连接后可先「测试」仓库读取权限。">${trHTML("默认连接用于新空间和未绑定仓库的默认选择；已有绑定保持不变。保存连接后可先「测试」仓库读取权限。")}</span></p>
    <p data-error role="alert" class="login-error"></p><div data-list aria-live="polite"></div>`;
  d.querySelector("[data-list]")!.append(skelRows(3, i18nText("读取中…"), { rowClass: "git-connection-row", lines: 2, actions: true }));
  host.append(d);
  decorateIcons(d);
  let generation = 0;
  const load = async () => {
    const gen = ++generation;
    d.querySelector("[data-error]")!.textContent = "";
    const [connections, def] = await Promise.all([api<GitConnection[]>("/git/connections"), api<{connection_id:string}>("/me/git/default")]);
    if (!d.isConnected || token !== S.token || gen !== generation) return;
    const list = d.querySelector<HTMLElement>("[data-list]")!; list.replaceChildren();
    if (!connections.length) list.innerHTML = `<div class="git-empty"><h3><span data-i18n="尚未添加 Git 连接">${trHTML("尚未添加 Git 连接")}</span></h3><p><span data-i18n="已有 Token 或 SSH 私钥？点击「添加连接」。也可以通过「网页授权」登录代码平台。">${trHTML("已有 Token 或 SSH 私钥？点击「添加连接」。也可以通过「网页授权」登录代码平台。")}</span></p><p><span data-i18n="只做本地提交时，无需添加连接。">${trHTML("只做本地提交时，无需添加连接。")}</span></p></div>`;
    for (const c of connections) {
      const managed=c.managed ?? c.owner===S.user;
      const row = document.createElement("div"); row.className = "git-connection-row";
      const title = document.createElement("strong"); setTextRender(title, () => c.label + (!managed ? i18nText(" · 共享自 ")+c.owner : "") + (def.connection_id === c.id ? i18nText(" · 默认") : ""));
      const meta = document.createElement("p"); meta.className = "field-hint"; setTextRender(meta, () => `${c.provider} · ${c.auth_type === "oauth" ? "OAuth" : c.auth_type === "ssh" ? "SSH" : "Token"} · ${c.base_url}\n${c.enabled ? i18nText("已保存") : i18nText("已停用")} · ${c.read_only ? i18nText("只读") : i18nText("允许推送")} · ${c.network?.route === "tunnel" ? i18nText("内网隧道") : i18nText("直连")}${c.host_fingerprint ? i18nText("\n主机指纹：")+c.host_fingerprint : ""}`);
      // 行内只露三个常用动作；启停、授权、公钥这些低频动作与删除收进 ⋯，删除放最后。
      const buttons = document.createElement("div"); buttons.className = "git-connection-tools";
      buttons.append(action(i18nText("测试"), "activity", async () => {
        const url = await askPrompt({get title() { return i18nText("测试 Git 连接"); },get label() { return i18nText("仓库地址"); },value:c.base_url+"/",get hint() { return i18nText("验证此仓库可读取；不会推送或证明写权限。"); }});
        if (url === null || token !== S.token) return;
        await gitRequest(`/git/connections/${c.id}/test`,{url},d);
        toast(i18nText("仓库可读取；写权限需在实际推送时由上游校验"));
      }, i18nText("测试仓库读取权限")), action(def.connection_id === c.id ? i18nText("取消默认") : i18nText("设为默认"), def.connection_id === c.id ? "undo" : "check", async () => {
        await api("/me/git/default", {method:"PUT", body:JSON.stringify({connection_id:def.connection_id === c.id ? "" : c.id})}); await load();
      }));
      if (managed) buttons.append(action(i18nText("编辑"), "rename", async () => editConnection(c, load), i18nText("编辑连接")));
      const later = (run: () => Promise<void>) => () => { run().catch(e => toast((e as Error).message, true)); };
      const items: MenuItem[] = [
        { label: c.enabled ? i18nText("停用") : i18nText("启用"), icon: c.enabled ? "stop" : "play", hidden: !managed, run: later(async () => {
          await api(`/git/connections/${c.id}`, { method:"PATCH", body:JSON.stringify({revision:c.revision, enabled:!c.enabled}) }); await load();
        }) },
        { label: i18nText("使用授权"), icon: "users", hidden: !(managed && S.role==="admin" && c.auth_type!=="oauth"), run: later(async () => openGitShares(c,load)),
          tip: i18nText("允许其他用户使用这个连接") },
        { label: i18nText("查看公钥"), icon: "key", hidden: !c.public_key, run: () => {
          const key=createGitSurface(() => i18nText("连接公钥"), `<p class="field-hint"><span data-i18n="将此公钥登记到上游 Git 服务账号或部署密钥；私钥不提供导出。">${trHTML("将此公钥登记到上游 Git 服务账号或部署密钥；私钥不提供导出。")}</span></p><pre data-public-key></pre>`);key.querySelector("[data-public-key]")!.textContent=c.public_key!;
        } },
        { label: i18nText("重新授权"), icon: "login", hidden: !(managed && c.auth_type === "oauth"), run: later(async () => openGitOAuth(c)) },
        { label: i18nText("撤销授权"), icon: "shield-off", danger: true, hidden: !(managed && c.auth_type === "oauth"), run: later(async () => {
          if(!await askConfirm(() => i18nText("撤销「{p0}」的 OAuth 授权？", { p0: String(c.label) }), {get title() { return i18nText("撤销 Git 授权"); },get hint() { return i18nText("会先停用本地连接，再请求上游撤销。上游可能同时影响使用同一应用授权的其他连接。"); },danger:true,get okLabel() { return i18nText("撤销"); },icon:"shield-off"}))return;
          const result=await gitRequest<{warning:string;remote_revoked:boolean}>(`/git/connections/${c.id}/revoke`,{revision:c.revision},d);
          toast(result.warning||i18nText("本地已停用，上游授权已撤销"),!result.remote_revoked);await load();
        }) },
        { label: i18nText("删除连接"), icon: "trash", danger: true, sep: true, hidden: !managed, run: later(async () => {
          if (!await askConfirm(() => i18nText("删除连接「{p0}」？", { p0: String(c.label) }), {get title() { return i18nText("删除 Git 连接"); }, get hint() { return i18nText("已绑定的连接需先解除引用。删除不会撤销上游平台的 Token。"); }, danger:true, get okLabel() { return i18nText("删除"); }, icon: "trash"})) return;
          await api(`/git/connections/${c.id}`, {method:"DELETE", body:JSON.stringify({revision:c.revision})}); await load();
        }) },
      ];
      if (items.some(item => !item.hidden)) buttons.append(moreButton(() => items, () => i18nText("{p0} 的更多操作", { p0: String(c.label) })));
      row.append(title, meta, buttons); list.append(row);
    }
  };
  d.querySelector("[data-oauth]")!.addEventListener("click",()=>openGitOAuth());
  d.querySelector("[data-oauth-apps]")!.classList.toggle("hidden",S.role!=="admin");
  d.querySelector("[data-oauth-apps]")!.addEventListener("click",openGitOAuthApps);
  d.querySelector("[data-operations]")!.addEventListener("click",openGitOperations);
  d.querySelector("[data-add]")!.addEventListener("click", () => editConnection(null, load));
  d.querySelector("[data-refresh]")!.addEventListener("click", () => { void load().catch(e => errorText(d,e)); });
  void load().catch(e => errorText(d,e));
}

function editConnection(c: GitConnection | null, saved: () => Promise<void>) {
  const token = S.token;
  const d = createGitSurface(() => c ? i18nText("编辑 Git 连接") : i18nText("添加 Git 连接"), `<form>
    <label><span data-i18n="认证方式">${trHTML("认证方式")}</span><select name="auth_type"><option value="pat">HTTPS Token</option><option value="ssh" data-i18n="SSH 私钥">${trHTML("SSH 私钥")}</option></select></label>
    <label><span data-i18n="连接名称">${trHTML("连接名称")}</span><input type="text" name="label" maxlength="128" required placeholder="${trHTML("例如：公司 GitLab")}" data-i18n-attrs="{&quot;placeholder&quot;:&quot;例如：公司 GitLab&quot;}"></label>
    <div class="dlg-row"><label><span data-i18n="平台">${trHTML("平台")}</span><select name="provider"><option value="github">GitHub</option><option value="gitlab" data-i18n="GitLab / 自建 GitLab">${trHTML("GitLab / 自建 GitLab")}</option><option value="generic" data-i18n="其他 HTTPS Git">${trHTML("其他 HTTPS Git")}</option></select></label>
    <label><span data-i18n="服务地址">${trHTML("服务地址")}</span><input type="url" name="base_url" required placeholder="https://git.example.com"></label></div>
    <label><span data-i18n="Git 用户名">${trHTML("Git 用户名")}</span><input type="text" name="username" maxlength="256" placeholder="${trHTML("GitHub / GitLab 留空可使用默认值")}" data-i18n-attrs="{&quot;placeholder&quot;:&quot;GitHub / GitLab 留空可使用默认值&quot;}"></label>
    <label data-token-row>${c ? i18nText("替换 Token（留空保留）") : "Personal Access Token"}<input type="password" name="token" autocomplete="new-password" maxlength="16384" ${c ? "" : "required"}></label>
    <div data-ssh class="hidden"><label><span data-i18n="SSH 私钥">${trHTML("SSH 私钥")}</span><textarea name="private_key" rows="4" autocomplete="off" spellcheck="false" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></label>
    <label><span data-i18n="私钥口令（如有）">${trHTML("私钥口令（如有）")}</span><input type="password" name="passphrase" autocomplete="new-password"></label>
    <label><span data-i18n="服务器主机公钥">${trHTML("服务器主机公钥")}</span><textarea name="host_key" rows="2" spellcheck="false" placeholder="ssh-ed25519 AAAA…"></textarea></label>
    <p class="field-hint"><span data-i18n="向 Git 服务管理员核对主机公钥，不能填个人公钥。私钥只在服务端使用，不复制到工作空间。修改已有连接时留空保留密钥。替换主机公钥前，请从管理员处重新核实指纹。">${trHTML("向 Git 服务管理员核对主机公钥，不能填个人公钥。私钥只在服务端使用，不复制到工作空间。修改已有连接时留空保留密钥。替换主机公钥前，请从管理员处重新核实指纹。")}</span></p></div>
    <label><span data-i18n="网络路由">${trHTML("网络路由")}</span><select name="route"><option value="" data-i18n="服务端直连">${trHTML("服务端直连")}</option><option value="tunnel" data-i18n="我的内网隧道">${trHTML("我的内网隧道")}</option></select></label>
    <label data-ca-row><span data-i18n="公司 CA 证书（可选）">${trHTML("公司 CA 证书（可选）")}</span><textarea name="ca_pem" rows="3" spellcheck="false" placeholder="${trHTML("PEM 根 CA / 中间 CA；留空使用系统信任库")}" data-i18n-attrs="{&quot;placeholder&quot;:&quot;PEM 根 CA / 中间 CA；留空使用系统信任库&quot;}"></textarea></label>
    <p class="field-hint"><span data-i18n="隧道需在线并放行服务域名和端口，断开时不会改为直连。路由与信任信息保存后固定，调整需新建连接。">${trHTML("隧道需在线并放行服务域名和端口，断开时不会改为直连。路由与信任信息保存后固定，调整需新建连接。")}</span></p>
    <label class="check"><input type="checkbox" name="read_only" checked><span data-i18n="只允许读取仓库">${trHTML("只允许读取仓库")}</span></label>
    <p class="field-hint"><span data-i18n="使用有目标仓库权限的 Token。推送需同时具有上游写权限。服务地址和用户名保存后不可修改；更换目标请创建新连接。">${trHTML("使用有目标仓库权限的 Token。推送需同时具有上游写权限。服务地址和用户名保存后不可修改；更换目标请创建新连接。")}</span></p>
    <p data-error role="alert" class="login-error"></p><div class="dlg-actions"><button type="submit" class="btn btn-primary" data-icon="save"><span class="action-label" data-i18n="保存">${trHTML("保存")}</span></button></div>
  </form>`);
  d.id = "dlg-git-connection-edit";
  const form = d.querySelector("form")!;
  const field = (name: string) => form.elements.namedItem(name) as HTMLInputElement;
  const provider = form.elements.namedItem("provider") as HTMLSelectElement;
  const save = d.querySelector<HTMLButtonElement>('[type="submit"]')!;
  const auth=form.elements.namedItem("auth_type") as HTMLSelectElement,route=form.elements.namedItem("route") as HTMLSelectElement;
  if(c?.auth_type==="oauth") auth.append(option("oauth","OAuth"));
  setSelectValue(auth,c?.auth_type||"pat");setSelectValue(route,c?.network?.route||"");field("ca_pem").value=c?.network?.ca_pem||"";
  const syncAuth=()=>{const ssh=auth.value==="ssh";d.querySelector("[data-ssh]")!.classList.toggle("hidden",!ssh);d.querySelector("[data-token-row]")!.classList.toggle("hidden",ssh);d.querySelector("[data-ca-row]")!.classList.toggle("hidden",ssh);field("token").required=!c&&!ssh;field("private_key").required=field("host_key").required=!c&&ssh;};
  auth.addEventListener("change",()=>{field("base_url").value=auth.value==="ssh"?"ssh://github.com":"https://github.com";field("username").value=auth.value==="ssh"?"git":"";syncAuth();});
  if(c){auth.disabled=route.disabled=field("ca_pem").disabled=true;setAttrRender(field("private_key"), "placeholder", () => i18nText("留空保留现有私钥"));setAttrRender(field("host_key"), "placeholder", () => c.host_fingerprint||i18nText("已保存主机公钥"));}
  syncAuth();
  field("base_url").value = c?.base_url || "https://github.com";
  setSelectValue(provider,c?.provider || "github");
  field("label").value = c?.label || ""; field("username").value = c?.username || "";
  field("read_only").checked = c?.read_only ?? true;
  if (c) provider.disabled = field("base_url").disabled = field("username").disabled = true;
  if (c?.auth_type === "oauth") {field("token").disabled=true;setAttrRender(field("token"), "placeholder", () => i18nText("OAuth 连接请通过授权入口重新连接"));}
  provider.addEventListener("change", () => { field("base_url").value = (auth.value === "ssh" ? "ssh://" : "https://") + (provider.value === "github" ? "github.com" : provider.value === "gitlab" ? "gitlab.com" : ""); });
  let busy = false;
  d.addEventListener("cancel", e => { if (busy) e.preventDefault(); });
  form.addEventListener("submit", async e => {
    e.preventDefault(); if(busy || token !== S.token) return;
    busy = true; save.disabled = true;
    (d.querySelector("[data-close]") as HTMLButtonElement).disabled = true;
    d.querySelector("[data-error]")!.textContent = "";
    const body = c
      ? {revision:c.revision,label:field("label").value.trim(),read_only:field("read_only").checked,...(field("token").value ? {token:field("token").value} : {}),...(c.auth_type==="ssh"&&field("private_key").value?{private_key:field("private_key").value,passphrase:field("passphrase").value}:{}),...(c.auth_type==="ssh"&&field("host_key").value?{host_key:field("host_key").value}:{})}
      : {label:field("label").value.trim(),provider:provider.value,base_url:field("base_url").value,username:field("username").value,token:field("token").value,read_only:field("read_only").checked,auth_type:auth.value,private_key:field("private_key").value,passphrase:field("passphrase").value,host_key:field("host_key").value,network:{route:route.value,ca_pem:auth.value === "ssh" ? "" : field("ca_pem").value}};
    try {
      await api(c ? `/git/connections/${c.id}` : "/git/connections",{method:c ? "PATCH" : "POST",body:JSON.stringify(body)});
      field("token").value = field("private_key").value = field("passphrase").value = ""; d.close(); if(token === S.token) {toast(i18nText("Git 连接已保存")); await saved();}
    } catch (e) {if(d.open) errorText(d,e);else if(token === S.token) toast((e as Error).message,true);}
    finally {busy = false; save.disabled = false;(d.querySelector("[data-close]") as HTMLButtonElement).disabled = false;}
  });
}

export async function openGitRemote(repo: string, status: GitStatus, refreshed: () => Promise<void>) {
  const session = S.current?.id, token = S.token; if (!session) return;
  const d = dialog(() => i18nText("仓库远程"), `<p data-repo class="field-hint"></p>
    <div class="dlg-row"><label>Remote<select data-remote></select></label><label><span data-i18n="Git 连接">${trHTML("Git 连接")}</span><select data-connection></select></label></div>
    <p data-target class="field-hint"></p><p class="field-hint"><span data-i18n="获取更新只更新远程跟踪分支，不合并工作文件。推送前会展示目标和提交；不会强制覆盖远程分支。">${trHTML("获取更新只更新远程跟踪分支，不合并工作文件。推送前会展示目标和提交；不会强制覆盖远程分支。")}</span></p>
    <div class="git-connection-tools"><button class="btn btn-sm" data-manage data-icon="link" data-tip="${trHTML("管理 Git 连接")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;管理 Git 连接&quot;}"><span class="action-label" data-i18n="连接">${trHTML("连接")}</span></button><button class="btn btn-sm" data-reload data-icon="refresh" data-tip="${trHTML("刷新 Git 连接与远程状态")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;刷新 Git 连接与远程状态&quot;}"><span class="action-label" data-i18n="刷新">${trHTML("刷新")}</span></button><button class="btn btn-sm" data-add-remote data-icon="plus" data-tip="${trHTML("添加远程仓库地址")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;添加远程仓库地址&quot;}"><span class="action-label" data-i18n="添加远程">${trHTML("添加远程")}</span></button><button class="btn btn-sm" data-edit-remote data-icon="rename" data-tip="${trHTML("编辑所选远程仓库地址")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;编辑所选远程仓库地址&quot;}"><span class="action-label" data-i18n="编辑">${trHTML("编辑")}</span></button><button class="btn btn-sm" data-remove-remote data-icon="trash" data-tip="${trHTML("删除所选远程配置，不删除服务器仓库")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;删除所选远程配置，不删除服务器仓库&quot;}"><span class="action-label" data-i18n="删除">${trHTML("删除")}</span></button><button class="btn btn-sm" data-bind disabled data-icon="plug" data-tip="${trHTML("保存当前仓库 remote 与 Git 连接的绑定")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;保存当前仓库 remote 与 Git 连接的绑定&quot;}"><span class="action-label" data-i18n="绑定">${trHTML("绑定")}</span></button><button class="btn btn-sm" data-default disabled data-icon="check" data-tip="${trHTML("设为空间默认 Git 连接")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;设为空间默认 Git 连接&quot;}"><span class="action-label" data-i18n="默认">${trHTML("默认")}</span></button><button class="btn btn-sm" data-operations data-icon="history" data-tip="${trHTML("查看 Git 操作记录")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;查看 Git 操作记录&quot;}"><span class="action-label" data-i18n="记录">${trHTML("记录")}</span></button><button class="btn btn-sm" data-terminal disabled data-icon="key" data-tip="${trHTML("管理当前仓库的终端 Git 授权")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;管理当前仓库的终端 Git 授权&quot;}"><span class="action-label" data-i18n="终端授权">${trHTML("终端授权")}</span></button><button class="btn btn-sm" data-reviews disabled data-icon="git-pr" data-tip="${trHTML("查看或创建 Pull Request / Merge Request")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;查看或创建 Pull Request / Merge Request&quot;}">PR / MR</button><button class="btn btn-sm" data-fetch disabled data-icon="git-fetch" data-tip="${trHTML("获取远程更新，不合并工作文件（fetch）")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;获取远程更新，不合并工作文件（fetch）&quot;}"><span class="action-label" data-i18n="获取">${trHTML("获取")}</span></button><button class="btn btn-sm" data-pull disabled data-icon="git-pull" data-tip="${trHTML("获取并快进合并上游提交，更新工作文件")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;获取并快进合并上游提交，更新工作文件&quot;}"><span class="action-label" data-i18n="拉取">${trHTML("拉取")}</span></button><button class="btn btn-sm btn-primary" data-preview disabled data-icon="eye" data-tip="${trHTML("预览待推送提交，确认后才推送")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;预览待推送提交，确认后才推送&quot;}"><span class="action-label" data-i18n="预览推送">${trHTML("预览推送")}</span></button></div>
    <p data-error role="alert" class="login-error"></p><p data-state role="status" class="field-hint"></p><div data-preview-box class="hidden"><pre data-commits></pre><button class="btn btn-primary" data-push data-icon="git-push" data-tip="${trHTML("确认推送已预览的提交到远程仓库")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;确认推送已预览的提交到远程仓库&quot;}"><span class="action-label" data-i18n="推送">${trHTML("推送")}</span></button></div>`);
  d.id = "dlg-git-remote";
  const remote = d.querySelector<HTMLSelectElement>("[data-remote]")!, connection = d.querySelector<HTMLSelectElement>("[data-connection]")!;
  const state = d.querySelector<HTMLElement>("[data-state]")!, previewBox = d.querySelector<HTMLElement>("[data-preview-box]")!;
  const button = (name: string) => d.querySelector<HTMLButtonElement>(`[data-${name}]`)!;
  const prefix = `/sessions/${session}/git`;
  let connections:GitConnection[] = [], bindings:GitBinding[] = [], inherited = "", busy = false, preview:GitPushPreview|null = null;
  setTextRender(d.querySelector("[data-repo]")!, () => repo || i18nText("空间文件根目录"));
  const activeBinding = () => bindings.find(b=>b.repo === repo && b.remote === remote.value);
  const sync = () => {
    const b = activeBinding(), c = connections.find(c=>c.id === connection.value);
    button("bind").disabled = busy || !remote.value;
    button("add-remote").disabled=busy||!c?.enabled;
    button("edit-remote").disabled=busy||!remote.value||!!b||!c?.enabled;
    button("remove-remote").disabled=busy||!remote.value||!!b;
    button("default").disabled = busy || !!connection.value && !c?.enabled;
    button("fetch").disabled = busy || !b || b.connection_id !== c?.id || !c.enabled;
    button("pull").disabled = button("fetch").disabled;
    button("terminal").disabled = button("fetch").disabled;
    button("reviews").disabled = button("fetch").disabled || c?.provider === "generic";
    button("preview").disabled = button("fetch").disabled || !!c?.read_only;
    setTextRender(d.querySelector("[data-target]")!, () => (status.remotes || []).find(r=>r.name===remote.value&&!r.push)?.url || b?.url || i18nText("当前仓库没有 remote，请先在终端配置远程地址。"));
  };
  const selected = () => {preview=null;previewBox.classList.add("hidden");setSelectValue(connection,activeBinding()?.connection_id || inherited);sync();};
  const load = async () => {
    const [cs,bs,sd,ud,fresh] = await Promise.all([api<GitConnection[]>("/git/connections"),api<GitBinding[]>(prefix+"/bindings"),api<{connection_id:string}>(prefix+"/default"),api<{connection_id:string}>("/me/git/default"),api<GitStatus>(prefix+"/status?repo="+encodeURIComponent(repo))]);
    if(!d.open || token!==S.token) return;
    connections=cs;bindings=bs;inherited=sd.connection_id || ud.connection_id;status=fresh;
    const previous=remote.value;
    remote.replaceChildren(...[...new Set([...(status.remotes || []).map(r=>r.name),...bs.filter(b=>b.repo===repo).map(b=>b.remote)])].map(r=>option(r,r)));
    if(previous) setSelectValue(remote,previous);
    connection.replaceChildren(option("",i18nText("不绑定 / 解除绑定")),...cs.map(c=>option(c.id,c.label+(c.enabled?(c.read_only?i18nText(" · 只读"):i18nText(" · 可推送")):i18nText(" · 已停用")))));
    selected();
  };
  const run = async (fn:()=>Promise<void>) => {
    if(busy || token!==S.token) return;busy=true;
    for(const b of d.querySelectorAll<HTMLButtonElement>("button")) b.disabled=true;
    remote.disabled=connection.disabled=true;d.querySelector("[data-error]")!.textContent="";
    try {await fn();}catch(e){if(d.open&&token===S.token)errorText(d,e);}finally{
      busy=false;for(const b of d.querySelectorAll<HTMLButtonElement>("button")) b.disabled=false;
      remote.disabled=connection.disabled=false;sync();
    }
  };
  d.addEventListener("cancel",e=>{if(busy)e.preventDefault();});
  remote.addEventListener("change",selected);connection.addEventListener("change",()=>{preview=null;previewBox.classList.add("hidden");sync();});
  button("manage").addEventListener("click",()=>{ d.close(); openGitConnections(); });
  button("operations").addEventListener("click",openGitOperations);
  button("reviews").addEventListener("click",()=>{const c=connections.find(c=>c.id===activeBinding()?.connection_id);if(c)openGitReviews(repo,remote.value,c,connections);});
  button("default").addEventListener("click",()=>void run(async()=>{
    await api(prefix+"/default",{method:"PUT",body:JSON.stringify({connection_id:connection.value})});
    setTextRender(state, () => connection.value?i18nText("空间默认连接已保存；已有仓库绑定不受影响。"):i18nText("已清除空间默认，将采用用户默认建议。"));
    await load();
  }));
  for(const kind of ["add","edit","remove"] as const)button(kind+"-remote").addEventListener("click",()=>void run(async()=>{
    let name=remote.value;
    if(kind==="add") {const chosen=await askPrompt({get title() { return i18nText("添加远程"); },get label() { return i18nText("Remote 名称"); },value:"origin",validate:v=>/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(v)?"":i18nText("名称仅支持字母、数字、点、下划线和连字符")});if(chosen===null)return;name=chosen;}
    const existing=(status.remotes||[]).find(r=>r.name===name&&!r.push)?.url||"";
    let url="";
    if(kind==="remove"){if(!await askConfirm(() => i18nText("删除 remote「{p0}」？", { p0: String(name) }), {get title() { return i18nText("删除远程配置"); },get hint() { return i18nText("移除本地远程配置和跟踪引用，不会删除服务器仓库。"); },danger:true,get okLabel() { return i18nText("删除"); }, icon: "trash"}))return;}
    else {const chosen=await askPrompt({get title() { return kind==="add"?i18nText("添加远程地址"):i18nText("修改远程地址"); },get label() { return i18nText("仓库地址"); },value:existing||connections.find(c=>c.id===connection.value)?.base_url+"/",get hint() { return i18nText("地址必须属于所选连接的平台。保存地址后需单独保存绑定。"); }});if(chosen===null)return;url=chosen;}
    await gitRequest(prefix+"/remotes",{repo,name,action:kind==="edit"?"update":kind,url,expected_url:existing,connection_id:connection.value},d);
    setText(state, "远程配置已保存，请核对后绑定连接。");await load();if(S.current?.id===session)await refreshed();
  }));
  button("reload").addEventListener("click",()=>void run(load));
  button("bind").addEventListener("click",()=>void run(async()=>{
    bindings=await api<GitBinding[]>(prefix+"/bindings",{method:"PUT",body:JSON.stringify({repo,remote:remote.value,connection_id:connection.value,revision:activeBinding()?.revision || 0})});selected();setText(state, "绑定已保存");
  }));
  button("terminal").addEventListener("click",()=>openGitTerminal(repo,remote.value,!!connections.find(c=>c.id===connection.value)?.read_only));
  button("fetch").addEventListener("click",()=>void run(async()=>{
    preview=null;previewBox.classList.add("hidden");setText(state, "正在获取远程分支…");
    const result=await gitRequest<{fetched_at:string}>(prefix+"/fetch",{repo,remote:remote.value},d);
    setText(state, "已获取最新远程分支；工作文件未合并更新。");
    if(result.fetched_at && S.current?.id===session) await refreshed();
  }));
  button("pull").addEventListener("click",()=>void run(async()=>{
    if(!await askConfirm(() => i18nText("获取并快进合并当前分支的上游提交？"), {get title() { return i18nText("快进拉取"); },get hint() { return i18nText("这会更新工作文件；需要工作区干净，分叉时停止。"); },get okLabel() { return i18nText("拉取"); },icon:"git-pull"}))return;
    preview=null;previewBox.classList.add("hidden");setText(state, "正在快进拉取…");
    await gitRequest(prefix+"/pull",{repo,remote:remote.value},d);
    setText(state, "已快进更新当前分支。");if(S.current?.id===session)await refreshed();
  }));
  button("preview").addEventListener("click",()=>void run(async()=>{
    preview=null;previewBox.classList.add("hidden");setText(state, "正在核对远程分支…");
    preview=await gitRequest<GitPushPreview>(prefix+"/push-preview",{repo,remote:remote.value},d);
    const snapshot = preview;
    setTextRender(state, () => `${snapshot.connection} → ${snapshot.url}\n${snapshot.ref}${snapshot.new_branch?i18nText("（新分支）"):""} · ${snapshot.expected_head.slice(0,12)}`);
    setTextRender(d.querySelector("[data-commits]")!, () => i18nText("待推送提交（最多显示 20 条）：\n")+snapshot.commits);previewBox.classList.remove("hidden");
  }));
  button("push").addEventListener("click",()=>void run(async()=>{
    const p=preview;if(!p)return;preview=null;previewBox.classList.add("hidden");setText(state, "正在推送…");
    await gitRequest(prefix+"/push",{repo:p.repo,remote:p.remote,ref:p.ref,expected_head:p.expected_head,expected_remote_head:p.expected_remote_head},d);
    setText(state, "已推送 {p0} 到 {p1}", { p0: String(p.expected_head.slice(0,12)), p1: String(p.ref) });
    if(S.current?.id===session)await refreshed();
  }));
  await run(load);
}

bus.addEventListener("git-section", e => {
  const { section, host } = (e as CustomEvent<{ section: string; host: HTMLElement }>).detail;
  if (section === "connections") renderGitConnections(host);
});
bus.addEventListener("signed-out",()=>{for(const d of dialogs)d.close();});

export function openGitClone(done: (repo:string)=>Promise<void>) {
  const session=S.current?.id,token=S.token;if(!session)return;
  const d=dialog(() => i18nText("克隆仓库"), `<form><label><span data-i18n="Git 连接">${trHTML("Git 连接")}</span><select name="connection" disabled></select></label>
    <label><span data-i18n="仓库地址">${trHTML("仓库地址")}</span><input name="url" type="text" required placeholder="https://git.example.com/team/project.git"></label>
    <label><span data-i18n="新文件夹名称">${trHTML("新文件夹名称")}</span><input name="directory" type="text" required maxlength="128" placeholder="project"></label>
    <p class="field-hint"><span data-i18n="克隆到当前空间根目录下的新文件夹；不会覆盖已有目录。先在用户菜单的「Git 管理 → 仓库连接」添加连接。">${trHTML("克隆到当前空间根目录下的新文件夹；不会覆盖已有目录。先在用户菜单的「Git 管理 → 仓库连接」添加连接。")}</span></p>
    <p data-error role="alert" class="login-error"></p><p data-state role="status" class="field-hint"><span data-i18n="读取连接中…">${trHTML("读取连接中…")}</span></p>
    <div class="dlg-actions"><button class="btn btn-primary" type="submit" disabled data-icon="git-clone"><span class="action-label" data-i18n="克隆">${trHTML("克隆")}</span></button></div></form>`);
  d.id="dlg-git-clone";
  const form=d.querySelector("form")!,picker=form.elements.namedItem("connection") as HTMLSelectElement;
  const field=(name:string)=>form.elements.namedItem(name) as HTMLInputElement;
  const submit=d.querySelector<HTMLButtonElement>('[type="submit"]')!,close=d.querySelector<HTMLButtonElement>("[data-close]")!;
  const state=d.querySelector<HTMLElement>("[data-state]")!;
  let busy=false;
  d.addEventListener("cancel",e=>{if(busy)e.preventDefault();});
  void Promise.all([api<GitConnection[]>("/git/connections"),api<{connection_id:string}>(`/sessions/${session}/git/default`),api<{connection_id:string}>("/me/git/default")]).then(([cs,sd,ud])=>{
    if(token!==S.token||!d.open)return;
    picker.replaceChildren(...cs.filter(c=>c.enabled).map(c=>option(c.id,c.label)));
    const selected=sd.connection_id||ud.connection_id;if(cs.some(c=>c.id===selected&&c.enabled))setSelectValue(picker,selected);
    picker.disabled=false;submit.disabled=!picker.value;setTextRender(state, () => picker.value?"":i18nText("没有可用连接，请先添加 Git 连接。"));
  }).catch(e=>errorText(d,e));
  form.addEventListener("submit",async e=>{
    e.preventDefault();if(busy||!picker.value||token!==S.token)return;
    busy=true;submit.disabled=close.disabled=true;setText(state, "正在克隆并检出文件…");d.querySelector("[data-error]")!.textContent="";
    try{
      const result=await gitRequest<{repo:string;warning:string}>(`/sessions/${session}/git/clone`,{connection_id:picker.value,url:field("url").value,directory:field("directory").value},d);
      if(token!==S.token)return;d.close();toast(result.warning||i18nText("仓库已克隆"));if(S.current?.id===session)await done(result.repo);
    }catch(e){if(d.open&&token===S.token){state.textContent="";errorText(d,e);}}
    finally{busy=false;submit.disabled=close.disabled=false;}
  });
}
