# 开源重构设计与实施计划

状态：实施中。基线：`21fac9a`；工作分支：`refactor/opensource-foundation`。

本设计承接 2026-09-23 的代码审查。创建分支时工作区已有主控制台、abox-link 面板及图标相关未提交改动，这些改动保留，不作为本次后端重构的成果。未进行生产部署。

## 目标与约束

首个开源版本面向个人自托管和可信小团队。保持 Go 单二进制、SQLite、本机 Docker、持久工作区和可选 abox-link 的部署模型。验收目标是：新用户可以按文档安装指定版本、完成首次使用、升级，并在干净机器上恢复备份。

采用模块化单体，逐个提取业务职责。每个实施单元需具备可审查的代码、针对性测试、兼容说明与回退条件，避免同时修改目录、协议和数据格式。

必须保留的行为：

- HTTP/WS 路径和响应兼容；必要的行为变化同时更新用户文档。
- 容器挂载点 `/workspace`、`/home/agent`、`/shared` 保持不变。
- 已有 `config.json`、SQLite、JSONL 和用户目录可继续使用。
- 用量插入与额度扣减同事务；没有额度行表示不限额；终端补记只记账。
- 凭证刷新先收敛、再刷新、立即播发；账号环境变量仅在 exec 时注入。
- 主控制台继续提交编译后 JS，直到 Release 构建接管前端构建。

## 安全边界

容器中的代码、Git 配置、hook、过滤器、文件路径和 CLI 输出均不可信。处理这些输入不能使用户代码在宿主机服务进程权限下执行。

共享订阅凭证会交给容器内的用户进程，因此当前模式需要信任账号使用者。账号授权范围能控制谁获得凭证，不能隐藏已经交给进程的凭证。面向不可信用户的托管服务需要另行设计隔离和凭证代理，不能仅依靠网页权限或余额判断。

### Git 执行边界（首个实施单元）

将所有网页 Git 命令迁入对应会话容器，以 `1000:1000` 执行。新增 `internal/gitx` 管理命令构造、相对路径约束和执行依赖；HTTP 层保留属主校验、仓库选择和响应转换。

- 宿主机只做目录发现和文件读取，禁止以宿主机 Git 作为降级路径。
- Git 请求按需启动会话；启动前检查额度，执行期间持有活动引用，避免空闲回收。
- Git 进程使用清理后的环境，不注入账号、代理或隧道凭证。仓库过滤器仍可能读取容器内已有文件，这是容器内权限，不是凭证保密保证。
- 网页提交禁用 hook 和签名；需要这些功能的用户在终端执行提交。
- 请求参数通过 argv 传递，不拼 shell；Git 命令在容器内受时间限制，输出在宿主机受大小限制。
- 无 HEAD 的新仓库有明确 diff 回退；其他错误正常报告。
- 保持多仓库选择、未知仓库写入拒绝、未跟踪文件逐项列出的行为。
- 目录扫描与文件查看的宿主机路径竞态在后续受限文件 API 单元处理，不能把 Git 执行迁移描述为整个文件系统安全工作完成。

## 目标模块与依赖

```text
cmd/agentbox -> internal/app -> HTTP/WS + 业务服务 + 基础设施
HTTP/WS -> workspace / credentials / usage / gitx
业务服务 -> 消费方定义的窄接口
store / dockerx / agent -> 实现存储、容器运行和 CLI 协议适配
```

`internal/server` 最终只负责鉴权、请求解析、响应和连接管理。禁止业务包反向依赖 `server`；不要创建通用万能接口或重复的事务层。

| 模块 | 职责 | 迁移时必须验证 |
| --- | --- | --- |
| `app` | 依赖组装、启动、后台任务和关闭 | 部分初始化失败清理；停止新请求后关闭 WS/隧道并收尾任务 |
| `workspace` | 创建、启停、删除、模板和凭证播种编排 | 并发启动幂等；停止/删除竞态；挂载与属主 |
| `credentials` | 账号凭证、刷新、同步和账号级锁 | 轮换顺序；刷新响应合并；代理失败不得直连 |
| `usage` | 事件归一化、计价、终端扫描、结算编排 | 去重；缓存 token 语义；入账与扣减原子性 |
| `gitx` | 容器内 Git 执行策略 | 不调用宿主机 Git；路径/参数隔离；失败不降级 |
| `agent` | Claude/Codex 命令、协议和能力 | app-server 回退；续聊；中断；用量样本 |
| `store` | SQL、版本化迁移和事务 | 旧版本升级；重复启动；失败回滚 |

