# HTTP 与 WebSocket API

[返回文档目录](README.md) · [项目首页](../README.md)

## 认证与权限

`POST /api/login` 接受 JSON `{"username":"alice","password":"..."}`，返回 `token`、`user`、`role`。随后用 `Authorization: Bearer <token>` 访问 API；这个 token 是数据库里的登录会话令牌，不是配置中的 `auth_token`。

- 浏览器 WebSocket 和下载直链可在 **GET** 请求上使用 `?token=`；POST / PUT / PATCH / DELETE 必须使用 Authorization 头。
- 登录令牌为签发后 30 天的固定有效期，使用不会延长；退出登录和密码变更也可能使其提前失效。
- 空间接口检查属主，管理员也不能通过这些接口读取其他用户的空间。
- 普通用户只能读取自己的用量；其传入的其他用户名不会扩大可见范围。
- JSON 错误保留 `error` 字符串；v0.1.11 起登录、空间启动和聊天等入口增补 `code`、`operation_id`、`hint`、`action`、`retryable`。客户端需容忍新增字段并兼容只有 `error` 的旧响应，详见[错误契约 v1](errors.md)。不要在调用日志中输出 token、Key 或配对码。

| 方法与路径 | 权限 | 请求 / 用途 |
| --- | --- | --- |
| `GET /api/ping` | 公开 | 轻量探活，成功 `204` |
| `POST /api/login` | 公开 | `{username, password}` |
| `POST /api/logout` | 已登录 | 撤销当前令牌 |
| `GET /api/me` | 已登录 | 当前用户、角色、时区、额度及界面所需信息 |
| `GET /api/onboarding` | 已登录 | 当前用户的首次准备快照；不联网验证提供方或启动空间 |
| `POST /api/me/password` | 已登录 | `{old_password, new_password}`；保留当前令牌，撤销其他登录 |
| `POST /api/tunnel/pair/redeem` | 凭配对码 | 配对码本身是一次性凭证，无需另带登录令牌 |

## 工作空间

```text
GET    /api/sessions                工作空间列表
GET    /api/sessions/{id}           单个工作空间详情
GET    /api/sessions/{id}/models    当前空间的候选模型、推理能力与发现状态（不保证上游调用权限）；restricted/default_model 见账号可用模型，client_effort 是账号 CLI 配置的默认原生强度
GET    /api/sessions/{id}/account/usage  当前空间账号的订阅额度（校验账号使用权限）
POST   /api/sessions                新建 {name, agent, account_id}
POST   /api/sessions/{id}/start     启动容器（幂等）
POST   /api/sessions/{id}/stop      停止容器（数据保留）
PATCH  /api/sessions/{id}           重命名工作空间 {name}
DELETE /api/sessions/{id}?purge=1   删除（purge 同时清除文件、配置和对话记录）
```

## 文件与预览

文件、预览和技能内容访问均通过受限目录句柄，不沿符号链接访问；无效路径或静态链接通常返回 `400`，路径并发变化可能返回 `404` 或操作错误。编辑器 PUT 以原子替换保存，并保留原文件权限位。上传在服务端 staging 目录验证后合并，解压失败不改动原目录；合并并非跨目录事务。

```text
POST   /api/sessions/{id}/upload    上传 multipart(file)，普通文件或 zip/tar.gz/tgz/tar；?path= 指定目标子目录（默认根目录），clear=1 先清空该目标目录
GET    /api/sessions/{id}/archive   打包下载 (zip)
GET    /api/sessions/{id}/files     文件列表（含权限/大小/时间）?path=
DELETE /api/sessions/{id}/files     递归删除文件或目录 ?path=
POST   /api/sessions/{id}/files/move  移动文件或目录
                                      {source_scope,source_path,destination_scope,destination_dir}
POST   /api/sessions/{id}/files/mkdir 新建文件夹 {scope,dir,name}
POST   /api/sessions/{id}/files/rename 重命名文件或目录 {scope,path,name}
GET    /api/sessions/{id}/file      读单个文件 ?path=；dl=1 强制下载
PUT    /api/sessions/{id}/file      保存文件内容（body 即内容，上限 16MB）
GET    /api/sessions/{id}/preview   ?path=&scope= 签发短时只读预览链接
POST   /api/sessions/{id}/images    图片 / 聊天附件上传（multipart file，上限 20MB），
                                    图片存入 /shared/.images/，其他文件存入 /shared/.file/；
                                    48 小时后清理未被历史引用的附件
（通用文件 / 上传 / 下载接口支持 ?scope=shared；图片上传固定落在用户共享目录）
```

## 技能与市场

```text
GET    /api/marketplace            官方插件目录（?refresh=1 强制重拉；含分类列表）
POST   /api/sessions/{id}/skills/market  从市场装技能 {name} ?scope=（插件不含技能时 422）
GET    /api/sessions/{id}/skills    技能列表 ?scope=session|template（source 标明来自工作空间/模板）
POST   /api/sessions/{id}/skills    安装技能 multipart(file=.md|.zip|.tar.gz, name?) ?scope=
GET    /api/sessions/{id}/skills/{name}         技能详情（SKILL.md 正文 + 整个目录的文件清单）?scope=
GET    /api/sessions/{id}/skills/{name}/file    读技能目录里的文件 ?path=&scope=
                                                （默认 JSON，文本超 256KB 截断、二进制只报大小；
                                                 ?raw=1 直出原始字节，加 &dl=1 下载）
DELETE /api/sessions/{id}/skills/{name}         删除技能 ?scope=
POST   /api/sessions/{id}/skills/{name}/copy    在范围间复制 {to:"session"|"template"} ?scope=
```

