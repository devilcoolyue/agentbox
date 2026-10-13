# 主管委派：强模型主导、经济模型跑腿

日期：2026-10-12。状态：已实现（schema 18），生产开发预览待用户验收；替换并删除了原“多 Agent 协作”（规划 → 实施 → 评审、独立副本与显式应用）。

场景只有一种：**当前空间的 Claude Code 主 Agent（通常选 Opus）直接面对用户，做架构、关键代码和审核；规则明确的具体活由它调用平台工具交给经济模型（Sonnet、Codex 小模型，或挂在中转账号下的 Gemini Flash 类模型）去跑**。交给谁、要不要交，由管理员维护的能力标签硬过滤，可选 Jev 在候选和“主 Agent 自己做”之间判断。

## 1. 整体流程

```text
用户（空间对话，输入框旁打开「委派」，勾选跑腿名单、可选 Jev）
   │  发送时委派选项冻结进聊天回执（ChatRequestInput.delegation，参与请求指纹）
   ▼
主 Agent：claude -p（原生线程续聊）
   │  追加 --mcp-config（agentbox 委派工具）、--append-system-prompt（委派守则）、
   │  --allowedTools 两个委派工具、--disallowedTools Agent,Task（关闭原生子 agent，委派统一走平台路由）；
   │  用户这条消息前另加一段 <agentbox-delegation> 提醒（只发给 CLI，历史里仍是原文）
   ├─ 自己读代码、定接口、写关键代码
   ├─ 调用 mcp__agentbox__delegate(task, task_type, risks, access, continue?)
   │       │  helper（容器内 python3 -I）经网桥临时端口 + Bearer 票据回连服务端
   │       ▼
   │   路由：额度 → 续派同一跑腿 → 标签硬过滤 →（Jev：候选 + self）→ 规则挑选
   │       ├─ 选中跑腿 → 在同一个 /workspace 上执行 → 报告 + 平台实测改动 + 费用
   │       └─ 没有合适跑腿 / Jev 判“自己做” → declined，主 Agent 自己完成
   ├─ 看实际改动；调用 mcp__agentbox__delegation_feedback 标注 ok/minor_fix/redo/abandoned
   └─ 向用户汇报
```

主 Agent 在工具调用返回前处于等待状态，同一时刻只有一个写者；跑腿直接修改当前空间文件，没有副本、快照、计划确认、交接 JSON 或“应用结果”步骤。

## 2. 用户界面

- **输入框**：工具栏里与附件、语音并排的「委派」图标按钮（`#chat-delegate`），开启时变色并在角标显示跑腿个数；点开弹层（`#chat-delegate-pop`），顶部是总开关，下面是跑腿名单。窄屏弹层贴住整个输入框展开。勾选某个跑腿会顺带打开委派，取消全部跑腿则关闭。名单来自 `GET /api/sessions/{id}/delegation/options`：本人有权使用的账号下、管理员标了跑腿标签的模型，显示 Agent、账号、档位、可接任务、不接的风险和备注。Jev 已启用且本人有权使用时多一项“用 Jev 辅助判断”，默认勾选与否跟随「智能决策 → 默认决策方式」。开关与名单按空间记在本浏览器（`agentbox_delegation_<空间>`），只是偏好。只有 Claude Code 空间显示开关；Codex 空间可以当跑腿，不能当主 Agent。
- **对话里**：每次 `delegate` 调用一张可折叠卡片（`features/delegation/card.ts`），显示跑腿、任务首行、状态、耗时与费用；展开后是任务类型/风险/访问方式、路由原因、完整任务说明、平台实测改动、跑腿报告、主 Agent 评价。运行中跑腿的完整事件实时滚动显示最近几条；结束后可“查看跑腿过程”按需读取完整记录（`GET …/delegations/{id}/log`，按行分页）。卡片按委派 ID 合并开始/结束两条历史记录，刷新后能还原。
- **管理员**：「系统设置 → 模型管理 → 账号模型」每个模型多一列“跑腿”按钮，账号「可用模型」弹窗的模型表也有同一列，都打开标签编辑器（`delegation-tags.ts`）；按钮只写档位，能接/不接的任务在悬停提示里。编辑器下方显示该模型按任务类型统计的历史委派结果（`GET /api/delegation/stats`，管理员）。系统不会根据模型名字推断能力，也不会自动改标签。
- **使用记录**：跑腿用量以“委派跑腿”类型显示在原空间下，详情与 CSV 带委派 ID；Jev 判断仍是“决策调用”，同样带委派 ID。

