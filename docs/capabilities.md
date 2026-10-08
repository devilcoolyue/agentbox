# 当前能力、版本与验证清单

维护日期：2026-10-08。这是服务端 / 网页 v0.1.11 正式版与当前 `main` 的导航与范围清单；发布内容、自动化通过和外部待验收分别标明。后续排期以[第二轮里程碑 M6～M11](roadmap-2.md)为准，进展见 [M6 记录](milestones/m6.md)；第一轮范围见[里程碑](roadmap.md)和[实施单元](milestones/backlog.md)，实施证据见 [M1 记录](milestones/m1.md)。本文不把旧记录当成本轮重新执行。

## 当前版本口径

| 对象 | 当前依据 | 不能混淆的边界 |
| --- | --- | --- |
| 服务端 / abox-link 正式版 | [v0.1.11](releases.md)（2026-10-08，构建提交 `cdaa457`），主仓库与 `agentbox-releases` 同步相同附件与 SHA256SUMS | 第一轮 M1～M5 的服务端与网页改动均已含在此版；生产实例已从开发版切到正式包。发布不等于真实用户、设备、签名和恢复目标等外部验收完成 |
| 服务端数据库 | v0.1.11：schema 12 | 11 新增创建/导入收据，12 新增聊天回执；v0.1.10 及更早只支持 ≤10，回退须恢复兼容备份到新目录，演练见 [M6 记录](milestones/m6.md#m6-07-回滚演练) |
| 托管 CI | `cdaa457` 上 CI（web、build-test、source-audit、chat-reliability、govulncheck）与 Release candidate（含 `docker.cli-candidate`、真实 systemd `docker.recovery-drill`、部署迁移/回退）通过；`desktop.yml` 于 `1ea2dae` 通过 | `verification-stability.yml` 与 `desktop-release.yml` 尚未在托管 runner 运行；20 次稳定性采样仍是本机 runner 记录 |
| 桌面公开测试版 | 已公开版本 0.1.2；[对应历史 CI/安装证据](architecture/desktop-client-acceptance.md) | 公开测试版未正式签名/公证；源码为 0.1.3-dev（三语、错误解释），尚未公开发布，仅有本地 macOS arm64 测试 ZIP 和旧包替换验收；旧安装包不会自动更新 |
| Agent 镜像基线 | `images/agent/versions.env`：Claude 2.1.280、Codex 0.145.0、Node 22.23.2 与固定 digest | `latest` 是可变标签；后续在线更新版本不自动成为已验证基线 |
| 构建工具 | `go.mod`：Go 1.26.6；网页 CI Node 22；包管理版本见 lockfile | 本机 Node、Go 及平台实际值以验证报告为准，不把本机结果当固定 runner 指标 |

## 能力与证据范围

下表“基础回归”指当前 Go/前端离线或合成 API 测试；它不包含默认跳过的 Docker、真实模型和原生交互。可执行目录见[统一验证入口](verification.md)，详细命令以 `python3 scripts/verify.py list` 为准。

| 能力 | 当前实现与使用边界 | 验证入口/现有证据 | 尚未由本轮证明 |
| --- | --- | --- | --- |
| 首次使用与项目引导 | **v0.1.11 发布**（M2-01～03）；按角色准备、三类导入/持久恢复、配置确认和可编辑首任务/交付路径 | Go onboarding/创建/导入回归；browser.onboarding 三语场景 | 真实模型首任务和 5 名真实用户观察仍缺；容器资源按创建时系统设置生效 |
| 空间创建、启停、空闲回收 | workspace 服务、每会话锁、持久目录；停止不删除文件 | Go workspace/server；Docker release.server | 本轮完整生产容量/长期负载与重启窗口 |
| Claude / Codex 网页聊天与终端 | Claude stream-json，Codex app-server/exec；tmux 持久终端 | Go agent/server；browser；docker.reasoning / terminal | 真实 provider 的所有模型/档位、长期多端连接 |
| 聊天草稿与持久发送恢复 | **v0.1.11 发布**（M3）；未发送草稿与冻结待确认副本分离，7 天正文保存、查询优先、显式核对、附件复查和退出清理 | web.drafts / outbox；Go receipt/usage/draft；三语合成浏览器；integration.chat / docker.chat-reliability 联合真实 Go API/SQLite/浏览器、SIGKILL/SIGTERM 与账本 | 联合矩阵执行端为合成 Docker/CLI；不代表真实 provider、物理断电/网络或触屏；旧 WS 无持久确认 |
| 空间名称搜索与状态筛选 | **v0.1.11 发布**（M3-04）；名称子串与运行/休眠/停止组合筛选，页面内条件，保留最近空间与线程搜索 | browser.workspace-filter：三语百空间、轮询/改名/删除/键盘/窄屏/退出；本机测量见 M3 | 约定设备/阈值、真实触屏与真实服务端百空间负载仍待验 |
| 用户、账号池与授权 | 属主与账号准入；撤权不追回已交付凭证 | Go credentials/server/store；账号浏览器合成流程 | 真实 OAuth 轮换链与提供方策略变化 |
| 管理员离线密码恢复 | **v0.1.11 发布**（M1）；停服锁、隐藏输入、事务撤销令牌 | M1-01：macOS 与 Linux 隔离恢复、PTY、回滚/数据保留/race | 对生产实例执行恢复；本轮未操作生产密码 |
| 错误码、操作 ID 与恢复建议 | **v0.1.11 发布**（M1）；已覆盖登录、启动、聊天及鉴权，非全量 API | M1-02：Go/TS v1 样本、三语浏览器、六类合成错误 | 所有旧客户端版本的冻结完整矩阵；错误 ID 不是消息幂等键 |
| 分层环境诊断 | **v0.1.11 发布**（M1）；实例/自己的空间/CLI；模型始终未检查 | M1-03/04：权限白名单、root/非 root、WS、三语导出 | 实际 provider 授权、所有文件 ACL、生产反向代理 |
| 文件、归档、预览与模板 | 受限目录、拒绝链接逃逸、原子写入与上传边界 | Go safefs/archivex/server；browser.preview；docker.filesystem | 所有磁盘/文件系统/浏览器组合和超大项目体验 |
| Git、远程连接与变更审查 | Git 在容器执行；凭证受控网桥；不降级宿主 Git | Go gitx/gitaccess/server；browser.git；现有 Linux Git CI | 企业 SSO、真实私有远程和所有托管平台组合 |
| 技能、MCP、远程浏览器 | 已有模板/官方目录、Claude MCP 管理与可选浏览器镜像 | browser 主场景；docker.mcp / mcp-server / remote-browser / browser-proxy | 任意第三方 MCP、真实 OAuth 页面与网站兼容性 |
| 用量与额度 | 整数微美元、持久去重、账本同事务；终端只记不扣 | Go usage/store/server；价格/费用浏览器回归；合成修复脚本 | 未知上游事件形状；真实计费端对端不由本轮替代 |
| 账号出口与 abox-link | 外网出口代理；透明 IPv4/TCP 默认；兼容代理/映射保留 | Go linkapp/netaccess/tunnel；browser.link-i18n；docker.transparent | 任意公司内网、所有代理软件或 UDP/ICMP/IPv6 隧道 |
| 备份、恢复与部署 | 系统/完整备份有不同范围；版本目录与旧源码布局分开 | Go backup/app；backup、policy、release 组；M4 docker.recovery-drill 真实 systemd 系统/完整恢复、规模/计时/账本/文件核验（托管 Release candidate 已运行）；M6-07 真实 v0.1.11 → v0.1.10 发布包回滚演练 | 当前生产数据的恢复演练、实例自己的 RTO/RPO |
| 候选 Agent 行为验证 | **v0.1.11 发布**（M4-04）；11 项真实 CLI＋合成上游检查，验证后才切换不可变 ID | docker.cli-candidate、ui.image-update-gate；Go/策略/race；[M4 证据](milestones/m4.md) | 无真实 provider 调用；候选不自动成为固定基线；托管 Release candidate 已对固定镜像运行门槛 |
| 三类更新说明 | **v0.1.11 发布**（M2-04）；服务端/镜像/桌面分别展示版本观察、兼容边界和重启范围；沿用原更新器 | Go update-components 权限/白名单/配置变化；browser.browser；桌面 renderer build/Vitest | 镜像标签不是协议通过；网页不能读取本机安装版本；不替代真实升级、签名或设备验收 |
| 桌面项目/终端/同步与恢复 | 同步默认关闭、显式绑定和计划；原生文件/凭证边界 | 当前 desktop-unit；历史三架构原生、旧服务端与跨 OS 报告 | 正式签名更新、最低系统、真实 IME/拖放/休眠、安全软件、物理断电 |
| 多语言 | 网页、abox-link 与桌面源码三语；用户内容/协议不翻译 | web 词典、browser 三语、desktop-unit | 旧桌面包不会因服务端更新获得新 UI |

## 尚未交付或仍需外部验收

- M1：统一入口和当前清单已建立，但仍缺 5～10 次真实任务/故障观察及实际评审、招募负责人；不能用本地脚本补齐这些样本。
- M2：按角色的首次准备引导已实现；空/上传/Git 创建及收据恢复已进入 M2-02 验收；完整配置确认和更新范围说明已通过自动化；5 名未参与开发用户的观察尚未开始。
- M3：草稿隔离、持久接收/消息 ID、查询恢复和核对界面已接入；空间搜索筛选已通过三语百空间合成验证；完整联合自动矩阵已通过，百空间性能设备/阈值仍待确认。错误关联 ID 与消息去重 ID 继续分开。
- M4：[实现与证据](milestones/m4.md)已覆盖聊天业务编排/输入/流式渲染/设置生命周期拆分、版本化契约、三层验证、候选 CLI 的 11 项门槛，以及真实 systemd schema 9→12 升级和系统/完整恢复。恢复测量使用 32 空间、611 个合成文件；固定本机 Linux runner 的 20 次实际采样已通过，管理员 RTO/RPO 约定仍待完成。托管 Release candidate 已在 `cdaa457` 运行恢复演练与候选门槛，但合成规模的计时不构成生产 SLA；开发版生产交付见[交付记录](milestones/delivery-20261005.md)，相关改动已随 v0.1.11 发布。
- M5：[首批交付](milestones/m5.md)统一了桌面/网页的休眠状态、三语错误解释和操作编号；后续批次补齐上传/下载/WS 握手错误，已显示提示随语言更新，原生 WebView 三语自动化通过，Rust 边界只转发安全字段；Apple/Windows 正式签名、公证、最低系统实机、正式旧包升级、正式多平台桌面三语发包与完整迭代周期试用仍待完成；本地 macOS arm64 新测试包及公开 0.1.2→候选的隔离包替换已通过。源码和历史未签名自动化不替代稳定版门槛。

每次更新本表时，同时记录：实现/版本来源、此次实际运行的验证配置组、报告、跳过项、外部依赖和未验收项。历史设计与过程文档保留原日期；有冲突时先核对代码与对应版本的实际证据，再更新本表。
