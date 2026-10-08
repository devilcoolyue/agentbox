# AGENTS.md

给代码助手/维护者的项目说明。用户向的部署与功能文档见 `README.md`（英文）和 `README_CN.md`（中文）；这里记录实际代码结构、开发命令和容易踩坑的约定。

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
| `internal/chat` | Service 编排聊天回合、历史/用量/回执收尾；Executor 管理 CLI 传输与中断，server 保留准入及 Runtime 适配。 |
| `internal/protocol` | M1/M3 的版本化错误/聊天封装，与 contracts/ schema 和生成 TS 类型共同校验。 |
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

## 验证入口与证据

- `python3 scripts/verify.py list` 是测试命令目录；`run quick` 执行网页、Go 与本地策略，`run browser/desktop-unit/docker-core/release` 按需选择。资源及当前能力见 `docs/verification.md`、`docs/capabilities.md`。
- 入口保留每步命令、耗时、退出码、Go skip 和工作树前后摘要；缺条件必须 blocked/failed，不能算通过。报告输出在 gitignored 的 `output/verification/`，已有目录不覆盖；这类日志不是用户脱敏诊断。
- Linux Go 组先普通用户执行真实 chown 拒绝，再仅用 sudo 执行完整 server 测试；不要去掉闸门或放宽生产 chown。macOS 对 Linux 专属项只能标 not_applicable。
- 新增 test-* 脚本须在 `scripts/verification_catalog.py` 登记入口、父场景或外部资源条件。默认组清除继承的 live/helper/部分测试开关，付费模型与原生安装继续显式独立验收。
- 网页产物检查比较完整源/产物集合与重新构建字节，允许开发工作树本来有合法未提交修改；不忽略新生成或遗留 JS。CI 与本地使用同一入口，保留失败报告。

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

Linux 的服务端同步写入必须能设置容器属主 `1000:1000`；普通用户不能直接运行完整
`internal/server` 写入测试。CI 保持普通用户构建及其他包测试，先用
`AGENTBOX_CHOWN_DENIAL_TEST=1` 定向执行 `TestSyncMutationLinuxChownDeniedPreservesTarget`
验证真实 EPERM 与文件保留，再用 `go test -exec 'sudo -n --' ./internal/server` 执行完整服务端测试。
不要为通过测试放宽生产 Linux 的 Chown 校验；该拒绝回归在 root 全量测试中明确跳过。

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

### M3 联合测试

- `scripts/test-chat-integration.mjs` 使用真实 agentbox 二进制、SQLite、HTTP/WS 与浏览器，`scripts/fixtures/chat-integration` 仅替代 Docker/CLI 输出，不执行收到的命令，不能标为真实 CLI/provider 验收。控制 API 只属于独立测试 helper，禁止加进产品 Handler。
- `integration.chat` 为本机入口；完整 Linux 上传/Chown/重启走 `docker.chat-reliability --image <已准备的本地镜像>`。独立卷/网络/随机回环端口、不挂 Docker socket；服务端重启必须保持相同端口，不能通过换 origin 丢掉浏览器 outbox 来“通过”测试。非 root macOS 的附件 chown 拒绝明确记录，不能放宽生产权限来适配。
- 同时核对 fixture 执行计数与真实回执/usage/ledger/余额；只看到 UI completed 不足以证明不重复扣款。模拟结算错误的 trigger 仅可安装在带 synthetic-only 标记的临时数据库；禁止对已有实例运行。清理失败不得静默算通过。

### 空间搜索

- `features/workspaces/filter.ts` 拥有页面内搜索/状态条件，通过 app/lifecycle 初始化和销毁，不加入 S 或持久化。只按名称 NFKC + trim + 小写子串匹配，状态复用 `sessionState`，running 优先于遗留 stop_reason。不改原 sessions 顺序、首页最近空间或线程搜索。
- `shell.renderSidebar` 保留列表滚动和仍存在的键盘焦点；筛选使焦点空间消失时回搜索入口。输入法组合期间不应用半成品查询，Esc 不得关闭抽屉；下拉重置必须走 `setSelectValue` 同步增强控件。
- `browser.workspace-filter` 用百个合成空间覆盖三语/窄屏/轮询及退出，记录本机计时；不能将其当作约定设备或真实触屏性能验收。

### 聊天

- 未发送草稿在 `features/chat/draft-store.ts` / `drafts.ts`；`/me` 的 `draft_scope` 来自实例身份与用户创建身份，`draft_protocol` 只声明草稿上下文/附件校验，不代表持久消息确认。页面各写自己的记录，会话存储仅保存指针；关闭保存/退出清除副本，刷新先保存。输入、校验和上传都有空间/线程代际，迟到结果不得写入新的输入框。
- 新网页历史请求带 `draft_context=1`，空线程登记无正文 `draft_context` 事件并返回 `active_thread`，让未发送草稿可从列表返回；该事件不能算用户消息或消耗起标题额度，只有它的空线程继续复用。附件恢复及发送前走属主受限的 `/attachments/validate`，不跟随链接。消息确认独立走 chat 协议，不能用正文相同的广播冒充回执。
- `GET /api/sessions/{id}/chat` 是一个会话一个 room 的广播模型；同一房间同一时刻只跑一个回合。
- 网页 outbox 冻结 text/model/effort/control/refs 后先写副本；默认保存失败不提交，明确关闭本机保存后仅保留页内副本。草稿和待确认区分离；重连、刷新、轮询只查询，显式重试也先查原 ID，不换 ID 自动重发。回执须核对空间、线程、输入和单调 revision；旧文字广播不是确认。`pending_only=1` 避免轮询下载历史正文；未知接收 ID 的正文 7 天后过期，但保留标识用于查询/放弃。退出清理副本与本功能确认框，迟到响应不得改变新空间的输入或提示。仅服务端回执恢复附件时也要重建附件元数据并重新校验；不得退化为正文中的裸路径。
- 登录替换由发起页按 `local-identity.ts` 的不含令牌/正文的范围提示清上一身份副本；收到 storage/迟到 401 的旧页保留较新的 token，只忘掉自己的内存和会话指针，不能重复清持久区，否则冻结旧页恢复时会删除同一用户新登录后保存的内容。`preserveChatCopiesOnSignout` 只控制该次清理，不放宽 API 授权；旧 `/me` 响应还须核对浏览器当前 token 后才能初始化新编辑器。显式未知 chat 版本不得作为旧服务端静默降级。
- schema 12 的 `store.ChatRequest` 已接入独立 HTTP v1（`chat_protocol`/`chat_scope`），浏览器通过 outbox-store.ts / outbox.ts 接入待确认发送与查询恢复。仅 `AcceptChatRequest` 成功返回 fresh=true 的调用者可安排工作，重放和 reviewed 不获得执行资格；状态推进须使用修订号。启动在 Docker 初始化前将未结束回执改为 uncertain，不能自动重跑；旧 WS、线程变更与删除必须遵守同一持久准入；回执在原用量 flush 和转录同步后完成，结算/历史失败保持 uncertain。`chatReceiptTurn` 只观察原执行器，不另启模型/计费路径；传统 exec 必须检查 Scanner 错误，provider 结束状态由 agent.Adapter 解码。用户中断和服务关闭不得混为一谈。存储连接 WAL + synchronous=FULL；空间/用户删除同事务清正文留 ID 围栏，线程删除先清对应回执正文。附件 TTL 扫描流式读取回执引用，错误时拒绝删除；与接收校验/提交共用 attachmentMu。
- 消息落盘到当前线程 `chats/<threadID>.jsonl`；`chats/active` 指向当前线程。旧版 `chat.jsonl` 首次访问自动迁移。
- Claude 走 `claude -p --output-format stream-json`；Codex 优先走 `codex app-server`（真流式增量），握手失败回退 `codex exec --json`。
- 思考显示：Claude Opus 4.7+/Claude 5 的 API 默认 `display=omitted`，Codex 内置模型默认 `default_reasoning_summary=none`，不显式要就只有空 thinking/reasoning。`ChatCommand` 固定带 `--thinking-display summarized`；Codex app-server `turn/start.summary` 与 exec `-c model_reasoning_summary` 共用 `agent.CodexReasoningSummary`（标记不支持推理的模型不加），agentprobe 以同一值验证候选镜像。原始推理任何参数都拿不到，摘要按段到达、常为英文。前端 `stream.ts` 的 thinking 块等首段非空文字才插入，不能回退成先建空框。
- `stream_event`/增量事件只广播不落盘；完整事件落盘并广播。provider 会话 id 用 `agent.ExtractSessionID` 提取，写入 `chat_session` 供 `--resume`/thread resume。
- 用户中断：Codex app-server 优先协议内 `turn/interrupt`，否则用容器里的 PID 文件发 SIGINT。

