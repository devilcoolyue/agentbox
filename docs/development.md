# 开发与贡献指南

[返回文档目录](README.md) · [项目首页](../README.md)

## 开发环境

- Go 工具链按 [`go.mod`](../go.mod) 使用当前 `1.26.6`；启用 `GOTOOLCHAIN=auto` 时，较旧的 Go 可自动下载所需工具链。
- 修改主控制台需要 Node.js 和 npm，CI 使用 Node.js 22。
- 完整运行需要 Linux 与 Docker；macOS 可执行构建和单元测试，但不能替代 Linux 容器链路验证。
- 用户资料、账号凭证、配置和真实数据库不用于测试夹具。

## 仓库结构

```text
agentbox/
├── cmd/
│   ├── agentbox/            服务端入口；internal/app 管理启动与数据锁
│   └── abox-link/           本机面板 / 无头隧道入口
├── internal/
│   ├── agent/              CLI 命令、凭证、home 模板、Codex app-server
│   ├── app/                启动、独占锁与依赖清理
│   ├── workspace/          空间生命周期、活动引用与回收
│   ├── credentials/        账号凭证同步、续期与账号锁
│   ├── usage/              用量解析、定价快照与终端补记
│   ├── backup/             在线快照、备份校验与恢复
│   ├── safefs/             基于 os.Root 的受限文件操作
│   ├── gitx/               容器内 Git 执行策略
│   ├── archivex/           压缩包解压与目录打包
│   ├── config/             配置加载、校验和原子写回
│   ├── dockerx/            容器、exec、PTY、镜像和资源数据
│   ├── linkapp/            abox-link 面板、重连与开机自启
│   ├── server/             HTTP/WS、鉴权、响应转换、代理与隧道
│   ├── store/              SQLite、迁移、用量与额度账本
│   ├── tunnel/             yamux、白名单与 TCP 映射
│   └── web/static/         主控制台 HTML / CSS / JS 与资源
├── web/src/                主控制台 TypeScript 源码
├── images/agent/           工作空间镜像、tmux 与 Claude HUD
├── scripts/                镜像、客户端、备份与域名辅助工具
├── deploy/                 systemd、部署脚本与生产参数模板
└── docs/                   用户与开发文档
```

涉及核心链路前阅读 [`AGENTS.md`](../AGENTS.md)，其中说明了凭证轮换、配置原子更新、用量口径、文件路径验证和前端约定。

## 构建与基本检查

```bash
go build ./...
go vet ./...
go test ./...

# 指定输出二进制
go build -o agentbox ./cmd/agentbox
go build -o abox-link ./cmd/abox-link
```

Go 测试默认不执行真实模型调用。需要验证本机 Codex app-server 时可显式运行：

```bash
CODEX_LIVE_TEST=1 go test -run TestRunCodexTurnLive ./internal/agent/
```

这个测试要求本机已安装并登录 Codex，会真实调用模型并消耗额度，不属于常规离线校验。

