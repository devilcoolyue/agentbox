# 桌面客户端 API v1（开发中）

这些接口是现有服务端 API 的增量扩展，不取代网页 `/api/sessions/{id}/term`、文件、聊天或
abox-link。首次运行开发服务端迁移到 schema 10；回退见[数据库迁移说明](architecture/database-migrations.md)。

除兑换配对码外，所有接口要求已有登录 Bearer token。会话路径统一按空间属主校验，子资源
按空间 ID 再校验，跨用户空间返回 404。写操作只接受 Authorization，不能用 query token。

| 方法与路径 | 请求 / 行为 |
| --- | --- |
| `GET /api/clients/capabilities` | `protocol_version:1`、`server_id`（64 位小写 hex）、当前认证 `user`，features 中 pairing/project_terminals/sync_recovery_gc/sync_recovery_inspect=1、sync=0；另含资源上限。两个 recovery 能力只宣告显式恢复管理，不能开启同步；sync=0 期间可做本地预检及历史恢复管理，不执行同步批次 |
| `POST /api/clients/pair` | 为当前用户签发 `{code,expires_in}`，不依赖隧道；10 分钟、一次性、重新生成替换旧码 |
| `POST /api/clients/pair/redeem` | `{code}` 换取 `{token,user,role}`；码是此接口的凭证，失败受登录速率限制 |
| `GET /api/sessions/{id}/client-projects` | 项目数组；只读元数据，不启动容器 |
| `POST /api/sessions/{id}/client-projects` | `{name,path,arguments:[]}`；建立已有目录的逻辑映射，不创建/移动目录 |
| `PUT /api/sessions/{id}/client-projects/{project}` | `{name,path,arguments:[],revision}`；完整替换设置，带修订号防并发覆盖 |
| `DELETE /api/sessions/{id}/client-projects/{project}?revision=N` | 仅移除映射，不删除目录；有终端时拒绝 |
| `GET /api/sessions/{id}/client-terminals` | 终端资源数组；不探测容器，不表示进程一定正在运行 |
| `POST /api/sessions/{id}/client-terminals` | `{project_id,kind:"agent"或"shell"}`；建立资源，第一次连接时才启动/附加进程 |
| `GET /api/sessions/{id}/client-terminals/{terminal}/stream` | 鉴权 WebSocket；二进制 PTY 数据，文本 `{"type":"resize","cols":N,"rows":N}` |
| `DELETE /api/sessions/{id}/client-terminals/{terminal}` | 明确结束资源；先持久化 closing，停止其 tmux，再删元数据；失败可重试 |
| `GET /api/sessions/{id}/sync/manifest?project=ID` | 只读、完整有界清单、digest、project_revision、project_path 和 windows_issues；不启动容器 |
| `GET /api/sessions/{id}/sync/file` | 条件下载；query 必填 project、project_revision、rules_hash、path、hash、size、executable（true/false），均来自预览 |
| `POST /api/sessions/{id}/sync/apply` | 条件 replace/delete/mkdir/rmdir；元数据与租约走头，文件原始字节走 body；详见下文 |
| `GET /api/sessions/{id}/sync/operations/{operation}` | 持久化操作状态（applied/uncertain）、路径及前后状态；另含 digest/device/project、retirement 和 retirement_confirmation；不触发重试 |
| `GET /api/sessions/{id}/sync/operations/{operation}/before` | 覆盖/删除前原始字节，带 Content-Length/ETag；不自动恢复 |
| `GET /api/sessions/{id}/sync/storage` | 当前空间的活动操作、永久执行收据、恢复内容、逻辑元数据占用和上限；不扫描工作文件 |
| `POST /api/sessions/{id}/sync/operations/{operation}/retire` | `{device,digest,confirmation}`，必须匹配原实例及持久化 applied 操作；显式回收原内容和活动名额，永久保留原 ID 收据 |
| `GET /api/sessions/{id}/sync/recovery-operations?device=&cursor=` | 独立于本地绑定/历史的分页清单，每页至多 50 项；可筛选原设备，默认全部设备；缺失/损坏日志明确显示 unknown 原因 |
| `GET /api/sessions/{id}/sync/recovery-operations/{operation}?device=ID` | 原设备范围内核验旧内容，比较当前文件与操作前后状态；返回显式清理摘要，不推断未知操作已完成 |
| `POST /api/sessions/{id}/sync/recovery-operations/{operation}/retire` | `{device,confirmation}`，空间锁内重新核对上述摘要后退役单个 applied 收据；永久保留原执行状态/ID，不修改当前文件 |
| `POST /api/sessions/{id}/sync/lease` | `{project,device,action,generation}`，action 为 acquire/renew/release；续租/释放另带 `X-Agentbox-Sync-Lease` 头 |