## 3. 能力标签与路由

### 3.1 受控词表（`config/delegation.go`）

任务类型（主 Agent 单选）：`search` 查找定位、`summarize` 阅读总结、`mechanical_edit` 按规则批量修改、`implement_spec` 按接口实现、`write_tests` 补测试、`fix_tests` 跑测试修失败、`ui_copy` 文案与样式、`docs` 文档、`refactor_local` 模块内重构、`other`。

风险（主 Agent 多选）：`auth`、`billing`、`migration`、`concurrency`、`security`、`data_loss`、`public_api`、`cross_module`、`ambiguous_spec`。

### 3.2 模型侧标签

账号模型条目上的可选字段 `delegation`（经 `Config.mutate`/persist/settings 保存；全局 `models` 拒绝该字段）：

```json
{"id": "claude-sonnet-5-5", "label": "Sonnet 5.5",
 "delegation": {"enabled": true, "tier": "economy",
   "can": ["mechanical_edit", "implement_spec", "write_tests", "fix_tests", "docs"],
   "avoid": ["auth", "billing", "migration", "concurrency", "ambiguous_spec"],
   "notes": "按明确接口实现、批量改调用、补单测稳定；跨模块设计不稳"}}
```

`tier` 只影响规则挑选的偏好；`can` 之外的任务类型、`avoid` 命中的风险一律不派；`notes`（≤300 字）原样作为 Jev 候选描述的一部分。中转站模型只要挂在 Claude Code 或 Codex 账号下且能跑通工具调用，就可以打标签当跑腿；是否可用以实测为准。开了跑腿但没配价格的模型按 0 记账，选项里会标出未定价。

### 3.3 路由顺序（`server/delegation.go` 的 `route`）

1. 用户额度见底：`declined`（`rules:quota_exhausted`）。
2. `continue` 指向本空间上一次委派，且那个跑腿仍在候选里：续用它的原生会话（`continue:same_worker`），不调 Jev。
3. 候选 = 本回合冻结的名单 ∩ 当前仍获授权的账号 ∩ `delegation.enabled`；按 `can`/`avoid` 硬过滤，被排除的原因记进路由（`task_type`、`risk:<名>`、`not_tagged`）。没有候选：`declined`（`rules:no_eligible_worker`）。
4. 本回合勾选了 Jev：问题模板 `delegation-worker-choice-v1`，候选为全部合格跑腿加固定的 `self`（主 Agent 自己做）。选 `self` → `declined`（`jev:main_agent`）；选中跑腿 → 执行（`jev:worker`，带置信度）；弃权/超时/出错 → 按规则挑选（`fallback:<原因>`）。
5. 规则挑选（`delegation.Pick`）：经济档优先，同档取单价低（`rules:only_eligible_worker` / `rules:cheapest_eligible_worker`）。

Jev 只收到任务说明（有上限）、任务类型、风险、访问方式和由标签拼出的候选描述，不发送账号、凭证或模型绑定。

## 4. 执行

| 跑腿 | 运行位置 |
| --- | --- |
| 与主 Agent **同账号、同 CLI**（Opus 主 + Sonnet 跑腿，同一个 Claude 账号） | 主容器内另起一次 headless CLI，独立 PID 文件 `/tmp/.agentbox-delegate-<id>.pid`，同一个 home |
| **其他账号或 Codex**（含中转模型） | 隐藏的跑腿会话（`delegation_workers` 表，每个属主空间 × 账号一个）：独立 home、凭证与容器，`/workspace` 挂载属主空间的工作区；占用容器名额，名额不足时本次委派失败并返回，主 Agent 自己做 |