### 用量计量

- 回合收尾事件里的 token/费用落进 `usage_events` 表（`internal/usage/events.go` 解析，
  `runTurn` 的 `onLine` 里挂钩），并在**同一个事务**里从用户额度扣掉（见下节）。
- Claude 网页对话在 `internal/usage/claude_messages.go` 按 `message.id` 去重：优先有 `stop_reason` 的记录，同等完整度取 output 最大者。`message_delta` 必须在 partial 早返回前交给 tally；回合启动前保存 transcript 文件边界，收尾只读同一 provider 会话及子 Agent 的新增记录补齐用量。`modelUsage` / `total_cost_usd` 可能包含续聊历史，绝不用于网页扣款；仅独立、不 resume 的起标题进程允许 result 计量。按回合开始锁定的价目表逐请求计价，再按模型聚合。`usage_messages` 按空间/message ID 持久去重，认领、写流水、扣额度同事务，重启或重放不能重复扣款。
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
- `pricing_catalog`、`pricing_managed`、`pricing_history` 必须一起进入 Config 的 mutate/persist；价格历史最多 10 次，回退不改历史用量或目录地址。后台每日检查通过 server 生命周期运行，默认不联网、不自动应用。`modelsdev.go` 为精确 URL `https://models.dev/api.json` 提供第三方转换：Claude 5m 写入价校验后转 1h、GPT 明确 context tiers；缺项/未知规则进入 issues，不能填假核验时间。`auto_apply` 显式启用后仅在每日新成功拉取时更新已绑定当前 URL 的跟随模型；单价变化超过 25%、零价切换或长档规则变化留待手动核对。手动检查只预览；回退价格暂停自动跟随。
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
- **Claude 网页对话按独立消息用量查价目表**，独立起标题才可用 CLI 报价；不报价的走 `config.json` 的 `pricing`
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
  计费方式并入费用列；窄屏明细纵向展示，字段名来自 `<td>` 的 `data-l`。缓存命中率与 CSV 共用 `usage-math.ts`，分母为未缓存输入 + 缓存读 + 缓存写，无输入显示 —，输出不进分母。
- **每行「费用」旁的 `?` 是费用明细弹窗**（`usage.ts` 的 `openCost` + `dlg-cost`）：把
  四个 token 桶各自的 `token × 单价` 摊开，末尾对上实收金额，脚注写清计价算法。单价
  由服务端随行下发（`usageRowView.Rate` ← `rateFor` ← `config.PriceLookup`，顺带给出
  命中的键与档位），**别改成前端自己查价目表**——普通用户根本拿不到 `settings`。两件事
  不能含糊：① 新行使用入账价格快照（rate.snapshot=true），旧行才回退当前参考价并明确提示；② CLI 报价的起标题/历史行总额拆不出分项，`basis=reference`；新 Claude 网页行 `per_request=true`，逐请求计价后汇总，不能拿整行输入判长上下文档。
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
  - 按 `message.id` 去重（旧日志缺 ID 才回退 requestId）：优先完整 stop_reason，再选 output 较大者；时间取最早一份。不能让晚到占位记录覆盖最终用量。
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
- xterm 6 会丢掉 iOS 中文键盘直接上屏的标点（「，」等，上游 xtermjs/xterm.js#3070），也不认连按标点键时 iOS 不带按键事件的删除/替换（「，。？！」循环）。`term.ts` 的 `bridgeDroppedInput` 在 xterm 所有发送时机（keydown、keypress、229 差分定时器、输入事件）都过去后，若这次按键 xterm 一字未发且不在组字，才按 textarea 前后差异补发（先退格再插入，最多删 8 个字符）；升级 xterm 后若上游已修复（PR #5614），删掉它并保留 `test-browser.mjs` 的「恰好发送一次」断言。真机事件顺序用地址参数 `?imedebug` 打开 `term-input-debug.ts` 的诊断面板，上传到 `/shared/.file/`（只在开启期间记录，含期间输入的字符）。
- xterm 6 没有触摸滚动，单指拖动原本落给浏览器滚走整页。`term-touch.ts` 把单指纵向滑动换算成滚轮事件派发给 xterm，与桌面滚轮同路：tmux（`mouse on`）下是鼠标滚轮上报，tmux 进历史模式、一次翻 5 行（所以每 3 行手指位移发一次），普通屏才直接 `scrollLines`。单指 touchmove 一律 `preventDefault`（页面放大时除外），双指交还浏览器缩放；滑动过的触摸抬手吞掉点击，免得弹出键盘。历史模式里打字会被 tmux 吞掉，Esc 或滑回底部退出，与桌面滚轮翻历史一致。长按 500ms 由同一模块回调 `term.ts`，用 `menu.ts` 的 `openMenuAt` 在手指上方弹「粘贴」（不挪焦点，否则软键盘收起）：触屏上 xterm 输入框只是光标处一个点，系统粘贴菜单出不来，只能 Clipboard API 读剪贴板（要 HTTPS），图片走与桌面粘贴相同的上传，文字走 `term.paste`（括号粘贴）。Android 长按的 contextmenu 在捕获阶段拦下，不能让 xterm 当右键处理。回归在 `scripts/test-term-touch.mjs`（CDP 真实触摸）。

