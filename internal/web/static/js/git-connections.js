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
import { enhanceSelects, setSelectValue } from "./select.js";
const dialogs = new Set();
function dialog(title, markup) {
    const d = document.createElement("dialog");
    d.className = "dlg-git-connections";
    d.innerHTML = `<div class="dlg-head"><h2></h2><button type="button" class="dlg-x" aria-label="关闭" data-close data-icon="close"></button></div>` + markup;
    d.querySelector("h2").textContent = title;
    d.querySelector("[data-close]").addEventListener("click", () => d.close());
    d.addEventListener("close", () => { dialogs.delete(d); d.remove(); });
    document.body.append(d);
    dialogs.add(d);
    decorateIcons(d);
    enhanceSelects(d);
    d.showModal();
    return d;
}
function errorText(d, error) { d.querySelector("[data-error]").textContent = error.message; }
function option(value, label) { return Object.assign(document.createElement("option"), { value, textContent: label }); }
function action(label, icon, run, tip = label) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "btn btn-sm";
    actionButton(b, label, icon, tip);
    b.addEventListener("click", async () => { b.disabled = true; try {
        await run();
    }
    catch (e) {
        toast(e.message, true);
    }
    finally {
        b.disabled = false;
    } });
    return b;
}
export function openGitConnections() { openGitManagement("connections"); }
function renderGitConnections(host) {
    const token = S.token;
    const d = document.createElement("section");
    d.className = "git-surface";
    d.id = "git-connections";
    d.innerHTML = `<div class="sec-intro"><div><h2>仓库连接</h2><p>连接可在你的多个工作空间复用。添加后，到空间「变更 → 远程」绑定仓库。</p></div></div>
    <div class="git-connection-tools"><button class="btn btn-primary btn-sm" data-add data-icon="plus">添加连接</button><button class="btn btn-sm" data-oauth data-icon="login">网页授权</button><button class="btn btn-sm" data-oauth-apps data-icon="key">OAuth 应用</button><button class="btn btn-sm" data-refresh data-icon="refresh">刷新</button><button class="btn btn-sm" data-operations data-icon="history">操作记录</button></div>
    <p class="git-note">默认连接用于新空间和未绑定仓库的默认选择；已有绑定保持不变。保存连接后可先「测试」仓库读取权限。</p>
    <p data-error role="alert" class="login-error"></p><div data-list aria-live="polite">读取中…</div>`;
    host.append(d);
    decorateIcons(d);
    let generation = 0;
    const load = async () => {
        const gen = ++generation;
        d.querySelector("[data-error]").textContent = "";
        const [connections, def] = await Promise.all([api("/git/connections"), api("/me/git/default")]);
        if (!d.isConnected || token !== S.token || gen !== generation)
            return;
        const list = d.querySelector("[data-list]");
        list.replaceChildren();
        if (!connections.length)
            list.innerHTML = '<div class="git-empty"><h3>尚未添加 Git 连接</h3><p>已有 Token 或 SSH 私钥？点击「添加连接」。也可以通过「网页授权」登录代码平台。</p><p>只做本地提交时，无需添加连接。</p></div>';
        for (const c of connections) {
            const managed = c.managed ?? c.owner === S.user;
            const row = document.createElement("div");
            row.className = "git-connection-row";
            const title = document.createElement("strong");
            title.textContent = c.label + (!managed ? " · 共享自 " + c.owner : "") + (def.connection_id === c.id ? " · 默认" : "");
            const meta = document.createElement("p");
            meta.className = "field-hint";
            meta.textContent = `${c.provider} · ${c.auth_type === "oauth" ? "OAuth" : c.auth_type === "ssh" ? "SSH" : "Token"} · ${c.base_url}\n${c.enabled ? "已保存" : "已停用"} · ${c.read_only ? "只读" : "允许推送"} · ${c.network?.route === "tunnel" ? "内网隧道" : "直连"}${c.host_fingerprint ? "\n主机指纹：" + c.host_fingerprint : ""}`;
            // 行内只露三个常用动作；启停、授权、公钥这些低频动作与删除收进 ⋯，删除放最后。
            const buttons = document.createElement("div");
            buttons.className = "git-connection-tools";
            buttons.append(action("测试", "activity", async () => {
                const url = await askPrompt({ title: "测试 Git 连接", label: "仓库地址", value: c.base_url + "/", hint: "验证此仓库可读取；不会推送或证明写权限。" });
                if (url === null || token !== S.token)
                    return;
                await gitRequest(`/git/connections/${c.id}/test`, { url }, d);
                toast("仓库可读取；写权限需在实际推送时由上游校验");
            }, "测试仓库读取权限"), action(def.connection_id === c.id ? "取消默认" : "设为默认", def.connection_id === c.id ? "undo" : "check", async () => {
                await api("/me/git/default", { method: "PUT", body: JSON.stringify({ connection_id: def.connection_id === c.id ? "" : c.id }) });
                await load();
            }));
            if (managed)
                buttons.append(action("编辑", "rename", async () => editConnection(c, load), "编辑连接"));
            const later = (run) => () => { run().catch(e => toast(e.message, true)); };
            const items = [
                { label: c.enabled ? "停用" : "启用", icon: c.enabled ? "stop" : "play", hidden: !managed, run: later(async () => {
                        await api(`/git/connections/${c.id}`, { method: "PATCH", body: JSON.stringify({ revision: c.revision, enabled: !c.enabled }) });
                        await load();
                    }) },
                { label: "使用授权", icon: "users", hidden: !(managed && S.role === "admin" && c.auth_type !== "oauth"), run: later(async () => openGitShares(c, load)),
                    tip: "允许其他用户使用这个连接" },
                { label: "查看公钥", icon: "key", hidden: !c.public_key, run: () => {
                        const key = createGitSurface("连接公钥", '<p class="field-hint">将此公钥登记到上游 Git 服务账号或部署密钥；私钥不提供导出。</p><pre data-public-key></pre>');
                        key.querySelector("[data-public-key]").textContent = c.public_key;
                    } },
                { label: "重新授权", icon: "login", hidden: !(managed && c.auth_type === "oauth"), run: later(async () => openGitOAuth(c)) },
                { label: "撤销授权", icon: "shield-off", danger: true, hidden: !(managed && c.auth_type === "oauth"), run: later(async () => {
                        if (!await askConfirm(`撤销「${c.label}」的 OAuth 授权？`, { title: "撤销 Git 授权", hint: "会先停用本地连接，再请求上游撤销。上游可能同时影响使用同一应用授权的其他连接。", danger: true, okLabel: "撤销", icon: "shield-off" }))
                            return;
                        const result = await gitRequest(`/git/connections/${c.id}/revoke`, { revision: c.revision }, d);
                        toast(result.warning || "本地已停用，上游授权已撤销", !result.remote_revoked);
                        await load();
                    }) },
                { label: "删除连接", icon: "trash", danger: true, sep: true, hidden: !managed, run: later(async () => {
                        if (!await askConfirm(`删除连接「${c.label}」？`, { title: "删除 Git 连接", hint: "已绑定的连接需先解除引用。删除不会撤销上游平台的 Token。", danger: true, okLabel: "删除", icon: "trash" }))
                            return;
                        await api(`/git/connections/${c.id}`, { method: "DELETE", body: JSON.stringify({ revision: c.revision }) });
                        await load();
                    }) },
            ];
            if (items.some(item => !item.hidden))
                buttons.append(moreButton(() => items, `${c.label} 的更多操作`));
            row.append(title, meta, buttons);
            list.append(row);
        }
    };
    d.querySelector("[data-oauth]").addEventListener("click", () => openGitOAuth());
    d.querySelector("[data-oauth-apps]").classList.toggle("hidden", S.role !== "admin");
    d.querySelector("[data-oauth-apps]").addEventListener("click", openGitOAuthApps);
    d.querySelector("[data-operations]").addEventListener("click", openGitOperations);
    d.querySelector("[data-add]").addEventListener("click", () => editConnection(null, load));
    d.querySelector("[data-refresh]").addEventListener("click", () => { void load().catch(e => errorText(d, e)); });
    void load().catch(e => errorText(d, e));
}
function editConnection(c, saved) {
    const token = S.token;
    const d = createGitSurface(c ? "编辑 Git 连接" : "添加 Git 连接", `<form>
    <label>认证方式<select name="auth_type"><option value="pat">HTTPS Token</option><option value="ssh">SSH 私钥</option></select></label>
    <label>连接名称<input type="text" name="label" maxlength="128" required placeholder="例如：公司 GitLab"></label>
    <div class="dlg-row"><label>平台<select name="provider"><option value="github">GitHub</option><option value="gitlab">GitLab / 自建 GitLab</option><option value="generic">其他 HTTPS Git</option></select></label>
    <label>服务地址<input type="url" name="base_url" required placeholder="https://git.example.com"></label></div>
    <label>Git 用户名<input type="text" name="username" maxlength="256" placeholder="GitHub / GitLab 留空可使用默认值"></label>
    <label data-token-row>${c ? "替换 Token（留空保留）" : "Personal Access Token"}<input type="password" name="token" autocomplete="new-password" maxlength="16384" ${c ? "" : "required"}></label>
    <div data-ssh class="hidden"><label>SSH 私钥<textarea name="private_key" rows="4" autocomplete="off" spellcheck="false" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></label>
    <label>私钥口令（如有）<input type="password" name="passphrase" autocomplete="new-password"></label>
    <label>服务器主机公钥<textarea name="host_key" rows="2" spellcheck="false" placeholder="ssh-ed25519 AAAA…"></textarea></label>
    <p class="field-hint">向 Git 服务管理员核对主机公钥，不能填个人公钥。私钥只在服务端使用，不复制到工作空间。修改已有连接时留空保留密钥。替换主机公钥前，请从管理员处重新核实指纹。</p></div>
    <label>网络路由<select name="route"><option value="">服务端直连</option><option value="tunnel">我的内网隧道</option></select></label>
    <label data-ca-row>公司 CA 证书（可选）<textarea name="ca_pem" rows="3" spellcheck="false" placeholder="PEM 根 CA / 中间 CA；留空使用系统信任库"></textarea></label>
    <p class="field-hint">隧道需在线并放行服务域名和端口，断开时不会改为直连。路由与信任信息保存后固定，调整需新建连接。</p>
    <label class="check"><input type="checkbox" name="read_only" checked>只允许读取仓库</label>
    <p class="field-hint">使用有目标仓库权限的 Token。推送需同时具有上游写权限。服务地址和用户名保存后不可修改；更换目标请创建新连接。</p>
    <p data-error role="alert" class="login-error"></p><div class="dlg-actions"><button type="submit" class="btn btn-primary" data-icon="save">保存</button></div>
  </form>`);
    d.id = "dlg-git-connection-edit";
    const form = d.querySelector("form");
    const field = (name) => form.elements.namedItem(name);
    const provider = form.elements.namedItem("provider");
    const save = d.querySelector('[type="submit"]');
    const auth = form.elements.namedItem("auth_type"), route = form.elements.namedItem("route");
    if (c?.auth_type === "oauth")
        auth.append(option("oauth", "OAuth"));
    setSelectValue(auth, c?.auth_type || "pat");
    setSelectValue(route, c?.network?.route || "");
    field("ca_pem").value = c?.network?.ca_pem || "";
    const syncAuth = () => { const ssh = auth.value === "ssh"; d.querySelector("[data-ssh]").classList.toggle("hidden", !ssh); d.querySelector("[data-token-row]").classList.toggle("hidden", ssh); d.querySelector("[data-ca-row]").classList.toggle("hidden", ssh); field("token").required = !c && !ssh; field("private_key").required = field("host_key").required = !c && ssh; };
    auth.addEventListener("change", () => { field("base_url").value = auth.value === "ssh" ? "ssh://github.com" : "https://github.com"; field("username").value = auth.value === "ssh" ? "git" : ""; syncAuth(); });
    if (c) {
        auth.disabled = route.disabled = field("ca_pem").disabled = true;
        field("private_key").placeholder = "留空保留现有私钥";
        field("host_key").placeholder = c.host_fingerprint || "已保存主机公钥";
    }
    syncAuth();
    field("base_url").value = c?.base_url || "https://github.com";
    setSelectValue(provider, c?.provider || "github");
    field("label").value = c?.label || "";
    field("username").value = c?.username || "";
    field("read_only").checked = c?.read_only ?? true;
    if (c)
        provider.disabled = field("base_url").disabled = field("username").disabled = true;
    if (c?.auth_type === "oauth") {
        field("token").disabled = true;
        field("token").placeholder = "OAuth 连接请通过授权入口重新连接";
    }
    provider.addEventListener("change", () => { field("base_url").value = (auth.value === "ssh" ? "ssh://" : "https://") + (provider.value === "github" ? "github.com" : provider.value === "gitlab" ? "gitlab.com" : ""); });
    let busy = false;
    d.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        if (busy || token !== S.token)
            return;
        busy = true;
        save.disabled = true;
        d.querySelector("[data-close]").disabled = true;
        d.querySelector("[data-error]").textContent = "";
        const body = c
            ? { revision: c.revision, label: field("label").value.trim(), read_only: field("read_only").checked, ...(field("token").value ? { token: field("token").value } : {}), ...(c.auth_type === "ssh" && field("private_key").value ? { private_key: field("private_key").value, passphrase: field("passphrase").value } : {}), ...(c.auth_type === "ssh" && field("host_key").value ? { host_key: field("host_key").value } : {}) }
            : { label: field("label").value.trim(), provider: provider.value, base_url: field("base_url").value, username: field("username").value, token: field("token").value, read_only: field("read_only").checked, auth_type: auth.value, private_key: field("private_key").value, passphrase: field("passphrase").value, host_key: field("host_key").value, network: { route: route.value, ca_pem: auth.value === "ssh" ? "" : field("ca_pem").value } };
        try {
            await api(c ? `/git/connections/${c.id}` : "/git/connections", { method: c ? "PATCH" : "POST", body: JSON.stringify(body) });
            field("token").value = field("private_key").value = field("passphrase").value = "";
            d.close();
            if (token === S.token) {
                toast("Git 连接已保存");
                await saved();
            }
        }
        catch (e) {
            if (d.open)
                errorText(d, e);
            else if (token === S.token)
                toast(e.message, true);
        }
        finally {
            busy = false;
            save.disabled = false;
            d.querySelector("[data-close]").disabled = false;
        }
    });
}
export async function openGitRemote(repo, status, refreshed) {
    const session = S.current?.id, token = S.token;
    if (!session)
        return;
    const d = dialog("仓库远程", `<p data-repo class="field-hint"></p>
    <div class="dlg-row"><label>Remote<select data-remote></select></label><label>Git 连接<select data-connection></select></label></div>
    <p data-target class="field-hint"></p><p class="field-hint">获取更新只更新远程跟踪分支，不合并工作文件。推送前会展示目标和提交；不会强制覆盖远程分支。</p>
    <div class="git-connection-tools"><button class="btn btn-sm" data-manage data-icon="link" data-tip="管理 Git 连接">连接</button><button class="btn btn-sm" data-reload data-icon="refresh" data-tip="刷新 Git 连接与远程状态">刷新</button><button class="btn btn-sm" data-add-remote data-icon="plus" data-tip="添加远程仓库地址">添加远程</button><button class="btn btn-sm" data-edit-remote data-icon="rename" data-tip="编辑所选远程仓库地址">编辑</button><button class="btn btn-sm" data-remove-remote data-icon="trash" data-tip="删除所选远程配置，不删除服务器仓库">删除</button><button class="btn btn-sm" data-bind disabled data-icon="plug" data-tip="保存当前仓库 remote 与 Git 连接的绑定">绑定</button><button class="btn btn-sm" data-default disabled data-icon="check" data-tip="设为空间默认 Git 连接">默认</button><button class="btn btn-sm" data-operations data-icon="history" data-tip="查看 Git 操作记录">记录</button><button class="btn btn-sm" data-terminal disabled data-icon="key" data-tip="管理当前仓库的终端 Git 授权">终端授权</button><button class="btn btn-sm" data-reviews disabled data-icon="git-pr" data-tip="查看或创建 Pull Request / Merge Request">PR / MR</button><button class="btn btn-sm" data-fetch disabled data-icon="git-fetch" data-tip="获取远程更新，不合并工作文件（fetch）">获取</button><button class="btn btn-sm" data-pull disabled data-icon="git-pull" data-tip="获取并快进合并上游提交，更新工作文件">拉取</button><button class="btn btn-sm btn-primary" data-preview disabled data-icon="eye" data-tip="预览待推送提交，确认后才推送">预览推送</button></div>
    <p data-error role="alert" class="login-error"></p><p data-state role="status" class="field-hint"></p><div data-preview-box class="hidden"><pre data-commits></pre><button class="btn btn-primary" data-push data-icon="git-push" data-tip="确认推送已预览的提交到远程仓库">推送</button></div>`);
    d.id = "dlg-git-remote";
    const remote = d.querySelector("[data-remote]"), connection = d.querySelector("[data-connection]");
    const state = d.querySelector("[data-state]"), previewBox = d.querySelector("[data-preview-box]");
    const button = (name) => d.querySelector(`[data-${name}]`);
    const prefix = `/sessions/${session}/git`;
    let connections = [], bindings = [], inherited = "", busy = false, preview = null;
    d.querySelector("[data-repo]").textContent = repo || "空间文件根目录";
    const activeBinding = () => bindings.find(b => b.repo === repo && b.remote === remote.value);
    const sync = () => {
        const b = activeBinding(), c = connections.find(c => c.id === connection.value);
        button("bind").disabled = busy || !remote.value;
        button("add-remote").disabled = busy || !c?.enabled;
        button("edit-remote").disabled = busy || !remote.value || !!b || !c?.enabled;
        button("remove-remote").disabled = busy || !remote.value || !!b;
        button("default").disabled = busy || !!connection.value && !c?.enabled;
        button("fetch").disabled = busy || !b || b.connection_id !== c?.id || !c.enabled;
        button("pull").disabled = button("fetch").disabled;
        button("terminal").disabled = button("fetch").disabled;
        button("reviews").disabled = button("fetch").disabled || c?.provider === "generic";
        button("preview").disabled = button("fetch").disabled || !!c?.read_only;
        d.querySelector("[data-target]").textContent = (status.remotes || []).find(r => r.name === remote.value && !r.push)?.url || b?.url || "当前仓库没有 remote，请先在终端配置远程地址。";
    };
    const selected = () => { preview = null; previewBox.classList.add("hidden"); setSelectValue(connection, activeBinding()?.connection_id || inherited); sync(); };
    const load = async () => {
        const [cs, bs, sd, ud, fresh] = await Promise.all([api("/git/connections"), api(prefix + "/bindings"), api(prefix + "/default"), api("/me/git/default"), api(prefix + "/status?repo=" + encodeURIComponent(repo))]);
        if (!d.open || token !== S.token)
            return;
        connections = cs;
        bindings = bs;
        inherited = sd.connection_id || ud.connection_id;
        status = fresh;
        const previous = remote.value;
        remote.replaceChildren(...[...new Set([...(status.remotes || []).map(r => r.name), ...bs.filter(b => b.repo === repo).map(b => b.remote)])].map(r => option(r, r)));
        if (previous)
            setSelectValue(remote, previous);
        connection.replaceChildren(option("", "不绑定 / 解除绑定"), ...cs.map(c => option(c.id, c.label + (c.enabled ? (c.read_only ? " · 只读" : " · 可推送") : " · 已停用"))));
        selected();
    };
    const run = async (fn) => {
        if (busy || token !== S.token)
            return;
        busy = true;
        for (const b of d.querySelectorAll("button"))
            b.disabled = true;
        remote.disabled = connection.disabled = true;
        d.querySelector("[data-error]").textContent = "";
        try {
            await fn();
        }
        catch (e) {
            if (d.open && token === S.token)
                errorText(d, e);
        }
        finally {
            busy = false;
            for (const b of d.querySelectorAll("button"))
                b.disabled = false;
            remote.disabled = connection.disabled = false;
            sync();
        }
    };
    d.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    remote.addEventListener("change", selected);
    connection.addEventListener("change", () => { preview = null; previewBox.classList.add("hidden"); sync(); });
    button("manage").addEventListener("click", () => { d.close(); openGitConnections(); });
    button("operations").addEventListener("click", openGitOperations);
    button("reviews").addEventListener("click", () => { const c = connections.find(c => c.id === activeBinding()?.connection_id); if (c)
        openGitReviews(repo, remote.value, c, connections); });
    button("default").addEventListener("click", () => void run(async () => {
        await api(prefix + "/default", { method: "PUT", body: JSON.stringify({ connection_id: connection.value }) });
        state.textContent = connection.value ? "空间默认连接已保存；已有仓库绑定不受影响。" : "已清除空间默认，将采用用户默认建议。";
        await load();
    }));
    for (const kind of ["add", "edit", "remove"])
        button(kind + "-remote").addEventListener("click", () => void run(async () => {
            let name = remote.value;
            if (kind === "add") {
                const chosen = await askPrompt({ title: "添加远程", label: "Remote 名称", value: "origin", validate: v => /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(v) ? "" : "名称仅支持字母、数字、点、下划线和连字符" });
                if (chosen === null)
                    return;
                name = chosen;
            }
            const existing = (status.remotes || []).find(r => r.name === name && !r.push)?.url || "";
            let url = "";
            if (kind === "remove") {
                if (!await askConfirm(`删除 remote「${name}」？`, { title: "删除远程配置", hint: "移除本地远程配置和跟踪引用，不会删除服务器仓库。", danger: true, okLabel: "删除", icon: "trash" }))
                    return;
            }
            else {
                const chosen = await askPrompt({ title: kind === "add" ? "添加远程地址" : "修改远程地址", label: "仓库地址", value: existing || connections.find(c => c.id === connection.value)?.base_url + "/", hint: "地址必须属于所选连接的平台。保存地址后需单独保存绑定。" });
                if (chosen === null)
                    return;
                url = chosen;
            }
            await gitRequest(prefix + "/remotes", { repo, name, action: kind === "edit" ? "update" : kind, url, expected_url: existing, connection_id: connection.value }, d);
            state.textContent = "远程配置已保存，请核对后绑定连接。";
            await load();
            if (S.current?.id === session)
                await refreshed();
        }));
    button("reload").addEventListener("click", () => void run(load));
    button("bind").addEventListener("click", () => void run(async () => {
        bindings = await api(prefix + "/bindings", { method: "PUT", body: JSON.stringify({ repo, remote: remote.value, connection_id: connection.value, revision: activeBinding()?.revision || 0 }) });
        selected();
        state.textContent = "绑定已保存";
    }));
    button("terminal").addEventListener("click", () => openGitTerminal(repo, remote.value, !!connections.find(c => c.id === connection.value)?.read_only));
    button("fetch").addEventListener("click", () => void run(async () => {
        preview = null;
        previewBox.classList.add("hidden");
        state.textContent = "正在获取远程分支…";
        const result = await gitRequest(prefix + "/fetch", { repo, remote: remote.value }, d);
        state.textContent = "已获取最新远程分支；工作文件未合并更新。";
        if (result.fetched_at && S.current?.id === session)
            await refreshed();
    }));
    button("pull").addEventListener("click", () => void run(async () => {
        if (!await askConfirm("获取并快进合并当前分支的上游提交？", { title: "快进拉取", hint: "这会更新工作文件；需要工作区干净，分叉时停止。", okLabel: "拉取", icon: "git-pull" }))
            return;
        preview = null;
        previewBox.classList.add("hidden");
        state.textContent = "正在快进拉取…";
        await gitRequest(prefix + "/pull", { repo, remote: remote.value }, d);
        state.textContent = "已快进更新当前分支。";
        if (S.current?.id === session)
            await refreshed();
    }));
    button("preview").addEventListener("click", () => void run(async () => {
        preview = null;
        previewBox.classList.add("hidden");
        state.textContent = "正在核对远程分支…";
        preview = await gitRequest(prefix + "/push-preview", { repo, remote: remote.value }, d);
        state.textContent = `${preview.connection} → ${preview.url}\n${preview.ref}${preview.new_branch ? "（新分支）" : ""} · ${preview.expected_head.slice(0, 12)}`;
        d.querySelector("[data-commits]").textContent = "待推送提交（最多显示 20 条）：\n" + preview.commits;
        previewBox.classList.remove("hidden");
    }));
    button("push").addEventListener("click", () => void run(async () => {
        const p = preview;
        if (!p)
            return;
        preview = null;
        previewBox.classList.add("hidden");
        state.textContent = "正在推送…";
        await gitRequest(prefix + "/push", { repo: p.repo, remote: p.remote, ref: p.ref, expected_head: p.expected_head, expected_remote_head: p.expected_remote_head }, d);
        state.textContent = `已推送 ${p.expected_head.slice(0, 12)} 到 ${p.ref}`;
        if (S.current?.id === session)
            await refreshed();
    }));
    await run(load);
}
bus.addEventListener("git-section", e => {
    const { section, host } = e.detail;
    if (section === "connections")
        renderGitConnections(host);
});
bus.addEventListener("signed-out", () => { for (const d of dialogs)
    d.close(); });
