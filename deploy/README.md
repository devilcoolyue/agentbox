# 部署与运维

[项目首页](../README.md) · [文档目录](../docs/README.md) · [配置参考](../docs/configuration.md)

生产目标是 **Linux + systemd + 本机 Docker Engine**。服务端是一个 Go 二进制，前端随二进制嵌入；工作空间在独立 Docker 容器中运行。以下命令在服务器的 agentbox 仓库根目录执行，默认使用 `config.json`、`data/` 和 `127.0.0.1:8180`，自定义路径时需相应调整。

## 部署前准备

- 安装 Docker Engine、Git、Go（版本以 `go.mod` 为准）、Python 3、curl 和提供 `ss` 的 iproute2。
- 确认 Docker daemon 已启动，执行构建镜像的用户有 Docker 权限。
- 用于构建的服务器能访问 Go 模块源、容器镜像源、Debian 软件源和 npm。
- 准备账号订阅凭证或 API Key；也可先以空账号池启动，再从网页配置。
- 如需域名访问，准备 DNS、TLS 证书与支持 WebSocket 的反向代理。

备份使用服务端二进制内置的 SQLite 在线备份 API，不依赖 `sqlite3` 命令行；定时脚本的轮转依赖 Python 3。生产服务器不需要 Node.js；改动 TypeScript 后应在开发机生成并提交 JS 产物。

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
| `agentbox-image-update.service` / `.timer` | 实验性每日追新，默认不启用 |
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

# 显式选择实验性追新
AGENTBOX_AUTO_UPDATE=1 ./scripts/auto-update-image.sh
```

稳定安装默认禁用 `agentbox-image-update.timer`，镜像使用固定基线；手动启用 timer 后才每日运行最新版检查，日志在 `/var/log/agentbox-image-update.log`。运行中空间不被直接打断；镜像变化后，停止再启动空间时会使用新镜像。

固定版本及回退说明见 [兼容矩阵](../docs/compatibility.md)。需要覆盖基线时可显式传入 Dockerfile 构建参数：

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

### 系统备份与完整备份

`agentbox-backup.timer` 每天约 04:17（宿主机时区，另加最多 20 分钟随机延迟）调用 `scripts/backup.sh` 创建系统备份。实际备份由 Go 二进制完成；脚本增加校验和文件、保留份数和可选远端传输。默认输出 `<data_dir>/backups/`，保留最近 14 份同类型备份，包与 SHA-256 文件权限为 0600。

| 内容 | 系统备份 | `--full` 完整备份 |
| --- | --- | --- |
| SQLite `state.db`（含 WAL 中已提交记录） | 在线备份 API 一致快照 | 相同 |
| 指定的配置文件、同目录 `accounts/` | 是 | 是 |
| `data/creds/`、所有账号配置的外部 `credentials_dir` | 是 | 是 |
| 服务器 home 模板、每个用户的 home 模板 | 是 | 是 |
| 用户 workspace、home、聊天记录、shared | 否 | 是 |
| marketplace 缓存、已编译 abox-link、旧备份、锁文件 | 否，可重新生成 | 否 |

配置的凭证目录缺失、不可读取或扫描中有文件变化会导致备份失败，避免产生静默漏数据的成功包。文件清单保存权限、UID/GID、mtime、链接目标和文件 SHA-256；符号链接原样记录、不跟随，特殊文件（设备、FIFO、socket）会报错，需要先处理这些运行时残留。setuid/setgid/sticky 特殊权限不恢复，不保存扩展属性和 ACL。当前单包最多 100000 个条目、manifest 最大 32 MiB，超限会报错；较大实例需按数据规模规划外部快照方案。

```bash
# 使用已经构建/部署的新版二进制
./agentbox backup --config /etc/agentbox/config.json
./agentbox backup --config config.json --output /backup/system.tar.gz
./agentbox backup-verify /backup/system.tar.gz

# 定时脚本：默认使用仓库下的 agentbox/config.json，也可指定独立部署路径
sudo env AGENTBOX_BIN=/opt/agentbox/current/agentbox \
  AGENTBOX_CONFIG=/etc/agentbox/config.json BACKUP_KEEP=30 \
  BACKUP_REMOTE=user@backup-host:/backups/agentbox ./scripts/backup.sh