### 桌面项目与终端

- 顶部/侧栏状态只取服务端sessions结果，不从WebSocket已连接/断开推导容器运行/停止。
  TerminalPane经ProjectWorkspace向App通知连接变化；SessionRefresh串行合并并补一次尾随读取，
  可见时5秒轮询、focus/visibility恢复刷新，stop/start代次拒绝旧账号结果，刷新不重建现有终端。
- `desktop/` 为独立 Tauri/Vue/xterm 构建；Rust 原生 HTTP/WS 管令牌，Go `cmd/abox-sync`
  提供私有 stdio 预检及显式同步批次命令，服务端 sync 默认关闭。管理员显式设置
  `desktop_sync_enabled=true` 后通告 sync v1；该字段须同时保留在 Config mutate/persist/settings。
  开关控制能力发现与新计划，不强杀在途写入，恢复管理独立。桌面不进入服务端 Go 构建依赖。
- `internal/syncproto/syncclient/syncfs` 是便携同步基础层。scan 失败不能变空清单，规则变化
  不能当删除，冲突必须暂停项目；写入用预期哈希、受限句柄与恢复副本，不跟随链接/硬链接。
  Windows 原生实现不得依赖 Unix safefs；本地预检路径只由原生 picker 经私有管道授权。
  执行器与尚未完成的验收边界见 docs/architecture/desktop-sync.md；sync capability 默认仍为 0。
- 登录与两条配对通道发 token 时用 `CreateTokenIfUserUnchanged` 原子核对用户的密码 hash 与
  CreatedAt；HTTP 改密通过 `ResetPasswordIfUserUnchanged` 同事务更新密码和失效其他 token。
  不能退回先校验再无条件 CreateToken，或拆开改密与撤销；否则重置/同名重建期间能续签旧登录。
  配对码满额仍允许属主替换自己的旧码，两个通道的码不能互换。
- 服务端 client_manifest 使用 safefs 而非客户端绝对路径遍历，最多两项并行、空间锁内有界
  扫描；错误不返回部分清单。client_lease 进程内 30 秒租约协调重叠目录，校验设备/令牌/代次，
  重启全部失效。持有租约时禁止编辑/移除项目。
- client_mutation 的 apply 支持 replace/delete/mkdir/rmdir；接收上传不占空间锁，最终检查与
  写入在空间锁内，发布前复核租约。新建不覆盖、rmdir 仅空目录且 AT_REMOVEDIR，不递归。
  日志与 before 在 sessions/<id>/client-sync（容器挂载外），uncertain 不盲目重放；applied
  仅历史收据，执行器仍须重扫。每空间 1000 活动操作/256 MiB 旧内容，到限停写，无自动清理。
  状态/恢复 GET 仍按空间属主；完整备份包含日志，默认系统备份不包含；sync 默认仍为 0。
- `sync_recovery_gc=1` 只宣告显式回收协议，不能作为开启 sync 的授权。`GET sync/storage` 统计当前
  空间；`POST sync/operations/{id}/retire` 要求原实例头、原 device、意图 digest 与实例/空间绑定的
  confirmation，只清理 applied，无需项目仍存在；空间存在活动租约则拒绝。先持久化 retiring，再校验/
  删除旧字节、同步目录，最后记 retired；原 ID 的 applied 收据永久保留，旧服务端也不能将其当新操作重放。
  每空间最多 100000 永久收据（retiring 已预占），到限拒绝新的回收；只释放旧内容与活动名额，
  不释放永久收据元数据。容量缓存绑定 journal 根 inode，空间锁内更新，错误失效；冷扫描有界且限 60 秒，
  大量收据时可能慢，扫描失败停写，不降级为空统计。metadata 为逻辑字节，不含 inode/目录分配开销。
- `client_file` 条件下载核对项目 revision、规则 hash 和文件 hash/size/executable，校验完快照
  才发送。与清单共用两个读取槽，文件各限 64 MiB；网络发送不持有空间锁。Go Remote 只提供
  原生传输，禁重定向、验证 TLS、请求头传令牌；读取失败不能当空树；Engine 已接入显式执行和 IPC，pending 禁止重放。
- `client_projects.go/client_terminals.go/client_pair.go` 新 API 保持 auth + withSession 属主检查；
  元数据通过 workspace.WithSession 与 purge 串行，目录通过 safefs 固定句柄校验。
- 每个终端以稳定 ID 使用独立 tmux socket，旧 `/term` 仍是默认 socket 的 `main`。在空间锁内
  ExecCommandEnv 等待 tmux 创建完成，再 ExecPTY 仅 attach；不能改成延迟 new-session，否则
  并发结束可能确认成功后再产生孤儿终端。参数逐项 shellQuote，凭证只经 Docker exec Env。
- 关闭先持久化 closing，再停止对应 tmux，失败保留可重试记录；输入/心跳复查资源状态和账号
  权限。项目改路径/删除必须先结束所有终端；移除项目映射不删除文件。终端用量仍只补记不扣款。
- clientidentity 将 server_id 放 data/client-instance-id（0600），不进系统/完整备份；恢复实例
  生成新身份，客户端重新确认基线。损坏身份只阻止能力/同步接口，不阻断旧业务 API。
- syncclient.StateStore 是原生专用应用目录的 SQLite，保存设备、绑定、基线、pending 与历史，
  不存令牌/内容。prepared→started→verified，崩溃 started 不重放；完整重扫符合计划才在同
  事务提交基线和归档。目录/祖先实际 ID 防重叠；Engine/IPC/UI 已接通核对/收尾/副本导出，sync 默认保持 0。
  1024 绑定/每绑定 1000 完成批次/全库 256 MiB 逻辑元数据，到限停写，无自动清理。
- 待定批次核对重新获租并重扫，finish 必须全树符合且远端收据全部 applied；replan 只归档
  不改文件或旧基线。副本按批次/操作 ID 导出到原生选择的映射外目录，不覆盖现有文件；
  收据错误不变成 missing，哈希失败不发布，renderer 不能提供本地源路径。
- 同步进度只观察状态，不参与收据/基线。Go 私有 IPC 的 progress=true 为可选，100ms
  合并快照；Rust Channel 一条未 ACK 加一个最新值，ACK 限定原生任务 ID/序号。取消/退出
  登录关闭转发，Vue 代次丢弃旧事件。传输读满不等于已发布，N/N 操作不等于基线已提交。
