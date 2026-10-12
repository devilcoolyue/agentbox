# Jev 与真实 Claude / Codex 生产验收

日期：2026-10-11。已按用户明确要求发布 `v0.1.12-dev.collaboration.20261011.10`，并在生产已有账号上完成真实端到端验收。此次没有使用模型或 Docker 测试替身。

## 发布结果

- 本地完整 quick：26 项通过、0 失败，1 项 Linux 专属闸门在 macOS 为 not_applicable；真实服务/原生环境项目的显式跳过单独保留。[报告](../../output/verification/decision-production-preflight-20261011145001/report.json)。
- 七平台安装包、归档校验、原生 Linux 包运行/备份恢复通过；敏感扫描无发现。[包验证](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/release-checks.json)、[扫描摘要](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/secret-scan-summary.json)。
- 通过不可变版本目录的既有 stage/activate 流程发布。旧版本为 `.9`，激活前用旧程序创建系统备份并由新程序验证，随后迁移 schema 14 → 15。
- 服务 active，内外网 HTTP 200，SQLite quick_check 为 ok，启动日志正常；升级前后的原有空间、用户、用量、账本、任务及步骤数量一致。[发布后检查](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/postflight.json)。
- 生产源码 checkout 的已有修改与未跟踪文件未覆盖。本次是私有生产预览，未提交/推送、未创建公开 Release。

## 真实任务与证据

空间名称：**Jev 真实协作验收 20261011**。任务及结果保留在生产供用户查看；验证结束后空间已停止。

新建独立 Git 项目，初始 `sum.cjs` 错将加法实现为减法。不可修改的 `verify.cjs` 覆盖正数、负数和零；执行前确认测试失败，任务只允许修复一个文件，不联网、不安装依赖。

| 环节 | 真实结果 |
| --- | --- |
| 页面创建 | 在原聊天输入框选择协作、配置固定 Claude 主方与自动 Codex 搭档、启用 Jev |
| Jev | Mindshub 真实返回 `jev-1.13.0`，1402 输入 / 149 输出 token，按当前推广价 $0 |
| 模型决策 | Jev **弃权**，系统使用预设回退 `gpt-6.1-sol`；主方保持 `claude-sonnet-5-5` |
| 规划 | Claude Code 使用真实账号读项目并生成计划；页面确认后才执行 |
| 执行 | Codex 使用真实账号将 `a - b` 修改为 `a + b` |
| 平台验证 | 独立实际容器执行 `node verify.cjs`，exit 0，输出 `AGENTBOX_COLLAB_SMOKE_OK` |
| 评审 | Claude Code 基于同一快照及平台验证证据批准 |
| 应用 | 页面将结果应用到源空间，application=completed，verify.cjs 内容保持原样 |
| 应用后验证 | 再启动测试空间，在其实际容器运行相同验证，仍通过，然后停止空间 |
| 刷新/重放 | 重放原创建请求返回已有任务，刷新后恢复结果；决策仍 1 次，编程回合仍 3 次 |

真实编程费用按当前平台价格快照记账：规划 $0.057678、执行 $0.015407、评审 $0.021656，合计 **$0.094741**。四条用量记录（含免费决策）对应三条扣款记录，逐条归因后费用与账本相符。这是平台对本次任务的记账结果，不是便宜模型与高级模型的成本对照实验。

[验收汇总](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/live-evidence-resume-1791731145/summary.json) · [页面/API记录](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/live-evidence-resume-1791731145/browser-report.json) · [真实用量/账本/步骤](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/live-evidence-resume-1791731145/ledger-evidence.json) · [应用后测试与差异](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/live-evidence-resume-1791731145/applied-verification.json) · [生产页面截图](../../output/playwright/decision-production-20261011.png)。

## 结论与限制

**生产链路可用，真实任务完成；自动选择更便宜模型的收益尚未证明。** 本次候选有 13 个合法模型，但描述主要是显示名称和价格，没有明确的能力档位。Jev 返回弃权，符合当前“缺少足够依据则回退”的策略，不能将其报告成“Jev 自动选中了经济模型”。后续需要可靠的能力描述及真实任务对照评测，才能验收自动降档的效果。

生产已保存 Mindshub 对应密钥，插件开启且仅管理员可用，超时设为 15 秒。全局默认保持 builtin，由任务勾选 Jev；固定角色不改变。密钥经设置 API 写入服务端配置，未写入源码、浏览器报告或发布包。

本次自动化的中间失败记录均保留：

- 首轮在历史/草稿初始化完成前切换模式，被恢复过程覆盖，尚未提交任务；后续等待历史加载完成。界面初始化期间可覆盖模式选择的竞态仍属待改进项。
- 第二轮选择器误把增强下拉按钮作为原生 select 操作，未提交任务；只修正测试脚本定位。
- 正式任务观察遇到短暂 `paused / verification_required`，脚本误将其视为需要人工处理。该状态及 `delivery_required` 是调度器自动交接阶段，任务自行完成，未发送 resume 或重新提交任务；继续的是**同一任务**的查询和页面应用。[原观察记录](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/live-evidence-resume-1791730903/browser-report.json)。
- 应用后截图一度出现普通聊天的查询提示；新浏览器刷新复验中消息查询 HTTP 200、身份范围匹配、提示消失、无页面异常。[刷新观察](../../output/deployment/v0.1.12-dev.collaboration.20261011.10/live-evidence-resume-1791731145/refresh-observation.json)。

DP-12 已增加真实 Jev + CC/Codex 的任务成功与计费证据；代表性工作负载准确率、低价模型实际采用率、质量损失及升级收益仍未验收，不能由这一个小任务推断。
