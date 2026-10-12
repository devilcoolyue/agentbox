# 可选决策插件实施与验收

[实现与接口](../architecture/decision-plugins.md) · [路由研究](../architecture/collaboration-model-routing.md) · [统一验证](../verification.md)

日期：2026-10-11。状态：已发布生产 `v0.1.12-dev.collaboration.20261011.10`，[真实 Jev + CC/Codex 端到端验收](decision-production-20261011.md)通过。功能范围为新协作任务首次执行前的自动选模，主辅分别固定或自动，Jev 可关闭并回退。DP-01～DP-11 的功能和恢复行为已验证；DP-12 已完成 [Mindshub 真实路由及页面链路验证](decision-mindshub-live.md)，真实编程模型的质量与费用对照仍待执行。

## 实现落点

| 路径 | 内容 |
| --- | --- |
| `internal/decision` | Provider、唯一候选规则、Jev Choice、固定价目与预留上界、并发限制和熔断 |
| `internal/config/decision.go`、`internal/server/settings.go` | 配置持久化、revision CAS、密钥 keep/set/clear、授权与脱敏视图 |
| `internal/server/decision*.go` | 冻结候选、一次调用、代次取消、回退与费用复核 API |
| `internal/store/collaboration_decisions.go`、迁移 015 | 同事务接收、不可变决定、调用唯一性、费用证据/预留/流水及恢复围栏 |
| `web/src/features/settings/decision.ts` | 管理员可选插件配置 |
| `web/src/features/collaboration/decision.ts` 及 composer/pending | 主辅自动/固定、空间默认、原意图核对、选模原因与待核对费用 |

内置规则只确定唯一合法候选，不从名称推测模型等级。Jev 当前仅接 Choice，`confidence >= 0.65` 为待评测阈值；管理员已配置单价可作为候选参考，缺少能力/价格证据不得臆测。TypeSafe 的已核查版本 `jev-1.13.0` 按完整上下文预留 2,753 微美元；Mindshub 当前推广期零价单独归因，未知用量仍待核对。原协作执行器未改成按需委派或并行团队。

## 验证入口与报告

已完成组件验证：`go test -race ./internal/decision ./internal/config`，包括正式协议形状、匿名候选、异常用量保留、不重试/不重定向、超时、无效分布、重复 JSON 键、费用计算、配置持久化/权限/凭证、并发 CAS 与配置失效通知。之后的整组检查仍以本轮统一报告为准。

| 验证层 | 入口/范围 | 本轮结果 |
| --- | --- | --- |
| quick 各组 | `quick` 后按 `web`、`go`、`policy` 及失败项续验 | 网页 10 项、Go 构建/vet/非 server 与完整 server、策略 12 项通过；macOS 的 Linux chown 拒绝项为 not_applicable |
| Go 存储/服务端 | `TestDecision*`、新增迁移和旧聊天/协作回归 | [完整 server 通过](../../output/verification/20261011T140212Z-f3fe566e/report.json)；最后的纯计费修订修复另经 store/server 专项 race 通过 |
| Chromium | `browser.decision-plugins` → `scripts/test-decision-plugins.mjs` | 通过：Chromium/WebKit 合成 API；组件执行命令见下方 |
| WebKit | `webkit.decision-plugins`，同一浏览器场景 | 通过：Chromium/WebKit 合成 API；组件执行命令见下方 |
| Linux 真实后端与完整聊天 DOM | `scripts/test-collaboration-runtime.py --image <本地镜像> --chat` | 通过：[Linux HTTP/SQLite/页面报告](../../output/verification/20261011T135701Z-610a070b/report.json)；Docker/CLI 输出边界为合成替身 |
| 真实 Jev/provider | 中文路由、真实 Jev 与页面贯通 | [接入与正常/超时链通过](decision-mindshub-live.md)；8 个路由样例 7 个满足预期、1 个低置信回退；另已完成[真实编程任务](decision-production-20261011.md)，费用/质量对照未执行 |

本轮已有报告：

- [最终网页 10 项检查](../../output/verification/20261011T140822Z-29ea1d32/report.json)：类型、产物、i18n、契约、草稿/outbox/历史/生命周期均通过。[首次 quick 报告](../../output/verification/20261011T135510Z-738e00a0/report.json)因旧研究缓存内 Go 源码被递归扫描而在构建处失败；保留失败报告，为 gitignored 研究目录增加独立模块边界后重跑 Go 组。
- [Go 构建/vet/非 server](../../output/verification/20261011T135601Z-e78f6efd/report.json)通过；该轮 server 的四项原额度/终端回归失败，原因是缺少当前用户记录的预留查询错误改变旧额度行为。修复为无当前身份则无预留后，[完整 server 复验](../../output/verification/20261011T140212Z-f3fe566e/report.json)通过。Linux chown 拒绝项在本机 macOS 明确 not_applicable。
- [本地策略 12 项](../../output/verification/20261011T135648Z-59ceefff/report.json)：全部通过。
- [Linux 协作调度器](../../output/verification/20261011T135850Z-c1820b4e/report.json)：包含成功自动选模、超时未知预留、原角色组合、真实文件/SQLite/账本与恢复场景；上游 Docker/CLI 输出由隔离 fixture 提供，不调用模型。
- [Linux 后端＋真实页面](../../output/verification/20261011T135701Z-610a070b/report.json)：同一对话创建自动意图、无插件选择、补充重规划、执行、刷新及应用文件，分类调用 0、CLI 调用 4、用量/账本各 4、余额一致。
- [原聊天 outbox 浏览器回归](../../output/verification/20261011T135849Z-ffb7fae0/report.json)：通过。

