# CLI 与运行时兼容矩阵

推理参数回归可运行：

```bash
AGENTBOX_CLI_TEST_IMAGE=agentbox-agent:claude-2.1.280-codex-0.145.0 \
  go test ./internal/agent -run 'TestReasoningCLI' -v -count=1
```

该测试在本机 Linux Docker 容器以 UID 1000 执行，`network=none`，临时 home 和模拟 HTTP 上游只使用合成凭证，不消耗模型额度。覆盖 Claude 原生 effort、固定预算、默认设置继承与思考摘要（`thinking.display=summarized`），Codex app-server / exec 参数、推理摘要（`reasoning.summary`）及默认继承，并单独验证 `config/read` + `model/list` 协议。它验证 CLI 发出的请求字段，不证明真实 provider 接受任意模型与档位组合。

## 固定基线

| 组件 | 默认版本 | 验证范围 |
| --- | --- | --- |
| Node 基础镜像 | 22.23.2-bookworm-slim，OCI index SHA-256 见 versions.env | 包含 amd64/arm64；固定 digest |
| 终端默认环境 | Debian 12、彩色 Bash / `ll`、完整 Vim | 系统配置不受空 home 挂载影响；真实浏览器验证方向键、退格、冒号命令、保存和 tmux 重连 |
| Claude Code | 2.1.280 | Linux arm64 固定镜像无凭证版本命令；合成会话启动/文件/Git 验证通过 |
| Codex CLI | 0.145.0 | Linux arm64 固定镜像版本命令；终端 rollout 解析按此版本公开结构及合成样本验证；未做本阶段在线推理 |
| claude-hud | 0.5.1 | vendored 文件与上游提交逐字节比对 |
| abox-link | 与服务端相同发布版本 | 先升级服务端；配对依赖 `/api/tunnel/pair/redeem` |

`images/agent/versions.env` 和 Dockerfile 的默认值必须一致。`scripts/build-image.sh` 默认固定 CLI 版本和基础镜像 digest；APT 包仍从 Debian 当前仓库获取，因此它是固定关键运行时的构建配方，不是完全可重现的系统镜像。安全补丁通过有意更新基线、测试和发布引入。

既有 `agentbox-agent:latest` 只是本地兼容别名，不代表构建脚本安装 npm latest。构建同时保留 `agentbox-agent:claude-<版本>-codex-<版本>` 标签，不自动清理旧层。镜像标签仍可被覆盖；需要保留精确镜像时记录 image ID 或自行 `docker save` 归档。

## 桌面客户端与服务端

Agentbox Desktop 独立发布；服务器上的 Claude Code / Codex CLI 镜像更新与桌面应用更新是不同操作。

| 组合 | 可用能力 | 数据与升级要求 |
| --- | --- | --- |
| Desktop 0.1.2 → 服务端 v0.1.8 | 基础模式：用户名/密码登录、空间选择、共享终端、手动文件传输 | 沿用服务端 schema 9；无桌面配对、逻辑项目、独立多终端及目录同步接口 |
| Desktop 0.1.2 → 服务端 v0.1.9，默认配置 | 基础功能，加桌面配对、逻辑项目、独立 AI/Shell 终端及恢复记录管理 | 服务端首次启动将 schema 9 升至 10；同步默认关闭（`desktop_sync_enabled=false`、`sync=0`） |
| Desktop 0.1.2 → 服务端 v0.1.9，管理员显式开启同步 | 增加本地目录映射、差异预览、逐文件冲突处理、手动/持续同步 | 开启 `desktop_sync_enabled` 后重新登录；仍需客户端确认目录与同步计划 |
| 既有网页 / abox-link → 服务端 v0.1.9 | 保持原有接口、空间及账号授权规则 | 无需桌面应用；服务端升级不自动更换工作空间镜像 |
| 网页 / abox-link v0.1.10 → 服务端 v0.1.10 | 简中、繁中、英文与跟随系统；现有 API、终端和配对协议不变 | 从 v0.1.9 升级仍为 schema 10；从 v0.1.8 升级执行已有的 schema 9 → 10 迁移 |
| Desktop 0.1.2 → 服务端 v0.1.10 | 沿用 v0.1.9 的桌面能力；已发布的桌面界面不含三语适配 | 同步仍默认关闭；升级服务端不会更新桌面安装包，桌面源码的多语言功能需后续独立发包 |