export function openGitClone(done) {
    const session = S.current?.id, token = S.token;
    if (!session)
        return;
    const d = dialog("克隆仓库", `<form><label>Git 连接<select name="connection" disabled></select></label>
    <label>仓库地址<input name="url" type="text" required placeholder="https://git.example.com/team/project.git"></label>
    <label>新文件夹名称<input name="directory" type="text" required maxlength="128" placeholder="project"></label>
    <p class="field-hint">克隆到当前空间根目录下的新文件夹；不会覆盖已有目录。先在用户菜单的「Git 管理 → 仓库连接」添加连接。</p>
    <p data-error role="alert" class="login-error"></p><p data-state role="status" class="field-hint">读取连接中…</p>
    <div class="dlg-actions"><button class="btn btn-primary" type="submit" disabled data-icon="git-clone">克隆</button></div></form>`);
    d.id = "dlg-git-clone";
    const form = d.querySelector("form"), picker = form.elements.namedItem("connection");
    const field = (name) => form.elements.namedItem(name);
    const submit = d.querySelector('[type="submit"]'), close = d.querySelector("[data-close]");
    const state = d.querySelector("[data-state]");
    let busy = false;
    d.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    void Promise.all([api("/git/connections"), api(`/sessions/${session}/git/default`), api("/me/git/default")]).then(([cs, sd, ud]) => {
        if (token !== S.token || !d.open)
            return;
        picker.replaceChildren(...cs.filter(c => c.enabled).map(c => option(c.id, c.label)));
        const selected = sd.connection_id || ud.connection_id;
        if (cs.some(c => c.id === selected && c.enabled))
            setSelectValue(picker, selected);
        picker.disabled = false;
        submit.disabled = !picker.value;
        state.textContent = picker.value ? "" : "没有可用连接，请先添加 Git 连接。";
    }).catch(e => errorText(d, e));
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        if (busy || !picker.value || token !== S.token)
            return;
        busy = true;
        submit.disabled = close.disabled = true;
        state.textContent = "正在克隆并检出文件…";
        d.querySelector("[data-error]").textContent = "";
        try {
            const result = await gitRequest(`/sessions/${session}/git/clone`, { connection_id: picker.value, url: field("url").value, directory: field("directory").value }, d);
            if (token !== S.token)
                return;
            d.close();
            toast(result.warning || "仓库已克隆");
            if (S.current?.id === session)
                await done(result.repo);
        }
        catch (e) {
            if (d.open && token === S.token) {
                state.textContent = "";
                errorText(d, e);
            }
        }
        finally {
            busy = false;
            submit.disabled = close.disabled = false;
        }
    });
}
