# 多 Agent 协作专项 C1～C3

[设计方案](../architecture/agent-collaboration.md) · [第二轮计划](../roadmap-2.md) · [统一验证](../verification.md)

创建日期：2026-10-11。状态：C1 开发中，起始源码基线 `fa170409`。C1 后端与三语网页流程已形成可端到端测试的开发预览，包括平台验证后评审、答疑/返工、配置切换、异常核对、差异与删除。当前不是发布候选，尚未完成真实 provider 验收、完整故障矩阵或部署。[开发预览使用说明](../collaboration-preview.md)。

## 当前实施记录

2026-10-11 第一批基础实现（尚未完成 C1，以下不代表用户可用）：

- 角色/账号/模型独立配置与严格交接解析，拒绝重复 JSON 键、未知字段、错误角色、无效引用形状和超限输出。schema 与生成的 TS 声明位于 `contracts/collaboration-v1.schema.json` / `web/src/contracts/collaboration-v1.d.ts`。
- Codex 协作专用严格续聊：恢复失败或线程 ID 改变时拒绝新开线程；执行器可禁用 exec 回退。普通聊天保留原有兼容行为。最终答复采集独立于增量/工具输出，保留 Codex phase 与消息 ID。
- `docker.collaboration-cli` 已登记，真实本地镜像 `sha256:2dcb5f36a55f8175830fc84714f2b4ba6ef801f17278eb3f0b64fdc52d56a2bc`（Claude 2.1.280 / Codex 0.145.0）在无网络、无账号挂载的隔离容器中通过双类型计划/结果 JSON 交接和续聊。它使用合成上游，不证明真实模型稳定遵守输出格式，也尚未覆盖协作只读 profile。
- schema 13 增加任务、版本化配置、参与者、步骤、消息、动作、事件及产物元数据。已验证创建和动作幂等、配置/参与者/步骤事务、步骤 CAS、交接与下一步骤原子提交、重启围栏、用户身份隔离和删除时清正文。
- 服务启动在 Docker 初始化前恢复协作状态；尚未接入实际调度器、受管挂载、配置替换、预算结算闸门、验证交付和网页。中间状态仅供继续开发，不作为发布候选。

验证记录：

- `output/verification/20261011T001436Z-0e30dd7b/report.json`：协作 CLI 探针通过。
- `output/verification/20261011T002914Z-c4a23648/report.json`：原有 CLI 候选门槛通过，覆盖中断/MCP/用量等已有行为。
- `output/verification/20261011T003218Z-8ce54417/report.json`：完整 quick，26 passed；Linux 专属 chown 检查在 macOS 为 not_applicable。含 Go 构建/vet/全部本机单测、前端契约/类型/产物及本地策略。
- `go test ./internal/store -run Collaboration -count=1`：并发创建、动作重放、事务故障回滚、身份及重启隔离、交接排队通过。
- `go test -race ./internal/store ./internal/collaboration ./internal/agent ./internal/chat -run 'Collaboration|Handoff|FinalOutput|StrictResume|Roles' -count=1`：新增协议/存储/执行器并发回归通过。
- 首次协作探针失败记录保留于 `output/verification/20261011T001316Z-ab5d3eb5/`：合成上游改成 JSON 正文后，测试驱动器用序列化字符串查找正文导致转义不匹配；修正为逐字符串检查后通过，未放宽产品解析规则。

第一批结束时的后续安排：补齐 C1-02 的配置/答疑/取消收尾与产物、预算状态；完成 C1-03 的受管 Session 入口封锁和容器 profile；再接共同执行器、调度与网页。C1-01 仍需补齐所有协作能力的候选门槛，C1-02 仍需完整迁移/恢复与故障矩阵；两项均不标记完成。

2026-10-11 第二批实现（在第一批基础上继续）：

