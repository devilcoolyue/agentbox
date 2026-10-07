# 错误码与操作关联（v1）

[API](api.md) · [排障](troubleshooting.md) · [M1 进展](milestones/m1.md)

当前源码为登录、通用鉴权/空间属主检查、空间启动、聊天准入及回合错误增加兼容字段。其他接口仍可能只有 `error`；本次不承诺全量 API 已迁移。旧发布包可能尚未包含这些字段。

## 报文与兼容

```json
{
  "error": "工作空间镜像不存在",
  "code": "agent_image_missing",
  "operation_id": "0123456789abcdef0123456789abcdef",
  "hint": "请联系管理员构建或安装配置中指定的 Agent 镜像。",
  "action": "contact_admin",
  "retryable": false
}
```

- `error` 保留为字符串，供旧客户端显示；HTTP 状态仍需检查。新客户端按 `code` 翻译，未知码回退服务端 `error` / `hint`，不能把服务端任意字符串当翻译键。
- `operation_id` 是 32 位小写十六进制关联编号。HTTP 错误同时提供 `X-Agentbox-Operation-ID`。它只用于排障，不是任务 ID、消息接收确认、幂等键或权限凭证。
- `hint` 是服务端提供的安全处理建议；`action` 表示处理类别，不能直接当作可执行命令或 URL。`contact_admin`、`reauthorize_account`、`configure_mcp` 需要对应权限者处理；`edit_request`、`sign_in`、`refresh_sessions`、`free_capacity`、`wait`、`check_result`、`reconnect` 由界面给出建议。
- `retryable` 只说明当前拒绝/连接问题是否适合在条件恢复后重试，不授权客户端自动重放任务。未知、超时、执行失败均要求先检查结果。M3 的[聊天持久协议](chat-protocol.md)使用独立客户端请求 ID，不能据本页的操作关联编号自动重发。
- 服务端生成每次 HTTP 操作的编号，忽略客户端传入的同名 HTTP header。每条聊天提交使用独立编号，和 WebSocket 连接的生命周期分开。

聊天 `type:"error"` 在同层增加上述字段；`type:"status", state:"error"` 也增加这些字段并随 JSONL 历史保存。旧事件仍可读取，其他 provider 事件格式不变。参数拒绝原有 `retry_text` 行为保留，仅发给空间内的已授权客户端，不写入操作关联日志。

浏览器无法读取失败 WebSocket 握手的响应体，因此聊天连接在 URL 上带由浏览器随机生成的 `connection_id`。服务端仅在聊天和诊断 WebSocket 路径接受符合上述 32 位格式的值，用作这次握手的关联编号；无效值会替换成服务端新编号。该值是不可信的排障参考，不证明请求身份，也不用于去重。重连生成新编号，页面在重连期间保留上一次失败编号。

如果 DNS、网络或反向代理在请求到达 Agentbox **之前**就失败，服务端可能没有对应日志。页面的连接编号不能证明服务端已收到连接或任务；需要继续检查网络与代理日志。

## 当前错误目录

权威合成样本在 [`internal/server/testdata/problems-v1.json`](../internal/server/testdata/problems-v1.json)，Go 与 TS 回归共同核对目录、字段和三语文案。

