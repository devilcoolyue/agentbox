import { api } from "./api.js";
import { S, bus } from "./state.js";
import { toast } from "./util.js";
import { decorateIcons } from "./icons.js";
let active = null;
export function openGitShares(c, saved) {
    if (S.role !== "admin" || active)
        return;
    const token = S.token, d = document.createElement("dialog");
    active = d;
    d.id = "dlg-git-shares";
    d.className = "dlg-git-connections";
    d.innerHTML = `<div class="dlg-head"><h2></h2><button class="dlg-x" data-close aria-label="关闭" data-icon="close"></button></div>
 <p class="field-hint">仅对明确选中的用户开放连接。只读用户不能推送或创建 PR/MR；连接本身设为只读时，所有用户都只读。私钥和 Token 不向用户提供，个人 OAuth 不共享。</p>
 <p class="field-hint">撤权阻止后续请求，已经发到上游的操作可能继续完成。用户使用各自隧道，不会借用管理员内网。</p>
 <div data-users>读取用户中…</div><p data-error role="alert" class="login-error"></p><div class="dlg-actions"><button class="btn btn-primary" data-save disabled data-icon="save" data-tip="保存 Git 连接的用户使用授权">保存</button></div>`;
    d.querySelector("h2").textContent = c.label + " · 使用授权";
    const error = d.querySelector("[data-error]"), save = d.querySelector("[data-save]"), close = d.querySelector("[data-close]");
    let revision = 0, busy = false;
    const picks = new Map();
    close.addEventListener("click", () => d.close());
    d.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    d.addEventListener("close", () => { active = null; d.remove(); });
    save.addEventListener("click", async () => {
        if (busy || token !== S.token)
            return;
        busy = true;
        save.disabled = close.disabled = true;
        error.textContent = "";
        const users = [];
        for (const [user, p] of picks)
            if (p.enabled.checked)
                users.push({ user, write: p.write.checked });
        try {
            await api(`/git/connections/${c.id}/shares`, { method: "PUT", body: JSON.stringify({ revision, users }) });
            d.close();
            if (token === S.token) {
                toast("共享授权已保存");
                await saved();
            }
        }
        catch (e) {
            if (d.open)
                error.textContent = e.message;
            else if (token === S.token)
                toast(e.message, true);
        }
        finally {
            busy = false;
            save.disabled = close.disabled = false;
        }
    });
    document.body.append(d);
    decorateIcons(d);
    d.showModal();
    void Promise.all([api("/users"), api(`/git/connections/${c.id}/shares`)]).then(([users, access]) => {
        if (!d.open || token !== S.token)
            return;
        revision = access.revision;
        const list = d.querySelector("[data-users]");
        list.replaceChildren();
        for (const user of users) {
            if (user.name === c.owner)
                continue;
            const row = document.createElement("div");
            row.className = "git-connection-row git-connection-tools";
            const enabled = document.createElement("input"), write = document.createElement("input");
            enabled.type = write.type = "checkbox";
            const entry = access.users.find(s => s.user === user.name);
            enabled.checked = !!entry;
            write.checked = entry?.write ?? false;
            write.disabled = !enabled.checked;
            const name = document.createElement("label");
            name.className = "check";
            name.append(enabled, document.createTextNode(user.name));
            const permission = document.createElement("label");
            permission.className = "check";
            permission.append(write, document.createTextNode("允许写入"));
            enabled.addEventListener("change", () => { write.disabled = !enabled.checked; });
            row.append(name, permission);
            list.append(row);
            picks.set(user.name, { enabled, write });
        }
        if (!picks.size)
            list.textContent = "暂无其他用户。";
        save.disabled = false;
    }).catch(e => { error.textContent = e.message; });
}
bus.addEventListener("signed-out", () => active?.close());