这些同步接口当前为开发中的协议基础，`sync=0` 不变。租约 30 秒有效，同一工作空间内
父子目录互斥，续租/释放须匹配设备、令牌和代次；令牌只走请求头，不放 URL。重启后必须重新
获取租约。错误 409 表示租约已失效或被其他写者占用，429 为容量/扫描并发限制，413 为扫描
超限，422 为不支持的文件/规则。活动租约期间项目更新/删除返回 409。写入要求租约、项目修订、规则与目标条件全部匹配。

服务端 `data/client-instance-id` 保存随机安装身份，正常重启不变；身份文件不进入系统或完整
备份，恢复出的实例首次访问生成新身份，客户端必须重新确认绑定/基线。损坏、链接或硬链接
身份文件不自动修复，能力/同步接口返回 503；原网页、会话和隧道接口不依赖这个身份文件。
直接复制整个 data_dir 会复制身份，不等价于备份恢复；独立克隆实例须使用备份恢复流程。
身份字段用于区分实例，连接认证仍依赖 Bearer 和 TLS。

同步请求可带 `X-Agentbox-Server-ID`，服务端在处理清单/文件/日志/租约/写入前比较，不一致
返回 409 并带当前身份；匹配时响应也带此头。现有开发原语仍可省略此头，未来执行器必须
使用已固定身份的 Remote；固定后遇到响应缺头或身份不同会停止。`Remote.ForBinding` 先查
认证能力，验证规范化 URL、server_id 和 user，固定身份不等于授权开启自动同步。

file 返回 `application/octet-stream`、精确 Content-Length、SHA-256 ETag 和 no-store；不转换换行、
编码或二进制。服务端在空间锁内检查项目修订、当前忽略规则、路径和文件属性，生成最多 64 MiB
的已验证内存快照后释放锁，再发送响应。与清单共用两个全局读取槽，下载槽持续到发送结束，
发送设置 60 秒写截止时间。过期条件返回 409、非法参数 400，链接/硬链接/忽略项 422；校验失败不发送
成功头或部分文件内容。下载只读，不要求租约；客户端自动覆盖本地副本仍必须经过执行器的租约、
预览和本地条件检查，不能因有下载接口就绕过 `sync=0`。错误或中途断流不得推进基线。

apply 使用 `X-Agentbox-Sync-Request` 传 base64url（无 padding）JSON，头值上限 16 KiB：

```json
{"version":1,"operation_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","project":"0123456789ab","project_revision":1,"rules_hash":"<64位SHA-256>","device":"device-id","generation":"<租约代次>","path":"src/file.txt","kind":"replace","before":null,"after":{"kind":"file","hash":"<64位SHA-256>","size":123,"executable":false}}
```

操作 ID 为随机 32 位小写 hex。replace 的 after 必须是文件；before=null 要求路径不存在，
否则必须匹配原文件 hash/size/executable。delete 需要文件 before、after=null。mkdir 为
before=null、after={kind:"directory"}；rmdir 相反且只能删空目录。后两者和 delete 的 body
必须为空。不能传递递归删除或文件/目录互换。所有写入仍只接受 Authorization Bearer；另需
`X-Agentbox-Sync-Lease`。接收文件不占空间锁，最终条件检查和写操作在空间锁内串行。

成功返回 `{operation_id,status:"applied",recovery,replayed}`；replayed=true 表示返回历史
收据，不代表当前文件仍未变化。重复 ID 不再次改文件，不同意图复用 ID 返回 409。故障后若
结果未确认，409 响应另含 `operation:{operation_id,status:"uncertain",recovery,...}`，客户端
须查询状态、重扫并核对，不能换个 ID 盲目重试。接收长度/hash 不符为 400；租约失效、条件
变化为 409；超限 413、传输并发满 429。日志在容器挂载之外持久化，不增加数据库迁移。

状态/恢复读取不要求租约或仍有项目映射，但空间属主权限始终必需。恢复内容下载按 hash/size
校验；取回文件不会自动写回工作区。桌面历史按绑定修订号分页，未完成批次固定在第一页，并显示
1024 绑定、1000 批次/绑定、256 MiB 逻辑元数据和本地恢复 1000 文件/256 MiB 的占用。只有原本不含
恢复引用且所有远端操作均已 retired 的已完成历史可逐条确认删除本机记录，保留服务器永久收据。
桌面私有 IPC `sync_recovery_discard` 另允许按操作 ID/revision 明确清理已完成批次的本地副本，保留审计引用。
`sync_remote_cleanup_review` / `sync_remote_cleanup` 提供整批远端回收的预览与确认，不自动清理或重试。
逐文件冲突选择、默认关闭且切换空间仍运行的持续同步，以及无法核对批次的显式未知结果归档均由 sidecar 提供。外部
CLI/IDE 并发写入不受租约锁定，详见 [同步边界](architecture/desktop-sync.md)。

