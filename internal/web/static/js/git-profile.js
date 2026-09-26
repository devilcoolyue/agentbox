import { api } from "./api.js";
import { S, bus } from "./state.js";
import { $, toast } from "./util.js";
let active = null;
export async function openGitProfile() {
    if (active)
        return;
    const token = S.token;
    const dialog = document.createElement("dialog");
    active = dialog;
    dialog.id = "dlg-git-profile";
    dialog.setAttribute("aria-labelledby", "git-profile-title");
    dialog.innerHTML = `<form>
    <h2 id="git-profile-title">我的 Git 身份</h2>
    <p class="field-hint">用于你所有工作空间的网页本地提交。姓名和邮箱会写进提交历史；这不代表已授权 GitHub 或 GitLab。</p>
    <fieldset disabled>
      <label>提交姓名<input name="name" type="text" required maxlength="128" autocomplete="name"></label>
      <label>提交邮箱<input name="email" type="email" required maxlength="254" autocomplete="email"></label>
    </fieldset>
    <p role="status" class="field-hint">读取中…</p>
    <p role="alert" class="login-error"></p>
    <div class="dlg-actions"><button type="button" class="btn btn-ghost" data-cancel>取消</button><button type="submit" class="btn btn-primary" disabled>保存</button></div>
  </form>`;
    const form = dialog.querySelector("form");
    const name = form.elements.namedItem("name");
    const email = form.elements.namedItem("email");
    const fields = dialog.querySelector("fieldset");
    const save = dialog.querySelector('[type="submit"]');
    const cancel = dialog.querySelector("[data-cancel]");
    const status = dialog.querySelector('[role="status"]');
    const error = dialog.querySelector('[role="alert"]');
    let busy = false;
    cancel.addEventListener("click", () => dialog.close());
    dialog.addEventListener("cancel", e => { if (busy)
        e.preventDefault(); });
    dialog.addEventListener("close", () => { dialog.remove(); if (active === dialog)
        active = null; });
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        if (busy || save.disabled || token !== S.token)
            return;
        busy = true;
        save.disabled = cancel.disabled = fields.disabled = true;
        error.textContent = "";
        try {
            await api("/me/git", { method: "PUT", body: JSON.stringify({ name: name.value.trim(), email: email.value.trim() }) });
            if (token !== S.token)
                return;
            dialog.close();
            toast("Git 身份已保存，下一次网页提交生效");
        }
        catch (e) {
            if (token === S.token)
                error.textContent = e.message;
        }
        finally {
            busy = false;
            save.disabled = cancel.disabled = fields.disabled = false;
        }
    });
    document.body.append(dialog);
    dialog.showModal();
    try {
        const profile = await api("/me/git");
        if (token !== S.token || !dialog.open)
            return;
        name.value = profile.name;
        email.value = profile.email;
        status.textContent = profile.email.endsWith("@localhost") ? "当前为本地身份；如需在代码托管平台关联提交，请填写该平台已验证的邮箱或隐私邮箱。" : "更改身份不会改写已有提交，也不会修改终端的 Git 配置。";
        save.disabled = fields.disabled = false;
        name.focus();
    }
    catch (e) {
        if (token === S.token) {
            status.textContent = "";
            error.textContent = e.message;
        }
    }
}
$("btn-git-profile").addEventListener("click", () => void openGitProfile());
$("btn-changes-profile").addEventListener("click", () => void openGitProfile());
bus.addEventListener("signed-out", () => active?.close());