## Git 变更

网页本地提交采用用户级 Git 身份（缺省为用户名与 `用户名@localhost`），不会推送到远程。身份 API 对所有登录用户开放，只能读写本人。commit/discard 在同一仓库内串行；终端和 Agent 并发操作仍由 Git 自身锁协调。

status 额外返回 `head/detached/unborn/upstream/ahead/behind/tracking_known/remotes`。ahead/behind 仅在 tracking_known=true 时有效，是本地远程跟踪引用的缓存值；请求不会 fetch。remotes 为 `{name,url,push}` 数组，地址去除 userinfo/query/fragment，未识别的地址隐藏；远程操作需显式调用下文的连接与 fetch/pull/push 接口。

Git 命令在会话容器内执行，会按需启动空间；额度拦截返回 `403`。Git 执行模块未配置返回 `503`，其他启动/运行错误按接口返回错误，不回退宿主机 Git，也不以空 diff 隐藏失败。本地单条命令限时 15 秒（随后最多 2 秒强制终止），stdout 上限 4 MiB；超过限制返回错误。网页提交禁用 hook 与签名。`git/file` 是普通文件读取，不执行 Git。

```text
GET    /api/sessions/{id}/git/status  变更列表（repos=工作区里发现的仓库、repo=当前那个、
                                      分支 + 文件状态，未跟踪目录逐个文件列出、超 2000 条
                                      truncated=true；一个仓库都没有时 is_repo=false）?repo=
GET    /api/sessions/{id}/git/diff    unified diff（?path= 查看单文件，相对仓库根）?repo=
GET    /api/sessions/{id}/git/file    ?path= 文件当前完整内容（新文件没有 diff，看的就是它；
                                      纯文本，超 512KB 或二进制拒绝）?repo=
GET    /api/me/git                    当前用户网页 Git 身份 {user, name, email, updated_at}
PUT    /api/me/git                    保存自己的身份 {name, email}，不接受 user 字段
POST   /api/sessions/{id}/git/commit  git add -A 后本地提交 {message, repo?}；返回 {output, sha, pushed:false, warning}
POST   /api/sessions/{id}/git/discard 丢弃改动 {path?, repo?}（省略 path=全部，恢复到 HEAD）
```

## 对话与终端

```text
GET    /api/sessions/{id}/history   当前对话线程的历史（含线程元数据）
GET    /api/sessions/{id}/chat/threads              对话线程列表（标题/时间/轮数/是否可续聊）
POST   /api/sessions/{id}/chat/threads              开启新对话线程（旧线程保留可切回）
POST   /api/sessions/{id}/chat/threads/{tid}/activate  切换到指定线程并恢复其上下文
PATCH  /api/sessions/{id}/chat/threads/{tid}        重命名线程 {title}
DELETE /api/sessions/{id}/chat/threads/{tid}        删除线程（删当前线程自动切到最近一条）
WS     /api/sessions/{id}/chat      对话通道（JSON 事件）
WS     /api/sessions/{id}/term      终端通道（二进制 PTY；?mode=shell|agent）
```

## 使用记录

```text
GET    /api/usage                   消耗汇总（按用户/模型；普通用户只看得到自己的）
                                    ?user=&session=&kind=&since=&until=（此汇总接口时间仅接受 RFC3339）
GET    /api/usage/events            消耗明细流水（使用记录页）；同上筛选，另加
                                    &agent=&model=&limit=（默认 50，上限 500）&offset=&order=asc|desc
                                    明细时间支持 RFC3339 或系统时区 YYYY-MM-DDTHH:mm
                                    返回 rows + 整个筛选范围的 total + 筛选可选值 facets + 异步补记状态 sync
```

## 账号与出口代理

除账号列表外，本节接口均仅限管理员。账号订阅额度由属主通过 `GET /api/sessions/{id}/account/usage` 查询。

```text
GET    /api/accounts                已登录用户可读账号概要，普通用户返回值隐藏敏感配置
POST   /api/accounts                新增账号 {id, type, label, env?, proxy_id?, access?, model_reasoning?, models?, default_model?}
DELETE /api/accounts/{id}           删除账号（仍被空间使用则拒绝，凭证目录保留）
POST   /api/accounts/{id}/oauth/start  Claude / Codex：生成对应 OAuth 授权链接
POST   /api/accounts/{id}/oauth/finish 提交 {code}；Claude 为授权码，Codex 为完整 localhost 回调 URL
POST   /api/accounts/{id}/apikey    保存 {api_key, base_url?, wire_api?}
DELETE /api/accounts/{id}/apikey    清除 Claude 中转配置
POST   /api/accounts/{id}/apikey/test  探测账号 API Key 配置
POST   /api/accounts/{id}/models/discover  读取账号上游的模型列表（只读，不保存）；?source=official 改为列出官方模型目录（Agent 镜像里 CLI 自带的目录 + 内置快照，不访问上游）。API Key / 中转账号读不到列表时同样返回官方目录，并带 upstream_error；响应的 official 说明来源（kind=cli|builtin、cli_version、verified_at），模型的 reasoning_source=official 表示强度来自官方目录
PATCH  /api/accounts/{id}           改账号 {label?, env?, proxy_id?, access?, model_reasoning?, models?, default_model?}（proxy_id 空串=解绑；models 整表替换，[] 恢复系统模型列表；模型条目的 hidden=true 表示不在对话下拉里显示，默认模型不能隐藏）
GET    /api/proxies                 IP 代理池 + 桥接状态
POST   /api/proxies                 新增代理 {name,scheme,host,port,username?,password?,disabled?}
PATCH  /api/proxies/{id}            改代理（password 留空=不改）
DELETE /api/proxies/{id}?force=1    删代理（仍被账号绑定时需 force=1，会连带解绑）
POST   /api/proxies/test            连通性探测 {id?} 或直接给字段；返回延迟与出口 IP
POST   /api/proxies/import          批量导入 {text}，每行一条
GET    /api/proxies/export          导出为可再导入的文本（含密码明文）
```

