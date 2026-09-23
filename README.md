<div align="center">

<img src="internal/web/static/img/logo.svg" alt="agentbox 标志" width="104" height="104" />

# agentbox

**打开浏览器，继续你的 AI 编码工作。**

在自己的服务器上运行 Claude Code 与 Codex CLI。<br />
对话、终端、文件与代码审查，共用一个持久化工作空间。

[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8?logo=go&logoColor=white)](go.mod)
[![TypeScript](https://img.shields.io/badge/TypeScript-7-3178C6?logo=typescript&logoColor=white)](package.json)
[![Docker](https://img.shields.io/badge/Docker-工作空间-2496ED?logo=docker&logoColor=white)](images/agent/Dockerfile)
[![SQLite](https://img.shields.io/badge/SQLite-持久化-003B57?logo=sqlite&logoColor=white)](internal/store)
[![CI](https://github.com/devilcoolyue/agentbox/actions/workflows/ci.yml/badge.svg)](https://github.com/devilcoolyue/agentbox/actions/workflows/ci.yml)

[快速开始](#快速开始) · [使用文档](#使用文档) · [部署与运维](deploy/README.md) · [反馈问题](https://github.com/devilcoolyue/agentbox/issues)

</div>

## 为什么使用 agentbox？

- **编码环境随时可用。** 浏览器连接云端工作空间，CLI、依赖和代码都留在服务器；工作空间停止后，文件、home 配置和对话历史仍然保留。
- **对话与终端自由切换。** 通过流式对话下发任务，也能打开原版 Claude Code / Codex CLI 的交互终端。终端使用 tmux，网络断线后可重新连接。
- **交付过程集中在一处。** 上传项目、预览和编辑文件、查看 Git diff、提交改动、下载产物，不必反复切换工具。
- **多个工作空间共用资源。** 每位用户拥有自己的 `/shared`，可在不同项目间传递文件；home 模板让技能、MCP 与常用配置复用到多个空间。
- **账号统一管理。** 管理员维护订阅账号、API Key、中转配置和出口代理，用户创建空间时选择对应账号。
- **用量有据可查。** 按用户、空间、模型查看 token 与费用，导出 CSV；管理员可配置价目表、充值和余额拦截。
- **需要时连回内网。** 本机运行 `abox-link`，让云端 Agent 访问白名单内的局域网服务、数据库与内部代码仓库。
- **部署依赖清晰。** 服务端是嵌入前端的 Go 单二进制，使用 SQLite 存储状态，通过 Docker 管理空间，配套 systemd 发布和备份脚本。

## 选择适合你的入口

| 入口 | 适合做什么 | 需要安装什么 |
| --- | --- | --- |
| **浏览器对话** | 下发编码任务、查看流式结果、管理多条对话 | 用户只需浏览器；管理员先部署服务端 |
| **浏览器终端** | 使用原版 CLI、执行命令、安装项目依赖 | CLI 和常用工具已包含在工作空间镜像中 |
| **abox-link 本机面板** | 让云端工作空间访问本机可达的内网 | 在自己的电脑上运行 `abox-link` |
| **abox-link 命令行** | 在无头机器上配置内网白名单与端口映射 | 同一个 `abox-link` 二进制，传入 `--server` |
| **HTTP / WebSocket API** | 接入脚本、管理空间与读取使用记录 | 使用登录后取得的 Bearer token |

`abox-link` 是可选的内网连接工具；普通浏览器使用不需要安装它。

## 工作方式

```mermaid
flowchart LR
    B[浏览器] -->|HTTPS / WebSocket| S[agentbox 服务端]
    S -->|Docker API| W["工作空间容器<br/>Claude Code / Codex CLI"]
    S --> D[(SQLite 与聊天记录)]
    F["持久目录<br/>workspace / home / shared"] --- W
    W --> P[模型服务 / 账号出口代理]
    W -. 按需访问内网 .-> S
    S -. WSS 隧道 .-> L[本机 abox-link]
    L --> N[白名单内的服务]
```

一个工作空间对应一个容器和一组持久目录；同一空间可以有多条对话线程，但同一时刻只运行一个网页对话回合。用户级共享目录在该用户的所有空间中挂载为 `/shared`。

## 快速开始

### 1. 准备 Linux 服务器

| 依赖 | 用途 |
| --- | --- |
| Linux + Docker Engine | 运行服务端与工作空间容器；生产托管使用 systemd |
| Go 1.26.6 或兼容的自动工具链 | 从源码构建服务端，版本以 [go.mod](go.mod) 为准 |
| Git | 拉取源码、Git 变更审查、获取技能市场内容 |
| Python 3、curl、iproute2（`ss`） | 发布、探活和备份脚本 |
| OpenSSL | 生成初始管理员密码 |
| Node.js 22 / npm（可选） | 仅修改主控制台 TypeScript 时需要 |

部署机无需编译前端：`internal/web/static/js/` 的产物已经提交，并随 Go 二进制嵌入。备份由二进制内置 SQLite 在线备份 API 完成，定时脚本的轮转使用 Python 3。

每个容器默认限制为 **2048 MiB 内存、2 CPU、512 个进程**；按并发空间数为服务器预留资源。macOS 可用于构建和单元测试，完整容器与 systemd 部署以 Linux 为目标。

### 2. 获取源码并构建

```bash
git clone https://github.com/devilcoolyue/agentbox.git
cd agentbox

go build -o agentbox ./cmd/agentbox
./scripts/build-image.sh
```

镜像构建需要访问基础镜像仓库、Debian 软件源和 npm。构建用户应有 Docker 访问权限；服务启动时还需要为挂载目录设置 `1000:1000` 属主，配套 systemd 单元以 root 运行。

### 3. 初始化配置

```bash
cp config.example.json config.json
openssl rand -hex 24
```

编辑 `config.json`：

1. 将 `auth_token` 换成刚生成的随机字符串，它是首次启动时 `boxadmin` 的初始密码。
2. 初次体验可将 `accounts` 与 `proxies` 都设为 `[]`，登录后从网页添加真实账号。示例中的代理地址与密钥均为占位内容，不能直接使用。
3. 保留 `listen: "127.0.0.1:8180"`，数据默认写入配置文件所在目录下的 `data/`。

可直接使用以下最小配置，先替换密码占位符：

```json
{
  "listen": "127.0.0.1:8180",
  "auth_token": "CHANGE_ME_TO_A_LONG_RANDOM_TOKEN",
  "data_dir": "data",
  "agent_image": "agentbox-agent:latest",
  "accounts": [],
  "proxies": []
}
```

密码占位符会被启动校验拒绝。完整字段、默认值与生效时机见[配置参考](docs/configuration.md)。

### 4. 启动并登录

在 Linux 服务器的仓库目录前台试跑：

```bash
sudo ./agentbox -config config.json
```

服务器本机访问 **<http://127.0.0.1:8180>**。若服务器在远端，可以在自己的电脑另开终端，通过 SSH 转发访问：

```bash
ssh -N -L 8180:127.0.0.1:8180 user@your-server
```

然后在本机浏览器打开相同地址，使用 **`boxadmin` + 配置中的 `auth_token`** 登录。首次建号以后，密码保存在数据库中，修改 `auth_token` 不会重置登录密码。

### 5. 添加账号，创建第一个空间

1. 打开「系统设置 → 账号池」，新增 Claude 或 Codex 账号。
2. Claude 订阅可在网页生成授权链接并粘贴授权码；API / 中转账号填写地址与 Key。Codex 订阅使用已登录 CLI 的 `auth.json`，具体见[账号与模型](docs/accounts-and-models.md)。
3. 点击创建工作空间，填写名称，选择 Agent 与账号。
4. 在「文件」页上传项目，或打开「终端」执行 `git clone`。
5. 在「对话」页发送任务；完成后到「变更」页审查 diff，再提交或下载文件。

### 6. 转为长期运行

结束前台试跑后，在仓库目录执行：

```bash
sudo ./deploy/install.sh
sudo ./deploy/deploy.sh
```

`install.sh` 安装服务、镜像更新与备份定时器；`deploy.sh` 构建、替换二进制、重启并探活。远程长期访问请配置 HTTPS 与 WebSocket 反向代理，完整步骤见[部署与运维](deploy/README.md)。

## 用户与权限

| 能力 | 普通用户 | 管理员 |
| --- | --- | --- |
| 创建空间、对话、终端、文件与技能管理 | 自己的空间 | 自己的空间 |
| 使用共享目录、用户 home 模板、内网隧道 | 自己的资源 | 自己的资源 |
| 选择账号池中的账号 | 仅授权范围内 | 全部账号 |
| 查看使用记录 | 自己的记录 | 全部用户的记录 |
| 管理账号凭证、出口代理、模型、价格与系统配置 | 不可以 | 可以 |
| 创建用户、重置用户密码、管理额度 | 不可以 | 可以 |

账号池由管理员统一维护，每个账号支持「全体用户」「指定用户」「仅管理员」三种使用范围，在账号行的「使用范围」按钮设置。旧配置未设置范围时保持全体共享；管理员始终可用。管理员的工作空间 API 同样受属主校验；系统管理权限不提供跨用户的空间浏览入口。服务器管理员仍可直接访问宿主机持久数据。

撤销账号授权会阻止新操作和后续凭证同步，已交付的凭证及已启动的进程不会自动撤回；彻底撤销还需停止相关容器并在上游轮换凭证。账号凭证会提供给容器内的 CLI，拥有终端访问权的用户可以读取这些凭证。因此，共享账号池适用于可信用户；不要将管理员的订阅或 API 凭证分享给不可信用户。

## 当前支持范围

| 能力 | Claude Code | Codex CLI |
| --- | --- | --- |
| 网页对话与多线程历史 | 支持，使用流式无头回合 | 支持，优先 app-server，握手失败时回退 exec |
| 原版 CLI 终端 | 支持 | 支持 |
| 网页订阅授权 | 支持 OAuth 授权码流程 | 通过 CLI 登录后导入凭证 |
| API Key / 中转配置 | 支持 | 支持，需匹配 provider 协议 |
| 网页对话与自动起标题用量 | 支持 | 支持；费用需按价目表折算 |
| 终端用量补记 | 支持，只记录、不扣余额 | 尚未实现 |
| 网页技能管理 | 支持 `.claude/skills` | 使用 CLI 和 home 模板配置 |

还有几项使用边界：

- **文件持久化不等于进程持久化。** 终端网络断开可续接；停止或重建容器会终止其中进程。应把需要保留的内容放在 `/workspace`、`/home/agent` 或 `/shared`。
- **额度不是实时硬上限。** 网页回合结束时结算，余额见底的这一轮可能超支；终端补记不扣余额，Codex 终端消耗尚未计入。
- **默认系统备份不包含工作区。** 系统备份覆盖数据库、配置、账号凭证与双层模板；`agentbox backup --full` 额外覆盖用户文件和历史，需要先停止服务及相关容器。提供 `backup-verify` 校验和 `restore --to` 恢复到新目录，见[备份与恢复](deploy/README.md#备份与恢复)。
- **生产发布会短暂断开连接。** 当前采用单机、单服务进程与本机 Docker，同一 `data_dir` 只允许一个 agentbox 进程。
- **容器允许 Agent 执行代码。** 默认权限模式为 `bypassPermissions`。容器以非 root 用户运行，设置资源限制与 `no-new-privileges`；服务端具有 Docker 权限，适合由可信管理员部署和维护。
- **Git 审查在会话容器内执行。** 进入审查会按需启动空间，并遵循终端相同的额度入口限制；网页提交不执行 Git hook 或签名，需要这些功能时请在终端提交。
- **网页文件操作不沿符号链接访问。** 工作区、共享目录、技能和凭证文件使用受限目录句柄；上传先在临时目录验证，编辑器保存以原子替换方式写入。模板中的链接仍可由容器内 CLI 使用。

## 使用文档

详细文档均为中文，也可以从[文档目录](docs/README.md)按角色开始阅读。

| 文档 | 内容 |
| --- | --- |
| [工作空间使用指南](docs/user-guide.md) | 对话、终端、文件、HTML 预览、Git 审查与共享目录 |
| [账号与模型](docs/accounts-and-models.md) | OAuth、API Key、Codex 凭证、默认模型与账号生命周期 |
| [配置参考](docs/configuration.md) | 配置字段、默认值、持久目录与生效时机 |
| [技能、插件与 MCP](docs/skills-and-mcp.md) | 技能页、官方市场、双层 home 模板与 MCP 配置 |
| [使用记录与额度](docs/usage-and-quotas.md) | 统计口径、费用明细、价目表、充值、超支行为与 CSV |
| [出口代理与内网隧道](docs/networking.md) | 代理池、abox-link 配对、白名单、端口映射与环境变量 |
| [部署与运维](deploy/README.md) | systemd、HTTPS、更新、备份恢复、迁移与日志 |
| [API 参考](docs/api.md) | 登录、工作空间、文件、聊天、用量与管理接口 |
| [开发指南](docs/development.md) | 仓库结构、构建、前端热加载、测试与贡献约定 |
| [开源重构设计](docs/architecture/opensource-refactor.md) | 模块边界、目录迁移、分阶段实施与验收进度 |
| [常见问题](docs/troubleshooting.md) | 启动、登录、容器、代理、用量、备份与前端排障 |

## 技术栈与本地开发

| 层次 | 技术 |
| --- | --- |
| 服务端 | Go、HTTP / WebSocket、Docker Engine API |
| 主控制台 | TypeScript、原生 ES Modules、xterm.js、KaTeX |
| 持久化 | SQLite、宿主机文件目录、JSONL 聊天记录 |
| 工作空间 | Debian / Node.js 镜像、Claude Code、Codex CLI、tmux |
| 内网连接 | abox-link、yamux、WebSocket、SOCKS5 与 TCP 映射 |
| 部署 | Linux、systemd、HTTPS 反向代理 |

```bash
go build ./...
go test ./...

# 修改前端时执行；CI 使用 Node.js 22
npm ci
npm run check
npm run build
```

修改 `web/src/*.ts` 后必须一起提交 `internal/web/static/js/` 的构建产物。Linux 容器验证、可选真实模型测试与 abox-link 构建见[开发指南](docs/development.md)。

贡献步骤见 [CONTRIBUTING.md](CONTRIBUTING.md)，漏洞私密报告见 [SECURITY.md](SECURITY.md)，变化记录见 [CHANGELOG.md](CHANGELOG.md)，第三方许可见 [third_party/](third_party/README.md)。二进制候选包与安装流程见 [发布说明](docs/releases.md)，固定 CLI 版本见 [兼容矩阵](docs/compatibility.md)。

欢迎提交聚焦具体问题的 Issue 或 PR。反馈时附上平台、代码提交版本、复现步骤和脱敏日志；真实凭证、用户文件与数据库不要放入提交或截图。

## 许可证

Agentbox 采用 [Apache-2.0](LICENSE)，版权声明见 [NOTICE](NOTICE)。第三方组件保留各自许可；模型服务和运行时 CLI 的使用与再分发遵循其上游条款，详见 [第三方说明](third_party/README.md)。
