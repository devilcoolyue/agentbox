# 部署

宿主机上的 systemd 单元、logrotate 配置与部署脚本。单元文件里的绝对路径用
`__APP_DIR__` 占位，由 `install.sh` 按仓库实际位置注入 —— 这样单元不会和目录
脱节。

## 目录内容

| 文件 | 装到哪 |
| --- | --- |
| `agentbox.service` | `/etc/systemd/system/` |
| `agentbox-image-update.service` | `/etc/systemd/system/` |
| `agentbox-image-update.timer` | `/etc/systemd/system/` |
| `agentbox-backup.service` | `/etc/systemd/system/` |
| `agentbox-backup.timer` | `/etc/systemd/system/` |
| `agentbox.logrotate` | `/etc/logrotate.d/agentbox` |
| `production.env.example` | 生产参数模板，复制为 `production.env`（不入库）后填真实值 |
| `install.sh` | 装单元、`daemon-reload`、设开机自启 |
| `deploy.sh` | 构建、换二进制、重启、验证（含 `/api/ping` 应用级探活） |

## 首次部署

前置：Docker、Go 1.26+、python3、sqlite3（备份取一致快照用）。配置与账号凭证的
准备见根目录 `README.md` 的「部署」一节，这里只管进程托管。

```bash
sudo ./deploy/install.sh   # 装单元，路径按当前目录注入
sudo ./deploy/deploy.sh    # 构建 + 启动
```

## 日常发布

改完代码：

```bash
sudo ./deploy/deploy.sh
```

它会构建、备份旧二进制（保留最近 3 份 `agentbox.bak-*`）、用 rename 换新的、
重启服务，然后确认服务 active 且监听端口已绑定。起不来会打印日志尾部和回滚
命令，并以非零码退出。

## 迁移到新目录或新机器

**换目录后必须重跑 `install.sh`**，否则单元还指向旧路径：

```bash
sudo systemctl stop agentbox
mv /old/path/agentbox /new/path/agentbox
cd /new/path/agentbox
sudo ./deploy/install.sh
sudo ./deploy/deploy.sh
```

`deploy.sh` 会核对单元里的 `WorkingDirectory` 和当前目录是否一致，不一致就
拒绝执行并让你先跑 `install.sh`。

## 单实例保证

同一个 `data_dir` 只允许一个 agentbox 进程，靠 `data/agentbox.lock` 上的
flock 保证。第二个实例会在启动第一步就退出：

```
data dir /root/workspace/agentbox/data is already in use by pid 20141
if you meant to restart the service, use: systemctl restart agentbox
```

锁由内核在进程退出时释放，崩溃不会留下陈旧锁。**重启服务一律走
`systemctl restart agentbox`，不要手动跑二进制** —— 手动实例即使被锁挡下，
也会让 systemd 那边多几次无谓的重启。

## 崩溃循环保护

`agentbox.service` 带 `StartLimitIntervalSec=60` / `StartLimitBurst=5`：不可
恢复的启动失败（端口被占、配置错误）重试 5 次后停进 `failed` 状态，不再无限
重启。查看：

```bash
systemctl status agentbox
journalctl -u agentbox -n 50
```

修好之后要先 `systemctl reset-failed agentbox` 再 `start`。

## 备份与恢复

`install.sh` 会一并装上 `agentbox-backup.timer`，每天凌晨跑 `scripts/backup.sh`，
把关键状态打包进 `<data_dir>/backups/`（保留最近 14 份，产物权限 0600）：

- `state.db` — 用 `sqlite3 .backup` 取的一致快照（sessions/users/tokens）
- `config.json` — 含 `auth_token` 与账号 env 密钥
- `accounts/` — OAuth 凭证（刷新令牌轮换制，丢失需逐账号重新授权）

> 备份内容含密钥，别把 `backups/` 暴露出去。设环境变量 `BACKUP_REMOTE=user@host:/path`
> 可让脚本额外用 `rsync` 把每份备份推到异机（异地容灾），`BACKUP_KEEP` 改保留份数。

手动备份：`sudo ./scripts/backup.sh`。恢复（服务停机下操作）：

```bash
sudo systemctl stop agentbox
cd /path/to/agentbox
tar -xzf data/backups/agentbox-backup-YYYYmmdd-HHMMSS.tar.gz -C /tmp/restore
cp /tmp/restore/state.db   data/state.db      # 覆盖数据库
cp /tmp/restore/config.json .                 # 如需恢复配置
cp -a /tmp/restore/accounts .                 # 如需恢复凭证
sudo systemctl start agentbox
```

恢复后确认 `journalctl -u agentbox` 无报错、`/api/ping` 返回 204。

## 排查

日志分四处：

- `/var/log/agentbox.log` — 服务自身 stdout/stderr，按周轮转保留 8 份
- `journalctl -u agentbox` — systemd 视角的启停与失败原因
- `/var/log/agentbox-image-update.log` — 每日镜像更新检查
- `/var/log/agentbox-backup.log` — 每日数据备份

正常关闭会记一行 `received terminated, shutting down`；没有这行而进程没了，
说明是崩溃或被 SIGKILL，不是运维停的。

注意 journald 默认只存内存（无 `/var/log/journal`），重启后 `journalctl` 就
看不到上个 boot 了。跨重启排查要看 `/var/log/messages`（rsyslog 写的，持久
保留）。
