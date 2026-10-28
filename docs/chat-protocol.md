# 聊天持久接收协议 v1（v0.1.11）

[API](api.md) · [草稿与恢复](chat-recovery.md) · [M3 进度](milestones/m3.md)

v0.1.11 已实现服务端协议和原执行器接入。浏览器现已接入冻结待确认副本、按原 ID 查询和结果核对界面。存储、服务端合成执行与真实浏览器合成 API 场景分别验证；真实 API 与浏览器 Linux 联合故障矩阵已通过；性能设备/阈值仍待确认。M4 的版本化字段与状态契约见 `contracts/chat-errors-v1.schema.json`，TS 类型由 `scripts/generate-contracts.py` 生成。

## 能力、身份与请求

`GET /api/me` 返回 `chat_protocol:1` 和 `chat_scope`；后者按实例身份、用户名和用户创建时间隔离，读取实例身份失败时为空，禁止提交。它与 `draft_protocol` / `draft_scope` 独立。正常重启保留范围，备份恢复到新实例重新生成范围。旧客户端继续使用 WS，未提供客户端 ID 的消息不获得持久确认或重放保证。

网页只有在能力缺省或为 0 时使用旧发送路径。显式未知协议版本会停止发送并要求刷新/升级；已选择 v1 后，格式错误或断网只保留副本并查询，不自动降级到无 ID 的 WS。轮询响应必须含有效的版本、scope、requests 和 pending；跨实例范围变化停止新提交。

所有下列路径均需要登录并验证空间属主。写操作只接受 Authorization 头；管理员也不能读取他人空间回执。回复为 `Cache-Control: no-store`。请求 ID 是客户端在发送前生成的 32 位小写十六进制随机值，按用户创建身份、空间和 ID 去重，不是日志关联 ID。

`PUT /api/sessions/{id}/chat/requests/{request}` 的 JSON：

```json
{
  "scope": "从当前 /api/me 获取的 chat_scope",
  "thread_id": "从当前历史获取的 active_thread",
  "text": "冻结后的完整提示词（含原附件标记）",
  "model": "gpt-5.5",
  "effort": "",
  "effort_control": "",
  "attachments": []
}
```

可选 `"suggestions": true` 请求回合结束后的下一步提示，只在 `/api/me` 返回 `prompt_suggestions:1` 时发送（旧服务端拒绝未知字段），值为 false 时省略以保持旧指纹。服务端仅对 Claude 空间、且容器内 `claude --help` 含 `--prompt-suggestions` 时给 CLI 加该参数（探测结果按容器缓存）；Codex 或旧镜像照常执行、不报错。CLI 在 result 之后输出 `prompt_suggestion` 事件，落盘并广播，前端在空输入框里显示为占位文字，Tab 或「填入」只填入、不发送。生成提示是一次额外模型请求，不出现在输出流和 transcript 消息里，只能从进程退出时写入的累计 `cost-state` 减去 result 时的 `modelUsage` 得到；按 `prompt-suggestion:<turn_id>:<模型>` 去重，记为 `kind=suggestion` 并照常扣额度；缺少任一份累计值或差值为负时只记日志、不猜测。

可选 `"questions": true` 表示发送页能显示 Agent 的提问并作答，只在 `/api/me` 返回 `chat_questions:1` 时发送，false 省略。服务端仅对 Claude 空间、且容器内 `claude --help` 含 `--permission-prompt-tool` 与 `--input-format` 时改用交互回合：CLI 加 `--input-format stream-json --permission-prompt-tool stdio`，正文作为一条 JSON 用户消息写入，stdin 保持打开到 `result` 事件后再关闭（下一步提示照常在关闭后输出）。不带该字段、旧镜像或 Codex 仍是一次性回合，模型拿不到 AskUserQuestion 工具，不会出现没人能答的提问。

交互回合里 CLI 用控制协议（`control_request` / `control_response`）向服务端请示：AskUserQuestion 的 `can_use_tool` 交给用户；其他工具的权限请求一律拒绝（与 `-p` 原行为一致，bypass 模式下不会出现）；未实现的控制请求回 error，CLI 不会卡住。控制消息不落盘、不广播。每个提问在线程 JSONL 落两条 `kind:"question"`，`event` 为 `{id, state, questions?, answers?, expires_at?}`：先是 `pending`（带问题、选项与截止时间），再是 `answered` / `skipped` / `expired` / `cancelled`。`id` 是 `tool_use_id`。

