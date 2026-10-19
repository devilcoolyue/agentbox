# 扩展与 SPI 协议设计

日期：2026-10-16，2026-10-17 解耦（§19）。状态：**设计已评审；P1 已实现**（「扩展」菜单、循环保护、Webhook 通知）。真实 Claude Code / Codex 经网关与生产容器布局已于 2026-10-19 验收，见 §20。SPI 1.0 冻结前（见 §15 P3）字段和方法名可能调整，不承诺兼容。

**已定决策（2026-10-16）**

1. 检查模型流量的扩展只能绑定 API Key 和中转账号；OAuth 订阅账号（Claude 订阅、ChatGPT 登录）不改道。其他扩展不受此限。
2. 检查型扩展失效时默认放行（`fail_open`），管理员可以按扩展改成拦截。事件型扩展没有失效策略。
3. 对用户可见：空间里显示有哪些扩展作用于它（检查模型流量或接收本空间事件）。
4. 不做自动续跑：中断后由用户决定怎么继续，SPI 不提供发起消息的能力。
5. 扩展框架与模型网关分离（2026-10-17）：网关是系统级设施，只在有扩展用到模型钩子时启动；扩展按钩子声明依赖，不是所有扩展都建立在网关之上。

## 1. 目标与边界

**目标**

- 管理员在独立的「扩展」菜单里浏览、启用、配置、停用扩展，并能看到每个扩展的状态和最近的动作。
- 首批扩展由官方维护：循环保护（现 `abox-loop-guard`，检查模型流量）和 Webhook 通知（接收空间与对话事件）。以后开放市场，接收第三方提交。
- 官方扩展和第三方扩展共用一套 SPI。接入新扩展不需要改服务端代码。
- 扩展出错不拖垮主服务；默认最小权限；**凭证和网络出口永远不交给扩展**。

**不做的事**

- 内网隧道（abox-link 与服务端 tunnel/netaccess）保持内嵌，不纳入扩展。
- 不在服务端进程里运行第三方代码，不支持 Go `plugin`（.so）。服务端以 root 运行，持有 Docker socket、账号凭证和 Git 密钥库，进程内插件等于整机权限；`.so` 还要求完全相同的工具链与依赖版本，不能卸载，也配不上多平台发布。
- 扩展不能往网页注入自己的脚本或样式。界面由宿主按扩展的声明渲染，理由同自定义主题：控制台没有 CSP，令牌在 localStorage。
- 不替代 Agent 侧的扩展方式。Agent 里要加工具或技能，继续用 MCP、技能、home 模板和 Claude 插件市场。
- 不做自动续跑。扩展只能放行、拒绝或中断，不能替用户发消息、重试或切换模型。

**命名**：界面和代码里统一叫「扩展」（`extension`）。「插件」继续专指 Claude Code 插件（`market.go`、技能页），两者不混用。

## 2. 总体结构

```text
                     网页「扩展」菜单（管理员）          系统设置 → 模型网关
                              │ /api/extensions*              │ /api/settings
                              ▼                               ▼
空间服务 ─ workspace.lifecycle ─┐                  容器内 CLI ─▶ 模型网关 ─账号出口─▶ provider / 中转
对话服务 ─ chat.turn.ended ─────┤                               │ model.request / model.stream
                                ▼                               ▼
                             扩展宿主（事件投递、检查调用、权限、绑定）──▶ 扩展实例
                                               ├─ builtin：编进二进制，进程内直接调用
                                               └─ container：Docker 沙箱，stdio JSON-RPC
```

宿主不知道网关的存在：网关只是模型钩子的一个调用方，和空间服务、对话服务地位相同。

| 组件 | 位置 | 职责 |
|---|---|---|
| 扩展宿主 | `internal/extension`（官方扩展在 `internal/extension/builtin`） | 清单校验、注册表、实例生命周期、钩子分发、权限、故障策略、状态与日志 |
| 模型网关 | `pkg/modelgw`（公开，独立 `abox-loop-guard` 共用）+ `internal/server/modelgateway.go` | 转发、四协议 SSE 解码成统一增量、执行中断并按协议写错误事件；服务端部分负责监听、路径鉴权、上游与账号出口解析。配置在顶层 `model_gateway`，界面在系统设置 |
| 事件源 | `workspace.Service.SetLifecycleHook`、`chat.Runtime.TurnEnded` → `internal/server/extension_events.go` | 把空间启停和对话回合结果转成 `Host.Emit` |
| SPI SDK | `pkg/extspi`（公开） | 消息类型、NDJSON JSON-RPC 编解码、`Serve(handler)`、`Pipe` 一致性测试；官方和第三方 Go 扩展共用 |
| 扩展实例 | builtin / container | 只处理被授权的数据，只能返回裁决 |

关键取舍：**网络这一跳由宿主执行**（对模型钩子而言）。凭证、出口代理、协议解析和中断的写法都在宿主手里，扩展只看到授权给它的数据（例如模型输出的文字增量），只能回答「放行 / 拒绝 / 中断」。市场扩展因此看不到 API Key，也发不出网络请求。

## 3. 概念

| 概念 | 含义 |
|---|---|
| 扩展包 | 清单 `extension.json` 加运行时产物（builtin 无产物；container 为按 digest 固定的 OCI 镜像） |
| 实例 | 一个启用中的扩展在宿主里的运行副本。每个扩展最多一个实例，按需并发处理多条流 |
| 钩子 | 宿主在固定时机调用扩展的点。分两类：检查（`inspect`：`model.request`、`model.stream`，可以拒绝或中断）和事件（`event`：`workspace.lifecycle`、`chat.turn.ended`，只通知）。每个钩子有分组（`model` / `workspace` / `chat`）、所需权限、能用的绑定方式，以及依赖的系统设施（目前只有 `model_gateway`），都登记在 `extspi.Hooks()` |
| 权限 | 扩展能拿到哪些数据、能调用哪些宿主方法。清单声明，管理员启用时逐项批准 |
| 绑定 | 扩展作用于哪些空间：`none` / `all` / `selected`（按账号和 / 或用户挑选）。能用哪种挑选方式由钩子决定 |
| 故障策略 | 检查型钩子在实例不可用、超时、积压时放行（`fail_open`）还是拦截（`fail_closed`）；事件型钩子不适用 |

## 4. 清单 `extension.json`

以循环保护为例：