## 源码与运行目录

源码目标结构（按提取顺序创建，不预建空包）：

```text
cmd/{agentbox,abox-link}/
internal/{app,server,workspace,credentials,usage,gitx}/
internal/{agent,store,dockerx,archivex,tunnel,linkapp,web}/
internal/store/migrations/
web/src/{app,shared,features}/
images/agent/
deploy/
scripts/
docs/{architecture,adr}/
third_party/
.github/
```

前端先拆分 chat/settings 的连接、状态、渲染和交互职责，逐步引入显式初始化/清理。保留原生 ES Modules 的内容哈希缓存机制；abox-link 面板继续独立嵌入与发布。

生产目录目标：

```text
/etc/agentbox/config.json
/opt/agentbox/releases/<version>/
/opt/agentbox/current -> releases/<version>/
/var/lib/agentbox/{state.db,creds,home-template,users}/
/var/cache/agentbox/marketplace/
```

现有仓库内部署模式继续兼容。安装、构建、发布、备份脚本需接受显式配置/数据/输出目录，逐步消除对当前工作目录的依赖。动态设置仍写回配置时，必须保证配置目录允许原子替换。

数据迁移流程：先验证备份 → 停服务并停止相关会话容器 → 迁移且保留 UID/GID/权限/mtime → 更新路径 → 重建旧 bind mount 容器 → 验证会话、凭证和历史 → 保留旧副本至验收。不得仅移动目录后假定旧容器挂载已经更新。

## 实施阶段与验收

### A：安全与恢复

- [x] A1：Git 执行迁入容器，提取 `gitx`，增加回归测试和使用说明；最小 Linux Git 容器验证通过。
- [x] A2：新增 `safefs` 目录句柄封装，迁移文件、归档、技能、预览、模板/凭证和终端 transcript 读取；并发链接替换及 Linux 回归通过。
- [x] A3：系统/完整备份内置到 Go 二进制，覆盖全部配置凭证根与模板；完整备份含 users，输出格式版本、清单与 SHA-256。
- [x] A4：恢复仅发布到新目录，验证清单/哈希与 SQLite 完整性；完整备份需持有服务 flock 并检查 Docker 挂载；系统/完整合成数据恢复演练通过。
- [x] A5：账号按全体/指定用户/仅管理员授权；管理界面、执行入口与凭证同步校验，兼容既有共享池。

验收：安全回归通过；在 Linux 上完成容器内 Git 验证；在隔离环境完成一次备份恢复。真实凭证不进入测试夹具。

### B：开源发布基础

- [x] B1：负责人选择 Apache-2.0；补 LICENSE/NOTICE、第三方来源、许可全文、版本与哈希清单。
- [x] B2：已扫描全部已获取历史/分支/标签、跟踪源码、解包产物及二进制可打印内容；无未审查密钥命中。旧生产域名历史披露列为公开前人工决策，未改写历史。
- [x] B3：贡献、安全反馈、Issue/PR 模板和变更记录已加入；GitHub 私密漏洞报告需发布前启用。
- [x] B4：Tag 候选工作流、7 平台构建、版本/校验和实现；正式 Apache-2.0 候选包校验和 Linux 无源码安装/会话冒烟通过，未运行远端 CI。
- [x] B5：固定 Claude 2.1.280 / Codex 0.145.0 / Node 22.23.2 digest，默认禁用追新，保留旧镜像标签；合成 Linux 会话链路验证通过。

验收：干净 Linux 环境能从发布包安装，不需要源码构建；上游 CLI 的分发条件已核对。许可证选择与公开发布由项目负责人决定。

### C：后端模块化