## 内网隧道

```text
WS     /api/tunnel                  内网反向隧道（abox-link 客户端拨入；yamux over WSS）
GET    /api/tunnel/status           本用户隧道状态（在线/映射/透明规则/空间网络就绪状态）
POST   /api/tunnel/probe            检查本用户客户端到目标的 TCP 连通性 {target:"host:port"}
POST   /api/tunnel/pair             生成配对码（一次性，10 分钟有效）
POST   /api/tunnel/pair/redeem      用配对码换会话令牌（无需登录：码本身即凭证）
GET    /api/tunnel/clients          可下载的 abox-link 预编译客户端列表
GET    /api/tunnel/clients/{name}   下载客户端二进制（实际 <data_dir>/abox-link/ 下的文件）
```

## 用户、额度与系统管理

本节均要求管理员角色。

| 方法与路径 | 请求 / 返回用途 |
| --- | --- |
| `GET /api/users` | 用户列表 |
| `POST /api/users` | `{username, password}`，创建普通用户 |
| `DELETE /api/users/{name}` | 删除普通用户及其空间 / 容器，保留磁盘文件 |
| `POST /api/users/{name}/password` | `{password}`，重置密码 |
| `GET /api/users/{name}/quota` | 额度状态和最近 100 条账本流水 |
| `PUT /api/users/{name}/quota` | `{metered, enforced}`，修改额度模式 |
| `POST /api/users/{name}/credits` | `{micro_usd, ref?, note?}`，正数充值、负数冲正 |
| `GET /api/settings` | 当前配置视图；不返回管理员初始密码 |
| `PUT /api/settings` | 配置 patch；`pricing` 是整表替换 |
| `GET /api/pricing` | 管理员：生效价格/修订、候选目录、差异、缺价与兜底提示（含各账号可用模型）、版本历史；`in_use` 为在用模型实际命中的价格键 |
| `PUT /api/pricing` | 管理员：`revision` 必填；`prices` 整表编辑、`custom_models` 设为自定义、`catalog: {url, auto_check}` 修改来源 |
| `POST /api/pricing/check` | 管理员：检查候选更新，间隔至少 1 分钟；失败保留旧价并在响应 `candidate.error` 描述 |
| `POST /api/pricing/apply` | 管理员：`revision`、`catalog_revision`、`models`；覆盖自定义需逐项列入 `adopt_custom`；修订冲突返回 409 |
| `POST /api/pricing/restore` | 管理员：`revision`、历史版本 `id`；恢复价格与跟随状态 |
| `GET /api/system` | 服务、Docker、数据目录与数量概览 |
| `GET /api/monitor` | 运维监控数据 |
| `GET /api/storage` | 数据 / 缓存磁盘容量与分类统计 |
| `DELETE /api/cache/marketplace` | 清理可重建的技能市场缓存 |
| `GET /api/diagnostics` | 导出按字段白名单生成的诊断信息 |
| `GET /api/updates` | 当前构建信息与上次版本检查缓存 |
| `GET /api/updates/components` | 管理员：只读本地版本观察 v1；服务端版本/schema、配置镜像元数据与桌面协议/同步开关，不检查远端更新、不启动升级 |
| `POST /api/updates/check` | 检查正式发布；`?force=1` 手动触发，仍受 1 分钟间隔限制 |
| `GET /api/updates/upgrade` | 管理员：查询在线升级支持情况、当前运行版本及持久化任务；支持结果为 `supported/reason`，任务为 `job` 或 null |
| `POST /api/updates/upgrade` | 管理员：提交 `{"version":"vX.Y.Z"}`，须匹配成功检查到的新版本；返回 202 与任务。同一进行中目标复用任务，不接受 URL/路径；不支持的部署或过期版本返回 409，提交结果不确定返回 503，应先查询任务再重试 |

`/api/updates/components` 使用 `Cache-Control: no-store`，返回 `version:1`、`observed_at`、`server`、`image`、`desktop`。`server.candidate_compatibility=preflight_required` 表示本接口不证明候选兼容；`image.state=labels_only/unavailable/changed`，版本是当前配置镜像的标签，不能冒充运行中容器或协议验收结果；`desktop.installed_version_state=browser_unknown` 表示网页无法读取本机安装版本。观察范围和更新生效方式见[三类更新](update-components.md)。

## 界面主题

清单格式、令牌白名单与取值规则见[界面主题](themes.md)。写接口的请求体就是主题文件本身（JSON，最大 64 KiB），路径里的 `{id}` 必须与文件里的 `id` 一致。

