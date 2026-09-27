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
| `cmd/agentbox/main.go` | 服务端入口与信号；`internal/app` 管理数据锁、启动与依赖清理。 |
| `cmd/abox-link/main.go` | 隧道客户端入口；面板模式与 `--server` 无头模式分流。 |
| `internal/app` | 启动编排、数据目录独占锁与依赖收尾。 |
| `internal/workspace` | 会话创建/启停/删除、模板与凭证播种、活动引用与空闲回收。 |
| `internal/credentials` | 账号凭证读取/保存、轮换同步、续期与账号级可取消锁。 |
| `internal/config` | 配置 schema、校验、运行时修改与原子写回。所有设置变更必须经 `Config.mutate`/`ApplySettings`/账号方法。 |
| `internal/server` | HTTP API、鉴权、用户/账号/设置、会话、文件、聊天 WS、终端 WS、隧道、账号出口代理（`proxy*.go`）、监控、空闲回收、凭证同步、Git 变更审查（`git.go`）、用量计量与额度（`usage.go`/`quota.go`/`usagelog.go`）。 |
| `internal/usage` | 回合用量归一化、定价快照、结算编排及 Claude/Codex 终端扫描。 |
| `internal/store` | SQLite(`data/state.db`)：sessions/users/tokens/usage_events/quotas/credit_ledger；首次打开会导入旧版 `state.json`。 |
| `internal/backup` | 版本化 tar.gz 备份、SQLite 在线快照、清单/哈希验证、恢复到新目录；CLI 在 cmd/agentbox/backup.go。 |
| `internal/safefs` | 基于 os.Root 的受限目录句柄、普通文件读取、原子写入及跨目录不覆盖重命名；详见 docs/architecture/filesystem-boundaries.md。 |
| `internal/gitx` | 网页 Git 的容器执行策略：argv、环境隔离、超时与窄执行接口；禁止宿主机 Git 降级。 |
| `internal/dockerx` | Docker Engine API 封装：容器生命周期、exec PTY/stream、stats、镜像/挂载检查。 |
| `internal/agent` | Claude/Codex 适配层：headless 命令、标题生成、凭证播种、Claude HUD、Codex app-server 协议。 |
| `internal/archivex` | 上传压缩包解压（防 zip-slip/符号链接/解压炸弹）与工作区 zip 下载。 |
| `internal/tunnel` | yamux 隧道协议、白名单、端口映射。 |
| `internal/linkapp` | abox-link 客户端实现：配置、面板、守护/自启、重连监督器；`static/` 是面板前端，随 `cmd/abox-link` 独立 `go:embed`。 |
| `internal/web` | 嵌入前端静态资源（`static/js` 是 TS 编译产物）；`AGENTBOX_WEB_DIR` 可改为磁盘热加载。 |
| `web/src` | 主控制台前端 TypeScript 源码，`npm run build` 编译到 `internal/web/static/js`。 |
| `images/agent` | 会话容器镜像 Dockerfile；内置 Claude Code、Codex CLI、tmux、claude-hud。 |
| `scripts` | 镜像构建/自动升级、abox-link 交叉编译、域名与账号登录辅助脚本。 |
| `deploy` | systemd 单元（服务、镜像更新、数据备份）、logrotate、安装/发布脚本、生产参数模板。 |

## 常用命令