- 本地同步 schema 5 支持 archived、逐文件选择、abandoned 未知结果与本地/远端清理审计；schema 1/2/3/4
  迁移保留设备/记录，旧 sidecar 拒绝重新打开 schema 5。远端清理先持久化 retiring，成功后记 retired，
  不修改基线。当前绑定有 pending 或历史为 replan/abandoned 时禁止；只允许全部 verified，或由 finish
  及有效核验摘要证实完成的 started。旧能力不发送 storage/retire 请求，身份变化不发送旧操作 ID；失败
  重新加载 revision 并预览续做，无自动重试。原本无恢复引用的历史也必须所有远端操作 retired 才可删除，
  避免丢失回收入口。解绑须核对 revision、拒绝 pending，仅归档不删文件/旧基线/历史；新绑定
  不继承基线。活动映射按实例或同 URL 排除重叠，不能换 server_id 绕过旧 pending。
- 同 URL/认证用户可以发现本机旧实例记录；身份变化时只允许历史/本地副本恢复，禁止将旧
  远端恢复引用发往新实例。历史目录仍排除为导出目的地，归档仍计容量，移动副本不自动定位。
- schema 10 仅新增项目/终端表，Store.Delete 同事务清理。回退须兼容备份；不得改变旧账号绑定。
- 回归：`go test ./internal/store ./internal/server ./internal/dockerx`；真实 Linux PTY/tmux 用
  `AGENTBOX_CLIENT_TEST_IMAGE=<client.Dockerfile 镜像> go test ./internal/server -run TestClientTerminalsContainerLive`。
  原生桌面 smoke 有 legacy/projects 两种合成模式；`TestDesktopSyncNativeSmoke` 通过
  AGENTBOX_SYNC_SMOKE_BINARY 指定显式测试包，使用真实 Go 路由和私有 sidecar 验证同步/
  恢复 UI。临时目录注入仅编译进 desktop-smoke，不能进入正常 picker 路径或接受 renderer
  提供目录。此服务端测试包不能在 Windows 构建；平台、真实 picker 与安装验收另行记录。

### 文件/共享目录

- 默认操作为会话 `workspace`；`?scope=shared` 操作用户级共享目录（挂载到所有会话容器 `/shared`）。
- 上传落在 `?path=` 指定的子目录（前端传当前浏览目录，服务端用 `safefs.Root.Sub` 逐级固定，拒绝符号链接）；
  `clear=1` 只清空这个目标目录，前端只在「⋯ → 清空当前目录后上传」并二次确认后才带它。
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

- 侧栏「Git 管理」（与使用记录、系统设置并列）进入独立 Git 管理页（`#/git/guide|profile|connections`）。`git-management.ts` 管分区与说明，`git-surface.ts` 管页内详情及清理；工作空间的远程操作仍留在空间内。切换分区、离页或退出登录时清理表单和记录轮询；身份、连接异步响应需校验登录 token 与挂载状态。

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
- 隧道本身不设置全局 `HTTP_PROXY`（账号出口代理另有该变量）；兼容模式在 exec env 注入 `AGENTBOX_INTRANET_PROXY` / `AGENTBOX_INTRANET_MAPS`，由 agent 按需使用。服务端与客户端均启用透明模式时，不依赖且不注入这组专用变量。
- exec env 只到达 exec 出来的那个进程。终端附着的是常驻 tmux，已有会话的 shell 是更早的
  exec fork 出来的，所以 `termCommand` 在 attach 前用 `tmux set-environment -g` 把当前
  env 镜像进 tmux 全局环境（隧道变量缺失时 `-gu` 清除）——否则「会话先开、隧道后连」时
  终端里的 agent 永远看不到代理变量。运行中的窗格改不了，须重连后新开 tmux 窗口；在旧 Shell 内仅重启 agent 仍会继承旧环境。
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
- 界面风格（`<html data-skin>`）与明暗（`data-theme`）正交。琥珀就是 `base.css` 的默认令牌；其余五种在 `css/skins.css` 各写全明暗两套，新增颜色令牌须十套都补（`scripts/test-skins.mjs` 查漏回琥珀）。风格专属形态在 `css/skin-effects.css`，选择器必须挂 `:root[data-skin=…]`。像素圆角写 `calc(Npx * var(--radius-scale))`，胶囊用 `--radius-pill`、大号圆形按钮用 `--radius-round`，否则直角风格收不成 0。液态玻璃只给浮层（弹窗、菜单、气泡、窄屏抽屉）加 `backdrop-filter`，画布不做动画，免得背景模糊每帧重算。偏好只存 localStorage（`agentbox_skin`），改键名或取值要同步 `index.html` 首屏脚本。
- 可滚动的弹层/列表不要在 `pointerdown` 上无条件 `preventDefault()`：Safari 26.5 起这会取消这次触摸的滚动。只对 `pointerType === "mouse"` 拦截。
- 获焦提示与按钮焦点环只给键盘操作：iOS / Safari 点按钮不获焦，随后 `showModal()` 或菜单用程序挪过去的焦点会被判成 `:focus-visible`（手指点开的弹窗，关闭按钮上带框带「关闭」气泡）。`modality.ts` 在 `<html data-input>` 记最近一次是键盘还是指针；新写「获焦就显示」的逻辑要同时核对 `keyboardInput()`。
- 窄屏顶栏的分区切换（系统设置 / Git 管理，`responsive.ts` 的 `mobile-section-menu`）是导航菜单，不是表单下拉：条目照桌面导航按钮生成，一次列全、竖屏不滚动，没有搜索框（获焦就弹键盘、把列表挤成一小截）。别把它并回 `select.ts`；分区多到一屏放不下时再考虑分组。
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
- 会话镜像内禁用 CLI 自升级（`DISABLE_AUTOUPDATER=1`）；默认版本由 `images/agent/Dockerfile` 管理。网页「Agent 镜像更新」（旧名「客户端更新」）由 `internal/imageupdate` + `internal/dockerx/image_update.go` 执行，服务端生命周期内按系统时区每日调度；旧 `scripts/auto-update-image.sh` / systemd timer 仅供旧部署手动选择，网页管理时保持停用。
- `GET /api/updates/components` 仅管理员，只观察当前构建/schema、配置镜像标签与服务端桌面能力，最多 3 秒 Docker inspect；不访问发行/npm 源或运行 CLI。labels_only 不证明协议兼容，更不代表运行中空间版本；网页不能读取本机桌面安装版本。前端离开页面/退出必须作废请求，三个升级动作仍归原更新器。
- `image_updates` / `previous_agent_image` 必须进入 Config 的 mutate/persist；更新以当前镜像不可变 ID 为基础保留浏览器层，验证 CLI 版本及隔离合成行为、同步报告后通过 `SwitchAgentImage` 比较原镜像与策略再原子切换，不能覆盖构建期间的新设置。回退暂停自动更新。任务单飞、可取消、30 分钟超时，失败不切换、不自动 prune。状态落在 data_dir/image-update-state.json。
- `config.json`、`accounts/`、`data/` 含密钥和运行时状态，已在 `.gitignore`；不要提交。
- 若改动影响用户可见行为、部署步骤、API 或配置字段，同步更新 `README.md` 与 `README_CN.md`，保持中英文内容一致（必要时也更新 `deploy/README.md`）。

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

