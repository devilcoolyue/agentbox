# AGENTS.md

给代码助手/维护者的项目说明。用户向的部署与功能文档见 `README.md`；这里记录实际代码结构、开发命令和容易踩坑的约定。

## 项目是什么

`agentbox` 是一个 Go 单二进制服务端：在 Linux 服务器上通过 Docker 为每个浏览器会话拉起一个容器，容器里运行 Claude Code / Codex CLI。浏览器通过 HTTP/WebSocket 使用对话、终端、文件、共享目录、账号池和内网反向隧道。

两个入口：

- `cmd/agentbox`：服务端。加载 `config.json`，对 `data_dir` 加 flock，初始化 store/docker/server 后监听 HTTP。
- `cmd/abox-link`：用户本机反向隧道客户端。无参数时开本机控制台 `127.0.0.1:7801`；带 `--server` 时走命令行无头模式。

运行环境：服务端依赖 Docker daemon；生产部署目标是 Linux + systemd。macOS 上可以 `go build`/`go test`，但不能完整验证容器链路。

## 仓库地图

| 路径 | 作用 |
|---|---|
| `cmd/agentbox/main.go` | 服务端启动入口；`lockDataDir` 防止多个进程共用同一个 `data_dir`。 |
| `cmd/abox-link/main.go` | 隧道客户端入口；面板模式与 `--server` 无头模式分流。 |
| `internal/config` | 配置 schema、校验、运行时修改与原子写回。所有设置变更必须经 `Config.mutate`/`ApplySettings`/账号方法。 |
| `internal/server` | HTTP API、鉴权、用户/账号/设置、会话、文件、聊天 WS、终端 WS、隧道、监控、空闲回收、凭证同步、Git 变更审查（`git.go`）。 |
| `internal/store` | SQLite(`data/state.db`)：sessions/users/tokens/usage_events/quotas/credit_ledger；首次打开会导入旧版 `state.json`。 |
| `internal/dockerx` | Docker Engine API 封装：容器生命周期、exec PTY/stream、stats、镜像/挂载检查。 |
| `internal/agent` | Claude/Codex 适配层：headless 命令、标题生成、凭证播种、Claude HUD、Codex app-server 协议。 |
| `internal/archivex` | 上传压缩包解压（防 zip-slip/符号链接/解压炸弹）与工作区 zip 下载。 |
| `internal/tunnel` | yamux 隧道协议、白名单、端口映射。 |
| `internal/linkapp` | abox-link 客户端实现：配置、面板、守护/自启、重连监督器；`static/` 是面板前端，随 `cmd/abox-link` 独立 `go:embed`。 |
| `internal/web` | 嵌入前端静态资源；`AGENTBOX_WEB_DIR` 可改为磁盘热加载。 |
| `images/agent` | 会话容器镜像 Dockerfile；内置 Claude Code、Codex CLI、tmux、claude-hud。 |
| `scripts` | 镜像构建/自动升级、abox-link 交叉编译、域名与账号登录辅助脚本。 |
| `deploy` | systemd 单元（服务、镜像更新、数据备份）、logrotate、安装/发布脚本、生产参数模板。 |

## 常用命令

```bash
# 基础校验
go build ./...
go test ./...

# 构建两个二进制
go build -o agentbox ./cmd/agentbox
go build -o abox-link ./cmd/abox-link

# 构建会话容器镜像
./scripts/build-image.sh

# 交叉编译 abox-link 到 data/abox-link/ 供 Web UI 下载
# 注意：客户端依赖较新服务端接口，通常先 deploy 服务端再跑这个
./scripts/build-clients.sh

# 检查 npm 上 Claude Code / Codex 新版并重建镜像
./scripts/auto-update-image.sh

# 本地试跑（先 cp config.example.json config.json 并改 auth_token/accounts）
./agentbox -config config.json

# 前端热改：不嵌入，直接吃磁盘文件
AGENTBOX_WEB_DIR=internal/web/static ./agentbox -config config.json

# 生产部署/日常发布（Linux，需要 root）
sudo ./deploy/install.sh
sudo ./deploy/deploy.sh
```

