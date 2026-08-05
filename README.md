# <img src="internal/web/static/img/logo.svg" width="30" alt=""> AGENTBOX

> 品牌资产：logo 与吉祥物「盒仔」的 SVG 在 `internal/web/static/img/`（页面内联的同源版本见 `index.html` 的 `<template>`）。
> abox-link 是另一个二进制、各自 `go:embed`，所以 `internal/linkapp/static/img/` 存了一份副本——改了品牌资产记得两边都换。

在自己的 Linux 服务器上，用 Docker 按需拉起 **Claude Code / Codex CLI** 编码实例，通过浏览器远程使用：聊天式下发任务、进入容器终端、上传代码包、下载工作区。会话关闭后工作区与对话历史持久保存，随时重新打开继续。

```
浏览器客户端 ──(HTTPS/WSS)──> agentbox 服务端 ──(Docker API)──> 会话容器
   · 对话（headless 流式）        · Go 单二进制               · claude / codex CLI
   · 终端（PTY 透传）            · 账号池调度                 · /workspace   ← 宿主机挂载
   · 上传/下载代码包             · 会话/历史持久化            · /home/agent  ← 宿主机挂载
```

## 核心概念

- **会话（Session）**：一个独立容器 + 一块宿主机持久目录（`data/users/<user>/sessions/<id>/`，含 `workspace` 与 `home`）。停止会话只是停容器，数据不丢；重新打开自动拉起容器并通过 `--resume` 续接对话。
- **共享目录（Shared）**：每个用户一块跨会话共用的目录（`data/users/<user>/shared/`），挂载到该用户所有会话容器的 `/shared`。会话工作区互相隔离，需要在会话间传递代码/产物时放这里；文件页签可切换「工作区 / 共享目录」进行上传下载，也可删除文件/目录，或把它们移动到两个范围内的任意目录（API 加 `?scope=shared`）。
- **主题**：登录页与侧栏主题钮支持跟随系统（默认）、浅色、深色三种模式；桌面端悬停展开另外两个选项，触屏端点按展开。abox-link 本机控制台只跟随系统深浅色，没有切换钮。
- **粘贴图片**：对话输入框和终端里都可以直接 Ctrl+V 粘贴截图。图片自动上传到 `/shared/.images/`，对话里以 `[Image #N]` 占位（发送时替换为容器内路径，Agent 用 Read 工具查看），消息里显示可点击缩略图；终端里直接注入路径文本，路径可点击弹出预览。图片保留 48 小时后由服务端自动清理；**仍被对话记录引用的附件不会被清掉**，历史里的缩略图不会随时间变成失效占位。
- **账号池（Accounts）**：配置多个订阅账号或 API Key，新建会话时选择。凭证在每次启动时从池目录同步进会话 home。
- **技能与 MCP**：容器里是原版 CLI，skill / MCP 按官方方式装即可；每个会话 home 相互独立，要让所有会话都预置，把文件放进 `data/home-template/`（见「技能（Skill）与 MCP」一节）。
- **两种交互**：
  - **对话**：服务端用 `claude -p --output-format stream-json`（或 `codex exec --json`）跑无头回合，事件流经 WebSocket 推给浏览器。一个会话可以开多条**对话线程**（各自独立上下文，可随时切回继续），每条线程落盘为 `chats/<线程id>.jsonl` 并记录自己的 provider 会话 id 供 `--resume` 续聊；旧版单文件 `chat.jsonl` 首次访问时自动迁移。历史加载超时或失败时页面会显示重试入口，并在恢复前暂停发送，避免把消息发进尚未确认的线程。
  - **终端**：浏览器 xterm.js ⇄ WebSocket ⇄ `docker exec` PTY，可选进 Shell 或直接进 Agent 交互界面。
