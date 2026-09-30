# 独立部署、迁移与运行维护

阶段 D 保持单二进制、SQLite、本机 Docker 和原生 ES Modules。源码目录不再必须承载运行数据；仓库内的 `install.sh` / `deploy.sh` 继续兼容旧安装。

## 新安装

Linux 主机需要 Docker Engine、Python 3、GNU coreutils 和 systemd。先校验发布包的 SHA256SUMS，再解包；以下 `PACKAGE` 是解包后的绝对目录。安装服务器不需要 Go 或 Node。

```text
/etc/agentbox/config.json                 配置（0600，父目录 0700）
/opt/agentbox/releases/<version>/         不可覆盖的版本目录
/opt/agentbox/current                    原子切换的符号链接
/var/lib/agentbox/                        SQLite、凭证、模板、用户文件
/var/cache/agentbox/marketplace/          可重新下载的市场缓存
```

```bash
sudo install -d -m 0700 /etc/agentbox
sudo install -m 0600 "$PACKAGE/config.example.json" /etc/agentbox/config.json
```

编辑配置：替换初始口令，设置 `data_dir=/var/lib/agentbox`、`cache_dir=/var/cache/agentbox`，去掉示例代理/账号，或改为自己的账号。凭证路径用绝对路径。构建固定版本会话镜像后，把 `agent_image` 配为版本标签。配置目录需允许服务原子替换文件；新单元默认以 root 管理本机 Docker。

```bash
sudo python3 "$PACKAGE/deploy/release.py" install --package "$PACKAGE"
sudo python3 "$PACKAGE/deploy/release.py" activate --version v0.1.0
```

版本参数以实际包为准。`install` 校验配置、暂存完整发布目录、拒绝覆盖已有版本、安装并 enable 单元；`activate` 才启动服务。日志写 journald。路径参数 `--app`、`--config`、`--unit-dir` 放在子命令前；部署路径不接受空白、引号、反斜杠和 `%`。`agentbox check-config --config PATH` 只读校验配置并报告当前程序支持的 schema/兼容代号，不连接 Docker 或迁移数据库。

新安装不启用自动追新，也不自动下载镜像。定时备份可复用包内 `scripts/backup.sh`，显式配置 `AGENTBOX_BIN=/opt/agentbox/current/agentbox`、`AGENTBOX_CONFIG=/etc/agentbox/config.json`、`BACKUP_DIR`，按维护者自己的 cron/systemd timer 执行。动态客户端下载仍读取 `data_dir/abox-link/`；开发机可用 `AGENTBOX_CLIENT_OUTPUT=/指定输出目录 scripts/build-clients.sh` 构建，再放到服务器对应目录。

## 升级与回退

对新包执行 `install`，再 `activate --version NEW`。版本切换由部署锁串行；激活前检查配置和只读 schema，停服务后再次检查，使用当前版本创建并验证系统备份，然后切换 `current`，启动并检查 `/api/ping` 和 systemd active。旧版本目录与旧镜像保留。