特殊测试：

```bash
# 默认跳过；真实调用本机 codex app-server，消耗少量额度
CODEX_LIVE_TEST=1 go test -run TestRunCodexTurnLive ./internal/agent/
```

## 生产环境发布

生产机的具体地址、目录、域名等敏感信息不写进版本库，集中放在
`deploy/production.env`（已 gitignore，模板见 `deploy/production.env.example`）。
执行下面任何命令前先加载它：

```bash
source deploy/production.env   # 提供 PROD_SSH / PROD_DIR / PROD_LISTEN / PROD_DOMAIN / PROD_URL
```

| 变量 | 含义 |
|---|---|
| `PROD_SSH` | 生产机 SSH 目标（已配置免密登录，自动化时加 `-o BatchMode=yes`） |
| `PROD_DIR` | 生产机上的仓库目录 |
| `PROD_LISTEN` | 服务监听地址（如 `127.0.0.1:8180`） |
| `PROD_URL` | 公网访问地址 |

systemd 服务名固定为 `agentbox.service`。

只有用户明确要求部署到生产时才执行以下操作。发布会重启服务并短暂断开现有
HTTP/WebSocket 连接，不是滚动发布。

### 发布前检查

本地先确认改动范围并跑完整校验：

```bash
git status --short --branch
go build ./...
go test ./...
```

然后确认远端服务与仓库状态。远端可能有用户留下的未跟踪文件；保留它们，不要
使用 `git clean`、`git reset --hard` 或 `rsync --delete`：

```bash
ssh -o BatchMode=yes "$PROD_SSH" 'systemctl is-active agentbox'
ssh -o BatchMode=yes "$PROD_SSH" "git -C $PROD_DIR status --short --branch"
```

### 已提交代码发布

代码已提交并推送到 `origin/main` 时，在生产仓库仅快进拉取，再运行部署脚本：

```bash
ssh -o BatchMode=yes "$PROD_SSH" "git -C $PROD_DIR pull --ff-only origin main"
ssh -o BatchMode=yes "$PROD_SSH" "$PROD_DIR/deploy/deploy.sh"
```

如果远端有已跟踪文件改动，先判断来源；不要擅自覆盖或回退。冲突时停止发布并向
用户说明。

### 本地未提交改动发布

用户要求直接预览尚未提交的本地改动时，只用 `rsync -R` 定向同步本次改动涉及的
源码文件，保持仓库相对路径。不要同步整个仓库，也不要覆盖 `config.json`、
`accounts/`、`data/` 或远端未跟踪文件。例如：

```bash
rsync -azR \
  internal/web/static/index.html \
  internal/web/static/css/shell.css \
  internal/web/static/js/theme.js \
  "$PROD_SSH:$PROD_DIR/"

ssh -o BatchMode=yes "$PROD_SSH" "git -C $PROD_DIR diff --check"
ssh -o BatchMode=yes "$PROD_SSH" "$PROD_DIR/deploy/deploy.sh"
```

文件列表必须按实际改动调整。`deploy.sh` 会在生产机用当地 Go 工具链重新构建，
备份最近的旧二进制、原子替换、重启服务，并检查 systemd 与监听端口。日常发布
不要重复运行 `install.sh`；只有首次安装、systemd 单元缺失或仓库目录变更时才运行。

### 发布后验证

部署脚本成功后仍要检查服务状态、最近日志、本机监听和公网入口：

```bash
ssh -o BatchMode=yes "$PROD_SSH" 'systemctl is-active agentbox'
ssh -o BatchMode=yes "$PROD_SSH" "curl -sS -o /dev/null -w '%{http_code}\n' http://$PROD_LISTEN/"
ssh -o BatchMode=yes "$PROD_SSH" 'journalctl -u agentbox --since "5 minutes ago" --no-pager -n 30'
curl -sS -o /dev/null -w '%{http_code}\n' --connect-timeout 10 --max-time 20 "$PROD_URL"
```