- 跑腿不带委派 MCP，不能递归委派；跑腿提示要求只做这项任务、拿不准就停下并在报告里说明，结尾按固定小标题输出报告。报告是普通文本，提取后截到 6 KB。
- 改动列表由平台计算：委派前后各取一次工作区清单（大小/mtime/权限，跳过 `.git`、`node_modules` 等，上限 5 万条或 10 秒），超限时标为“未知”，不伪造空列表。跑腿自报的“测试通过”只在报告里，不当作验证。
- 限额：每回合最多 8 次委派，任务说明 ≤32 KB，单次跑腿 20 分钟；开启委派的主回合时限放宽到 2 小时，MCP 工具超时 3 小时（同时设置 `MCP_TOOL_TIMEOUT` 兜底）。
- 取消：用户中断主回合时先中断进行中的跑腿；helper 收到 MCP `notifications/cancelled` 也会通知服务端中断。
- 服务重启：启动时 `RecoverDelegations` 把未结束的委派标为 `uncertain`，不自动重跑；主回合按原聊天回执语义变为待核对。

## 5. 记录与计费

- `delegations` 表（schema 18）：主回合 turn_id/序号、冻结任务说明、类型/风险/访问方式、路由 JSON、跑腿绑定与原生会话、状态、报告、改动、错误码、主 Agent 评价、时间。线程 JSONL 写入 `kind:"delegation"` 的开始/结束记录供历史还原；跑腿完整过程另存 `sessions/<id>/delegations/<委派ID>.jsonl`。
- 跑腿用量走原 `chat.Service` 结算：同事务扣额度，用量行记在属主空间与主线程下并带 `usage_events.delegation_id`，使用记录只读投影为“委派跑腿”。同容器跑腿是 `sdk-cli` 入口，终端补记扫描本来就跳过，不会重复记账。
- Jev 用量记为 `kind=decision`，带 `delegation_id`；未知费用按预留上限计。
- 统计：按 账号 × 模型 × 任务类型 汇总次数、评价分布、失败数与平均花费，作为管理员调整标签的依据。

## 6. 删除的旧协作

schema 18 迁移删除协作参与者会话及全部协作表（`collaborations*`、`collaboration_*`、`decision_attempts` 与相关触发器），旧任务和产物不保留；`config.collaboration_enabled` 与协作接口、页面、契约一并移除。Jev 的接入配置（网关、密钥、超时、访问名单）沿用「智能决策」设置。升级前保留 schema 17 兼容备份，回退须恢复该备份到新目录并使用旧二进制。

## 7. 验证与边界

- Go：`go test ./internal/delegation ./internal/decision ./internal/store ./internal/workspace`，以及 `internal/server` 的 `TestDelegation*`（假 Docker 引擎：同容器跑腿、风险拒绝、Jev 判自己做、其他 CLI 在隐藏工作区运行）。
- 浏览器：`browser.chat-delegation`（输入框开关与名单、回执冻结、实时/历史卡片、跑腿过程、Codex 空间隐藏）、`browser.browser` 的使用记录段、`browser.web-i18n`。均为合成 API，不调用模型。
- 已在本机用镜像同版本 Claude Code 实测：`--mcp-config` 的 stdio 服务器能连接并暴露工具、`--disallowedTools Agent,Task` 移除原生子 agent、未知 MCP 配置键被容忍。真实主 Agent 是否愿意委派、跑腿质量和费用收益需要在生产用真实账号验证，本文不承诺省钱比例。
- 生产实测发现：只靠系统提示词时，续聊会话里 Opus 沿用前几轮“自己读代码”的做法，一次也不委派；Claude Code 还把 MCP 工具作为延迟加载，需要先 `ToolSearch`。因此委派回合在用户消息前加一段提醒（说明本条已开启委派，并给出加载工具的 `ToolSearch` 查询）。本机用真实 Opus 加桩后端对同一续聊会话复现：只有系统提示词时第一步是 Bash，加提醒后两次都先委派。这是少量样本，不代表所有任务都会委派。
- 待观察：同一 home 下两个 Claude Code 进程共享 OAuth 凭证的续期；中转模型在 CLI 下的工具调用兼容性；Jev 对中文任务说明的判断质量（置信度不是正确率）。