### 首次使用与创建入口

- `GET /api/onboarding` 只读并按请求身份过滤，返回授权账号的最小字段及新空间默认模型；凭证存在不等于登录/模型可用，`can_create` 只表示有获准使用的支持账号，不作服务端准入凭据。
- `web/src/onboarding.ts` 由 app 生命周期初始化/清理；诊断观察仅在本次登录保留，普通用户不显示实例配置动作。离开空间回首页须发 navigation-changed，以同步 URL 和准备状态。
- 创建弹窗由 `features/workspaces/create.ts` 管理，不把状态再放回全局 S。打开/提交前刷新账号，无账号或读取失败禁止提交；仅一种可用 Agent 时自动选中。撤权不能静默换账号提交，退出清理草稿/账号选项，旧请求不得覆盖新弹窗。
- 创建流程使用 `PUT /api/session-creations/{request}` 的 schema 11 持久收据，按用户创建身份隔离；ID 与请求参数冲突必须拒绝，删除/放弃保留 tombstone，不能因重试分配新空间。浏览器会话存储只保存待确认 ID/创建参数，不保存文件、Git URL 或凭证。
- 项目上传在容器挂载外完整校验/计总大小，再不覆盖重命名到新目录；Linux chown 失败拒绝发布。Git 导入复用原 gitx/授权/审计链路，不增加宿主 Git fallback。每次导入另有持久 attempt 收据，running/uncertain 阻止新尝试，先查看文件再显式 review；不能把确认丢失当失败自动重跑。
- 创建/导入与会话销毁共用原生命周期边界；旧 POST /sessions 仍兼容但不声称具备新协议保证。构建产物、三语词典、browser.onboarding 和 Go/迁移/权限回归一起维护。
- 创建配置摘要的模型取收据保存的空间值，`container_resources` 只公开 CPU/内存/进程上限，表示当前新容器配置；不把它当作已有容器的实测值或每空间冻结值。摘要与收据返回值不一致时网页先停在核对状态，再允许导入。首任务引导只在用户点击后填入输入框、不覆盖已有输入、不自动发送；工作区 ZIP 显式固定 scope，不能随上次共享目录选择改变下载范围。

### 分层环境诊断

- `internal/diagnostics` 提供固定白名单的 passed/failed/not_checked 观察；不能根据配置、凭证或镜像存在声称模型可用。不得刷新 OAuth、启动容器、调用模型或修复现有文件。
- `check-config` 默认保持离线配置校验及 schema/compatibility 字段；只有显式 `--environment` 才访问 Docker 和临时写入探测。不得把扩展检查自动加入 release.py 的兼容校验；不创建数据目录/数据库、不执行迁移。
- `GET /api/diagnostics` 为管理员只读导出，`POST` 才执行独立临时文件探测。空间 `POST …/diagnostics` 严格按属主、持 workspace 锁做只读检查，授权失败不读取账号凭证；不能返回实例计数、路径、账号或其他用户信息。
- 临时探测使用固定 safefs 根和独立 `.agentbox-diagnostic-*` 目录；只对新文件验证 Linux 1000:1000 chown，失败如实报告。空间根目录元数据检查不替代文件全量权限/真实容器运行验证。
- `/diagnostics/ws` 和空间对应路径仅回复固定探测帧，仍需登录、同源/属主校验，纳入 server 生命周期；不能加入聊天房间或计费。服务端报告的 WS 保持未检查，浏览器收到匹配编号的响应后才记录 `client_checks`，日志 sent 不等于浏览器已收到。
- 网页诊断窗口负责请求取消和关闭清理，导出再次白名单过滤；词典/合成协议样本/构建产物同步。验证 `npm run test:diagnostics`、完整浏览器、Go diagnostics/credentials/server/cmd 及 Linux root/非 root 权限回归。

### 错误契约与关联 ID

- `internal/server/problems.go` 的 v1 错误保留 `error`，增补 `code/operation_id/hint/action/retryable`，公开消息和关联日志均走白名单，禁止拼接底层 err.Error/URL/路径/凭证。类型映射覆盖 Docker、镜像、磁盘、容量、授权和聊天准入；新故障优先保留 typed/sentinel error，不能按文案猜类别。
- HTTP 关联中间件不能包装掉 Hijacker/Flusher；聊天每条提交单独生成 ID，不能复用整个连接 ID。浏览器握手参考仅接受 32 位小写十六进制 `connection_id`，不作身份或幂等凭据；到达服务端前的网络失败可能没有日志。
- 聊天错误字段随 JSONL 保存并在新旧历史中兼容；嵌入的 `ProblemDetails` 必须导出，否则 encoding/json 无法为历史分配嵌入指针。保留参数拒绝的 retry_text 语义，不据 retryable 自动重放任务。
- `web/src/problems.ts` 按稳定码翻译，旧/未知码回退服务端文案，纯文本展示；词典、TS、构建产物和 `testdata/problems-v1.json` 同步维护。回归 `npm run test:problems`、`scripts/test-browser.mjs` 与 Go `TestProblems*`；Linux 完整启动回归需要生产 chown 权限。

### 管理员离线密码恢复

- `admin-reset-password --config CONFIG --user ADMIN` 必须显式指定目标，取得 `data_dir/agentbox.lock` 后重新核对配置，只处理已存在的管理员；不能自动停服、建号、提权或删除锁文件。
- `store.OpenForMaintenance` 仅打开已存在且 schema 与二进制一致的数据库，不迁移、不导入 JSON、不创建空库。密码与全部令牌撤销复用 `ResetPasswordIfUserUnchanged` 事务；不修改空间、额度和流水。
- 网页与 CLI 共用 `internal/password` 的原 PBKDF2 格式。密码从 `/dev/tty` 两次隐藏输入，不接受 argv/env/stdin；SIGINT/SIGTERM 取消须恢复终端回显。macOS `/dev/tty` 不可靠支持 poll，使用可取消的非阻塞读取。
- 回归入口 `go test ./cmd/agentbox ./internal/store ./internal/server`；CLI 测试里的 Python 伪终端仅使用隔离的合成数据库，覆盖实际 main 分流、无回显、输入不一致、无终端及信号取消，不连接生产。

### 开源发布约定

