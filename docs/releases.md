# 版本发布与二进制安装

源码仓库保持私有；公开下载仓库为 `devilcoolyue/agentbox-releases`，仅提交安装脚本、用户说明和许可证，Release 附件为二进制安装包。打包只携带 `deploy/downloads/README.md` 用户说明，不复制内部架构、审计或开发文档。

首个正式版本为 [v0.1.0](https://github.com/devilcoolyue/agentbox-releases/releases/tag/v0.1.0)。一键安装默认下载最新正式版本；下面说明构建、安装与维护流程。

新用户的一键入口是仓库根目录 `install.sh`，使用方法见[一键安装](../deploy/README.md#一键安装)。维护者需推送该入口，并将新构建的 Linux 发布包和 `SHA256SUMS` 附到公开 Release：默认命令读取 latest 正式发布，只有预览包时必须指定 `--version`。仅创建 Actions artifact 不会让安装命令可用；支持一键安装的包必须包含 `deploy/bootstrap.py`。

## 维护者构建

在干净 checkout 中准备 Go（版本见 go.mod）、Node.js 22、npm、Python 3.12+、Docker，以及 gitleaks v8.24.3。

```bash
python3 scripts/verify-third-party.py
python3 scripts/build-release.py --version v0.1.0 --output /tmp/agentbox-release
python3 scripts/test-release.py /tmp/agentbox-release
# 已在本机构建固定镜像后，可验证真实服务与容器链路（合成数据，无模型请求）
python3 scripts/test-release-server.py /tmp/agentbox-release --image agentbox-agent:claude-2.1.280-codex-0.145.0
python3 scripts/scan-secrets.py --artifacts /tmp/agentbox-release --output /tmp/agentbox-audit
```

输出 Linux amd64/arm64 的 agentbox，以及 Linux/macOS amd64/arm64、Windows amd64 的 abox-link。每个归档含许可证和第三方声明；`build.json`、`--version` 提供版本、提交、构建时间，`release.json` 是平台清单，`SHA256SUMS` 覆盖全部包与清单。构建时间取提交时间，便于追溯；不承诺跨 Go/压缩工具版本逐字节重现。服务冒烟使用 Docker socket 和一次性命名卷，仅运行本项目二进制与合成工作区，不挂载现有账号/用户数据。候选包不是数字签名产物，SHA-256 只能检查完整性。

发布脚本拒绝已有输出目录、缺失 LICENSE 或脏工作区。`--allow-dirty` 仅供本地候选验证，版本信息会标记 dirty，不可公开发布。发布前须重新从干净提交构建。

`v*` 标签触发 `.github/workflows/release.yml`，执行验证、构建、Linux 包冒烟及敏感信息扫描，上传供评审的工作流候选 artifact（可见性跟随仓库及 Actions 权限，不保证私密）。它不自动公开 GitHub Release，也不推送含 Claude Code 的镜像。维护者审阅检查结果、变更说明及许可证后，再手工创建 Release 并附上候选文件；预览版本标记为 prerelease。创建/推送标签与公开发布需要项目负责人的明确决定。

## 从包安装（无需 Go/Node 编译服务端）

以下以 Linux arm64 为例，将文件名替换为所选版本和服务器架构。需要 Linux、Docker daemon 与足够的磁盘空间。解压到不存在的新目录，不覆盖在运行的部署。

```bash
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf agentbox_v0.1.0_linux_arm64.tar.gz
cd agentbox_v0.1.0_linux_arm64
./agentbox --version
cp config.example.json config.json
chmod 600 config.json
# 编辑 auth_token；首次可将 accounts、proxies 设为 []
./scripts/build-image.sh
./agentbox --config "$(pwd)/config.json"
```

校验必须报告所选包 OK；只校验不相关文件不能替代校验安装包。不要直接运行示例中的占位凭证。镜像构建会联网下载固定版本的 CLI，需遵循其上游条款；服务端本身不需要 Go/Node。

新安装推荐使用一键安装器，自动配置 systemd 与独立运行目录；手工安装、升级和迁移使用包内 `deploy/release.py`，详见[目录与迁移手册](architecture/deployment-layout.md)。`deploy/install.sh` / `deploy/deploy.sh` 仍属于源码安装流程。

单独下载对应平台的 abox-link，校验后解压，运行 `./abox-link --version` 或 `abox-link.exe --version`。若要让控制台提供下载，将它按 `abox-link-<os>-<arch>[.exe]` 命名，放到配置的 `<data_dir>/abox-link/`；先升级服务端，再更新客户端。

## 升级与回退

1. 验证系统备份；重要工作区另做完整备份，并验证恢复。
2. 阅读 CHANGELOG、账号授权及数据库回退限制。
3. 将新包解压到新目录，保留原包、配置和备份；不要随意移动正在挂载的数据目录。
4. 停止服务，保持原配置/数据路径，通过绝对 `--config` 路径启动新版，再验证会话、凭证和历史。
5. 失败时先停新版，按兼容限制决定回退；不要将旧二进制连接到不支持的数据/配置，尤其旧版本会忽略账号使用范围。

CLI 镜像独立于服务端包，版本与回退见 [兼容矩阵](compatibility.md)。正式发布前需在真实 Linux/systemd 上验证启动、升级、备份和恢复；容器内包冒烟不等于生产验收。
