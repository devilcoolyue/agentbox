# HTTP 与 WebSocket API

[返回文档目录](README.md) · [项目首页](../README.md)

## 认证与权限

`POST /api/login` 接受 JSON `{"username":"alice","password":"..."}`，返回 `token`、`user`、`role`。随后用 `Authorization: Bearer <token>` 访问 API；这个 token 是数据库里的登录会话令牌，不是配置中的 `auth_token`。

- 浏览器 WebSocket 和下载直链可在 **GET** 请求上使用 `?token=`；POST / PUT / PATCH / DELETE 必须使用 Authorization 头。
- 登录令牌为签发后 30 天的固定有效期，使用不会延长；退出登录和密码变更也可能使其提前失效。
- 空间接口检查属主，管理员也不能通过这些接口读取其他用户的空间。
- 普通用户只能读取自己的用量；其传入的其他用户名不会扩大可见范围。
- JSON 错误通常为 `{"error":"说明"}`，应结合 HTTP 状态处理。不要在调用日志中输出 token、Key 或配对码。

| 方法与路径 | 权限 | 请求 / 用途 |
| --- | --- | --- |
| `GET /api/ping` | 公开 | 轻量探活，成功 `204` |
| `POST /api/login` | 公开 | `{username, password}` |
| `POST /api/logout` | 已登录 | 撤销当前令牌 |
| `GET /api/me` | 已登录 | 当前用户、角色、时区、额度及界面所需信息 |
| `POST /api/me/password` | 已登录 | `{old_password, new_password}`；保留当前令牌，撤销其他登录 |
| `POST /api/tunnel/pair/redeem` | 凭配对码 | 配对码本身是一次性凭证，无需另带登录令牌 |

## 工作空间

```text
GET    /api/sessions                工作空间列表
GET    /api/sessions/{id}           单个工作空间详情
GET    /api/sessions/{id}/models    当前空间的候选模型、推理能力与发现状态（不保证上游调用权限）
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
POST   /api/sessions/{id}/upload    上传 multipart(file)，普通文件或 zip/tar.gz/tgz/tar；clear=1 先清空
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

Git 命令在会话容器内执行，会按需启动空间；额度拦截返回 `403`。Git 执行模块未配置返回 `503`，其他启动/运行错误按接口返回错误，不回退宿主机 Git，也不以空 diff 隐藏失败。单条命令限时 15 秒（随后最多 2 秒强制终止），stdout 上限 4 MiB；超过限制返回错误。网页提交禁用 hook 与签名。`git/file` 是普通文件读取，不执行 Git。

```text
GET    /api/sessions/{id}/git/status  变更列表（repos=工作区里发现的仓库、repo=当前那个、
                                      分支 + 文件状态，未跟踪目录逐个文件列出、超 2000 条
                                      truncated=true；一个仓库都没有时 is_repo=false）?repo=
GET    /api/sessions/{id}/git/diff    unified diff（?path= 查看单文件，相对仓库根）?repo=
GET    /api/sessions/{id}/git/file    ?path= 文件当前完整内容（新文件没有 diff，看的就是它；
                                      纯文本，超 512KB 或二进制拒绝）?repo=
POST   /api/sessions/{id}/git/commit  git add -A 后提交 {message, repo?}
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
POST   /api/accounts                新增账号 {id, type, label, env?, proxy_id?, access?, model_reasoning?}
DELETE /api/accounts/{id}           删除账号（仍被空间使用则拒绝，凭证目录保留）
POST   /api/accounts/{id}/oauth/start  Claude / Codex：生成对应 OAuth 授权链接
POST   /api/accounts/{id}/oauth/finish 提交 {code}；Claude 为授权码，Codex 为完整 localhost 回调 URL
POST   /api/accounts/{id}/apikey    保存 {api_key, base_url?, wire_api?}
DELETE /api/accounts/{id}/apikey    清除 Claude 中转配置
POST   /api/accounts/{id}/apikey/test  探测账号 API Key 配置
PATCH  /api/accounts/{id}           改账号 {label?, env?, proxy_id?, access?, model_reasoning?}（proxy_id 空串=解绑）
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
| `GET /api/system` | 服务、Docker、数据目录与数量概览 |
| `GET /api/monitor` | 运维监控数据 |
| `GET /api/storage` | 数据 / 缓存磁盘容量与分类统计 |
| `DELETE /api/cache/marketplace` | 清理可重建的技能市场缓存 |
| `GET /api/diagnostics` | 导出按字段白名单生成的诊断信息 |
| `GET /api/updates` | 当前构建信息与上次版本检查缓存 |
| `POST /api/updates/check` | 检查正式发布；`?force=1` 手动触发，仍受 1 分钟间隔限制 |

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