```bash
# 基础校验
go build ./...
go test ./...
npm run check          # 前端类型检查（tsc --noEmit）

# 前端：改了 web/src/*.ts 必须重新构建，产物要一起提交
npm ci                 # 首次或依赖变动时
npm run build          # web/src/*.ts -> internal/web/static/js/*.js

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

# 前端热改：不嵌入，直接吃磁盘文件（配合 npm run watch 自动重新编译 TS）
AGENTBOX_WEB_DIR=internal/web/static ./agentbox -config config.json
npm run watch          # 另开一个终端；改完 .ts 刷新浏览器即可

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

发布前用 `systemctl show agentbox -p WorkingDirectory -p ExecStart` 核对实际布局。下文 `deploy.sh` 仅适用于服务直接运行在源码仓库内的旧布局；若启动路径是 `/opt/agentbox/current/agentbox`，使用独立发布目录与 `deploy/release.py` 的暂存、激活流程（见 `docs/architecture/deployment-layout.md`），不要为了适配源码目录重写现有 systemd 单元。

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

# 注意：同步的是 npm run build 的产物 internal/web/static/js/*.js，
# 不是 web/src/*.ts —— 生产机没有 node，不会自己编译。先在本地构建好。

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
生产机只跑 `go build`，不会编译 TypeScript——前端改动必须在本地 `npm run build`
并把 `internal/web/static/js/` 的产物一并提交，否则部署出去的还是旧脚本。

## 核心链路

### 会话生命周期

- 创建：`POST /api/sessions` 在 `data/users/<user>/sessions/<id>/` 下创建 `workspace` 与 `home`，并 `chown` 到容器用户 `1000:1000`。
- 启动：`startSession` 是幂等路径，REST 启动、聊天 WS、终端 WS 都会走它。流程：同步账号池 OAuth 轮换凭证 → `agent.SeedHomeTemplate` 铺 home 模板 → `agent.SeedCredentials` 播种 home → 可选播种内网提示 → 确保用户共享目录 → `dockerx.EnsureRunning` 复用/重建容器 → 更新 SQLite。
- home 模板：会话 home 每次都是全新空目录，skill / 用户级 MCP / rc 文件本来每开一个会话就得重装一遍，模板就是补这个。两层，后者盖前者：服务器级 `data/home-template/`（全体用户）、用户级 `data/users/<user>/home-template/`（该用户所有会话，「技能」页签写的就是它）。**两层先合并再落盘**，否则用户层里较旧的文件会输给服务器层。合并结果与会话副本之间逐文件按 mtime「谁新用谁」（同 credsync 的收敛规则）：容器里改过的留着，模板更新的推下去；符号链接原样重建、不跟随，可以把大块内容指向 `/shared`。必须排在 `SeedCredentials` 之前，模板里万一混进凭证文件也压不过账号池。模板失败只记日志，不挡会话启动。
- 官方市场（`internal/server/market.go`）：`anthropics/claude-plugins-official` 浅克隆到 `data/marketplace/repo`（12h 过期，整仓重克隆而非增量；拉不动就沿用旧副本），它既是目录数据源也直接提供一方插件的内容。目录条目的 `source` 有四种写法（仓库内相对路径 / git-subdir / url / github），`parsePluginSource` 归一化，路径与协议都当不可信输入校验。**市场的单位是插件不是技能**：`discoverSkillDirs` 按「显式 skills 声明 → skills/<名字>/ → 插件根就是技能」三级找，一个都没有就回 422 并让用户改用终端装整包。git 抓取由 `marketMu` 串行化，`GIT_TERMINAL_PROMPT=0` 防私有仓库卡在密码提示上。
- 技能页（`internal/server/skills.go` + `web/src/skills.ts`）：管理 `.claude/skills`，范围 `session`（会话 home）与 `template`（用户模板）。列表里的 `source` 靠探测两层模板里有没有同名目录得出，顺序与 SeedHomeTemplate 的分层一致。装／复制统一走 `replaceSkillDir`：整目录替换并把 mtime 戳成当下，保证刚进模板的技能一定比各会话里的旧副本新，下次启动推得下去。技能名同时是目录名，`skillNameRe` 卡死路径穿越。详情接口连整个技能目录的扁平清单（`entries`，含子目录，父在子前，上限 2000 条）一起返回，前端 `buildTree` 拼成左侧文件树；点开单个文件走 `GET …/skills/{name}/file?path=`，路径通过 `s.openDataDir` / `safefs.Root.OpenFile` 固定目录句柄并拒绝符号链接（技能目录在会话 home 里，容器内随手就能造一个指向宿主机文件的链接）。
- **账号 env 绝不烘进容器**（`dockerx.baseContainerEnv`）：容器 `Config.Env` 在 create 那一刻定死，之后只能靠 exec 往上加、减不掉。账号从中转站切回订阅登录时 `clearClaudeRelay` 只改得动 `config.json`，旧容器里那份 `ANTHROPIC_AUTH_TOKEN` 还在，而 claude CLI 认 env 里的 Bearer 令牌优先于 OAuth 凭证——订阅登录形同虚设，CLI 卡在重试里直到被杀（回合报「进程退出码 137」，stderr 只剩一句 connectors are disabled 的告警）。所以账号 env 一律走 `server.execEnv` 每次 exec 注入，加和减都即时生效。`EnsureRunning` 里的 `hasBakedEnv` 负责认出老版本烘过 env 的容器并重建（比键不比值，镜像升级改 `NODE_VERSION` 的值不算脏）。
- 停止/删除：只停/删容器；工作区、home、聊天线程仍在宿主机。`DELETE ?purge=1` 才删除会话目录。
- 重启服务端后：`Server.reconcile` 以 Docker 实际运行状态修正 session status。
- **OAuth 令牌服务端自动续期**（`internal/credentials/refresh.go`）：访问令牌只有几小时
  寿命，账号池那份新不新鲜取决于容器里的 CLI 最近跑没跑过——挂一夜的账号第二天点
  「查额度」必然过期。服务端自己拿刷新令牌续，不再让用户「先发一轮对话」。刷新令牌
  是轮换制（一份用掉另一份作废），所以 `ensureClaudeCred` 里的三步顺序不能动：
  ① **先跑一遍 credSync 把各会话 home 里可能更新的凭证收回池子**——CLI 刚续过的话池子
  那份已经是废纸，拿它去换只会白挨一个 `invalid_grant`，而正确答案就躺在会话 home 里；
  ② 收敛完再判断要不要续；③ **续完立刻反向播发**，不等 credSyncLoop 那趟 45 秒的兜底
  ——空窗期里 CLI 手上还是老链条，一发对话就掉登录。同一账号全程一把锁串行，提前量
  （`credRefreshSkew`）刻意只有一分钟：提前得越多越容易和正在跑的 CLI 抢同一个刷新
  令牌。写回凭证走「读旧的 → 改字段 → 写回」而不是整份重建，`subscriptionType` /
  `rateLimitTier` / `scopes` 这些刷新响应里不回的字段必须留着，冲掉会让容器里的
  Claude Code 把订阅号当成 API 账号。查额度撞上 401 时还会强制续一次重打——凭证里的
  `expiresAt` 不是唯一真相。

关键目录：

```text
data/
  state.db
  home-template/   -> 叠加到每个会话 home（skill / MCP / rc 文件）
  users/<user>/
    home-template/ -> 只叠加到该用户的会话，盖住上面那层
    shared/
    sessions/<id>/
      workspace/   -> 容器 /workspace
      home/        -> 容器 /home/agent
      chats/<tid>.jsonl