```json
{
  "id": "agentbox.loop-guard",
  "version": "1.0.0",
  "spi": "1.0",
  "publisher": "agentbox",
  "name": {"zh-CN": "循环保护", "zh-TW": "循環保護", "en": "Loop guard"},
  "description": {
    "zh-CN": "检测模型输出里的短句复读，命中后中断这次回复。",
    "zh-TW": "偵測模型輸出中的短句複讀，命中後中斷這次回覆。",
    "en": "Interrupts a model reply that keeps repeating short sentences."
  },
  "license": "Apache-2.0",
  "homepage": "https://github.com/devilcoolyue/agentbox/blob/main/docs/loop-guard.md",
  "runtime": {"type": "builtin"},
  "hooks": ["model.request", "model.stream"],
  "permissions": ["model.request.fingerprint", "model.stream.text"],
  "bindings": ["account"],
  "failure_policy": "fail_open",
  "config_schema": {
    "type": "object",
    "properties": {
      "window": {"type": "integer", "minimum": 32, "maximum": 1024, "default": 256,
        "title": {"zh-CN": "检测窗口（句）", "zh-TW": "偵測視窗（句）", "en": "Window (sentences)"}},
      "distinct_limit": {"type": "integer", "minimum": 2, "maximum": 256, "default": 24,
        "title": {"zh-CN": "不同句子上限", "zh-TW": "不同句子上限", "en": "Distinct sentence limit"}},
      "cooldown_seconds": {"type": "integer", "minimum": 0, "maximum": 600, "default": 30,
        "title": {"zh-CN": "相同请求冷却（秒）", "zh-TW": "相同請求冷卻（秒）", "en": "Identical request cooldown (s)"}}
    }
  },
  "errors": [
    {"code": "model_output_loop", "message": {
      "zh-CN": "模型在重复短句，这次回复已中断。",
      "zh-TW": "模型在重複短句，這次回覆已中斷。",
      "en": "The model kept repeating short sentences; this reply was interrupted."}}
  ],
  "status": {
    "metrics": [
      {"name": "trips", "title": {"zh-CN": "中断次数", "zh-TW": "中斷次數", "en": "Interruptions"}},
      {"name": "cooldown_rejects", "title": {"zh-CN": "冷却拒绝", "zh-TW": "冷卻拒絕", "en": "Cooldown rejections"}}
    ]
  }
}
```

| 字段 | 规则 |
|---|---|
| `id` | `^[a-z0-9]+(\.[a-z0-9-]+)+$`，最长 64。`agentbox.` 前缀保留给官方 |
| `version` | semver。同一扩展可装多个版本，同一时刻只启用一个 |
| `spi` | 声明需要的 SPI `major.minor`，宿主必须支持同 major 且 minor 不低于它 |
| `name` / `description` / `title` / `message` | 三语对象，缺的语言回退 `en`，再回退任意一种。纯文本，宿主按文本节点渲染 |
| `runtime` | `{"type":"builtin"}` 仅官方；`{"type":"container","image":"<仓库>@sha256:<digest>","resources":{...}}`，必须用 digest，不接受可变标签。`process` 保留不开放（见 §5.3） |
| `hooks` | 只能是 §6.4 里的名字；宿主不会调用未声明的钩子。扩展必须实现对应接口（检查型 `ModelHandler`，事件型 `EventHandler`），否则启动失败 |
| `permissions` | 见 §7。钩子所需权限没声明时，清单校验失败 |
| `bindings` | `account`、`user` 的子集，每一项必须被所有声明的钩子支持（模型钩子只支持 `account`） |
| `failure_policy` | 默认值；管理员可以覆盖。只对检查型钩子有意义 |
| `config_schema` | JSON Schema 子集：顶层 `object`；属性类型 `boolean` / `integer` / `number` / `string` / `enum`；支持 `minimum`、`maximum`、`maxLength`（≤ 1024）、`default`、`title`、`description`。属性最多 64 个。v1 不支持嵌套和密钥字段 |
| `errors` | 扩展能用来拒绝或中断的错误码及三语文案。`code` 为 `^[a-z][a-z0-9_]{2,47}$`，0～32 个，只做事件的扩展可以不声明 |
| `status.metrics` | 扩展能上报的数值指标及三语名称，最多 32 个。只能报这里声明过的指标 |

清单本身是不可信输入：宿主校验全部字段与长度，拒绝未知的顶层字段（以后通过 `spi` 次版本扩展）。

## 5. 运行时

### 5.1 builtin

官方扩展用 Go 实现 `extspi.Handler`，编进 `agentbox` 二进制，清单用 `go:embed` 嵌入。宿主直接调用 Handler，不经过序列化。为了保证 builtin 没有偷用进程内的便利，`pkg/extspi` 的一致性测试会让每个 builtin 扩展再走一遍真实的 JSON-RPC 编解码，结果必须一致。

### 5.2 container

市场扩展只能用这种运行时，官方扩展也可以用。宿主为每个启用的扩展起一个容器：

- 镜像按 digest 引用，只在安装时拉取，启动时不拉。
- `network=none`、只读根文件系统、`CapDrop: ALL`、`no-new-privileges`、非 root 用户。
- 默认资源 128 MiB 内存、0.5 CPU、64 个进程；`/tmp` 是 16 MiB tmpfs。清单可以申请更多，上限由管理员批准。
- 不挂载用户目录、会话 home、Docker socket；不传宿主环境变量，只传 `ABOX_EXT_ID`、`ABOX_EXT_VERSION`。
- stdin/stdout 走 Docker attach 承载 SPI 消息；stderr 作为日志，按 §6.5 的限额读取。
- 容器带标签 `agentbox.extension=<id>`；停用、升级、服务关闭时删除。服务启动时按标签清理上次遗留的容器。

隔离方式沿用候选镜像验证（`dockerx.ValidateCLIImage`）的做法。

### 5.3 process（保留）

在宿主上直接起子进程需要处理降权（服务端是 root）、文件描述符和环境变量继承，v1 不开放。官方扩展需要独立二进制时，先用 container。

## 6. SPI 协议

### 6.1 传输与编码

- JSON-RPC 2.0，换行分隔的 JSON（NDJSON）：宿主写 stdin，扩展写 stdout，每行一条完整消息。与 MCP stdio、Codex app-server 相同。
- UTF-8；单条消息最大 1 MiB，超限视为协议错误并重启实例。
- 请求带 `id`，通知不带。双方都可以发请求；`id` 由发送方保证在自己方向上唯一。
- builtin 运行时使用同一套 Go 类型，语义与线上格式一致。

### 6.2 版本

- `spi` 版本是 `major.minor`。同一 major 内只做增量：新增方法、新增可选字段、新增钩子与权限。
- 双方忽略不认识的字段；收到不认识的方法时，请求回 `-32601`，通知直接丢弃。
- 宿主不会发送扩展没有在 `initialize` 结果里订阅、或者没有被授予权限的钩子。
- major 变化时宿主拒绝启动旧扩展，并在扩展页说明原因。

### 6.3 生命周期

```text
宿主                                         扩展
 │──── initialize (5 s 期限) ───────────────▶│
 │◀─── result: spi_version, hooks ───────────│
 │──── initialized（通知）──────────────────▶│
 │         ……钩子调用、ping、状态、日志……      │
 │──── shutdown (2 s 期限) ─────────────────▶│
 │◀─── result ───────────────────────────────│
 │──── exit（通知）─────────────────────────▶│   之后宿主关闭 stdin；超时则强制结束
```

