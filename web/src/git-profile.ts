import { htmlText as trHTML, setText, setTextRender, t as i18nText } from "./i18n.js";
import { api } from "./api.js";
import { S, bus } from "./state.js";
import type { GitProfile } from "./types.js";
import { $, toast } from "./util.js";
import { openGitManagement } from "./git-management.js";
import { decorateIcons } from "./icons.js";

export function openGitProfile() { openGitManagement("profile"); }

async function renderGitProfile(host: HTMLElement) {
  const token = S.token;
  const section = document.createElement("section");
  section.className = "git-surface";
  section.id = "git-profile";
  section.innerHTML = `<form>
    <div class="sec-intro"><div><h2><span data-i18n="提交身份">${trHTML("提交身份")}</span></h2><p><span data-i18n="设置提交历史中显示的姓名和邮箱。">${trHTML("设置提交历史中显示的姓名和邮箱。")}</span></p></div></div>
    <p class="field-hint"><span data-i18n="用于你所有工作空间的网页本地提交。姓名和邮箱会写进提交历史；这不代表已授权 GitHub 或 GitLab。">${trHTML("用于你所有工作空间的网页本地提交。姓名和邮箱会写进提交历史；这不代表已授权 GitHub 或 GitLab。")}</span></p>
    <fieldset disabled>
      <label><span data-i18n="提交姓名">${trHTML("提交姓名")}</span><input name="name" type="text" required maxlength="128" autocomplete="name"></label>
      <label><span data-i18n="提交邮箱">${trHTML("提交邮箱")}</span><input name="email" type="email" required maxlength="254" autocomplete="email"></label>
    </fieldset>
    <p role="status" class="field-hint"><span data-i18n="读取中…">${trHTML("读取中…")}</span></p>
    <p role="alert" class="login-error"></p>
    <div class="dlg-actions"><button type="button" class="btn btn-ghost" data-reload data-icon="refresh"><span class="action-label" data-i18n="重新读取">${trHTML("重新读取")}</span></button><button type="submit" class="btn btn-primary" disabled data-icon="save"><span class="action-label" data-i18n="保存">${trHTML("保存")}</span></button></div>
  </form>`;
  const form = section.querySelector("form")!;
  const name = form.elements.namedItem("name") as HTMLInputElement;
  const email = form.elements.namedItem("email") as HTMLInputElement;
  const fields = section.querySelector("fieldset")!;
  const save = section.querySelector<HTMLButtonElement>('[type="submit"]')!;
  const reload = section.querySelector<HTMLButtonElement>("[data-reload]")!;
  const status = section.querySelector<HTMLElement>('[role="status"]')!;
  const error = section.querySelector<HTMLElement>('[role="alert"]')!;
  let busy = false;
  reload.addEventListener("click", () => { if (!busy) openGitProfile(); });
  form.addEventListener("submit", async e => {
    e.preventDefault();
    if (busy || save.disabled || token !== S.token) return;
    busy = true;
    save.disabled = reload.disabled = fields.disabled = true;
    error.textContent = "";
    try {
      await api<GitProfile>("/me/git", { method: "PUT", body: JSON.stringify({ name: name.value.trim(), email: email.value.trim() }) });
      if (token !== S.token || !section.isConnected) return;
      setText(status, "已保存。下一次网页提交使用此身份；已有提交和终端配置不变。");
      toast(i18nText("Git 身份已保存，下一次网页提交生效"));
    } catch (e) {
      if (token === S.token) error.textContent = (e as Error).message;
    } finally {
      busy = false;
      save.disabled = reload.disabled = fields.disabled = false;
    }
  });
  host.append(section);
  decorateIcons(section);

  try {
    const profile = await api<GitProfile>("/me/git");
    if (token !== S.token || !section.isConnected) return;
    name.value = profile.name;
    email.value = profile.email;
    setTextRender(status, () => profile.email.endsWith("@localhost") ? i18nText("当前为本地身份；如需在代码托管平台关联提交，请填写该平台已验证的邮箱或隐私邮箱。") : i18nText("更改身份不会改写已有提交，也不会修改终端的 Git 配置。"));
    save.disabled = fields.disabled = false;

  } catch (e) {
    if (token === S.token) { status.textContent = ""; error.textContent = (e as Error).message; }
  }
}

$("btn-changes-profile").addEventListener("click", openGitProfile);
bus.addEventListener("git-section", e => {
  const { section, host } = (e as CustomEvent<{ section: string; host: HTMLElement }>).detail;
  if (section === "profile") void renderGitProfile(host);
});