| 方法与路径 | 权限 | 用途 |
| --- | --- | --- |
| `POST /api/themes/validate` | 登录用户 | 只校验、不保存：成功返回 `{manifest}`（规范化后的清单），失败与写接口相同；主题编辑器每次改动后调用 |
| `GET /api/themes` | 登录用户 | 返回 `site`（全站主题）、`user`（本人主题）、`tokens`（可改写令牌及种类）、`bases`、`limits` 与 `can_manage_site`；每条主题为 `{scope, manifest, updated_at}` |
| `PUT /api/themes/user/{id}` | 登录用户 | 新增或整份替换本人的主题，最多 20 个 |
| `DELETE /api/themes/user/{id}` | 登录用户 | 删除本人的主题 |
| `PUT /api/themes/site/{id}` | 管理员 | 新增或整份替换全站主题，最多 50 个 |
| `DELETE /api/themes/site/{id}` | 管理员 | 删除全站主题 |

校验失败返回 400，`error` 是简体中文说明，`theme_error` 给出稳定的 `code`（`bad_json`、`bad_type`、`unknown_field`、`format`、`id`、`name`、`base`、`author`、`description`、`unknown_token`、`bad_value`），以及相关的 `token`、`section`（`common` / `dark` / `light`）。超过 64 KiB 返回 413（`too_large`），数量达到上限返回 409（`limit`）。

## 草稿能力

v0.1.11 起 `/api/me` 增补 `draft_protocol:1` 与 `draft_scope`，仅用于未发送草稿隔离；它不声明 WS 接收确认能力。历史接口可显式携带 `?draft_context=1` 获取稳定的 `active_thread`（空对话会登记不含正文的元数据），附件校验接口为 `POST /api/sessions/{id}/attachments/validate`。权限、上限与恢复边界见[聊天草稿](chat-recovery.md)。

## 调用示例

以下示例使用 curl，不会读取浏览器登录状态。先把地址和用户名 / 密码占位符替换为测试环境的实际值：

```bash
ABOX_URL='https://box.example.com'

curl --fail-with-body -sS "$ABOX_URL/api/login" \
  -H 'Content-Type: application/json' \
  --data '{"username":"alice","password":"REPLACE_WITH_YOUR_PASSWORD"}'
```

从响应取得 token，再调用：

```bash
ABOX_TOKEN='REPLACE_WITH_LOGIN_TOKEN'

curl --fail-with-body -sS "$ABOX_URL/api/sessions" \
  -H "Authorization: Bearer $ABOX_TOKEN"

curl --fail-with-body -sS "$ABOX_URL/api/sessions" \
  -H "Authorization: Bearer $ABOX_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"name":"demo","agent":"claude","account_id":"claude-sub"}'
```

`account_id` 必须存在且类型匹配；创建响应中的 `id` 用于后续路径。上传一个文件到共享目录：

```bash
ABOX_SESSION='REPLACE_WITH_SESSION_ID'

curl --fail-with-body -sS "$ABOX_URL/api/sessions/$ABOX_SESSION/upload?scope=shared" \
  -H "Authorization: Bearer $ABOX_TOKEN" \
  -F 'file=@./example.txt'
```

查询系统时区某一天的全部用量合计与第一页明细：

```bash
curl --fail-with-body -sS --get "$ABOX_URL/api/usage/events" \
  -H "Authorization: Bearer $ABOX_TOKEN" \
  --data-urlencode 'since=2026-09-01T00:00' \
  --data-urlencode 'until=2026-09-02T00:00' \
  --data-urlencode 'limit=20' \
  --data-urlencode 'offset=0'
```

`since` 为包含边界，`until` 为不包含边界。响应的 `rows` 受分页限制，`total` / `facets` 基于整个筛选范围，另返回 `timezone`、`scope`、`order`。普通用户的 total 和 facets 同样只在自己的数据范围内计算。

`/api/usage` 是较早的汇总接口，当前最多读取 2000 条用量行进行汇总，且时间仅解析 RFC3339。需要完整筛选合计时使用 `/api/usage/events` 的 `total`；CSV 是前端根据明细生成，服务端没有单独的 CSV 路由。

管理员开启计量并充值 10 美元（下面使用的 token 必须属于管理员）：

```bash
curl --fail-with-body -sS -X PUT "$ABOX_URL/api/users/alice/quota" \
  -H "Authorization: Bearer $ABOX_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"metered":true,"enforced":true}'

curl --fail-with-body -sS "$ABOX_URL/api/users/alice/credits" \
  -H "Authorization: Bearer $ABOX_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"micro_usd":10000000,"ref":"demo-grant-001","note":"测试充值"}'
```

重试同一笔充值保留原 `ref`；新充值使用新 `ref`。金额单位为微美元，具体结算边界见[额度说明](usage-and-quotas.md)。

## WebSocket 报文

对话连接为 `wss://box.example.com/api/sessions/<id>/chat?token=<token>`。先通过历史接口加载当前线程，再连接实时通道；断线后重取历史以补齐完整事件。

发送消息：

```json
{"type":"user_message","text":"解释这个项目的结构"}
```

可选字段为 `model` 与 `effort`；省略模型时使用空间保存的默认值，推理选项须与模型能力匹配。中断当前回合：

```json
{"type":"interrupt"}
```