`initialize` 请求：

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "spi_versions":["1.0"],
  "host":{"name":"agentbox","version":"0.2.0"},
  "extension":{"id":"agentbox.loop-guard","version":"1.0.0"},
  "config":{"window":256,"distinct_limit":24,"cooldown_seconds":30},
  "permissions":["model.request.fingerprint","model.stream.text"],
  "limits":{"request_timeout_ms":200,"max_message_bytes":1048576}
}}
```

- `config` 已按 `config_schema` 校验并补齐默认值。
- `permissions` 是管理员实际批准的集合，可能少于清单声明。

`initialize` 结果：

```json
{"jsonrpc":"2.0","id":1,"result":{"spi_version":"1.0","hooks":["model.request","model.stream"]}}
```

`hooks` 必须是清单声明且已获授权的子集；超出范围时宿主终止实例并标记为失败。

其他约定：

- **配置变更**：宿主按新配置重启实例，不提供热更新。实例内存里的状态（如冷却表）随之丢失。
- **健康检查**：宿主每 30 秒发一次 `ping`（期限 5 秒），连续 3 次失败就重启。
- **崩溃重启**：退避 1 s、2 s、4 s……最长 60 s。10 分钟内崩溃 5 次后标记为 `failed`，不再自动拉起，管理员处理后手动重新启用。
- **服务关闭**：随服务端生命周期（`spawn` / `track`）关闭，与聊天的 2 秒收尾窗口一致。

### 6.4 钩子（v1）

| 钩子 | 消息 | 是否阻塞 | 所需权限 | 扩展可以做什么 |
|---|---|---|---|---|
| `model.request` | 请求 `model/request` | 阻塞，默认 200 ms 期限 | `model.request.fingerprint`（可选 `model.context`） | 放行或拒绝这次请求 |
| `model.stream` | 通知 `model/stream/open`、`model/stream/delta`、`model/stream/close` | 不阻塞 | `model.stream.text`（可选 `model.context`） | 随时发 `model/stream/abort` 中断 |

**流标识**：宿主给每个进入网关、协议已知的 POST 请求分配不透明的 `stream` ID。同一 `stream` 先收到 `model/request`（订阅了该钩子时），放行后再收到 `open` 和若干 `delta`，最后**恰好一条** `close`。被拒绝的请求没有 `open`，但仍然会收到 `close`（`reason: "rejected"`），方便扩展清理按流保存的状态。实例重启后旧流不再有任何消息。

#### `model/request`

```json
{"jsonrpc":"2.0","id":7,"method":"model/request","params":{
  "stream":"s_9f2c1e",
  "protocol":"responses",
  "method":"POST",
  "path":"/v1/responses",
  "agent":"codex",
  "fingerprint":"5b1f…e04a",
  "context":{"session":"…","account":"…","user":"…","source":"chat"}
}}
```

- `protocol`：`responses` / `chat` / `anthropic` / `gemini`，由路径判断，规则同现在的 `protocolFor`。
- `path` 不含查询串（Gemini 可能把 key 放在 `?key=` 里）。
- `fingerprint`：沿用 loop-guard 的算法（请求 URI、三个认证头、按键序归一化并去掉顶层投递字段 `stream` / `max_tokens` 等的 JSON 正文，见 `docs/loop-guard.md`），再以**每个扩展实例独立的随机盐**做 HMAC-SHA256，输出十六进制。加盐后，不同扩展之间无法关联同一请求，也无法拿已知提示词做字典比对。宿主重启后盐会变，这和冷却表本来就不跨重启一致。
- `context` 只有授予 `model.context` 时才出现。`source` 为 `chat` / `terminal` / `unknown`（见 §8.4）。
- v1 不提供请求正文。

结果二选一：

```json
{"jsonrpc":"2.0","id":7,"result":{"action":"allow"}}
{"jsonrpc":"2.0","id":7,"result":{"action":"reject","code":"model_output_loop","message":"相同请求刚刚触发过循环，请修改上下文后再试。"}}
```

多个扩展订阅同一钩子时按管理员设定的顺序调用，第一个 `reject` 生效。拒绝时宿主不连上游，直接回 HTTP 422，正文为 `{"error":{"type":code,"code":code,"message":message}}`。**不带 `Retry-After`**：Codex 0.162.1 会等冷却结束后自动重发同一请求（见 `docs/loop-guard.md`）。

#### `model/stream/open` / `delta` / `close`

```json
{"jsonrpc":"2.0","method":"model/stream/open","params":{"stream":"s_9f2c1e","protocol":"responses"}}

{"jsonrpc":"2.0","method":"model/stream/delta","params":{"stream":"s_9f2c1e","seq":12,
  "deltas":[
    {"part":"0:msg_1:0:","channel":"text","text":"我再检查一下。"},
    {"part":"0:rs_1::0:summary","channel":"reasoning","text":"Check again."}
  ],
  "ended_parts":["1"]}}

{"jsonrpc":"2.0","method":"model/stream/close","params":{"stream":"s_9f2c1e","reason":"completed"}}
```

- 宿主把四种协议解码成统一的文字增量。`part` 是不透明的分区键（候选、输出项、内容块、摘要序号的组合），`channel` 为 `text` 或 `reasoning`。扩展必须按 (`part`, `channel`) 分别处理。`ended_parts` 表示这些分区已结束（例如 Anthropic 的 `content_block_stop`），扩展可以释放对应状态。
- 只包含增量，不重复发送最终的完整快照。工具调用参数、工具结果和 usage 不发送（`model.stream.tools` 保留）。
- `seq` 在每条流内从 1 递增，用来发现丢失。增量先进入实例队列，宿主轮到这条流时把积压的增量合并成一条通知，没有积压就逐事件发送。
- **转发不等扩展**：宿主把上游事件立刻转给客户端，同时把增量异步送给扩展。中断可能晚到几个事件，换来的是对回复速度没有影响。
- `close.reason`：`completed` / `upstream_error` / `client_closed` / `aborted` / `rejected` / `not_inspected` / `overflow` / `timeout`。中断时另带 `aborted_by`（扩展 ID）与 `code`。

#### `model/stream/abort`（扩展 → 宿主）

```json
{"jsonrpc":"2.0","id":3,"method":"model/stream/abort","params":{
  "stream":"s_9f2c1e","code":"model_output_loop",
  "message":"Last 256 short sentences contain only 12 distinct phrases.",
  "detail":{"window":256,"distinct":12}}}
