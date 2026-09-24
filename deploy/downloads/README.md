# Agentbox 下载与安装

Agentbox 提供浏览器中的 AI 编码工作空间，支持 Claude Code / Codex CLI、对话、终端、文件、Git、账号池及内网反向隧道。

本仓库仅提供安装入口、用户说明和二进制发布包。下载页面：<https://github.com/devilcoolyue/agentbox-releases/releases>。

## Linux 一键安装

在目标服务器执行（root 用户可以去掉 `sudo`）：

```bash
curl -fsSL https://raw.githubusercontent.com/devilcoolyue/agentbox-releases/main/install.sh | sudo bash
```

默认安装最新正式版本。固定版本或只监听本机：

```bash
curl -fsSL https://raw.githubusercontent.com/devilcoolyue/agentbox-releases/main/install.sh | sudo bash -s -- --version v0.1.1 --listen 127.0.0.1:8180
```

要求 Linux x86_64 / arm64、systemd 和本机 Docker Engine。Ubuntu 22.04+、Debian 12+ 自动安装缺失依赖和 Docker；其他发行版需预装 Python 3.9+、Git、curl、CA 证书、tzdata 和 Docker。服务端使用预编译包，宿主机无需 Go 或 Node。

首次安装会校验发布包、构建固定版本的 Claude/Codex 工作空间镜像、自动安装随包附带的五个平台 abox-link 客户端、生成管理员密码、安装 systemd 服务并设置开机自启。镜像构建需要访问基础镜像仓库、Debian 软件源和 npm，可能耗时数分钟。

完成后访问 `http://服务器IP:8180`，使用终端显示的 `boxadmin` 和随机初始密码登录，在「系统设置 → 账号池」添加账号，再创建工作空间。默认监听 `0.0.0.0:8180`；远程访问需在防火墙/安全组放行 TCP 8180，公网长期使用请配置支持 WebSocket 的 HTTPS 反向代理。管理员密码首次创建后保存在数据库中，修改配置里的初始密码不会重置已有账号。

## 一键卸载与重新安装

适用于默认目录的一键安装，兼容 v0.1.0。默认停止并禁用服务、移除本安装的工作空间容器，把程序、配置、凭证和数据移到 `/var/backups/agentbox-uninstall/<时间>/`（仅 root 可读），让原目录可用于全新安装。保留 Docker、镜像、其他容器和防火墙规则。备份目录与安装目录需在同一文件系统；自定义目录/服务覆盖配置会拒绝自动卸载。

```bash
curl -fsSL https://raw.githubusercontent.com/devilcoolyue/agentbox-releases/main/uninstall.sh | sudo bash -s -- --yes
curl -fsSL https://raw.githubusercontent.com/devilcoolyue/agentbox-releases/main/install.sh | sudo bash -s -- --version v0.1.1
```

新安装会生成新密码，旧数据保留在备份中，不自动导入。只预览卸载计划用 `--dry-run`；确定不需要数据时加 `--purge --yes` 永久删除本次安装的配置、凭证及工作区（不删除以前的卸载备份）。没有 `--yes` 时从终端询问确认。

v0.1.1 安装器自动恢复 SELinux 程序标签，无需关闭 SELinux。SELinux 启用时需提供 `restorecon`（`policycoreutils`）。旧包启动失败可以先执行 `sudo restorecon -R /opt/agentbox` 后重试激活。

## 运行维护

| 内容 | 位置 |
| --- | --- |
| 配置 | `/etc/agentbox/config.json`（仅 root 可读） |
| 数据与工作空间 | `/var/lib/agentbox` |
| 缓存 | `/var/cache/agentbox` |
| 程序版本 | `/opt/agentbox/releases/<版本>` |
| 当前版本 | `/opt/agentbox/current` |

```bash
sudo systemctl status agentbox
sudo journalctl -u agentbox -f
sudo systemctl restart agentbox
```

安装器仅用于首次安装：发现已有目录或服务就退出，不覆盖数据。下载或构建镜像失败且尚未写入部署目录时，可以重跑。已写入配置的失败安装需保留文件并查看日志；若版本目录已安装完成，用对应版本的 `deploy/release.py activate --version <版本>` 重试激活。

停止服务端不会停止工作空间容器或终端 tmux。一个数据目录只允许一个服务端实例。不要移动运行中容器挂载的数据目录。

## 手工安装与升级

从 Releases 下载匹配架构的包与同一版本 `SHA256SUMS`。例如 Linux arm64：

```bash
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf agentbox_v0.1.1_linux_arm64.tar.gz
```

必须确认所选安装包校验为 `OK`。SHA-256 检查完整性，不是独立数字签名。新安装可以运行解压包中的安装器，仍会下载并验证所选版本：

```bash
sudo bash agentbox_v0.1.1_linux_arm64/install.sh --version v0.1.1
```

后续升级时，将新包解压到新的目录，使用包内工具（把路径和版本替换为实际值）：

```bash
sudo python3 /绝对路径/新版本包/deploy/release.py install --package /绝对路径/新版本包
sudo python3 /绝对路径/新版本包/deploy/release.py activate --version vX.Y.Z
```

升级会检查配置和数据库兼容性、备份、切换版本并重启服务，HTTP/WebSocket 会短暂断开；失败不会自动回滚。不要覆盖已有版本目录。工作空间镜像单独管理，服务端升级不自动更新镜像。回退也使用 `activate`，只允许兼容当前数据库的版本。

## 备份

在线系统备份覆盖数据库、配置、账号凭证与模板，不包含完整工作区：

```bash
sudo /opt/agentbox/current/agentbox backup --config /etc/agentbox/config.json --output /安全备份目录/system.tar.gz
sudo /opt/agentbox/current/agentbox backup-verify /安全备份目录/system.tar.gz
```

完整备份需先结束任务、停止相关工作空间容器及 agentbox 服务，再添加 `--full`；仅停服务端不足以取得完整一致性备份。`restore --to <不存在的新目录> <备份包>` 恢复到新目录，恢复后需检查配置、凭证和容器挂载再启动。安装器不自动配置定时备份。

## abox-link 客户端

下载对应系统/架构的 `abox-link` 包并校验、解压。无参数运行打开本机控制台；通过浏览器中的内网隧道页面取得配对信息。无头模式用 `abox-link --help` 查看参数。

发布平台：Linux amd64/arm64、macOS amd64/arm64、Windows amd64。v0.1.1 起安装/升级会把随包编译好的五个平台客户端放到 `/var/lib/agentbox/abox-link/`，控制台直接提供下载，不需要服务器安装 Go 或用户自行编译。

## 许可证与验证范围

发布包携带 `LICENSE`、`NOTICE` 及 `third_party/` 声明。工作空间镜像由用户本机从官方源下载 CLI 构建，Claude Code、Codex CLI 和模型服务遵循各自条款。

发布验证覆盖包校验、Linux 服务登录、工作空间、文件/Git、终端用量、重启、备份恢复与迁移。v0.1.1 增加客户端安装、默认保留数据卸载、彻底卸载与重装回归。安装器自动化演练中的镜像构建和 systemctl 为模拟调用，干净主机上的 apt 安装与开机自启尚未验收。
