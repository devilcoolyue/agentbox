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
| `internal/server` | HTTP API、鉴权、用户/账号/设置、会话、文件、聊天 WS、终端 WS、隧道、账号出口代理（`proxy*.go`）、监控、空闲回收、凭证同步、Git 变更审查（`git.go`）、用量计量与额度（`usage.go`/`quota.go`/`usagelog.go`）。 |
| `internal/store` | SQLite(`data/state.db`)：sessions/users/tokens/usage_events/quotas/credit_ledger；首次打开会导入旧版 `state.json`。 |
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
- 技能页（`internal/server/skills.go` + `web/src/skills.ts`）：管理 `.claude/skills`，范围 `session`（会话 home）与 `template`（用户模板）。列表里的 `source` 靠探测两层模板里有没有同名目录得出，顺序与 SeedHomeTemplate 的分层一致。装／复制统一走 `replaceSkillDir`：整目录替换并把 mtime 戳成当下，保证刚进模板的技能一定比各会话里的旧副本新，下次启动推得下去。技能名同时是目录名，`skillNameRe` 卡死路径穿越。详情接口连整个技能目录的扁平清单（`entries`，含子目录，父在子前，上限 2000 条）一起返回，前端 `buildTree` 拼成左侧文件树；点开单个文件走 `GET …/skills/{name}/file?path=`，路径用 `resolveFileEntry` 逐段 Lstat 拒绝符号链接（技能目录在会话 home 里，容器内随手就能造一个指向宿主机文件的链接）。
- **账号 env 绝不烘进容器**（`dockerx.baseContainerEnv`）：容器 `Config.Env` 在 create 那一刻定死，之后只能靠 exec 往上加、减不掉。账号从中转站切回订阅登录时 `clearClaudeRelay` 只改得动 `config.json`，旧容器里那份 `ANTHROPIC_AUTH_TOKEN` 还在，而 claude CLI 认 env 里的 Bearer 令牌优先于 OAuth 凭证——订阅登录形同虚设，CLI 卡在重试里直到被杀（回合报「进程退出码 137」，stderr 只剩一句 connectors are disabled 的告警）。所以账号 env 一律走 `server.execEnv` 每次 exec 注入，加和减都即时生效。`EnsureRunning` 里的 `hasBakedEnv` 负责认出老版本烘过 env 的容器并重建（比键不比值，镜像升级改 `NODE_VERSION` 的值不算脏）。
- 停止/删除：只停/删容器；工作区、home、聊天线程仍在宿主机。`DELETE ?purge=1` 才删除会话目录。
- 重启服务端后：`Server.reconcile` 以 Docker 实际运行状态修正 session status。
- **OAuth 令牌服务端自动续期**（`internal/server/credrefresh.go`）：访问令牌只有几小时
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
- 已知小瑕疵：claude 对话行里 provider 偶尔报 0（子 agent），配了 claude 价之后这类行
  会被 `priceEvent` 按表折算，但 `billingMode` 仍标成「官方报价」——因为我们没有存
  「这个价是哪来的」。数更准了，标签略糙。
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
  不能含糊：① 单价取自**当前**价目表，实收却是入账当时算的，中途改过价就对不上，
  弹窗按差额自己提示「以实收为准」；② claude 对话行的钱是 provider 自报的一个总额、
  拆不出分项，这类行的 `basis` 是 `reference`，表里那份只是照价目表推的参考值。
- **终端消耗靠事后扫 transcript 补记**（`internal/server/termusage.go`，`kind=terminal`）。
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
  - 触发有三条路，都汇到 `Server.termScan`（扫描进度上锁，同一时刻只有一趟在跑）：
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
- **codex 的终端消耗还没补。** rollout 在 `<会话home>/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`，
  `token_count` 事件带 `last_token_usage`，`session_meta.originator` 是 `codex-tui`（终端）
  还是 exec，判据齐全，只是格式另写一套解析。

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

- `internal/server/git.go` 用**宿主机的 git** 直接操作会话工作区里的仓库（不进容器），
  统一带 `-c safe.directory=<repo>`：仓库属主是 uid 1000，服务端是 root。
- **必须挡住 git 的向上仓库发现**：`data_dir` 通常就在服务端自己的 checkout 里
  （默认相对路径 `data`），workspace 自己没有 `.git` 时 `git -C <ws>` 会一路向上找到
  **服务端仓库**——曾经的表现是每个会话的「变更」页都显示 agentbox 自己的改动，
  而「提交」会把服务端仓库整棵工作树 `add -A` 进去。`.gitignore` 里的 `/data/` 挡不住，
  ignore 只管文件跟不跟踪，不管仓库发现。两道防线：`repoRoots` 只认真实存在 `.git`
  的目录，`runGit` 再用 `GIT_CEILING_DIRECTORIES`（**必须绝对路径**，git 忽略相对项）
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
  「差异」按钮置灰；删除的文件反过来（没内容可读，只有 diff）。`git/file` 复用
  `resolveUnderRoot` 做逐段 Lstat 的符号链接校验，并按 `maxFileViewBytes` 拒绝大文件、
  按 NUL/非 UTF-8 拒绝二进制——它渲染成一个个 DOM 行，不能由着文件大小来。
- `status` 必须带 `--untracked-files=all`：默认口径会把整个未跟踪目录折叠成一条
  `dir/`，用户看到的是「.claude/」而不是里面那个新文件，单文件的 diff 和丢弃都无从下手。
  代价是未跟踪的大目录（没 gitignore 的 node_modules）会撑爆列表，所以服务端按
  `maxStatusFiles` 截断并回 `truncated`。
- **宿主机 root 的 git 配置不能漏进来**：`GIT_CONFIG_GLOBAL=/dev/null` +
  `GIT_CONFIG_NOSYSTEM=1`，另外 `-c core.excludesFile=/dev/null` —— 全局排除文件
  走的是自己的默认路径（`~/.config/git/ignore`），**`GIT_CONFIG_GLOBAL` 管不着它**，
  必须单独指空。不这么做的话 Claude Code 给 root 写的那条
  `**/.claude/settings.local.json` 会把用户的文件从审查列表里悄悄抹掉，而且这事只在
  `HOME` 有值时发生（systemd 起的服务没有 HOME，手工在 shell 里跑就有），
  两种跑法结论不一样最难查。仓库自己的 `.gitignore` 和 `.git/info/exclude` 照常生效。
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