```

`backup` 与 `backup-verify` 输出 JSON 结果；备份格式版本和程序提交号在包内 `manifest.json`。新版验证/恢复命令仅处理带 manifest 的新格式，旧版 `state.db/config.json/accounts` 包请按旧格式单独恢复，不要混用。

系统备份适合日常在线运行，但数据库与文件不构成同一时点的全局快照。完整一致性备份要求先让用户结束任务、停止相关空间容器，再停止 agentbox：

```bash
# 先在控制台停止所有需要备份的空间，再停止服务
sudo systemctl stop agentbox
sudo ./agentbox backup --config config.json --full --output /backup/full.tar.gz
sudo systemctl start agentbox
```

命令会持有 `data_dir/agentbox.lock` 的独占锁，并在归档前后检查 Docker：任何运行中的容器挂载了数据目录或账号凭证目录（含其父目录/子目录）都会拒绝。**仅停止服务端不等于停止容器**。检查不会主动停止或暂停容器；备份期间也不要从 Docker CLI 或其他宿主机进程修改源文件。需连接同一个本机 Docker daemon；Go 客户端读 `DOCKER_HOST`，不自动读取 Docker CLI context。

命令自身不要求 Python、sqlite3 或 tar；定时脚本的保留和传输仍依赖 Python 3，远端传输依赖 rsync。定时环境通过 `systemctl edit agentbox-backup.service` 配置；交互 Shell 的环境不会自动进入 systemd。系统与完整备份分别轮转，旧格式文件不自动删除。远端传输失败时保留本机备份并报告失败。

### 校验与恢复

```bash
./agentbox backup-verify /backup/full.tar.gz
# 目标目录必须尚不存在，父目录须已创建
sudo ./agentbox restore --to /var/lib/agentbox-restored /backup/full.tar.gz
```

恢复先检查归档路径、类型、清单和哈希，再在私有临时目录解包；文件完成后才创建链接，并执行 SQLite integrity_check，最后以不覆盖方式发布新目录。失败不会覆盖已有实例，不会复用旧 WAL/SHM。普通文件权限和 mtime 恢复（文件时间精度到微秒），root 执行时保留 UID/GID；非 root 演练时文件归当前用户，生产迁移请使用 root。

恢复目录结构：

```text
agentbox-restored/
  config.json                 # data_dir 改为相对路径 data
  data/state.db
  data/creds/
  data/home-template/
  data/users/                 # 系统备份只有用户模板，完整备份含全部用户数据
  accounts/                   # 旧目录保留
  credentials/<编号>/         # 账号使用的独立凭证副本
  backup-manifest.json
```

配置中各账号的 `credentials_dir` 会指向恢复目录下 `credentials/<编号>`；同源账号继续共用同一份目录。其他配置字段原样保留，**不会自动改监听地址、域名、代理、镜像或用户文件中的绝对路径**。数据库内容原样保留，含会话 ID 和原容器引用。

启动前必须检查：

1. 恢复包类型和来源正确，用户文件、聊天历史及凭证齐全；系统备份不能单独恢复项目文件。
2. 新的 `data_dir` 和凭证路径正确；按新目录刷新 systemd 配置。
3. 新实例使用独立 Docker daemon，或在停旧实例后人工处理旧会话容器，确保按新宿主路径重建 bind mounts。同一 daemon 上不能同时启动共享会话 ID 的旧、新实例，容器名称会冲突。
4. 用户 home 中 OAuth 刷新链可能已失效，逐账号检查，必要时重新授权。
5. 启动后检查 `/api/ping`、登录、文件、聊天历史、用量和额度；不能只凭首页可访问判断恢复完成。

SHA-256 和清单用于检测损坏，不是来源认证。备份含源码、登录令牌和账号密钥，存储权限与异机副本应受控；定期使用 `scripts/test-backup.sh` 验证命令，并对自己的真实数据执行隔离恢复演练。

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

服务停止会中断正在处理的网页回合并等待本地收尾，断开 WebSocket、隧道和代理连接；会话容器与终端 tmux 保留。后台退出超时会报错，不能将服务停止当作完整备份所需的“容器已停”条件。
