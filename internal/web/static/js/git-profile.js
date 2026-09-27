import { api } from "./api.js";
import { S, bus } from "./state.js";
import { $, toast } from "./util.js";
import { openGitManagement } from "./git-management.js";
import { decorateIcons } from "./icons.js";
export function openGitProfile() { openGitManagement("profile"); }
async function renderGitProfile(host) {
    const token = S.token;
    const section = document.createElement("section");
    section.className = "git-surface";
    section.id = "git-profile";
    section.innerHTML = `<form>
    <div class="sec-intro"><div><h2>提交身份</h2><p>设置提交历史中显示的姓名和邮箱。</p></div></div>
    <p class="field-hint">用于你所有工作空间的网页本地提交。姓名和邮箱会写进提交历史；这不代表已授权 GitHub 或 GitLab。</p>
    <fieldset disabled>
      <label>提交姓名<input name="name" type="text" required maxlength="128" autocomplete="name"></label>
      <label>提交邮箱<input name="email" type="email" required maxlength="254" autocomplete="email"></label>
    </fieldset>
    <p role="status" class="field-hint">读取中…</p>
    <p role="alert" class="login-error"></p>
    <div class="dlg-actions"><button type="button" class="btn btn-ghost" data-reload data-icon="refresh">重新读取</button><button type="submit" class="btn btn-primary" disabled data-icon="save">保存</button></div>
  </form>`;
    const form = section.querySelector("form");
    const name = form.elements.namedItem("name");
    const email = form.elements.namedItem("email");
    const fields = section.querySelector("fieldset");
    const save = section.querySelector('[type="submit"]');
    const reload = section.querySelector("[data-reload]");
    const status = section.querySelector('[role="status"]');
    const error = section.querySelector('[role="alert"]');
    let busy = false;
    reload.addEventListener("click", () => { if (!busy)
        openGitProfile(); });
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        if (busy || save.disabled || token !== S.token)
            return;
        busy = true;
        save.disabled = reload.disabled = fields.disabled = true;
        error.textContent = "";
        try {
            await api("/me/git", { method: "PUT", body: JSON.stringify({ name: name.value.trim(), email: email.value.trim() }) });
            if (token !== S.token || !section.isConnected)
                return;
            status.textContent = "已保存。下一次网页提交使用此身份；已有提交和终端配置不变。";
            toast("Git 身份已保存，下一次网页提交生效");
        }
        catch (e) {
            if (token === S.token)
                error.textContent = e.message;
        }
        finally {
            busy = false;
            save.disabled = reload.disabled = fields.disabled = false;
        }
    });
    host.append(section);
    decorateIcons(section);
    try {
        const profile = await api("/me/git");
        if (token !== S.token || !section.isConnected)
            return;
        name.value = profile.name;
        email.value = profile.email;
        status.textContent = profile.email.endsWith("@localhost") ? "当前为本地身份；如需在代码托管平台关联提交，请填写该平台已验证的邮箱或隐私邮箱。" : "更改身份不会改写已有提交，也不会修改终端的 Git 配置。";
        save.disabled = fields.disabled = false;
    }
    catch (e) {
        if (token === S.token) {
            status.textContent = "";
            error.textContent = e.message;
        }
    }
}
$("btn-changes-profile").addEventListener("click", openGitProfile);
bus.addEventListener("git-section", e => {
    const { section, host } = e.detail;
    if (section === "profile")
        void renderGitProfile(host);
});