- 项目采用 Apache-2.0；保留 LICENSE、NOTICE 和 `third_party/` 中第三方许可。内置资源哈希及 Go 链接模块清单经 `scripts/verify-third-party.py` 校验；更新 Go 依赖后运行 `scripts/collect-go-licenses.py` 并审查变化。
- v0.1.5 起正式发布、安装与更新统一到 `devilcoolyue/agentbox`；`agentbox-releases` 保留为旧版更新兼容镜像，同版本只构建一次、同步相同附件和 SHA256SUMS。历史包原样迁移，构建提交映射及发布顺序见 `docs/releases.md`，不能仅更新旧库说明或删除旧发布地址。
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
- 当前 SQLite schema=12（迁移明细见 docs/architecture/database-migrations.md），user_version 在迁移事务内更新；未知更高版本必须在建表/改 journal 前拒绝。版本 1 接收旧库，版本 2 加价格快照。旧二进制可能无版本检查，不应连接已迁移库，回退使用兼容备份副本。
- `agent.Adapter` 提供能力、聊天/标题命令与事件解码，Event 统一 session ID、partial/output 标记及原始 JSON。server 按能力决定 app-server 优先/exec 回退，不在 Handler 内新增 provider 事件形状判断。
- 协议样本放在 agent/usage 的 testdata，全部为合成脱敏数据。Linux 测试脚本必须复制这些 testdata；禁止读取真实用户 rollout 当测试夹具。

### 阶段 D 维护约定

- 在线升级入口为 `internal/server/upgrade.go` + `deploy/update.py` + `web/src/updates.ts`。只支持经过探测的 Linux/systemd 版本目录布局；开发版（含预发布、dirty）可切换到最新正式版而不比较版本高低，正式版之间仍禁止降级/重装，配置与 schema 兼容检查不可跳过；管理员只能提交已检查的版本号，不能传 URL/路径/命令。独立 systemd 临时服务执行下载与激活，复用 `.deploy.lock`、`release.stage/activate` 和兼容检查，状态落 `<app>/.update/state.json`；禁止在主服务子进程里直接停自身单元。发布包必须携带 update.py。校验和必需，归档拒绝链接/穿越/超限；版本切换后不自动回退数据库。`scripts/test-update.py` 模拟 systemd/下载，不代表真实 systemd 验收。

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


### 远程浏览器

- 工作空间浏览器入口为 `internal/server/browser.go` / `web/src/remote-browser.ts`；可选 `images/browser` 镜像叠加到 Agent 镜像，amd64 默认固定 Chrome for Testing，ARM 明确使用 Chromium。启用和边界见 `docs/remote-browser.md`。
- VNC 仅监听容器回环，经 Docker exec 原始流和鉴权 WebSocket 转发，禁止发布 VNC/CDP 端口。所有操作按空间属主、账号授权和额度准入；长连接随 server 生命周期关闭并持有空间活动引用。
- 使用 `workspaces.UseRunning` 串行浏览器启动/停止与空间停止/删除；不得在持锁回调里等待整个 WebSocket 生命周期。Python 控制端另用 flock 防止同空间重复启动。
- Chrome 保留自身沙箱；仅带 `agentbox.browser=1` 镜像使用附带来源/许可的 user-namespace seccomp 配置，不加 privileged / SYS_ADMIN，不使用 `--no-sandbox`。
- 只注入浏览器专用代理 env，不传账号 API Key。账号代理桥不可用须失败；Chrome 的本机适配器处理代理认证，网络配置改变须重新启动浏览器。网页 Cookie 与 CLI OAuth 凭证互不转换。
- profile 存 `home/.agentbox-browser`，与空间同 UID；不承诺对同空间 Agent 隔离。备份须 `--full`。noVNC 使用原生 ES module，第三方哈希与许可证必须一起更新。UTF-8 剪贴板走单独接口，不依赖旧 VNC Latin-1。
- 真实浏览器测试 `scripts/test-remote-browser-live.py` 用独立卷与合成站点，禁用空间外网，不访问真实用户网页或调用模型。`scripts/test-browser-runtime.py` 验证代理 HTTP/CONNECT/WebSocket 转发和失败不直连。

- Chrome DNS 规则须显式 `EXCLUDE 127.0.0.1`，通配 `MAP * ~NOTFOUND` 也会阻止本地代理 IP。修改代理参数必须跑 `scripts/test-browser-proxy-live.py` 的真实浏览器回归，不能仅验证 Python 代理本身。

### Claude MCP 管理

- `internal/mcpconfig` 管理用户/空间独立配置、脱敏、修订和原生配置同步。源文件在容器挂载外的 users/<user>/mcp.json、sessions/<id>/mcp.json，均进入系统备份。不得复用 home 模板的整文件 mtime 覆盖规则。
- 工作空间 MCP hook 覆盖已运行分支；网页当前回合期间跳过附带启动同步，runTurn 在执行 CLI 前显式同步并阻止冲突回合。终端保留修复入口。空间编辑通过 WithSession 防止 purge 后重建目录。
- 只接管明确授权的原生用户级条目，记录 Applied/Pending 后通过容器 Claude 原生配置命令修改，取消后可重试。未知字段不做有损接管。当前终端 CLI 不承诺热更新；项目批准与插件仍由 CLI 管理。
- `helper.py` 嵌入服务端，通过 python3 -I -c 在目标容器执行，敏感载荷走 stdin。stdin EOF 取消，独立 28 秒闹钟兜底，清理检测进程组；禁止宿主机执行 MCP 或返回原始 stderr。HTTP 支持 Streamable HTTP 的 JSON/SSE 响应，不含旧 SSE transport；首版检测不读取 CLI OAuth 凭证。
- 回归：`go test ./internal/mcpconfig ./internal/server ./internal/workspace ./internal/backup`；`scripts/test-mcp.mjs` 纳入浏览器合成 API 测试；`python3 scripts/test-mcp-live.py` 使用固定镜像、隔离临时 home、network=none 与本地模拟模型验证真实工具调用，不发付费请求。`scripts/test-mcp-server.py --binary <Linux binary>` 验证真实 Go API→Docker 同步与检测，使用独立卷并清理。

### 桌面附件、文件与更新边界

- `desktop/src-tauri/src/attachments.rs` 从原生 picker/剪贴板/DragDrop 获取文件；renderer 只收
  两分钟、单次、绑定当前 Remote 实例的 ticket，不提供任意绝对路径读文件命令。最多 32 个/
  总 64 MiB，单附件 19 MiB（旧服务端 20 MiB 上限包含 multipart）。上传确认后走旧 images API，
  返回路径限定 `/shared/.images|.file/<安全名字>`，只通过用户点击插入终端，不写入伪终端输出。
- 附件与下载共用原生互斥及按 task ID 取消；退出登录先取消并等待网络任务收尾。取消/断流不能宣称
  服务器回滚，未知结果不自动重试。手动下载当前限 64 MiB，原生保存路径、不覆盖、收到完整内容才发布。
- 剪贴板图片经 `attachments/clipboard_image.rs` 原生取 PNG/TIFF（Mac）或 PNG/DIB（Windows），
  先检查同一快照的头与尺寸再解码，不得改回 `clipboard-rs::get_image` 的解码后限额。输入 ≤128 MiB、
  ≤32 Mpx、单边 ≤32768、解码像素 ≤128 MiB、输出 PNG ≤19 MiB；库分配限额不是进程 RSS 上限，
  Mac 系统物化 NSData 前的开销不受此限制。BigTIFF 拒绝；文件剪贴板仍优先。
  Windows packed DIB 用虚拟 BMP 头显式给像素偏移；image 0.25.10 的无文件头解码对 V4/V5
  BI_BITFIELDS 会多跳 12 字节，不能直接改回 new_without_file_header；标准手写样本覆盖行序和透明度。