```

### 账号使用授权

- `config.Account.Access` 缺省兼容全体共享；`mode=all/users/admin`，管理员始终可用。指定用户按用户名匹配，HTTP 编辑校验用户存在。
- `sessionAccount` 按空间属主的当前角色判断；创建/启动/聊天/终端/Git/额度查询都需校验。`execEnv` 返回错误时必须停止执行，不能丢掉账号 env 后继续调用 Docker。
- `syncRotatingCred` 重新读取当前账号授权，撤权空间不得再参与双向凭证同步；保持同步→刷新→立即播发的原顺序。
- 授权名单仅发给管理员；普通用户的账号列表过滤范围，空间计数只含本人。文件/历史/停止/删除仍按空间属主授权。
- 撤权是准入控制，不能撤回已交付凭证或杀死 tmux；终端输入/30 秒心跳复查，关闭码 4004。旧二进制忽略授权字段，不能无条件回退。详见 `docs/accounts-and-models.md`。

### 默认模型

- `config.default_models` 按 `claude` / `codex` 配置，初始为 `claude-opus-5` / `gpt-5.5`。
  `Config.mutate`、持久化结构、设置 API 必须一起保留此字段；更改默认模型不改已有空间。
- 创建时将默认模型保存到 `sessions.default_model`；老空间在新版服务首次启动时补齐一次。
  对话请求未指定模型时，服务端用空间保存的值，前端也用同一字段初始化并显示具体名称。
- `SeedDefaultModel` 必须排在 `SeedCredentials` 后，把空间默认模型写进 CLI 配置（含 Codex
  当前启用的 profile）；否则账号池的 config 会覆盖它。只改模型，保留 provider、推理强度、MCP。

### 聊天

- `GET /api/sessions/{id}/chat` 是一个会话一个 room 的广播模型；同一房间同一时刻只跑一个回合。
- 消息落盘到当前线程 `chats/<threadID>.jsonl`；`chats/active` 指向当前线程。旧版 `chat.jsonl` 首次访问自动迁移。
- Claude 走 `claude -p --output-format stream-json`；Codex 优先走 `codex app-server`（真流式增量），握手失败回退 `codex exec --json`。
- `stream_event`/增量事件只广播不落盘；完整事件落盘并广播。provider 会话 id 用 `agent.ExtractSessionID` 提取，写入 `chat_session` 供 `--resume`/thread resume。
- 用户中断：Codex app-server 优先协议内 `turn/interrupt`，否则用容器里的 PID 文件发 SIGINT。

### 用量计量

- 回合收尾事件里的 token/费用落进 `usage_events` 表（`internal/usage/events.go` 解析，
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
- `ttft_ms` 是**我们自己掐的表**，provider 不报：`runTurn` 在 `startSession` 之后开表，
  第一个模型输出事件（`isOutputEvent`）停表。判定必须排除会话初始化事件（claude 的
  `system`/init、codex 的 `thread.started`）——它们在模型被调用前就发出来了，认了会把
  首字延迟量成一个恒定的小数字。掐表要在 `IsPartialEvent` 的早返回**之前**，最早的
  输出往往就是个增量事件，放后面量到的是「整段话说完」。
- `provider` 取自 claude `modelUsage` 里的同名字段（实测 `firstParty`），只用于在使用
  记录里区分官方直连与中转，别让别的逻辑依赖它——codex 根本不报这个字段。
- **耗时有两块表，别混用**：`wall_ms` 是我们量的（`turnStart` → 回合收尾，与 `ttft_ms`
  同源，含 CLI 启动），`duration_ms` 是 provider 自报的模型侧耗时（**不含** CLI 启动）。
  实测容器里跑一趟 haiku：墙钟 4534ms，claude 自报 `duration_ms` 2335ms，差的 2.2 秒
  全是 Claude Code 自己的启动。`wall_ms` 由 `flushUsage` 在回合真结束的那一刻统一盖到
  各行上（回合中途量不到）。使用记录页的「延迟」列显示**首字 + 总耗时**，两个数同源
  （都是我们量的），所以首字必然 ≤ 总耗时；「总耗时」必须用 `wall_ms`——早先拿
  `duration_ms` 当总耗时，出过「首字 3.1s / 总耗时 2.3s」这种看着不可能的记录。老数据
  没量过墙钟（`wall_ms == 0`），那种行退回显示 `duration_ms` 并把标签换成「模型」，
  不能顶着「总耗时」的名字混口径。`duration_ms` 平时不上表，但仍在记、仍进 CSV 导出。
- `duration_ms` / `wall_ms` / `ttft_ms` 都是**回合级**指标，在同回合拆出的各行上重复；
  聚合时只能按 `turn_id` 取一份，绝不能 SUM。`store.UsageTotals` 因此故意不含这几项。

### 价格目录维护

- `internal/pricecatalog` 管理独立、版本化 JSON 候选与缓存；拉取不修改生效价格。旧前端快照迁入 `catalog.json`，明确未重新核验，不能填虚假的核验时间。远程目录要求四项显式单价、HTTPS 来源与核验时间；维护流程见 `docs/pricing-catalog.md`。
- 管理接口 `/api/pricing`、`/check`、`/apply`、`/restore` 均为 admin；应用/编辑/回退带修订号防并发覆盖。旧配置行默认自定义；手动改价转自定义，目录应用不能静默覆盖自定义或删除消失模型。
- `pricing_catalog`、`pricing_managed`、`pricing_history` 必须一起进入 Config 的 mutate/persist；价格历史最多 10 次，回退不改历史用量或目录地址。后台每日检查通过 server 生命周期运行，默认不联网、不自动应用。
- 网页回合与起标题用 `usage.Service.NewTally()` 在 CLI 调用前锁定整张表，Flush 不得重新读新价；终端仍首次入账锁定。用量 JSON 快照增补修订与目录来源，schema 无需加列。

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
- **claude 对话行的价以 provider 报的为准**；不报价的走 `config.json` 的 `pricing`
  按 token 折算——codex 的全部回合，以及从 transcript 补记的**终端行**（那里只有
  token 没有美元，claude 也一样要查表）。价目表单位是「每百万 token 多少美元」，
  微美元成本正好等于 `tokens × rate`（两个 1e6 约掉）。查表顺序：**精确模型名 →
  去掉 `-YYYYMMDD` 日期后缀再查 → agent 名兜底**（codex 事件不报模型名，兜底那条要
  配成账号 `config.toml` 里的默认模型）。日期回退是必须的：provider 会报
  `claude-haiku-4-5-20251001`，而价目表里配的是系列名。**没配价目表的模型按 0 计**，
  只记不扣。
- 价目表在「系统设置 → 价目表」里编辑（`web/src/pricing.ts` + `SettingsPatch.Pricing`），
  **整表提交**而不是逐键合并——删行没法用增量表达。`sanitizePricing` 卡住负数、NaN、
  离谱大的单价（手滑多打几个零会一次扣穿余额）、非法键，以及「配了长上下文单价却
  没有阈值」这种安静失效的组合。
- **Claude 的 `cache_write` 取 1 小时档**（= 2× 输入价）而不是 5 分钟档（1.25×）：
  Claude Code 实际用的就是 1h 缓存，实测 transcript 里
  `cache_creation.ephemeral_1h_input_tokens` 有值、`ephemeral_5m_input_tokens` 是 0。
  我们的用量只有一个「缓存写入」桶，两档合不了，只能取实际用的那档。
- Claude 4.6 及之后的模型 1M 上下文按标准价计费，所以 claude 各行**都不配长上下文档**。
- Claude 0 费用按表补算时，新记录保存 table 来源；旧记录没有价格快照，仍只能按旧规则推断来源。不要把事后参考价当作历史入账价格。
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

### 使用记录（流水明细）

`internal/server/usagelog.go` + `web/src/usage.ts`，侧栏「使用记录」，管理员与普通
用户都有入口。`GET /api/usage` 出的是聚合，这里 `GET /api/usage/events` 出的是一行行
明细，带筛选、翻页、合计与筛选可选值。

- **一行 = 一个回合 × 一个模型，不是一次 API 调用。** 容器里的 CLI 直连 provider，
  我们不在链路上，看不见单次 HTTP 请求。中转站面板上的「端点 / API 密钥 / 分组 / IP」
  我们没有对应物，别为了凑齐列去编。想要单请求粒度只能自己做反代，那是另一件事。
  （顺带：claude 的 `assistant` 事件确实带每次调用的 usage，但其中 `output_tokens`
  是流式开始时的占位值、永远不更新，**算不了钱**——所以粒度只能停在回合。）
- **合计与筛选可选值都按「筛选条件」算，不按「当前这一页」算。** 否则翻页时表头的
  总花费跟着变，没法用。三者共用 `UsageFilter.where()`，不会各算各的。
- **可见范围与筛选项是两回事。** `scopeUser` 是硬边界（普通用户只能是自己），`f.User`
  是用户自己选的筛选项。`FacetsUsage` 会为了「选了还能改回来」把每列自己的过滤放宽，
  放宽时必须重新按 `scopeUser` 收口——否则普通用户能从用户下拉里读到全部用户名。
- 一页上限 `usageRowsMax`，这个接口没有游标、全靠 OFFSET 翻页，放开上限等于允许一次
  拖走整张表。CSV 导出走的也是这个上限，超出会提示用户缩小时间范围分批导。
- 使用记录与全站时间统一走 `config.timezone`（默认 `Asia/Shanghai`）。筛选框提交的是不带
  offset 的墙上时间，必须由服务端用该 IANA 时区 `ParseInLocation`；不要在浏览器里用
  `new Date(localValue).toISOString()`，那会偷偷套用访问者电脑的时区。同一时区也从 `/me`
  下发给普通用户，并写入明细接口响应，表格、快捷日期与 CSV 必须保持同一口径。
- 前端筛选条整条可折叠，首次进入桌面默认展开、窄屏默认收起（手动切换后按浏览器记忆）。时间区间由
  `web/src/date-range.ts` 的独立组件管理，使用记录默认与重置均选系统时区的当天；
  当天/昨日快捷范围在取值时重新按系统时区计算，避免模块先于 `/me` 加载或跨天导致日期过期。
  popover 里左侧编辑日期/时分，右侧选日历，
  **草稿与已应用值分离**，只有确定才更新查询。时/分仍用 number + min/max，避免
  原生 time 控件在 0/23 之间绕回；留空补起始 00:00、截止 23:59。组件输出系统时区的
  墙上时间，`usage.ts` 查询时给 until 加一分钟以适配服务端开区间。
  「跟随当前时刻」在页面可见时每 30 秒刷新；相对天数快捷范围滚动起止两端，自定义
  范围只更新截止。日历的键盘移动和月份计算使用 UTC 做日历运算，不套浏览器时区。
  页面由筛选/合计、独立滚动的明细、常驻底部分页组成，不能把滚动放回 `.usage-body`。
  默认每页 20 条，可选 50 / 100 条；翻页/改筛选重置列表滚动位置，自动轮询保留位置。
  页大小只影响明细请求，合计与 CSV 仍按整个筛选范围计算。
  计费方式并入费用列；窄屏明细纵向展示，字段名来自 `<td>` 的 `data-l`。
- **每行「费用」旁的 `?` 是费用明细弹窗**（`usage.ts` 的 `openCost` + `dlg-cost`）：把
  四个 token 桶各自的 `token × 单价` 摊开，末尾对上实收金额，脚注写清计价算法。单价
  由服务端随行下发（`usageRowView.Rate` ← `rateFor` ← `config.PriceLookup`，顺带给出
  命中的键与档位），**别改成前端自己查价目表**——普通用户根本拿不到 `settings`。两件事
  不能含糊：① 新行使用入账价格快照（rate.snapshot=true），旧行才回退当前参考价并明确提示；② claude 对话行的钱是 provider 自报的一个总额、
  拆不出分项，这类行的 `basis` 是 `reference`，表里那份只是照价目表推的参考值。
- **终端消耗靠事后扫 transcript 补记**（`internal/usage/terminal.go`，`kind=terminal`）。
  终端里的 CLI 是容器内进程、输出直接进 PTY，`runTurn` 看不见；但 Claude Code 把完整
  记录落在 `<会话home>/.claude/projects/<cwd目录>/<provider会话id>.jsonl`，而会话 home
  是宿主机 bind mount，所以读文件就够，不用 hook、不用反代、不用进容器。要点：
  - **`entrypoint` 是分水岭**：`cli` = 用户手敲的 TUI（要补），`sdk-cli` = 我们自己发的
    `claude -p`（已记账，漏掉这个过滤就是对话消耗记两遍）。
  - transcript 里的 `usage` 是**最终值**，不是流式 `assistant` 事件里那个恒为 2 的
    `output_tokens` 占位——所以这条路其实看得见单次 API 调用，但仍按回合聚合落库，
    好让两种来源的行粒度一致。回合边界用最近一条 `user` 记录的 `uuid`（老版本 CLI 的
    `promptId` 是 null，不能用）。
  - 同一个 `requestId` 会在文件里出现多次（流式中途一遍、收尾一遍）：**用量取最后一份，
    时间取最早一份**，累加会把一次调用算两遍。
  - 去重靠 `usage_events.req_id`（回合首个 requestId）上的**部分唯一索引**
    （`WHERE req_id != ''`，对话行的空串必须排除）。`UpsertTerminalUsage` 的
    `ON CONFLICT` 必须把这个 WHERE 原样带上，否则 SQLite 认不出这条约束。upsert 而非
    insert 是因为扫描时回合可能还没结束，下一轮要把新调用更新到同一行上。
  - **只记账不扣额度**：补记是异步的、价还是我们查表折的，拿它动余额用户没法对账。
    拦终端仍然靠余额见底不让开终端（见下节）。`billingMode` 因此对 `terminal` 特判——
    哪怕是 claude 也只能标「价目表 / 未定价」，不能标「官方报价」。
  - 扫描按 (size, mtime) 跳过没动过的文件；只遍历库里存在的会话，磁盘上已删除会话的
    残留目录不补（补了也是归不到人头上的孤儿行）。
  - 触发有三条路，都汇到 `usage.Service` 的扫描器（扫描进度上锁，同一时刻只有一趟在跑）：
    **inotify**（`termWatchLoop`，主力，实测写入到落库 ~700ms）、`termUsageLoop`
    每分钟的全量兜底、以及 `handleUsageEvents` 进来时先补一趟。
  - **inotify 能收到容器里的写入**：会话 home 是 bind mount，容器与宿主机是同一个
    inode，写操作走同一个内核 VFS（实测 CREATE/WRITE/REMOVE 都到）。监听的是
    `projects/` 及其各 cwd 子目录，`syncTermWatches` 每 30 秒与库里的会话对齐一次；
    watch 加不上（`fs.inotify.max_user_watches` 有上限）只记日志，定时那趟仍然覆盖。
  - 写入攒 `termWatchSettle` 再扫，而且只在一波的**第一下**开表——每次事件都重置的话，
    一个持续写文件的长回合会把补记无限推迟。
  - **延迟压得小但消不掉**：终端流量不经过服务端，我们永远是在读 CLI 事后写的文件。
    回合进行中扫到的是半截，upsert 保证下一轮补齐同一行。
- 终端页顶栏的「本会话已花」（`term.ts` 的 `termSpendPolling`）直接复用
  `GET /api/usage/events?session=<id>&limit=1` 的 `total`，没有单独的接口——那个 total
  本来就是「按筛选条件算、与翻页无关」，正好是这个数。只在终端页轮询（15 秒）。
- Codex 终端补记见 `internal/usage/codex_terminal.go`：仅 codex-tui 来源，按 thread/turn/model 聚合；累计 token 差值去掉重复 token_count，reasoning 不重复加，缓存输入是 input 子集。数据库 req_id 加空间前缀，防止复制 rollout 跨空间覆盖；仍只记账不扣额度。

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
- 上传支持普通文件与 `.zip/.tar.gz/.tgz/.tar`；一律在容器挂载外的 staging 完整验证，
  `archivex.ExtractRoot` 限制解压量并拒绝路径逃逸，再用目录句柄合并；属主通过
  `ChownRoot` 调整为 1000:1000。合并不是跨目录事务，遇到冲突/磁盘错误可能部分完成。
- 文件路径必须经 `safefs` 目录句柄访问；禁止恢复「Lstat 校验后返回绝对路径再调用
  os.Open/WriteFile」的写法。读取只接受普通文件，保存通过随机临时文件原子替换，
  保留权限位、避免覆盖原硬链接 inode。边界与已迁移入口见
  `docs/architecture/filesystem-boundaries.md`。

### 变更审查（Git）

- `internal/server/git.go` 负责 HTTP 与仓库选择；`internal/gitx` 通过 `dockerx.ExecCommand`
  在会话容器里以 uid/gid 1000:1000 执行 Git。**禁止回退宿主机 Git**：仓库 hook、
  过滤器等是用户代码，曾确认宿主机 root 的 commit 会执行用户仓库 hook。
- Git 请求通过 `prepareGitSession` 按需启动空间，先检查与终端一致的额度拦截，
  并持有活动引用避免空闲回收。文件查看/下载仍可独立使用。
- Git 命令由 argv 传入，容器内用 `timeout` 限时 15 秒、再给 2 秒终止宽限；Docker
  exec 断连不会杀进程，不能只依靠请求 context。stdout 上限 4 MiB，超限明确报错。
- 网页 Git 使用清理后的环境，不注入账号/代理凭证；禁用 hook、fsmonitor、签名和
  自动维护，需要 hook/签名时用户可在终端操作。容器里的已有凭证仍可被用户代码读取。
- **必须挡住 git 的向上仓库发现**：`data_dir` 通常就在服务端自己的 checkout 里
  （默认相对路径 `data`），workspace 自己没有 `.git` 时 `git -C <ws>` 会一路向上找到
  **服务端仓库**——曾经的表现是每个会话的「变更」页都显示 agentbox 自己的改动，
  而「提交」会把服务端仓库整棵工作树 `add -A` 进去。`.gitignore` 里的 `/data/` 挡不住，
  ignore 只管文件跟不跟踪，不管仓库发现。两道防线：`repoRoots` 只认真实存在 `.git`
  的目录，`gitx` 再用容器内的 `GIT_CEILING_DIRECTORIES`（**必须绝对路径**）
  把发现范围钉死在目标目录。
- **工作区根几乎从来不是仓库**，项目一般 clone/解压在子目录里，所以 `repoRoots` 往下
  找两层（跳过隐藏目录与 `node_modules`/`__MACOSX`/`vendor`，符号链接目录不跟随），
  返回相对 workspace 的斜杠路径列表（`""` = workspace 本身），多个时前端下拉切换。
  接口的 `repo` 参数只接受这个列表里的值：**写操作（commit/discard）遇到不认识的
  `repo` 一律报错，不能回落到别的仓库**——用户选的是 A，别把 B 给提交了。
  status 是读操作，可以回落到第一个并把权威列表带回去让前端重新对齐。
- 变更列表与 `?path=` 都是**相对仓库根**，不是相对 workspace。
- 右侧支持「差异 / 完整内容」两种视图：新文件（`??`）相对 HEAD 根本没有 diff，
  只能走 `git/file` 读工作树里的内容，所以选中新文件时默认就是完整内容视图，
  「差异」按钮置灰；删除的文件反过来（没内容可读，只有 diff）。`git/file` 通过
  `safefs` 固定目录句柄读取普通文件，并按 `maxFileViewBytes` 拒绝大文件、
  按 NUL/非 UTF-8 拒绝二进制——它渲染成一个个 DOM 行，不能由着文件大小来。
- `status` 必须带 `--untracked-files=all`：默认口径会把整个未跟踪目录折叠成一条
  `dir/`，用户看到的是「.claude/」而不是里面那个新文件，单文件的 diff 和丢弃都无从下手。
  代价是未跟踪的大目录（没 gitignore 的 node_modules）会撑爆列表，所以服务端按
  `maxStatusFiles` 截断并回 `truncated`。
- Git 全局配置隔离使用 `env -i`、`GIT_CONFIG_GLOBAL=/dev/null`、
  `GIT_CONFIG_NOSYSTEM=1` 和 `-c core.excludesFile=/dev/null`。仓库自己的 `.gitignore`
  与 `.git/info/exclude` 照常生效；`GIT_LITERAL_PATHSPECS=1` 保证文件名不被当成通配表达式。
- 写操作由容器用户执行，不再用宿主机递归 chown 修补属主。无 HEAD 的新仓库 diff
  使用 `--cached`；Docker、启动等其他错误必须向用户报告，不能回空 diff 掩盖。
- `discard` 先用 `ls-files` 判断是否含已跟踪文件，需要时执行 `checkout HEAD -- <path>`，
  成功后才 `clean -fd -- <path>`；仅未跟踪文件跳过 checkout。破坏性操作，
  前端有二次确认。

### Git 远程连接

- 用户菜单统一进入独立 Git 管理页（`#/git/guide|profile|connections`）。`git-management.ts` 管分区与说明，`git-surface.ts` 管页内详情及清理；工作空间的远程操作仍留在空间内。切换分区、离页或退出登录时清理表单和记录轮询；身份、连接异步响应需校验登录 token 与挂载状态。

