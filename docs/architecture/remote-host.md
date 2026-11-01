# 本机算力扩展（abox-remote）一期设计

状态：设计已定（2026-11-01）。改动一已实现；改动二、三进行中。

## 背景

工作空间容器的 CPU、内存、磁盘和显卡都有限，编译大工程、渲染视频、跑本地数据集这类任务更适合放在用户自己的机器上。生产用户 hyxsbcjg 已经自己搭了一套：Claude 在 agentbox 里写代码，经 abox-link 透明隧道 SSH 回家里的 Windows，在那里渲染，再把截图和报告取回来检查（见其工作区 `docs/环境与工具.md`「agentbox 控制 Windows 制作」）。这套方案验证了需求和整体路线，但除了隧道之外都是他自己的脚本和手工配置，门槛高，也有几个结构性问题：

| 问题 | 原因 |
| --- | --- |
| 只能填本机的局域网 IP，DHCP 一换就断，要同时改三处 | 透明访问拒绝回环目标（`internal/netaccess/policy.go` 的 `splitRule`） |
| 每条命令都要重新握手，很慢 | 他自己写了 `winbridge.py` 常驻会话来解决 |
| SSH 一断，Windows 上的后台任务就被杀 | sshd 把会话进程放进作业对象；他用 `CREATE_BREAKAWAY_FROM_JOB` 解决 |
| 容器里没有 rsync，文件传输要自己写 | 他用 git bundle 加分块 SHA-256 传输 |
| Claude 用 `until …; sleep 3` 轮询任务，一挂十几个小时 | 任务结束后没有通知手段 |

一期目标是把这套做法产品化：用户只需配好本机 sshd，其余由平台提供。

## 目标与非目标

目标：

- 工作空间里的 Agent 能在用户本机（Linux / macOS / Windows）上执行短命令、跑能挂起和恢复等待的长任务、双向传文件。
- 用户不用知道本机 IP；隧道断开重连后任务不丢。
- 本机不装 abox-link 以外的东西，所有远端逻辑都随每次调用临时发送过去。
- 给 Claude 一个开箱即用的 Skill，让它知道什么时候、怎么把重活交给本机。

非目标（留给二期）：

- 不需要 sshd 的原生执行器（abox-link 内置执行）。
- 开机自动恢复中断的任务（一期只把它们标成 `lost`）。
- MCP 工具形式和任务完成的服务端主动推送。
- 一个工作空间同时用多台本机做负载均衡。

## 整体结构

```text
Agent (Claude/Codex)
  └─ abox-remote（~/.local/bin，Python 单文件）
       └─ ssh（ControlMaster 复用连接）
            └─ 透明网络辅助容器 → 用户隧道 → 本机 abox-link
                 └─ 保留名 self.abox-link:22 → 127.0.0.1:22 → 本机 sshd
                      └─ 远端运行时（每次调用临时发送的 sh / pwsh 脚本）
```

一期共三项改动：隧道支持「本机」目标、`abox-remote` 工具、内置 Skill。

## 改动一：abox-link「本机」目标

**约定一个保留域名 `self.abox-link`，表示「运行 abox-link 的这台机器」。**

- 服务端：`splitRule` 已经接受这个名字（它是合法的普通域名），透明访问会把它映射到虚拟地址，**不用改**。只在规则归一化时要求它必须带端口（`self.abox-link:22`），不允许不带端口的整机放行，因为本机回环上常有不带认证的开发服务和数据库。
- 客户端（`internal/tunnel/link.go`）：`resolveAndCheck` 先按原规则做白名单匹配；命中 `self.abox-link` 时，拨号地址改成 `127.0.0.1:<端口>`，不走 DNS。白名单的语义不变，仍以客户端为最终授权。
- 面板（`internal/linkapp`）：加一个开关「允许 agentbox 通过 SSH 连接本机（端口 22）」，打开即写入 `self.abox-link:22`；高级模式里可以改端口（比如 Windows 用户另开一个 2222）。
- 兼容：旧客户端收到 `self.abox-link` 会去做 DNS 解析并失败，连接被明确拒绝，不会误连。新客户端在握手里带 `X-Abox-Link-Features: self-target`，状态接口返回 `client_self_target`，隧道页对不支持的旧客户端提示升级。
- 旧客户端的临时办法：端口映射 `--map 2222=127.0.0.1:22` 也能连到本机回环。

这一项还顺便解决了 hyxsbcjg 的 DHCP 问题，他可以按需迁移，不强制。

## 改动二：abox-remote 工具

### 分发