- **长对话定位**：上滚离开底部后，输入框上方会浮出返回最新消息按钮，接近底部时自动收起。
- **变更审查**：工作台「变更」页签直接看 workspace 相对上次提交的改动（文件列表 + 彩色 diff），可一键提交或丢弃（单文件/全部）。默认 `bypassPermissions` 下，这是审查 Agent 改动的主入口，不必切到终端敲 `git diff`。
- **断线与休眠**：对话通道断开时页面顶部出现状态条并指数退避重连，重连后自动补拉断线期间错过的消息；与服务器彻底失联会常驻离线横幅。会话被空闲自动停机后标记为「休眠」（区别于手动停止），直接发消息即自动唤醒并把这条消息发出去。
- **文件管理**：除上传/下载/移动/删除外，还可新建文件夹、重命名、多选与拖拽上传（带进度条）。
- **重命名与检索**：会话可在 ⋯ 菜单里改名；对话线程可改名，历史面板支持按标题搜索。

## 部署

前置：Linux、Docker、git 与 sqlite3（变更审查与数据备份用）、python3（部署脚本用）；
编译还需 Go 1.26+，或直接使用编译好的 `agentbox` 二进制。

部署机不需要 Node：前端 TypeScript 的编译产物已提交进仓库并由 `go:embed` 打进二进制，
`go build` 一步即可。只有改前端源码时才需要 Node（见下方「前端开发」）。

```bash
# 1. 构建服务端与 agent 镜像
go build -o agentbox ./cmd/agentbox
./scripts/build-image.sh

# 2. 准备配置
cp config.example.json config.json
# 修改 auth_token（openssl rand -hex 24）与账号池

# 3. 放置账号凭证
mkdir -p accounts/claude-1
# 把已登录机器上的 ~/.claude/.credentials.json 拷贝到 accounts/claude-1/
# codex 账号则拷贝 ~/.codex/auth.json 到 accounts/codex-1/

# 4. 启动
./agentbox -config config.json
# 浏览器打开 http://127.0.0.1:8180 ，用管理员账号 boxadmin 登录，
# 初始密码 = config.json 里的 auth_token（首次启动时自动建号）
# 登录后可在「系统设置 → 安全与访问」修改密码、创建普通用户
```

上面第 4 步是前台试跑。要托管给 systemd（开机自启、崩溃循环保护、日志轮转），
用 `deploy/`：

```bash
sudo ./deploy/install.sh   # 装单元，路径按当前目录注入
sudo ./deploy/deploy.sh    # 构建 + 启动；日常发布也是这一条
```

单元文件、迁移步骤与排查手册见 [`deploy/README.md`](deploy/README.md)。
注意同一个 `data_dir` 只允许一个实例（flock 保证），重启一律走
`systemctl restart agentbox`，不要在服务运行时手动跑二进制。

### 镜像内 CLI 的升级

容器内禁用了 Claude Code / Codex 的自升级（CLI 装在镜像的 root 目录，
会话用户无权限，且升级会随容器重建丢失），版本统一由镜像管理：

- `scripts/auto-update-image.sh` 对比 npm 最新版与镜像标签，有新版就重建镜像；
- 配套 systemd 定时器每天跑一次（`agentbox-image-update.timer`，单元文件在
  `/etc/systemd/system/`，日志在 `/var/log/agentbox-image-update.log`）；
- 运行中的会话容器不受打断，下次停止再启动时自动换用新镜像。

### 获取订阅账号凭证

在任意一台机器上用目标账号完成 `claude` 登录（或 `codex login`），然后拷贝：

| Agent | 凭证文件 | 放入 |
|---|---|---|
| Claude Code | `~/.claude/.credentials.json` | `accounts/<id>/` |
| Codex CLI | `~/.codex/auth.json`（如使用自定义 provider/中转，还需 `~/.codex/config.toml`） | `accounts/<id>/` |

API Key 方式则直接在账号的 `env` 字段配置（见 `config.example.json`）。

> ⚠️ 合规提示：把消费级订阅账号共享给多个真实用户使用可能违反 Anthropic/OpenAI 的服务条款；多用户服务的合规做法是使用 API Key 计费。自用请自行评估。

## 技能（Skill）与 MCP：home 模板

