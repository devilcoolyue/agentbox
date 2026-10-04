import { htmlText as trHTML } from "./i18n.js";
import { S, bus, emit } from "./state.js";
import { showView } from "./shell.js";
import { $ } from "./util.js";
import { decorateIcons } from "./icons.js";

export function openGitManagement(section = "guide") {
  emit("git-page-cleared");
  S.gitSec = ["guide", "profile", "connections"].includes(section) ? section : "guide";
  showView("git");
  const content = $("git-content");
  content.replaceChildren();
  content.scrollTop = 0;
  for (const button of $("git-nav").querySelectorAll<HTMLButtonElement>("[data-git-sec]")) {
    const active = button.dataset.gitSec === S.gitSec;
    button.classList.toggle("active", active);
    if (active) button.setAttribute("aria-current", "page");
    else button.removeAttribute("aria-current");
  }
  if (S.gitSec === "guide") renderGuide(content);
  else emit("git-section", { section: S.gitSec, host: content });
  content.focus({ preventScroll: true });
}

function renderGuide(host: HTMLElement) {
  host.innerHTML = `<section class="git-guide">
    <div class="sec-intro"><div><h2><span data-i18n="第一次使用，从这里开始">${trHTML("第一次使用，从这里开始")}</span></h2><p><span data-i18n="身份决定提交署名，连接用于访问远程仓库。配置一次，即可在你的多个空间使用。">${trHTML("身份决定提交署名，连接用于访问远程仓库。配置一次，即可在你的多个空间使用。")}</span></p></div></div>
    <ol class="git-steps">
      <li><div><h3><span data-i18n="填写提交身份">${trHTML("填写提交身份")}</span></h3><p><span data-i18n="填写提交时显示的姓名，以及代码平台已验证的邮箱或隐私邮箱。仅需本地提交时，完成这一步即可。">${trHTML("填写提交时显示的姓名，以及代码平台已验证的邮箱或隐私邮箱。仅需本地提交时，完成这一步即可。")}</span></p><button class="btn btn-sm" data-git-go="profile" data-icon="user"><span class="action-label" data-i18n="设置提交身份">${trHTML("设置提交身份")}</span></button></div></li>
      <li><div><h3><span data-i18n="添加仓库连接">${trHTML("添加仓库连接")}</span></h3><p><span data-i18n="选择 Token、SSH 或网页授权。需要推送代码时，关闭「只允许读取仓库」，并确保平台凭证也有写权限。">${trHTML("选择 Token、SSH 或网页授权。需要推送代码时，关闭「只允许读取仓库」，并确保平台凭证也有写权限。")}</span></p><button class="btn btn-sm" data-git-go="connections" data-icon="link"><span class="action-label" data-i18n="管理仓库连接">${trHTML("管理仓库连接")}</span></button></div></li>
      <li><div><h3><span data-i18n="在工作空间使用">${trHTML("在工作空间使用")}</span></h3><p><span data-i18n="从侧栏打开工作空间，进入「变更」。新项目选择「克隆」；已有仓库打开「远程」，选择连接并点击「绑定」。">${trHTML("从侧栏打开工作空间，进入「变更」。新项目选择「克隆」；已有仓库打开「远程」，选择连接并点击「绑定」。")}</span></p><p><span data-i18n="先「获取」检查远程更新，再按需拉取或预览推送。设置默认连接只提供默认选择，不会替换已有仓库绑定。">${trHTML("先「获取」检查远程更新，再按需拉取或预览推送。设置默认连接只提供默认选择，不会替换已有仓库绑定。")}</span></p></div></li>
    </ol>
    <section class="git-guide-section"><h2><span data-i18n="连接方式怎么选？">${trHTML("连接方式怎么选？")}</span></h2>
      <dl class="git-methods">
        <div><dt>HTTPS Token <span><span data-i18n="通用选择">${trHTML("通用选择")}</span></span></dt><dd><span data-i18n="在代码平台创建个人访问令牌（Personal Access Token），开放目标仓库的读取权限；需要推送时再开放写入权限。选择「添加连接」，填写服务地址和 Token。">${trHTML("在代码平台创建个人访问令牌（Personal Access Token），开放目标仓库的读取权限；需要推送时再开放写入权限。选择「添加连接」，填写服务地址和 Token。")}</span></dd></div>
        <div><dt><span data-i18n="网页授权">${trHTML("网页授权")}</span> <span><span data-i18n="无需粘贴 Token">${trHTML("无需粘贴 Token")}</span></span></dt><dd><span data-i18n="选择「网页授权」，生成链接后在 GitHub / GitLab 完成授权，返回连接列表点击「刷新」。若没有可用服务，请联系管理员配置 OAuth 应用，或使用 Token。">${trHTML("选择「网页授权」，生成链接后在 GitHub / GitLab 完成授权，返回连接列表点击「刷新」。若没有可用服务，请联系管理员配置 OAuth 应用，或使用 Token。")}</span></dd></div>
        <div><dt><span data-i18n="SSH 私钥">${trHTML("SSH 私钥")}</span> <span><span data-i18n="已有 SSH 配置">${trHTML("已有 SSH 配置")}</span></span></dt><dd><span data-i18n="选择「添加连接 → SSH 私钥」，填写私钥及口令（如有），并向 Git 服务管理员核对服务器主机公钥。个人公钥需提前登记到代码平台。">${trHTML("选择「添加连接 → SSH 私钥」，填写私钥及口令（如有），并向 Git 服务管理员核对服务器主机公钥。个人公钥需提前登记到代码平台。")}</span></dd></div>
      </dl>
      <p class="git-note"><span data-i18n="服务地址填写平台根地址，如">${trHTML("服务地址填写平台根地址，如")}</span> <code>https://gitlab.example.com</code><span data-i18n="；仓库地址则是">${trHTML("；仓库地址则是")}</span> <code>https://gitlab.example.com/team/project.git</code><span data-i18n="，在克隆或绑定远程时使用。">${trHTML("，在克隆或绑定远程时使用。")}</span></p>
    </section>
    <section class="git-guide-section"><h2><span data-i18n="日常操作速查">${trHTML("日常操作速查")}</span></h2>
      <dl class="git-methods">
        <div><dt><span data-i18n="提交到本地">${trHTML("提交到本地")}</span></dt><dd><span data-i18n="将选中的文件改动保存为当前仓库的一次提交。不会自动上传到代码平台。">${trHTML("将选中的文件改动保存为当前仓库的一次提交。不会自动上传到代码平台。")}</span></dd></div>
        <div><dt><span data-i18n="获取 / 拉取">${trHTML("获取 / 拉取")}</span></dt><dd><span data-i18n="「获取」只更新远程分支信息，不改工作文件。「拉取」会快进更新工作文件，需要工作区干净；分支已分叉时会停止。">${trHTML("「获取」只更新远程分支信息，不改工作文件。「拉取」会快进更新工作文件，需要工作区干净；分支已分叉时会停止。")}</span></dd></div>
        <div><dt><span data-i18n="预览推送">${trHTML("预览推送")}</span></dt><dd><span data-i18n="先核对目标仓库、分支和待推送提交，确认后点击「推送」，才会更新远程仓库。">${trHTML("先核对目标仓库、分支和待推送提交，确认后点击「推送」，才会更新远程仓库。")}</span></dd></div>
      </dl>
    </section>
    <section class="git-guide-section"><h2><span data-i18n="常见问题">${trHTML("常见问题")}</span></h2>
      <details><summary><span data-i18n="保存连接后，为什么还不能访问仓库？">${trHTML("保存连接后，为什么还不能访问仓库？")}</span></summary><p><span data-i18n="保存只记录连接配置。可在连接列表点击「测试」，输入完整仓库地址验证读取权限；随后到工作空间的「远程」绑定该连接。若失败，请检查 Token 是否过期、是否开放目标仓库权限，以及仓库地址是否属于该连接的平台。">${trHTML("保存只记录连接配置。可在连接列表点击「测试」，输入完整仓库地址验证读取权限；随后到工作空间的「远程」绑定该连接。若失败，请检查 Token 是否过期、是否开放目标仓库权限，以及仓库地址是否属于该连接的平台。")}</span></p></details>
      <details><summary><span data-i18n="为什么不能推送？">${trHTML("为什么不能推送？")}</span></summary><p><span data-i18n="检查连接是否允许推送、平台凭证是否有写权限，以及仓库分支是否受保护。管理员共享的连接还需授予你写权限。「测试」通过只代表可以读取，不能证明可以推送。">${trHTML("检查连接是否允许推送、平台凭证是否有写权限，以及仓库分支是否受保护。管理员共享的连接还需授予你写权限。「测试」通过只代表可以读取，不能证明可以推送。")}</span></p></details>
      <details><summary><span data-i18n="公司内网 Git 怎么连接？">${trHTML("公司内网 Git 怎么连接？")}</span></summary><p><span data-i18n="先在侧栏「内网隧道」连接你的客户端，放行 Git 服务域名与端口；添加连接时选择「我的内网隧道」。使用公司证书的 HTTPS 服务还需填写公司 CA 证书。隧道需保持在线。">${trHTML("先在侧栏「内网隧道」连接你的客户端，放行 Git 服务域名与端口；添加连接时选择「我的内网隧道」。使用公司证书的 HTTPS 服务还需填写公司 CA 证书。隧道需保持在线。")}</span></p></details>
      <details><summary><span data-i18n="这里的设置会影响终端吗？">${trHTML("这里的设置会影响终端吗？")}</span></summary><p><span data-i18n="提交身份仅用于网页提交，不修改终端 Git 配置。网页保存的 Token 和私钥不会复制进空间；如需在终端使用连接，请先在「变更 → 远程 → 终端授权」创建限时授权，并按页面说明操作。">${trHTML("提交身份仅用于网页提交，不修改终端 Git 配置。网页保存的 Token 和私钥不会复制进空间；如需在终端使用连接，请先在「变更 → 远程 → 终端授权」创建限时授权，并按页面说明操作。")}</span></p></details>
      <details><summary><span data-i18n="操作中断了，如何确认结果？">${trHTML("操作中断了，如何确认结果？")}</span></summary><p><span data-i18n="在「仓库连接 → 操作记录」查看执行结果。推送中断或结果未确认时，请先核对远程仓库；中断不代表已经发送的提交被撤回。">${trHTML("在「仓库连接 → 操作记录」查看执行结果。推送中断或结果未确认时，请先核对远程仓库；中断不代表已经发送的提交被撤回。")}</span></p></details>
    </section>
  </section>`;
  decorateIcons(host);
  for (const button of host.querySelectorAll<HTMLButtonElement>("[data-git-go]")) {
    button.addEventListener("click", () => openGitManagement(button.dataset.gitGo));
  }
}

$("btn-git-management").addEventListener("click", () => openGitManagement());
for (const button of $("git-nav").querySelectorAll<HTMLButtonElement>("[data-git-sec]")) {
  button.addEventListener("click", () => openGitManagement(button.dataset.gitSec));
}
bus.addEventListener("open-git", e => openGitManagement((e as CustomEvent<string>).detail));
bus.addEventListener("view-changed", () => {
  if (S.view !== "git") { emit("git-page-cleared"); $("git-content").replaceChildren(); }
});
bus.addEventListener("signed-out", () => { emit("git-page-cleared"); $("git-content").replaceChildren(); });