做成 Python 3 单文件，只用标准库（镜像里已有 `python3` 和 `openssh-client`），**和内置 Skill 一起放进内置 home 模板层**（见改动三），落到 `~/.local/bin/abox-remote`。这样它跟着 agentbox 二进制一起发版，不需要重建 Agent 镜像（镜像改动要整包重建，代价大）。

### 状态存放

home 和 `/workspace` 按空间隔离，`/shared` 跨空间共享，所以主机配置、密钥、known_hosts 和连接复用 socket 都放在 `/shared/.abox-remote/`（目录 0700）：

```text
/shared/.abox-remote/
  hosts.json          # 名字 → {host, port, user, os, root}
  keys/<name>         # ed25519 私钥（0600）和 .pub
  known_hosts
  cm/                 # ControlMaster socket
  jobs/<name>.json    # 本地记录的最近任务，方便 wait 不带编号
```

### 命令

| 命令 | 作用 |
| --- | --- |
| `setup <名字> [--user U] [--port 22] [--root <目录>]` | 生成密钥，打印公钥，并按本机系统给出开 sshd、授权公钥的具体步骤；默认 host 为 `self.abox-link` |
| `doctor [<名字>]` | 逐段检查：隧道是否就绪、SSH 横幅能否读到、主机密钥、登录、远端 shell 类型、`root` 是否可写；每一项都给出失败原因 |
| `exec <名字> -- <命令>` | 在 `root` 下前台执行短命令，退出码原样返回 |
| `run <名字> <任务名> -- <命令>` | 在 `root` 下起后台任务，打印任务编号 |
| `status` / `log [-n N]` / `cancel` / `list` | 查状态 / 看日志尾部 / 结束整个进程树 / 列出任务 |
| `wait <名字> [编号] [--timeout 秒]` | 阻塞到任务结束，断线自动重连；结束时打印状态和日志尾部，退出码等于任务的退出码 |
| `push <名字> <本地路径>... [--to <远端相对目录>]` | 上传文件或目录 |
| `pull <名字> <远端相对路径> [本地路径]` | 下载文件或目录 |

所有远端路径都相对于 `root`；`..` 和绝对路径直接拒绝。这只是工具层面的护栏，真正的边界是远端账号的权限（见「安全」）。

### 远端运行时

本机不装任何东西。每次调用时，`abox-remote` 把一段运行时脚本经 SSH 发过去执行：POSIX 用 `sh`，Windows 用 `pwsh -EncodedCommand`（找不到 pwsh 时退回 Windows PowerShell 5.1）。`setup` 和 `doctor` 自动识别远端系统，结果记进 `hosts.json`。

任务目录放在 `<root>/.abox-remote/jobs/<编号>/`：

```text
cmd            # 任务脚本
status.json    # 先写临时文件再改名；含 pid、进程启动时间、开始/结束时间、退出码
out.log
err.log
```

- **脱离 SSH 会话**：POSIX 用 `setsid nohup`；Windows 用 `CreateProcess` 加 `CREATE_BREAKAWAY_FROM_JOB`（hyxsbcjg 实测不需要管理员权限）。
- **lost 判定**：状态是 running，但 pid 不在了或进程启动时间对不上（pid 被复用），就报 `lost` 并写回状态文件。
- **取消**：POSIX 杀进程组；Windows 用 `Process.Kill($true)` 结束整个进程树。

### 连接复用

用 OpenSSH 自带的 ControlMaster（`ControlPersist 60`、`ServerAliveInterval 15`），一期不另写常驻守护进程。hyxsbcjg 观察到隧道一抖，复用连接上的所有会话会一起断，所以：

- `exec`、`status` 这类幂等操作断线后自动重试一次。
- `wait` 不依赖长连接，每次短连接查一下状态，间隔按 2 秒、5 秒、15 秒、30 秒逐步拉长。
- `run` 不自动重试，避免重复起任务。断线后用 `list` 按任务名对账。

如果实测发现延迟仍是瓶颈，再引入 winbridge 那种常驻会话池。

### 文件传输

- 目录：`tar` 流经 SSH 传输（Windows 10 1803 以后自带 bsdtar），完成后逐文件核对 SHA-256。
- 大文件（大于 64 MB）：分块传输，每块单独校验，中断后可续传。
- 不做删除式同步，也不做双向实时同步。源码同步建议用 git：本机仓库作为 remote，`git push` 后在本机 checkout。Skill 里写清楚这一点，不在一期重复造 git bundle 那套。

### 等待任务时的用法