agentbox 不代管 skill / MCP，容器里就是原版 CLI，按官方方式装即可（`claude mcp add -s user …`、
`~/.claude/skills/<名字>/SKILL.md`、`/plugin` 等）。但**每个会话的 home 都是全新空目录**，
装在会话里的东西只属于那个会话。要预置给多个会话，用 home 模板 —— 它在每次会话启动时
叠加到 `/home/agent`，分两层，后者盖前者：

| 模板 | 位置 | 影响范围 | 谁维护 |
|---|---|---|---|
| 服务器模板 | `data/home-template/` | **所有用户的所有会话** | 管理员，宿主机上改 |
| 用户模板 | `data/users/<user>/home-template/` | 该用户的所有会话 | 用户自己，网页「技能」页签或宿主机 |

```text
data/home-template/
  .claude/
    skills/my-skill/SKILL.md     # 所有 claude 会话都带这个技能
    settings.json                # 例如 enableAllProjectMcpServers
  .codex/AGENTS.md
  .bashrc
```

规则：

- **逐文件按 mtime「谁新用谁」**：容器里改过的文件保留；模板里更新过的文件推送到已存在的会话。
  反过来说，在会话里删掉模板文件不会持久——下次启动又回来。
- **符号链接原样重建、不跟随**，所以大块内容可以指向 `/shared` 而不必每个会话复制一份。
- 可执行位保留（hook 脚本能直接跑）；`.claude/` 只对 claude 会话有意义、`.codex/` 只对 codex
  有意义，放在同一份模板里互不干扰。
- 模板在凭证播种**之前**执行，所以模板里误放的凭证文件压不过账号池；模板出错只记日志，
  不会挡住会话启动。
- 两层模板在写盘**之前**先合并（用户层覆盖服务器层），所以用户模板里较旧的同名文件
  照样能盖住服务器模板 —— mtime 比较只发生在合并结果与会话副本之间。

### 「技能」页签

工作台的**技能**页签（仅 claude 会话）把上面这套东西做成了界面：列出当前会话
`~/.claude/skills` 里的技能（名字、描述、SKILL.md 正文、附带文件），并标出每个技能是
**会话自装**、来自**我的模板**还是**服务器模板**。SKILL.md 默认按 Markdown 渲染（复用对话
那套渲染器），右上角可切「预览 / 源码」；预览时 front matter 里 name/description 之外的键
单独列成小标签，不会被吞掉。

- 范围切到「我的模板」即直接管理 `data/users/<user>/home-template/.claude/skills`，
  用户不用碰宿主机就能把技能铺给自己的所有会话；
- 「安装技能」弹窗有两个来源：
  - **本地上传**：`.md`（单文件技能，存成 `<名字>/SKILL.md`）或 `.zip`/`.tar.gz`
    （技能目录打包，允许外面套一层同名目录），支持拖拽；
  - **官方市场**：浏览 `anthropics/claude-plugins-official`（278 条，可搜索、按分类筛选），
    安装时服务端拉取该条目的源码并把其中的技能装进当前范围。注意**市场的单位是插件**，
    可能只含斜杠命令或 MCP 服务器——这类条目没有技能可装，会提示改用终端
    `claude plugin install <名字>@claude-plugins-official` 装整包；
- 目录仓库浅克隆缓存在 `data/marketplace/repo`，12 小时过期，可在弹窗里点「刷新目录」强制更新；
  拉不动时沿用旧副本，浏览不会整个瘫掉；
- 「复制到我的模板」把会话里调好的技能推给自己的所有会话，「装到本会话」反向把模板技能
  立刻装进正在跑的会话（模板本身要下次启动才铺，这个按钮省掉一次重启）。

服务器模板不在界面里开放：它对全体用户可见，仍由管理员在宿主机上维护。

MCP 的两个注意点：

- **Claude**：用户级 MCP 写在 `~/.claude.json`，服务端只在缺失时生成该文件，不会覆盖，
  会话内 `claude mcp add -s user` 即可长期生效。项目级 `/workspace/.mcp.json` 在 headless
  回合里默认不加载，需要在 `~/.claude/settings.json` 里加 `"enableAllProjectMcpServers": true`。