- `collaboration_questions` 记录问题及原问题关联，完成“辅方提问 → 主方转问用户 → 用户答复 → 主方回答原问题 → 辅方继续”的持久流转；用户答复和后续排队同事务，错误/过期问题 ID 拒绝。规划/评审中的用户答疑回到原阶段。
- 暂停记录待恢复阶段和待执行步骤，回合结束后暂停不会丢失下一步；恢复不跳过计划确认、无效交接或验证闸门。取消只登记意图，实际停止后才能收尾；执行不明须停止确认并显式核对，旧步骤保留 reviewed 状态。
- `configure` 保存新的完整角色配置版本，支持互换主辅、更换账号/模型/强度，同步撤销旧待执行步骤和旧参与者。新参与者使用新代际/独立 home/线程；旧配置、交接和费用归属保留。角色替换前由 process/stopped epoch 阻止旧进程未停止时交权。账号授权与模型能力的 HTTP 准入仍待后续 API 接入，不能把存储形状校验当作授权。
- 普通 `/sessions/{id}` 的公共入口拒绝受管参与者，列表隐藏内部空间，生命周期/容量/用量枚举仍包含它们；workspace 普通 Start/SwitchAccount 拒绝受管 Session。`QuiesceCollaboration` 持空间锁、停止 Docker、复查状态并确认当前代际，停止失败不能释放写权限。
- Docker 协作 profile 使用不可变镜像 ID、主方只读项目/辅方可写项目、独立 home/scratch、只读输入；清除镜像隐式环境和启动命令继承，保留原资源/browser/quota 策略，拒绝运行中复用、外来容器和名称冲突清除。私有工具缓存按参与者隔离，位于原用户缓存配额根下；公共工具继续只读。
- 快照以 safefs 固定句柄、有界目录读取生成独立代码树/清单摘要，递归同步后不覆盖发布。保留普通文件/执行位/安全相对链接，拒绝硬链接、外部/循环/链式越界链接、嵌套仓库、特殊文件、超量及取消。VerifySnapshot 核对清单摘要和实际树，缺失/额外/篡改文件拒绝；产物登记只能使用服务端合成路径，ready 与文件发布分别落盘，重放不能替换已登记内容。

第二批证据：

- `output/verification/20261011T004706Z-b4256a33/report.json`：`docker.collaboration-mounts` 实际 UID1000 Docker 只读/可写项目、只读附件、home/scratch 写入、运行中拒绝复用和容器停止通过；仅私有临时目录，network=none，无账号/provider。该场景未启用共享工具，缓存隔离由工具库单测覆盖，尚非工具挂载的真实协作验收。
- `output/verification/20261011T005123Z-c7df3a2b/report.json`：quick 26 passed，1 个 Linux 专属项 not_applicable；覆盖角色控制/挂载/发布基础。此轮启动后增加的快照完整性读取与产物登记另跑定向 Go 回归/vet，不把此前报告冒充最后新增代码的完整验证。
- `go test ./internal/store ./internal/server ./internal/workspace ./internal/dockerx ./internal/toolstore -run 'Collaboration|Participant' -count=1`：状态控制、普通路由封锁、停止确认、容器失败清理及缓存隔离通过。
- `go test ./internal/collaboration ./internal/safefs -count=1`：发布/读取完整性、链接展开越界、超限、取消、不覆盖及原 safefs 回归通过。
- `go test ./internal/store ./internal/collaboration -run 'Collaboration|Snapshot' -count=1` 与相关包 vet：后补快照完整性/产物登记通过。
- `go test -race ./internal/store ./internal/collaboration ./internal/workspace ./internal/dockerx ./internal/toolstore -run 'Collaboration|Snapshot|Participant' -count=1`：第二批最终改动的并发检测通过。

下一步优先接 C1-03/04 的实际源仓库导入、受管启动与凭证/CLI 配置边界、共同回合执行，然后 C1-05 调度器与 C1-08 网页。预算/时限、验证执行与最终交付、创建放弃/删除及完整备份恢复仍未收口；现有基础函数及定向测试不能算 C1 完成。第二批未调用付费模型、未提交推送、未部署。

2026-10-11 第三批实现与恢复补强（延续第二批，尚未完成 C1）：

