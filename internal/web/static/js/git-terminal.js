import { htmlText as trHTML, setText, setTextRender, t as i18nText } from "./i18n.js";
import { api } from "./api.js";
import { S, bus } from "./state.js";
import { askConfirm, fmtTime } from "./util.js";
import { gitRequest } from "./git-operations.js";
import { actionButton, decorateIcons } from "./icons.js";
const dialogs = new Set();
export function openGitTerminal(repo, remote, readOnly) {
    if (!S.current)
        return;
    const session = S.current.id, token = S.token, prefix = `/sessions/${session}/git/terminal`, d = document.createElement("dialog");
    d.id = "dlg-git-terminal";
    d.className = "dlg-git-connections";
    d.innerHTML = `<div class="dlg-head"><h2><span data-i18n="终端 Git 授权">${trHTML("终端 Git 授权")}</span></h2><button class="dlg-x" data-close aria-label="${trHTML("关闭")}" data-icon="close" data-i18n-attrs="{&quot;aria-label&quot;:&quot;关闭&quot;}"></button></div><div class="dlg-body">
 <p data-target class="field-hint"></p><p class="field-hint"><span data-i18n="授权有效期 30 分钟，仅用于当前仓库和 remote。请在终端运行下方命令；原生 git push 不会自动使用此授权。">${trHTML("授权有效期 30 分钟，仅用于当前仓库和 remote。请在终端运行下方命令；原生 git push 不会自动使用此授权。")}</span></p>
 <label class="check"><input type="checkbox" data-write><span data-i18n="允许推送">${trHTML("允许推送")}</span></label>
 <p class="field-hint"><span data-i18n="默认允许查看状态、获取更新与快进拉取。授权后，空间内的用户程序和 Agent 都能使用；开启推送代表允许它们写入此远程仓库。长期 Token 和 SSH 私钥保留在服务端。">${trHTML("默认允许查看状态、获取更新与快进拉取。授权后，空间内的用户程序和 Agent 都能使用；开启推送代表允许它们写入此远程仓库。长期 Token 和 SSH 私钥保留在服务端。")}</span></p>
 <div class="dlg-actions"><button class="btn btn-primary btn-sm" data-create data-icon="key" data-tip="${trHTML("创建当前仓库的终端授权，有效期 30 分钟")}" data-i18n-attrs="{&quot;data-tip&quot;:&quot;创建当前仓库的终端授权，有效期 30 分钟&quot;}"><span class="action-label" data-i18n="创建授权">${trHTML("创建授权")}</span></button><button class="btn btn-sm" data-refresh data-icon="refresh"><span class="action-label" data-i18n="刷新">${trHTML("刷新")}</span></button></div>
 <p data-error class="login-error" role="alert"></p><div data-list></div></div>`;
    const write = d.querySelector("[data-write]"), error = d.querySelector("[data-error]"), list = d.querySelector("[data-list]");
    write.disabled = readOnly;
    setTextRender(d.querySelector("[data-target]"), () => `${repo || i18nText("空间文件根目录")} · ${remote}`);
    let busy = false;
    async function load() {
        const grants = await api(prefix);
        if (!d.open || token !== S.token)
            return;
        list.replaceChildren();
        const selected = grants.filter(g => g.repo === repo && g.remote === remote);
        if (!selected.length)
            setText(list, "当前没有终端授权。");
        for (const g of selected) {
            const row = document.createElement("div");
            row.className = "git-connection-row";
            const label = document.createElement("p");
            label.className = "field-hint";
            setTextRender(label, () => i18nText("{p0} · 到期 {p1}", { p0: String(g.write ? i18nText("允许推送") : i18nText("只读远程")), p1: String(fmtTime(Date.parse(g.expires_at))) }));
            const commands = document.createElement("pre");
            commands.style.whiteSpace = "pre-wrap";
            commands.style.overflowWrap = "anywhere";
            commands.textContent = `${g.command} status\n${g.command} fetch\n${g.command} pull` + (g.write ? `\n${g.command} push` : "");
            const revoke = document.createElement("button");
            revoke.className = "btn btn-sm";
            actionButton(revoke, () => i18nText("撤销"), "shield-off", () => i18nText("撤销授权"));
            revoke.addEventListener("click", () => void run(async () => { await api(prefix + "/" + g.id, { method: "DELETE" }); await load(); }));
            row.append(label, commands, revoke);
            list.append(row);
        }
    }
    async function run(fn) {
        if (busy || token !== S.token)
            return;
        busy = true;
        error.textContent = "";
        for (const b of d.querySelectorAll("button"))
            b.disabled = true;
        try {
            await fn();
        }
        catch (e) {
            if (d.open && token === S.token)
                error.textContent = e.message;
        }
        finally {
            busy = false;
            for (const b of d.querySelectorAll("button"))
                b.disabled = false;
        }
    }
    d.querySelector("[data-create]").addEventListener("click", () => void run(async () => {
        if (write.checked && !await askConfirm(() => i18nText("允许空间内的程序推送此仓库？"), { get title() { return i18nText("终端推送授权"); }, get hint() { return i18nText("30 分钟内可推送当前仓库的分支，不允许强制推送或删除远程分支。命令的确认提示不能阻止空间内的其他程序使用授权。"); }, get okLabel() { return i18nText("创建授权"); }, icon: "key" }))
            return;
        if (!d.open || token !== S.token)
            return;
        await gitRequest(prefix, { repo, remote, write: write.checked }, d);
        await load();
    }));
    d.querySelector("[data-refresh]").addEventListener("click", () => void run(load));
    d.querySelector("[data-close]").addEventListener("click", () => d.close());
    d.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    d.addEventListener("close", () => { dialogs.delete(d); d.remove(); });
    document.body.append(d);
    dialogs.add(d);
    decorateIcons(d);
    d.showModal();
    void run(load);
}
bus.addEventListener("signed-out", () => { for (const d of dialogs)
    d.close(); });