- **Codex**：`~/.codex/config.toml` 每次启动都会被账号池目录里的同名文件覆盖（凭证播种会把
  `accounts/<id>/` 下所有普通文件拷进去），所以 `[mcp_servers.*]` 要写在
  `accounts/<id>/config.toml` 里，写在会话内或 home 模板里都会被冲掉。控制台改中转站地址
  是行级替换，不会破坏该文件里的其它段落。
- 对话模式每回合都新起一次 CLI 进程，stdio 型 MCP server 每回合都会重新拉起；依赖
  `npx -y` 现拉包的 server 会让每条消息都多等几秒，建议预装到 home 里。

## 配置项

| 字段 | 说明 |
|---|---|
| `listen` | 监听地址。默认只绑 `127.0.0.1`；对外请置于 TLS 反向代理之后 |
| `auth_token` | 管理员 `boxadmin` 的初始密码（首次启动建号用；之后密码存数据库，改这里不生效） |
| `data_dir` | 用户数据根目录（工作区、home、对话历史、state.json） |
| `agent_image` | 会话容器镜像 |
| `permission_mode` | headless 回合的权限模式，容器即沙箱，默认 `bypassPermissions` |
| `container.*` | 每容器资源限制：内存、CPU、进程数、网络 |
| `tunnel.*` | 反向内网隧道（见下）。`enabled` 开关；`proxy_bind` 服务端 SOCKS5 监听地址，须为容器可达，默认 docker 网桥网关 `172.17.0.1:1080`；`proxy_host` 注入容器时用的地址，缺省取 `proxy_bind` 的主机 |
| `proxy_bridge.*` | 账号出口代理的本地 HTTP 桥接（见下）。`bind` 监听地址，须为容器可达，默认 `172.17.0.1:1081`；`host` 注入容器时用的地址，缺省取 `bind` 的主机 |
| `proxies[]` | 出口 IP 代理池；`scheme` 为 `socks5`/`http`/`https`，另有 `host`、`port`、可选 `username`/`password`、`disabled` |
| `accounts[]` | 账号池；`credentials_dir` 放凭证文件，或用 `env` 注入 API Key；`proxy_id` 绑定出口代理 |

## 账号出口 IP 代理

给账号绑一个出口 IP：绑定后，**这个账号的一切官方请求都从该 IP 出去**——容器里
`claude`/`codex` 打的模型接口，以及服务端代发的 OAuth 换令牌、订阅查询、Key 连通性
探测。多个订阅账号共用一台服务器时，这是把它们的来源 IP 分开的办法。

在「系统设置 → IP 代理」里维护代理池（增删改、连通性探测、批量导入导出），再到
「账号池 → 编辑 → 出口 IP 代理」为账号选一个。改动对新对话与新开的终端立即生效，
不必重启容器。

几个需要知道的行为：

- **代理池里多是 SOCKS5，而容器里的 claude 是 Node 客户端，只认 http(s) 代理。**
  所以 agentbox 在网桥网关上起一个本地 HTTP 代理（`proxy_bridge`），容器只跟它说
  HTTP 代理协议，SOCKS5 那一段由服务端走完。代理池非空时自动启动。
- **绑了代理就绝不退回直连。** 代理停用、配置坏掉或桥接没绑上时，请求会直接报错，
  而不是悄悄改用服务器自己的 IP——后者正是绑定代理要避免的事。
- **注入的是全局 `HTTP(S)_PROXY`**（大小写各一份，外加 `NO_PROXY` 放行本机与网桥）。
  也就是说容器里的 `git clone`、`npm install` 同样走这个出口。
- 每个账号在桥接上的口令由服务端 `auth_token` 派生，互不相同，同一台机器上的其他
  容器借不到别人账号的出口 IP。改 `auth_token` 会让所有口令一起轮换。
- 批量导入支持 `scheme://user:pass@host:port`、`host:port:user:pass`、`host:port`
  三种写法，可用 `#名称` 结尾；不写协议按 SOCKS5 处理。导出的文本**含密码明文**。

## 内网反向隧道（abox-link）