v0.1.11 的网页与 abox-link 保留三语及旧接口；网页增加创建/导入、草稿与持久聊天 v1。服务端从 schema 10 自动迁移至 12，旧二进制回退须恢复兼容备份。Desktop 0.1.2 沿用既有服务端能力、同步默认关闭，本次不发布新桌面包。

客户端按能力发现显示扩展入口，旧服务端返回 404 或页面时进入基础模式；鉴权错误、网络故障和无效响应不会冒充基础模式。新桌面包不会自动升级服务端。

v0.1.9 的 schema 10 只新增项目和终端元数据，不移动空间文件。升级前保存并验证配套备份；v0.1.8 不能直接打开已迁移数据库，回退必须恢复兼容备份到新目录，并保全升级后的文件变化。桌面本地状态另使用 schema 5，不与服务端 schema 混用。详见[数据库迁移与回退](architecture/database-migrations.md)。

Desktop 0.1.2 的 Mac ARM64、Mac Intel 和 Windows x64 构建与自动化验收，以及旧服务端/旧网页/abox-link 兼容、Windows→Linux 同步和 Linux 故障恢复记录见[开发验收表](architecture/desktop-client-acceptance.md)。这些记录不替代最低系统、真实离线 Windows、物理交互及正式签名升级验收；测试包的系统要求与边界见[桌面说明](../desktop/README.md)。

## 客户端镜像更新

v0.1.7 起，管理员可在「系统设置 → 容器与资源 → Agent 镜像更新」（旧界面名「客户端更新」）手动检查、更新和回退，也可设置系统时区下的每日检查时间。自动更新默认关闭，Claude 默认 stable 渠道，Codex 默认保持当前版本；选择 latest 渠道或同时更新 Codex 需明确启用。基于当前镜像保留浏览器与自定义功能，更新通过 CLI 版本验证后才切换；v0.1.11 还要求[候选行为验证](agent-image-validation.md)，报告落盘后按不可变镜像 ID 切换，回退也通过同一门槛。更新后的版本不自动成为本表已验证的固定基线。回退会暂停自动更新，运行中的空间停止再启动后使用所选镜像。

v0.1.11 在「关于与更新」中增加[三类更新与生效范围](update-components.md)的只读版本观察。镜像标签、版本大小比较和本次桌面登录能力各有范围，不作为候选完全兼容的证明；桌面安装版本需在应用中查看。

## 旧部署的实验性追新

网页已管理镜像更新时，应保持旧 systemd 更新 timer 停用，避免两套调度同时改镜像。旧源码部署的手动追新仍默认关闭：

```bash
AGENTBOX_AUTO_UPDATE=1 ./scripts/auto-update-image.sh
```

源码安装脚本默认禁用每日更新 timer，包括之前已启用的 timer。明确要每日追新的管理员可用 `sudo env AGENTBOX_ENABLE_AUTO_UPDATE=1 ./deploy/install.sh`，或在安装后 `sudo systemctl enable --now agentbox-image-update.timer`。timer 服务显式设置 `AGENTBOX_AUTO_UPDATE=1`。v0.1.11 要求新版 `agentbox --check-agent-image`：先构建独立候选并验证，通过后才更新目标标签，缺少验证命令则失败保留原镜像。实验版本不自动成为兼容矩阵中已验证的版本；运行中的容器继续使用原镜像。

## 回退镜像

先停止相关会话，再将系统设置中的 `agent_image` 指向保留的旧版本标签或 image ID，然后重启会话。确认 Docker 实际重建为所选镜像并测试续聊/终端。不要为节省磁盘自动 prune 回退所需镜像，也不要只替换 CLI 的宿主机文件就假定已运行的容器更新。

无凭证 `--version`、帮助和协议握手可以在本地验证；真实推理、OAuth 轮换和计费需要单独授权的在线测试。不要将构建成功描述为所有上游行为已验证。

## v0.1.11 的创建/导入协议

v0.1.11 包含新增的 `session-creations` v1 协议及服务端 schema 11。新版网页先发现该协议，再使用独立 PUT 路径创建；旧服务端不支持时明确报错，不降级成忽略请求 ID 的旧 POST。旧网页/abox-link/桌面接口继续保留，但没有使用新协议的调用不获得其重放保证。

