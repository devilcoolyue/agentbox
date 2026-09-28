# 开源发布审查记录（2026-09-23）

## 主仓库公开准备复核（2026-09-28）

在 `c19245a` 基础上重新获取 origin 分支与标签；扫描时全部本地 refs 可达 108 个提交，当前跟踪 656 个文件。使用仓库既有 gitleaks 配置复扫历史、跟踪文件，以及从主仓库下载的已发布 v0.1.0 附件（解包并提取二进制 printable strings），均为 0 个未审查命中。报告位于仓库外；没有扩大扫描豁免。

额外按本机生产参数比对所有可达 Git 对象：当前跟踪文件没有生产地址，历史 `deploy/agentbox.service` 与 `scripts/enable-domain.sh` 含旧生产域名。负责人选择先清理历史再公开。已在仓库外备份本地与远端 Git bundle，在隔离镜像中把域名替换为 `agentbox.example.com`，并使用原引用 SHA 作为 lease 原子更新全部 4 个远端分支与 3 个标签。清理后的历史域名比对及 gitleaks 复扫均无命中，主分支当前文件树未改变；本地普通分支、标签与跟踪引用已同步。旧克隆不能直接合并或推回，以免重新引入旧历史。

GitHub 的已合并 PR #1 仍保留 `refs/pull/1/head`，其历史包含上述两个文件。实测 GitHub 拒绝替换该引用，返回 `deny updating a hidden ref`；旧提交的服务端缓存也不能靠强推保证移除。仓库因此暂时保持私有，尚未完成公开。彻底移除 PR 引用与缓存需按 [GitHub 历史清理说明](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/removing-sensitive-data-from-a-repository#fully-removing-the-data-from-github) 联系 Support；GitHub 不承诺为非敏感数据提供清理。也可由负责人另行决定保留旧库私有，以清理后的历史建立公开仓库。

本地 `go build ./...`、`go test ./...`、`go vet ./...`、前端类型检查与重新构建后的产物一致性检查、第三方清单/哈希校验及固定镜像策略检查均通过。本次没有运行生产部署或真实模型调用；已有 CI 的 `c19245a` 运行成功。README 与发布文档改为 Apache-2.0 源码入口说明，二进制安装与自动更新继续使用 `devilcoolyue/agentbox-releases`。

以下保留 2026-09-23 的候选审查记录，版本及待办描述对应当时状态。

## 许可证与来源

项目负责人已选择 Apache-2.0。LICENSE 使用 Apache 官方全文，NOTICE 标注 Agentbox contributors，并链接第三方声明。源代码内置的 xterm、KaTeX、字体与 claude-hud 已核对上游包/提交，逐文件比较并记录哈希；webgl 唯一差异是移除 sourceMappingURL 注释。33 个跨平台实际链接的 Go 模块携带上游许可/版权/NOTICE 全文，标准库附 Go LICENSE。

Claude Code npm 许可声明不是开源授权。因此候选包仅提供用户本地安装的固定版本构建配方，不发布含该 CLI 的公共镜像；Codex 和系统包的上游许可也不会被项目许可证覆盖。详见 `third_party/README.md`。

## 敏感信息扫描

本地执行 `git fetch --all --tags` 后扫描全部 refs。范围含 3 个本地分支、origin/main、origin/feat/audit-improvements、origin/refactor/frontend-ts；当时无 tag，共 75 个可达提交。gitleaks 对差异扫描报告 72 个提交（不产生普通差异的提交不计入其统计）。另扫描当前跟踪文件、七个平台解包候选及二进制 printable strings。

使用 gitleaks v8.24.3，报告在仓库外并脱敏。首轮 6 个命中已复核：公开 OAuth client ID、生成令牌的固定字符表、上游压缩 JS 的类型导出；二进制 strings 额外命中 Go 类型名拼接。`.gitleaks.toml` 仅豁免这些精确字符串，不跳过文件、目录或整个提交。配置后无未审查的密钥命中；未尝试验证令牌，不向外部服务发送候选密钥。

补充审查了生产 SSH/URL 参数引用，发现 `deploy/agentbox.service` 仍有旧生产域名；当前版本已替换为项目主页。**旧域名仍留在历史中**，默认扫描器不会把域名视作密钥。正式公开前负责人需决定是否接受历史披露，或在备份后另建清理后的公开历史；本次没有改写历史、强推、轮换真实凭证或访问生产服务。

扫描不能证明所有秘密不存在。被忽略的真实 config/accounts/data、其他机器的未推送分支、GitHub 未暴露的对象以及既往外部分发物不在本地扫描范围。新生成的正式包必须在发布前重扫；原工作区未提交 UI 不会进入本次干净提交的候选包。

## 验证及发布前事项

本地验证包括源码构建/测试/vet、前端一致性、第三方哈希与模块覆盖、固定镜像策略、多平台打包、校验和、无 Go/Node 的 Linux 包执行与备份恢复、真实 Docker 的合成会话 CRUD/文件/Git。服务冒烟只挂载临时命名卷与 Docker socket，测试容器与卷已清理。

这不包括在线模型推理、生产数据恢复、Linux systemd 的正式升级/切换或远端 GitHub Actions 运行。公开 Release 和 tag 尚未创建。维护者在公开前还需启用 GitHub 私密漏洞报告、审阅候选文件和历史域名决策，再执行发布。源码安装脚本与按版本运行目录迁移的剩余工作仍属于 D1。

## 最终本地候选

采用正式 Apache-2.0 的本地候选 `v0.1.0-rc.1` 来自干净提交 `5dceb81`，未创建同名 tag。七个平台归档、版本/提交元数据及校验和核对通过；Linux arm64 包在 Alpine 容器中执行备份/校验/恢复后再次读取恢复库，合成内容一致；Debian Agent 镜像的服务链路也复测通过。最终密钥扫描对源码、全部已获取 refs、解包产物及二进制 printable strings 无未审查命中。

本机候选目录 `/tmp/agentbox-b-final-candidates`、脱敏报告 `/tmp/agentbox-b-final-audit` 均不进入版本库。工作流 artifact 的访问范围取决于仓库/Actions 设置，并非私密漏洞反馈渠道。
