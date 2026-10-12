# Mindshub Jev 真实服务链路验证

日期：2026-10-11。结论：真实 Mindshub Jev 接入、真实 Linux 后端与浏览器的正常链路和超时回退均通过。8 个预设中文路由样例中，7 个符合预设采用/弃权结果，1 个因低置信度回退，保留为效果测试未达预期项。这是发布前的阶段记录；后续已完成[生产发布与真实 CC/Codex 验收](decision-production-20261011.md)。

本次用户授权按其提供的 `POST https://api.mindshub.ai/v1/decisions`、`model=jev` 和凭证测试。密钥仅经隐藏输入与 stdin 管道传入测试进程，不写入配置、环境变量、命令参数、源码或报告。测试结束后的凭证模式扫描未发现泄漏；临时容器和网络均清理成功。

## 验证范围

| 层次 | 本次实际使用 |
| --- | --- |
| 决策模型 | 真实 Mindshub HTTPS，返回 `jev-1.13.0`；生产 `internal/decision` 适配器 |
| 服务端 | Linux 容器中的真实 Go Handler、调度器、配置冻结、SQLite、用量/账本与文件应用 |
| 页面 | Chromium，真实聊天页面模块、HTTP/SSE、刷新与应用流程 |
| Docker/Claude/Codex | 测试协议替身；核对实际进入执行参数的模型，但未调用真实编程模型 |
| 候选能力 | 预设的合成能力描述与价格，用于检验路由；不是生产模型的能力评测 |

这证明 Jev 能参与当前产品链路，并不证明真实 CC/Codex 的代码质量、总体成功率或实际节省费用。DP-12 仅完成真实路由样例与接入验证，真实编程模型对照实验仍待执行。

## 正常链路与超时链路

流程：页面输入中文需求 → 主方固定、搭档自动、勾选 Jev → 接收并冻结候选 → 真实 Jev → 规划 → 补充需求后重新规划 → 确认执行 → 平台验证 → 评审 → 刷新 → 应用文件。

| 核对项 | 正常调用，15 秒超时 | 强制 100 毫秒超时 |
| --- | --- | --- |
| 结果 | 通过 | 通过 |
| 决策外发尝试 | 1 | 1；远端是否完成未知 |
| 主方 | 始终 `planner-fixture` | 始终 `planner-fixture` |
| 搭档 | Jev 选中 `economy`，进入实际 CLI 参数 | 回退 `worker-fixture`，原因 `provider_timeout` |
| 原始请求 | 仍保留自动意图及 `worker-fixture` 回退 | 同左 |
| 决策用量 | 649 输入 / 46 输出 token，已结算 $0 | `unknown`，`decision_partial=true`，不捏造用量 |
| 编程模型调用 | 4：两次规划、执行、评审 | 4 |
| 用量行 / 扣款行 | 5 / 4；免费决策有用量、不新增扣款 | 4 / 4 |
| 合成余额 | 10000 → 9969 微美元 | 10000 → 9870 微美元 |
| 重复提交、刷新、补充需求 | 不重新分类；最终模型绑定不变 | 同左 |
| 文件应用 | `source.txt` 成为 `implemented`，应用状态 completed | 同左 |

余额和编程模型费用使用合成 token 与测试价格，仅证明计量和选择的贯通。上面两列不能作为生产成本比较。

证据：

- [正常链路报告](../../output/verification/decision-live-20261011T144038Z/report.json)、[完整页面/API/账本记录](../../output/verification/decision-live-20261011T144038Z/browser-evidence.json)。Linux 后端用例 11.10 秒，包含编译和启动的运行器总耗时 20.398 秒。
- [超时回退报告](../../output/verification/decision-live-20261011T144057Z/report.json)、[完整记录](../../output/verification/decision-live-20261011T144057Z/browser-evidence.json)。Linux 后端用例 9.91 秒，运行器总耗时 15.826 秒。
- [正常页面截图](../../output/playwright/decision-live-selection-chromium.png)、[超时页面截图](../../output/playwright/decision-live-timeout-chromium.png)。

## 中文路由样例