四项均正常的最低标准是：systemd 返回 `active`、内外网 HTTP 都返回 `200`，且
最近日志没有启动失败。前端静态资源嵌入二进制，部署重启后会生成新的内容哈希；
不需要单独执行 npm 构建或复制静态目录。

## 核心链路

### 会话生命周期

- 创建：`POST /api/sessions` 在 `data/users/<user>/sessions/<id>/` 下创建 `workspace` 与 `home`，并 `chown` 到容器用户 `1000:1000`。
- 启动：`startSession` 是幂等路径，REST 启动、聊天 WS、终端 WS 都会走它。流程：同步账号池 OAuth 轮换凭证 → `agent.SeedCredentials` 播种 home → 可选播种内网提示 → 确保用户共享目录 → `dockerx.EnsureRunning` 复用/重建容器 → 更新 SQLite。
- 停止/删除：只停/删容器；工作区、home、聊天线程仍在宿主机。`DELETE ?purge=1` 才删除会话目录。
- 重启服务端后：`Server.reconcile` 以 Docker 实际运行状态修正 session status。

关键目录：

```text
data/
  state.db
  users/<user>/
    shared/
    sessions/<id>/
      workspace/   -> 容器 /workspace
      home/        -> 容器 /home/agent
      chats/<tid>.jsonl
```

### 聊天

- `GET /api/sessions/{id}/chat` 是一个会话一个 room 的广播模型；同一房间同一时刻只跑一个回合。
- 消息落盘到当前线程 `chats/<threadID>.jsonl`；`chats/active` 指向当前线程。旧版 `chat.jsonl` 首次访问自动迁移。
- Claude 走 `claude -p --output-format stream-json`；Codex 优先走 `codex app-server`（真流式增量），握手失败回退 `codex exec --json`。
- `stream_event`/增量事件只广播不落盘；完整事件落盘并广播。provider 会话 id 用 `agent.ExtractSessionID` 提取，写入 `chat_session` 供 `--resume`/thread resume。
- 用户中断：Codex app-server 优先协议内 `turn/interrupt`，否则用容器里的 PID 文件发 SIGINT。

### 用量计量

- 回合收尾事件里的 token/费用落进 `usage_events` 表（`internal/server/usage.go` 解析，
  `runTurn` 的 `onLine` 里挂钩），并在**同一个事务**里从用户额度扣掉（见下节）。
- Claude 的 `type:"result"` 按 `modelUsage` **每个模型出一行**（含子 agent 用的 haiku），
  同回合各行共享 `turn_id`。实测 `total_cost_usd` 等于各行 `costUSD` 之和，而顶层 `usage`
  只覆盖主模型——计费别用顶层 `usage`，会漏记。
- Codex 的 `type:"turn.completed"` 只有 token、**不报费用**（`cost_micro_usd` 记 0，
  待定价表就位后按 token 折算）。两处形状已对 codex-cli 0.145.0 实测核对过，
  app-server 翻译出的事件与 `codex exec --json` 自己吐的完全一致，`parseUsage` 一套
  分支通吃。两条语义是实测结论，别按字面直觉改：
  - `cached_input_tokens` 是 `input_tokens` 的**子集**，写库时减掉才对齐 Claude 的
    「未命中缓存输入」语义；
  - `reasoning_output_tokens` 已经含在 `output_tokens` 里（实测 output=150 /
    reasoning=143 而答案只有几个字），**再加一遍就是重复计费**。
- 费用一律存整数微美元（USD × 1e6），不进 float——报表是上万行累加，float 会漂。
  原始事件存在 `raw`（每回合一份），归一化判断错了可据此重算。
- `kind` 列分开「用户的对话」(`chat`) 与「服务端自动起标题」(`title`)。两者都可能
  落在同一个便宜模型上，只看 `model` 分不出来。
