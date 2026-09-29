# 第三方组件与分发边界

Agentbox 的项目许可证不替代第三方许可证。发布包携带本目录与上游版权/许可全文；Go 依赖仍保留其各自许可。

## 内置资源

| 组件 | 版本 / 来源 | 许可 | 本地修改 |
| --- | --- | --- | --- |
| @xterm/xterm | 6.0.0，npm / xtermjs/xterm.js | MIT | 无；JS/CSS 与 npm 逐字节一致 |
| @xterm/addon-fit | 0.11.0，npm / xtermjs/xterm.js | MIT | 无 |
| @xterm/addon-webgl | 0.19.0，npm / xtermjs/xterm.js | MIT | 仅移除末尾 sourceMappingURL 注释 |
| KaTeX | 0.16.9，npm / KaTeX/KaTeX | MIT；另附字体仓库 MIT 许可 | JS/CSS/20 个 woff2 与 npm 逐字节一致 |
| Prism | 1.30.0，npm / PrismJS/prism | MIT | 按 vendor.json 中顺序拼接上游压缩核心与语言组件，添加许可头；未修改语法规则 |
| noVNC | 1.6.0，novnc/noVNC | MPL-2.0，另附 BSD 许可 | core/vendor 原生模块与上游一致 |
| Playwright seccomp profile | 1.58.2，microsoft/playwright | Apache-2.0 | 无；仅用于可选浏览器镜像的用户命名空间沙箱 |
| claude-hud | 0.5.1，jarrodwatts/claude-hud @ 10979d16dce075b112b7564bead95c5f236d8a87 | MIT | package.json 和 54 个 dist 文件与上游一致；补入上游 LICENSE |

来源 URL、每个文件的 SHA-256、许可文件位置见 `vendor.json`。更新资源后需重新核对来源、版本、修改及许可，不能仅修改哈希绕过校验。执行 `python3 scripts/verify-third-party.py` 验证。

## Go 依赖

`go-modules.json` 记录七个发行目标实际链接的模块及版本；`go/` 保留这些模块中的 LICENSE/COPYING/NOTICE 等全文（含嵌套版权文件）。`licenses/Go-LICENSE` 是当前构建工具链的标准库许可。源码的完整依赖图以 go.mod/go.sum 为准，模块清单不包含仅构建工具、测试或未链接模块。为完整保留上游声明，已链接模块目录中的辅助示例/测试许可也一并收录；保留其文本不代表这些辅助代码被链接进发行二进制。

更新 Go 依赖后执行 `python3 scripts/collect-go-licenses.py`，审查新增和删除的组件及文本后提交清单。不要把工具链升级自动视为许可证已审查。

## 运行时 CLI 和镜像

发布包提供 Agentbox/abox-link 二进制与镜像构建配方，不分发预装 Claude Code 的公共镜像。

- Claude Code 不是本项目的开源代码。所核对的 npm 2.1.280 包 LICENSE.md 声明 © Anthropic PBC，All rights reserved，并指向 https://code.claude.com/docs/en/legal-and-compliance 。用户构建镜像时从官方 npm 下载，使用须遵循上游法律协议；不能由 Agentbox 的许可证推导出 Claude Code 的再分发权。
- Codex CLI 上游仓库及 npm 0.145.0 元数据声明 Apache-2.0：https://github.com/openai/codex 。当前仅在用户本地镜像构建时下载；如果后续分发其二进制或镜像，还需携带对应版本 LICENSE/NOTICE 与内嵌依赖声明。
- 可选浏览器镜像在用户本地从 Google 官方地址下载固定版本 Chrome for Testing；Chrome/Chromium 及其商标、网页服务分别遵循上游条款。项目只提供构建配方，不公开分发包含 Chrome 的镜像。
- Node.js/Debian 与 apt 软件包含各自许可。当前不发布这些镜像层；用户本地构建的系统包许可可在 `/usr/share/doc/*/copyright` 查看。
- 模型服务、订阅与商标的使用权不由代码许可证授予。发布工作流仅构建可审查的候选包，公开发布由负责人执行。