- 用户私有 HTTPS 连接独立于 Agent 账号池；入口 `/api/git/connections`，账号与绑定持久化在 SQLite schema 5。创建空间可选 `git_connection_id`，省略采用用户默认，空串表示不绑定。
- Token 由 `internal/gitaccess.Vault` 加密保存；主密钥在 `data/git-secrets/master.key`，不可放进会话挂载或模板。系统备份/恢复必须验证密文能由配套密钥解开，不可把缺密钥当成自动生成新密钥的机会。
- Git 执行留在容器；HTTPS smart HTTP 通过 `gitaccess.Grant` 按次转发，长效 Token 不交付容器。只读仅 upload-pack；写 grant 校验 old/new/ref，禁止多 ref/删除，且每次请求重查连接修订和启用状态。
- `push-preview` 与 `push` 分开；推送必须核对预览的分支/本地 SHA/远端 SHA，并检查快进关系。`pull` 只快进、要求工作区干净；不自动 stash/rebase。绑定 URL 改变需重新绑定，不能按域名轮试账号。
- 当前网桥复用 proxy_bridge 主机配置并分配临时端口；network 指定直连/用户隧道和专用 CA，绝不回落其他路由。SSH 使用服务端 Go SSH + 固定主机公钥，长效私钥不交付容器；native Git grant 继续约束 push old/new/ref。完整进展见 `docs/architecture/git-management.md`；本机 Git smart HTTP fixture 不等于 Linux Docker 验收。