schema 11 不修改已有会话、模型、用量或额度，首次启动自动迁移。v0.1.10/schema 10 回退仍须兼容备份，不能直接打开新版数据库。本版同时包含下述 schema 12；历史版本组合与桌面本地 schema 不变。

v0.1.11 同时新增 schema 12 聊天回执存储与启动恢复。HTTP 接收/查询/核对接口已接入原执行器，`chat_protocol:1`、`chat_scope` 独立协商。`draft_protocol:1` 仍仅声明草稿能力；新版浏览器发现 chat v1 后使用持久 HTTP 发送、按 ID 查询和显式核对；仅在服务端未声明该能力时保留旧 WS 发送并提示限制，已选择新协议后不会因错误降级成无 ID 发送。旧 WS 和线程变更也会被持久活动/待核对请求阻止；未知结果不能通过换线程绕过。运行连接改用 WAL + synchronous=FULL；schema 11 和 10 二进制均不能直接打开 12，回退需对应备份。系统备份会包含数据库中的已接收提示词副本，附件内容仍需完整备份。详见[数据库迁移](architecture/database-migrations.md)及 [M3 记录](milestones/m3.md)。

## 错误、聊天与多端兼容窗口

M4 的契约覆盖 M1～M3 新增错误/聊天字段，M5 桌面复用错误解释与空间状态；服务端与网页契约随 v0.1.11 发布，桌面改动仍需独立安装包。

| 能力/调用方 | 发现与可接受版本 | 缺省、未知或错误时的行为 | 本轮证据 |
| --- | --- | --- | --- |
| 旧网页/abox-link/桌面基础接口 | 保留既有 HTTP/WS 路径及旧 error 字符串 | 不获得新请求 ID 的持久确认；不得绕过当前属主/额度/待核对准入 | 冻结 eb845db 网页/abox-link → schema 12 实际 Linux 回归；桌面旧协议 Rust loopback |
| 新网页持久聊天 | `/api/me.chat_protocol=1` + 非空 chat_scope；回应/WS version=1 | 缺省/0 才可旧 WS；显式未知版本拒绝发送，v1 失败只保留副本/查询，不能自动降级或重放 | Go/TS v1 schema、全状态样本、浏览器/真实 API 联合矩阵 |
| 新网页草稿 | draft_protocol=1 + draft_scope，独立于 chat_protocol | 未获确认的空间/线程身份不跨范围恢复；保存开关不撤回已接收输入 | 既有草稿/附件/用户隔离回归 |
| 网页和开发桌面的结构化错误 | v1 是字段契约版本，wire 保留旧 error；code/operation_id 为增补 | 已知码显示本地三语建议；未知码走兼容提示，桌面不转发原始服务端/代理文本；retryable 不是写操作自动重试授权 | 共同错误目录、Go/TS/Rust 字段和脱敏检查 |
| 开发桌面空间状态 | status + 可选 stop_reason；不新增接口版本 | 老服务端缺省 reason 视作普通停止；running 优先；idle reason 表示休眠 | 共享纯状态函数及 Rust 缺省字段、三语测试 |
| 桌面扩展/同步 | `/api/clients/capabilities` protocol_version=1；各 feature 独立数字能力 | 旧服务端 404/历史 SPA 页面仅进入基础模式；鉴权/网络/未知版本不得伪装成不支持；sync=0 不启用同步 | 既有冻结与 Rust 兼容回归，本批未开启任何实例同步 |

兼容窗口以声明支持的协议与冻结基线为准，不由 CLI 的版本大小推断。本轮保留 legacy 路径与 v1，无自动到期日期。v1 允许增补可选响应字段；不能在原版本内重命名/删除字段、改变状态含义或金额单位。请求仍拒绝未知字段。增加破坏性协议须新增版本并显式协商，提供迁移说明和可工作的旧版本路径；淘汰前须在正式版本说明宣布支持矩阵变更，并针对计划保留的最老客户端完成冻结回归。数据库升级/回退规则独立，不能用 HTTP 兼容保证旧二进制可打开新库。

本批冻结升级验收见 [M4](milestones/m4.md)；桌面发布和签名/设备差距见 [M5](milestones/m5.md)。桌面开发源码能力不代表已发布 Desktop 0.1.2 安装包具有这些改动。
