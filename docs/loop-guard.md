# 独立模型回复循环网关（开发预览）

`abox-loop-guard` 放在 agent harness 与模型 API 之间，实时检测短句复读并取消上游请求。它是独立二进制，不需要运行 Agentbox、Docker 或数据库；同一实现也能通过 Go 包 `agentbox/pkg/loopguard` 嵌入其他网关。它不属于 Claude/Codex 的技能或 MCP 工具：这些入口通常不能观察并中断一次尚未结束的模型输出。

Agentbox 内已作为首个扩展 `agentbox.loop-guard` 提供：管理员在「扩展」菜单启用、批准权限并绑定 Claude 或 Codex 的 API Key / 中转账号即可，不必再单独部署本网关或手改账号 base URL；独占连接在扩展页的「每次请求独占一条连接」开启。迁移步骤见下文「从独立网关迁到扩展」。两者共用 `pkg/modelgw` 的转发与协议解码和 `pkg/loopguard` 的检测核心。设计与限制见 [扩展与 SPI 协议](architecture/extensions-spi.md)。独立二进制继续保留，用于不运行 Agentbox 的场景。

```text
Codex / Claude Code / DSH / Pi / 其他客户端
                │ HTTP SSE，自定义 API base URL
                ▼
         abox-loop-guard
                │ 同协议转发，不转换请求
                ▼
          模型供应商 / 现有中转
```

## 参考方案与行为