- 接入默认关闭的 `collaboration_enabled`、独立 HTTP v1 创建/查询/动作回执/事件及三语网页。主辅各自选择 Claude/Codex、账号、模型、强度；交换时整套配置一起交换。创建/动作 ID 在页内保存后发送，失联和刷新只查原 ID。网页按事件序号分页追赶，不再只显示前 100 条；待答问题来自服务端当前 `pending_question`，问题更换清空旧答复。
- 源项目使用离线只读 Docker helper，通过 NUL Git tree/有界 archive 验证固定提交内容，再建立私有仓库。服务端不使用宿主 Git 降级。运行前检查嵌套项目 CLI 配置，拒绝配置别名、自动 hooks/MCP/技能继承；启动参数的镜像、项目、home、输入和临时目录绑定到当前任务/参与者。
- 协作回合复用 `chat.Service` 的历史落盘、原用量计量和结算；只允许持久步骤 CAS 获得执行资格。两角色独立 home/线程、逐回合停容器再交接；原生 Codex 恢复失败不能另起线程或降级 exec。调度器跟随服务生命周期，启动不自动重放中间任务。
- 主方评审通过后执行独立验证容器，再交付同一不可变快照 ZIP；验证可写副本中的原路径不可改动/删除，新增构建文件只在该副本内并随清理删除，不进入交付快照。未填写命令明确为“仅模型评审”。当前平台验证在评审**之后**，尚未把平台证据回传主方评审，仍需调整以满足 C1-07 目标顺序。
- 验证 claim 持久化随机 helper ID，Docker 创建前已经落库；创建响应丢失、服务重启、清理失败保留围栏。显式控制先按名称及标签认领、再按已检查的容器 ID 清理，不能删除外来同名容器。只有真实清理证明才能把 unknown 改为 failed；不能改为通过或自动重跑。缺快照等启动前错误也收尾验证 claim，避免永久 running。
- 备份恢复到新目录时解绑旧参与者容器/provider 会话；full 恢复保留代码和快照，未执行队列不重放，可能已经执行的步骤仍需核对，后续必须创建新配置代际。system 恢复将任务标为 `failed/artifacts_missing`，产物不再 advertised ready；只能基于重新取得的源码另建任务。启动为恢复身份更新未来动作 scope，并留审计事件，原创建输入和 ID 回执保持不变，旧实例 scope 不能操作恢复副本。
- 补充 schema 12→13 既有聊天/空间保留回归，真实 tar.gz system/full 备份恢复、随机 helper 身份/迟到结果/外来容器、缺产物验证收尾、追加日志链接/FIFO/容量边界等测试。

第三批已有证据（各报告只覆盖其记录的工作树，不能拼成“最终完整通过”）：

- `output/verification/20261011T035036Z-c4940d49/report.json`：quick 26 passed，Linux 专属 chown 在 macOS 为 not_applicable；包含完整 Go/server 单测、build/vet、网页与本地策略。运行期间仅补充文档和刷新需求回填的前端改动，最后的前端改动由 `20261011T035445Z-90d9b178` 重新验证。

- `output/verification/20261011T035445Z-90d9b178/report.json`：本批最后的前端类型/完整产物/i18n 与 Chromium、WebKit 共 5 项通过，包含刷新后需求回填；本地 WebKit 二进制补齐后重跑，不将缺条件的首轮算通过。

- `output/verification/20261011T035035Z-65749911/report.json`：四组合通过真实 HTTP 创建 → 等待用户计划确认 → 重复确认动作 → 后台调度 → 验证/交付 → 重复创建查询；合成模型调用仍为 3 次，费用流水/余额及参与者停止核对通过。Docker/CLI 为合成协议，不是实际 provider。
- `go test -race ./internal/store ./internal/collaboration ./internal/workspace ./internal/chat ./internal/agent ./internal/dockerx ./internal/backup ./internal/safefs -run 'Collaboration|Snapshot|AppendJournal|Executor' -count=1`：本批恢复/作用域/追加日志/项目配置与共享执行器定向并发回归通过。

