# 协作成本对照分析

`scripts/collaboration-cost-report.py` 只读已有 SQLite 账本。它不会启动模型、访问生产、修改余额或按最新价目表重新计价。报告使用账本整数微美元，费用范围固定为所选编码回合及其协作决策（`cost_scope=selected_coding_turns_and_decisions`），不含自动标题、终端、基础设施费用或订阅实付金额；`excluded_kinds` 显式列出 `title`、`terminal`。本报告不是完整供应商账单，订阅账号的固定月费、剩余额度和供应商实付金额不能由此推出。

先准备独立 SQLite 备份或已停止写入的实验数据库，不要复制运行中数据库的单个 `state.db` 文件。脚本拒绝未检查点的非空 WAL，使用 `mode=ro`、`immutable=1`、`query_only` 和一个读取事务，不创建数据库旁的日志文件。

```sh
python3 scripts/collaboration-cost-report.py \
  --db /path/to/standalone-state.db \
  --manifest /path/to/experiment.json \
  --output output/collaboration-cost-report.json
```

输出必须是新文件，已有报告不会覆盖。清单不填写任务正文、凭证或原始模型输出：

```json
{
  "version": 1,
  "attempts": [
    {
      "id": "task-a-strong-1",
      "task_id": "task-a",
      "variant": "strong_single",
      "baseline": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "acceptance_id": "task-a-tests-v1",
      "accepted": true,
      "user": "experiment-user",
      "session_id": "single-agent-space",
      "turn_ids": ["111111111111"],
      "elapsed_ms": 60000,
      "model_repairs": 0,
      "human_repairs": 0,
      "human_ms": 0
    },
    {
      "id": "task-a-reviewed-1",
      "task_id": "task-a",
      "variant": "reviewed",
      "baseline": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "acceptance_id": "task-a-tests-v1",
      "accepted": null,
      "run_id": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "human_repairs": 0,
      "human_ms": 0
    }
  ]
}
```

- `variant` 是分组标签，可用 `strong_single`、`economy_single`、`reviewed`、`direct`。同一 `task_id` 必须使用相同 Git 基线和相同独立验收标准标识。协作基线另外与持久请求核对；普通聊天基线由实验组织者声明，账本本身无法证明代码环境相同。
- `accepted` 必须显式填写 `true`、`false` 或 `null`，表示独立验收通过、失败、尚未验收。服务端 `completed` 只用于检查用量是否收尾，绝不自动转换为验收通过。建议使用统一测试和盲评，记录所有失败、重试和人工修复。
- 每个 attempt 选择一个 `run_id`，或 `user + session_id + turn_ids`。普通单 Agent 的全部修复回合均应包含在 `turn_ids` 中；一次失败后重新开始可作为同组的新 attempt。不能省略失败费用。重复 run、重复 turn，以及跨两种来源重复认领同一回合都会拒绝。
- `elapsed_ms` 是可选的实验实测总耗时；协作缺省使用创建到最后更新的时间差，包含等待时间。普通聊天缺少该字段时不猜测总耗时。`model_wall_ms` 另外按每回合账本最大 wall time 累加，避免多模型行重复计算；它不包含平台验证、排队和用户等待。用量缺失时总模型耗时为 `null`，`recorded_model_wall_ms` 仅表示已记录部分。
- 普通聊天的模型返工次数可用 `model_repairs` 显式声明；缺省为未知，不能把多回合简单推断成返工。协作直接使用持久 `repairs_used`，不接受清单覆盖。
- `human_repairs`、`human_ms` 可记录人工修复次数和时间。省略表示未知，不能自动当成零；人工劳动没有换算成美元。

下文的总成本均指上述所选编码回合和决策的账本范围。报告会按 variant 汇总尝试次数、验收覆盖率、独立验收成功率、独立成功交付的任务数、全部已记录成本、每个成功交付任务的平均成本、缓存分桶、返工和耗时。平均成功交付成本的分子包含该组失败尝试，分母按成功的不同任务去重；仅在所有尝试均有验收结论且费用完整时提供。这个指标需要相同任务组合才可比较。

普通聊天先读取持久回执的 `thread_id`，再按 `user + session_id + turn_id + thread_id` 精确关联用量。同用户、空间和回合编号下的其他线程用量不会混入；缺少回执时不猜线程归属，报告为未知。

协作模型用量按原步骤的 `turn_id + participant generation/session + user + run` 精确关联，包含失败和返工。决策费用单独取 `decision_attempts`，不再叠加 `usage_events.kind=decision`，避免双算；未结费用的预算占用另列。免费决策调用也可以有未知 token，报告分别标记费用和 token 完整性。

`recorded_cost_micro_usd` 是已记录金额，`total_cost_micro_usd=null` 表示尚不能确定总额。无用量、未定价、未完成的回合、旧聊天无持久收据、未知决策费用都会标注原因；不能将它们当成零成本成功。缓存读取占比只用编码模型的未缓存输入、缓存读取和缓存写入作为分母，不把输出 token 或 Jev token 放进分母。

`comparison` 列出各组共有的任务和是否完全配对。首版不自动输出节约百分比，也不声称存在因果收益。报告不导出 prompt、raw、provider session、账号凭证、用户或空间身份。验证入口为：

```sh
python3 scripts/verify.py run --step policy.collaboration-cost-report
```

这里的本地回归使用临时数据库与合成账本，只证明分析逻辑和只读边界。真实模型成功率和省钱效果需要另行组织相同基线、相同验收标准的真实任务实验。