服务端发送 `status`、`user_message`、`agent_event`、`agent_raw`、`thread_title`、`turn_cost`、`error` 等消息。`turn_cost` 按 `turn_id` 返回已结算金额及来源，历史接口的 `costs` 提供对应回合费用，`entries[].turn` 保存模型和推理设置快照。`agent_event.event` 内含 provider 事件；实时增量只广播，完整事件保存到线程 JSONL。调用方应保留对未知事件的兼容，不把某个 CLI 版本的字段当成永远不变的协议。

终端连接为 `/api/sessions/<id>/term?mode=shell&token=<token>`，也可选 `mode=agent`。**二进制帧**承载原始 PTY 输入 / 输出，文本帧仅用于调整尺寸：

```json
{"type":"resize","cols":120,"rows":36}
```

额度拦截使用关闭码 `4003`，账号撤权使用 `4004`；客户端应显示 reason 并停止无意义的自动重连。配对隧道则使用 yamux over WebSocket，由 abox-link 处理，不属于聊天 JSON 协议。

## 接口依据

路由清单以 [`internal/server/server.go`](../internal/server/server.go) 为准，前端类型见 [`web/src/types.d.ts`](../web/src/types.d.ts)。`POST /api/sessions/{id}/chat/reset` 保留为旧客户端兼容入口，语义等同新建线程；新接入优先使用线程接口。

预览入口 `/preview/<grant>/...` 使用短时通行证读取文件，不需要常规 Bearer 头；应先调用已鉴权的 `GET /api/sessions/{id}/preview` 取得 URL，不能将其当作长期公开文件托管地址。

用量明细的 `rate.snapshot=true` 表示单价来自该行入账时保存的快照；缺省或 false 表示旧记录的当前参考价。`billing` 对新记录使用持久化来源 provider/table/none，不再按 Agent 名猜测；接口路径与原字段保持兼容。

### Git 远程连接（HTTPS Token）

所有 `/git/connections` 路由需登录，返回本人连接和明确授予的共享连接；管理写入仍按所有者隔离，管理员不会因此取得其他用户的私有连接。Token 仅写入，响应只含元数据。更新/删除携带 `revision`，陈旧修订返回 409。

```text
GET    /api/git/connections
POST   /api/git/connections               {label, provider:github|gitlab|generic, base_url, username?, token, read_only?}
POST   /api/git/connections/{connection}/test {url}；验证指定仓库读取能力，不推断写权限
PATCH  /api/git/connections/{connection}  {revision, label?, token?, read_only?, enabled?}
DELETE /api/git/connections/{connection}  {revision}
GET    /api/me/git/default
PUT    /api/me/git/default                {connection_id}；空串清除默认
GET    /api/sessions/{id}/git/default
PUT    /api/sessions/{id}/git/default      {connection_id}
GET    /api/sessions/{id}/git/bindings
PUT    /api/sessions/{id}/git/bindings     {repo, remote, connection_id, revision}；revision=0 新建，空 connection_id 解除
POST   /api/sessions/{id}/git/clone        {connection_id, url, directory}；新目录克隆，不覆盖已有目录
POST   /api/sessions/{id}/git/fetch        {repo, remote}
POST   /api/sessions/{id}/git/pull         {repo, remote}；仅上游分支、工作区干净、fast-forward
POST   /api/sessions/{id}/git/push-preview {repo, remote}
POST   /api/sessions/{id}/git/push         {repo, remote, ref, expected_head, expected_remote_head}
```

创建空间新增可选 `git_connection_id`：省略采用用户默认，显式空串不绑定，非空必须是当前用户可用连接。默认连接作为绑定表单的建议值，不会授权用户未选中的仓库。绑定时校验仓库真实 remote 与连接地址，操作时再次核对；地址变化返回 409 要求重新绑定。

`base_url` 限 HTTPS，支持服务子路径，不接受 userinfo/query/fragment。连接创建后 provider/base_url/username 不可改；换服务需新建连接。GitHub 缺省 username 为 x-access-token，GitLab 为 oauth2，通用服务需填写。缺省 read_only=true；这只是 Agentbox 的限制，上游仍需赋予对应权限。

push-preview 返回目标 URL、连接标签、完整 ref、本地和远程 SHA 以及最多 20 条提交摘要；push 必须带回三个校验值。当前仅推送当前分支到远程同名分支，禁止强推、删除、多 ref 和隐式标签推送。远端提前推进或本地 HEAD 变化返回 409。网络中断后结果可能未知，应重新核对远端，而非重复本地提交。

网络传输通过按次的网桥 HTTP grant 转发，只允许所选仓库的 upload-pack 或 receive-pack。长效 Token 留在服务端，receive-pack 校验 ref/old/new；拒绝重定向，不把调用方 Cookie 或任意 Header 发往上游。Git 仍在容器运行，单条网络命令最长 120 秒。网桥使用 `proxy_bridge.bind` 的监听主机及动态端口，容器通过 `proxy_bridge.host` 访问，需允许容器到该网桥的连接。上游缺省由服务端直连 HTTPS；可按下文配置公司 CA、用户隧道或 SSH。

连接、绑定、默认值及 fetch/pull/push 写入 `git_operations` 审计（不记录 Token 和原始网络错误）。个人 Git 连接中的「操作记录」可查询并取消活动操作。正在传输的请求无法撤回；停用/修改连接会拒绝后续 grant 请求。

### Git 操作状态与取消

`GET /api/git/operations?before=<id>` 返回当前用户的 `{rows, active, next_before}`，每页 50 条，使用 ID 游标。管理员也不因此看到他人私有 Git 操作。历史包含启动/结束时间、空间/仓库、操作类型、目标和结果；不记录 Token、提交信息或原始远程错误。