- `output/verification/20261011T032959Z-e11344bb/report.json`：四种主辅组合运行链路通过；真实 Go/SQLite/文件/UID1000，Docker/CLI 为不执行输入命令的合成协议 fixture。每任务 3 次模型输出、3 条费用流水、余额 10000→9970，参与者停止，结果为预期内容。不是 provider 验收。
- `output/verification/20261011T034545Z-97eaf7f6/report.json`：加入验证 helper 身份和恢复改动后的四组合运行回归通过。随后将入口改为真实 HTTP 创建/计划确认/重复动作/后台调度，最终报告见下方本批收尾记录。
- `output/verification/20261011T033230Z-4d483fbf/report.json`、`output/verification/20261011T034546Z-4b684f61/report.json`：真实本地 Docker 离线 Git 导入与验证命令成功/非零退出通过，无账号、无 provider。
- `output/verification/20261011T034544Z-e74278f0/report.json`：Chromium 合成 API 下分页历史、当前待答问题/输入切换、主辅交换、独立模型、丢响应刷新和三语/窄屏通过。
- `output/verification/20261011T021926Z-3690a28f/report.json`：较早 quick 26 passed，Linux chown 为 not_applicable；不作为恢复补强后的完整验证。
- 故障证据保留：`20261011T033124Z-fe5b3ac7` 中 runtime 通过、import 失败，不能将整体写为通过；`20261011T034430Z-5cdaa19b` 是浏览器断言选中增强控件而非原生 select 导致失败；`20261011T034936Z-b99ff590` 是 HTTP fixture 复用解码目标保留了省略的旧 wait_reason，已修正为逐响应清空；`20261011T034937Z-cbd6365b` 中 Chromium 通过、WebKit 因浏览器二进制缺失失败。

第三批结束时待收口（下述第四批已推进）：平台验证证据进入主方评审、交接修复专用步骤、结果差异浏览、实时输出事件、完整任务删除/残留目录清理、可配置累计时间/等待 TTL、辅助容器容量准入、新 home/profile 的实际 CLI 候选验证，以及 HTTP 联合故障矩阵。默认关闭不代表这些缺口已验收；后续真实 provider 与发布另需明确授权。

## 第四批：后端闭环与真实后端页面验证

本批补齐可操作的 C1 开发闭环；不把无账号合成测试称为正式 provider 或发布验收。

- 平台验证提前到主方评审之前；固定快照/config 的命令、argv、退出码、输出/截断标记和耗时进入持久证据及冻结评审输入。验证失败可返工，模型 approve 不能绕过失败，未知 helper 不能继续派发。构建新增目录明确限定，既有文件仍逐项核对。
- 独立 `handoff_repair` 使用原角色原生上下文、只读快照补写交接，不再次执行可写实施步骤；修复仍占步骤与费用预算。
- 文件变更清单/文本 diff、最终 ZIP；异常回合额外生成只能用于核对的 inspection 快照、差异/下载及分页运行记录，勾选确认之后仍需新配置/规划。inspection 不替代已确认结果。
- 删除入口先记录 tombstone、清正文与访问能力，再清容器、参与者 home、工作树、快照和当前私有缓存；清理失败在列表保留重试入口，费用/请求身份不清除。删除与调度并发的再次唤醒不丢失。停止/删除前收敛当前仍获授权的轮换凭证。
- 页面接入可丢弃 SSE 输出，事件/历史仍由持久查询恢复；对关键协议身份/状态/计数做运行时校验。三语、手动核对影响说明、独立模块及编译产物同步。
- 可配置单步/总模型执行时间、等待到期暂停；步骤开始/结束独立保存，不把后来的人工核对计入模型时长。未知崩溃回合按配置时限预留。每任务登记产物 512 MiB 上限；帮助容器与参与者共用容量准入，残留 helper 继续占容量；锁顺序保持空间锁在启动闸门之前。
- 真实后端 HTTP 场景扩展为四种主辅组合及 9 个控制/故障场景，全部设置全局/用户最多 1 个运行容器：问题升级与答复、验证失败返工、交接补写、暂停恢复、预算、用量缺失、等待时取消、撤权和运行中取消。核对执行数、原账本、余额、文件、停止证明及异常副本不被提升为交付。
- 新 `docker.collaboration-browser`：真实浏览器操作实际 Linux Go handler/SQLite/调度器/文件，Docker/CLI 协议为不执行 argv 的合成 fixture。页面创建→交换主辅→重新配置并规划→确认→执行→验证→评审→差异/ZIP→刷新→删除，核对 4 次模型步骤/4 条用量、余额 10000→9960，无隐藏模型调用。控制接口仅在测试 helper 中。
- 离线 CLI 探针现在通过正式 `SeedCollaborationHome` 生成无真实账号的配置，再交给隔离容器中的真实 Claude/Codex 验证交接与续聊。未挂用户 home/账号或 Docker socket。