## 服务器恢复副本回收

桌面仅在能力包含 `sync_recovery_gc=1` 时请求 storage/retire；旧能力不探测这些路径。请求仍经过
空间属主鉴权，桌面固定原 `X-Agentbox-Server-ID`，身份变化时不发送旧操作 ID。retire 服务端强制要求
该头存在且等于原实例身份，不接受只凭同一 URL 回收。清理不启动容器，不要求项目仍存在，但空间内
存在活动同步租约时返回 409，避免与正在执行的批次交错。

storage 返回：

```json
{"active_operations":0,"operation_limit":1000,"retained_receipts":0,"receipt_limit":100000,"recovery_bytes":0,"recovery_byte_limit":268435456,"metadata_bytes":0}
```

`retained_receipts` 包括已完成 retired 和 retiring 预占；retiring 在完成前仍占活动操作名额。逻辑
元数据包含永久收据文件，统计不含 inode/目录与磁盘分配开销。服务端缓存固定 journal 根 inode，在
空间锁内维护，日志变更后发生错误使受影响缓存失效。冷扫描按 1000 活动操作加 100000 收据有界枚举，
最多 60 秒；大量收据时可能较慢，错误不返回空统计，也不允许写入绕过配额。

调用方先读取操作状态，将 `digest`、`device` 与本地持久化意图逐项比较；`confirmation` 必须使用
返回的 `retirement_confirmation`，它绑定原实例、空间、操作 ID、意图摘要、设备和项目。只允许
`operation.status=applied`；uncertain、确认过期或不匹配均不能回收。成功返回完整操作状态，
`retirement="retired"`、`operation.recovery=false`。

服务器先持久化 `retirement="retiring"` 并预占永久收据名额，再校验 before 的单链接普通文件、
哈希/大小后删除字节并同步目录，最后持久化 retired。retiring/retired 均禁止下载 before。中断后
可用相同意图与确认续做；不能换 ID 重试原写操作。目录与原 ID 的 applied 收据始终保留，旧二进制
也仍将其识别为已执行操作。回收只释放旧内容容量与活动名额，不释放永久收据元数据；100000 收据
到限后拒绝新的回收，活动配额随后耗尽会停止新增操作。

桌面本地 schema 5 保存 `remote_retirement` 审计，自动迁移 schema 1/2/3/4，旧 sidecar 拒绝打开。
清理要求绑定无 pending，历史非 replan/abandoned，全部操作 verified，或 started 具有持久化 finish
及有效的整树核验摘要。预览绑定 revision 和精确操作列表摘要，至多 1000 项；执行前重新核对，并先
持久化本地 retiring，再请求服务器。成功后本地记录 retired，不修改当前项目文件或同步基线。失败
加载最新 revision/history，用户重新预览后续做；不自动重试，也不将缺失操作当成清理成功。

`sync=0` 仍保持；新增回收接口和本地 schema 迁移不改变服务端 schema 10，也不代表 P3～P5 已全面验收。

## 本地历史丢失后的独立恢复管理

`sync_recovery_inspect=1` 支持按空间列举与核对服务器日志，`sync_recovery_gc=1` 另授权显式清理。
这两项不依赖 `sync=1`。所有新接口强制要求已确认的 `X-Agentbox-Server-ID` 和空间属主鉴权；
本地数据库丢失后可使用新设备身份登录，再明确选择日志记录的原设备。device 是日志范围标签，
不能替代用户身份，也不能跨用户读取；核对/清理必须匹配该条日志的原 device。

清单返回 `{server_id,workspace,device,items:[{id,status?,issue?}],next_cursor}`。每页最多 50 项，
ID 升序；游标绑定实例、空间、设备及最后一个 ID。末页 `next_cursor` 为空字符串。目录枚举上限
为活动名额与永久收据名额之和，60 秒超时；错误不降级为空列表。列表不是冻结快照，期间新增的
较小 ID 应通过刷新第一页查看。缺少 record.json 的目录显示 `missing_record`；无法验证的记录
显示 `invalid_record`，均不猜测原设备、路径、执行状态，不能核对/导出/清理。设备筛选只列出能
验证设备的记录，查看所有设备才能看到这类未知条目。