- [x] C1：新增 app 管理数据锁/启动清理；后台任务、WS、代理/隧道和在途回合统一关闭，等待已观察到的用量落库。
- [x] C2：提取 workspace，集中创建/启停/删除/播种/回收、可取消会话锁和活动引用；运行时失败不写假成功状态。
- [x] C3：提取 credentials，统一凭证读写/续期/同步及账号锁；修复刷新后播发受 mtime 容差抑制的问题。
- [x] C4：提取 usage 的事件/定价/结算/终端扫描；用量、价格快照和扣款同事务，新历史明细使用入账单价。
- [x] C5：引入 user_version 连续迁移，旧库基线升级、拒绝高版本、失败/进程中断回滚及回退说明完成。
- [x] C6：Adapter 统一能力/聊天/标题/事件，保留合成协议样本，补 Codex TUI 终端聚合、去重和日期目录监听。

验收：每次提取后 API/WS 行为兼容，现有测试通过；迁移覆盖历史库、重复启动和迁移中断；新增业务模块不依赖 HTTP 包。

### D：部署、前端与运维

- [x] D1：运行目录独立，提供旧部署迁移工具和按版本发布/回退。
- [x] D2：前端按功能提取，显式初始化/销毁，收窄共享状态；增加关键浏览器流程测试。
- [x] D3：每用户/全局并发容器限制、磁盘使用和回收策略。
- [x] D4：终端扫描移出用量查询同步路径，返回同步时间；增加脱敏诊断与运行版本信息。

验收：执行完整安装、升级、备份、恢复流程；前端编译产物一致；验证旧 abox-link 的兼容范围。

## 验证规则

每个代码单元执行针对性测试及 `go build ./...`、`go test ./...`、`go vet ./...`。前端改动执行 `npm run check`、`npm run build` 并提交产物。并发或生命周期改动增加 race 检查。

真实 CLI 付费调用、生产部署和发布公开产物不属于普通本地重构验证。macOS 的构建与测试不能替代 Linux Docker、systemd 和恢复演练；未执行的验证须明确记录。

## 实施记录

- 2026-09-23：创建重构分支与设计文档；完成 A1 的实现及本地验证。新增容器命令执行适配器、`gitx`、额度入口检查、Git hook/环境隔离、首次提交 diff 与丢弃失败回归，并接入 CI 容器冒烟步骤。
- 验证通过：`go build ./...`、`go test ./...`、`go vet ./...`、`npm run check`、`go test -race ./internal/gitx ./internal/dockerx`。
- 本机 Docker 的 Linux arm64 最小 Alpine Git 容器实测通过：uid=1000、提交/diff、hook 禁用、过滤器不继承测试密钥环境变量。临时容器已清理，测试镜像保留用于复用。未调用模型、未挂载真实账号或工作区。
- 仍待验证：生产 Debian Agent 镜像与完整会话启动链路、systemd 部署；新 CI 步骤尚未在远端运行。B 阶段本地实施与验证已完成，D 阶段仍待实施；公开发布前事项见开源审查记录。

- 2026-09-23：完成 A2。新增 `internal/safefs`，移除检查后返回绝对路径的文件助手与旧 rename 降级实现；普通文件读、原子保存、移动/删除、归档、预览、技能与凭证统一使用固定目录句柄。详见 [文件系统边界](filesystem-boundaries.md)。
- A2 验证通过：Go build/test/vet、前端类型检查、safefs/archivex/agent/server 的 race 检查；`scripts/test-filesystem-linux.sh` 在无宿主挂载、无外网的 Linux arm64 容器运行四个包的完整测试通过。最小镜像补齐 tzdata 后解决了时区测试环境缺失；未连接生产或调用模型。

- 2026-09-23：完成 A3/A4。新增 `internal/backup`、`agentbox backup` / `backup-verify` / `restore --to`；定时脚本改为编排内置命令，支持独立配置/二进制路径，分别轮转系统与完整备份。SQLite 使用在线 Backup API，临时快照转独立 DELETE journal 模式，保留源库 WAL。
- A3/A4 验证通过：Go 全量构建/测试/vet，backup/safefs/cmd 的 race；`scripts/test-backup.sh` 实际命令演练覆盖 WAL、外部凭证、模板、哈希验证、恢复、拒绝覆盖与轮转；启用 Docker 检查的完整工作区恢复通过。`scripts/test-filesystem-linux.sh` 在 Linux arm64 最小镜像运行五个包（含 backup）的全量测试通过。
- 备份恢复验证使用合成数据和隔离临时目录，未读取真实凭证、未停止生产服务；实际生产数据的恢复演练、systemd 配置切换和新 CI 远端运行仍待验证。恢复不自动接管原容器，同一 daemon 上的会话 ID 冲突须按部署手册处理。C/D 阶段仍未完成；许可证、扫描与发布基础的后续实施见 B 阶段记录。

