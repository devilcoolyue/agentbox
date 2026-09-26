# 配置参考

[返回文档目录](README.md) · [项目首页](../README.md)

配置示例见 [`config.example.json`](../config.example.json)，完整结构与校验见 [`internal/config/config.go`](../internal/config/config.go)。服务启动时用 `-config` 指定配置文件；相对的 `data_dir` 和 `credentials_dir` 均相对于该配置文件所在目录解析。

## 如何修改配置

日常调整优先通过「系统设置」：服务会校验并原子写回配置。直接编辑磁盘上的 JSON 不会自动热加载，需要重启；也应避免同时从网页保存，导致人工修改被旧内存配置覆盖。

下面的“默认值”指配置文件省略该字段时的加载值；示例文件中的值不一定等于加载默认值。尤其是 `listen`：**代码默认 8080，示例和本文部署流程使用 8180**。

## 基础字段

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8080` | HTTP 监听地址；示例显式设为 `127.0.0.1:8180`，更改后需重启 |
| `auth_token` | 必填 | 至少 8 字符，拒绝示例占位值；首次创建管理员密码，也用于派生代理口令 |
| `data_dir` | `data` | SQLite、用户目录、模板、缓存等运行数据根目录 |
| `agent_image` | `agentbox-agent:latest` | 用于创建工作空间的 Docker 镜像 |
| `permission_mode` | `bypassPermissions` | 接受 `default`、`acceptEdits`、`plan`、`bypassPermissions`；供 Claude 无头命令使用 |
| `max_upload_mb` | `512` | 项目上传大小上限，必须 ≥ 1；反向代理也需允许相应请求体 |
| `idle_timeout_min` | `30` | 空闲停机分钟数；显式设为 `0` 关闭，不能为负 |
| `timezone` | `Asia/Shanghai` | 有效的 IANA 时区，控制页面时间和使用记录筛选 |

首次启动且用户表为空时，服务创建管理员 `boxadmin`。后续登录密码由数据库中的密码哈希决定，修改 `auth_token` 不会改密码；它仍需作为服务端密钥保留。

`permission_mode` 不应被理解成跨两种 CLI 的统一交互审批界面。Codex 有自己的执行配置，终端中的 CLI 也由自己的交互与配置控制。

## 容器资源

| 字段 | 默认值 | 校验与用途 |
| --- | --- | --- |
| `container.memory_mb` | `2048` | 内存上限，按 MiB 转换，至少 128 |
| `container.cpus` | `2` | CPU 限额，必须大于 0 |
| `container.pids_limit` | `512` | 进程数上限，至少 16 |
| `container.network` | `bridge` | Docker 网络模式 / 网络名称，不能为空 |

容器以 `1000:1000` 运行，挂载 `/workspace`、`/home/agent`、`/shared`，并启用 `no-new-privileges`。

**资源与网络配置在创建容器时使用。** 当前实现可以复用已停止的同镜像容器，因此单纯停止再启动不保证刷新现有容器的资源限制。新空间使用新配置；排查旧空间时应以 `docker inspect` 为准，需要更新旧容器时安排维护窗口重建，先确认持久目录完整。

镜像标签对应的新镜像构建完成后，运行中的容器继续使用旧镜像；停止再启动时会识别镜像 ID 变化并重建。容器可写层里的临时安装会随重建丢失，常用系统依赖应加入镜像。

## 账号、模型与价格

| 字段 | 结构 | 用途 |
| --- | --- | --- |
| `accounts` | 账号数组，可为空 | `id`、`type`、`label`、`credentials_dir`、`env`、`proxy_id`、`access`、`model_reasoning` |
| `models` | 按 `claude` / `codex` 分组的数组 | 每项含 `id`、`label` 与可选 `reasoning` 能力，维护对话候选模型 |
| `default_models` | 按 Agent 分组的模型 ID | 初始为 `claude-opus-5` / `gpt-5.5`，必须在对应候选列表中 |
| `pricing_catalog` | `url` + `auto_check` | 独立 HTTPS 价格目录；默认不联网，自动检查只生成候选，详见 [维护流程](pricing-catalog.md) |
| `pricing_managed` / `pricing_history` | 跟随目录元数据 / 最近 10 次价格版本 | 由价格管理接口维护，随配置原子持久化 |
| `pricing` | 模型 ID / Agent 名到单价的映射 | provider 不报价时用于 token 折算；省略则无价格表 |

`accounts[].type` 仅接受 `claude` 或 `codex`。账号 ID 与代理 ID 为 2–32 位小写字母、数字、`-`、`_`，首位为字母或数字，且在各自列表内唯一。账号引用的 `proxy_id` 必须存在于代理池。

`access` 配置 `all` / `users` / `admin` 使用范围，省略时全体共享；`users` 模式用 `users` 数组列出用户名。`model_reasoning` 按模型 ID 覆盖推理能力，字段与示例见[账号与模型](accounts-and-models.md#推理强度与思考预算)。

`env` 是字符串键值映射，常用于 API Key 和 provider 地址，属于敏感配置。网页创建账号时会使用 `<data_dir>/creds/<id>/` 作为凭证目录。

模型示例（配置片段）：

```json
{
  "models": {
    "claude": [{ "id": "claude-opus-5", "label": "Opus 5" }],
    "codex": [{ "id": "gpt-5.5", "label": "GPT-5.5" }]
  },
  "default_models": {
    "claude": "claude-opus-5",
    "codex": "gpt-5.5"
  }
}
```

账号操作见[账号与模型](accounts-and-models.md)；费用字段和定价示例见[使用记录与额度](usage-and-quotas.md)。

## 出口代理与隧道

| 字段 | 默认值 / 格式 | 说明 |
| --- | --- | --- |
| `proxies` | 空数组 / 省略 | 每项含 `id`、`name`、`scheme`、`host`、`port`、可选认证与 `disabled` |
| `proxies[].scheme` | `socks5` / `http` / `https` | 上游代理协议，端口范围 1–65535 |
| `proxy_bridge.bind` | `172.17.0.1:1081` | 容器访问的本地 HTTP 代理桥接监听地址 |
| `proxy_bridge.host` | 从 bind 提取主机 | 注入容器代理 URL 的主机；监听通配地址时应显式给出可达地址 |
| `tunnel.enabled` | `false` | 是否接受 abox-link 内网连接 |
| `tunnel.transparent` | `true` | 默认选择透明模式；总开关仍由 `enabled` 独立控制，显式 `false` 保留兼容代理模式 |
| `tunnel.network_bind` | `proxy_bind` 主机的 `1082` 端口 | 透明网络控制监听地址，仅向容器网络开放 |
| `tunnel.network_image` | `agentbox-network:latest` | 透明网络辅助镜像，使用前需构建 |
| `tunnel.proxy_bind` | 隧道启用或透明模式为 true 且未填写时为 `172.17.0.1:1080` | 内网 SOCKS5 代理监听地址 |
| `tunnel.proxy_host` | 从 proxy_bind 提取主机 | 容器实际连接的主机；bind 为通配地址时必须显式配置 |

桥接和隧道地址应为宿主机上可绑定、且容器可达的地址。可在 Linux 上查看默认网桥网关：

```bash
docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'
```

这些是容器侧服务端口，日常部署不需要把它们开放到公网。用户电脑通过 HTTPS / WSS 连接 agentbox 即可。配置和使用步骤见[网络手册](networking.md)。

## 终端提示语

```json
{
  "terminal_tips": {
    "tips": ["可直接粘贴图片，路径可点击预览", "项目文件请保存在 /workspace"],
    "interval_sec": 4,
    "animation": "scroll"
  }
}
```

提示语可在「系统设置 → 界面与提示」维护，最多 30 条、每条 200 个字符。间隔 ≤ 0 时只显示第一条，不轮播；当前动画为 `scroll`。省略该块或启动加载时列表为空，会补入默认提示语。

## 设置何时生效

| 修改 | 生效时机 |
| --- | --- |
| 在网页修改空闲时间、价格、候选模型和时区 | 保存后供后续操作读取 |
| 系统默认模型 | 随后新建的空间；已有空间保留自己的值 |
| 账号 env、出口代理与兼容隧道环境变量 | 后续 CLI 执行；已运行终端需重连并新建窗口 |
| 透明隧道放行规则 | 约 2 秒同步，以空间网络就绪状态为准；既有 Shell 的新连接生效 |
| 透明模式开关 | 影响容器网络布局；切换前停止空间，再按[网络手册](networking.md#透明内网访问)重启 |
| home 模板 | 下次启动路径执行时同步；技能也可手动装到当前空间 |
| 镜像 | 运行中空间不打断，停止后再次启动识别新镜像 |
| 容器资源和网络 | 新建 / 重建容器时；旧容器可能继续复用 |
| HTTP 监听地址 | 服务重启后；网页返回 `restart_required` |
| 直接编辑 JSON | 不自动读取，需重启服务 |

## 持久目录

```text
config.json                         服务配置与账号 env
accounts/<id>/                      手工指定的账号凭证（示例约定）
data/                               实际根目录由 data_dir 决定
  agentbox.lock                     单实例锁
  state.db                         SQLite：用户、空间、令牌、用量和额度账本
  state.db-wal / state.db-shm       SQLite 运行期间的辅助文件
  creds/<id>/                      网页创建账号的凭证
  home-template/                   全体用户的 home 模板
  marketplace/repo/                官方插件缓存（设置 cache_dir 后移到该目录下）
  abox-link/                       网页提供下载的客户端二进制
  backups/                         内置脚本生成的备份包
  users/<user>/
    home-template/                 用户模板
    shared/                        用户跨空间共享目录
    sessions/<id>/
      workspace/                   容器 /workspace
      home/                        容器 /home/agent
      chats/<thread-id>.jsonl       对话线程
      chats/active                 当前线程标记
