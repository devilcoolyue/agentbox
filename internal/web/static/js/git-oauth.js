import { createGitSurface } from "./git-surface.js";
import { api } from "./api.js";
import { S } from "./state.js";
import { setSelectValue } from "./select.js";
import { toast } from "./util.js";
import { actionButton } from "./icons.js";
const modal = createGitSurface;
function option(value, textContent) { return Object.assign(document.createElement("option"), { value, textContent }); }
export function openGitOAuth(connection) {
    const token = S.token, d = modal("授权 GitHub / GitLab", `<form><label>服务<select name="app" disabled></select></label>
 <label>连接名称<input name="label" type="text" maxlength="128" placeholder="例如：我的公司 GitLab"></label>
 <label class="check"><input name="read_only" type="checkbox" checked>仅允许 Agentbox 读取仓库</label>
 <label class="check"><input name="api_access" type="checkbox">允许平台 API（GitLab PR/MR 需要；会申请更广的 api 或 read_api 范围）</label>
 <p class="field-hint">由服务管理员预先注册 OAuth 应用。GitHub 的 repo 授权范围包含写权限；勾选只读后，Agentbox 仍会限制此连接不能推送。GitLab 会按所选模式申请读取或写入范围。</p>
 <p data-error role="alert" class="login-error"></p><div class="dlg-actions"><button type="submit" class="btn btn-primary" disabled data-icon="link" data-tip="生成 GitHub / GitLab 授权链接">生成链接</button></div></form>
 <p data-link class="hidden"><a target="_blank" rel="noopener noreferrer">前往服务平台授权</a></p><p data-help class="field-hint"></p>`);
    d.id = "dlg-git-oauth";
    const form = d.querySelector("form"), app = form.elements.namedItem("app"), save = d.querySelector('[type="submit"]'), error = d.querySelector("[data-error]");
    if (connection) {
        form.elements.namedItem("label").value = connection.label;
        form.elements.namedItem("read_only").checked = connection.read_only;
    }
    void api("/git/oauth/apps").then(apps => {
        if (!d.open || token !== S.token)
            return;
        app.replaceChildren(...apps.filter(a => a.enabled && (!connection || a.id === connection.oauth_app_id)).map(a => option(a.id, a.label + " · " + a.base_url)));
        app.disabled = false;
        save.disabled = !app.value;
        if (!app.value)
            error.textContent = "尚未配置可用 OAuth 应用，请联系管理员，或先使用 HTTPS Token 连接。";
    }).catch(e => { error.textContent = e.message; });
    let busy = false;
    d.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        if (busy || !app.value || token !== S.token)
            return;
        busy = true;
        save.disabled = true;
        error.textContent = "";
        d.querySelector("[data-link]").classList.add("hidden");
        try {
            const result = await api("/git/oauth/start", { method: "POST", body: JSON.stringify({ app_id: app.value, connection_id: connection?.id || "", label: form.elements.namedItem("label").value.trim(), read_only: form.elements.namedItem("read_only").checked, api_access: form.elements.namedItem("api_access").checked }) });
            if (!d.open || token !== S.token)
                return;
            const link = d.querySelector("[data-link] a");
            link.href = result.url;
            d.querySelector("[data-link]").classList.remove("hidden");
            d.querySelector("[data-help]").textContent = "请在当前浏览器完成授权，10 分钟内有效。完成后返回连接列表点击刷新。申请范围：" + result.scope;
        }
        catch (e) {
            error.textContent = e.message;
        }
        finally {
            busy = false;
            save.disabled = false;
        }
    });
}
export function openGitOAuthApps() {
    if (S.role !== "admin")
        return;
    const token = S.token, d = modal("Git OAuth 应用", `<p class="field-hint">在 GitHub OAuth Apps 或公司 GitLab Applications 注册应用，回调地址填写当前 Agentbox 域名加 /api/git/oauth/callback。Client Secret 加密保存，不下发给浏览器。</p>
 <button class="btn btn-sm btn-primary" data-add data-icon="plus" data-tip="添加 OAuth 应用">添加</button><p data-error role="alert" class="login-error"></p><div data-list>读取中…</div>`);
    d.id = "dlg-git-oauth-apps";
    const load = async () => {
        const apps = await api("/git/oauth/apps");
        if (!d.open || token !== S.token)
            return;
        const list = d.querySelector("[data-list]");
        list.replaceChildren();
        if (!apps.length)
            list.textContent = "尚未注册应用。";
        for (const app of apps) {
            const row = document.createElement("div");
            row.className = "git-connection-row";
            const title = document.createElement("strong");
            title.textContent = app.label + (app.enabled ? " · 启用" : " · 停用");
            const info = document.createElement("p");
            info.className = "field-hint";
            info.textContent = `${app.provider} · ${app.base_url}\n${app.redirect_url}`;
            const edit = document.createElement("button");
            edit.className = "btn btn-sm";
            actionButton(edit, "编辑", "rename", "编辑 OAuth 应用");
            edit.addEventListener("click", () => editApp(app, load));
            row.append(title, info, edit);
            list.append(row);
        }
    };
    d.querySelector("[data-add]").addEventListener("click", () => editApp(null, load));
    void load().catch(e => { d.querySelector("[data-error]").textContent = e.message; });
}
function editApp(app, done) {
    const token = S.token, d = modal(app ? "编辑 OAuth 应用" : "添加 OAuth 应用", `<form>
 <label>显示名称<input name="label" type="text" required maxlength="128"></label>
 <label>平台<select name="provider"><option value="github">GitHub / GitHub Enterprise</option><option value="gitlab">GitLab / 自建 GitLab</option></select></label>
 <label>服务地址<input name="base_url" type="url" required></label>
 <label>网络路由<select name="route"><option value="">服务端直连</option><option value="tunnel">授权用户的内网隧道</option></select></label>
 <label>公司 CA 证书（可选）<textarea name="ca_pem" rows="3" spellcheck="false"></textarea></label>
 <p class="field-hint">隧道模式下，授权、续期、撤销和 Git 传输均使用授权用户自己的在线隧道；浏览器也需能打开平台授权页。</p>
 <label>Client ID<input name="client_id" type="text" required maxlength="256" autocomplete="off"></label>
 <label>Client Secret${app ? "（留空保留）" : ""}<input name="client_secret" type="password" ${app ? "" : "required"} autocomplete="new-password" maxlength="8192"></label>
 <label>回调地址<input name="redirect_url" type="url" required></label>
 <label class="check"><input name="enabled" type="checkbox" checked>启用应用</label>
 <p class="field-hint">停用会阻止新的授权与后续凭证使用。服务地址、Client ID、回调地址创建后固定；更换这些信息请注册新应用。域名需与用户实际访问 Agentbox 的域名一致。</p>
 <p data-error role="alert" class="login-error"></p><div class="dlg-actions"><button type="submit" class="btn btn-primary" data-icon="save">保存</button></div></form>`);
    d.id = "dlg-git-oauth-app-edit";
    const form = d.querySelector("form"), field = (name) => form.elements.namedItem(name);
    const provider = form.elements.namedItem("provider");
    field("label").value = app?.label || "";
    setSelectValue(provider, app?.provider || "github");
    field("base_url").value = app?.base_url || "https://github.com";
    field("client_id").value = app?.client_id || "";
    field("redirect_url").value = app?.redirect_url || location.origin + "/api/git/oauth/callback";
    field("enabled").checked = app?.enabled ?? true;
    setSelectValue(form.elements.namedItem("route"), app?.network?.route || "");
    field("ca_pem").value = app?.network?.ca_pem || "";
    if (app) {
        form.elements.namedItem("route").disabled = true;
        field("ca_pem").disabled = true;
        provider.disabled = true;
        for (const name of ["base_url", "client_id", "redirect_url"])
            field(name).disabled = true;
    }
    provider.addEventListener("change", () => { field("base_url").value = provider.value === "gitlab" ? "https://gitlab.com" : "https://github.com"; });
    let busy = false;
    const save = d.querySelector('[type="submit"]'), close = d.querySelector("[data-close]");
    d.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        if (busy || token !== S.token)
            return;
        busy = true;
        save.disabled = close.disabled = true;
        try {
            await api("/git/oauth/apps", { method: "PUT", body: JSON.stringify({ id: app?.id || "", revision: app?.revision || 0, label: field("label").value, provider: provider.value, base_url: field("base_url").value, client_id: field("client_id").value, client_secret: field("client_secret").value, redirect_url: field("redirect_url").value, enabled: field("enabled").checked, network: { route: form.elements.namedItem("route").value, ca_pem: field("ca_pem").value } }) });
            field("client_secret").value = "";
            d.close();
            if (token === S.token) {
                toast("OAuth 应用已保存");
                await done();
            }
        }
        catch (e) {
            if (d.open)
                d.querySelector("[data-error]").textContent = e.message;
            else if (token === S.token)
                toast(e.message, true);
        }
        finally {
            busy = false;
            save.disabled = close.disabled = false;
        }
    });
}