- 2026-09-23：完成 A5。`Account.access` 支持 `all/users/admin`，缺省保持共享，管理员始终可用；账号列表过滤并隐藏授权名单，普通用户空间计数限本人。创建/启动/聊天/标题/终端/Git/订阅额度及 exec 环境生成统一检查账号使用权，凭证同步重新读取当前策略。文件和历史继续按空间属主访问。
- A5 界面：账号行新增「使用范围」，独立 `web/src/account-access.ts` 负责编辑；设置页仅增加入口，TS 与 JS 一同提交，原有未提交 UI 改动保持独立。非法用户提示、名单去重、保存回显、取消及 390px 窄屏布局通过本地 Playwright 合成 API 页面验证；截图保存在本机 `output/playwright/`，不进版本库。
- A5 验证通过：Go 全量 build/test/vet，config/server race，前端 check/build；仅暂存源码在临时目录编译后与全部暂存 JS 一致。Linux arm64 隔离容器的 safefs/archivex/agent/server/backup 回归通过。HTTP/WS 测试覆盖撤权后的既有连接拒绝、授权持久化、普通用户越权、撤权后的双向凭证同步禁止及文件属主边界。
- 撤权按操作准入生效，已通过检查的在途操作、容器 CLI/tmux、已交付凭证和既有代理连接不自动撤回；彻底撤销须停容器并轮换上游凭证。旧二进制不识别授权字段，不能直接回退后继续共享服务。未部署生产、未调用真实模型；浏览器使用合成 API，未验证生产 Debian 镜像完整会话链路或远端 CI。

- 2026-09-23：阶段 B 实施中。负责人选择 Apache-2.0；补齐许可证、第三方资产比对及 33 个链接 Go 模块许可、贡献/安全模板、版本元数据、7 平台候选构建和扫描流水线。CLI 追新改为显式开启，修复宿主 CODEX_VERSION 污染默认镜像版本的问题，构建覆盖使用 AGENTBOX_ 前缀。
- B 本地初步验证：Go 全量 build/test/vet、固定镜像策略、第三方哈希/链接模块覆盖；隔离 fixture 的 7 平台打包及校验通过，Linux arm64 包备份恢复通过；实际 Debian 固定镜像的 CLI/Node 版本和合成空间登录/启动/挂载/UID/文件/Git/停止/删除通过，无模型调用。正式 LICENSE 已落地，正从干净提交重建最终候选。详细审查与公开前限制见 [开源审查](opensource-audit.md)。

- B 最终验证：`5dceb81` 干净 worktree 构建 `v0.1.0-rc.1` 本地候选（未创建 tag），七个平台包与 SHA256SUMS 验证通过。Linux arm64 Alpine 中直接运行包内二进制并完成合成 SQLite 备份/校验/恢复且核对数据；真实 Debian 会话镜像的合成服务链路再次通过。gitleaks 对当前全部 refs、跟踪源码和解包候选（含 strings 提取的二进制内容）均为 0 个未审查命中。
- 候选位于本机 `/tmp/agentbox-b-final-candidates`，扫描报告在 `/tmp/agentbox-b-final-audit`；测试容器/命名卷已清理。未推送、未创建 GitHub Release、未调用模型或部署生产。B 的本地验收完成不表示已对外发布；历史域名披露、私密漏洞报告设置、远端 CI 及生产 systemd 验证仍按 [审查记录](opensource-audit.md) 在对应发布/部署步骤处理。