- `updates.rs` 固定桌面专用清单、编译时更新公钥及版本绑定签名，不能读取服务端 releases/latest。
  更新与文件/同步排他，安装保留 keyring；开发包不配置公钥时明确禁用更新。
- `desktop-release.yml` 只生成 artifacts；正式签名需独立 Apple/Windows/更新密钥且验证成功，不静默降级。
  versioned release 和 desktop-stable feed 发布均须 make_latest=false。验收门槛见
  `docs/architecture/desktop-release.md`，安装探针不等于 GUI/升级/最低系统实机验收。

- 目录手动上传走 `file_upload.rs`：预览消耗原生 ticket 并保存 ≤19 MiB 字节快照，两分钟确认摘要绑定
  目标空间/范围/目录、文件内容和目录列表；执行重读列表，变化即拒绝。旧 upload API 无 CAS，UI 必须
  明示覆盖/解压合并及部分完成风险，不传 clear，不假称同步恢复副本。与 sync_work/attachment_work 排他。
- 持续同步组件跨空间保留；`SyncScheduler` 统一排队进入 native sidecar，队列最多 64。取消排队项只让
  该项不再派发，不发全局 sync_cancel；当前运行项仍由 Rust 互斥/EOF 取消。退出登录卸载所有空间监督器。
- 只读预览的明确瞬时网络/HTTP故障经 Go `sync_preview_retryable`→Rust 原命令核对后传给循环，
  仅预览阶段按1/2/4/8秒最多重试四次；证书/身份/协议/取消不重试，写入阶段任何错误都暂停。
  Windows原子替换仅对sharing/lock violation有界重试，每次重验租约/目录/目标，等待后重验暂存哈希。
  不能重试ACCESS_DENIED、先删除目标或重新创建恢复副本来掩盖失败。
- `TestLinuxDiskFullPreservesOriginalAndRecovery` 只对显式、≤8 MiB tmpfs 注入真实 ENOSPC；不得传普通
  项目目录/宿主临时盘。原生 Smoke.app 单实例，只能串行运行 legacy/projects/sync 场景。

- 本地副本清理 `sync_recovery_discard` 只接受绑定/批次/操作 ID 和 revision，不收路径。只允许完整核验
  且非 replan/abandoned 的历史（verified 或有 finish 核验摘要的 started），拒绝 pending；先 FULL 事务提交 discarding，再核对根身份/哈希/大小/
  单链接普通文件，unlink + 目录同步后写 discarded。中断重试须重新加载 revision；保留原 Recovery 引用
  和历史，不修改基线或服务器收据。远端副本另经显式预览与 retire 协议回收，不得直接删除目录绕过重放隔离。
  新增清理实现不代表 P3～P5 已全面验收，sync 默认保持 0。
- 更新器 tests 用 mock Tauri runtime + 真实 loopback HTTP/插件 minisign 校验，只保留合成公钥与签名；
  不运行安装器，不触碰 keyring。更新下载 URL 必须属于清单版本的精确 desktop-v 标签；失败复查清空旧候选，
  下载限制还要在返回字节时复核，避免短响应在 select 观察超限信号前完成。
- 冻结兼容回归为 `desktop/scripts/test-server-compat.py`；默认固定 eb845db 全提交，临时 archive/
  Docker 卷构建旧 server/link，验证升级、旧接口/隧道/重启/离线完整备份/purge。`--browser-smoke`
  只冻结静态资源，实际 HTTP/WS 连新服务端；`--desktop-smoke` 只在测试构建接受 loopback fixture。
  正常产品不得接受 `AGENTBOX_SMOKE_COMPAT`。同一 Smoke.app 原生场景必须串行。
- 安装探针可传 `--previous`，使用旧包真实 sidecar 生成隔离状态并由新包读回；未提供旧包必须报告
  skipped，不能算升级通过。Windows NSIS 即使临时 /D 路径也会改注册表，只在明确标记的托管 CI
  临时账号执行。探针不证明 GUI 偏好/keyring/Tauri更新安装或中断恢复。
- 独立服务器恢复管理能力 `sync_recovery_inspect=1` 不启用同步；四个 `sync_orphan_*` IPC 可在
  sync=0 下核对/导出原日志，清理额外要求 recovery_gc。固定当前登录实例、空间属主与原 device，
  每页50条，未知日志可见但不得猜测状态；uncertain有真实且校验通过的before也可导出，不可清理。
  retire复核当前比较摘要，先持久化 inspection_confirmation 再沿原协议清理，保留永久applied收据；
  SQLite IMMEDIATE检查本机同实例/用户/空间/操作的pending引用，不能凭不同URL绕过。该维护入口
  不更新文件或基线；新前端独立于绑定历史，所有导出路径仍由原生picker给出。
- macOS syncfs.Open只允许系统元数据正向核验的内部固定卷，固定diskutil路径、无shell、限时/限输出；
  guard附属于固定Root，CheckIdentity及子Root用fstatfs校验原卷，禁止嵌套挂载到另一卷。热路径不得
  逐文件运行diskutil，也不按diskN缓存成功。卷策略/实测见docs/architecture/desktop-volume-policy.md。
- 整体缩放使用受限 desktop_set_zoom（80%～200%、main WebView），成功才保存renderer偏好；
  原生drop位置须同时除以系统scale factor和应用zoom。IME期间暂缓缩放，Windows Ctrl+C仍中断。
  终端右键/更多/Shift+F10菜单复用受控粘贴，不增加本地路径读取权限。
- Windows↔Linux CI必须由原生Windows PE侧车连接WSL2内实际Linux ELF peer，核对内核/架构/
  本地Linux文件系统及随机run_id，WSL1、跳过或假peer均失败；只用托管临时账号，清理固定临时目录。
  这不替代Windows最低版本、实体设备、真实Explorer/IME或物理断电验收。

### 界面多语言

- 三端共用 `web/src/i18n/core.ts` 的简中/繁中/英文检测与本地偏好；网页与 Vue 适配分别位于
  `web/src/i18n.ts` / `desktop/src/i18n.ts`。不修改服务器语言、时区、协议枚举或用户内容。
- 网页仅翻译显式标记和绑定的 UI 文案；禁止全局扫词替换、DOM 原型拦截或重载页面切语言。
  翻译带用户值时保留转义，聊天附件协议标记不可本地化；日期仍按服务端系统时区查询。