前端可在 Git 操作 POST 中传 `X-Git-Request-ID`（16–80 位字母、数字、`_`、`-`）；活动列表中的 request_id 可用于 `POST /api/git/operations/{operation}/cancel`。同时最多 4 个活动操作/用户；活动列表提供阶段、耗时和实际传输字节，不伪造完成百分比。取消请求返回 202，最终结果仍来自原操作响应/历史。远程已接受的推送不能回滚，`cancelled_unknown` / `interrupted_unknown` 必须重新核对远程。

服务重启将残留 running 行标记 interrupted_unknown。SQLite schema 6 增加 finished_at，历史没有结束时间的不伪造。status 的 `last_fetch` 为最近成功网页 fetch/pull 的 `{target,at}`；不代表终端最近的 fetch 时间。

Git Docker exec 使用镜像已有的 `/usr/bin/python3 -I` 监督器；stdin EOF 请求结束独立进程组，TERM 后最多 2 秒再 KILL。Docker 连接不可用时仍受原 timeout 限制，不能把失联当作远端回滚。自定义 Agent 镜像需包含 python3。

### GitHub / GitLab OAuth

```text
GET  /api/git/oauth/apps                     登录用户可见已启用应用；管理员额外读取配置元数据
PUT  /api/git/oauth/apps                     admin；{id?,revision,label,provider,base_url,client_id,client_secret?,redirect_url,enabled}
POST /api/git/oauth/start                    {app_id,label?,read_only?,connection_id?}；read_only 缺省 true
GET  /api/git/oauth/callback                 平台回调；校验 state、Cookie、PKCE、应用修订与原登录会话
POST /api/git/connections/{connection}/revoke {revision}；OAuth 连接专用
```

管理员在 Git 连接 → OAuth 应用配置平台应用。`git_oauth_apps` 经 Config.mutate 原子保存，Client Secret 使用 Git 主密钥加密，GET 不返回密文或明文；修改应用带 revision。服务地址、Client ID、回调地址创建后固定，更换需注册新应用。回调必须为当前 Agentbox 域名的 `/api/git/oauth/callback`，要求 HTTPS，仅 loopback 开发允许 HTTP。

授权 state、PKCE verifier、浏览器 nonce 和原登录令牌关联在服务端短时内存中，10 分钟到期、每用户最多 4 个、重启后需重试。回调同时要求 HttpOnly / SameSite=Lax Cookie，原登录失效或应用停用/修改后不保存授权（凭证写入事务再次验证原登录令牌，防止删除/重建同名用户的并发串号）；授权码单次消费，回调页面不反射上游错误/令牌。

GitHub 请求 `repo read:user`（repo 本身包含写权限，Agentbox 的只读策略在服务端执行）；GitLab 只读请求 `read_user read_repository`，可写再加 `write_repository`。自建 GitLab 可使用规范 HTTPS 服务前缀。网络缺省服务端直连并验证系统 CA；应用的 network 可选择用户隧道与公司 CA。

OAuth Access/Refresh Token 均在连接密文内。使用前按连接串行续期，保留未重新返回的刷新令牌/范围；刷新失败不回落其他账号。`connection_id` 用于保留原连接 ID 的重新授权，不影响已有绑定；需使用原 OAuth 应用并核对连接修订。

撤销先停用本地连接，再请求上游；响应区分 `local_disabled` 与 `remote_revoked`，网络失败时明确提示上游撤销未确认。删除连接仍只删除本地记录；上游可能按同一用户/应用授权整体撤销，影响同一应用的其他连接。

### 企业网络与 SSH

连接和 OAuth 应用新增 `network: {route?: "direct"|"tunnel", ca_pem?: string}`；缺省保持服务端直连。`tunnel` 使用连接属主自己的 abox-link 隧道，目标 DNS 在客户端解析，仍受该客户端白名单控制；总开关关闭、用户离线、白名单拒绝均报错，绝不回落直连。兼容和透明客户端均可用于显式 Git 隧道。

`ca_pem` 限 32 KiB，只接受 PEM 根 CA/中间 CA，不接受私钥或非 CA 证书。追加到此连接专用系统信任池，保留域名验证和 TLS >= 1.2，不更改系统全局 CA。路由和 CA 是连接/应用不可变身份的一部分，并加入密文认证数据；更换需新建连接/应用。旧无 network 行仍按原认证数据解密。SQLite schema 7 保存 network；备份兼容旧库并验证新的认证数据。

OAuth 应用配置的路由/CA 贯穿授权码交换、续期、撤销与随后 Git 传输；请求使用授权用户自己的隧道，浏览器也必须能打开平台授权页。GitHub Enterprise API 与 GitLab 子路径通过同一策略访问。

创建 SSH 连接：

```json
{
  "label": "公司 GitLab SSH",
  "provider": "gitlab",
  "auth_type": "ssh",
  "base_url": "ssh://git.example.com:2222",
  "username": "git",
  "private_key": "PEM / OpenSSH 私钥",
  "passphrase": "可选解密口令",
  "host_key": "ssh-ed25519 AAAA... 管理员提供的服务器主机公钥",
  "read_only": true,
  "network": {"route": "tunnel"}
}
```