云端容器默认到不了「只有你本机能连」的内网（公司内网、局域网设备、本地数据库）。
开启 `tunnel` 后，你在**自己的机器**上跑 `abox-link` 客户端，它通过 WebSocket 拨回
服务器；服务器为每个用户暴露一条共享 SOCKS5 代理（绑在网桥网关上），容器发起的内网
请求经该用户的隧道回到本机、由本机真实拨号——**域名也在本机侧解析**（`socks5h`），
所以只有本机能解析的内网域名同样可用。

```
容器 --socks5h--> 服务器 SOCKS5(172.17.0.1:1080) --yamux/WSS--> 本机 abox-link --> 内网
```

- 开关与状态都在 Web UI：管理员在 系统设置→安全与访问 启用（**即时生效**，无需重启）；
  所有用户侧栏可见「内网隧道」入口——在线状态、活动端口映射、接入指引与各平台
  abox-link 下载（把交叉编译好的二进制放进 `data/abox-link/` 即出现下载按钮）
- 构建客户端：`go build -o abox-link ./cmd/abox-link`

### 用法一：本机控制台（默认，不用碰命令行）

abox-link 不带任何参数运行时，会在 `127.0.0.1:7801` 起一个本机控制台并自动打开浏览器：

1. 在 agentbox 的「内网隧道」弹窗点**生成配对码**（10 分钟有效、只能用一次），复制
2. 双击运行 abox-link，把配对码粘进去——**不用填服务器地址，也不用输密码**
3. 在页面上增删放行规则与端口映射，点「启动」；可勾选**开机自启**与**启动时自动连接**

页面顶部是一条 `agentbox — 本机 — 内网` 的电路：中点就是你这台机器，也是放行规则
生效的那一点（挂着当前规则数）。状态按腿区分——凭证失效只断朝服务器的那一侧，
连接中也只点亮 agentbox 那头的灯，两端都亮才表示电路真的通了。宽屏下运行日志与
规则并排，改完点「保存并应用」后隧道按新规则重连，重连过程在同屏就能看到。

配置存在 `~/.abox-link/config.json`（0600）。配对换回来的是一枚会话令牌，**密码不落盘**；
改过密码后令牌失效，控制台会提示重新配对。控制台只监听回环地址，并要求
`X-Abox-Panel` 头与回环 `Host`/`Origin`，挡掉 DNS 重绑定与网页发起的跨站请求。

开机自启按平台落地为 systemd 用户单元 / LaunchAgent / 登录计划任务，均**不需要管理员权限**。
（Linux 上未开 linger 时注销即停，控制台会把这句提示显示出来。）

### 用法二：命令行（无头机器 / 脚本）

带 `--server` 即走原来的一次性模式，行为不变：

```
ABOX_PASSWORD=<你的密码> ./abox-link \
    --server https://box.example.com --user alice \
    --allow 192.168.1.0/24 --allow db.corp.local:5432
```

`--allow` 可重复，支持 CIDR、主机名、`主机:端口`；放行判断在本机侧强制执行，
即使服务器被攻破也无法把你的机器当作任意内网跳板。每条连接都会打印审计日志。
两种模式共用同一套白名单与重连逻辑，只是前者把开关做成了按钮。
- 隧道在线时，容器内自动注入 `AGENTBOX_INTRANET_PROXY=socks5h://<user>:<secret>@<gateway>`。
  它**不是**全局 `HTTP_PROXY`（避免模型 API 等全部流量绕行你的家宽），智能体按需使用，例如
  `curl --proxy "$AGENTBOX_INTRANET_PROXY" http://gitlab.corp.local/...`。
- 变量随每次 exec 注入，对话每回合都取当前值；终端因为附着在常驻 tmux 上，**已经在跑的
  窗格保持它启动那一刻的环境**（进程改不了自己的环境变量）。所以会话先开、隧道后连时，
  在旧窗格里会看不到该变量。重连终端会把最新值写进 tmux 全局环境，此时开一个新窗口
  （`Ctrl-b c`）或重启智能体即可拿到；隧道断开同理会被清除。
