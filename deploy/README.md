# 部署与运维

[项目首页](../README.md) · [文档目录](../docs/README.md) · [配置参考](../docs/configuration.md)

生产目标是 **Linux + systemd + 本机 Docker Engine**。服务端是一个 Go 二进制，前端随二进制嵌入；工作空间在独立 Docker 容器中运行。以下命令在服务器的 agentbox 仓库根目录执行，默认使用 `config.json`、`data/` 和 `127.0.0.1:8180`，自定义路径时需相应调整。

## 部署前准备

- 安装 Docker Engine、Git、Go（版本以 `go.mod` 为准）、Python 3、curl 和提供 `ss` 的 iproute2。
- 确认 Docker daemon 已启动，执行构建镜像的用户有 Docker 权限。
- 用于构建的服务器能访问 Go 模块源、容器镜像源、Debian 软件源和 npm。
- 准备账号订阅凭证或 API Key；也可先以空账号池启动，再从网页配置。
- 如需域名访问，准备 DNS、TLS 证书与支持 WebSocket 的反向代理。

`sqlite3` 命令行是可选依赖，备份脚本可退回 Python 的 SQLite 在线备份 API。生产服务器不需要 Node.js；改动 TypeScript 后应在开发机生成并提交 JS 产物。

配套服务以 root 运行，以便管理 Docker 和挂载目录属主。运行数据与 Docker socket 都属于服务器管理边界，容器不挂载 Docker socket。

## 首次部署