- 2026-09-23：完成 C1。信号下沉到入口、组装和数据锁迁入 `internal/app`，`Server.Close` 统一任务准入、取消、连接关闭及依赖释放。初始化失败关闭已创建资源；Docker attach 随 context 取消关闭。聊天中断最多等待 2 秒，随后取消并等待结算；现有容器/tmux 保留，不保证强杀所有 CLI 子进程。
- C1 验证：Go 全量 build/test/vet、前端 check、server/dockerx/app race，Linux arm64 无网络容器七包回归；HTTP WS/TCP/CONNECT/yamux 关闭、延迟结算/额度同事务、超时不早关库、新任务拒绝与初始化锁释放测试通过。真实 Linux Docker 合成服务验证 SIGTERM 退出码 0、WS 断开、同卷重启取得锁、会话容器和 tmux 存活。未调用模型、未部署生产。C2–C6 继续实施。

- 2026-09-23：完成 C2。`internal/workspace` 使用消费者定义的 Store/Runtime 窄接口，集中会话创建、模板→凭证→默认模型播种、幂等启动、停止/删除及空闲回收；授权与凭证同步暂通过注入函数保持边界，C3 再提取凭证服务。停止/删除与启动共用锁，等待锁支持 context 取消；Docker 失败保留会话，已不存在容器按幂等成功处理；清理目录使用受限句柄。
- C2 验证：全量 Go build/test/vet，workspace/server/app race，合成 Linux Docker 会话+SIGTERM 重启验证通过。并发启动/删除与 UID 播种测试在 Linux 容器运行；发现旧 Docker mock 未读完请求体即关闭会引起 TCP reset，已修正测试夹具并重跑 Linux 八包回归。无真实凭证和模型调用。

- 2026-09-23：完成 C3。`internal/credentials` 承接 OAuth 池文件与 Codex Key/provider 读写、状态读取、刷新合并、双向同步和账号级可取消锁；workspace 使用带 context 的同步接口。HTTP 层只保留授权流程、参数/响应与出口客户端适配，不引入业务模块对 server 的反向依赖。
- 保留先同步→判断并刷新→立即播发的顺序，续期保留订阅档位/scopes/未知字段。刷新成功和新 OAuth 保存后的播发不再受日常同步的 2 秒 mtime 容差抑制；仍重查授权并仅更新已经播种的 home。Codex 两个文件逐文件原子写入，不宣称跨文件事务。
- C3 验证：Go 全量 build/test/vet、credentials/workspace/server race，Linux arm64 隔离容器回归；20 并发请求只刷新一次、立即播发与撤权过滤、锁等待取消、代理失败不直连、字段合并、受限读取和 Codex 原配置保留测试通过。实际 Linux Docker 合成空间 CRUD/文件/Git/SIGTERM 重启再次通过；未使用真实凭证、未调用模型、未部署生产。C4–C6 后续实现见下方记录。

- 2026-09-23：完成 C4–C6。`internal/usage` 承接回合归一化、定价、结算与终端扫描，server 保留接口鉴权/参数/响应。新增 PriceSnapshot 保存来源、价格键、两档单价与阈值；终端续写在 store 的 upsert 事务内沿用首次快照，旧行不伪造历史价。正常用量仍由 InsertUsage 同事务记账扣款；无额度行不限额、余额允许负数、终端只记账不变。
- schema 版本 1 引导旧库并补齐遗留字段，版本 2 添加价格快照。所有待执行迁移及 user_version 同事务提交，读出不支持的高版本后拒绝写入；测试覆盖 SQL 失败与迁移进程 os.Exit 中断。回退说明见 [数据库迁移](database-migrations.md)，旧二进制无版本保护时仍不得直接连接新库。
- Agent Adapter 提供能力、命令和事件解码，保持 Codex app-server 优先/exec 回退、续聊及中断。Codex 终端从日期目录解析 codex-tui rollout，以累计 token 差值排除重复额度通知，按 thread/turn/model 聚合并以空间区分去重；缓存输入和 reasoning 语义保持。样本为按固定版本公开结构构造的合成记录，没有读取真实 transcript 或调用模型。
- C4–C6 验证：Go 全量 build/test/vet，usage/store/agent/server race，前端 check/build；Linux arm64 隔离容器 11 包回归通过。实际 Debian 会话内注入合成 rollout，经真实 HTTP 查到 3 行终端流水，修改价目表后费用/单价保持快照，SIGTERM 与同卷重启继续通过。Linux 脚本现同步复制 testdata；未执行远端 CI 或生产部署。