```

升级首次打开数据库时会导入旧版 `state.json`；旧版单文件聊天记录在访问时迁移到线程存储。迁移或恢复前，应同时保留数据库快照和对应用户文件，详见[部署手册](../deploy/README.md)。

## 独立缓存与容量限制

`cache_dir` 相对配置文件目录解析；空值/省略时沿用 `data_dir`，市场位于其 `marketplace/` 子目录。缓存目录是部署配置，修改后需重启；设置 API 不修改路径，其他设置保存会保留它。恢复备份时缓存重定位到恢复目录的 `cache/`，不写回原实例缓存。

`resources` 支持 `max_running`、`max_running_per_user` 和 `min_free_bytes`，均为非负整数，0 禁用。可从管理员设置 API/界面原子保存并立即影响新启动。限制与磁盘统计口径见[运行维护](architecture/deployment-layout.md#容量与回收)。

Git 连接与 OAuth 应用可在网页选择 `network.route=tunnel` 及 `network.ca_pem`；路由固定为连接属主的 abox-link 隧道，断开时不直连，客户端需放行目标域名/端口。公司 CA 仅附加到该连接 TLS 信任池。SSH 连接需配置可信的服务器主机公钥并核对 SHA256 指纹，私钥不下发容器。详见 [Git API](api.md#企业网络与-ssh)。
