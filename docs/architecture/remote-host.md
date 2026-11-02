# 本机算力扩展（abox-remote）一期设计

状态：一期已实现（2026-11-01）。Windows 专属部分（脱离作业对象、taskkill）还需要真机验收，见「验收」。

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

## 改动一：abox-link「本机」目标（已实现）

**约定一个保留域名 `self.abox-link`，表示「运行 abox-link 的这台机器」。**

- 服务端：`splitRule` 已经接受这个名字（它是合法的普通域名），透明访问会把它映射到虚拟地址，**不用改**。只在规则归一化时要求它必须带端口（`self.abox-link:22`），不允许不带端口的整机放行，因为本机回环上常有不带认证的开发服务和数据库。
- 客户端（`internal/tunnel/link.go`）：`resolveAndCheck` 先按原规则做白名单匹配；命中 `self.abox-link` 时，拨号地址改成 `127.0.0.1:<端口>`，不走 DNS。白名单的语义不变，仍以客户端为最终授权。
- 面板（`internal/linkapp`）：加一个开关「允许 agentbox 通过 SSH 连接本机（端口 22）」，打开即写入 `self.abox-link:22`；高级模式里可以改端口（比如 Windows 用户另开一个 2222）。
- 兼容：旧客户端收到 `self.abox-link` 会去做 DNS 解析并失败，连接被明确拒绝，不会误连。新客户端在握手里带 `X-Abox-Link-Features: self-target`，状态接口返回 `client_self_target`，隧道页对不支持的旧客户端提示升级。
- 旧客户端的临时办法：端口映射 `--map 2222=127.0.0.1:22` 也能连到本机回环。

这一项还顺便解决了 hyxsbcjg 的 DHCP 问题，他可以按需迁移，不强制。

## 改动二：abox-remote 工具（已实现）

代码：`internal/agent/builtin/.local/bin/abox-remote`，回归：`scripts/test-abox-remote.py`（`policy.abox-remote`）。

### 分发

Python 3 单文件，只用标准库（镜像里已有 `python3` 和 `openssh-client`），和 Skill 一起放在内置 home 模板层（见改动三），落到 `~/.local/bin/abox-remote`，跟着 agentbox 二进制发版，不用重建 Agent 镜像。镜像的 `/etc/agentbox.bashrc` 已把 `~/.local/bin` 加进 PATH，下次重建镜像后生效；在那之前 Skill 让 Agent 必要时用全路径。

### 状态存放

home 和 `/workspace` 按空间隔离，`/shared` 跨空间共享，所以主机配置、密钥和 known_hosts 放在 `/shared/.abox-remote/`（目录 0700，可用 `ABOX_REMOTE_HOME` 改）：

```text
/shared/.abox-remote/
  hosts.json          # default + 名字 → {host, port, user, root, os, ps, tofu}
  keys/<name>(.pub)   # ed25519 密钥
  known_hosts         # doctor 第一次登录时记下主机密钥，之后严格校验
  last-<name>         # 在这台机器上最近起的任务编号，status/wait 不带编号时用
```

ControlMaster socket 放在容器自己的 `/tmp/abox-remote-%C`，不放 `/shared`：停掉的空间留下的 socket 不能挡住别的空间。

### 命令

| 命令 | 作用 |
| --- | --- |
| `setup <名字> [--address H] [--port P] [--user U] [--root DIR] [--default]` | 生成密钥，打印要授权的 `restrict ssh-ed25519 …` 一行和三个系统的设置步骤；地址默认 `self.abox-link`，账号默认 `agentbox`，工作目录默认 `agentbox`（相对远端 home） |
| `doctor` | 逐项检查：能否解析地址、能否读到 SSH 横幅（区分隧道离线和 sshd 未运行）、密钥登录、系统类型（POSIX / Windows 与 PowerShell 版本）、工作目录可写 |
| `hosts` | 列出配置的机器 |
| `exec -- <命令>` | 在工作目录前台执行，输出直接透传，退出码原样返回 |
| `run <任务名> -- <命令>` | 起后台任务，打印任务编号 `MMDD-HHMMSS-<任务名>` |
| `status` / `log [-n N]` / `cancel` / `list` | 状态 / 日志尾部 / 结束整个进程组（树） / 最近的任务 |
| `wait [编号] [--timeout 秒]` | 等任务结束，断线自动重连；结束时打印状态和日志尾部 |
| `push <本地路径>... [--to DIR]` | 上传到工作目录（或其下的 DIR） |
| `pull <远端路径> [本地路径]` | 下载文件或目录 |