- 起标题的消耗两种 agent 都记，都由 `agent.TitleOutput` 从命令输出里拆出用量：
  - claude 的 `TitleCommand` 用 `--output-format json`（不是 `text`），标题在
    `result` 字段，同一对象带着 usage；
  - codex 的 `TitleCommand` 把 `--json` 事件流写进临时文件，正文之后补一行
    `---abox-usage---` 分隔符再接 `turn.completed`；`TitleOutput` 从**末尾**找分隔符，
    这样标题里恰好出现这串字符也不会吞掉用量行。
  **改这两处的输出格式会直接让标题消耗重新变成漏账。**
- 回合跑完一行用量都没记到时会打 `usage: … 未记到用量` 警告。见到它说明有 agent
  版本报用量的形状没被认出来，别忽略。

### 额度与扣减

`internal/store/quota.go`（账本）+ `internal/server/quota.go`（定价、拦截、管理接口）。

- **没有 `quotas` 行 = 不限额。** 老库升上来一行都没有，所有人照旧畅通；管理员给谁
  开额度谁才被计。别把「没开额度」写成「余额 0」，那等于全员断服。
- **扣减和 `usage_events` 的插入同事务**（`store.InsertUsage`）。不存在「用量记了但
  钱没扣」的中间态。想绕过它单独插用量行时先想清楚这一点。
- **幂等键是 `credit_ledger.ref`，带 UNIQUE 索引。** 消耗流水用 `usage:<行id>`，
  充值用 `grant:<管理员填的或服务端生成的>`——前缀是故意的，防止管理员填个
  `usage:1` 撞掉一条扣款。重放同一个 ref 一分钱都不会动。
- **余额可以是负数，这是设计。** 一个回合花多少钱要等 provider 收尾事件才知道，中途
  没有可靠累计值，所以拦截只发生在**回合开始前**（`handleChatWS` 的 `user_message`
  分支），余额见底的那个回合允许超支。宁可多花一个回合，也不把用户跑到一半的任务
  腰斩。起标题这趟服务端自发的消耗在余额见底时直接跳过。
- **claude 的价以 provider 报的为准**；只有不报价的 codex 用 `config.json` 的
  `pricing` 按 token 折算。价目表单位是「每百万 token 多少美元」，微美元成本正好
  等于 `tokens × rate`（两个 1e6 约掉）。查表顺序：精确模型名 → agent 名兜底
  （codex 事件不报模型名，兜底那条要配成账号 `config.toml` 里的默认模型）。
  **没配价目表的模型按 0 计**，只记不扣。
- **长上下文是「过线整轮翻倍」，不是对超出部分加价。** 提示词超过
  `long_context_over`（OpenAI 现为 272000 input token）后，整个回合的四个桶都按
  `long` 那一档算。判定用的是「这轮喂进去多少」= 未命中缓存的输入 + 命中缓存的
  输入，两者相加正好还原 codex 报的 `input_tokens`；`cache_write` 不计入，它是否
  含在 `input_tokens` 里没实测过，而这家 provider 一直报 0。
- `quotas.balance_micro_usd` 是账本的物化缓存，两者永远同事务更新；对不上账用
  `store.RecomputeBalance` 按 `credit_ledger` 重算核对。
- 管理接口：`GET/PUT /api/users/{name}/quota`、`POST /api/users/{name}/credits`、
  `GET /api/usage`（普通用户只能看自己的）。前端在 `js/quota.js`：系统设置 → 用户
  管理每行的「额度」按钮开弹窗（余额、三种模式、充值、流水），侧栏给用户显示自己
  的剩余额度。**金额在前后端之间一律传微美元整数**，前端只在显示的最后一步除 1e6；
  别把美元浮点传回服务端，绕一圈会把分账算歪。充值按钮每次点击生成一个 `ref`
  幂等键，连点或重试不会重复入账。
- 报表页（按用户/模型/时段的消耗统计）还没做，`GET /api/usage` 已经能出汇总。
- **已知缺口：终端页不计费。** 用户在终端里直接敲 `claude`/`codex` 走的是容器内进程，
  输出直接进 PTY，不经过 `runTurn`，既不记用量也不扣额度。折中办法是**余额见底就
  不让用终端**（见下节），拦住入口而不是按量收费。要真按量算准，唯一能同时覆盖对话
  和终端的位置是中转站侧（账号池给容器注入 `base_url`，agent 流量必经那里），不是
  这一层能解决的。

