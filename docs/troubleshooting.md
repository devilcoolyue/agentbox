# 常见问题与排障

[返回文档目录](README.md) · [项目首页](../README.md)

先区分故障位于浏览器、服务端、Docker、CLI 还是 provider。首页正常只能证明 HTTP 入口可访问，不能证明空间镜像、账号和 WebSocket 都正常。

当前源码的登录、空间启动和聊天错误会显示处理建议与操作编号，三种界面语言均可读取。反馈时优先保留编号、发生时间和操作类型；管理员在 `journalctl -u agentbox` 中查找同一 `operation_id`。错误目录和兼容范围见[错误码与操作关联](errors.md)。连接在到达服务端前就失败时可能没有对应日志，需继续核对网络和反向代理；不要把连接编号当成任务已接收的证明。

管理员可在「系统设置 → 容器与资源 → 环境检查」运行实例检查；普通用户在自己空间的「更多操作 → 环境检查」查看账号授权、额度和环境依赖。每项明确区分通过、失败和未检查，详情见[分层环境诊断](diagnostics.md)。无法打开网页时，可在服务端主动运行 `agentbox check-config --config /实际配置/config.json --environment`；它不调用模型。

## 服务无法启动

```bash
systemctl status agentbox --no-pager
journalctl -u agentbox -n 50 --no-pager
tail -n 50 /var/log/agentbox.log
```

| 报错 / 现象 | 检查方式 |
| --- | --- |
| `auth_token must be a secret` | 替换初始密码占位符，至少 8 字符 |
| JSON parse / invalid timezone | 检查 JSON 语法、字段类型与 IANA 时区 |
| `data dir ... already in use` | 同一数据目录已有实例；通过 systemd 重启，不要删除锁文件来绕过 |
| `address already in use` | 用 `ss -ltnp` 检查监听端口和重复实例 |
| Docker socket 无权限 / daemon 不可达 | 检查 Docker 服务及运行用户权限 |
| `Start request repeated too quickly` | 修复原始故障后 `systemctl reset-failed agentbox` 再启动 |
| 单元指向旧仓库或失效路径 | 先用 `systemctl show agentbox -p WorkingDirectory -p ExecStart` 核对布局；版本目录部署用 `deploy/release.py`，不要覆盖为源码目录单元，见[部署布局](architecture/deployment-layout.md) |

代码省略 `listen` 时默认 8080；示例配置是 8180。探活、反向代理和 SSH 转发使用同一个实际端口。

## 忘记密码或改了 auth_token 仍不能登录

`auth_token` 只在用户表为空时作为 `boxadmin` 的初始密码，之后登录密码在 SQLite 中。普通用户由管理员重置；已登录管理员可在安全设置改自己的密码。

忘记管理员密码且没有有效管理员登录时，使用当前源码新增的 `admin-reset-password` 离线命令（旧发布包可能尚未包含）。它只重置已存在的管理员，不创建用户、不提升普通用户权限，也不修改额度、用量、空间索引或项目文件。不要删除 `state.db` 来重置密码。

先核对实际服务布局和配置，使用与数据库 schema 匹配、包含该命令的二进制。以下路径是**版本目录布局示例**；源码部署应换成服务实际使用的二进制和配置路径：

```bash
systemctl show agentbox -p WorkingDirectory -p ExecStart
# 按上面的实际路径填写；不要直接复制到未知布局的实例
RECOVERY_BIN=/opt/agentbox/current/agentbox
RECOVERY_CONFIG=/etc/agentbox/config.json
RECOVERY_BACKUP=/安全备份目录/before-admin-reset.tar.gz
sudo systemctl stop agentbox
sudo "$RECOVERY_BIN" backup --config "$RECOVERY_CONFIG" --output "$RECOVERY_BACKUP"
sudo "$RECOVERY_BIN" backup-verify "$RECOVERY_BACKUP"
sudo "$RECOVERY_BIN" admin-reset-password --config "$RECOVERY_CONFIG" --user boxadmin
sudo systemctl start agentbox
```

逐条执行，任一步失败先处理再继续。命令会显示目标配置、数据库和管理员，再要求两次隐藏输入新密码（8～1024 字节）。SSH 操作需有终端，如 `ssh -t`；不接受密码参数、环境变量或 stdin 管道。按 Ctrl+C 可取消。服务未停止/数据目录锁被占用、目标不是管理员、数据库不存在或 schema 不匹配时拒绝执行；不会自动初始化或迁移数据库。**不要删除锁文件绕过检查。**

