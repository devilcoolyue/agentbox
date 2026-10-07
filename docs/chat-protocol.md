# 聊天持久接收协议 v1（开发中）

[API](api.md) · [草稿与恢复](chat-recovery.md) · [M3 进度](milestones/m3.md)

当前源码已实现服务端协议和原执行器接入，尚未发布。浏览器现已接入冻结待确认副本、按原 ID 查询和结果核对界面。存储、服务端合成执行与真实浏览器合成 API 场景分别验证；真实 API 与浏览器 Linux 联合故障矩阵已通过；性能设备/阈值仍待确认。M4 的版本化字段与状态契约见 `contracts/chat-errors-v1.schema.json`，TS 类型由 `scripts/generate-contracts.py` 生成。

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

提交前冻结所有字段，不在异步附件检查后重新读取模型或输入。模型必须是具体名称；空推理设置仍表示 CLI 继承，不声称已测得 provider 的有效档位。附件列表最多 64 项，只接受 `/shared/.images/` 或 `/shared/.file/` 中受限普通文件；接收和运行前均复核，不跟随链接。正文中的普通路径文字不自动解释为附件，避免阻止“请创建这个文件”之类任务。

正文最多 1 MiB UTF-8；HTTP JSON 最多 4 MiB。拒绝未知字段、多段 JSON、无效线程和过长选项。线程必须仍为当前 active_thread。服务端先检查已存在的同 ID 回执；内容相同返回原结果，内容改变返回 409。新消息才重新检查额度、账号、模型/推理选项、线程和附件。模型执行前仍沿原路径再次验证配置/授权。

新接收返回 **202**，重放返回 **200**，形状为 `{version:1, receipt:{...}, replayed:false|true}`。回执先以 SQLite FULL 事务提交，仅取得 fresh=true 的调用者能安排原回合执行；202 不表示 provider 已执行。客户端必须按 `request_id` 和 `revision` 合并乱序结果，不能用相同文本、WS 广播或“连接正常”冒充确认。

## 查询与状态

| 接口 | 返回 |
| --- | --- |
| `GET .../chat/requests/{request}` | `{version:1, receipt}`；查不到为 `chat_request_not_found` / 404 |
| `GET .../chat/requests?thread=<可选线程>` | `{version:1, scope, requests, pending}`；requests 为最新 50 条，pending 独立返回当前空间的活动/待核对回执，不受线程过滤影响；pending_only=1 返回空 requests，供网页轮询避免反复下载历史正文 |

回执字段为 `request_id/session_id/thread_id/turn_id/request/state/error_code/revision/created_at/updated_at`。`request` 保存冻结输入，删除/放弃的围栏为 null；`error_code` 为固定标识，不含 provider 原始错误、凭证或宿主路径。turn_id 从接收起稳定，沿原用量路径用于回合归属。

| state | 含义 |
| --- | --- |
| accepted | 已持久接收，尚未进入启动阶段 |
| starting | 正在准备空间和选项，尚未进入模型调用阶段 |
| running | 已持久登记即将调用模型；该阶段发生断线/错误时不能推断没有执行 |
| completed | 收到 provider 明确成功结束信号，执行路径正常收尾，已完成原用量 flush 和转录同步 |
| failed | 在模型调用阶段之前明确失败，可核对后以新 ID 编辑/发送 |
| interrupted | 用户请求中断且得到结束证据；不代表回滚文件或扣款 |
| uncertain | 可能已执行、部分执行或结果/结算无法确认，阻止新的回合及线程变更 |
| reviewed | 用户已明确核对未知结果，允许以新 ID 开始后续工作；不把旧结果改成成功、不重跑旧 ID |
| abandoned / deleted | 阻止迟到重放的永久 ID 围栏 |

启动在连接 Docker 前将 accepted/starting/running 改为 uncertain。查询发现回执没有对应执行者时也转为 uncertain，不派发任务。缺少终结事件、流损坏、模型明确失败但可能已有副作用、历史/用量写入失败均保留 uncertain。服务关闭不会被冒充为用户确认中断。completed 表示执行协议和已识别用量的收尾，不证明任务质量、所有工具副作用或上游未上报的费用。

丢失确认时先查询原 ID。404 不证明迟到请求不会到达；可以查询或按**原 ID、原内容**重试。若决定不再发送该未知 ID，先成功写入 abandon 围栏，再使用新 ID。绝不因网络失败自动换 ID 重发。

## 显式动作

`POST .../chat/requests/{request}` 接收 `{scope, action, revision}`，回复 `{version:1, receipt}`。

- `interrupt`：revision 必须匹配，且该 ID 仍拥有当前执行者。请求发送中断信号，不立即伪造 interrupted；重放中断不会重复发 SIGINT，也不能误中断后来开始的回合。
- `review`：先查看历史和文件，再明确核对。只接受无执行者的 uncertain 及匹配修订；重复核对可读取 reviewed。释放新工作的准入，不重跑、不结算旧 ID，也不保证已脱离连接的外部进程停止。
- `abandon`：只围住尚未接收的 ID；重复放弃可重放。已接收请求返回冲突，不能通过放弃隐藏在途结果。此动作不要求 revision。

状态变更通过 WS 增补 `{type:"chat_request", version:1, receipt}` 广播；旧客户端忽略该消息。广播丢失时 GET 是恢复入口。旧 WS、线程新建/切换/删除也检查持久活动状态，不能用换线程绕过待核对记录。读取历史和文件仍可用。

## 保留与备份

回执进入系统/full 数据库备份，包含已接收的提示词、选项和附件引用；本机草稿保存开关不会撤销服务端已接收请求。附件实际内容与工作区仍需 full 备份。TTL 清理保护回执和转录中的附件引用，读取回执失败时不删除候选文件。

线程删除先清回执正文并保留 ID，再移除转录；两者并非同一个文件系统/数据库事务，失败可重试，但旧 ID 不复活。空间/用户删除在原数据库事务内清正文留围栏；保留旧备份和数据库空闲页不等于物理擦除。删除空间后的 API 仍遵守空间属主入口，空间已不存在时返回 404，不因此重新分配空间或重新执行消息。