验证报告与边界：

- `output/verification/20261011T041742Z-fa8ab60f/report.json`：扩展 HTTP 场景、实际后端浏览器通过；当时还未加入运行中取消/inspection，后续报告单独覆盖。
- `output/verification/20261011T042228Z-895f8c6c/report.json`：含运行中取消、核对副本不提升为交付，runtime/browser 两项通过。
- `output/verification/20261011T042534Z-c53adc93/report.json`：实际全局/用户单容器名额下的 13 个 HTTP 场景及浏览器闭环通过。
- `output/verification/20261011T041825Z-d2293fc5/report.json`：真实本地镜像、正式生成的 stripped home 配置、离线 Git/验证命令与只读/可写挂载三项通过；不是实际 provider。
- `output/verification/20261011T042307Z-79440aff/report.json`：i18n、Chromium/WebKit 的异常核对确认、分页/当前问题/刷新恢复通过。
- `output/verification/20261011T041648Z-1c8bf046/report.json`：当时工作树 quick 26 passed，Linux chown 在 macOS 为 not_applicable；后续检查记录见本批最终报告。
- 跨 store/collaboration/workspace/chat/agent/agentprobe/dockerx/backup/safefs/server 的协作定向 `-race` 通过；最终修改后再复核涉及的调度/恢复包。
- 保留失败报告：`040955Z-7f5bba55`/`041041Z-7049c16c` 为 Docker internal 网络未提供宿主发布端口，改用独立禁 masquerade 网络及随机回环端口；`041108Z-e526ea03` 为浏览器刷新后未等任务列表加载；`041609Z-b5298fad` 为撤权 fixture 未用合法配置加载，已修正。以上不曾当作通过证据。

本批最终收尾证据：

- `output/verification/20261011T043010Z-e40d26ad/report.json`：13 个真实 HTTP/调度/SQLite 场景，以及 Chromium、WebKit 接真实 Linux 后端的页面闭环，3 项通过。每场景额外核对 spend 账本条数与 usage 相同、缓存余额与重算账本一致；浏览器明确检查 4 个执行 / 4 条 usage / 4 条 spend / 9960 余额，删除后不丢账。
- `output/verification/20261011T042508Z-24e54fbc/report.json`：完整 quick 26 passed、1 not_applicable（macOS 上的 Linux 普通用户 chown 闸门）。运行中继续补充的取消/恢复细节及最终 fixture 由后续定向和全包构建校验覆盖，不把早启动的测试冒充最终源码重新全量测试。
- `output/verification/20261011T043036Z-1a3c0bd5/report.json`：最终源码的构建、vet、全部非 server Go 测试及验证目录检查 4 项通过。
- 最终 store/server/workspace/agentprobe/dockerx/backup 协作相关 `-race` 通过；SSE 属主隔离、断线清理、时间预算/等待到期、删除重放、异常核对及验证前置均有回归覆盖。
- `output/verification/20261011T042811Z-1b4810f4/report.json`：最后前端的类型、完整产物、三语和 Chromium/WebKit 合成 API 回归，5 项通过。
- `output/verification/20261011T042938Z-446dda8b/report.json`：最终目录头校验和命令结果实现下的真实 Docker Git/验证回归通过。正式 stripped home 的 CLI/挂载证据仍为 `20261011T041825Z-d2293fc5`。
- 页面截图：`output/playwright/collaboration-e2e-chromium.png`、`output/playwright/collaboration-e2e-webkit.png`；窄屏无水平溢出，已人工查看 Chromium 截图。`git diff --check` 和生成契约一致性检查通过。

剩余发布验收范围：真实订阅/API Key/中转账号及模型输出稳定性、真实刷新令牌并发、实际 Linux 磁盘满/掉电与部署恢复。首期仍只接收干净 Git 提交，不自动应用、合并或推送结果；更宽输入、多执行者并行属于 C2/C3。未提交推送、未部署，不因开发测试通过而开启生产功能。

## 产品目标与范围

让用户自行选择主辅 Agent、账号、模型及推理强度，完成“规划 → 执行 → 双向答疑 → 评审 → 有限返工 → 交付”。Claude/Codex 可以互换主辅，同类型组合也支持；运行中配置调整在安全边界生效。