另外已执行 `go test -race ./internal/decision ./internal/config`、`go test -race ./internal/store ./internal/backup` 与服务端 `TestDecision*` race；新 `scripts/test-decision-plugins.mjs` 和原 `scripts/test-collaboration-chat.mjs` 各在 Chromium、WebKit 通过。浏览器使用已安装本地 Playwright 模块；截图在 `output/playwright/decision-plugins-{chromium,webkit}.png`。没有安装/连接真实模型进行这些检查。

失败报告均保留；上述结论是各组及失败项续验的合并结果，不把先前失败整轮改写为成功。随后发现的迟到费用修订竞争已修复：纯费用不改任务/路由修订；`go test -race ./internal/store -run TestDecision` 和服务端决策/额度/终端专项均通过，验证准备固定镜像与取消 CAS 不再受迟到费用影响。

## DP-01～DP-12 证据矩阵

下列“通过”仅指上述本地/合成集成功能验收，核对了调用次数、冻结绑定、流水/余额和未知占用；不代表真实服务选模质量。

| 编号 | 场景与关键断言 | 已有检查位置 | 本轮验收证据 |
| --- | --- | --- | --- |
| DP-01 | 全固定角色保持原绑定，Jev 开关均零调用 | server `TestDecisionFixedAndOptionalPathsDoNotCallProvider`；浏览器固定请求 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-02 | 仅自动角色决策；反向主辅同样保留固定约束 | server `TestDecisionOnlyUnresolvedRolesAndOneClaim`；候选测试 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-03 | 唯一候选先解；仅未决角色合并一次 HTTP；整组冻结 | builtin/provider/server 组合测试 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-04 | 关闭、无权、无密钥、builtin 选择不外发 | config 授权和 server 可选路径；设置页 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-05 | 超时、429、非法分布或弃权回退；没有隐式重试 | decision `TestJev*`；server 超时测试 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-06 | 重复接收、竞争、刷新和响应丢失不重复分类/执行/扣费 | store claim/ledger 并发测试；浏览器查询恢复 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-07 | 暂停、禁用、取消、超时、迟到返回及结算失败相互竞争 | server/store 决策状态、晚到证据、复核测试 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-08 | 账号/模型撤权只缩小候选；配置和默认值不漂移 | `TestDecisionCandidatesKeepConstraintsAndDoNotExpand`、撤权测试 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-09 | 密钥不出服务端；预留、已知/未知费用与账本一致 | config secret/CAS；store 预算、证据合并和结算回滚 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-10 | 决策不占 Agent 容器；重启不重发；恢复遵守围栏 | restart/restore 测试；Linux 真实后端场景 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-11 | 旧固定协议、原聊天草稿/回执/附件/费用不变 | quick 各组、旧聊天 outbox/协作浏览器、Linux 协作页面联合测试 | 通过；见上方 Go、race 与相应浏览器/Linux 证据 |
| DP-12 | 中文任务效果、路由耗时、总费用及升级收益 | 独立真实服务效果验收 | **部分完成**：[真实 Jev 中文样例与页面链路](decision-mindshub-live.md)已执行，[真实生产任务](decision-production-20261011.md)也已完成；Jev 在实际候选上弃权，真实编程质量/费用对照未执行，不能宣称节省费用 |

## 运维与恢复边界

- 保存插件设置不测试连接、不收费；具体 API、范围与默认值见[实现说明](../architecture/decision-plugins.md#配置与-api)。
- 费用复核使用 `POST /api/collaboration/runs/{run}/decision/reconcile`，提交 scope 与当前任务 revision，只处理已存证据，不重分类、不重跑 CLI。无有效证据不能释放未知预留。
- 普通重启后未完成路由须显式恢复为被冻结的回退；完整备份恢复后尚未冻结路由的自动任务须取消并重建。取消旧任务不清除未知费用占用。
- 第三方 HTTP 协议替身、fixture CLI 与合成浏览器仅证明实现和恢复行为，不证明 Jev 能正确选择模型。真实服务验收应另行记录授权、输入范围、模型/策略版本和可比较数据。