```

```json
{"jsonrpc":"2.0","id":3,"result":{"applied":true}}
{"jsonrpc":"2.0","id":3,"result":{"applied":false,"reason":"closed"}}
```

宿主对中断请求的处理：

1. 校验：`code` 必须在清单 `errors` 里；`message` 为纯文本，最长 500 字符；`detail` 只能是数值，最多 16 个键。
2. 取消上游请求的 context 并关闭响应体，与现在 loop-guard 的做法一样。
3. 按协议写终止错误（现 `streamGuard.failure` 的逻辑迁到宿主）。扩展不能自己拼 SSE，也就不能伪造成功完成。
4. 记录一条扩展事件（§10），再向所有订阅了这条流的扩展发 `close`（`reason: "aborted"`）。

多个扩展同时请求中断时，以第一个为准，其余返回 `applied:false, reason:"superseded"`。流已经结束时返回 `reason:"closed"`。

#### 事件钩子：`event`

| 钩子 | 时机 | 所需权限 | 绑定 |
|---|---|---|---|
| `workspace.lifecycle` | 空间创建、启动（只算冷启动）、停止（停止前须在运行；空闲回收带 `reason: "idle"`）、删除。隐藏的委派跑腿空间不报 | `workspace.events` | `account`、`user` |
| `chat.turn.ended` | 网页对话的每个回合结束，用量入账之后。委派跑腿回合不报 | `chat.turns` | `account`、`user` |

两种都用同一个通知方法 `event`，不阻塞、不回结果：

```json
{"jsonrpc":"2.0","method":"event","params":{"hook":"workspace.lifecycle","id":"evt_…","time":"2026-10-17T15:00:00Z",
  "workspace":{"action":"stopped","reason":"idle","workspace":{"id":"…","name":"…","agent":"claude","account":"relay-a","user":"alice"}}}}

{"jsonrpc":"2.0","method":"event","params":{"hook":"chat.turn.ended","id":"evt_…","time":"…",
  "turn":{"workspace":{…},"thread":"…","turn":"…","status":"failed","code":"turn_failed","model":"…",
          "duration_ms":81234,"cost_micro_usd":12345,"cost_source":"…"}}}
```

- `action`：`created` / `started` / `stopped` / `deleted`。`status`：`completed` / `failed` / `interrupted`。
- 只发送标识、名称和数值，**不发送对话内容**。
- 宿主按订阅、批准的权限和绑定（空间的账号、属主）过滤后放进实例队列（每实例 1024 条）。队列满了丢弃并写扩展日志，不阻塞空间或对话。实例未运行时事件直接丢弃，不补发。
- `usage.recorded`、`extension.aborted` 仍保留名字，以后按次版本加入。

### 6.5 扩展 → 宿主的其他消息

| 方法 | 类型 | 说明 |
|---|---|---|
| `status/report` | 通知 | `{"state":"ok"|"degraded","metrics":{"trips":3}}`。只接受清单 `status.metrics` 里声明过的数值指标，显示在扩展页 |
| `log` | 通知 | `{"level":"info"|"warn"|"error","message":"…"}`。单条最长 1 KiB，每秒最多 20 条，超出丢弃并计数；只保留在内存环形缓冲里，只给管理员看。container 的 stderr 也进入同一缓冲，每分钟最多 64 KiB |

v1 不提供文件、网络、聊天等宿主能力。以后计划通过权限开放 `storage`（扩展私有目录或 KV，带配额）。按已定决策，不提供发起聊天消息的能力。

### 6.6 错误码

| 代码 | 含义 |
|---|---|
| -32700 / -32600 / -32601 / -32602 / -32603 | JSON-RPC 标准错误 |
| -32010 | `permission_denied`：调用的方法需要未授予的权限 |
| -32011 | `unknown_stream`：流不存在，或不属于这个实例 |
| -32012 | `invalid_verdict`：错误码未声明、文案过长或 `detail` 不合规 |
| -32013 | `rate_limited`：超出调用频率 |

扩展对 `model/request` 返回错误或超时时，按故障策略处理（§9）。

### 6.7 时序示例：循环保护命中

```text
宿主 → model/request {stream:s1, fingerprint:F}           扩展：F 不在冷却表 → allow
宿主 → model/stream/open {stream:s1}
宿主 → model/stream/delta {stream:s1, seq:1..40}           扩展：逐句喂给 Detector
扩展 → model/stream/abort {stream:s1, code:model_output_loop, detail:{window:256,distinct:12}}
                                                            扩展：把 F 记进冷却表（30 s）
