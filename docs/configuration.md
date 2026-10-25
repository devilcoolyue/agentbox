# 配置参考

[返回文档目录](README.md) · [项目首页](../README.md)

配置示例见 [`config.example.json`](../config.example.json)，完整结构与校验见 [`internal/config/config.go`](../internal/config/config.go)。服务启动时用 `-config` 指定配置文件；相对的 `data_dir` 和 `credentials_dir` 均相对于该配置文件所在目录解析。

## 如何修改配置

日常调整优先通过「系统设置」：服务会校验并原子写回配置。直接编辑磁盘上的 JSON 不会自动热加载，需要重启；也应避免同时从网页保存，导致人工修改被旧内存配置覆盖。

下面的“默认值”指配置文件省略该字段时的加载值；示例文件中的值不一定等于加载默认值。尤其是 `listen`：**代码默认 8080，示例和本文部署流程使用 8180**。

## 基础字段

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8080` | HTTP 监听地址；示例显式设为 `127.0.0.1:8180`，更改后需重启 |
| `auth_token` | 必填 | 至少 8 字符，拒绝示例占位值；首次创建管理员密码，也用于派生代理口令 |
| `data_dir` | `data` | SQLite、用户目录、模板、缓存等运行数据根目录 |
| `agent_image` | `agentbox-agent:latest` | 用于创建工作空间的 Docker 镜像 |
| `permission_mode` | `bypassPermissions` | 接受 `default`、`acceptEdits`、`plan`、`bypassPermissions`；供 Claude 无头命令使用 |
| `max_upload_mb` | `512` | 项目、文件、技能包及聊天附件（含图片 / PDF）的上传大小上限，必须 ≥ 1；在系统设置中修改后即时生效，反向代理也需允许相应请求体及 multipart 开销 |
| `chat_history_timeout_sec` | `120` | 网页打开空间、刷新历史及加载更早历史的超时秒数，范围 1–3600；系统设置 → 安全与访问可修改，其他已打开的页面刷新后生效，不改变登录或模型执行期限 |
| `idle_timeout_min` | `30` | 空闲停机分钟数；显式设为 `0` 关闭，不能为负 |
| `turn_timeout_min` | `120` | 单轮对话的时长上限（分钟），范围 5–1440；到时像用户停止一样中断 CLI，30 秒内没停下才放手。等待用户回答提问的时间不计入；可能委派跑腿的回合至少 2 小时。系统设置 → 容器与资源可修改，即时生效 |
| `timezone` | `Asia/Shanghai` | 有效的 IANA 时区，控制页面时间和使用记录筛选 |

首次启动且用户表为空时，服务创建管理员 `boxadmin`。后续登录密码由数据库中的密码哈希决定，修改 `auth_token` 不会改密码；它仍需作为服务端密钥保留。

`permission_mode` 不应被理解成跨两种 CLI 的统一交互审批界面。Codex 有自己的执行配置，终端中的 CLI 也由自己的交互与配置控制。

## 容器资源

| 字段 | 默认值 | 校验与用途 |
| --- | --- | --- |
| `container.memory_mb` | `2048` | 内存上限，按 MiB 转换，至少 128 |
| `container.cpus` | `2` | CPU 限额，必须大于 0 |
| `container.pids_limit` | `512` | 进程数上限，至少 16 |
| `container.network` | `bridge` | Docker 网络模式 / 网络名称，不能为空 |

容器以 `1000:1000` 运行，挂载 `/workspace`、`/home/agent`、`/shared`，并启用 `no-new-privileges`。

**资源与网络配置在创建容器时使用。** 当前实现可以复用已停止的同镜像容器，因此单纯停止再启动不保证刷新现有容器的资源限制。新空间使用新配置；排查旧空间时应以 `docker inspect` 为准，需要更新旧容器时安排维护窗口重建，先确认持久目录完整。

镜像标签对应的新镜像构建完成后，运行中的容器继续使用旧镜像；停止再启动时会识别镜像 ID 变化并重建。容器可写层里的临时安装会随重建丢失，常用系统依赖应加入镜像。

## 账号、模型与价格

| 字段 | 结构 | 用途 |
| --- | --- | --- |
| `accounts` | 账号数组，可为空 | `id`、`type`、`label`、`credentials_dir`、`env`、`proxy_id`、`access`、`model_reasoning`、`models`、`default_model` |
| `models` | 按 `claude` / `codex` 分组的数组 | 每项含 `id`、`label` 与可选 `reasoning` 能力，维护对话候选模型 |
| `default_models` | 按 Agent 分组的模型 ID | 初始为 `claude-opus-5` / `gpt-5.5`，必须在对应候选列表中；配置了 `models` 的账号改用自己的 `default_model` |
| `pricing_catalog` | `url` + `auto_check` | 独立 HTTPS 价格目录；默认不联网，自动检查只生成候选，详见 [维护流程](pricing-catalog.md) |
| `pricing_managed` / `pricing_history` | 跟随目录元数据 / 最近 10 次价格版本 | 由价格管理接口维护，随配置原子持久化 |
| `pricing` | 模型 ID / Agent 名到单价的映射 | provider 不报价时用于 token 折算；省略则无价格表 |

`accounts[].type` 仅接受 `claude` 或 `codex`。账号 ID 与代理 ID 为 2–32 位小写字母、数字、`-`、`_`，首位为字母或数字，且在各自列表内唯一。账号引用的 `proxy_id` 必须存在于代理池。

`access` 配置 `all` / `users` / `admin` 使用范围，省略时全体共享；`users` 模式用 `users` 数组列出用户名。`model_reasoning` 按模型 ID 覆盖推理能力，字段与示例见[账号与模型](accounts-and-models.md#推理强度与思考预算)。`models` / `default_model` 是账号自己的可用模型和默认模型，非空时网页对话只能使用其中的模型，见[账号可用模型](accounts-and-models.md#账号可用模型)。

`env` 是字符串键值映射，常用于 API Key 和 provider 地址，属于敏感配置。网页创建账号时会使用 `<data_dir>/creds/<id>/` 作为凭证目录。

模型示例（配置片段）：

```json
{
  "models": {
    "claude": [{ "id": "claude-opus-5", "label": "Opus 5" }],
    "codex": [{ "id": "gpt-5.5", "label": "GPT-5.5" }]
  },
  "default_models": {
    "claude": "claude-opus-5",
    "codex": "gpt-5.5"
  }
}
```

账号操作见[账号与模型](accounts-and-models.md)；费用字段和定价示例见[使用记录与额度](usage-and-quotas.md)。

## 出口代理与隧道

| 字段 | 默认值 / 格式 | 说明 |
| --- | --- | --- |
| `proxies` | 空数组 / 省略 | 每项含 `id`、`name`、`scheme`、`host`、`port`、可选认证与 `disabled` |
| `proxies[].scheme` | `socks5` / `http` / `https` | 上游代理协议，端口范围 1–65535 |
| `proxy_bridge.bind` | `172.17.0.1:1081` | 容器访问的本地 HTTP 代理桥接监听地址 |
| `proxy_bridge.host` | 从 bind 提取主机 | 注入容器代理 URL 的主机；监听通配地址时应显式给出可达地址 |
| `tunnel.enabled` | `false` | 是否接受 abox-link 内网连接 |
| `tunnel.transparent` | `true` | 默认选择透明模式；总开关仍由 `enabled` 独立控制，显式 `false` 保留兼容代理模式 |
| `tunnel.network_bind` | `proxy_bind` 主机的 `1082` 端口 | 透明网络控制监听地址，仅向容器网络开放 |
| `tunnel.network_image` | `agentbox-network:latest` | 透明网络辅助镜像，使用前需构建 |
| `tunnel.proxy_bind` | 隧道启用或透明模式为 true 且未填写时为 `172.17.0.1:1080` | 内网 SOCKS5 代理监听地址 |
| `tunnel.proxy_host` | 从 proxy_bind 提取主机 | 容器实际连接的主机；bind 为通配地址时必须显式配置 |

桥接和隧道地址应为宿主机上可绑定、且容器可达的地址。可在 Linux 上查看默认网桥网关：

```bash
docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'
```

这些是容器侧服务端口，日常部署不需要把它们开放到公网。用户电脑通过 HTTPS / WSS 连接 agentbox 即可。配置和使用步骤见[网络手册](networking.md)。

## 模型网关

```json
{
  "model_gateway": {"bind": "172.17.0.1:1083", "host": "", "dedicated_connections": false}
}
```

模型网关是「检查模型流量」这类扩展（如循环保护）依赖的基础设施，不属于扩展框架本身：只有启用了带 `model.request` / `model.stream` 钩子的扩展时才监听，其他扩展用不到它。

| 字段 | 默认值 / 格式 | 说明 |
| --- | --- | --- |
| `model_gateway.bind` | `172.17.0.1:1083` | 监听地址，须容器可达；不能与 `proxy_bridge.bind`、`tunnel.proxy_bind`、`tunnel.network_bind` 相同。改动需重启服务，已在运行的 CLI 持有旧地址 |
| `model_gateway.host` | 从 bind 提取主机 | 写进容器 base URL 的主机；bind 为通配地址时必须填写 |
| `model_gateway.dedicated_connections` | `false` | 每次上游请求独占一条 HTTP/1.1 连接、不复用，中断时关掉这条连接（同独立网关的 `-dedicated-connections`）。代价是每次请求重新握手。可在「系统设置 → IP 代理 → 模型网关」切换，即时生效 |

2026-10-17 之前这一段写在 `extensions.gateway` 下；旧配置加载时自动迁移到 `model_gateway`，下次保存配置时写成新位置。

## 请求计时遥测

```json
{
  "usage_telemetry": {"bind": "172.17.0.1:1084", "host": "", "disabled": false}
}
```

Claude 与 Codex 空间的 CLI 经 OpenTelemetry（OTLP/HTTP JSON）把每次 API 请求的首字与耗时报到这里，使用记录据此显示每次请求的实测延迟与输出速度（见[用量与额度](usage-and-quotas.md#每次请求的实测计时cli-遥测)）。Claude 靠注入的环境变量开启，Codex 靠空间 `.codex/config.toml` 里的 `[otel]` 段。默认开启，只接收容器的上报，不需要开放到公网。

| 字段 | 默认值 / 格式 | 说明 |
| --- | --- | --- |
| `usage_telemetry.bind` | `172.17.0.1:1084` | 监听地址，须容器可达；不能与 `proxy_bridge.bind`、`model_gateway.bind`、`tunnel.proxy_bind`、`tunnel.network_bind` 相同。改动需重启服务；已在运行的 CLI 持有旧地址，重启 CLI 后才用新地址 |
| `usage_telemetry.host` | 从 bind 提取主机 | 写进容器上报地址的主机；bind 为通配地址时必须填写 |
| `usage_telemetry.disabled` | `false` | 设为 `true` 时不监听，也不再给新启动的 Claude 注入遥测环境变量，Codex 空间的 `[otel]` 段在下次对话或打开终端时移除；使用记录只显示日志估算的延迟 |

监听失败（地址被占用、网桥不存在）只记一条日志，服务照常运行，此时也不注入遥测环境变量、不写 Codex 的 `[otel]` 段。上报路径为 `/otel/<空间 ID>/<MAC>/v1/logs`，MAC 由 `auth_token` 派生，轮换 `auth_token` 后已在运行的 CLI 上报会被拒绝，重启 CLI 即恢复。

## 对话缓存保活

```json
{
  "cache_keepalive": {"disabled": false, "max_hours": 3, "max_hours_limit": 6, "min_context_tokens": 100000, "interval_minutes": 50, "pause_percent": 95}
}
```

Claude 网页对话每轮都是一个新的 CLI 进程（`claude -p --resume`），provider 在最后一次使用后把对话的提示缓存保留 1 小时（订阅账号；API Key 与中转账号的 CLI 写 5 分钟缓存）。用户离开超过 1 小时再回来，要把整段对话重新写进缓存，按价目表约是读一遍缓存的 20 倍。开启缓存保活的空间，用户离开期间服务端按间隔续聊这个对话，发一条不保存的短请求读一遍缓存，让缓存重新计时。对话记录里看不到这条请求，使用记录里单列为「缓存保活」，见[用量与额度](usage-and-quotas.md#记录范围)。

保活按空间开启，默认关闭：空间属主在工作台「⋯ → 缓存保活…」里打开，可同时勾选「终端里的 Claude 会话也保活」，并在管理员上限内改自己的上下文门槛与最长时长（留空跟随默认值）。弹窗列出正在进行的计划（下次时间、已续次数、每次预计费用），可单独停止；对话输入栏的上下文占用旁、终端顶栏会显示保活状态。最近一轮在 1 小时内结束的对话，开启后立即排上。

这一段是管理员的设置，在「系统设置 → 容器与资源 → 对话缓存保活」修改，即时生效：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `cache_keepalive.disabled` | `false` | 设为 `true` 时不允许开启，所有空间立即停止保活 |
| `cache_keepalive.max_hours` | `3` | 空间没设时，最后一次请求后最多续多久，1–24；最后一次保活后缓存还能再留约 1 小时 |
| `cache_keepalive.max_hours_limit` | `6` | 空间最多可设多少小时，1–24；默认值不会超过它 |
| `cache_keepalive.min_context_tokens` | `100000` | 空间没设时的门槛：最后一次请求的提示词（含缓存读写）少于这个数的对话不保活。保活和重写的费用都与上下文成正比，小对话省下的钱抵不上唤醒空间的开销 |
| `cache_keepalive.interval_minutes` | `50` | 两次读缓存的间隔，30–55 分钟；要给计时误差、较长的回答和唤醒空间留出余量 |
| `cache_keepalive.pause_percent` | `95` | 空间没设时的 5 小时额度暂停阈值，50–99（见下文「5 小时额度暂停」） |

只保活最后一次请求写的是 1 小时缓存的 Claude 对话；空间账号是 API Key 或中转（且账号 env 没把 `CLAUDE_CODE_PROMPT_CACHE_TTL` 设成 `1h`）时弹窗说明不可用。开启委派的回合带临时的系统提示和工具，无法复现前缀，不保活。

- **网页对话**：每个空间一个计划，跟随最近一轮所在的线程。空间被空闲回收休眠时，保活会临时唤醒它，续完重新休眠，期间有人打开就保持运行；这两步不触发扩展的启动 / 停止事件。服务重启（包括升级）不打断保活：各会话的保活进度记在数据目录的 `keepalive-points.json`，启动后照原来的时间接着续；重启时正在跑的那一轮被服务中断，也和用户中断一样留下它的缓存。
- **5 小时额度暂停**：Claude 订阅的用量按 5 小时窗口计，长任务一路跑满后连保活的请求也会被拒，保住的缓存随之过期。开启保活的空间因此在 5 小时额度用到暂停阈值（弹窗里的「暂停阈值」，留空跟随管理员默认 95%，可改成 50–99 或取消勾选关闭）时暂停对话：正在跑的回合在下一次模型请求前像用户停止一样中断（工具结果已写进对话，不丢已做的工作），历史里记一行「5 小时额度达到暂停阈值，这一轮已暂停」；服务端随即用原来的模型与推理设置排队一条「继续」，额度重置一分钟后自动执行。暂停期间页面发送的消息排在它后面（普通发送被拒，问题码 `chat_quota_reserved`），可以在发送状态里撤回这条「继续」；调高或关闭阈值立即解除暂停。保活不受暂停影响，并且至少续到重置后 15 分钟（不超过 `max_hours_limit`），恢复时直接读缓存。用量来自 CLI 每次响应附带的 `rate_limit_event`，按账号记在数据库里，服务重启后照样暂停到重置；同一账号在终端或其他电脑上的用量，要等下一次网页对话或保活的响应才知道。
- **终端会话**：终端用量补记扫到交互式 Claude Code 会话的新请求时排上，每个工作目录一个计划（同一目录开了新会话就换成新的）。保活在会话当时的工作目录里 `--resume` 它：Claude Code 2.1.303 把系统提示与工具定义记在 transcript 里，续聊时沿用，所以读到的正是终端写的缓存。空间被空闲回收休眠时同样临时唤醒、续完重新休眠：终端里的 CLI 虽已随容器退出，用户回来后在终端里 `claude -c` 或 `claude --resume` 接着这个会话，新进程照样沿用记录的前缀，读得到保住的缓存（2.1.304 实测）；另开新会话就用不上。用户自己退出 CLI（写下 cost-state）时停止。服务重启后照 `keepalive-points.json` 接着保活，已经续过一小时以上的会话也不会断。

用户手动停止的空间、切换了账号、对话已删除或 `/clear`、余额用完、空间关闭了保活，或者保活时发现缓存已经失效（读到的不到一半，或写的是 5 分钟缓存）时，停止保活。

## 扩展

```json
{
  "extensions": {
    "items": {
      "agentbox.loop-guard": {
        "enabled": true,
        "config": {"window": 256, "distinct_limit": 24, "cooldown_seconds": 30},
        "permissions": ["model.request.fingerprint", "model.stream.text"],
        "bindings": {"mode": "selected", "accounts": ["relay-a"]},
        "failure_policy": "",
        "revision": 1
      }
    }
  }
}
```

| 字段 | 默认值 / 格式 | 说明 |
| --- | --- | --- |
| `items.<id>.enabled` | `false` | 是否运行该扩展 |
| `items.<id>.config` | 清单默认值 | 按扩展清单的 `config_schema` 校验，未知键会被拒绝 |
| `items.<id>.permissions` | 空 | 管理员批准的权限，只能是清单申请过的；缺少钩子所需权限时不能启用 |
| `items.<id>.bindings` | `{"mode":"none"}` | 作用范围：`none` 不生效；`all` 全部（含以后新增的）；`selected` 按 `accounts`（账号 ID）和 / 或 `users`（用户名）选择，命中其一即生效。能按哪种选由扩展清单的 `bindings` 决定：模型钩子（如循环保护）只能按账号，且只改道 Claude / Codex 的 API Key / 中转账号，订阅与 ChatGPT 登录不会改道；Codex 的地址写在空间的 `.codex/config.toml` 里，账号池保持真实上游。旧配置的 `accounts` 模式按 `selected` 读取 |
| `items.<id>.failure_policy` | 空 = 清单默认 | `fail_open` 放行或 `fail_closed` 拦截；循环保护默认放行 |
| `items.<id>.revision` | 服务端维护 | 每次保存加一；网页提交时用它防止覆盖他人的修改 |

内置扩展：

- `agentbox.loop-guard`（循环保护）：检查模型流量，依赖模型网关，只能按账号绑定。参数见 [loop-guard.md](loop-guard.md)。
- `agentbox.webhook`（Webhook 通知）：把空间的创建、启动、停止、删除和对话回合结果推送出去，不依赖模型网关，可按账号或用户绑定。`config` 字段：`url`（http / https，机器人地址里的密钥只有管理员看得到，日志里不会出现）、`format`（`json` 推送事件原文并附 `text` 摘要；`feishu` / `dingtalk` / `wecom` 推送对应机器人的文本消息）、`turns`（`failed` 只推失败和中断的回合，默认；`all`；`none`）、`workspaces`（`all` 默认 / `none`）、`language`（`zh-CN` 默认 / `zh-TW` / `en`）。权限 `workspace.events` 与 `chat.turns` 可以只批准一个，只接收那一类事件。发送失败重试一次（网络错误、429、5xx），之后计入「送达失败」并写扩展日志；积压超过 256 条时丢弃并计数。机器人自己的关键词、加签等安全设置需要在机器人那边按「关键词」配置（消息都以 `agentbox ·` 开头），v1 不支持加签密钥。

建议在「扩展」页修改，不必手写；网页保存会校验清单并立即生效（扩展重启、冷却记录清空）。删除账号或用户时会同时把它从各扩展的绑定中移除。带模型钩子的扩展绑定后，新开的对话与终端执行改走模型网关，已打开的终端窗口需新开窗口。

## 终端提示语

```json
{
  "terminal_tips": {
    "tips": ["可直接粘贴图片，路径可点击预览", "项目文件请保存在 /workspace"],
    "interval_sec": 4,
    "animation": "scroll"
  }
}
```

提示语可在「系统设置 → 界面与提示」维护，最多 30 条、每条 200 个字符。间隔 ≤ 0 时只显示第一条，不轮播；当前动画为 `scroll`。省略该块或启动加载时列表为空，会补入默认提示语。

## 设置何时生效

| 修改 | 生效时机 |
| --- | --- |
| 在网页修改空闲时间、价格、候选模型和时区 | 保存后供后续操作读取 |
| 系统默认模型 | 随后新建的空间；已有空间保留自己的值 |
| 账号 env、出口代理与兼容隧道环境变量 | 后续 CLI 执行；已运行终端需重连并新建窗口 |
| 扩展的启用、参数与账号绑定 | 网页保存后扩展立即重启；base URL 改写对后续 CLI 执行生效，已运行终端需新建窗口 |
| 透明隧道放行规则 | 约 2 秒同步，以空间网络就绪状态为准；既有 Shell 的新连接生效 |
| 透明模式开关 | 影响容器网络布局；切换前停止空间，再按[网络手册](networking.md#透明内网访问)重启 |
| home 模板 | 下次启动路径执行时同步；技能也可手动装到当前空间 |
| 镜像 | 运行中空间不打断，停止后再次启动识别新镜像 |
| 容器资源和网络 | 新建 / 重建容器时；旧容器可能继续复用 |
| HTTP 监听地址 | 服务重启后；网页返回 `restart_required` |
| 直接编辑 JSON | 不自动读取，需重启服务 |

## 持久目录

```text
config.json                         服务配置与账号 env
accounts/<id>/                      手工指定的账号凭证（示例约定）
data/                               实际根目录由 data_dir 决定
  agentbox.lock                     单实例锁
  state.db                         SQLite：用户、空间、令牌、用量和额度账本
  state.db-wal / state.db-shm       SQLite 运行期间的辅助文件
  creds/<id>/                      网页创建账号的凭证
  home-template/                   全体用户的 home 模板
  themes.json                      管理员导入的全站界面主题（见 themes.md）
  icons.json                       管理员设置的全站图标覆盖（见 icons.md）
  announcement.json                当前系统通告（见 announcements.md；不进系统备份）
  marketplace/repo/                官方插件缓存（设置 cache_dir 后移到该目录下）
  abox-link/                       网页提供下载的客户端二进制
  backups/                         内置脚本生成的备份包
  users/<user>/
    home-template/                 用户模板
    themes.json                    该用户的个人界面主题
    shared/                        用户跨空间共享目录
    sessions/<id>/
      workspace/                   容器 /workspace
      home/                        容器 /home/agent
      chats/<thread-id>.jsonl       对话线程
      chats/active                 当前线程标记