- **端口映射（`--map`）**：psql / mysql / redis-cli 及各类数据库驱动不认 SOCKS，
  用 `--map 3306=10.0.1.5:3306`（可重复，每用户最多 16 条）在网关上开一个原生
  TCP 端口，直通指定内网目标：

  ```
  容器 --tcp--> 172.17.0.1:3306 --隧道--> 本机 abox-link --> 10.0.1.5:3306
  ```

  映射目标自动并入白名单；监听端口须 ≥1024，绑定结果在连接握手时逐条回执到
  abox-link 日志。裸 TCP 无凭证，服务器按**来源 IP** 隔离——只有属主用户自己的
  容器（和宿主机）能连上映射端口，其他用户的容器一律拒绝。隧道在线时容器内注入
  `AGENTBOX_INTRANET_MAPS=<监听地址>=<内网目标>,...`，智能体据此直连。

## API 概览

先 `POST /api/login`（`{"username","password"}`）换取会话令牌；其余接口需
`Authorization: Bearer <token>`（WebSocket 用 `?token=`）。设置与用户管理类
接口仅管理员角色可用，普通用户只能操作自己的会话。

```
GET    /api/sessions                会话列表
POST   /api/sessions                新建 {name, agent, account_id}
POST   /api/sessions/{id}/start     启动容器（幂等）
POST   /api/sessions/{id}/stop      停止容器（数据保留）
PATCH  /api/sessions/{id}           重命名会话 {name}
DELETE /api/sessions/{id}?purge=1   删除（purge 同时删工作区）
POST   /api/sessions/{id}/upload    上传代码包 multipart(file)，zip/tar.gz；clear=1 先清空
GET    /api/sessions/{id}/archive   打包下载 (zip)
GET    /api/sessions/{id}/files     文件列表（含权限/大小/时间）?path=
DELETE /api/sessions/{id}/files     递归删除文件或目录 ?path=
POST   /api/sessions/{id}/files/move  移动文件或目录
                                      {source_scope,source_path,destination_scope,destination_dir}
POST   /api/sessions/{id}/files/mkdir 新建文件夹 {scope,dir,name}
POST   /api/sessions/{id}/files/rename 重命名文件或目录 {scope,path,name}
GET    /api/sessions/{id}/file      读单个文件 ?path=；dl=1 强制下载
PUT    /api/sessions/{id}/file      保存文件内容（body 即内容，上限 16MB）
POST   /api/sessions/{id}/images    粘贴图片上传（multipart file，上限 20MB），
                                    存入 /shared/.images/，48 小时后自动清理
（以上文件类接口均支持 ?scope=shared 操作共享目录，默认工作区）
GET    /api/marketplace            官方插件目录（?refresh=1 强制重拉；含分类列表）
POST   /api/sessions/{id}/skills/market  从市场装技能 {name} ?scope=（插件不含技能时 422）
GET    /api/sessions/{id}/skills    技能列表 ?scope=session|template（source 标明来自会话/模板）
POST   /api/sessions/{id}/skills    安装技能 multipart(file=.md|.zip|.tar.gz, name?) ?scope=
GET    /api/sessions/{id}/skills/{name}         技能详情（SKILL.md 正文 + 附带文件）?scope=
DELETE /api/sessions/{id}/skills/{name}         删除技能 ?scope=
POST   /api/sessions/{id}/skills/{name}/copy    在范围间复制 {to:"session"|"template"} ?scope=
GET    /api/sessions/{id}/git/status  变更列表（分支 + 文件状态；非 git 仓库时 is_repo=false）
GET    /api/sessions/{id}/git/diff    unified diff（?path= 查看单文件）
POST   /api/sessions/{id}/git/commit  git add -A 后提交 {message}
POST   /api/sessions/{id}/git/discard 丢弃改动 {path?}（省略=全部，恢复到 HEAD）
GET    /api/sessions/{id}/history   当前对话线程的历史（含线程元数据）
GET    /api/sessions/{id}/chat/threads              对话线程列表（标题/时间/轮数/是否可续聊）
POST   /api/sessions/{id}/chat/threads              开启新对话线程（旧线程保留可切回）
POST   /api/sessions/{id}/chat/threads/{tid}/activate  切换到指定线程并恢复其上下文
PATCH  /api/sessions/{id}/chat/threads/{tid}        重命名线程 {title}
DELETE /api/sessions/{id}/chat/threads/{tid}        删除线程（删当前线程自动切到最近一条）
WS     /api/sessions/{id}/chat      对话通道（JSON 事件）
WS     /api/sessions/{id}/term      终端通道（二进制 PTY；?mode=shell|agent）
GET    /api/accounts                账号池
PATCH  /api/accounts/{id}           改账号 {label?, env?, proxy_id?}（proxy_id 空串=解绑）
GET    /api/proxies                 IP 代理池 + 桥接状态
POST   /api/proxies                 新增代理 {name,scheme,host,port,username?,password?,disabled?}
PATCH  /api/proxies/{id}            改代理（password 留空=不改）
DELETE /api/proxies/{id}?force=1    删代理（仍被账号绑定时需 force=1，会连带解绑）
POST   /api/proxies/test            连通性探测 {id?} 或直接给字段；返回延迟与出口 IP
POST   /api/proxies/import          批量导入 {text}，每行一条
GET    /api/proxies/export          导出为可再导入的文本（含密码明文）
WS     /api/tunnel                  内网反向隧道（abox-link 客户端拨入；yamux over WSS）
GET    /api/tunnel/status           本用户隧道状态（在线/映射；管理员另见在线用户列表）
POST   /api/tunnel/pair             生成配对码（一次性，10 分钟有效）
POST   /api/tunnel/pair/redeem      用配对码换会话令牌（无需登录：码本身即凭证）
GET    /api/tunnel/clients          可下载的 abox-link 预编译客户端列表
GET    /api/tunnel/clients/{name}   下载客户端二进制（data/abox-link/ 下的文件）
```