作答接口 `POST /api/sessions/{id}/chat/questions/{question}`（属主鉴权），正文 `{"answers":{"问题原文":"回答"}}` 或 `{"skip":true}`：每个问题都要有非空回答，可以是选项之外的自由文本，多选按 `, ` 连接。只认第一份回答，之后或问题已结束返回 409 `question_closed`。回答原样作为 `updatedInput.answers` 交回 CLI，同一回合继续；跳过与超时以拒绝工具调用的方式告诉模型按自己的判断继续并说明假设。提问等待 10 分钟（`chat.QuestionTimeout`）后自动按超时处理，等待期间回合的总时限暂停计时。用户中断走协议内 `interrupt`，CLI 撤回提问（`control_cancel_request`，记为 `cancelled`），以失败结果和退出码 1 收尾，回执记为已中断；10 秒内没有结束才退回 SIGINT。提问只存在于运行中的房间，服务重启时随回合丢失，回执按原规则记为结果不确定。

提交前冻结所有字段，不在异步附件检查后重新读取模型或输入。模型必须是具体名称；空推理设置仍表示 CLI 继承，不声称已测得 provider 的有效档位。附件列表最多 64 项，只接受 `/shared/.images/` 或 `/shared/.file/` 中受限普通文件；接收和运行前均复核，不跟随链接。正文中的普通路径文字不自动解释为附件，避免阻止“请创建这个文件”之类任务。

正文最多 1 MiB UTF-8；HTTP JSON 最多 4 MiB。拒绝未知字段、多段 JSON、无效线程和过长选项。线程必须仍为当前 active_thread。服务端先检查已存在的同 ID 回执；内容相同返回原结果，内容改变返回 409。新消息才重新检查额度、账号、模型/推理选项、线程和附件。模型执行前仍沿原路径再次验证配置/授权。

新接收返回 **202**，重放返回 **200**，形状为 `{version:1, receipt:{...}, replayed:false|true}`。回执先以 SQLite FULL 事务提交，仅取得 fresh=true 的调用者能安排原回合执行；202 不表示 provider 已执行。客户端必须按 `request_id` 和 `revision` 合并乱序结果，不能用相同文本、WS 广播或“连接正常”冒充确认。

## 查询与状态

| 接口 | 返回 |
| --- | --- |
| `GET .../chat/requests/{request}` | `{version:1, receipt}`；查不到为 `chat_request_not_found` / 404 |
| `GET .../chat/requests?thread=<可选线程>` | `{version:1, scope, requests, pending, pendings}`；requests 为最新 50 条；pendings 列出空间里各线程的活动/待核对回执（每条线程至多一条，按时间），pending 是其中所给线程的那条，没给线程或该线程没有时取最早一条（旧客户端照旧只读它）；pending_only=1 时 requests 只含该空间排队中的回执（按顺序，最多 50 条），供网页轮询显示和撤回，避免反复下载历史正文 |

回执字段为 `request_id/session_id/thread_id/turn_id/request/state/error_code/revision/created_at/updated_at`，引导消息另有 `merged_into`。`request` 保存冻结输入，删除/放弃的围栏为 null；`error_code` 为固定标识，不含 provider 原始错误、凭证或宿主路径。turn_id 从接收起稳定，沿原用量路径用于回合归属。

| state | 含义 |
| --- | --- |
| queued | 回合运行中发来的跟进（`delivery`），已持久接收，排在本线程当前回合（及更早的排队消息）之后，或空间已有 3 条线程在运行时等空位；不占活动名额，可撤回 |
| accepted | 已持久接收，尚未进入启动阶段 |
| starting | 正在准备空间和选项，尚未进入模型调用阶段 |
| running | 已持久登记即将调用模型；该阶段发生断线/错误时不能推断没有执行 |
| completed | 收到 provider 明确成功结束信号，执行路径正常收尾，已完成原用量 flush 和转录同步 |
| failed | 明确失败且执行已结束。`error_code` 为 `turn_failed` 时表示 CLI 已运行后自报错误或以非零状态退出：进程已不在运行，但可能改动了部分文件；其余代码表示在模型调用阶段之前失败、没有执行。都不阻止新 ID |
| interrupted | 用户请求中断且得到结束证据；不代表回滚文件或扣款 |
| uncertain | 可能已执行、部分执行或结果/结算无法确认，阻止本线程新的回合（其他线程照常） |
| reviewed | 用户已明确核对未知结果，允许以新 ID 开始后续工作；不把旧结果改成成功、不重跑旧 ID |
| abandoned / deleted | 阻止迟到重放的永久 ID 围栏 |

