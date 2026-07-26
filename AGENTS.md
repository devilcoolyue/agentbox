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
| `internal/store` | SQLite(`data/state.db`)：sessions/users/tokens；首次打开会导入旧版 `state.json`。 |
| `internal/dockerx` | Docker Engine API 封装：容器生命周期、exec PTY/stream、stats、镜像/挂载检查。 |
| `internal/agent` | Claude/Codex 适配层：headless 命令、标题生成、凭证播种、Claude HUD、Codex app-server 协议。 |
| `internal/archivex` | 上传压缩包解压（防 zip-slip/符号链接/解压炸弹）与工作区 zip 下载。 |
| `internal/tunnel` | yamux 隧道协议、白名单、端口映射。 |
| `internal/linkapp` | abox-link 客户端实现：配置、面板、守护/自启、重连监督器。 |
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

### 终端

- `GET /api/sessions/{id}/term` 自动启动会话后 `docker exec` 进容器。
- 默认 attach 到持久 tmux 会话 `main`（老镜像回退 bash），断线重连不丢 shell/agent 状态。
- WebSocket 二进制帧是原始 PTY 字节；文本帧只接受 `{"type":"resize","cols":...,"rows":...}`。

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
- 会话镜像内禁用 CLI 自升级（`DISABLE_AUTOUPDATER=1`）；Claude/Codex 版本由 `images/agent/Dockerfile` 与 `scripts/auto-update-image.sh` 管理。
- `config.json`、`accounts/`、`data/` 含密钥和运行时状态，已在 `.gitignore`；不要提交。
- 若改动影响用户可见行为、部署步骤、API 或配置字段，同步更新 `README.md`（必要时也更新 `deploy/README.md`）。