SSH 用户名缺省 git。仓库接受 `ssh://git@host:port/group/repo.git` 或默认端口的 `git@host:group/repo.git`；绑定时规范化并严格匹配连接主机、端口、服务路径及用户名。当前仓库路径各段支持 ASCII 字母/数字/点/下划线/连字符，拒绝路径跳转和 shell 元字符；不支持 SSH 主机证书或交互式密码认证。

`host_key` 是从管理员可信渠道确认的服务器公钥（单行算法 + base64），功能上相当于对该连接唯一目标固定 known_hosts 条目；不自动接受首次见到的主机。私钥、口令和主机公钥一起加密，列表只显示连接公钥与服务器 SHA256 指纹，不提供私钥导出。PATCH 可通过 `private_key/passphrase/host_key` 更换密钥或主机 pin，仍需 revision；旧 grant 会因修订变化拒绝后续连接。

服务端用 Go SSH 客户端建立认证连接，只运行远端 git-upload-pack/git-receive-pack，不在宿主机运行 Git。容器使用按次 native Git TCP grant，网桥转发至固定 SSH 仓库。只读不开放 receive-pack；推送继续校验 old/new/ref 且不允许多 ref/删除。SSH 连接可复用现有克隆、测试读取、fetch/pull/push 和审计入口。当前不把 SSH 私钥播种到终端 home。

### Git 分支管理

```text
GET  /api/sessions/{id}/git/branches?repo=...
POST /api/sessions/{id}/git/branches
```

GET 返回 `{branches,state,dirty,truncated}`，本地和远程跟踪分支最多显示 500 条，跳过 symbolic remote HEAD。每行包含 name/head/upstream/current/remote，state 包含当前分支和 HEAD。列表不联网。

POST 请求为 `{repo,action,name,expected_head,expected_branch,target_head?,start?}`。action 支持 create/switch/delete/upstream。create 在当前 HEAD 或 `start=refs/heads/...|refs/remotes/...` 创建并切换；指定起点需要 target_head。switch/delete 的 name 为本地分支名，upstream 的 name 为远程跟踪分支短名（如 origin/main）；这些动作均需提供列表中的 target_head。若当前或目标提交变化返回 409。

切换/创建要求干净工作区；删除拒绝当前分支、拒绝当前 HEAD 未包含的提交，并使用 `git branch -d` 再检查 Git 自身上游/worktree 约束。无 force、reset、自动 stash 或自动变基。设置上游要求本地正常分支及已获取的远程跟踪引用。操作写入个人 Git 审计。

### GitHub PR / GitLab MR

```text
GET  /api/sessions/{id}/git/reviews?repo=...&remote=origin&api_connection_id=...&page=1
POST /api/sessions/{id}/git/review-preview
POST /api/sessions/{id}/git/reviews
```

GET 查询打开的 PR/MR（每页 50 条，上限 1000 页），返回 provider/project/connection_id/read_only/rows/has_more/page/default_branch/source_branch/head。URL 由服务器推导到当前平台 API，GitHub 公网使用 api.github.com，Enterprise 使用 /api/v3，GitLab 使用 /api/v4/projects/{编码仓库路径}，支持服务子路径。禁止跟随 API 重定向、拒绝外域或异常平台链接；上游错误正文不直接显示或记录。

预览请求：`{repo,remote,api_connection_id?,title,body,source,target,draft,expected_head}`。校验当前本地分支、远程来源 SHA 与已推送提交一致，并读取目标分支 SHA/保护状态和已有同源/目标 PR/MR。响应包含 source/target（name/sha/protected）、标题、描述、草稿、已有请求。创建时携带相同字段并加 `expected_target`，再次核对；存在相同打开请求时返回 existing=true 及已有链接，不重复 POST。标题最多 240 字节，描述最多 32 KiB。

此入口只创建同一仓库内的 PR/MR，不自动推送、合并、审批或删除来源分支；跨 fork 的请求仍到平台处理。创建为显式确认动作；超时/平台响应不完整返回结果未知，不自动重试，需刷新平台列表。平台 API 按分支创建，预检之后仍可能发生外部并发提交，因此不宣称对上游分支构成锁定事务。

HTTPS 使用已绑定的 PAT/OAuth 连接。SSH 仓库需显式选择同平台、同主机的 HTTPS API 连接；SSH 密钥不具备 REST API 权限。创建要求有效写权限，GitHub Fine-grained PAT 需 Pull requests 写权限，GitLab PAT 需 api；只读连接可查询，不能创建。

GitLab OAuth start 新增 `api_access`（缺省 false），用户勾选后只读申请 read_api，可写申请 api；原 read_repository/write_repository 仅覆盖 Git，不冒充平台管理权限。权限不足返回明确错误，需重新授权或更换合适的 Token。

### 远程配置

`POST /api/sessions/{id}/git/remotes` 接受 `{repo,name,action:add|update|remove,url?,expected_url?,connection_id?}`。新增/修改地址必须匹配所选连接平台；修改/删除需提交当前页面看到的脱敏地址作为并发检查。已绑定的 remote 先解除绑定；修改地址不自动绑定凭证。多个 URL 或独立 pushurl 需明确处理，不静默保留旧推送目标。删除只移除本地 remote 配置和跟踪引用，不删除服务器仓库。

### 共享 Git 服务账号

```text
GET /api/git/connections/{connection}/shares
PUT /api/git/connections/{connection}/shares  {revision,users:[{user,write:false}]}
```