本专项与 M11 的“增加 Agent 类型”分开跟踪，不重排 M6～M11，也不依赖尚未接入的新 CLI。已有存储升级兼容缺口继续由 S1-09 处理，不以协作开发代替修复。

| 阶段 | 用户结果 | 状态 |
| --- | --- | --- |
| C1 双角色串行闭环 | 主辅/模型可选；双向答疑、评审返工、暂停换配置、回执恢复、费用与结果可查 | 后端/网页闭环与离线端到端已接入；真实 provider 与发布验收未完成 |
| C2 更灵活的委派与输入 | 协作 MCP、未提交/非 Git 项目快照、结果应用、独立评审角色 | 后续候选；按 C1 使用反馈选取 |
| C3 并行团队 | 多执行者、独立分支、依赖调度、冲突合并与更强预算控制 | 长期候选；无交付承诺 |

## C1 实施单元

| 编号 | 交付内容 | 依赖 | 验收重点 |
| --- | --- | --- | --- |
| C1-01 | 冻结角色/配置/交接/状态契约；CLI 能力探针 | 本设计 | 四种主辅组合；最终答复、严格恢复、中断；缺能力明确阻断 |
| C1-02 | store 任务、参与者、步骤、动作、事件与产物元数据；迁移 | C1-01 | 同 ID 重放、CAS、代际、唯一下一步、删除围栏、旧 schema 升级/高版本拒绝 |
| C1-03 | 受管 Session 和容器 profile；基线/工作副本/只读快照 | C1-02 | 独立凭证、受限挂载、关闭普通写入口、唯一写者、容量与资源计数、产物原子发布 |
| C1-04 | 抽取共同回合执行和收尾；接入协作步骤 | C1-01～03 | 普通聊天无行为回归；沿用用量去重/扣款；严格续聊失败；history/usage 失败不推进 |
| C1-05 | 协调器：规划、执行、答疑、评审、返工与预算 | C1-02～04 | 双向问题关联；计划版本；有界循环；同一任务单步骤；无需两个同时运行名额 |
| C1-06 | 暂停/取消/核对/继续及角色、账号、模型变更 | C1-05 | 旧进程停止证明；新代际新上下文；交换主辅；撤权/迟到事件/重启均不误派发 |
| C1-07 | 验证步骤、结果快照、差异和交付下载 | C1-03～05 | 区分模型声明与平台验证；测试/评审指向相同代码版本；源项目不被修改 |
| C1-08 | HTTP/SSE 协议、三语网页配置与任务时间线 | C1-01、02、05～07 | 对称角色选择、模型能力、待确认动作、刷新/多标签/退出、窄屏/键盘 |
| C1-09 | 故障矩阵、备份恢复、兼容文档和候选门槛 | C1-02～08 | 合成执行计数/账本/文件证据；Linux 真实挂载；候选 CLI 无真实凭证探针 |
| C1-10 | 经授权的真实 provider 验收与发布收口 | C1-09 | 两种跨类型方向及同类型组合；模型切换；实际费用；发布、恢复与最终能力边界 |

顺序建议：先完成 C1-01 探针和协议，接 C1-02～04 打通单个受管回合，再实现 C1-05～07 的完整闭环，最后接网页与联合验收。API/界面可按已冻结 schema 使用合成数据开发，但不能把合成界面演示算作后端交付。

每个单元单独提交，涉及 TS 时同步产物；先记录实现和验证证据，再更新完成状态。当前不承诺周数，CLI 探针及快照测量完成后再估算排期。

## 验收矩阵