`-H <名字>` 选机器；只配了一台或设了默认机器时可省略。退出码：`wait` 返回任务自己的退出码，130 已取消，69 丢失，124 超时（任务还在跑），255 连不上。

远端路径都相对于工作目录，绝对路径、`~` 和跳出工作目录的 `..` 在本地直接拒绝。这只是护栏，真正的边界是远端账号的权限（见「安全」）。

### 远端运行时

本机不装任何东西，每次调用都把运行时脚本发过去：

- **POSIX**：脚本作为 `sh -c` 的参数，stdin 留给文件数据。远端登录 shell 要能接受 POSIX 引号（sh/bash/zsh/dash；fish 不行）。
- **Windows**：cmd.exe 的命令行上限是 8191 字符，所以命令行只放一段固定的引导脚本（`-EncodedCommand`），它从 stdin 读 `<长度>\n<UTF-8 脚本>` 再执行，后面的 stdin 是文件数据。支持 pwsh 7 和 Windows PowerShell 5.1，`doctor` 探测后记进 `hosts.json`。

任务目录 `<root>/.abox-remote/jobs/<编号>/` 里每项状态一个小文件，结果先写临时文件再改名：

```text
name  cmd.sh | cmd.ps1  run.sh | runner.ps1  started  pid  pstart  ended  exit  canceled  out.log  err.log
```

- **脱离 SSH 会话**：POSIX 用 `nohup setsid`，没有 setsid 的 macOS 用 perl 的 `POSIX::setsid`。Windows 用 `CreateProcess` 加 `CREATE_BREAKAWAY_FROM_JOB`；作业不允许脱离时退回普通启动并给出警告。
- **状态**：有 `exit` 就是 succeeded / failed（有 `canceled` 标记则为 canceled）；pid 存活且启动时间对得上（排除 pid 复用，Linux 上的 .NET 时间允许 1 秒误差）就是 running；否则是 lost。
- **取消**：POSIX 杀整个进程组；Windows 用 `taskkill /T /F` 结束进程树。
- **Windows 的执行语义**：任务脚本开着 `$ErrorActionPreference = 'Stop'`（pwsh 7.3+ 还有 `$PSNativeCommandUseErrorActionPreference`），遇错即停，`err.log` 最后一行写 `error: … (at: <那一行>)`，退出码沿用原生命令的退出码。exec 不中断，错误记录以纯文本写到 stderr。输出统一用 `Out-String -Width 4096` 渲染：没有控制台时 pwsh 的宽度为 0，表格会渲染成空行。错误的 ANSI 颜色也已关掉。
- **审计**：exec、start、cancel 都会把时间、操作和命令追加到远端 `<root>/.abox-remote/audit.log`。

### 连接复用与断线

用 OpenSSH 自带的 ControlMaster（`ControlPersist 60`、`ServerAliveInterval 15`），没有另写守护进程。`wait` 每次用短连接查状态，间隔按 2、5、15、30 秒逐步拉长。断线时只提示一次，之后安静重试，恢复后提示已重连。`run` 不自动重试，避免重复起任务。实测停掉 sshd 再拉起，任务照常跑完，`wait` 自动接上。

### 文件传输

- 目录和小文件：tar.gz 流经 SSH 传输。Windows 端先落成临时文件再调 `tar.exe`（Windows 10 1803 起自带），因为 PowerShell 5.1 的管道会破坏二进制数据。完整性靠 SSH 的 MAC 和 gzip 的 CRC。本地解包时拒绝跳出目标目录的条目和链接。
- 单个大文件（大于 32 MB）：按偏移续传到 `<文件>.abox-part`，结束后比对 SHA-256 再改名；校验不过就删掉分段文件并报错。下载同理。
- 不做删除式同步，也不做双向实时同步。源码同步建议用 git，Skill 里写明了。

### 等待任务

Claude Code 的 Bash 工具支持后台执行，命令结束时会通知 Agent。Skill 要求长任务用 `run` 起，再把 `abox-remote wait` 放到后台 Bash 里，不写 `until …; sleep` 循环。Codex 没有这个机制，用带 `--timeout` 的前台 `wait` 分段等待。

## 改动三：内置 Skill 与内置模板层（已实现）

`internal/agent/builtin/` 用 `//go:embed all:builtin` 打进二进制（`internal/agent/builtin.go`）。`workspace.Service` 在进程里第一次需要时，把它整棵写到 `<data_dir>/builtin-template/`（先写临时目录再改名），放在叠加顺序的最前面：

```text
<data_dir>/builtin-template/（内置）→ data/home-template/ → data/users/<user>/home-template/
```