密码与该管理员的全部登录令牌在同一事务内修改；成功后用新密码登录，原浏览器、桌面和 abox-link 需重新登录或配对。其他用户的令牌保持有效。修改 `auth_token` 仍不参与这次恢复。

密码恢复只修改数据库，默认系统备份足以保存这次修改涉及的状态；系统备份不含项目文件和聊天历史，需要它们的完整快照时按[备份与恢复](../deploy/README.md#备份与恢复)停相关容器并使用 `--full`。恢复异常时保持服务停止，用 `backup-verify` 检查备份，再执行 `restore --to /新的恢复目录 备份包`，核对新目录配置与容器挂载后切换实例；不要覆盖原数据库或混入旧 WAL。恢复备份也恢复旧密码和旧令牌，须在启动恢复实例前重新执行密码重置。

## 首页正常，但对话或终端连接失败

1. 打开浏览器开发者工具，检查 WebSocket 是否成功升级为 `101`。
2. 核对反向代理的 `Host`、`Upgrade`、`Connection` 和长连接超时，见[Nginx 示例](../deploy/README.md#https-与-websocket-反向代理)。
3. 检查页面是否显示登录过期、额度不足或历史加载失败；历史未恢复时发送会被暂停。
4. 查服务日志里的空间启动和 CLI 错误，确认 Docker 镜像存在。

终端显示额度原因并停止重连时，先处理余额；这类关闭是服务端明确拒绝，并非普通网络掉线。

## 镜像找不到、空间启动失败或退出码 137

```bash
docker image inspect agentbox-agent:latest
docker ps -a
docker stats --no-stream
```

镜像缺失先构建；自定义了 `agent_image` 时检查实际标签。写文件失败时检查挂载目录及 `1000:1000` 属主，不能只看宿主机 root 是否可写。

退出码 137 表示进程被强制终止，不一定是内存不足。结合 `docker inspect <container-id>` 的 OOM 状态、资源限制、CLI stderr 和账号配置判断。订阅 / 中转切换后旧认证环境也可能造成重试卡住，不能只通过提高内存处理。

容器停止会终止进程；文件应放在挂载目录中。容器内系统目录的安装和临时修改会在镜像升级重建时丢失。

## 账号授权或模型调用失败

- Claude OAuth 链接过期 / state 不匹配：重新生成链接，粘贴此次授权的完整返回值。
- 查询订阅额度提示认证失败：服务端会尝试同步 / 刷新凭证；刷新链已失效时重新授权。
- 切回订阅仍使用中转：检查账号 env 是否清除 Bearer Key，并重新建立终端环境。
- Codex Base URL 清空后仍走旧地址：空值保留已有 `config.toml`，需要检查账号凭证目录中的 provider 配置。
- 模型不可用：系统候选列表不验证上游授权范围，换成该账号 / provider 实际支持的模型；也可以在账号的「可用模型」里从上游读取并只勾选实际可用的模型。
- 配置了可用模型后对话里没有思考强度：强度只对标为「支持调整」的模型显示。Claude 官方接口和 Codex 订阅目录会带回档位，在「可用模型」里再点一次「从上游读取」即可补上；OpenAI 风格的中转列表不报告能力，读取时会按官方模型目录（Agent 镜像里 CLI 自带的目录）补全同名模型；中转站没有模型列表时会直接列出官方目录，也可以点「官方目录」手动列出。改了名字的中转模型查不到，需要逐个或用「批量设置强度」按中转站实际支持的档位设置。
- 「官方目录」显示「内置官方快照」而不是 CLI 版本：服务端读不到 Agent 镜像（Docker 不可用、镜像缺失或隔离容器启动失败），原因在服务端日志 `models:` 开头的行里；快照仍可用，但不含快照之后发布的新模型。

若绑定了出口代理，连同[代理故障](#代理已配置但请求失败)一起检查。网络探测成功也不代表每种模型和接口协议都兼容。

## 代理已配置但请求失败

账号绑定代理后不会在故障时自动直连。检查代理是否停用、账号绑定是否正确，以及 `proxy_bridge` 是否成功监听容器可达地址。

```bash
docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'
```

默认 `172.17.0.1` 不适用于所有网络。绑定不存在的网卡地址会失败；不要把容器要访问的地址设成容器自己的 `127.0.0.1`。

先在「IP 代理」做连通性测试，再用新网页回合验证。终端需要重新连接并新建窗口，旧进程保留旧环境。

## abox-link 配对或内网访问失败

1. 管理员已开启隧道，且页面显示代理实际在线。
2. 配对码仅可使用一次、10 分钟有效；失效时重新生成。
3. 确认本机本身能访问目标，目标在 `--allow` 白名单或映射列表中。
4. 先核对模式：透明模式用原内网地址访问，并确认空间「网络就绪」；兼容模式才显式使用 `socks5h`，例如 `curl --proxy "$AGENTBOX_INTRANET_PROXY" ...`。两种模式的真正域名解析都在本机客户端。
5. 查看本机面板日志，端口映射可能因冲突、端口低于 1024 或规则错误而失败。

兼容模式下，工作空间先开、隧道后连时，旧 Shell 没有最新变量。重连网页终端，然后执行 `tmux new-window`；在旧 Shell 中仅重启 Agent 不能更新继承环境，`Ctrl-b c` 也不适用。透明模式不依赖这些变量，规则生效以空间网络就绪状态为准，已有 Shell 发起的新连接无需重开终端。

下载按钮没有客户端时，检查实际 `<data_dir>/abox-link/`；构建脚本默认输出到仓库的 `data/abox-link/`，自定义数据目录需复制过去。更新客户端前先更新服务端配对接口。

## 技能或 MCP 没有生效

技能安装到「我的模板」后，要等空间下次启动同步；要立刻使用可执行「装到本空间」。模板中的文件在空间里删除后，下次启动可能重新出现。

官方目录单位是插件，不是每项都含技能。返回 422 且提示没有技能时，按提示在终端安装完整插件。

Claude 项目级 MCP 需要相应设置才能在无头对话里加载。Codex 的账号 `config.toml` 会在启动时覆盖空间中的同名配置，MCP 应维护在账号配置中。具体位置和模板覆盖顺序见[技能与 MCP](skills-and-mcp.md)。

## 使用记录为 0，或余额与总费用对不上

先看计费方式和类型：

- Codex 不报告美元费用；缺少命中的价格时费用为 0。
- Claude 终端只提供 token，费用依赖价目表；这类记录不扣余额。
- Codex 终端依赖 codex-tui rollout 中的来源、回合 ID 和 token_count；格式未知或关键字段缺失时不推测用量。
- 不限额用户照常有使用记录，但不扣额度。
- 总费用按全部筛选结果计算；余额是另一个独立账本，两者范围可能不同。

新记录的费用明细使用入账价格快照，修改价目表不重算历史费用；旧记录仍显示当前参考价。终端长回合的记录会逐渐补齐，首次看到的可能不是最终值。日志出现「未记到用量」时，应保留脱敏的 CLI 版本和错误上下文排查事件格式。

## 前端更新后还是旧界面

检查是否执行 `npm run build` 并发布 `internal/web/static/js/`。只提交 TS 或只重启服务都不会生成新 JS；生产服务器不执行 npm。

嵌入模式需要重新构建并重启 agentbox，启动后生成新内容哈希。开发热加载需正确设置 `AGENTBOX_WEB_DIR`；abox-link 面板则必须重新构建并运行新的客户端二进制。

## 上传被拒绝、HTML 预览异常或 Git 列表为空

- 上传遇到 413：同时核对 `max_upload_mb`、反向代理和外层 CDN 上传限制。
- 压缩包拒绝解压：检查符号链接、路径穿越和解压后体积限制，不能靠改后缀绕过。
- HTML 预览链接过期：重新从文件页打开；需要后端服务的应用不能仅靠静态预览运行。
- Git 列表为空：确认工作区根或两层子目录内确有仓库；忽略文件不会出现在待提交列表。
- 大文件 / 二进制无文本预览：使用下载或终端查看。

## 备份包里没有项目文件

先检查备份类型与版本：新版系统备份包含数据库、配置、全部账号凭证及双层模板，但不包含工作区、聊天 JSONL、会话 home 和共享目录；这些需要 `backup --full`。旧版无 manifest 的包仅包含数据库、配置和 `accounts/`，不能用新恢复命令处理。详见[备份表](../deploy/README.md#备份与恢复)。

如果曾直接复制运行中的 `state.db`，即使文件可以打开，也不能保证包含 WAL 中的最新数据。应使用在线备份 API 重新取快照，并做恢复核对。

## 反馈问题时提供什么

提供 `git rev-parse --short HEAD`、系统和架构、Go / Docker / CLI 版本、最短复现步骤、预期结果与脱敏日志。聊天 / 终端问题说明是否经反向代理、是否绑定账号代理，以及是在网页对话还是 CLI 终端复现。

不要附上 token、完整代理 URL、OAuth 返回码、真实数据库或用户项目；需要复现文件时创建最小化的模拟样例。