- Git OAuth 应用在 config.git_oauth_apps，经 mutate/persist 同步保存；secret 为 Vault 密文，不能通过 GET 下发。state/PKCE/verifier/Cookie/原登录 token 短时绑定，回调不能用查询参数指定用户。刷新按连接串行，重新授权需原应用/修订且保留连接 ID；撤销区分本地停用与上游成功。
- Git 审计 schema 6 增加 finished_at，重启残留 running 标记 interrupted_unknown；历史未量过的结束时间留空。活动查询/取消按 actor 硬隔离。Docker Git 使用 python3 -I 监督进程组，取消先 EOF，再等待 TERM/KILL 收尾；不能以网络断开证明远端回滚。

- Git network 是连接/OAuth 应用不可变身份，schema 7 保存并加入密文认证数据；备份必须按 schema 兼容验证，不能用新 AAD 去解旧无 network 的密文。公司 CA 只影响该连接的 TLS trust，不能关闭域名/证书验证。OAuth 交换/续期/撤销使用授权用户自己的路由。

- 分支动作必须核对 expected_head/expected_branch/target_head，创建/切换要求工作区干净；删除先检查目标是当前 HEAD 祖先，再用 branch -d，禁止 force 删除或自动 stash/rebase。分支列表取 for-each-ref，最多 500 条，纯本地不 fetch。