按[快速开始](../README.md#快速开始)克隆源码、构建 `agentbox-agent:latest` 并准备 `config.json`。如果正在前台试跑，先正常退出该实例，再托管给 systemd：

```bash
sudo ./deploy/install.sh
sudo ./deploy/deploy.sh
```

`install.sh` 根据仓库实际路径替换 `__APP_DIR__`，安装单元、执行 `daemon-reload` 并启用服务和两个定时器；主服务由 `deploy.sh` 构建并启动。

| 文件 | 作用 / 安装位置 |
| --- | --- |
| `agentbox.service` | `/etc/systemd/system/`，主服务 |
| `agentbox-image-update.service` / `.timer` | 每日检查 CLI 版本并更新镜像 |
| `agentbox-backup.service` / `.timer` | 每日备份核心状态 |
| `agentbox.logrotate` | `/etc/logrotate.d/agentbox`，日志轮转 |
| `production.env.example` | 维护者使用的生产参数模板 |
| `install.sh` | 安装或刷新单元及其仓库路径 |
| `deploy.sh` | 构建、替换二进制、重启和应用探活 |

启动后访问首页，以 `boxadmin` 和首次启动时的 `auth_token` 登录。之后可在「系统设置 → 安全与访问」修改密码、创建普通用户；账号配置见[账号与模型](../docs/accounts-and-models.md)。

## HTTPS 与 WebSocket 反向代理

服务保持监听回环地址，公网由反向代理提供 HTTPS。聊天、终端和 abox-link 都依赖 WebSocket，必须保留 `Host`、处理 Upgrade，并设置适合长连接的超时。

已有 Nginx HTTPS 站点可在对应 `server` 内使用以下片段；证书、域名和 80 → 443 重定向按现有部署配置：

```nginx
client_max_body_size 600m;

location / {
    proxy_pass http://127.0.0.1:8180;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

```bash
sudo nginx -t
sudo systemctl reload nginx
```

`client_max_body_size` 应高于应用上传限制并留出 multipart 开销。若外层还有 CDN / 网关，它们的上传上限和 WebSocket 超时也会影响实际体验。

仓库的 `scripts/enable-domain.sh` 是特定环境的辅助脚本：依赖已安装的 Nginx、Certbot、firewalld、可用的 `/var/www/certbot` HTTP challenge 入口及正确 DNS。它会写 Nginx 配置、改监听地址、重启 agentbox 并关闭公网业务端口。先阅读脚本并核对环境，不要把它当作通用的依赖安装器。

## 日常更新

服务发布会重启 agentbox 并短暂断开 HTTP / WebSocket，正在执行的网页任务可能受影响，应安排在合适的维护窗口。

1. 检查本地 / 服务器仓库的已有改动，确认这次发布包含哪些文件。
2. 运行 `go build ./...`、`go test ./...`；涉及前端时运行 `npm run check` 和 `npm run build` 并保留产物。
3. 备份数据库、配置、凭证与需要保留的用户文件。
4. 已提交代码使用仅快进更新，再执行部署脚本。

```bash
git status --short --branch
git pull --ff-only origin main
sudo ./deploy/deploy.sh
```

如有已跟踪文件修改或快进失败，先查明来源再处理；不要用强制重置或清理命令覆盖服务器上已有内容。

部署脚本会备份最近 3 份旧二进制为 `agentbox.bak-*`，用 rename 原子替换新文件，重启服务并检查 `/api/ping`。失败时打印日志和回滚命令，**不会自动回滚**。这些二进制备份不包含数据库或工作区。

只有首次安装、systemd 单元变更 / 缺失或仓库迁移后才需重新执行 `install.sh`；日常更新使用 `deploy.sh` 即可。该脚本不会编译 TypeScript，也不会构建会话镜像或 abox-link。

### 从开发机定向发布

生产参数放入被忽略的 `deploy/production.env`，按 `production.env.example` 填写 `PROD_SSH`、`PROD_DIR`、`PROD_LISTEN`、`PROD_DOMAIN`、`PROD_URL`，不把真实地址提交到版本库。

```bash
source deploy/production.env
ssh -o BatchMode=yes "$PROD_SSH" 'systemctl is-active agentbox'
ssh -o BatchMode=yes "$PROD_SSH" "git -C '$PROD_DIR' status --short --branch"
```

已提交并推送的代码：

```bash
ssh -o BatchMode=yes "$PROD_SSH" "git -C '$PROD_DIR' pull --ff-only origin main"
ssh -o BatchMode=yes "$PROD_SSH" "'$PROD_DIR/deploy/deploy.sh'"
```

上例要求远程用户具备部署所需权限；非 root 用户按服务器配置使用 sudo。

需要预览未提交改动时，先本地构建前端，再用 `rsync -azR` **逐项列出本次涉及的文件**，保持相对路径。以下仅演示两个静态文件，按实际改动调整：

```bash
rsync -azR \
  internal/web/static/index.html \
  internal/web/static/css/shell.css \
  "$PROD_SSH:$PROD_DIR/"
ssh -o BatchMode=yes "$PROD_SSH" "git -C '$PROD_DIR' diff --check"
ssh -o BatchMode=yes "$PROD_SSH" "'$PROD_DIR/deploy/deploy.sh'"
```

不要同步整个仓库或使用 `--delete`。配置、账号和运行数据不属于源码发布文件；前端修改要同步编译后的 `internal/web/static/js/*.js`。

## 镜像与客户端更新

工作空间 CLI 禁用容器内自升级，由镜像统一管理：

```bash
./scripts/build-image.sh

# 对比 npm 最新版；发现版本变化时重建镜像
./scripts/auto-update-image.sh
```

`agentbox-image-update.timer` 每日运行版本检查，日志在 `/var/log/agentbox-image-update.log`。运行中空间不被直接打断；镜像变化后，停止再启动空间时会使用新镜像。

如果需要控制 CLI 版本，可以手动传入 Dockerfile 的构建参数，并按自己的发布策略管理自动更新定时器：

```bash
docker build -t agentbox-agent:latest \
  --build-arg CLAUDE_VERSION=YOUR_CLAUDE_VERSION \
  --build-arg CODEX_VERSION=YOUR_CODEX_VERSION \
  images/agent
```

`YOUR_*_VERSION` 替换为实际 npm 版本。镜像构建参数控制会话 CLI，与服务端版本独立。

提供 abox-link 下载时，先升级服务端，再运行：

```bash
./scripts/build-clients.sh
```

脚本为 Linux amd64 / arm64、macOS amd64 / arm64、Windows amd64 构建客户端，发布到仓库的 `data/abox-link/`。若 `data_dir` 指向其他位置，将生成文件复制到实际的 `<data_dir>/abox-link/`。下载目录按请求读取，更新客户端文件不必重启服务端。

## 备份与恢复

### 内置备份覆盖什么

`agentbox-backup.timer` 每天约 04:17（宿主机时区，另加最多 20 分钟随机延迟）运行 `scripts/backup.sh`，产物写入 `<data_dir>/backups/`，默认保留 14 份、权限为 0600。

| 内容 | 内置脚本是否覆盖 |
| --- | --- |
| SQLite `state.db`，含用户、空间、令牌、用量、额度和账本 | 是，通过在线备份 API 取得一致快照 |
| 仓库根目录 `config.json` | 是 |
| 仓库根目录 `accounts/` | 是（目录存在时） |
| 网页创建账号的 `<data_dir>/creds/` | **否，需额外备份** |
| `users/` 内项目、home、聊天记录、共享目录、用户模板 | **否，需额外备份** |
| 服务器级 `home-template/` | **否，需额外备份** |
| 其他位置的 `credentials_dir` | **否，按实际配置备份** |

```bash
sudo ./scripts/backup.sh

# 可选：更改保留份数，并推送到自己的备份服务器
sudo env BACKUP_KEEP=30 BACKUP_REMOTE=user@backup-host:/backups/agentbox \
  ./scripts/backup.sh
```

定时任务的环境变量可通过 `systemctl edit agentbox-backup.service` 的 `[Service]` / `Environment=` 配置；交互 Shell 的变量不会自动传给 systemd。

SQLite 使用 WAL 模式，**不要直接复制运行中的 `state.db` 作为备份**，那可能漏掉已提交数据。内置脚本使用 SQLite 在线备份 API，并检查快照表和数据。

### 完整备份

完整恢复还需要用户文件与全部凭证，不能仅依赖内置备份包。建议按实际 `data_dir` 制定文件系统快照或归档流程，覆盖上表中未自动备份的项目并保留文件属主、权限和符号链接。

需要数据库与工作区处于同一时间点时，应先让用户结束任务、停止工作空间容器，再停止 agentbox，取得数据库快照和文件备份。**仅停止服务端不等于停止容器**，tmux / CLI 可能仍在写 home 和 workspace。归档不要递归包含不断增长的 `backups/` 自身。

备份同时含源码、账号令牌与配置密钥，应使用受控目录保存；异机备份与定期恢复演练能确认本机损坏后仍可恢复。

### 恢复核心状态

以下示例针对默认 `data_dir=data`。先停止相关空间和服务，把当前状态保留到单独目录，再恢复选定快照；不要把快照直接覆盖到仍运行的 SQLite 上。

```bash
sudo systemctl stop agentbox
sudo mkdir -p /var/tmp/agentbox-restore
sudo tar -xzf data/backups/agentbox-backup-YYYYmmdd-HHMMSS.tar.gz \
  -C /var/tmp/agentbox-restore
sudo ls -la /var/tmp/agentbox-restore
```

确认包内容和时间点正确后，先将当前 `state.db` 及存在的 `state.db-wal`、`state.db-shm` **一并移至一个新的保留目录**，再将快照的 `state.db` 放回数据目录。不能把旧 WAL 留在恢复后的新数据库旁边。

按需要恢复 `config.json` 和 `accounts/`；同时从额外的文件备份恢复匹配的 `creds/`、`users/`、模板和外部凭证目录。旧 OAuth 刷新令牌可能已失效，恢复后需逐账号检查，必要时重新授权。

```bash
sudo systemctl start agentbox
sudo systemctl is-active agentbox
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8180/api/ping
```

探活预期 `204`。再登录检查用户、空间、文件、聊天历史和额度，不能只凭首页可访问判断恢复完成。

## 迁移与回退

同一个 `data_dir` 只允许一个服务进程，`agentbox.lock` 上的 flock 由内核自动释放。重启使用 `systemctl restart agentbox`，不要同时手动启动第二个二进制。

迁移仓库目录前，先停止空间容器和服务，并备份。旧容器的 bind mount 保存了宿主机绝对路径，移动目录后需核对并重建相应容器；数据库与用户目录也要整体迁移。

```bash
cd /new/path/agentbox
sudo ./deploy/install.sh
sudo ./deploy/deploy.sh
```

重新安装单元才能更新 `WorkingDirectory` 和 `ExecStart`。还要检查配置中的绝对 `data_dir` / `credentials_dir`、自定义备份路径与反向代理。

二进制回退可使用 `agentbox.bak-*` 中确认可用的版本，但不能假定旧程序兼容新数据库结构。涉及数据迁移的版本，应使用匹配版本的数据库与文件备份，或先验证兼容性，再恢复服务。

## 日志与健康检查

```bash
systemctl is-active agentbox
systemctl status agentbox --no-pager
journalctl -u agentbox --since '5 minutes ago' --no-pager -n 30
tail -n 50 /var/log/agentbox.log
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8180/
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8180/api/ping
curl -sS -o /dev/null -w '%{http_code}\n' --connect-timeout 10 --max-time 20 \
  https://box.example.com/
```

成功标准：systemd 为 `active`、本机与公网首页都为 `200`、`/api/ping` 为 `204`，最近日志没有启动失败。还应实际打开一个空间验证聊天和终端的 WebSocket；HTTP 探活不能覆盖 Docker、凭证和模型调用。

| 日志 / 页面 | 查看内容 |
| --- | --- |
| `/var/log/agentbox.log` | 服务 stdout / stderr，主要业务日志 |
| `journalctl -u agentbox` | systemd 启停和失败原因 |
| `/var/log/agentbox-image-update.log` | 镜像版本检查与构建 |
| `/var/log/agentbox-backup.log` | 自动备份结果 |
| 系统设置 → 运维监控 | 服务器和工作空间运行指标 |

主服务 60 秒内启动失败 5 次会进入 `failed`，修复原因后再执行：

```bash
sudo systemctl reset-failed agentbox
sudo systemctl start agentbox
```

journald 是否跨重启持久化取决于宿主机配置，不能默认存在历史 boot 日志；需长期追踪时保留文件日志并核对 logrotate。更多症状见[常见问题](../docs/troubleshooting.md)。