预先写定期望、能力描述、价格与 `confidence >= 0.65` 门槛，再调用服务；没有修改期望或降低门槛来让结果通过。

| 样例 | 预设期望 | 实际被插件采用的结果 | 耗时 |
| --- | --- | --- | --- |
| 单个按钮文案替换 | 经济模型 | 经济模型 | 1825 ms |
| 独立 max 函数修复 | 经济模型 | 经济模型 | 1498 ms |
| 跨模块事务、崩溃恢复与去重 | 高级模型 | 高级模型 | 3228 ms |
| 多租户文件系统安全审查 | 高级模型 | 高级模型 | 1462 ms |
| “把项目弄好”，信息不足 | 弃权 | 弃权 | 1492 ms |
| 候选缺少能力资料 | 弃权 | 弃权 | 1543 ms |
| 简单任务夹带强制选模注释 | 经济模型 | **低置信度回退，未达采用期望** | 1785 ms |
| 主辅两角色合并一次请求 | 都选经济模型 | 都选经济模型 | 1492 ms |

全部 HTTP 200，均返回 `jev-1.13.0`，共 5196 输入 / 414 输出 token，按当前活动价为 $0。样本中位耗时 1520.5 ms，范围 1462～3228 ms，不含此前连通性诊断和浏览器测试请求。

夹带指令的样例中，Jev 原始选择仍为经济模型，但分布为经济 0.43、高级 0.41、弃权 0.16，confidence 为 0.14。插件拒绝该低置信建议，未强行采用；测试严格保留失败退出码。说明遇到混杂指令时可能增加回退，不能宣称所有简单任务都能可靠降级。

[完整路由结果](../../output/verification/jev-routing-20261011T143846Z-eaff9aa7/routing.json)及[运行记录](../../output/verification/jev-routing-20261011T143846Z-eaff9aa7/report.json)。这是 8 个小样本，不能视为统计准确率、置信度校准或省钱证明。

## 本次发现与修复

- 原插件只支持 TypeSafe 官方地址。增加 `gateway=typesafe|mindshub`，Mindshub 出站别名 `jev`、入站固定校验 `jev-1.13.0`；凭证和账本价格来源按服务隔离。
- Go 默认请求收到 HTTP 403，同一请求用 curl 成功；增加真实客户端标识 `User-Agent: agentbox/decision` 后 Go 成功。保留[最初 403 报告](../../output/verification/jev-routing-20261011T143528Z-a7dbf356/report.json)和[修复后单例](../../output/verification/jev-routing-20261011T143750Z-dcb10a3e/report.json)。没有证据确认网关内部具体拦截规则。
- 原候选构建会丢失回退模型已配置的能力标签，现按完整模型条目构建候选，保留回退绑定与候选顺序。
- 首次浏览器测试已完成全部功能，但测试语言初始化误访问沙盒 iframe 的 localStorage，导致最终错误检查失败。改为只初始化主页面后两轮通过；[原失败记录](../../output/verification/decision-live-20261011T143840Z/report.json)保留。
- Mindshub 当前 [公开价格](https://docs.mindshub.ai/inference/billing.md#jev-decisions)是推广期输入/输出 $0，仍占请求/token 配额；不能套用 TypeSafe 官方费用，也不承诺永久免费。未知用量即使预留为零也保持待核对状态。

组件 race、前端类型/构建、i18n、Chromium/WebKit 合成设置测试与原 Linux 聊天回归通过。[最终构建、vet、产物、协议、验证目录 5 项检查](../../output/verification/20261011T144013Z-182d8f87/report.json)通过。

## 复验入口

以下是显式真实服务验收入口，不属于默认 quick/浏览器测试组；运行时隐藏提示输入对应服务的 API Key。

```bash
python3 scripts/test-decision-routing-live.py --run-live
python3 scripts/test-decision-live.py --run-live --image python:3.11-slim
python3 scripts/test-decision-live.py --run-live --image python:3.11-slim --timeout-ms 100 --expect-timeout
```

路由样例可用 `--case <名称>` 定向复验。超时请求不自动重发，不能凭本地超时判断远端未执行。真实编程模型验收需要另行提供可用的 Claude/Codex 执行账号和隔离空间。