Linux 上试跑使用[快速开始](../README_CN.md#快速开始)的镜像和配置，保持与已有服务不同的监听端口与 `data_dir`。同一数据目录有进程锁，不能给两个实例共用。

## 主控制台前端

```bash
npm ci
npm run check
npm run build
```

主控制台使用 TypeScript，由 `tsc` 逐文件编译到 `internal/web/static/js/`，无额外打包器。产物提交到 Git，Go 通过 `go:embed` 嵌入，生产发布只需要 Go。

热加载时让服务端读取磁盘静态资源，在另一个终端监听 TS：

```bash
# 终端一：使用已准备好的开发配置
AGENTBOX_WEB_DIR=internal/web/static ./agentbox -config config.json
```

```bash
# 终端二
npm run watch
```

该运行方式要求当前用户具有运行服务所需的 Docker、目录和属主设置权限；使用 sudo 时通过 `sudo env AGENTBOX_WEB_DIR=...` 显式传入变量。

改动后刷新浏览器。完成后必须再次 `npm run build` 并提交 JS 产物，CI 会检查源码与产物是否一致。

### 前端约定

- `web/src/types.d.ts` 定义 API / WebSocket 报文类型，服务端结构变动时同步修改。
- `web/src/globals.d.ts` 声明 xterm、KaTeX 等脚本全局。
- 对话框使用 `askConfirm`、`askPrompt` 和 `toast`，不新加原生 alert / confirm / prompt。
- 下拉选择使用统一 `select.ts`，动态插入控件后增强，代码赋值使用 `setSelectValue`。
- 服务启动时按内容生成 `/_v/<hash>/` 静态资源前缀，ES Module 相对引用继承该前缀。不要只更新 HTML 而漏掉对应 JS。

## abox-link 是独立构建

abox-link 的前端在 `internal/linkapp/static/`，使用原生 JavaScript，随 `cmd/abox-link` 独立嵌入。修改主控制台不需要重建客户端；修改客户端面板则需重新编译客户端二进制。

```bash
go build -o abox-link ./cmd/abox-link
./abox-link
```

无参数默认在 `127.0.0.1:7801` 打开本机面板。跨平台构建：

```bash
./scripts/build-clients.sh
```

支持目标为 Linux amd64 / arm64、macOS amd64 / arm64、Windows amd64。脚本先完成全部目标构建，再把文件移到 `data/abox-link/`；在线发布时先升级服务端，保证配对接口兼容。

## 关键实现约定

| 范围 | 修改时要保持的行为 |
| --- | --- |
| 配置 | 通过 `Config.mutate` / `ApplySettings` / 账号方法变更，校验和原子写盘一致 |
| 鉴权 | 区分公开、已登录、管理员与空间属主；写请求不接受 query token |
| 文件 | 校验路径与符号链接，宿主机文件属主保持 `1000:1000` |
| 凭证 | 账号 env 在执行时注入，不能写死到容器创建环境里 |
| 计费 | 金额用整数微美元，用量插入与额度扣减同事务，保留幂等性 |
| SQLite | 迁移兼容已有数据库，不仅修改 CREATE TABLE |
| 终端 | 网络断开保留 tmux；新环境不能追溯修改运行中的 Shell |
| 文档 | 功能、配置、API 或部署变动同步更新对应用户手册 |

主控制台和 abox-link 各自包含 logo / 吉祥物「盒仔」资源。修改品牌素材时，同步核对 `internal/web/static/img/`、`internal/linkapp/static/img/` 和主页面内联模板。

### 服务生命周期与模块边界

服务收到 SIGINT/SIGTERM 后停止接收新任务，给在途聊天回合最多 2 秒请求中断并收尾，然后取消后台任务、断开 WebSocket/代理/隧道连接，等待已接收用量落库后释放数据库和数据目录锁。重启不会停止会话容器或终端 tmux；不能将服务退出等同于容器内进程全部终止。

工作空间的创建、启停、删除与空闲回收由统一工作区服务协调；容器停止或删除失败会返回错误并保留记录，避免界面显示成功但容器仍在运行。

凭证刷新、同步与保存由独立凭证服务管理，并按账号串行。成功续期后立即播发到有授权的已有会话，不再被日常同步的时间戳容差跳过。

用量归一化、定价与终端扫描已集中到 `internal/usage`。新记录保存入账价格快照；SQLite 使用事务化版本迁移，遇到更高 schema 版本会拒绝打开。升级前请验证备份，旧二进制回退限制见 [数据库迁移说明](architecture/database-migrations.md)。

## CI 与提交

文件安全改动需执行 `go test -race ./internal/safefs ./internal/archivex ./internal/agent ./internal/server`。macOS 上还可运行 Linux 容器回归：

```bash
./scripts/test-filesystem-linux.sh
```

脚本构建最小测试镜像，交叉编译并运行 safefs、archivex、agent、server、backup 测试；不挂载源码、账号或用户工作区，不调用模型。测试容器禁用外网，结束时删除容器及本机临时二进制；测试镜像保留复用。镜像安装 Git、coreutils 与时区数据，不替代完整生产镜像验收。

Git 容器执行有一个不调用模型、不挂载宿主机目录的 Linux 冒烟测试；CI 自动运行，本地可按需执行：

```bash
docker build -t agentbox-git-test:local -f internal/dockerx/testdata/git.Dockerfile internal/dockerx/testdata
AGENTBOX_DOCKER_TEST_IMAGE=agentbox-git-test:local go test ./internal/dockerx -run '^TestGitContainerLive$' -count=1 -v
```

它验证真实 Docker exec 的 uid、Git 提交/diff、hook 禁用和继承环境清理，测试结束自动删除临时容器；镜像保留用于重复测试。Go Docker 客户端读取 `DOCKER_HOST`，不会自动读取 CLI 的 context；非默认 Docker context 需要设置对应地址。该测试使用最小 Alpine Git 镜像，不替代生产 Debian Agent 镜像、会话启动和 systemd 的完整验收。

[`ci.yml`](../.github/workflows/ci.yml) 会执行前端类型检查与构建一致性、Go build / vet / test、abox-link 各平台交叉编译，以及按仓库策略执行 govulncheck。

提交前检查：

```bash
git diff --check
git status --short
```

PR 描述说明触发场景、行为变化和验证结果。涉及 Linux Docker、真实 OAuth 或真实模型的行为，应注明实际验证范围；本机编译通过不等于线上链路已经验证。

生产发布流程见[部署与运维](../deploy/README.md)。文档和代码提交本身不代表需要立即部署。

## 备份恢复回归

`go test ./internal/backup ./cmd/agentbox` 覆盖带 WAL 的数据库快照、系统/完整范围、外部凭证、清单哈希、拒绝损坏归档、停止条件和恢复时不覆盖现有实例。

```bash
./scripts/test-backup.sh
```

脚本临时构建服务端，创建纯测试数据库和凭证，实际调用 backup / backup-verify / restore 以及定时脚本，检查恢复结果与轮转；不读取真实配置、不连接生产、不调用模型。

设置 `AGENTBOX_BACKUP_DOCKER_TEST=1` 额外验证完整备份命令及工作区恢复，需要可达的 Docker daemon（非默认 context 设置 `DOCKER_HOST`）。该验证只读取 Docker 挂载列表，不操作已有容器；CI 默认开启。

## 真实终端回归

`scripts/test-terminal.mjs` 将当前网页的 xterm 接到临时 Docker 容器，验证空 home 下的 Shell 默认配置、vi/Vim 的插入、退格、方向键、冒号命令、保存、窗口缩放与 tmux 重连。API 使用合成数据，不读取账号或调用模型；需要本机 Docker Unix socket 和 Playwright，结束后删除测试容器。

```bash
docker build -t agentbox-agent:terminal-test images/agent
AGENTBOX_TERMINAL_IMAGE=agentbox-agent:terminal-test \
  AGENTBOX_PLAYWRIGHT_MODULE=/tmp/agentbox-docs-browser/node_modules/playwright/index.mjs \
  AGENTBOX_BROWSER_CHANNEL=chrome node scripts/test-terminal.mjs
```

Playwright 安装方法见下节，截图保存在 `output/playwright/`。这项测试使用真实 Docker PTY，但 API 和 WebSocket 桥接由测试脚本提供，不覆盖 Go 服务端鉴权或会话生命周期。

## 文档截图

README 的截图由 [`scripts/capture-readme.mjs`](../scripts/capture-readme.mjs) 渲染当前 `internal/web/static/` 生成，使用本机临时 HTTP 服务和合成 API / WebSocket 数据，不连接生产、不调用模型、不需要 Docker。终端输出、对话、用户与费用均为演示数据，不是实际模型或性能测试结果。

使用与浏览器回归相同的 Playwright 环境（依赖放在仓库外，不修改项目依赖）：

```bash
npm install --prefix /tmp/agentbox-docs-browser playwright
AGENTBOX_PLAYWRIGHT_MODULE=/tmp/agentbox-docs-browser/node_modules/playwright/index.mjs \
  AGENTBOX_BROWSER_CHANNEL=chrome node scripts/capture-readme.mjs
```

以上使用本机 Chrome；也可先执行 `/tmp/agentbox-docs-browser/node_modules/.bin/playwright install chromium`，再省略 `AGENTBOX_BROWSER_CHANNEL` 使用 Playwright Chromium。仅安装所需浏览器，不需要账号登录。

脚本生成桌面 1440 × 960 和手机 390 × 844 截图到 `output/playwright/readme/`，检查页面异常与遗漏的 API 夹具。逐张检查后将 PNG 复制到 `docs/images/`，随文档一起提交。变更页面布局时重拍受影响页面，不能手工修改截图伪造界面。截图中的时间按生成当天计算，避免“当天”用量筛选与演示记录冲突。

## Git 密钥维护与容器网桥验收

先备份配置、数据库及 `data/git-secrets/`，停止服务，再运行：

```bash
agentbox git-key-rotate --config /path/to/config.json
# 若轮换中断，用当前活动密钥继续重加密，不再新增密钥
agentbox git-key-rotate --config /path/to/config.json --resume
```

命令检查 data_dir 锁并验证现有密文；不会自动停止服务。输出只有密钥版本 ID 与处理数量。旧密钥保留用于中断/历史数据恢复，不能将此命令当作泄露密钥销毁流程。完成后应另做一份完整配套系统备份。旧二进制不能解密 V2 密文，不可直接回退。

真实 Linux 容器网桥测试（临时容器，无宿主挂载、真实凭证或外部仓库）：

```bash
docker build -f internal/dockerx/testdata/git.Dockerfile -t agentbox-git-test:local .
AGENTBOX_DOCKER_TEST_IMAGE=agentbox-git-test:local go test ./internal/server -run '^TestGitBridgeContainerLive$' -count=1 -v
```

测试使用合成 TLS Git smart HTTP 与 SSH 服务，经过真实容器 Git、宿主能力网桥、公司 CA/SSH pin 校验，验证 clone/fetch/ff-only pull、网页预览与显式 push、容器 Python 终端授权命令。默认网桥地址为 `host.docker.internal`，Linux 可用 `AGENTBOX_DOCKER_BRIDGE_HOST` 指定容器可达的服务端地址；服务端测试临时监听 `0.0.0.0`。此测试不替代真实 GitHub/GitLab 注册应用、企业 SSO 和实际内网的现场联调。

## Claude MCP 回归

```bash
go test ./internal/mcpconfig ./internal/server ./internal/workspace ./internal/backup
npm run check
npm run build
# 沿用上文安装在仓库外的 Playwright
AGENTBOX_BROWSER_ONLY_MCP=1 \
  AGENTBOX_PLAYWRIGHT_MODULE=/tmp/agentbox-docs-browser/node_modules/playwright/index.mjs \
  AGENTBOX_BROWSER_CHANNEL=chrome node scripts/test-browser.mjs
python3 scripts/test-mcp-live.py --image agentbox-agent:claude-2.1.280-codex-0.145.0
# Go 服务端到 Docker 的 API 集成；binary 必须匹配 Docker daemon 的 Linux 架构
python3 scripts/test-mcp-server.py --binary /tmp/agentbox-linux --image agentbox-agent:claude-2.1.280-codex-0.145.0
```

`test-mcp-live.py` 使用临时容器、空 home、禁用外网和本地模拟 Anthropic API，验证原生配置写入、stdio/HTTP JSON/SSE 工具发现、Claude 实际工具调用、取消与超时清理。`test-mcp-server.py` 用独立 Docker volume 启动真实 Go 服务，覆盖 HTTP 管理接口、已运行空间同步、冲突接管与删除，并清理自己的容器和卷。两者不读真实凭证、不发付费模型请求，也不代表生产第三方 MCP 的网络或 OAuth 已验收。