核对返回 `status`（原执行收据）、`comparison`、`current`（可验证时）、当前项目路径/修订、
`recovery_state`、`lease_active`、`can_retire` 与 `digest`。比较结果为 matches_after、matches_before、
changed、project_missing、project_changed 或 unavailable；项目修订改变后不把新映射下的文件当成
原文件。旧内容状态为 available、missing、corrupt、none、retiring 或 retired。available 表示按原
before 哈希/大小、单链接普通文件约束验证了旧字节。目录、符号链接、硬链接和读错不会当作空文件。

applied 只证明这一条历史写入完成，不证明整批同步或当前目录一致；用户之后编辑文件、改变项目
映射或删除映射不阻止清理已完成操作的旧内容。uncertain 即使当前内容恰好等于 after 也保持未知，
不能退役。uncertain 的 before 若可验证仍允许导出，即使进程在持久化 recovery 标志之前崩溃。
导出只写到原生 picker 选择的映射外目录，不覆盖现有文件，不自动还原工作区。

独立清理摘要绑定原实例/空间/设备/操作意图和用户看到的当前比较快照。开始前复核，变化返回 409，
必须重新核对；空间内存在活动租约同样拒绝。本机仍有该操作 pending 引用时，sidecar 设置
`local_pending=true`、禁止此入口清理，应先完成原批次核对。清理期间 SQLite IMMEDIATE 事务阻止
其他 sidecar 新建 pending 引用；本机没有绑定/历史时也能完成服务器维护。

服务器把授权摘要与 retiring 意图一起持久化，随后沿用先验证旧字节、删除、同步目录、记录 retired
的流程。收到错误或丢失响应时不能显示成功；同一授权可以跨取消/服务重启续做，此时即使当前文件
后来又变化也不要求重做已开始的清理。保留永久 applied 收据，绝不重放旧写入、删除操作目录或
改变同步基线。unknown/uncertain 不会因清理入口存在而变成 applied。

私有 IPC：`sync_orphan_list/review/export/retire` 共用 `workspace,server_id`，list 可带 `device,cursor`，
其余要求 `device,operation_id`；retire 另带 review 的 `confirmation=digest`，export 目录仅由原生
picker 填写。事件分别是 `sync_orphan_listed`（orphan_page）、`sync_orphan_reviewed`（orphan_review）、
`sync_orphan_exported`（filename）、`sync_orphan_retired`（orphan_review）。这些命令不要求本地
binding ID 或 batch ID，不触发后台同步。

## 项目与终端

项目对象含 `id,session_id,name,path,arguments,revision,created_at`。路径是 `/workspace` 内规范化
POSIX 相对目录（根为 `.`），拒绝穿越/反斜杠/链接遍历；目录必须存在。名称最多 128 UTF-8 字节，
路径最多 1024 字节；最多 32 个启动参数，单项 2048 字节、总计 8192 字节。启动参数只用于
新建的 AI 终端，按参数原样传入，不由服务端当 shell 片段求值。

终端对象含 `id,session_id,project_id,kind,state,arguments,created_at`。创建时快照项目参数，
后续修改项目不改变已有终端。`open` 表示允许连接，不代表进程存活；`closing` 表示有待完成的
结束操作，此时禁止连接/输入。每空间最多 128 个项目和 32 个终端，包括 closing 资源。

项目修改目录或删除前必须结束其所有终端。空间删除会同事务清理元数据；目录操作与空间
生命周期通过 workspace 锁串行。每个新终端有独立 `abox-client-<id>` tmux socket 和 `work`
会话；旧网页仍使用默认 socket 的 `main`。名字改变不改 ID 或产生新的终端进程。

连接复用原来的账号准入、额度、启动/凭证准备、活动引用、30 秒授权/额度复查及用量补记。
关闭窗口、标签或 WebSocket 不杀 tmux；明确“结束”才停止该 tmux，自行脱离终端的后台进程
可能继续运行。停止整个空间保留终端元数据，容器重新启动后用户可再次连接。新终端要求镜像
包含 tmux；旧网页接口保留旧镜像 bash fallback。

常见错误：400 参数/目录无效，401 登录无效，403 账号/额度拒绝创建，404 资源不可见或已删除，
409 修订冲突/有终端占用/资源上限/正在关闭。WS 4003 为额度不足，4004 为账号撤权，4005 为
终端结束/关闭中；这些情况不自动重连。原接口的正常接管关闭行为保持不变。

旧服务端可能对能力查询返回成功 HTML，桌面按基础模式处理；401/网络错误/5xx/不兼容 JSON
不当作旧版降级。同步能力始终返回 0，直到条件写入、租约和跨平台同步实现并验收。

验证范围与剩余门槛见[实施记录](architecture/desktop-client-progress.md)。原生凭证库、Windows
安装、跨版本完整 Docker 启动、服务重启和 purge 并发仍需要相应验收，不能用本接口说明代替。