### 终端

- `GET /api/sessions/{id}/term` 自动启动会话后 `docker exec` 进容器。
- 默认 attach 到持久 tmux 会话 `main`（老镜像回退 bash），断线重连不丢 shell/agent 状态。
- WebSocket 二进制帧是原始 PTY 字节；文本帧只接受 `{"type":"resize","cols":...,"rows":...}`。
- **额度见底的用户进不来**，口径与对话页一致（没额度行 = 不限额；只计不拦照常进）。
  两道关：进门前查一次（在 `startSession` 之前，欠费用户连容器都不拉起），以及每个
  ping 周期（30s）复查一次——余额是在对话页扣穿的，挂着的终端不会自己发现，不复查
  的话把标签页一直开着就绕过了入口检查。
- 拒绝用私有关闭码 `closeQuota`（4003）+ reason 送达，**不是** HTTP 错误：浏览器的
  WebSocket 拿不到升级失败的状态码和响应体，只会收到无原因的 1006，前端分不清是
  欠费还是网络抖动，于是无限退避重连。前端 `term.js` 见 4000–4999 就把 reason 写进
  终端画面并停止自动重连。控制帧上限 125 字节，reason 由 `truncReason` 按 rune 截断。
- 断开只停得住新的输入：tmux detach 不杀进程，已经在跑的 agent 会继续跑完。

### 文件/共享目录

- 默认操作为会话 `workspace`；`?scope=shared` 操作用户级共享目录（挂载到所有会话容器 `/shared`）。
- 上传支持普通文件与 `.zip/.tar.gz/.tgz/.tar`；解压经 `archivex` 做安全校验，且解压后统一 `ChownTree` 到 `1000:1000`。
- 文件路径必须过 `filepath.IsLocal` 校验；读取/保存只接受普通文件，避免符号链接逃逸。

### 变更审查（Git）

- `internal/server/git.go` 用**宿主机的 git** 直接操作会话 workspace（不进容器），
  统一带 `-c safe.directory=<ws>`：仓库属主是 uid 1000，服务端是 root。
- 写操作（commit / discard）后必须 `chownWorkspace` 把属主修回 1000:1000，
  否则 root 写下的 `.git` 对象会让容器内 agent 后续 git 操作失权。
- `discard` 是 `checkout HEAD -- <path>` + `clean -fd -- <path>`，破坏性操作，
  前端有二次确认。

### 附件与清理

- 粘贴图片/附件落在用户共享目录的 `.images/`、`.file/`，48 小时 TTL。
- `cleanExpiredImages` 删除前会扫描该用户全部线程转录（`chats/*.jsonl`）
  收集仍被引用的文件名并跳过它们——历史对话里的图片不该随时间烂掉。
  为省开销，只有存在过期候选时才读转录。

### 空闲休眠

- reaper 停机时写 `stop_reason="idle"`；用户手动停止与再次启动都会清空该字段。
- 前端据此把「休眠」与「已停止」分开展示，并在发消息时自动重连唤醒
  （chat WS 的 `startSession` 是幂等的）。

### 内网隧道

- 管理端开启 `tunnel.enabled` 后，`applyTunnel` 热启动/停止/重绑 SOCKS5 代理；绑定失败只记错误，不打垮主服务。
- abox-link 通过 `GET /api/tunnel` 拨入，服务端以 yamux client 维持连接；每个用户一条隧道、一份稳定 SOCKS secret。
- 容器不会得到全局 `HTTP_PROXY`；只在 exec env 注入 `AGENTBOX_INTRANET_PROXY` / `AGENTBOX_INTRANET_MAPS`，由 agent 按需使用。
- exec env 只到达 exec 出来的那个进程。终端附着的是常驻 tmux，已有会话的 shell 是更早的
  exec fork 出来的，所以 `termCommand` 在 attach 前用 `tmux set-environment -g` 把当前
  env 镜像进 tmux 全局环境（隧道变量缺失时 `-gu` 清除）——否则「会话先开、隧道后连」时
  终端里的 agent 永远看不到代理变量。运行中的窗格改不了，只能新开窗口或重启 agent。