- 2026-09-23：完成 D1–D4。D1 新增 `deploy/release.py` 独立 `/etc`、`/opt`、`/var/lib`、`/var/cache` 布局，版本目录原子切换、schema/兼容代号检查、迁移停服务与相关 bind mount 容器、备份验证、UID/GID/mode/mtime/符号链接/历史/凭证保留；旧 `install.sh`/`deploy.sh` 保持兼容。D2 抽取 `ChatConnection`、可取消 `Poller`、设置运维 feature 和 app 生命周期；S 不再保存 chat/settings/poll timer；补合成 API 浏览器流程。D3 增加全局/每用户容器容量与数据盘准入、后台磁盘统计、市场缓存清理。D4 用量查询只排队后台扫描，响应下发同步状态；增加白名单诊断、版本/schema 信息。
- D 验证：`go build ./...`、`go test ./...`、`go vet ./...`、config/workspace/usage/server/dockerx race、`npm run check`/`npm run build`、Linux 文件系统 11 包回归、真实 Docker 会话容量/后台 Codex 用量/快照/重启冒烟、合成 Playwright 浏览器回归、部署策略测试、Linux 容器迁移/版本切换/回退演练通过。迁移脚本的 systemd 调用在隔离演练中模拟；未执行生产迁移、真实主机 systemd 或远端 CI。

- 2026-09-24：完成首台真实 Linux/XFS + systemd 生产迁移，运行 `v0.1.0-rc.2`（`55a9624`）。真实数据约 8.1 GiB / 12.9 万条、可用盘约 5 GiB，先补充 `--reflink --snapshot-backup` 自包含恢复目录及逐文件 SHA-256/元数据验证，保留原目录和恢复点；同时修复发布脚本循环变量覆盖造成 Linux 包同名的问题，增加平台文件名/重复包检查。实际生产迁移前先在该主机合成目录验证 reflink 路径；未读取真实凭证内容进入本地夹具。
- 生产验收：systemd 使用 `/opt/agentbox/current/agentbox -config /etc/agentbox/config.json`，数据与缓存分别位于 `/var/lib/agentbox`、`/var/cache/agentbox`；32 会话、5 用户、6 账号保留，密码/额度/账本逐行一致。通过既有登录、历史读取、临时无凭证空间的 UID/三处挂载/文件/Git/chat-term WebSocket 验证并清理测试资源。后台补齐 4 条终端用量且不扣额度。每日系统备份已成功执行，追新 timer 禁用，保留原运行镜像而不降级 CLI。原源码 tracked diff 未变；一个临时未跟踪 `.claude/` 条目在窗口内消失，记录在主机私有验收报告，迁移工具未清理源码目录。内网首页 200 / ping 204，公网首页 200、主机经公网域名 ping 204；开发机公网探活曾有间歇 TLS/HTTP2 传输错误，服务无重启或启动失败。

- 2026-09-30：在线升级允许标准版本目录中的 dev、预发布和 dirty 构建切换到最新正式版，不要求目标版本号高于开发版；正式版间仍禁止降级，目标校验、备份与配置/schema 兼容检查保留。Go 全量 build/test、前端 check/build、`scripts/test-update.py` 和 `scripts/test-deployment.py` 及合成 API 的 Playwright 浏览器回归本地通过；升级脚本使用模拟下载/systemd，未执行生产切换或真实 Linux/systemd 验收。

- 2026-09-30 生产补充验收：独立版本目录部署 `v0.1.6-dev-stable.20260930.090606`，复用 `release.stage/activate` 完成配置/schema 检查、系统备份及验证、真实 systemd 重启。服务 active、NRestarts=0、内外网 HTTP 200，启动日志正常，嵌入 updates.js 哈希与候选一致。管理员升级状态返回 supported=true；正式版本检查返回 latest_version=v0.1.6、available=true、comparable=false、无错误。当前仍运行开发构建，尚未执行开发版到正式版的实际下载与切换。
