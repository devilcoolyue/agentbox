# 协作最终结果与步骤输出修复

日期：2026-10-11。已发布生产 `v0.1.12-dev.collaboration.20261011.11`，静态资源标识 `0583edfe41e8`。

## 问题与行为

原“实时输出”只接收当前订阅期间的临时流。任务完成或暂停时，页面会断开流并清空文字，却保留一个空框；任务完成后打开详情也不会读取后端已有转录。对话卡片取最后一条消息，通常是主方的评审摘要，没有明确最终结果标题。

现在已完成任务的对话卡片与详情顶部单独显示“最终结果”，将执行摘要、评审确认、平台验证分别列明。结果选择必须对应当前任务版本、计划版本、已完成步骤、已发布快照及确认交付事件；不能把旧计划、未批准结果或模型自报测试当成最终交付。

“步骤输出”允许切换规划、执行、评审等步骤。运行时接收真实增量；结束、暂停或刷新后分页读取持久记录。共享参与者转录按真实 turn ID 分段，防止混入同一 Agent 的其他步骤；任务/步骤切换取消旧请求，迟到响应不会覆盖当前内容。空记录、加载、断线、重试与预览截断都有明确状态。

主要实现：`features/collaboration/{outcome,final-result,live,output}.ts`，聊天入口与旧独立协作面板共用。后端协议、任务调度与计费未增加调用路径。

## 验证

- 10 项结果关联检查覆盖执行、返工、格式修复、代际隔离、事件不同步和平台验证关联；步骤 ID 使用生产实际的 12 位格式。
- Chromium、WebKit 验证最终结果、持久输出、增量去重、历史分页、步骤缓存、跨任务迟到响应、旧面板及窄屏布局。[Chromium](../../output/verification/collaboration-output-chromium-20261011/report.json)、[WebKit](../../output/verification/collaboration-output-webkit-20261011/report.json)、[旧面板补充](../../output/verification/collaboration-output-panel-chromium-20261011/report.json)。
- 类型、构建、三语检查通过；完整 Go 构建、vet、普通单测与 server 全量通过。macOS 上 Linux 专属 chown 闸门明确 not_applicable。[Go 报告](../../output/verification/collaboration-output-production11-go/report.json)的 source_changed=true 来自执行期间补强浏览器测试夹具；运行时 TypeScript、Go 和嵌入产物保持冻结，最终发布源码清单逐文件复核无漂移。
- 七平台归档验证、原生 Linux 包/备份恢复和敏感扫描通过。[发布包检查](../../output/deployment/v0.1.12-dev.collaboration.20261011.11/release-checks.json)。
- 按既有不可变版本目录流程由 `.10` 升级至 `.11`，升级前系统备份验证成功，schema 仍为 15；发布后 systemd active、内外网 HTTP 200，启动正常。

## 已有生产任务复验

直接读取「Jev 真实协作验收 20261011」的真实 Claude/Codex 任务；没有创建新任务或重跑模型。

| 项目 | 结果 |
| --- | --- |
| 最终结果 | 显示“已修复 sum.cjs，使导出函数返回两个参数之和。” |
| 评审与验证 | Claude 评审摘要单列，平台 `node verify.cjs` 成功独立显示 |
| 规划输出 | 1042 字符，已保存记录 |
| 执行输出 | 405 字符，已保存记录 |
| 评审输出 | 578 字符，已保存记录；三份内容摘要不同 |
| 刷新与查询 | 最终结果保留，任务 revision 与费用未变 |
| 新增任务操作 | 0；仍为 1 次 Jev、3 个真实编程回合 |

[生产验证记录](../../output/deployment/v0.1.12-dev.collaboration.20261011.11/production-output-verification.json) · [最终结果截图](../../output/playwright/collaboration-final-production-20261011.png) · [步骤输出截图](../../output/playwright/collaboration-output-production-20261011.png)。

当前页面输出保留模型原文，不进行额外模型翻译。步骤输出是过程记录，最终结论以顶部“最终结果”及对应验证证据为准。
