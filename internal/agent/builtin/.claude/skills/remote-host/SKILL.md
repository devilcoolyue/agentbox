---
name: remote-host
description: 把编译、测试、渲染、训练等重活交给用户自己的机器（经 abox-link 隧道 SSH 连接），用 abox-remote 执行命令、跑长任务、传文件。工作空间资源不够、需要显卡或大数据、或者要用用户本机已有的工具链时使用。
---

# 在用户自己的机器上干活

工作空间的 CPU、内存、磁盘有限，也没有显卡。用户可以把自己的电脑（Linux / macOS / Windows）接进来，你用 `abox-remote` 在那台机器上执行（在 `~/.local/bin`；PATH 上找不到时用全路径 `~/.local/bin/abox-remote`）。`abox-remote --help` 有完整用法。

## 什么时候用

- 编译或测试大工程、需要显卡、数据有好几 GB；或者要用用户机器上现成的工具链、授权软件、本地数据。
- 小改动、几秒钟能跑完的命令，照常在工作空间里做，不用这个。

## 开始之前

1. `abox-remote hosts` 看有没有配置好的机器；没有就停下来，把下面「接入」一节的步骤告诉用户，不要自己猜。
2. `abox-remote doctor` 逐项检查隧道、sshd、登录、工作目录。有 `FAIL` 时把那一行原样告诉用户，请用户处理；不要绕过（比如改用其他地址或端口）。

## 用法

```bash
abox-remote exec -- 'nproc; df -h .'          # 短命令，在机器的工作目录里执行
abox-remote push src Makefile --to proj        # 上传到 <工作目录>/proj
abox-remote run build -- 'cd proj && make -j16' # 长任务，打印任务编号
abox-remote wait --timeout 600                 # 等最近一个任务结束；退出码就是任务的退出码
abox-remote log -n 80                          # 看日志尾部
abox-remote pull proj/out/report.txt           # 取回结果
```

- 命令在 Linux/macOS 上用 `sh` 执行，在 Windows 上用 PowerShell 执行（`doctor` 会显示系统类型）。Windows 任务遇到第一个错误就停下，`err.log` 最后一行写明停在哪条命令。
- 超过几十秒的事一律用 `run`。连接断开、工作空间重启都不影响任务继续跑。
- **等待长任务时**，用 Bash 工具的后台模式（`run_in_background`）执行 `abox-remote wait <编号>`，结束时会通知你，期间可以做别的事。不要写 `until …; sleep …` 轮询循环，也不要在前台一直等。
- `wait` 的退出码：0 成功；130 已取消；69 丢失（进程没写下结果就没了，比如被杀或机器重启，先看日志再决定是否重跑）；124 超时（任务还在跑）；255 连不上机器。
- 同时有几台机器时用 `-H <名字>` 选。

## 数据放哪

- 流量要经过 agentbox 服务器中转。**大文件留在用户机器上**，只取回日志、报告、截图这类小结果。超过 1 GB 时工具会提醒。
- 源码同步优先用 git（例如在用户机器上 `git pull`）；`push` 不会删除对方多出来的文件。
- 远端路径都相对于机器的工作目录（默认 `~/agentbox`），不能用绝对路径或 `..` 跳出去。

## 规矩

用户的机器是私人电脑。不在工作目录以外写东西，不安装软件，不改系统设置，不读与任务无关的文件。需要装依赖或改配置时，说清楚要做什么，请用户自己做。

## 接入（告诉用户）

1. 在 abox-link 面板勾选「允许 agentbox 通过 SSH 连接本机」，保存（面板里没有这个选项就是旧版，先升级 abox-link）；本机开启 SSH 服务。macOS 开的是「远程登录」，不是「远程管理」，并要把账号加进它的允许访问名单。
2. 在工作空间执行 `abox-remote setup <名字> --user <本机账号>`。它会打印公钥和各系统的设置步骤：推荐给 agentbox 单独建一个普通账号，只让它写工作目录。单独建账号还不够：macOS 的普通账号都在 staff 组，主目录默认组内可读，提醒用户在自己的账号里执行 `chmod 700 ~`。
3. 用户把公钥加进那个账号的 `authorized_keys` 后，执行 `abox-remote doctor`。