宿主：取消上游 → 写协议错误 → 记扩展事件 → {applied:true}
宿主 → model/stream/close {stream:s1, reason:aborted, aborted_by:agentbox.loop-guard}
—— CLI 自动重发同一请求 ——
宿主 → model/request {stream:s2, fingerprint:F}           扩展：F 在冷却中 → reject
宿主：HTTP 422（无 Retry-After），不连上游
宿主 → model/stream/close {stream:s2, reason:rejected}
```

## 7. 权限

| 权限 | 扩展能拿到什么 | 风险说明（启用时展示给管理员） | v1 |
|---|---|---|---|
| `model.stream.text` | 模型回复与推理的文字增量 | 能读到模型输出，可能包含代码或密钥片段 | 有 |
| `model.request.fingerprint` | 加盐的请求指纹 | 只能判断两次请求是否相同，拿不到原文 | 有 |
| `model.context` | 空间 ID、账号 ID、用户名、来源、回合 | 能把流量关联到具体用户 | 有 |
| `workspace.events` | 空间何时创建、启动、停止、删除，以及空间名称、账号和属主 | 能知道谁在什么时候用了哪个空间 | 有 |
| `chat.turns` | 每个回合的状态、错误码、模型、耗时和费用，以及所属空间和用户 | 能统计每个人的使用量；拿不到对话内容 | 有 |
| `model.request.body` | 请求正文（提示词） | 能读到完整对话上下文 | 保留 |
| `model.stream.tools` | 工具调用参数 | 能读到 Agent 要执行的命令和文件内容 | 保留 |
| `storage` | 私有持久存储 | 有配额 | 保留 |

- 管理员启用时逐项批准，可以只批准一部分（至少一个钩子的权限）；缺少钩子必需的权限时，宿主不调用该钩子。
- 升级时如果新版本要了更多权限，在管理员批准之前继续运行旧版本。
- 官方内置的 Webhook 扩展是唯一允许出站的扩展（它的用途就是推送）。container 扩展没有网络，能把数据带出去的途径只有拒绝和中断的文案、数值指标和日志，都有长度限制，而且只给管理员看。

## 8. 模型网关（模型钩子依赖的系统设施）

### 8.1 哪些账号能绑定

只支持 API Key 和中转账号（已定决策）：

- Claude：账号 env 里有 `ANTHROPIC_AUTH_TOKEN` 或 `ANTHROPIC_API_KEY`（即 `claudeRelay` 返回令牌）。
- Codex：账号池 `auth.json` 是 API Key 登录（`CodexAuthMode` 为 `apikey`），官方地址和中转都算：内置 `openai` provider 的上游是账号 env 的 `OPENAI_BASE_URL`（缺省为官方地址），自定义 provider 的上游是它的 `base_url`（`credentials.CodexUpstream`）。所选 provider 没有配置时不能绑定。

两种都在 P1 支持（Codex 在首版合并后补齐，见 §17）。

OAuth 订阅账号（Claude 订阅、ChatGPT 登录）不能绑定。账号从中转切回订阅登录后，宿主不再改写它的 base URL，网关对它的请求也不调用钩子。

扩展配置里的绑定写法：`{"mode":"none"|"all"|"selected","accounts":["relay-a"],"users":["alice"]}`。`selected` 时空间的账号在 `accounts` 里、或属主在 `users` 里即命中；`all` 表示全部，包括以后新建的（对模型钩子仍只覆盖符合条件的账号）。旧写法 `"mode":"accounts"` 加载时转成 `selected`。删除账号或用户时，在同一次 `Config.mutate` 里把它从所有扩展的绑定中移除，避免留下悬空引用。

### 8.2 网关地址与鉴权

- 监听地址配置在顶层 `model_gateway.bind` / `host`（2026-10-17 前在 `extensions.gateway`，加载时自动迁移），形式同 `proxy_bridge`，默认 `172.17.0.1:1083`。只有启用了带模型钩子（且批准了对应权限）的扩展时才监听，生命周期挂在服务端上。系统设置显示它是否在监听、是否有扩展需要它；扩展详情页的「依赖」一行链接到这里。
- 改写后的 base URL：`http://<host>:<port>/x/<session_id>/<mac>`，其中 `mac = base64url(HMAC-SHA256(auth_token, "agentbox-extension-gateway\x00" + session_id))` 的前 32 个字符。轮换 `auth_token` 后旧地址全部失效，与 `proxySecret` 相同。
- 每个请求都重新检查：MAC 正确、空间存在、`sessionAccount` 仍然允许属主使用该账号。撤销授权后返回 403。
- 上游**在每次请求时**根据空间当前绑定的账号解析：Claude 用账号 env 里的 `ANTHROPIC_BASE_URL`（缺省为官方地址），Codex 用账号池 `config.toml` 里所选 provider 的 `base_url`。修改账号后立即生效，不用重建容器。
- 认证头（`Authorization`、`x-api-key` 等）由容器里的 CLI 带上，宿主原样转给上游，**从不交给扩展**。
- 上游连接走账号的出口（与 `acctClient` / `dialThrough` 相同，fail-closed）：绑定的代理不可用时直接返回 502，不退回直连。
- `model_gateway.dedicated_connections` 开启后，每次上游请求独占一条 HTTP/1.1 连接、不复用（同独立网关的 `-dedicated-connections`，`loopguard.DedicatedTransport`）：中断时关掉这次请求的 TCP/TLS 连接，而不只是在共享的 HTTP/2 连接上重置一条流。只管得到网关到上游这一跳，中转站到供应商的连接不受控制。默认关闭，管理员在「系统设置 → IP 代理 → 模型网关」切换（随设置一起保存）；新请求立即生效，已经在跑的流沿用原连接。
- 其余限制沿用 loop-guard：正文最大 32 MiB、单个 SSE 事件最大 1 MiB、单请求最长 30 分钟、强制 `Accept-Encoding: identity`、不跟随重定向、不支持 WebSocket / CONNECT。响应头 `X-Agentbox-Extensions` 标记 `monitoring` 或 `not-inspected`。

### 8.3 base URL 改写的位置

全部改写经过同一个函数（计划名 `effectiveModelBase(sess, acct)`），不能散落在各处：

- **Claude**：在 `execEnv` 里替换 `ANTHROPIC_BASE_URL`。网关地址必须在 `NO_PROXY` / `no_proxy` 里，否则绑定了出口代理的容器会把发往网关的请求送进代理桥。默认网关和代理桥同一个 host，已经在 `proxyEnvList` 的 `NO_PROXY` 里；配成不同 host 时要一起追加。
- **Codex**：base URL 写在会话 home 的 `.codex/config.toml` 里，不在 env 里，所以改写的是这份文件（`credentials.Service.codexSessionConfig`）：先按原规则把账号池的连接设置套到会话配置上（`codexConnectionConfig`），再把当前 provider 的 `base_url` 换成网关地址。账号用的是内置 `openai` provider 时（它在配置里不能指定 `base_url`），换成同等的自定义 provider `agentbox`（`wire_api = "responses"`、`requires_openai_auth = true`），并同步改当前 profile 的 `model_provider`。写入的地方：
  - 空间启动：`workspace` 的连接钩子在 `SeedCredentials`、`SeedDefaultModel` 之后执行（种子写进来的是账号池原样，含真实上游），容器已在运行时每次准入（每轮对话、每次打开终端）也执行；
  - 账号改 Key、改地址或换登录方式时的播发（`broadcast`）；
  - 绑定、停用、网关地址变化时，`applyExtensions` 在后台把所有已播种的 Codex 空间对齐一遍（`reconcileCodexEndpoints`）。

  文件内容没变就不写。credsync 的 45 秒同步只同步 `auth.json`，不碰 `config.toml`，所以不会把网关地址收回账号池。**账号池里的 `config.toml` 永远不能写入网关地址**：账号池被多个空间共用，而网关地址是按空间生成的；网关在每次请求时从账号池读真实上游。改写失败时空间启动报错，不让 CLI 绕过网关。切换账号时 `clearCodexConnection` 会删掉 `agentbox` provider，新账号重新播种。
- **NO_PROXY**：两种 Agent 都在 `execEnv` 末尾把网关主机补进 `NO_PROXY` / `no_proxy`，Codex（reqwest）同样认这两个变量。
- **终端**：`tmuxEnvSync` 会把 env 同步到 tmux 全局环境，但已经打开的 shell 仍保留旧值，Claude 要新开 tmux 窗口才生效（与隧道变量的限制一样）。Codex 每次启动都重新读 `config.toml`，在原窗口重新运行即可。
- **跑腿和起标题**：都经过 `execEnv` 或会话 home，规则自动覆盖。

解绑或停用扩展后，新的 exec 用回原地址；还在运行的进程仍然带着网关地址，网关继续接受这些请求，只是不再调用钩子，直接转发。网关本身没有运行时，CLI 会连接失败并明确报错，不会悄悄直连。

### 8.4 归属

v1 的网关地址按空间生成，所以能确定是哪个空间，但不能确定是哪个回合：

- 网页对话的回合和终端可能在同一个容器里同时运行，宿主分不出一个请求来自哪边，所以 P1 的 `source` 一律填 `unknown`，**不猜**。
- 以后可以给 Claude 网页回合的每次 exec 生成带回合号的地址（`/x/<session>/t/<turn>/<mac>`，MAC 覆盖回合号），做到按回合归属。Codex 的地址在 `config.toml` 里，只能按空间归属。

## 9. 故障策略