| 场景 | 必须看到的证据 |
| --- | --- |
| Claude 主/Codex 辅；Codex 主/Claude 辅 | 相同任务流程均可规划、执行、反问、评审，角色行为不按品牌硬编码 |
| Claude/Claude；Codex/Codex | 同类型不同模型可选；同账号独立 home/线程；无凭证串写与用量重复 |
| 创建页交换角色 | 账号、模型、强度一起交换；能力校验随新类型刷新 |
| 运行中变更模型/账号/主辅 | 本步收尾后暂停，配置 revision 更新，旧执行者停止，新上下文收到完整有效交接 |
| 新模型无权限/能力变化 | 拒绝新步骤，不静默改用默认模型；仍能查询原结果 |
| 提问与返工 | 问题/回复关联正确，辅方从自己的上下文继续；返工指向具体快照和验收项 |
| 重复创建、动作、交接 | 同一逻辑请求只有一次实际执行；回执/事件重放不重复扣费 |
| 模型运行前后各崩溃点 | 已确认结果保留；可能执行的步骤 uncertain；不会自动补跑 |
| 换角色时旧 CLI 派生后台进程 | 未确认容器停止前没有第二个代码写者，快照不能在写入中发布 |
| 取消/暂停和迟到输出竞态 | 取消标记阻止新步骤，旧代际不能推进，暂停不会被模型一句 completed 解除 |
| 续聊失败 / app-server 丢连接 | 严格暂停或 uncertain，无新线程/exec 自动补跑 |
| 非法交接 JSON / 假冒角色 | 业务校验拒绝；正文保留且费用照记；不能伪造用户批准或跳过步骤 |
| 费用缺失/未定价/结算失败 | 标明不完整并阻止受费用预算约束的自动推进，不把未知计零 |
| 只读主方与文件边界 | 写项目失败；无普通 shared 绕过；链接/FIFO/超量/路径替换不能越界读取宿主 |
| 快照发布中磁盘满或崩溃 | 没有 ready 的半份产物；不删源文件；恢复可清理受管暂存 |
| 验证修改源码 / 旧评审迟到 | 快照版本变化后旧验证与评审失效，重新验证对应的最终版本 |
| 只有一个运行容器名额 | 两角色轮流工作，等待和超时可见，无互等死锁 |
| 普通聊天/桌面/终端回归 | 单 Agent 行为保持，受管参与者无法经旧入口被操作 |
| 页面刷新、退出、同名用户重建 | 只查询恢复；范围/代际隔离；旧动作不能影响新身份或新任务 |
| 删除与备份恢复 | 无孤儿执行；正文清理留 ID 围栏；系统备份缺文件时禁止继续；full 恢复核对摘要 |

## 验证入口规划

已登记的入口以 `python3 scripts/verify.py list` 为准。当前可用：`browser.collaboration`、`webkit.collaboration`、`docker.collaboration-runtime`、`docker.collaboration-import`、`docker.collaboration-mounts`、`docker.collaboration-cli`。下列综合名称和覆盖范围仍为目标，不能按已实现的窄场景冒称完整验收：

- `integration.collaboration`：真实二进制、SQLite、HTTP/WS 与合成 Docker/CLI。核对步骤执行计数、消息版本、回执、usage/ledger 和结果摘要；测试控制接口只属于独立 fixture。
- `browser.collaboration`：合成 API 的 Chromium/WebKit 三语、窄屏、模型选择、主辅交换、配置暂停与恢复；不代表真实 provider 或设备性能。
- `docker.collaboration`：独立 Linux 卷/网络，验证真实挂载、UID 1000、进程停止、快照、空间不足和单容器名额调度；不挂用户数据，不使用真实账号。
- 候选 CLI 协作探针：在现有 `agentprobe` 隔离模型下验证 Claude/Codex 的最终输出、恢复和中断；无账号、无付费模型、未知协议失败。
- 真实 provider 场景独立手动入口：需明确授权测试账号和费用范围；确认四种组合、一次反问、返工、配置变更、取消及用量。已有普通聊天的真实测试不能替代协作验收。

本地基础验证使用现有 `quick`；Linux 专属权限测试与 Docker 能力分别记录。缺资源标 blocked/not_applicable，不把默认跳过的 live 测试写成通过。故障日志、提示词和产物留在受限、gitignored 目录，公开记录只放脱敏摘要。

## C1 发布退出条件

- C1-01～09 完成，故障矩阵有对应自动化证据；C1-10 有明确的真实 provider 与费用记录。
- 用户确实能交换主辅、各选模型，并在安全边界重新配置；不是仅 UI 交换名称或提示词。
- 一次创建请求、一条交接或一次用户动作的重放不会导致重复模型执行和重复扣费。
- 基线/结果/测试/评审引用可核对，原项目保持可继续使用；交付不冒充已应用或已部署。
- 旧单 Agent 链路回归通过；schema/备份/恢复/候选镜像能力和升级回退说明齐全。
- 界面与文档明确预算是回合边界控制、上下文重建可能丢失原生隐含信息、只读代码不等于网络副作用隔离。