启动在连接 Docker 前将 accepted/starting/running 改为 uncertain。查询发现回执没有对应执行者时也转为 uncertain，不派发任务。缺少终结事件、流损坏、拿不到退出状态、历史/用量写入失败均保留 uncertain。只有「CLI 明确报错且进程已退出」（provider 失败事件后执行器正常返回，或拿到非零退出码）记为 failed/`turn_failed`：此时没有仍在运行的执行者，阻止新回合只会增加确认步骤；用户已请求中断时同样的退出记为 interrupted。服务关闭不会被冒充为用户确认中断。completed 表示执行协议和已识别用量的收尾，不证明任务质量、所有工具副作用或上游未上报的费用。

丢失确认时先查询原 ID。404 不证明迟到请求不会到达；可以查询或按**原 ID、原内容**重试。若决定不再发送该未知 ID，先成功写入 abandon 围栏，再使用新 ID。绝不因网络失败自动换 ID 重发。

## 显式动作

`POST .../chat/requests/{request}` 接收 `{scope, action, revision}`，回复 `{version:1, receipt}`。

- `interrupt`：revision 必须匹配，且该 ID 仍拥有当前执行者。请求发送中断信号，不立即伪造 interrupted；重放中断不会重复发 SIGINT，也不能误中断后来开始的回合。
- `review`：先查看历史和文件，再明确核对。只接受无执行者的 uncertain 及匹配修订；重复核对可读取 reviewed。释放新工作的准入，不重跑、不结算旧 ID，也不保证已脱离连接的外部进程停止。
- `abandon`：只围住尚未接收的 ID；重复放弃可重放。已接收请求返回冲突，不能通过放弃隐藏在途结果。此动作不要求 revision。
- `withdraw`：撤回仍在排队的跟进，revision 必须匹配，记为 abandoned/`withdrawn`，不会再执行；已开始的请求返回冲突。网页撤回后把原内容放回空输入框。

## 跟进：排队与引导

`/api/me` 返回 `chat_followups:1` 时，网页在回合运行中可以继续发送，输入带 `"delivery":"queue"` 或 `"steer"`（随正文冻结、参与指纹，不带时与旧请求指纹一致）。不带 delivery 的普通发送在本线程忙时仍返回 `chat_busy`，空闲但本线程队列非空时返回 `chat_pending`，不能插到排队消息前面；空间已有 3 条线程在运行时返回 `chat_parallel_limit`（网页此时自动带 `queue` 发送）。

- **排队**：本线程空闲、没有更早的排队消息且空间还有空位时直接作为普通回合执行；否则记为 `queued` 并广播。每个回合结束、核对 uncertain 之后以及服务启动时，按各线程最早排队消息的先后逐条线程派发：重新检查额度、账号、模型、线程仍存在和附件后改为 accepted 并执行；不再满足的记为 failed（如线程已删除时的 `chat_thread_changed`），不执行，继续取该线程的下一条；空位用完即停，等下一个回合结束。待核对的 uncertain 只挡住本线程的队列，直到用户核对。排队回执随数据库持久保存，服务重启不丢；删除线程、空间或用户时与其他回执一起清空正文。
- **引导**：只对正在运行、输入仍开着的 Claude 交互回合（即带 `questions` 的回合）生效。服务端在持有该回合输入锁时提交回执（直接记为 completed，`merged_into` 为运行中请求 ID，turn_id 为空），再写入 `{"type":"user",…,"priority":"next"}`；CLI 在当前工具步骤结束后接收，若模型已经结束则作为同一进程的下一轮执行。交互回合带 `--replay-user-messages`，CLI 接收时回显该消息，服务端据此在历史中、实际生效的位置记一条 `kind:"steer"`（`{id,text,state:"applied"}`）；回合结束仍未回显的记为 `dropped`，不重发。回显行和控制消息都不落盘。回合不接受输入时（没有回合、一次性回合、Codex、`result` 之后已关闭输入），引导自动按排队处理。
- 两种跟进都复用原回执的重放、指纹和查询规则；同一 ID 重试不会重复排队或重复写入 CLI。
- **后台任务**：模型用 `run_in_background` 启动的命令在 `result` 之后仍让 CLI 进程开着，结束时 CLI 把结果交回模型再答一段，同一回合内可能有多个 `result`。这段时间回合仍是 running，排队消息继续等待；引导因输入已关闭改为排队。网页据 `system` 的 `background_tasks_changed`（当前全部后台任务）、`task_started`、`task_updated`、`task_notification` 在执行指示上显示「等待后台任务结束：<描述>」，结束通知在对话里留一行。