| 情况 | `fail_open` | `fail_closed` |
|---|---|---|
| 实例未运行、启动中或已 `failed` | 放行，标记 `not-inspected`，扩展页显示「降级」 | 在连上游之前返回 503 `extension_unavailable` |
| `model/request` 超时或返回错误 | 放行 | 拒绝 |
| 增量积压超限（单实例 4 MiB） | 这条流停止检查，发 `close`（`overflow`） | 中断这条流，错误码 `extension_overloaded` |
| 实例在流进行中崩溃 | 流继续，不再检查 | 中断这条流 |

默认值来自清单，管理员可以覆盖。按已定决策，循环保护默认 `fail_open`：保护功能失效不应该挡住所有工作，但必须让人看得见（扩展页状态、响应头、日志）。

## 10. 中断之后：聊天与计费

- **聊天**：中断后 CLI 收到协议错误并退出，按现有规则记为 `failed` / `turn_failed`。P2 新增 SQLite 表 `extension_events`（扩展 ID、空间、账号、回合（如有）、流、错误码、数值 `detail`、时间，不存任何正文）。回合失败时如果同一空间在回合时间窗内有扩展中断记录，回执错误细节带上扩展 ID 和错误码，网页用清单里的三语文案显示「已被循环保护中断」，不再只显示「进程退出码」。
- **计费**：沿用 loop-guard 的原则。中断时 provider 可能还没发最终用量，网关不编造 token 数；网页对话仍按现有的 transcript 和 `cost-state` 对账。provider 是否停止计费取决于它自己的取消处理。
- **不自动重试，也不自动续跑**（已定决策）。冷却拒绝的语义见 §6.4。中断后由用户看过已完成的工具操作，再决定怎么继续。
- **P1 的中断记录**：先保存在内存里（每个扩展最近 200 条，扩展页可见，重启后清空），P2 再持久化到 `extension_events`。

## 11. 配置与存储

- `config.json` 新增 `extensions` 段，必须同时进入 Load、validate、mutate、persist 和设置 API（与其他配置字段的约定一致）：

  ```json
  "extensions": {
    "items": {
      "agentbox.loop-guard": {
        "enabled": true,
        "version": "1.0.0",
        "order": 10,
        "config": {"window": 256, "distinct_limit": 24, "cooldown_seconds": 30},
        "permissions": ["model.request.fingerprint", "model.stream.text"],
        "bindings": {"mode": "selected", "accounts": ["relay-a"], "users": []},
        "failure_policy": "fail_open",
        "revision": 3
      }
    }
  }
  ```

  所有写入都带 `expected_revision`，防止并发覆盖。模型网关在独立的顶层 `model_gateway` 段：`bind` / `host` 只在 `config.json` 里改（改了会换掉所有 CLI 持有的地址），`dedicated_connections` 经设置接口修改。
- 安装的扩展包放在 `data/extensions/<id>/<version>/`（只保存清单和镜像 digest）。builtin 的清单嵌在二进制里，不落盘。
- `extension_events` 表通过下一个 schema 版本迁移（当前为 19），遵守 `migrations.go` 的连续迁移规则。
- 系统备份包含 `extensions` 配置与数据库。以后的 `storage` 目录进入系统备份。

## 12. 管理 API 与界面

| 路由 | 鉴权 | 说明 |
|---|---|---|
| `GET /api/extensions` | admin | 已安装扩展、版本、状态、绑定数量 |
| `GET /api/extensions/{id}` | admin | 清单、配置、批准的权限、状态指标 |
| `PUT /api/extensions/{id}` | admin | 启停、配置、权限、绑定、故障策略、顺序；带 `expected_revision` |
| `GET /api/extensions/{id}/logs?since=` | admin | 日志环形缓冲 |
| `GET /api/extensions/{id}/events` | admin | 中断与拒绝记录。P1 返回内存里最近 200 条，P2 改为数据库分页 |
| `GET /api/sessions`、`GET /api/sessions/{id}`（新增字段） | auth + withSession | `extensions: [{id, name, groups}]`：哪些扩展作用于本空间，`groups` 为 `model` / `workspace` / `chat`（已定决策：对用户可见） |

- `/me` 增加能力位 `extensions: 1`。
- 侧栏新增管理员入口「扩展」，与「使用记录」「Git 管理」并列，路由 `#/extensions` 和 `#/extensions/<id>`。
- 列表页用卡片展示名称、说明、钩子分组标签（模型流量 / 空间事件 / 对话事件）、发布者、版本、状态和作用范围。详情页里检查型扩展才显示失效策略、检查中的流和中断记录；依赖网关的扩展显示网关状态。详情页分为：概览与权限、设置（按 `config_schema` 渲染表单，下拉沿用 `select.ts`）、绑定（账号选择）、状态指标、日志、事件。
- 三语文案进词典；扩展提供的文字一律按文本节点插入。
- 用户可见：工作空间头部的信息行（Agent · 账号 · 状态 · 编号）末尾显示拼图图标和扩展名，悬停按扩展说明「正在检查本空间的模型输出」或「会收到本空间的空间启停与对话结果通知」。该行在桌面和窄屏都可见；顶栏只在窄屏显示，不放在那里。
- 以后的「市场」作为同一菜单下的一个标签页。

## 13. 循环保护迁成首个扩展

P1 已按下表完成：

| 原代码 | 现在的位置 |
|---|---|
| `pkg/loopguard/detector.go` | 仍在 `pkg/loopguard`；窗口与不同句数可配置（`NewDetector`），零值保持原默认 256 / 24 |
| `protocol.go` 的解码 | `pkg/modelgw.Decoder`，产出 §6.4 的统一增量；按分区、通道持有检测器的部分是 `loopguard.Monitor` |
| `protocol.go` 的 `failure` | `pkg/modelgw.Decoder.Failure`，由网关统一写中断错误 |
| `gateway.go` 的转发、限额、指纹 | `pkg/modelgw.Proxy` 与 `Fingerprint`；宿主再按实例加盐（§6.4） |
| `gateway.go` 的冷却表 | `loopguard.Cooldown`，独立网关与扩展各用一份 |
| `cmd/abox-loop-guard` | 未改；`loopguard.Gateway` 改为 `modelgw.Proxy` + 同步检测，原有 HTTP 行为测试全部保留并通过 |

接入 agentbox 后，生产上不再需要单独的 `abox-loop-guard.service`，也不再手工改账号的 base URL。迁移步骤见 [`docs/loop-guard.md`](../loop-guard.md) 的「从独立网关迁到扩展」：先启用扩展并绑定账号（此时请求依次经过扩展网关和独立网关，保护不中断），再把账号地址改回原供应商，确认后停用独立服务并保留其配置用于回退。

## 14. 市场（以后）