```

升级首次打开数据库时会导入旧版 `state.json`；旧版单文件聊天记录在访问时迁移到线程存储。迁移或恢复前，应同时保留数据库快照和对应用户文件，详见[部署手册](../deploy/README.md)。

## 独立缓存与容量限制

`cache_dir` 相对配置文件目录解析；空值/省略时沿用 `data_dir`，市场位于其 `marketplace/` 子目录。缓存目录是部署配置，修改后需重启；设置 API 不修改路径，其他设置保存会保留它。恢复备份时缓存重定位到恢复目录的 `cache/`，不写回原实例缓存。

`resources` 支持 `max_running`、`max_running_per_user` 和 `min_free_bytes`，均为非负整数，0 禁用。可从管理员设置 API/界面原子保存并立即影响新启动。限制与磁盘统计口径见[运行维护](architecture/deployment-layout.md#容量与回收)。

Git 连接与 OAuth 应用可在网页选择 `network.route=tunnel` 及 `network.ca_pem`；路由固定为连接属主的 abox-link 隧道，断开时不直连，客户端需放行目标域名/端口。公司 CA 仅附加到该连接 TLS 信任池。SSH 连接需配置可信的服务器主机公钥并核对 SHA256 指纹，私钥不下发容器。详见 [Git API](api.md#企业网络与-ssh)。

## 共享工具存储

可选 `tool_storage.root` 指定数据盘上的共享工具和用户缓存根，`tool_storage.go_version` 指定已发布的 Go SDK。默认关闭，操作员配置后重启服务端，已有空间下次启动生效。见[共享工具与用户缓存](shared-tools.md)。

## 存储容量策略

操作员配置 `storage_policy`，默认关闭，修改后重启。用户数据/缓存项目配额、全实例分配预算、挂载 UUID 校验及维护参数见[本地存储容量管理](storage-capacity.md)。迁移与调整通过离线 `storage-config` / `storage-prepare`，网页设置写回保留这些字段；每用户数据/缓存上限可由管理员在用户管理里在线设置。