## 后续阶段进入条件

C2 以 C1 真实使用反馈为依据。协作 MCP 只提交到同一持久调度器，不能创建绕过授权/回执的第二执行路径；同时补齐 Codex 的配置与工具接入。未提交项目导入先解决源目录一致性、显式文件清单及快照容量，再扩大输入范围。

C3 需要独立代码副本、任务依赖、单独写租约、合并冲突策略、同用户并发额度和取消传播。先证明双角色串行可靠，再讨论多人并行，不能简单移除 chatRoom 的单回合限制。


## 授权生产预览（2026-10-11）

用户明确要求先发布到生产验证，已通过独立版本目录的 stage/activate 流程部署 `v0.1.12-dev.collaboration.20261011.1`。该包以当前未提交工作树构建并标注 `+dirty`；未覆盖生产旧源码目录的修改，未公开 Release 或推送 Git。准确源码清单及部署报告在本机 gitignored 的 `output/deployment/v0.1.12-dev.collaboration.20261011.1/`。

- 发布前完整 `go build ./...`、`go test ./...` 通过；七平台包/校验和和打包二进制的离线备份恢复检查通过。
- 生产原有镜像中的 Claude Code 2.1.296 / Codex 0.162.1 已通过离线协作交接/续聊探针，没有使用真实账号或付费模型。
- 激活前用旧版本创建系统备份，并以新版本验证；schema 从 12 升到 13。仅通过 Config.mutate 修改 `collaboration_enabled=true`，其他配置字段未变化，保留旧版本目录和镜像。
- 已确认 systemd active、新版本实际进程路径、数据库 quick_check、原空间/用户/用量/账本记录保留；内外网首页/协作脚本为 200，ping 为 204，未登录协作 API 为预期 401，启动日志无失败。
- 当前进入用户生产试用阶段；真实模型任务与费用验证由用户发起，本次上线不代表 C1-10 全部通过。


2026-10-11 空间对话整合：新增 schema14，将协作模式、独立主辅设置、上下文预览、计划确认和同输入框答复/补充接入原聊天。任务锚点持久绑定线程，重连查询不重复启动；普通聊天回执冻结最近协作结果材料。新增幂等补充要求与安全边界重新规划。结果应用采用完整工作区暂存、基线校验、目录切换、持久回执与显式恢复，保留原 Git 信息和忽略文件。附件材料复制、旧任务手动关联、单独的应用副本清理仍未实现。新版本尚未部署。

验证：`output/verification/collaboration-chat-20261011-browser/report.json` 中 Chromium/WebKit 合成 API 与真实 Linux 后端聊天页面三项通过。后者覆盖创建、补充后重新规划、确认、验证/评审、刷新、应用文件；核对 4 次模型调用、4 条用量与账本、余额 9960（初值 10000）。Docker/CLI 边界为合成输出，未调用真实付费 provider。Linux runtime 同时覆盖四种角色组合的文件应用、原 Git/忽略文件保留、重复请求和原项目备份。切换前、原目录移出后、结果移入后的恢复测试都保留原项目及核对副本。

早期全量回归报告 `collaboration-chat-20261011-quick` 在旧历史迁移夹具（未初始化 store）失败，已修正可选协作元数据查询在该夹具下的兼容性；后续报告独立保留，不能把首次失败记为通过。

最终证据：`collaboration-chat-20261011-acceptance` 的类型/完整 JS 产物/三语文案、Chromium/WebKit 新聊天流程和真实 Linux 后端聊天页面 6 项通过；`collaboration-chat-20261011-remaining` 中完整服务端与 11 项策略检查通过（runner 首次因新浏览器子脚本未登记失败，已补登记）；`collaboration-chat-20261011-confirmation` 中创建与动作两类失联恢复、双浏览器及 runner 3 项通过。schema13→14 原任务/回执指纹保留，以及应用放弃围栏、新操作重试、已完成禁止重复应用的定向 Go 测试通过。首次失败报告均保留，未修改生产二进制或数据库。