| 错误码 | 含义与下一步 |
| --- | --- |
| `invalid_request` | 检查请求输入；登录请求体上限 16 KiB |
| `invalid_credentials` | 核对用户名和密码；不存在的账号与密码错误使用相同响应 |
| `login_rate_limited` | 等待登录限制解除；忘记密码联系管理员 |
| `authentication_required` | 当前登录失效，重新登录 |
| `admin_required` | 请管理员执行该操作 |
| `session_not_found` | 空间不存在或不属于当前用户；刷新列表，不透露其他属主 |
| `account_access_denied` | 请管理员调整账号授权范围 |
| `account_unavailable` | 请管理员检查空间绑定账号是否仍存在 |
| `account_credentials_unavailable` | 请管理员检查凭证文件、轮换状态，必要时重新授权 |
| `quota_exhausted` | 请管理员充值；不改变不限额与回合开始前拦截的规则 |
| `capacity_exhausted` | 停止不用的空间或调整运行数量上限 |
| `storage_full` | 数据盘低于预留值，或文件操作返回磁盘/磁盘配额不足；管理员释放空间 |
| `storage_permission_denied` | 管理员检查目录及容器 UID 的读写权限 |
| `docker_unavailable` | 管理员检查 daemon、socket/网络及访问权限 |
| `agent_image_missing` | 管理员构建/安装配置中指定的镜像 |
| `workspace_start_failed` | 未归类的启动失败；管理员通过编号与环境检查排查 |
| `chat_busy` | 等待当前回合完成或先中断 |
| `chat_options_invalid` | 调整模型/推理参数，检查 CLI 兼容性 |
| `chat_failed` | 先检查历史与工作区结果，避免盲目重做已经执行的工作 |
| `chat_request_conflict` | 消息 ID 内容或状态修订冲突，先查询原回执 |
| `chat_request_gone` | 消息已放弃/删除，原 ID 不再执行 |
| `chat_request_not_found` | 尚无接收记录；保留原 ID 查询，不自动换 ID 重发 |
| `chat_pending` | 存在活动/未知结果；先查看历史与文件，再显式核对 |
| `chat_scope_changed` | 实例或登录身份范围改变；重新登录并核对原任务 |
| `chat_thread_changed` | 返回消息原属线程，核对输入后再发送 |
| `chat_attachments_invalid` | 移除失效附件或重新上传 |
| `mcp_configuration_failed` | 在 MCP 设置中处理冲突并检测配置 |
| `operation_timed_out` / `operation_cancelled` | 先检查空间和任务状态，再决定下一步 |
| `server_stopping` | 稍后重新连接并核对任务状态 |
| `websocket_failed` | 检查网络、同源策略与反向代理的 WebSocket 支持 |
| `internal_error` | 未分类的服务错误，提供操作编号给管理员 |

Docker 缺镜像在 ImageInspect 边界标记，连接失败使用 Docker 客户端的类型；磁盘预留、系统 errno、授权与容量使用哨兵/类型判断。不能根据 `err.Error()` 的关键词猜错误类型。未知底层错误不会直接拼进这些接口的公开响应。

## 日志与验证

相关失败输出一条白名单日志：

```text
operation_failed operation_id=0123456789abcdef0123456789abcdef operation=session.start code=agent_image_missing action=contact_admin retryable=false
```

关联日志不包含用户输入、密码、令牌、完整请求 URL、底层原始错误或宿主路径。普通用户只取得自己请求/空间的结果；增加编号不放宽任何鉴权。既有其他模块的日志仍按原规则处理，本次没有对所有日志做全局脱敏。

```bash
go test ./internal/server -run '^TestProblems|TestInvalidEffortRejected' -count=1
go test ./internal/dockerx -run TestMissingAgentImage -count=1
npm run check
npm run build
npm run test:i18n
npm run test:problems
# 使用开发文档中的 Playwright 配置；合成 API，无真实账号/模型
AGENTBOX_BROWSER_ONLY_PROBLEMS=1 node scripts/test-browser.mjs
```

`TestProblemsStartupThroughWorkspace` 需 root 才能实际播种 UID 1000 的 home，普通用户运行会明确跳过；Linux CI 的完整 server 测试按项目约定以 root 执行。Docker 不可达/缺镜像通过合成运行时注入，磁盘不足通过超过实际可用容量的保留值触发；没有填满真实数据盘、停止真实 Docker 或删除真实镜像。另有 Docker Engine 合成 HTTP 404 回归验证镜像错误分类和拒绝创建容器。浏览器使用合成 API/WS，不能代替生产反向代理验收。