仅管理员可配置自己拥有的 PAT/SSH 连接共享（专用服务账号或部署密钥）；个人 OAuth 不共享。管理员权限本身不能读取/使用其他用户的私有连接。名单整表更新，每人明确只读/可写，最多 500 人；连接自身 read_only=true 时所有消费者均只读。权限判断在服务端执行，覆盖创建空间默认、绑定、测试读取、clone/fetch/pull/push、PR/MR；HTTP/TCP grant 每次开始上游请求重查修订及当前授权。

连接列表包含私有和被授权连接，`managed` 表示是否允许维护。普通消费者不能编辑、删除、改密钥、撤销 OAuth 或修改共享名单，列表不下发密钥或其他被授权用户名。共享连接的网络路由使用实际操作者自己的隧道；审计记录实际操作者。

撤权增加连接修订并清理被撤销用户的默认建议；仓库绑定保留为不可用，需手工重新绑定，不能静默换账号。正在上游执行的请求不能收回，必要时在上游撤销 Token/密钥。删除用户同时清理授予名单，重建同名用户不会继承旧授权。SQLite schema 8 增加 git_connection_shares。

### 终端 Git 短期授权

- `GET /api/sessions/{id}/git/terminal`：列出本人空间中尚未过期/撤销的授权，返回 `id,repo,remote,write,expires_at,command`，不返回令牌。
- `POST /api/sessions/{id}/git/terminal`：`{repo,remote,write:false}`；需已绑定且可访问的 remote，`write:true` 额外要求有效写权限。授权固定 30 分钟，响应 201，字段同上。安装命令到空间 home，不自动执行 Git。
- `DELETE /api/sessions/{id}/git/terminal/{grant}`：撤销本人该空间授权并取消其活动请求。

短期控制网桥复用 `proxy_bridge.bind` 主机与 `proxy_bridge.host`，使用临时端口，仅接受独立能力令牌；不能用作通用用户 API。原生 Git 不继承此授权，请使用响应 `command` 加 `status/fetch/pull/push-preview/push`。状态、获取、快进拉取不包含远程写权限。推送确认使用预览的精确提交编号，不支持 force。登录失效、连接修订变化、解绑、撤权或实例重启后需重新签发。相同空间的程序共享该能力，长期凭证仍保留在服务端。

## 分层环境诊断

管理员 `GET /api/diagnostics` 保留旧字段并增补只读 `environment`；`POST /api/diagnostics` 主动验证数据目录临时写入与 Linux 属主设置。普通用户使用 `POST /api/sessions/{id}/diagnostics`，硬性按空间属主限制，报告不含实例或其他用户详情。

两种路径下的 `/ws` 提供独立、同源、已鉴权的 WebSocket 探测。它不启动空间、不加入聊天房间，不触发模型或用量。每项返回 `passed/failed/not_checked`；HTTP 200 不代表全部检查通过。报告形状、CLI 参数、脱敏范围和实际验证边界见[分层环境诊断](diagnostics.md)。

## 首次使用快照

`GET /api/onboarding` 返回 `{version:1, can_configure, can_create, has_workspaces, accounts, default_models, container_resources}`，使用 `Cache-Control: no-store`。`has_workspaces` 只统计本人；每个账号仅含 `id/type/label/credentials_present/default_model`（新空间使用的模型，配置了可用模型的账号取账号默认值），未授权账号在读取凭证前就被排除，普通用户只收到相应 Agent 的默认模型。`container_resources` 仅含 `cpus/memory_mb/pids_limit`，是当前的新容器配置，不是运行中容器的实际值；不含网络等管理配置。创建收据视图使用相同投影，空间模型仍取其保存值。

`can_create` 表示存在支持且获准使用的账号，不是持久授权凭据，也不证明凭证、上游模型或额度可用。管理员始终可配置；其他客户端仍应以服务端实际创建/启动准入为准。创建成功响应及原有默认模型快照语义不变。

## 创建与项目导入收据

v0.1.11 包含 schema 11 引入的 `session-creations` v1 接口，支持同编号创建复用、导入尝试结果查询和未知结果核对。接口、输入、状态、作用域及重放限制见[创建空间与导入项目](project-creation.md)。这些收据不替代聊天消息确认；聊天持久接收使用下述独立协议。

## 聊天持久接收 v1（v0.1.11）

v0.1.11 的 schema 12 已将独立 HTTP 接收/查询/动作 API 接入原执行器，能力由 `/me` 的 `chat_protocol:1`、`chat_scope` 发现。浏览器按该能力使用新发送路径，旧服务端仍显示兼容限制。完整字段、状态与丢确认恢复规则见[聊天协议](chat-protocol.md)；`draft_protocol` 不是该能力的替代。

| 方法与路径 | 作用 |
| --- | --- |
| `PUT /api/sessions/{id}/chat/requests/{request}` | 持久接收冻结消息；同 ID 同内容读取原回执，不重复执行 |
| `GET /api/sessions/{id}/chat/requests/{request}` | 按 ID 查询状态；无需再次通过模型账号或额度准入 |
| `GET /api/sessions/{id}/chat/requests` | 最新 50 条回执及独立 pending；thread 过滤最近列表，pending_only=1 仅返回 pending 和空 requests |
| `POST /api/sessions/{id}/chat/requests/{request}` | 带 scope 的 interrupt / review / abandon；前两项须匹配 revision |

以上均为登录用户自己的空间；写操作只接受 Authorization 头。未知结果阻止旧 WS 和线程变更；review 不把旧结果改成成功。最终回执在既有用量事务和转录同步之后提交，没有新增扣款路径。