参考 [Magpie #1359](https://github.com/yetone/magpie/issues/1359) 的网关思路：最近 256 个句子/行只有不超过 24 种内容时，判为低多样性复读。也参考了 [dsh-loop-guard](https://github.com/MrWeiCodes/dsh-loop-guard) 对流内中断、纠正提示和工具参数完整性的讨论。本实现独立编写，没有复制这两个项目的源码。

Magpie 的 issue 说明，v0.1.1142 加入流式保护，部分协议转换路径还能在未输出正文前重问一次；后续并发实验没有支持“同账号并发导致上下文串线”的判断，不能把限制并发当成已证实的修复。

DSH 插件依赖自己的 `llm/stream`、`steer`、`whenIdle` 和 `followup` 接口。其当前实现为兼容 DSH 会闭合文本块并发送 `stop`；这不能直接作为跨 harness 的通用语义。这里始终返回明确错误，不把被截断的任务标记为完成，不自动发送提示、不切模型、不重放工具。

## 构建与启动

使用本仓库 `go.mod` 对应的 Go 工具链：

```bash
go build -o /tmp/abox-loop-guard ./cmd/abox-loop-guard
/tmp/abox-loop-guard -upstream https://api.example.com
```

默认监听 `127.0.0.1:8788`。`-upstream` 是固定来源或路径前缀：客户端的完整请求路径会附加在它之后。例如客户端发 `/v1/responses`，上游为 `https://api.example.com`，则转到 `https://api.example.com/v1/responses`。不要在两侧重复添加 `/v1`。

客户端把原 API base URL 改为本网关地址，原模型名称与供应商 API Key 保持原含义。网关转发 `Authorization`、`x-api-key`、`x-goog-api-key` 等原始认证字段，使用现有环境中的出口代理，不做供应商登录与 OAuth 续期。不跟随 HTTP 重定向。

例如 OpenAI 兼容客户端的 base URL 为 `http://127.0.0.1:8788/v1`；Anthropic 兼容客户端使用 origin `http://127.0.0.1:8788`，由客户端追加 `/v1/messages`。Gemini 使用匹配其 SDK 配置的 origin，并启用 SSE（通常 `alt=sse`）。各客户端的配置项名称不同；不承诺不能修改 API 地址或只走 WebSocket 的客户端能直接接入。

监听非回环地址时，CLI 要求设置环境变量 `LOOP_GUARD_TOKEN`，每个客户端另加 `X-Loop-Guard-Token` 请求头；该头在转发前移除，不能替代供应商 API Key。在容器里，`127.0.0.1` 指容器自己；应使用可访问的宿主机地址或同网络 sidecar 地址。跨主机接入应通过已有 HTTPS 入口，网关自身只监听 HTTP。

已有客户端不能添加自定义头时，可用 `-auth-header Authorization`，并将 `LOOP_GUARD_TOKEN` 设置为完整的 `Bearer <原供应商 Key>`。网关会精确校验这个值，同时保留原 Authorization 转发给上游；仅修改客户端 base URL 即可。这种模式绑定一份供应商 Key，轮换 Key 时必须同步更新网关令牌并重启网关，不能把它当成支持任意账号 Key 的共享入口。令牌宜由权限为 0600 的 systemd EnvironmentFile 提供，避免放进命令行。

Linux 常驻服务模板见 [`deploy/abox-loop-guard.service`](../deploy/abox-loop-guard.service)。二进制位于 `/opt/abox-loop-guard/current/abox-loop-guard`，环境文件 `/etc/abox-loop-guard/gateway.env` 提供 `LOOP_GUARD_LISTEN`、`LOOP_GUARD_UPSTREAM`、`LOOP_GUARD_AUTH_HEADER` 和 `LOOP_GUARD_TOKEN`。服务使用动态低权限用户，无需改动 `agentbox.service`。先验证监听、鉴权和上游连通性，再切换目标账号；回退时先还原账号 base URL，再停止网关。

`-dedicated-connections` 开启请求独占连接：只用 HTTP/1.1，禁止 keep-alive 复用，循环取消时关闭这一次请求的 TCP/TLS 连接。systemd 模板要求环境文件另设 `LOOP_GUARD_DEDICATED_CONNECTIONS=true` 或 `false`。代价是每次请求重新握手。默认不开启时，HTTP/2 取消通常通过 RST_STREAM 结束单条流，连接仍可供其他请求使用；这本身也是合法取消，不能凭连接是否复用判断中转站有没有终止模型。独占连接能明确关闭我们这一跳，不能强制中转/CDN 关闭它自己到供应商的连接。

## 从独立网关迁到扩展

适用于已经把某个账号的 base URL 手工改到独立网关（如 `http://172.17.0.1:8788`）的部署。顺序的目的是全程都有保护：先让扩展网关接手，再撤掉独立网关这一层。

1. 发布包含 Codex 支持的版本（按 `docs/architecture/deployment-layout.md` 的布局用 `deploy/release.py` 暂存、激活）。发布会重启 `agentbox.service`，不影响独立网关。
2. 管理员打开「扩展 → 循环保护」：勾选启用，批准「请求指纹」「读取模型回复文字」，「作用于哪些账号」选「指定账号」并勾上原来受保护的账号；独立网关开了 `LOOP_GUARD_DEDICATED_CONNECTIONS=true` 的，在扩展列表页勾上「每次请求独占一条连接」。保存后扩展页显示「运行中」、网关状态为「运行中」。此时该账号的请求依次经过扩展网关和独立网关，两层都在检查。账号绑定了出口 IP 代理时例外：扩展网关会经代理去连宿主机上的独立网关，通常连不上（不会回落直连）；这种情况先做第 4 步、再启用扩展，中间短暂没有保护。
3. 在该账号的空间里新开一轮对话：工作空间头部出现盾牌标记；对话正常回复；回复期间扩展页「检查中的流」大于 0。容器访问不到网关（防火墙只放行了特定端口）时 CLI 会连接失败，需放行 `extensions.gateway.bind` 的端口（默认 1083）。
4. 编辑该账号的 API Key 设置：地址填回原供应商（例如 `https://api.1024gate.com/v1`），Key 填原来那一个（保存接口要求重新填写），接口类型不变。保存会把账号池改回真实上游，并把各空间的会话配置重新写成扩展网关地址。
5. 再开一轮对话确认正常，并确认独立网关日志不再有新请求：`journalctl -u abox-loop-guard --since "5 minutes ago"`。已经开着的终端里正在运行的 Codex 仍用旧地址，重新启动即可。
6. `systemctl disable --now abox-loop-guard`。保留 `/etc/abox-loop-guard/gateway.env` 和 `/opt/abox-loop-guard`，以便回退。

回退：`systemctl enable --now abox-loop-guard`，把账号地址改回独立网关，然后在扩展页解绑或停用。只停用扩展而不改账号地址时，账号直连原供应商、不再受保护。

## 协议与检测范围

| 协议 | 检测字段 | 中断结果 |
| --- | --- | --- |
| OpenAI Responses | output text、reasoning text、reasoning summary 的 delta | SSE `error` + `response.failed`，含递增 `sequence_number` |
| Chat Completions | `content`、`reasoning_content` / `reasoning` | SSE JSON `error` |
| Anthropic Messages | `text_delta`、`thinking_delta` | SSE `error` 事件 |
| Gemini | candidate parts 的 text / thought | SSE JSON `error`，`FAILED_PRECONDITION` |

错误码为 `model_output_loop`。检测按请求、候选、内容通道隔离，Responses 还区分 item / content / summary。只读取增量，不把最终全文快照再累计一次。工具调用参数、工具结果和 usage 不进入检测器。

Responses 错误使用 [OpenAI 官方流式事件定义](https://developers.openai.com/api/reference/resources/responses/streaming-events) 的 `type/code/message/param/sequence_number`。Codex 0.162.1 会忽略通用 `error` 并继续等待终止事件，因此随后补发 `response.failed`，保留上游响应身份并说明原因；不构造空 `output` 快照去覆盖客户端已收到的内容。

检测器按字符重建句子，支持中英文标点，不依赖网络分片方式；不是英文短语黑名单。超过 160 字符的单位、带数字或代码符号的行、代码围栏会清空候选窗口，以减少正常代码、日志和长推理的误伤。

**边界：**这是针对短句复读的保守启发式，不能保证无误报；用户确实要求大量重复自然语言时也可能触发。没有标点/换行的长串、长段落重复、代码围栏内循环、被供应商隐藏的推理、只在最终快照出现的正文，以及跨多次请求的重复工具调用不在当前覆盖范围。后者需要 harness 层的工具执行轨迹与进度判断，不能靠此文本检测器证明。

## 中断、重试与用量

- 命中后取消上游 HTTP context 并关闭响应体，保留此前已转发的内容，返回错误后结束当前流。不会发送成功完成事件或补齐半个工具调用。
- 已执行的工具不可回滚。模型上游是否停止计费取决于其取消处理，不能保证连接断开即退费。
- 网关不自动重试。相同路径/查询、供应商认证与请求正文的 SHA-256 指纹在命中后冷却 30 秒，再次请求返回 HTTP 422，不到达上游。JSON 按对象键序归一化后计算指纹，保留数字精度，避免 Codex 重试时仅改变对象字段顺序就绕过冷却；发给上游的仍是原字节。最多记 1024 条，重启会清空。不同正文、不同 Key、超过冷却时间以及此前已经并发发出的请求不受这个小型闸门阻止；它不是跨客户端的永久循环断路器。
- 正常流保持原字节与 usage。取消时供应商可能还没发送最终用量，网关不编造 token 数、不补写账本；账务仍需原系统/provider 的记录核对。
- 接入 Agentbox 后，网关错误必须由 CLI 正确上报，原有回执、历史、结算及不自动重放规则继续有效。当前没有改动 Agentbox 的聊天执行器；生产仅定向修改测试账号的模型地址。
- 恢复任务建议先检查已完成的工具操作，带简短纠正信息继续，必要时开启精简后的新上下文。自动纠偏需要各 harness 适配明确的取消确认、持久化和新回合准入，并限制次数；第一版不承诺自动续跑。

## 限制与诊断

### 中转站在客户端断开后继续生成

已核对 sub2api 0.1.133（提交 `68901cbfff783af794d96028be8dad3e532c0fe7`）的 Responses → Chat Completions 转换路径：它使用 `context.WithoutCancel` 将上游请求与客户端取消分离，客户端断开后仍读取上游直到结束，以收集计费用量。源码见 [请求上下文与断开后的读取逻辑](https://github.com/Wei-Shaw/sub2api/blob/68901cbfff783af794d96028be8dad3e532c0fe7/backend/internal/service/openai_gateway_responses_chat_fallback.go#L113)。因此，即使本网关关闭到该中转站的 TCP 连接，也无法阻止它继续请求供应商。

此类链路可评估将网关放在中转站的供应商出口：`客户端 → sub2api → loop guard → 原供应商`，仅调整目标上游账号。切换时需同步核对供应商鉴权、协议转换、重试与不完整用量处理，并避免网关与中转站互相转发形成环路。这是接入建议，当前实现不会自动修改 sub2api 配置，也不能保证最终供应商遵守取消。

### 其他边界

非 SSE、未知端点及非成功 HTTP 响应原样转发，响应头 `X-Loop-Guard-Status: not-inspected` 明确标记；受检 SSE 为 `monitoring`。不支持 WebSocket / CONNECT。上游必须遵守 `Accept-Encoding: identity`，不接受无法检查的压缩 SSE。

资源边界：受检 POST 正文最多 32 MiB，单个 SSE 事件最多 1 MiB，每请求最多同时保留 128 个检测通道，最多 64 个并发请求，单请求超时 30 分钟。事件过大/流读取失败会产生独立 `loop_guard_stream_error`，不会伪称模型循环。CLI 入站请求读取期限为 30 秒。超大多模态请求需另行评估这些限制。

日志仅记录错误码、通道、窗口和不同短句数量；不保存提示词、回复、认证或请求 URL。冷却缓存只有哈希和过期时间。

嵌入现有 Go 网关：

```go
guard, err := loopguard.New(loopguard.Config{Upstream: "https://api.example.com"})
// err 检查后，将 guard 注册为 HTTP handler。
```

只使用检测核心时，为每个请求/候选/文本通道创建独立 `loopguard.Detector`，将解码后的增量传给 `Push`。非 nil 结果表示命中；由宿主实施真实取消、错误呈现与账务收尾。检测器本身不发网络请求，不执行工具，也不持有全局状态。

## 验证状态

```bash
python3 scripts/verify.py run --step race.loop-guard
```

自动测试使用真实本地 HTTP 上下游与合成模型流，覆盖四协议错误、上游 context 取消、重复请求冷却、不同上下文可继续、正常流与 usage 原样转发、SSE 分片/CRLF/大小限制、候选与通道隔离，以及代码/数字日志/长推理等反例。

2026-10-15 对问题线程保存的 78 段去重文本离线回放，命中两段已确认复读：211,687 字符样本在第 54,996 字符命中；222,344 字符样本在第 15,498 字符命中。其余 76 段未命中。这是有限语料的离线文本回放，不是实际 SSE 时间线、准确 token/费用节省估算，也不是所有模型的误报率证明。生产原文未加入仓库或测试夹具。

2026-10-15 已在 Linux 隔离容器中用实际 Codex CLI 0.162.1 验证 exec / app-server 两条路径，使用默认重试设置、合成 Responses 上游、`network=none` 和临时 home，不挂载账号凭证。两条路径均检测后最终失败，单回合仅一次请求到达上游。Codex 仍会显示最多 5 次重连提示，但后续请求被网关冷却闸门以 422 拒绝；不是再次运行模型。不要在冷却拒绝中添加 `Retry-After`：该版本会等待冷却结束后重发上游。可复现入口：

```bash
python3 scripts/verify.py run --step docker.loop-guard \
  --binary /path/to/linux/abox-loop-guard --image <已准备的本地Agent镜像>
```

同日已按用户授权在生产安装独立 systemd 网关，并定向切换一个 DeepSeek 测试账号；Agentbox 主服务未重启。已核对鉴权、宿主机及容器到真实上游的模型列表连通性、账号池及会话配置同步，保留原配置和回退脚本。真实付费生成的复读效果由用户测试，不能把上述合成 CLI 验证当成实际模型复现。其他 harness 版本仍需各自验收。
