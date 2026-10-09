# 版本发布与二进制安装

主仓库 [devilcoolyue/agentbox](https://github.com/devilcoolyue/agentbox) 采用 Apache-2.0 许可证，统一提供源码、文档、正式 Release 和安装包。自 v0.1.5 起，安装器、控制台更新检查与升级下载均使用主仓库。

旧仓库 [devilcoolyue/agentbox-releases](https://github.com/devilcoolyue/agentbox-releases) 保留历史下载和兼容镜像：旧版二进制的检查地址无法通过修改网页变更，需同步同一份新发布附件，让旧用户能发现并安装过渡版本。v0.1.5 安装后转向主仓库检查后续更新。旧仓库不单独构建、不删除旧附件；未来发版仍同步兼容镜像，退役前另行公告。

二进制包携带 `deploy/downloads/README.md` 用户说明；架构、审计和开发文档在源码仓库查阅。

2026-09-28 源码公开前清理了历史中的生产域名，相关提交与标签的哈希因此改变。此前发布包的 `build.json` / `--version` 仍记录清理前构建提交，不能直接用它在新源码仓库定位；已有附件与 SHA256SUMS 未重新打包或修改。历史版本源码请按对应版本标签查阅，后续新包使用公开仓库的提交哈希。

当前服务端正式版本为 [v0.1.12](https://github.com/devilcoolyue/agentbox/releases/tag/v0.1.12)；桌面安装包仍为独立的 [Desktop 0.1.2](https://github.com/devilcoolyue/agentbox/releases/tag/desktop-v0.1.2)。一键安装默认下载最新服务端正式版本；桌面测试包独立发布，不占用服务端 `releases/latest`。

### v0.1.12：账号模型、切换账号与界面风格

账号可配置自己的可用模型、默认模型和逐模型推理强度，可读取上游列表或参考 Agent 镜像内 CLI 的官方目录；「模型管理」分为账号模型、价格和系统模型列表三个标签页。空间可切换到同类型、有权使用的其他账号，切换前停止容器并把续出的令牌收回原账号。网页新增五种界面风格与克制的动效，用户弹层改为单行选择，触屏按钮更紧凑。Go 1.26.9 与 golang.org/x/net v0.60.0 修复新安全公告。完整内容见 [CHANGELOG](../CHANGELOG.md)。

相对 v0.1.11 不新增数据库迁移（仍为 schema 12），不改现有聊天、终端和配对协议；新增切换账号与账号模型读取接口，见 [账号与模型](accounts-and-models.md)。配置新增的账号模型字段在回退到 v0.1.11 后会被忽略，旧版再次保存配置会丢掉它们。从 v0.1.10 直接升级仍需 schema 10 → 12 迁移并遵守下节备份、回退约束。本次发布两个 Linux 服务端包、五个平台 abox-link、`release.json` 和 `SHA256SUMS`；不发布新桌面安装包。

### v0.1.11：项目创建、聊天恢复与工作台改进

新增空项目、上传和 Git 导入流程、首次任务分步引导、空间搜索/状态筛选、本机草稿和持久消息确认；断线或重启后查询原请求，未知结果须核对，不自动重跑。Markdown 文件预览支持 GFM 与受限 HTML，聊天显示 Claude/Codex 思考摘要，侧栏、确认区和「关于与更新」更紧凑。新增环境诊断、离线管理员密码恢复与候选 Agent 镜像行为验证。完整内容见 [CHANGELOG](../CHANGELOG.md)。

从 v0.1.10 升级会执行 schema 10 → 12 迁移。升级前保存并验证备份；旧 schema 10/11 二进制不能打开新库，回退须恢复兼容备份到新目录，并保全升级后的文件变化。系统备份含已接收聊天正文与回执，项目和附件仍需完整备份。现有旧客户端接口保留，同步默认关闭，空间镜像独立更新。本次仍为两个 Linux 服务端包、五个平台 abox-link、`release.json` 和 `SHA256SUMS`；不发布新桌面安装包。

### v0.1.10：多语言与项目展示

网页控制台与 abox-link 面板支持简中、繁中和英文，默认跟随系统，不支持时回退英文；用户选择保存在当前浏览器／设备。即时切换保留草稿、终端与原有系统时区，服务端诊断和用户内容不翻译。语言菜单采用紧凑布局。中英文 README 增加高清截图、34 秒 MP4 / GIF 与双语架构图，完整规则见[界面多语言](i18n.md)。

本次发布 Linux amd64 / arm64 服务端和 Linux/macOS amd64/arm64、Windows amd64 的 abox-link：共七个安装归档，另附 `release.json` 与 `SHA256SUMS`。服务端包也携带五个平台的 abox-link 下载文件；本机已安装的旧 abox-link 需要另行更新。桌面源码已接入三语，但本次**不发布新桌面安装包**，`desktop-v0.1.2` 不会因升级服务端而获得语言选择器。

相对 v0.1.9 不新增数据库迁移（仍为 schema 10），不更改现有 API / WebSocket / 配对协议，不启用同步或更换工作空间镜像。从 v0.1.8 直接升级仍需执行 schema 9 → 10 迁移并遵守下节备份、回退约束。标准版本目录安装可使用「关于与更新 → 升级并重启」或发布包中的 `deploy/release.py`，会短暂断开连接。

### v0.1.9 与桌面客户端

v0.1.9 发布此前桌面集成开发中的服务端扩展：能力发现、桌面配对、逻辑项目、独立 AI/Shell 终端，以及带条件写入、执行收据和恢复管理的目录同步接口。Desktop 0.1.2 连接 v0.1.8 仍可使用登录、空间选择、共享终端和手动文件传输；扩展功能需要 v0.1.9，不能只安装新版桌面包。

同步默认关闭（`desktop_sync_enabled=false`、能力 `sync=0`）。管理员显式在配置中开启后重启，或通过 `PUT /api/settings` 提交 `{"desktop_sync_enabled":true}`，客户端重新登录后才显示同步入口。配对、项目终端及恢复管理独立开放，升级不会自动建立本地映射或开始传输文件。

首次启动将数据库从 schema 9 迁移至 10；升级前验证配套备份。**v0.1.8 不能直接打开已迁移的数据库**，回退必须恢复升级前兼容备份到新目录，并另外保全升级后的文件变化。现有网页与 abox-link 接口保持兼容，服务端包不自动更新空间镜像。详见[数据库迁移与回退](architecture/database-migrations.md)和[兼容矩阵](compatibility.md#桌面客户端与服务端)。

主仓库和 `agentbox-releases` 兼容镜像使用同一套 v0.1.9 服务端/abox-link 附件、`release.json` 与 `SHA256SUMS`，只构建一次；桌面安装包继续使用独立的 `desktop-v0.1.2` Release。

### 历史版本与源码

v0.1.0～v0.1.4 的七个平台包、`release.json` 和 `SHA256SUMS` 从旧仓库原样迁入，不重新编译。表中的原构建提交来自包内发布清单，清理后提交对应主仓库版本标签。

| 版本 | 包内原构建提交 | 清理后源码提交 |
| --- | --- | --- |
| v0.1.0 | `2a916ccbdc17` | `19c7678494a0` |
| v0.1.1 | `e4555ab841de` | `5d1266f986f7` |
| v0.1.2 | `4855187dfe3b` | `8ef557ed4595` |
| v0.1.3 | `4f21454474b8` | `639d038aebca` |
| v0.1.4 | `6576d45a2e3b` | `adaad57a4551` |

迁移补齐 v0.1.1、v0.1.2 标签，并将原 v0.1.0 标签从 `69f839756c47` 校正到公开包的实际构建源码 `19c7678494a0`（两者相差一次安装入口与打包调整）。已克隆旧标签的维护者需核对后单独刷新该标签。v0.1.2 来自保留的发布分支，其功能后来并入 main，不能用 main 上任意相近提交代替该发布源码。

新用户的一键入口是仓库根目录 `install.sh`，使用方法见[一键安装](../deploy/README.md#一键安装)。维护者需推送该入口，并将新构建的 Linux 发布包和 `SHA256SUMS` 附到公开 Release：默认命令读取 latest 正式发布，只有预览包时必须指定 `--version`。仅创建 Actions artifact 不会让安装命令可用；支持一键安装的包必须包含 `deploy/bootstrap.py`。

## 维护者构建

在干净 checkout 中准备 Go（版本见 go.mod）、Node.js 22、npm、Python 3.12+、Docker，以及 gitleaks v8.24.3。

```bash
python3 scripts/verify-third-party.py
python3 scripts/build-release.py --version v0.1.12 --output /tmp/agentbox-release
python3 scripts/test-release.py /tmp/agentbox-release
# 已在本机构建固定镜像后，可验证真实服务与容器链路（合成数据，无模型请求）
python3 scripts/test-release-server.py /tmp/agentbox-release --image agentbox-agent:claude-2.1.280-codex-0.145.0
python3 scripts/scan-secrets.py --artifacts /tmp/agentbox-release --output /tmp/agentbox-audit
```

输出 Linux amd64/arm64 的 agentbox，以及 Linux/macOS amd64/arm64、Windows amd64 的 abox-link。每个归档含许可证和第三方声明；`build.json`、`--version` 提供版本、提交、构建时间，`release.json` 是平台清单，`SHA256SUMS` 覆盖全部包与清单。构建时间取提交时间，便于追溯；不承诺跨 Go/压缩工具版本逐字节重现。服务冒烟使用 Docker socket 和一次性命名卷，仅运行本项目二进制与合成工作区，不挂载现有账号/用户数据。候选包不是数字签名产物，SHA-256 只能检查完整性。

发布脚本拒绝已有输出目录、缺失 LICENSE 或脏工作区。`--allow-dirty` 仅供本地候选验证，版本信息会标记 dirty，不可公开发布。发布前须重新从干净提交构建。

`v*` 标签触发 `.github/workflows/release.yml`，执行验证、构建、Linux 包冒烟及敏感信息扫描，上传供评审的工作流候选 artifact（可见性跟随仓库及 Actions 权限，不保证私密）。它不自动公开 GitHub Release，也不推送含 Claude Code 的镜像。维护者审阅检查结果、变更说明及许可证后，再手工创建 Release 并附上候选文件；预览版本标记为 prerelease。创建/推送标签与公开发布需要项目负责人的明确决定。

正式发布先在主仓库创建 draft 并上传通过验证的完整附件，核对远端附件清单与 SHA-256 后发布；再把**同一目录中的原始文件**上传到旧仓库的同版本 draft，验证一致后发布兼容镜像。只构建一次，不能给两个仓库分别构建同版本。旧仓库仅同步安装入口和用户说明，不复制主项目源码；镜像说明链接到主仓库的正式 Release。两边都验证 `/releases/latest` 和下载地址，避免只更新说明却没有实际安装包。

## 从包安装（无需 Go/Node 编译服务端）

以下以 Linux arm64 为例，将文件名替换为所选版本和服务器架构。需要 Linux、Docker daemon 与足够的磁盘空间。解压到不存在的新目录，不覆盖在运行的部署。

```bash
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf agentbox_v0.1.12_linux_arm64.tar.gz
cd agentbox_v0.1.12_linux_arm64
./agentbox --version
cp config.example.json config.json
chmod 600 config.json
# 编辑 auth_token；首次可将 accounts、proxies 设为 []
./scripts/build-image.sh
./agentbox --config "$(pwd)/config.json"
```

校验必须报告所选包 OK；只校验不相关文件不能替代校验安装包。不要直接运行示例中的占位凭证。镜像构建会联网下载固定版本的 CLI，需遵循其上游条款；服务端本身不需要 Go/Node。

新安装推荐使用一键安装器，自动配置 systemd 与独立运行目录；手工安装、升级和迁移使用包内 `deploy/release.py`，详见[目录与迁移手册](architecture/deployment-layout.md)。`deploy/install.sh` / `deploy/deploy.sh` 仍属于源码安装流程。

单独下载对应平台的 abox-link，校验后解压，运行 `./abox-link --version` 或 `abox-link.exe --version`。v0.1.1 起服务端包同时携带这五个平台的客户端，激活时自动安装到 `<data_dir>/abox-link/`，无需手工构建。

## 升级与回退

1. 验证系统备份；重要工作区另做完整备份，并验证恢复。
2. 阅读 CHANGELOG、账号授权及数据库回退限制。
3. 将新包解压到新目录，保留原包、配置和备份；不要随意移动正在挂载的数据目录。
4. 停止服务，保持原配置/数据路径，通过绝对 `--config` 路径启动新版，再验证会话、凭证和历史。
5. 失败时先停新版，按兼容限制决定回退；不要将旧二进制连接到不支持的数据/配置，尤其旧版本会忽略账号使用范围。

CLI 镜像独立于服务端包，版本与回退见 [兼容矩阵](compatibility.md)。正式发布前需在真实 Linux/systemd 上验证启动、升级、备份和恢复；容器内包冒烟不等于生产验收。

## 控制台更新提示

管理员侧栏版本徽标和「关于与更新」页显示运行中二进制的构建信息。`GET /api/updates` 读取本地版本与检查缓存，`POST /api/updates/check` 检查上游正式发布（两者均需管理员权限）；`?force=1` 可手动检查，仍有 1 分钟防重复请求间隔。自动检查由已登录、可见且联网的控制台每 4 小时触发，服务端缓存由所有页面共享，重启后重新检查。

数据源是公开下载仓库 `devilcoolyue/agentbox` 的 GitHub Releases latest 接口，不包含 draft/prerelease，也不单独检查会话镜像或 abox-link。没有正式发布、开发构建可切换到正式版、网络失败均单独显示；不会把检查失败当成最新版本。版本号由发布构建注入，普通 `go build` 为 `dev`。

标准 Linux/systemd 发布安装支持管理员点击「升级并重启」。首次使用需要先通过原有手工流程升级到包含此功能的发布包；旧版二进制不会自行获得新按钮。支持条件包括 root 运行、构建信息与版本目录及 current 链接一致、原始 agentbox.service 单元及无自定义 drop-in、Python 3 和 systemd-run。此布局下的 dev、预发布版和带未提交改动的构建均可点击「切换到正式版并重启」，无需目标版本号高于开发版；正式版之间仍只允许升级到更高版本。目标必须是检查结果中的最新正式发布，配置和数据库兼容检查仍在停服前执行。源码安装、自定义服务布局继续显示手工升级说明。开发实例需先安装包含此修复的二进制及 deploy/update.py，旧构建不会自动解除限制。

升级固定管理员确认的版本，从该版本下载匹配架构的完整服务端包和 SHA256SUMS，强制验证哈希、下载域名与重定向、归档路径和体积、构建信息及客户端完整性。下载不会自动改追后来发布的新版本。执行前检查配置与数据库兼容性，停服后创建并验证系统备份，切换版本、安装配套 abox-link、启动并检查健康状态及实际运行程序。会话镜像不随服务端升级，备份也不包含工作区的一致性快照。

`deploy/update.py` 将任务交给独立的 `agentbox-upgrade-<任务ID>.service`，不依附主服务进程。任务最多运行 30 分钟，状态原子写入 `<app>/.update/state.json`；重复提交同一进行中的目标复用任务，升级与手工发布共用部署锁。网页每 3 秒查询进行中的任务，刷新页面后可继续查看；断线不重复提交，恢复后核对目标版本，并提供刷新页面按钮加载新版界面。若服务无法恢复，在服务器运行页面显示的 `journalctl -u agentbox-upgrade-<任务ID>.service`，并检查 `journalctl -u agentbox`。服务器重启或任务超时退出会在下次查询时标为失败。

下载/校验/兼容检查失败不停止当前服务；停服后、版本切换前失败会尝试重新启动原服务。版本切换后不自动回退，避免把已迁移的 SQLite 数据库交给不兼容旧程序。重试可以复用内容完全一致的已暂存版本，不能覆盖已有版本目录。升级会中断 HTTP/WebSocket 连接及可能正在执行的网页对话，应先结束当前任务。

## v0.1.1

包含最新控制台铺满布局、版本提醒、登录错误处理与限时重试、配对码复制；修复 SELinux 安装启动以及卸载后立即重装的端口检查。服务端包携带五个平台客户端，并提供兼容 v0.1.0 默认布局的一键卸载入口。卸载默认保留数据，可显式彻底删除。源码提交和 GitHub 二进制发布互相独立，旧版本附件不会随 main 自动更新。