- 词典与编译产物一并维护；`npm run build` 同步生成独立 abox-link 的 `i18n-core.js`。
  改文案跑 `npm run test:i18n` 及合成浏览器/桌面 locale 回归，既有中文断言显式固定 zh-CN。
  详细约定见 `docs/i18n.md`。

### M4/M5 首批维护约定

- `internal/chat.Executor` 不依赖 server；Environment 必须在每次 exec 前重新授权。只有 app-server 提交回合前失败才可回退；取消/中断或无法保持推理选项时不得启动 exec。收据/用量事务仍沿 server 原顺序收尾。
- 错误与聊天字段优先改 `contracts/chat-errors-v1.schema.json`，运行 `python3 scripts/generate-contracts.py`；Go `protocol`/store、TS 运行时解析与三语字典一起核对。不是全量 API 生成，未知协议不能降级重发。
- `features/chat/history.ts` 拥有请求、deadline 和代际；重载/切换/退出须 cancel，异步 prepare 和收尾均要核对当前代际。禁止只按空间对象引用判断是否切换。
- 桌面 remote.rs 仅转发校验后的错误码/操作编号/retryable，不透传原始 error/hint。网页与桌面状态共用 contracts/workspace-state.ts；running 优先，stop_reason=idle 才是休眠。共享契约改动须跑桌面测试。
- 验证层为 pr / linux-integration / release-full；后者不替代签名、实机、付费或人工门槛。race.chat 包含关闭生命周期。新 npm 子测试在 verification_catalog.COVERED 登记。
- 冻结兼容脚本按 checkout SchemaVersion（可显式 --expected-schema）校验迁移，不能从待测 DB 自己推断预期；旧二进制应拒绝高 schema。冻结静态页的 Chromium 测试仅可向已校验 loopback 源授予 local-network-access，不能放宽生产浏览器策略。

### M4 候选镜像行为门槛

- `internal/agentprobe` 嵌入固定合成驱动器，Docker ValidateCLIImage 使用不可变 ID、network=none、UID 1000、只读根、受限 tmpfs/caps/资源与清空的镜像 ENV；禁止挂账号、宿主目录或 Docker socket。每次正常/失败/取消均清理本次容器，清理失败不得切换。
- 实际 CLI 输出需经现有 Adapter/RunCodexTurn/usage 解析核对；固定合成用量只进入内存记录器，不能接真实 store/ledger。未知格式失败并记录白名单 image_validation_failed，不能当作零费用成功。
- imageupdate 手动/每日自动/回退共用门槛，报告与父目录同步成功后才 Config CAS 并保存验证过的 image ID；保留候选标签，不靠可变标签激活。新报告字段不是已发布协议，真实 provider/MCP 仍另行验收。
- 独立 `--check-agent-image <本地镜像>` 在配置/服务初始化前分流，旧二进制未知 flag 必须失败；旧自动脚本先构建唯一候选，调用此命令后才改标签，不能提前覆盖活动或旧版本标签。该脚本最后复查不是 Docker CAS，不与其他镜像写入者并发。
- 可发现入口 docker.cli-candidate（显式 --image）与 ui.image-update-gate；发布候选工作流构建运行时后执行门槛并保留失败证据。独立检查不修改配置/DB、不拉镜像、不使用真实凭证。

### M5 桌面错误与实时语言

- remote.rs 的 JSON/上传/下载 HTTP 错误只读 ≤16 KiB、最多额外 2 秒；超限/断流/停顿仍保留 HTTP 分类和合法响应头操作编号，不透传原始 error/hint。WS 握手复用白名单解析已有正文，连接期限不变；不得按关闭帧原文猜错误码。
- 桌面显示错误用 errorNotice → messageRef，保留结构化键到渲染时翻译，嵌套 msg 参数也延迟翻译；诊断等需要固定字符串时才用 errorMessage。只捕获安全字段，不保存原始错误对象或重读后来变化的任务参数。
- native-problems.json 是常见本机 HTTP/网络提示白名单，kind＋文案都匹配才翻译；任意路径/服务器/终端原文保持字节含义。错误 retryable 只能收窄原有重连规则，不授权重试同步/上传写入。
- Smoke.app 三语场景只在 desktop-smoke/VITE_AGENTBOX_SMOKE=1 中运行：合成失败登录验证实时错误/编号与输入保留，已有终端验证切语言不重开/改写内容。legacy/projects 串行执行，结束恢复普通 renderer；这不是正式签名、真实 IME 或最低系统验收。

### M4 生命周期、恢复与稳定性

- chat.Service 不依赖 HTTP/server，Observation.Finish 必须直接 defer 以保留 panic recover；用量结算先于最终回执，room.end 最后执行。server 保留属主/持久接收准入和具体 Runtime 适配，不新增第二条执行路径。
- features/chat/sender 冻结输入与 owner，异步校验/旧连接唤醒后重新核对；stream 清理实时和回放 RAF，回调不得操作后来 block。设置 controller 属于一次登录，requests 串行写入、前后废弃旧 GET，退出阻止排队写入；取消已提交请求不等于服务端回滚。所有 disposer 必须幂等且不能清理新实例。
- docker.recovery-drill 使用显式 --recovery-image，自有 privileged/private-cgroup 的真实 systemd 容器，挂 Docker socket 仅供现有挂载检查；不挂宿主应用/systemd/cgroup 目录、不调用宿主 systemctl、不全局 prune。release.deployment 的 systemctl 仍为模拟。规模/计时/备份排程假设不等于管理员已接受 RTO/RPO。
- verification_stability.py 在同一 Linux runner 连续执行完整 PR profile；Go 用 -count=1，允许编译缓存，禁止结果缓存。保留全部失败，少于20次/缺步骤/源码工具平台变化/超过600秒不能通过。聚合单测不代表实际采样，新增 CI 不代表已执行。

### 工作台首次分步引导

- first-task.ts 使用原生 manual popover 浮层与独立高亮，不能再给工作台预留大面板高度；首次创建后自动展示，按 draftScope（旧端回退 origin/user）保存已展示标记，禁止用 bearer token 作为偏好键。更多操作可主动重看；示例仅在显式点击后填入且不覆盖已有输入。
- 关闭/切换/退出取消定位 RAF，销毁 ResizeObserver 和监听；支持 Escape、焦点返回、visualViewport 与 reduced-motion。三语、首次/刷新/后续空间、窄屏和无自动模型调用由 browser.onboarding 回归。
- 草稿设置在 composer 工具栏，正常保存说明收进弹层；存储失败、禁用保存、附件问题及恢复通知仍显示。不可为了压缩布局隐藏阻止发送的问题。

- 消息确认区默认单行，详情按需展开并限制列表高度；待确认/待核对优先于执行状态，错误文本不能被省略号隐藏。查询按钮只查不展开，不改变原 ID 重试/准入规则；切换空间/线程和发送新任务收起详情，轮询保留展开和键盘焦点。草稿、附件、语音入口统一 18px 图标与 34px 点击区。