Claude Code 的 Bash 工具支持后台执行，命令结束时会通知 Agent。Skill 要求：长任务用 `run` 起，然后**把 `abox-remote wait` 放到后台 Bash 里跑**，收到通知再看结果，不写 `until …; sleep` 轮询循环。Codex 没有这种通知机制，就用带 `--timeout` 的前台 `wait` 分段等待。

## 改动三：内置 Skill 与内置模板层

在 `internal/agent/hometemplate.go` 的叠加顺序最前面加一层「内置模板」，内容用 `embed` 打进二进制，优先级低于服务器模板和用户模板，管理员和用户都可以覆盖：

```text
内置模板（embed）→ data/home-template/ → data/users/<user>/home-template/
```

内置层包含：

- `.local/bin/abox-remote`
- `.claude/skills/remote-host/SKILL.md`
- `.codex/` 下对应的说明

Skill 要点：

- 什么时候用：编译、测试大工程，需要显卡，数据大于几 GB，或者要用本机已有的工具链和授权软件。
- 先 `abox-remote doctor`；不通时把失败原因原样告诉用户，不自己猜着修。
- 重数据留在本机，只取回日志、报告、截图这类小结果。
- 长任务的用法按上一节。
- 本机是用户的私人机器：不在 `root` 之外写东西，不装软件，不改系统设置；需要的话请用户自己做。

只有管理员启用了隧道时才放入这一层，没开隧道的部署不会多出用不了的技能。

## 安全

- **最大的风险是 Agent 在用户本机上执行任意命令**（Agent 常以跳过权限确认的方式运行，还可能被提示注入）。一期的对策：
  - 接入指引**推荐给 agentbox 单独建一个普通账号**，只给 `root` 目录的写权限；用自己的账号也行，但要明确告知风险。
  - `authorized_keys` 里给这把公钥加 `restrict,pty` 选项，禁止端口转发和 agent 转发。
  - 本机 abox-link 的审计日志记录每条连接；`abox-remote` 把执行过的命令追加到远端 `<root>/.abox-remote/audit.log`。
  - 要撤销：在面板里关掉开关，或者删掉 `authorized_keys` 里那一行。两者都立即生效。
- 私钥只在 `/shared` 里，是用户自己的数据，不同用户之间隔离。私钥不上传服务端，备份时按用户数据处理。
- 放行范围只有 `self.abox-link:22`（或用户指定的端口），本机回环上的其他服务不受影响。
- 服务端路由仍然不能扩大客户端的授权，这一点和现有透明访问一致。

## 带宽、存储与计费

- 流量路径是 用户家宽 → agentbox 服务器 → 容器，占服务器出入带宽。Skill 和文档都强调只取回小结果；`push`/`pull` 单次超过 1 GB 时打印警告。
- 取回的文件计入工作空间的存储配额，和现在一样。
- 本机算力不计费。Agent 等待任务期间不产生模型费用（后台 Bash 在等），这正是不让它前台轮询的理由之一。

## 验收

- 单元测试：`self.abox-link` 的白名单匹配和拨号改写、不带端口的规则被拒绝、旧客户端拒绝连接；`abox-remote` 的路径校验、状态机和 lost 判定。
- 合成测试：在 `scripts/test-transparent-network.py` 里加一个本机 sshd 目标，跑通 `doctor`、`exec`、`run`、`wait`、`cancel`、`push`、`pull`，以及 wait 期间隧道断开再重连。
- 真机验收：Linux、macOS、Windows 各跑一遍，Windows 要专门验证 SSH 断开后任务继续运行。
- 种子用户：请 hyxsbcjg 在一个新项目上试用，对比他现在那套的体验和稳定性。

## 实施顺序与估算

| 步骤 | 内容 | 估算 |
| --- | --- | --- |
| 1 | abox-link「本机」目标：客户端拨号改写、面板开关、能力标记、隧道页提示 | 1–1.5 天 |
| 2 | `abox-remote`：POSIX 远端的 exec/run/wait/cancel/push/pull/doctor | 2 天 |
| 3 | Windows 远端运行时（pwsh） | 1.5 天 |
| 4 | 内置模板层和 Skill | 0.5 天 |
| 5 | 测试、接入指引（`docs/networking.md` 新增一节）、真机验收 | 1.5 天 |

合计约一周半。第 1 步可以单独先发，现有的手工 SSH 用法立即受益。

## 已定决策（2026-11-01）

1. 保留名用 `self.abox-link`。
2. Windows 放进一期。
3. 接入指引默认推荐给 agentbox 单独建一个本机普通账号；用自己的账号作为可选项，并写明风险。
4. 一期跟随隧道总开关，不单独做按用户的准入。