- **包格式**：按 digest 引用的 OCI 镜像，镜像里带 `/extension.json`，同时作为镜像标签 `agentbox.extension.manifest` 便于不运行镜像就能读取。
- **目录**：一个 git 仓库作为索引，做法参照 `market.go`（浅克隆、按不可信输入校验来源）。
- **安装**：管理员查看清单和权限 → 按 digest 拉取 → 校验清单、SPI 版本、`runtime=container`、不能占用 `agentbox.` 前缀 → 在沙箱里跑一致性探测（`initialize`、钩子订阅、`ping`、`shutdown`），做法参照 `agentprobe` → 以「已安装、未启用」状态入库。
- **升级与回退**：新版本并排安装，权限有变化时需要重新批准，保留上一个版本用于回退。
- **签名与审核**：发布者签名（sigstore / cosign 等）和官方审核流程待定。

## 15. 分阶段

| 阶段 | 内容 | 验证 |
|---|---|---|
| P0 | 本文评审 | — |
| P1（已实现） | SPI 1.0-draft 类型与 `pkg/extspi`；宿主核心（仅 builtin）；`pkg/modelgw`；循环保护扩展；管理 API 与「扩展」菜单；Claude 与 Codex 的 API Key / 中转账号；网关独占连接 | 已完成：Go 单元与竞态测试、合成 API 的浏览器回归。真实 Claude / Codex CLI 经网关打合成上游（`cli.model-gateway`）与生产布局可达性见 §20 |
| P2 | `extension_events` 表与回执里的中断说明；按回合归属（若 §16 验证通过） | 同上 |
| P3 | container 运行时；一致性测试工具；**冻结 SPI 1.0** | 沙箱隔离实测（network=none、只读根、资源限额、标签清理） |
| P4 | 市场；`storage` | 另行设计 |

新增的测试入口要在 `scripts/verification_catalog.py` 登记。

## 16. 待验证与待决问题

- ~~**带路径前缀的 base URL**~~：2026-10-19 已用真实 Claude Code 2.1.302 与 Codex 0.163.0 验证，见 §20。
- ~~**透明网络**~~：2026-10-19 在生产「默认网桥 + 透明网络辅助容器」布局的空间里实测可达，见 §20。
- ~~**Codex 的 WebSocket 传输**~~：Codex 0.163.0 对自定义 provider 只发 HTTP POST `/responses`（§20 的 exec 与 app-server 两条路径均如此）；升级 Codex 后用 `cli.model-gateway` 复查。
- **非流式请求不检查**：网关只解析 SSE；客户端一开始就发非流式请求时，上游复读会一直生成到 token 上限。被中断后的非流式兜底由冷却挡住（§20），首发即非流式的情况目前没有保护。
- **用户级开关**：是否允许普通用户为自己的空间选择不检查（v1 不提供，由管理员按账号决定）。

## 17. P1 实施记录（2026-10-16）

**已实现**

- `pkg/modelgw`：独立网关与服务端共用的转发核心。路由函数负责鉴权与上游解析，Inspector 负责准入和观察；同步裁决（独立网关）与异步中断（扩展宿主）走同一套取消与错误写法。不检查的 SSE 也逐事件刷新。
- `pkg/extspi`：§6 的消息类型、`Handler` / `Host` 接口、NDJSON JSON-RPC 传输（应答由读协程分发、请求与通知由独立协程按序处理，扩展在通知处理里调用 `abort` 不会死锁；积压上限 1024 条、单条 1 MiB）、`Pipe`。
- `internal/extension`：清单与配置 schema 校验、builtin 注册、实例生命周期（崩溃按 1～60 秒退避重启，10 分钟内 5 次即 `failed`）、请求钩子 200 ms 期限、按实例 4 MiB 的增量队列（轮到时合并积压）、两种失效策略、中断校验与记录（内存最近 200 条）、日志限速。
- `internal/extension/builtin`：`agentbox.loop-guard`（清单内嵌）。
- 服务端：`extensions` 配置段（随 mutate/persist/validate，删除账号同一次写入解绑）、网关监听（默认 `172.17.0.1:1083`，与 1080～1082 的隧道、代理桥、透明网络控制端口错开，配置校验拒绝冲突）、`/x/<空间>/<MAC>/…` 鉴权、每次请求重查账号授权、按账号出口代理转发（不可用即 502，不回落直连）、`execEnv` 改写 `ANTHROPIC_BASE_URL` 并补 `NO_PROXY`、tmux 环境同步在解绑后清除注入的地址、管理 API、`/me` 能力位、空间视图 `extensions` 字段。
- 网页：侧栏「扩展」（仅管理员）、`#/extensions` 与 `#/extensions/<id>`、权限逐项批准、账号绑定（订阅账号不可选）、按 schema 渲染参数、带修订号保存与冲突重载、事件与日志、空间头部的扩展标记；三语词典。

**已验证**

- `go test ./...`：新增包与改动包全部通过（`-race` 多次重复无抖动）。`race.loop-guard`（现含 `pkg/modelgw`）、新登记的 `race.extensions` 通过。唯一失败的 `internal/filesearch TestBuildLimitsKeepTheTop` 在未改动的基线提交上同样失败，与本次无关。
- 扩展宿主测试让循环保护分别以进程内调用和真实 NDJSON 管道两种方式跑过真实网关：复读被中断、上游被取消、相同请求 422 冷却且不到上游、事件与指标正确。
- 服务端测试覆盖管理 API、修订冲突、启用前的权限要求、base URL 改写、订阅账号不改道、网关不可用时放行策略不改写、MAC 错误 403、未知空间 404、撤权 403、删除账号解绑。
- 浏览器回归 `scripts/test-extensions.mjs`（并入 `browser.browser`）：合成 API 下的列表、详情、保存、冲突、纯文本渲染（含 HTML 片段的说明不被解析）、事件与日志、空间头部标记、普通用户看不到入口；完整浏览器回归通过。

**尚未验证（启用到生产前必须补）**——2026-10-19 已补，见 §20；下列原文保留。

- 真实 Claude Code 能否使用带路径前缀的 base URL（`http://172.17.0.1:1083/x/<空间>/<MAC>`）正确拼接 `/v1/messages` 等路径；本环境没有 Docker，没有跑真实 CLI。若不能，改为在请求头里传令牌（§16）。
- 容器到网关端口的可达性（默认网桥、透明网络两种布局）。
- 生产上建议先只绑定一个测试中转账号，确认对话与终端都能正常请求、复读能被中断后，再扩大范围；回退只需在扩展页停用或解绑（新开的 CLI 执行即恢复原地址）。

**与设计的差异**

- 网关核心放在公开的 `pkg/modelgw`（设计稿写的是 `internal/modelgw`），以便独立 `abox-loop-guard` 与第三方网关复用。
- 增量批量方式改为「按积压合并」，没有固定 50 ms 定时器，`limits` 里也去掉了 `delta_batch_ms`。
- `source` 在 P1 一律为 `unknown`，不推断网页回合与终端。