## 前端开发

主控制台前端是 TypeScript，源码在 `web/src/`，用 `tsc` 逐文件编译（无打包器）到
`internal/web/static/js/`，产物提交进仓库并被 `go:embed` 打进二进制。

```bash
npm ci                 # 首次或依赖变动时
npm run check          # 类型检查（tsc --noEmit）
npm run build          # web/src/*.ts -> internal/web/static/js/*.js

# 热改：服务端直接读磁盘，另开一个终端跑 watch，改完刷新浏览器
AGENTBOX_WEB_DIR=internal/web/static ./agentbox -config config.json
npm run watch
```

改完前端务必 `npm run build` 并把 `internal/web/static/js/` 一起提交——生产机只跑
`go build`，不会编译 TypeScript；CI 会校验产物与源码一致。

之所以不打包：服务端用内容哈希发布资源（`index.html` 里的 `{{BUILD}}` 在启动时被换成
`/_v/<hash>/` 前缀，其余模块靠原生 ES Module 的相对 import 继承该前缀），一个 `.ts`
对一个 `.js` 才能维持这套可长缓存、且能穿透 Cloudflare 的机制。

`web/src/types.d.ts` 集中定义 API 与 WebSocket 报文类型，每个接口对应 Go 侧一个结构体；
改服务端报文时两边一起改。`web/src/globals.d.ts` 声明 xterm / KaTeX 等由 `<script>`
引入的全局。abox-link 面板（`internal/linkapp/static`）是另一套独立前端，仍是原生 JS。

## 安全模型

- 会话容器：非 root（uid 1000）、`no-new-privileges`、内存/CPU/PID 限额、仅挂载自己的 workspace 与 home。
- headless 默认 `bypassPermissions`——容器本身就是沙箱，这是容器化跑编码 Agent 的通行做法；如需更保守可在配置改为 `acceptEdits`。
- 上传解压有 zip-slip、符号链接、解压炸弹防护。
- 服务端进程需要访问 Docker socket（等价 root），请勿暴露公网；远程访问用反向代理加 TLS。

## 后续扩展（预留）

- 多宿主机：把 dockerx.Manager 换成远程 Docker host 或调度层。
- Codex 适配为尽力实现（事件字段随版本变化），Claude 链路为主。