## 用户中断的历史记录

用户请求中断后，CLI 以失败的 `result` 结束（交互回合是 `error_during_execution`，进程退出码 1；一次性回合是 SIGINT）。服务端在该 `result` 的历史条目上加 `state:"interrupted"`，并以 `{kind:"status", state:"interrupted"}` 收尾，不再记 `status:error`/`chat_failed`。网页对带此标记的 result 不显示错误，只在 status 处显示一行「已中断」。回执本身按上文仍记 interrupted/`operation_cancelled`。没有用户中断的失败 result 照常显示为错误。旧网页忽略这个新 status，回合同样视为结束。

状态变更通过 WS 增补 `{type:"chat_request", version:1, receipt}` 广播；旧客户端忽略该消息。广播丢失时 GET 是恢复入口。旧 WS 发送落在当前线程，同样检查它的持久活动状态。读取历史和文件仍可用。

## 并行线程

同一空间最多 3 条线程（`maxParallelChats`）可以同时运行回合，每条是容器里独立的 CLI 进程。服务端每条线程一个回合槽（`chat.go` 的 `chatTurn`）：回合写自己线程的转录，用自己的 provider 会话续聊（是当前线程时仍读写空间记录的 `chat_session`，否则读转录里最后一条 `chat_session`），PID 记在按线程区分的文件里，中断只打到本线程的 CLI；委派同一空间同时只给一条线程（另一条的消息照常执行，并推送 `delegation_notice`、`code:"delegation_busy_this_turn"`）。新建、切换线程不再等待运行中的回合，删除只等被删线程自己的回合；切换账号要求整个空间没有回合在跑或开始。

- 线程内的事件（`status`、`user_message`、`agent_event`、`turn_cost`、提问、委派等）在 WS 消息里带 `"thread":"<线程 id>"`；回执广播靠 `receipt.thread_id` 区分。网页只画自己正在看的线程，其余只记运行状态并在完成、出错或提问时提示。旧网页不认这个字段。
- 连接时的 `status` 增加 `threads`：`{"<线程 id>": {"elapsed_ms": …, "phase": …}}` 列出所有在跑的线程；顶层 `state`/`elapsed_ms`/`phase` 仍描述服务端的当前线程，供旧网页使用。
- 删除线程时广播 `{type:"thread_deleted", id}`，正看着它的页面回到当前线程；新建/切换仍广播 `{type:"thread", id}`，新网页不再跟随别的页面换线程。`GET .../history` 不翻页时也接受 `thread=<id>`（线程存在才生效），页面重连补拉时留在自己的线程上。

## 保留与备份

回执进入系统/full 数据库备份，包含已接收的提示词、选项和附件引用；本机草稿保存开关不会撤销服务端已接收请求。附件实际内容与工作区仍需 full 备份。TTL 清理保护回执和转录中的附件引用，读取回执失败时不删除候选文件。

线程删除先清回执正文并保留 ID，再移除转录；两者并非同一个文件系统/数据库事务，失败可重试，但旧 ID 不复活。空间/用户删除在原数据库事务内清正文留围栏；保留旧备份和数据库空闲页不等于物理擦除。删除空间后的 API 仍遵守空间属主入口，空间已不存在时返回 404，不因此重新分配空间或重新执行消息。