## 18. Codex 与独占连接补充记录（2026-10-16）

首版合并后发现，生产上独立网关保护的正是一个 Codex 中转账号（DeepSeek 渠道，开启了独占连接）。已定决策「只支持 API Key 和中转账号」本来就包括 Codex，首版只做了 Claude，不能替代生产部署，因此补齐：

- `credentials`：`CodexUpstream`（从账号池读真实上游、判断能否绑定）、`codexGatewayConfig`（改写当前 provider 或替换内置 `openai`）、`ApplyCodexEndpoint`（账号锁内只改已播种空间的 `config.toml`，内容不变不写）；播发改用 `codexSessionConfig`，改 Key、改地址时网关地址不丢。
- `workspace`：新增连接钩子，运行中准入与全新启动（种子之后）都执行，失败即启动失败。
- 服务端：`gatewayEligible` 覆盖两种 Agent；`extensionEnv` 只给 Claude 注入 `ANTHROPIC_BASE_URL`，`NO_PROXY` 两种都补；`extensionRoute` 按账号类型解析上游（Codex 每次读账号池，账号已改为 ChatGPT 登录时回 400 并提示重开）；`applyExtensions` 后台对齐 Codex 空间；`extensions.gateway.dedicated_connections` 与 `PUT /api/extensions/gateway`，传输缓存键包含连接方式。
- 网页：账号列表里 Codex API Key 账号可选、ChatGPT 登录不可选；列表页增加「每次请求独占一条连接」开关；三语词典。

**已验证**：`internal/credentials` 的改写、解绑恢复、播发保留网关、未播种空间不创建、账号池不被写入；`internal/workspace` 的钩子在运行中与全新启动都执行、失败阻止启动；服务端用合成 Responses 上游（推理摘要复读）跑通：绑定后后台改写空间配置、账号池不变、请求经网关转到账号池的真实上游并被中断（`response.failed` + `model_output_loop`）、事件记录、解绑后恢复上游；独占连接接口的修订号与传输生效；浏览器回归覆盖 Codex 账号可选与开关保存。Go 测试均在 `-race` 下通过。

**尚未验证**（2026-10-19 已补，见 §20）：真实 Codex CLI 在容器里使用改写后的 `config.toml` 走网关（含 app-server 与 exec 两条路径、终端）；生产网桥与防火墙对 1083 端口的放行。迁移时先绑定、再改账号地址，期间请求会依次经过两层网关，保护不中断。

## 19. 扩展与模型网关解耦（2026-10-17）

首版把网关做成了扩展框架的一部分：扩展页顶部是网关开关，SPI 的 `Handler` 必须实现模型方法，绑定只能按账号，清单的 `errors` 必填。这样后续每个扩展都隐含「建立在网关之上」，与「各种扩展各自独立」的目标不符。改造如下：

1. **网关移出**：配置从 `extensions.gateway` 迁到顶层 `model_gateway`（加载时自动迁移，旧段不再写回），代码在 `internal/server/modelgateway.go`，界面在「系统设置 → IP 代理 → 模型网关」，删除 `PUT /api/extensions/gateway`。网关地址的路径与 MAC 域（`agentbox-extension-gateway`）不变，正在运行的 CLI 不受影响。
2. **SPI 与宿主通用化**：钩子注册表 `extspi.Hooks()`（类型、权限、分组、依赖、绑定方式）；`Handler` 只剩 `Initialize` / `Shutdown`，模型方法在可选的 `ModelHandler`，`abort` 在 `ModelHost`；实例队列改为通用的 notice；`Host.Uses` / `Bound` 按钩子过滤。
3. **绑定通用化**：`none` / `all` / `selected`，可按账号和用户挑选；删除用户时同步解绑。
4. **事件钩子与 Webhook**：`workspace.lifecycle`、`chat.turn.ended`（§6.4）；第二个官方扩展 `agentbox.webhook`，推送通用 JSON 或飞书、钉钉、企业微信机器人消息（配置见 `docs/configuration.md`），完全不依赖网关。
5. **界面**：卡片显示钩子分组，空间头部列出所有作用于本空间的扩展。

外置进程运行时（§5.2、§5.3）仍按原计划在 P3 做；本次改动让 SPI 不再假设扩展处理模型流量，container 扩展可以只订阅事件。

## 20. 真实 CLI 与生产布局验收（2026-10-19）

**入口**：`AGENTBOX_GATEWAY_CLI_TEST=1 go test -run TestModelGatewayCLILive ./internal/server`，即 `python3 scripts/verify.py run --step cli.model-gateway`。本机已安装的 `claude` 与 `codex` 用产品的聊天 argv（去掉写容器 PID 文件的外壳）、产品 `execEnv` 给出的环境和绑定后改写的空间 `config.toml`，经服务端真实监听的模型网关访问合成上游；干净的临时 HOME，不用账号、不调模型、不起容器。版本：Claude Code 2.1.302、Codex 0.163.0（与生产 Agent 镜像相同）。

| 路径 | 正常回复 | 复读 |
| --- | --- | --- |
| Claude（`ANTHROPIC_BASE_URL=http://…/x/<空间>/<MAC>`） | 上游收到 `/api/v1/messages`（账号地址 `…/api` 加上 CLI 拼的 `/v1/messages`），`Bearer` 令牌原样 | 第一次流被中断；CLI 随即改发 `stream:false`、`max_tokens` 128000→64000 的兜底请求。修复前指纹不同，兜底到达上游；修复后被冷却以 422 拒绝，回合结果为 `API Error: 422 An identical request recently looped…[agentbox.loop-guard]` |
| Codex exec（空间 `config.toml` 的 `base_url` 为网关地址） | 上游收到 `/v1/responses` | 中断一次；CLI 重连 5 次全被冷却拒绝、不到上游，`turn.failed` 带同一说明 |
| Codex app-server（`RunCodexTurn`） | 同上 | 同上 |

**修复**：`pkg/modelgw.FingerprintBody` 去掉顶层投递字段（`stream`、`stream_options`、`max_tokens`、`max_output_tokens`、`max_completion_tokens`），扩展与独立 `abox-loop-guard` 共用。生产当时只绑定了 Codex 中转账号，未受这个缺口影响。

**生产布局**：从生产「Agentbox开发」空间（`net=bridge`，同时有透明网络辅助容器 `agentbox-net-*`）访问 `172.17.0.1:1083`：错误 MAC 返回 403 `model_gateway_forbidden`，未知路径 404，说明网桥与透明网络规则都放行网关端口。生产绑定的 Codex 中转账号空间，其 `config.toml` 已改写为网关地址，10-17 有多轮真实对话正常入账。

**仍未覆盖**：终端里的交互式 TUI（与上面同一 CLI 与 HTTP 客户端，环境来自 tmux 同步，只有单元测试）；真实供应商在中断后的行为；首发即非流式的请求（§16）。