- PR/MR 客户端只根据已绑定仓库构造 GitHub/GitLab API URL，不跟随 redirect；查询/预览不创建，确认再 POST，未知结果不得自动重试。GitLab API scope 由用户单独勾选，SSH 需同平台 HTTPS API 连接。当前只支持同仓库分支，服务端重新核对 HEAD/目标 SHA 和重复请求。
- Git 共享 ACL（schema 8）仅管理员自己的 PAT/SSH，可按用户只读/写；OAuth 私有。使用入口走 GitConnectionFor，管理仍走所有者 GitConnection；不得因为 admin 角色绕过私有账号。共享执行 actor 和凭证 owner 分开，路由/审计按 actor，AEAD 身份按 owner。撤权增修订、清默认但保留失效绑定；删除用户清名单，重建同名不继承。

### 附件与清理

- 粘贴图片/附件落在用户共享目录的 `.images/`、`.file/`，48 小时 TTL。
- `cleanExpiredImages` 删除前会扫描该用户全部线程转录（`chats/*.jsonl`）
  收集仍被引用的文件名并跳过它们——历史对话里的图片不该随时间烂掉。
  为省开销，只有存在过期候选时才读转录。

### 空闲休眠

- reaper 停机时写 `stop_reason="idle"`；用户手动停止与再次启动都会清空该字段。
- 前端据此把「休眠」与「已停止」分开展示，并在发消息时自动重连唤醒
  （chat WS 的 `startSession` 是幂等的）。

### 账号出口 IP 代理

`internal/config`（代理池 schema + 账号 `proxy_id`）+ `internal/server/proxydial.go`
（socks5 / http CONNECT 拨号）+ `proxybridge.go`（容器侧桥接与 env 注入）+
`proxies.go`（管理接口）。前端在 `web/src/proxies.ts`。

- **为什么要有本地桥接，而不是把 socks5:// 直接塞给容器。** 代理池基本都是 SOCKS5，
  但容器里的 claude 是 Node/undici，`HTTPS_PROXY` 只认 http(s)——给它 socks5:// 它
  **不报错、直接忽略**，照原样打官方接口。表现是「配了代理但 IP 没换」，且没有任何
  日志。codex 是 Rust reqwest 认 socks5，两个 CLI 行为不一致。所以服务端在网桥网关
  上起一个普通 HTTP 代理（`proxy_bridge.bind`，默认 `172.17.0.1:1081`），容器只说
  HTTP 代理协议，SOCKS5 那段由 `dialThrough` 走完。
- **注入的是全局 `HTTP(S)_PROXY`，且大小写各一份。** curl 只认小写 `http_proxy`，
  Node/Go 读大写；只给一半的后果是部分请求悄悄走了服务器自己的 IP，而不是报错。
  变量名列在 `proxyEnvNames`，`tmuxEnvSync` 靠它在解绑后清除终端里的残留值——
  加变量必须同步这个列表。这与隧道**刻意不设全局代理**的取向相反：隧道是「按需访问
  内网」，这里是「这个账号的一切请求都必须从这个 IP 出去」。
- **全链路 fail-closed。** 代理停用 / 悬空 / 桥接没起来时，`proxyEnvList` 照样注入、
  `bridgeAuth` 返回 502、`acctClient` 直接报错。**不要改成回落直连**：直连等于把
  服务器真实 IP 交给 provider，正是绑代理要防的事，而且是静默发生的。
- 桥接的账号口令是 `auth_token` 对账号 ID 的 HMAC（`proxySecret`），不落盘。没有它，
  同一台机器上任何容器都能白嫖别人账号的出口 IP。轮换 `auth_token` 会一起换掉。
- 服务端自己发的官方请求（OAuth 换令牌、profile、Key 探测）走 `acctClient`，
  与容器同一个出口。登录来源 IP 和推理请求 IP 对不上是订阅号被风控的典型形状。
- 域名一律在代理侧解析（socks5h 语义 / CONNECT 请求行），不在服务端本地解析——
  否则 DNS 查询会从服务器自己的解析器漏出去。
- 删代理会连带解绑账号，且在**同一次 mutate 里**完成：留下悬空的 `proxy_id` 会让之后
  任何一次配置写入都卡在校验上，那时已经看不出是哪一步埋的。

### 内网隧道

- `tunnel.transparent` 未指定时默认 true，JSON 必须保留显式 false（不能 omitempty），保证兼容模式保存后不会重载成透明模式；客户端同样默认 true，CLI 用 `--transparent=false` 选择兼容。总开关 disabled 时不启动辅助网络。
- `tunnel.transparent` 启用工作空间透明 IPv4/TCP：`internal/netaccess` 管理规则、虚拟 DNS 与辅助进程，`internal/server/netaccess.go` 管理按用户策略和按空间认证的控制端口，`internal/dockerx/netaccess.go` 管理无用户目录挂载的网络辅助容器。仅辅助容器有 NET_ADMIN，Agent 仍为 UID 1000。
- 客户端 `--transparent` / 面板开关通过能力协商上报规范化放行规则。透明授权仍由客户端白名单最终检查；账号 HTTP 桥接必须按实际工作空间归属选择隧道，不能仅按共享账号分流。
- `data/users/<user>/network.json` 保留历史捕获目标与不复用的域名虚拟 IP。掉线、规则撤销、服务重启不能删除捕获规则后静默直连。透明模式退出前停止空间，让网络命名空间重建；不要热清空规则。
- 透明模式不依赖说明文件或专用代理 env；旧客户端仍保留兼容代理。配置、部署和验收见 `docs/architecture/transparent-network.md`、`docs/networking.md`。真实合成容器测试用 `scripts/test-transparent-network.py`。

- 管理端开启 `tunnel.enabled` 后，`applyTunnel` 热启动/停止/重绑 SOCKS5 代理；绑定失败只记错误，不打垮主服务。
- abox-link 通过 `GET /api/tunnel` 拨入，服务端以 yamux client 维持连接；每个用户一条隧道、一份稳定 SOCKS secret。
- 容器不会得到全局 `HTTP_PROXY`；只在 exec env 注入 `AGENTBOX_INTRANET_PROXY` / `AGENTBOX_INTRANET_MAPS`，由 agent 按需使用。
- exec env 只到达 exec 出来的那个进程。终端附着的是常驻 tmux，已有会话的 shell 是更早的
  exec fork 出来的，所以 `termCommand` 在 attach 前用 `tmux set-environment -g` 把当前
  env 镜像进 tmux 全局环境（隧道变量缺失时 `-gu` 清除）——否则「会话先开、隧道后连」时
  终端里的 agent 永远看不到代理变量。运行中的窗格改不了，只能新开窗口或重启 agent。