包含在线升级功能的版本目录安装可在「关于与更新」直接升级，开发构建也可切换到最新正式版（仍需通过配置与数据库兼容检查）：`deploy/update.py` 下载并验证指定版本，通过独立 systemd 临时服务复用 `release.stage` / `release.activate`，全程持有同一 `.deploy.lock`。API 仅启动任务或读取持久化状态，不在主服务中执行停机操作。在线入口严格匹配原始服务单元、MainPID 与 current 目录；自定义布局继续手工发布。详情、失败恢复和首次启用要求见[控制台更新](../releases.md#控制台更新提示)。

回退也是 `activate --version OLD`，不直接手改 symlink。目标程序必须支持 `check-config`、同一 `compatibility_epoch`，且支持当前数据库 schema；否则拒绝。未来增加不兼容的配置/授权语义时须提升兼容代号，不能仅比较数据库列。

系统备份不包含工作区一致性快照，升级不会停会话容器。需要完整恢复点时在维护窗口停服务及相关容器，另做 `backup --full`。启动失败不会自动切回旧二进制，防止把已迁移的数据库交给不兼容程序。参见[数据库迁移](database-migrations.md)：跨 schema 回退须恢复旧备份到新目录，再显式配置对应实例。

## 从仓库内部署迁移

先排定维护窗口。迁移会停止服务及所有挂载源数据/凭证目录（包括祖先目录）的容器，删除这些已停止容器以消除旧 bind mount；容器可写层和 tmux 进程不会迁移。工作区、home、历史、共享目录与双层模板保留。需要保留的容器文件应先放入这些 bind mount。

先停用旧的 `agentbox-backup.timer`、`agentbox-image-update.timer` 及对应服务，避免它们继续操作旧部署。保留旧单元副本，迁移完成后再替换主服务单元。不要同时启动旧/新实例。

```bash
sudo python3 "$PACKAGE/deploy/release.py" migrate \
  --source-config /旧仓库/config.json \
  --binary "$PACKAGE/agentbox" \
  --backup /备份目录/migration-full.tar.gz
```

默认只打印源/目标路径和会停止的容器 ID；确认目标后，加 `--apply` 执行相同命令。也支持 `--data` 和 `--cache`。目标配置、数据和备份文件须不存在，且配置/数据/缓存目录与源数据/凭证根互不重叠。

工具顺序：系统备份并验证 → 停服务和相关容器 → 完整备份并验证 → 持有旧数据锁 → 恢复到私有暂存目录 → 按数值 UID/GID、mode、mtime 保留元数据 → 将账号凭证统一放入新数据目录并改写配置 → 校验配置 → 删除旧挂载容器并清空恢复库的 container_id → 发布新目录。旧数据、配置与备份不删除。操作中若失败，保持离线并根据错误检查留下的新目录/备份；不要直接对原命令反复覆盖重试。

恢复只带备份规范中的持久数据，不带市场缓存、旧备份集合、手工放入 data 根目录的其他文件，以及可重新构建的 abox-link 下载包。自行管理这些额外文件。原来的用户符号链接保留原文；指向旧宿主机绝对路径的自定义链接需手工核对。

迁移完成后，把旧 `/etc/systemd/system/agentbox.service` 留存为其他名称，再执行新包的 `install` 与 `activate`。安装器遇到其他布局的主服务单元会拒绝覆盖。验收应包括登录、会话清单、工作区文件、历史、凭证路径、以 UID 1000 启动空间，以及新容器三处挂载都指向新目录。缓存会在首次访问市场时重新生成。保留旧副本直到验收结束；恢复旧服务前须先停新服务及其容器，避免两份凭证独立轮换。

## 容量与回收

管理员在「系统设置 → 容器与资源」配置 `resources`：

| 字段 | 含义 |
| --- | --- |
| `max_running` | 本实例已登记会话的全局运行容器上限，0 不限 |
| `max_running_per_user` | 每个属主的运行容器上限，管理员也计入，0 不限 |
| `min_free_bytes` | 新启动前数据盘最少可用空间，0 不检查；界面以 GiB 编辑 |

新启动在同一可取消闸门内检查 Docker 实际状态并创建容器，防止并发越过上限；已运行会话的重新连接不占新名额。Docker 状态查询失败拒绝新启动。REST 容量拒绝返回 429，WS 使用原来的错误展示。降低上限不强杀已有任务。这里不限制用户创建多少条停止状态的空间，也不是磁盘硬配额；容器内持续写入仍可耗尽磁盘。绕过服务直接启动容器不受准入锁控制。

`GET /api/storage`（管理员）读取后台每 5 分钟生成的文件统计与当前文件系统水位。扫描最多 10 秒/10 万条，取消、权限错误或超过预算会标记 `partial`，不能将不完整结果当作完整用量。它按普通文件的逻辑大小估算，硬链接重复计数、稀疏文件不代表实际占用，不跟随符号链接。逐用户统计覆盖 users 下的 workspace/home/shared/history/template。

回收按明确范围执行：空闲超时仅停容器，保留数据；附件沿用 48 小时与历史引用保护；管理员可确认清理市场缓存（`DELETE /api/cache/marketplace`）；工作区仅通过原有显式 purge 删除。没有自动删除旧工作区、全局 Docker prune 或按磁盘水位清空用户目录的行为。

## 同步与诊断

用量明细接口不再同步扫描文件。请求立即返回已提交记录，向唯一后台 worker 合并刷新请求；请求触发的全量扫描间隔至少 5 秒，inotify 及每分钟兜底继续运行。

响应新增 `sync.last_scan_at`、`last_success_at`（毫秒，0 表示尚未完成）、`scanning`、`errors`。时间对应全量扫描，不代表 provider 已写完 transcript；inotify 的单会话更新可能更新。失败文件会重试，终端始终只记账、不扣额度。

管理员可从容量页下载 `GET /api/diagnostics`：白名单字段仅包含版本、构建信息、平台、schema、启动时间、会话数量、容量设置、磁盘水位与同步进度。不包含配置内容、路径、用户身份、账号、环境变量、日志、原始 Docker inspect 或对话。`GET /api/system` 继续为管理员提供本机详情，并新增运行版本信息。

## 前端模块与兼容

- `web/src/app/lifecycle.ts` 管理 chat/settings 的初始化、登录失效和页面退出清理。
- `features/chat/connection.ts` 私有持有 WS、重连计时器和代际；旧连接事件不再写到新会话。
- `features/settings/{state,operations}.ts` 管理设置状态与监控/关于页面；`shared/poller.ts` 提供串行、可取消轮询。
- 全局 S 不再持有聊天 socket、重连代际、设置缓存和列表轮询定时器。其他功能保留兼容入口，后续按需继续提取，不进行框架重写。
- 嵌套 ES Modules 仍通过 `/_v/<hash>/` 相对导入；TS 与 JS 一起提交。

旧 abox-link 的 `/api/tunnel` WebSocket、yamux、令牌鉴权及配对接口未改变。原有无头客户端可继续连接；依赖配对接口的新客户端仍须先升级服务端。D 阶段跑现有 linkapp/tunnel 回归，未逐个运行历史发布的客户端二进制，不把兼容协议推断当作全版本实测。

## 本地验证

`python3 scripts/test-deployment.py` 验证版本不可覆盖、路径边界、高版本库拒绝与主单元归属。`scripts/test-deployment-linux.py --binary <Linux程序>` 在临时 Linux 容器演练迁移、文件元数据、配置凭证、历史、升级、回退与 HTTP 登录；systemctl 调用由测试进程模拟，不能替代真实主机上的 systemd 验收。

`node scripts/test-browser.mjs` 使用合成 API 测试登录、保存容量、重复初始化、监控清理、用量同步显示、WS 代际与窄屏重新登录。需安装固定 `playwright@1.58.2` 及 Chromium；可通过 `AGENTBOX_PLAYWRIGHT_MODULE` 指向临时安装的 index.mjs，`AGENTBOX_BROWSER_CHANNEL=chrome` 使用本机 Chrome。CI 同样执行该脚本。

另有真实 Docker 会话冒烟（容量拒绝、挂载、文件/Git、后台终端用量、SIGTERM 重启）及 Linux 文件系统回归。未执行生产迁移、真实 systemd 服务切换或远端 CI；这些在对应环境的发布窗口验收。

### 空间紧张时的迁移

`migrate --reflink --apply` 要求同一文件系统支持 GNU cp 的写时复制，停机前先用合成文件探测，不支持则停止。完整备份仍照常生成并验证；配置/SQLite/凭证通过额外离线系统备份恢复，users 通过 reflink 创建独立副本，再逐条核对完整备份清单的路径、类型、权限、属主、mtime、链接目标和 SHA-256。旧 users 保留，之后新文件写入时才分配新块。需预留完整压缩备份和后续写入空间，reflink 不能替代备份。备份清单上限为 100 万条、256 MiB，创建与验证使用同一边界。

当完整压缩包也放不下时，可组合 `--reflink --snapshot-backup`：`--backup` 指向不存在的新目录。工具生成并验证离线系统备份，恢复数据库/配置/全部凭证，再 reflink 克隆 users；与源树逐条比对内容及元数据，写 `snapshot-manifest.json` 并重新验证，从恢复点再次 reflink 克隆后才发布运行目录。恢复点是自包含的完整目录（其中 config 路径为相对路径），旧源和恢复点均保留；不能把它当作 `backup-verify` 的 tar 包。恢复时应停所有相关写入、先调用脚本 `verify_snapshot` 校验，再复制到新目录启动；不要直接启动恢复点本身。XFS reflink 共享未修改的数据块，同盘恢复点仍不覆盖整盘损坏，应后续另做异机备份。

2026-09-24 首台生产 XFS 主机已完成真实 systemd 切换验收：8.1 GiB users 使用 `--reflink --snapshot-backup` 迁移，保留旧源码/数据与完整恢复点，运行目录、凭证、历史、额度、文件/Git/WS 和每日备份验证通过。该次保留已有 Claude/Codex 镜像并改用固定标签，未触发模型请求；远端 CI 仍未运行。