- 只有隧道总开关打开时才加这一层（`templateDirs`）。
- 内置文件的 mtime 统一为 `agent.BuiltinTemplateTime`。改动 `builtin/` 下任何文件都要同时调大这个时间，并更新 `builtinTemplateDigest`（`TestBuiltinTemplateDigest` 会提示新值），否则已有空间里的旧副本永远比它新，收不到更新。用户在空间里改过的副本（mtime 更新）会保留。
- 该目录每次启动都会重新生成，不进系统备份（备份是白名单）。
- 内容：`.local/bin/abox-remote` 和 `.claude/skills/remote-host/SKILL.md`。Codex 一期没有对应的技能文件，只有工具本身和 `--help`。

Skill 要点：何时用、先 `doctor`（失败原样转告用户，不绕过）、长任务后台等待、大数据留在本机、远端是用户的私人机器（不在工作目录外写东西，不装软件，不改系统设置），以及告诉用户如何接入。

## 安全

- **最大的风险是 Agent 在用户本机上执行任意命令**（Agent 常以跳过权限确认的方式运行，还可能被提示注入）。一期的对策：
  - 接入指引**推荐给 agentbox 单独建一个普通账号**，只给 `root` 目录的写权限；用自己的账号也行，但要明确告知风险。
  - 单独建账号不等于隔离：macOS 的普通账号都在 `staff` 组，主目录默认 `drwxr-x---`，这个账号能进入并列出用户的主目录（2026-11-02 实机确认），部分 Linux 发行版的主目录对所有人可读。指引要求用户在自己的账号里执行 `chmod 700 ~`。`exec` 的命令不受路径护栏限制，所以边界只能靠账号权限。
  - `setup` 打印的授权行带 `restrict` 选项，关掉这把密钥的端口转发、agent 转发和 X11 转发；指引还建议 sshd 只监听 127.0.0.1（abox-link 走回环连接，局域网就连不到它）。
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

已完成（2026-11-01）：

- Go 单元测试：`internal/tunnel` 的 self 白名单匹配与回环改写（含经 yamux 流真实拨到本机监听），网段规则覆盖不到 self，不带端口的 self 规则被拒；`internal/netaccess` 的规则校验与虚拟地址；`internal/linkapp` 握手带 `X-Abox-Link-Features`；`internal/agent` 的内置层落盘（权限、mtime、可被服务器模板覆盖）与内容摘要；`internal/workspace` 的内置层随隧道开关生效。
- `policy.abox-remote`：把 SSH 换成本地 `sh -c`，真实执行 sh 运行时；有 pwsh 时也执行 PowerShell 运行时。覆盖 exec、任务成功/失败/取消/丢失、list、目录与大文件双向传输（含续传）、越界路径、审计日志、不安全的归档条目。
- 手工：用户态 sshd（127.0.0.1:2222）上跑通 setup → doctor → exec/run/wait/cancel/push/pull；在任务运行中停掉 sshd 再拉起，任务照常完成，`wait` 自动重连。pwsh 7.4（Linux）上跑通 Windows 运行时除脱离作业对象和 taskkill 以外的部分；`AboxSpawn` 的 C# 能编译。
- abox-link 面板：`browser.link-i18n` 通过。

已完成（2026-11-02，macOS 15.6.1 Apple 芯片，经生产隧道）：`doctor` 全部通过；exec、run/wait/log、失败退出码、cancel（`pgrep` 确认进程已结束，走的是没有 setsid 时的 perl 分支）；关闭 SSH 主连接后任务继续；push/pull 目录、中文和空格文件名、10 MB 校验一致。

实机踩到的坑已写进 `setup` 指引：macOS 要开「远程登录」而不是「远程管理」，账号要加进允许名单；否则密钥通过后 sshd 直接断开，表现为 `Connection closed`，不是 `Permission denied`，`doctor` 据此给不同提示。

待做：

- Windows 真机：在 OpenSSH 会话里起任务后断开，确认任务继续；`taskkill` 取消；PowerShell 5.1；中文文件名经 `tar.exe` 往返；DefaultShell 分别为 cmd 和 PowerShell 两种情况。
- 经真实隧道的断线：任务运行中停掉隧道再恢复，确认任务继续、`wait` 自动重连。
- 种子用户：请 hyxsbcjg 在一个新项目上试用，对比他现在那套的体验和稳定性。

## 已定决策（2026-11-01）

1. 保留名用 `self.abox-link`。
2. Windows 放进一期。
3. 接入指引默认推荐给 agentbox 单独建一个本机普通账号；用自己的账号作为可选项，并写明风险。
4. 一期跟随隧道总开关，不单独做按用户的准入。