- 端口映射按来源 IP 鉴权：只有属主用户自己的容器（和宿主机）能连映射端口。

## 代码约定与注意事项

- Go 版本见 `go.mod`：当前 `go 1.26.6`。提交前至少跑 `go build ./...` 与 `go test ./...`。
  这个补丁号不是随手写的：CI 的 govulncheck 用 `go-version-file: go.mod` 决定用哪个
  工具链，stdlib 漏洞（`crypto/tls`、`net/http`、`net/url`、`encoding/asn1` 那批）只能靠
  抬这一行修——它们没法进豁免清单。宿主上的 go 比这行旧时，`go build` 会按
  `GOTOOLCHAIN=auto` 自动下载对应工具链，所以生产机上不必手动升级 `/usr/local/go`，
  但机器得能连 proxy.golang.org。
- 现有测试集中在 `internal/agent`、`internal/linkapp`、`internal/server`、`internal/store`、`internal/tunnel`；`dockerx`、`config`、`gitx`、`archivex`、`safefs` 也有测试。文件安全改动须跑链接替换、归档与播种回归。
- 配置变更是“副本上修改 → 校验 → 原子写盘 → 替换内存状态”的模式；不要绕过 `Config.mutate` 直接改字段。
- SQLite 变更走 `internal/store/migrations.go` 的连续版本迁移与 migrations/*.sql，事务内更新 user_version；先拒绝高版本，再迁移，不能再靠忽略 duplicate column 错误补列。旧未版本化库由基线迁移检查列后补齐。
- 前端不要用原生 `alert/confirm/prompt`：用 `util.js` 的 `askConfirm`/`askPrompt`
  （Promise 化的自定义对话框）或 `toast`。
- 令牌只在 GET 请求接受 `?token=`（WS 升级与下载直链需要）；写操作一律走
  `Authorization` 头，别在新接口上放宽这一点。
- API 增加路由时明确鉴权层级：公开、`s.auth`、`s.admin`、会话资源还必须套 `s.withSession` 做属主校验。
- 所有会话内文件/目录属主都要保持 `dockerx.AgentUID/AgentGID`（1000/1000），否则容器内 agent 用户可能写不了。
- 容器安全边界：非 root、`no-new-privileges`、内存/CPU/PID 限额、固定挂载 `/workspace`、`/home/agent`、`/shared`。不要轻率改挂载路径或容器用户。
- 主控制台前端是 TypeScript：源码在 `web/src/*.ts`，`npm run build`（tsc，无打包器）
  逐文件编译成 `internal/web/static/js/*.js`，产物提交进 git 并被 `go:embed` 吃进二进制。
  **改了 `.ts` 一定要重新 `npm run build` 并提交产物**，CI 会校验两者一致。
  刻意不打包：服务端启动时算内容哈希，把 `index.html` 的 `{{BUILD}}` 替换成 `/_v/<hash>/`
  前缀，其余模块靠原生 ES Module 的相对 import 继承该前缀（详见 `server.go` 的
  `staticHandler`），一个 .ts 对一个 .js 才能维持这套长缓存。
- 单选下拉统一走 `web/src/select.ts` + `css/select.css`：入口 `enhanceSelects()` 增强现有 `<select>`，原元素继续提供表单值与 `input/change` 事件。动态插入控件后调用 `enhanceSelects(root)`；代码赋值用 `setSelectValue(select, value)`，因为原生 `.value` / `.selectedIndex` 赋值不触发 MutationObserver。选项列表和禁用/隐藏属性变更自动同步，不要另写一套菜单。
- 前端类型约定：`web/src/types.d.ts` 是 API/WS 报文的接口定义，每个接口对应 Go 侧一个
  结构体，改服务端报文时两边一起改；`web/src/globals.d.ts` 声明 xterm/KaTeX 等
  `<script>` 引入的全局。两个纯类型文件用 `.d.ts`，不产生多余的 js。
  `util.ts` 的 `$()` 返回非空断言，需要具体元素接口时写 `$<HTMLInputElement>("id")`。
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

## 开源重构

目标模块、运行目录迁移与逐阶段验收见 `docs/architecture/opensource-refactor.md`。
每完成一个实施单元更新记录，明确本地验证与 Linux Docker 实测的区别。

### 备份与恢复约定

- `agentbox backup` / `backup-verify` / `restore` 在服务初始化之前分流，不能为了备份调用 `store.Open`，否则会迁移正在备份的旧数据库。
- 数据库通过 modernc SQLite Backup API 取快照，只将临时快照切为 DELETE journal 模式，源库 WAL 不改。
- 默认系统备份含配置、数据库、全部凭证与双层模板；`--full` 再含 users 全量，必须取得 data_dir flock 并验证 Docker 中无运行容器挂载源目录。命令不自动停容器。
- Manifest 校验内容和元数据，符号链接不跟随；恢复仅允许不存在的新目录，配置中的 data_dir/credentials_dir 重写为恢复目录内相对路径。恢复目录发布使用不覆盖 rename，不能混入旧 WAL。
- `scripts/backup.sh` 只负责编排内置命令、SHA-256 文件、同类型轮转和可选 rsync；`AGENTBOX_BIN`/`AGENTBOX_CONFIG` 支持独立部署路径。仅用户已授权运行该脚本的远端传输时才使用 BACKUP_REMOTE。
- 验证：`go test ./internal/backup ./cmd/agentbox`、`scripts/test-backup.sh`；需要实际 Docker 检查时设置 `AGENTBOX_BACKUP_DOCKER_TEST=1`。同 daemon 上不能同时启动带相同 session ID 的旧实例与恢复实例。

### 开源发布约定

- 项目采用 Apache-2.0；保留 LICENSE、NOTICE 和 `third_party/` 中第三方许可。内置资源哈希及 Go 链接模块清单经 `scripts/verify-third-party.py` 校验；更新 Go 依赖后运行 `scripts/collect-go-licenses.py` 并审查变化。
- `scripts/build-release.py` 在干净 checkout 构建 7 个平台包，含 `--version`/build.json/校验和。Tag 工作流只生成候选 artifact，不自动公开 Release 或包含 Claude Code 的镜像。
- 发布包验证用 `scripts/test-release.py`；真实 Linux Docker 会话冒烟用 `scripts/test-release-server.py --image <已构建镜像>`，仅合成数据，不发模型请求。两者创建自己的容器并清理。
- `scripts/scan-secrets.py` 扫描全部已获取 refs、当前跟踪文件和解包产物（含二进制 printable strings）；报告必须保存在仓库外，内容脱敏。扫描前先 fetch 分支和标签；扫描通过不是不存在敏感信息的证明，生产域名等仍需人工审查。
- CLI/基础镜像默认版本在 versions.env 与 Dockerfile；构建覆盖参数使用 `AGENTBOX_CLAUDE_VERSION` / `AGENTBOX_CODEX_VERSION` / `AGENTBOX_BASE_IMAGE`，避免运行环境同名变量污染。`scripts/test-image-policy.py` 验证缺省无追新且版本固定。
- 自动追新仅 `AGENTBOX_AUTO_UPDATE=1` 启用；install.sh 默认禁用更新 timer，显式 `AGENTBOX_ENABLE_AUTO_UPDATE=1` 才启用。保留旧镜像，禁止自动全局 prune。

### 生命周期

- `cmd/agentbox` 接收信号并调用 `internal/app.Run(ctx,cfg)`，数据目录锁最后释放；初始化失败需释放已打开的 SQLite/Docker 客户端。维护命令仍先于服务初始化分流。
- `server.Handler` 只组装路由；`Serve/Run` 绑定运行生命周期，`Close(ctx)` 幂等。请求和后台任务的准入与 WaitGroup.Add 同锁，退出后拒绝新任务。
- 后台任务走 `spawn` 和 `workContext`，长连接走 `track`；不能新增无法停止的 sleep 循环或脱离生命周期的模型任务。Docker hijack 必须显式随 context 关闭。
- 退出先拒绝新工作，尝试中断在途聊天并最多等 2 秒收尾，然后取消上下文、关闭连接、等待任务和已有用量事务，最后关闭依赖。超时返回错误，不提前关库或解数据锁；CLI 将退出，嵌入调用者的清理仍继续。
- 不杀会话容器/tmux，不承诺强制终止已脱离输出连接的 CLI 子进程或收到未发回的用量。关闭测试不使用真实凭证；`scripts/test-release-server.py --binary <Linux binary> --restart --image <image>` 验证 SIGTERM、WS 关闭、原卷重启、容器与 tmux 保留。

### 工作区服务

- `internal/workspace.Service` 持有会话锁、活动引用、创建/启动/停止/删除和回收。HTTP 层保留属主鉴权、参数校验和响应转换，业务包不能反向依赖 server。
- 同一会话的启停删都必须经过服务的可取消锁，不能单独调用 Docker 后更新 SQLite。Stop/Remove 失败时保留记录和状态；Docker 404 是幂等成功。模板→凭证→默认模型的顺序保持。
- 活动引用仍由聊天/终端/Git 执行持有，删除后迟到的 release 不重新创建活动记录。目录挂载/UID 保持原约定；Linux 测试覆盖需要 root chown 的并发创建/删除。

### 凭证服务

- `internal/credentials.Service` 管理池文件、续期/同步/保存与每账号锁；HTTP 层保留 OAuth 发起/回调参数和响应转换。账号网络出口通过注入客户端工厂提供，失败不能静默直连。
- 日常双向同步沿用 2 秒 mtime 容差；服务端续期成功后的播发改为单向立即写入已有凭证副本，不用该容差，避免刚播种的会话遗漏新链。刷新与后台同步、OAuth 保存、Codex Key 保存共用账号锁，锁等待可取消。
- 续期仍先回收会话新令牌，再判断/刷新，最后立即播发；合并保留订阅档位、scopes 及未知字段。每次同步/播发重查授权，不向撤权空间交付新凭证。未播种的 home 不由后台同步创建凭证树。
- Codex auth/config 各文件使用受限原子写入，但跨文件不构成事务；后一个文件失败时可能已更新 Key，API 返回错误，重试完成配置。

### 用量、迁移与 Agent 接口

- `usage.Service` 是解析、定价和终端扫描的业务入口，不能反向依赖 server。Store.InsertUsage 仍一次事务写用量与扣额度，终端 UpsertTerminalUsage 绝不扣额度。
- 新行 `PriceSnapshot` 包含来源、价格键、普通/长上下文档和阈值。终端 upsert 在同一事务读取首份快照，续写不能改用新价；旧行没有快照时不要伪造历史单价。Claude 0 费用按表补算时保存 table 来源。
- 当前 SQLite schema=8（迁移明细见 docs/architecture/database-migrations.md），user_version 在迁移事务内更新；未知更高版本必须在建表/改 journal 前拒绝。版本 1 接收旧库，版本 2 加价格快照。旧二进制可能无版本检查，不应连接已迁移库，回退使用兼容备份副本。
- `agent.Adapter` 提供能力、聊天/标题命令与事件解码，Event 统一 session ID、partial/output 标记及原始 JSON。server 按能力决定 app-server 优先/exec 回退，不在 Handler 内新增 provider 事件形状判断。
- 协议样本放在 agent/usage 的 testdata，全部为合成脱敏数据。Linux 测试脚本必须复制这些 testdata；禁止读取真实用户 rollout 当测试夹具。

### 阶段 D 维护约定

- 独立部署入口 `deploy/release.py`；路径、迁移停机与回退条件见 `docs/architecture/deployment-layout.md`。install 不覆盖其他布局单元或已有版本；activate 先查 schema 和 compatibility_epoch，再备份/停机/切换。不能对旧库调用 store.Open 来做只读兼容检查。
- cache_dir 缺省兼容 data_dir，配置 mutate/persist 必须保留原始路径；备份恢复重写 cache_dir，避免恢复实例碰原实例缓存。
- workspace 的启动闸门串行容量检查与容器创建，检查实际 Docker 运行状态；resources 的 0 为不限，不强杀已运行任务。
- 用量 HTTP 只调用 RequestScan，禁止重新引入同步 Scan；同步进度只描述完整扫描，不承诺 provider 已写完文件。
- 新运维接口均为 admin：storage、diagnostics、DELETE cache/marketplace。诊断严格字段白名单，不能拼接配置、环境、原始日志或 Docker inspect。
- chat/settings 通过 app/lifecycle 显式初始化和清理；聊天连接、设置缓存与轮询 timer 不得放回全局 S。新嵌套 TS 模块仍保留原生相对 .js 导入及哈希前缀。
- 浏览器测试 `scripts/test-browser.mjs` 使用合成 API；部署测试 systemctl 是模拟调用，记录时不得声称真实 systemd 已通过。

### Git 密钥与终端授权补充

- Git V2 密文嵌入 key ID，`git-secrets/keyring.json` 与旧 master.key 必须成套备份。`git-key-rotate` 只在停服并取得 data_dir 锁后执行，先全量验证再新增密钥，DB 事务/Config.mutate 重写；`--resume` 续写，保留旧密钥，不宣称销毁泄露密钥。
- `git_terminal.go` 的能力网桥固定用户登录/空间/仓库/remote/连接与绑定修订；长期凭证不进 home。只开放 status/fetch/pull/push-preview/push/cancel；命令必须复用原 Git handler 的准入/锁/审计。锁内和传输准入再检查 scope，不能只在控制请求开始时检查一次。
- 终端控制监听由 server 生命周期管理，到期/撤销取消所有下游请求。Python helper 用隔离模式、禁用环境代理/重定向，取消 ID 限定该授权；交互确认不是权限边界，同空间程序可使用已授予的写能力。只在容器执行 Git，不回退宿主机。