- 端口映射按来源 IP 鉴权：只有属主用户自己的容器（和宿主机）能连映射端口。

## 代码约定与注意事项

- Go 版本见 `go.mod`：当前 `go 1.26.5`。提交前至少跑 `go build ./...` 与 `go test ./...`。
- 现有测试集中在 `internal/agent`、`internal/linkapp`、`internal/server`、`internal/store`、`internal/tunnel`；`dockerx`、`archivex`、`config` 等目前没有测试。改动这些包时优先补针对性测试。
- 配置变更是“副本上修改 → 校验 → 原子写盘 → 替换内存状态”的模式；不要绕过 `Config.mutate` 直接改字段。
- SQLite 加列走 `store.migrate()` 里的幂等 `ALTER TABLE`（重复执行时忽略
  `duplicate column name`）——`CREATE TABLE IF NOT EXISTS` 对已存在的库不生效。
- 前端不要用原生 `alert/confirm/prompt`：用 `util.js` 的 `askConfirm`/`askPrompt`
  （Promise 化的自定义对话框）或 `toast`。
- 令牌只在 GET 请求接受 `?token=`（WS 升级与下载直链需要）；写操作一律走
  `Authorization` 头，别在新接口上放宽这一点。
- API 增加路由时明确鉴权层级：公开、`s.auth`、`s.admin`、会话资源还必须套 `s.withSession` 做属主校验。
- 所有会话内文件/目录属主都要保持 `dockerx.AgentUID/AgentGID`（1000/1000），否则容器内 agent 用户可能写不了。
- 容器安全边界：非 root、`no-new-privileges`、内存/CPU/PID 限额、固定挂载 `/workspace`、`/home/agent`、`/shared`。不要轻率改挂载路径或容器用户。
- 前端无构建步骤：`internal/web/static` 原生 ES modules + 静态资源。服务端启动时算内容哈希，把 `index.html` 的 `{{BUILD}}` 替换为版本前缀；改前端不需要 npm build。
- 两套前端互相独立：主控制台在 `internal/web/static`（进 `agentbox`），abox-link 面板在
  `internal/linkapp/static`（进 `abox-link`）。改了面板要重跑 `scripts/build-clients.sh`
  才能让下载按钮发新版；主控制台不受影响，服务端也不用重启。
- 改 abox-link 面板前先读 `app.js`：它按 id 直接抓 DOM（`wire`、`wire-rules`、`st-title`、
  `card-pair`、`allow-list`、`savebar` 等），并自己拼类名（`$("wire").className = "wire " + cls`）。
  动 `index.html` 结构时这些 id 必须留着，样式也别挂在被 JS 覆写的类上，否则轮询下一轮就被抹掉。
- 面板颜色一律走 `style.css` 顶部的令牌，别写死色值——浅色主题（`prefers-color-scheme`）
  只覆盖会变的令牌。琥珀分两支：`--amber` 画线与文字（浅色下压深才有对比度），
  `--accent` 是实心块底色（两个主题下都要够亮以托住 `--on-accent` 的深色文字），与
  `internal/web/static/css/base.css` 的约定一致。
- 会话镜像内禁用 CLI 自升级（`DISABLE_AUTOUPDATER=1`）；Claude/Codex 版本由 `images/agent/Dockerfile` 与 `scripts/auto-update-image.sh` 管理。
- `config.json`、`accounts/`、`data/` 含密钥和运行时状态，已在 `.gitignore`；不要提交。
- 若改动影响用户可见行为、部署步骤、API 或配置字段，同步更新 `README.md`（必要时也更新 `deploy/README.md`）。
