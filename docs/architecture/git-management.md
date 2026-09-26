# Git 管理实施记录

目标：用户管理多套 GitHub / GitLab / 企业 Git 凭证；创建空间可选默认连接，仓库 remote 可单独覆盖；网页支持本地提交、获取、快进拉取、显式推送、分支与 PR/MR。

## 归属与边界

- Git 身份（提交姓名/邮箱）、Git 连接（上游认证）、Agent 账号池是三个独立概念。
- 用户拥有多个私有 Git 连接；组织/管理员连接通过显式授权共享。管理员维护共享连接，不因此默认获得冒用其他用户私有连接的产品权限。
- 工作空间可选默认连接；最终操作绑定到具体仓库、具体 remote、具体连接。同域名多账号不靠自动试用凭证猜测。
- Git 提交/索引/工作树操作留在会话容器，禁止宿主机 Git 回退。网络动作需要独立操作策略，不能直接把当前任意 repo config 交给含秘密的 Git 进程。
- 凭证不写入仓库 URL、home 模板、镜像或公开 API。静态密文和主密钥分开保存；备份/恢复、密钥轮换与失败恢复必须一起实现。
- 读取缓存状态不会联网；fetch、pull、push 是显式操作。pull 缺省 fast-forward only；push 缺省禁止强推和删除远程分支。提交成功/推送失败分开反馈，不能引导用户重复提交。
- 终端/Agent 能执行任意容器用户代码。同 UID 的临时凭证不是隔离边界，不能声称已交付给进程的 Token/私钥不可读取。共享只读凭证要靠上游权限或服务端 broker 实施，不能只隐藏按钮。
- 企业内网独立处理认证、网络路由、DNS 和 TLS/SSH 信任。隧道离线不得静默回落公网；不能用跳过证书校验适配公司 CA。OAuth 服务端换令牌、平台 API 与容器 Git 传输都要支持所选路由。

## 实施顺序和验收

1. 本地提交语义：按钮/弹窗/结果明确本地提交；返回 SHA 和 pushed=false；测试远程 refs 不变。
2. 用户级身份：普通用户和管理员均可维护自己的提交身份；持久化、隔离、删除清理；在所有空间网页提交中使用，不改写历史。
3. 远程识别与获取：remote 地址脱敏、上游分支、ahead/behind、unborn/detached/conflict 状态；区分本地缓存和最近 fetch；显式 fetch 的生命周期、超时/取消/进度。
4. HTTPS 连接：加密 Token 存储/更新/撤销/测试；用户/空间默认与 remote 绑定；clone、fetch、ff-only pull、显式 push；只读策略、目标校验、重定向/URL rewrite 防凭证外泄、操作审计和并发控制。
5. GitHub 与 GitLab OAuth：管理员配置应用和自建实例地址，用户授权；state/PKCE、固定回调、续期/撤销；不依赖部署者不拥有的 OAuth client secret。
6. SSH 与企业实例：密钥、known_hosts、公司 CA、内网路由、自建 GitLab API；分别验证连接、读、写权限。私钥导出与不可导出代理能力必须说清。
7. 分支、PR/MR、组织策略：创建/切换/安全删除分支，冲突反馈；GitHub PR/GitLab MR API；共享账号授权/撤权、保护分支提示、审计。

## 当前进展

- 已实现阶段 1、2 的代码；默认本地身份为当前用户名 + @localhost，用户可改为平台验证邮箱/隐私邮箱。页面明确这不是上游授权。
- 已实现阶段 3 的本地状态读取与显式 fetch，命令有超时限制。已提供活动阶段、真实传输字节和主动取消入口，以及最近成功网页获取时间。远程计数注明本地缓存；remote URL 的 userinfo、query、fragment 不下发。
- Git status 改用 porcelain v2 -z，正确保留中文、空格、引号、换行、重命名路径。网页 commit/discard 以仓库锁串行；终端/Agent 仍可能并发更改仓库，不能宣称隔离事务。
- SQLite schema 4 增加 git_profiles。旧身份不迁移为上游认证；删除用户同时清理该身份。备份已包含数据库，新库不可交给只支持 schema 3 的旧二进制。
- 阶段 4 已接入：用户私有 HTTPS PAT 连接、AES-GCM 密文存储、独立主密钥、备份/恢复解密校验、修订控制、停用/删除、用户默认、创建空间选择默认、remote 绑定、clone、独立仓库读取探测、fetch、ff-only pull、push-preview/显式 push、基础审计落库。
- 长效 Token 保留在服务端，由按次 Git smart HTTP grant 转发；推送校验 old/new/ref，只读 grant 不支持 receive-pack。上游拒绝重定向；缺省服务端直连 HTTPS，可选用户隧道与公司 CA；网桥复用 proxy_bridge 主机配置并分配临时端口。
- SQLite schema 5 增加 connections/bindings/defaults/operations；不可由旧二进制打开。主密钥位于 data/git-secrets/master.key，0600，目录不挂进空间。不可单独丢弃密钥文件；系统备份与恢复都验证数据库密文能解密。
- 阶段 4 的主密钥轮换、终端复用授权已补齐（见下节）。自定义远程管理（添加、改址、删除）已接入，先解绑再改址。用户审计查询、主动取消/阶段与字节进度、workspace 默认编辑已接入。
- 阶段 5 已接入 GitHub/GitLab OAuth 应用配置、state/PKCE/Cookie/原会话绑定、回调、续期、保留连接 ID 的重新授权与撤销。应用 Client Secret 经 Config.mutate 保存密文，配置/连接密钥均在备份恢复时校验。模拟提供方验证不等于真实注册应用授权验收。
- SQLite schema 6 增加操作结束时间；旧历史不伪造结束时间。重启遗留 running 标记 interrupted_unknown。Docker Git 增加 stdin EOF 驱动的 Python 进程组监督器，取消有 TERM/KILL 收尾；Docker 失联仍保留结果未知语义。
- 阶段 6 已接入公司 CA、用户隧道路由（兼容/透明客户端）、SSH 私钥/口令/固定主机公钥、公钥/指纹展示与密钥替换。OAuth 交换/续期/撤销复用授权用户路由。SSH 通过服务端 Go SSH 转发 native Git 协议，私钥不进空间，限制远端仓库和 push old/new/ref。
- SQLite schema 7 增加不可变 network 策略并纳入 AEAD 认证数据；备份按旧/新 schema 验证。SSH 当前只支持普通主机公钥 pin（单目标 known_hosts 语义）、非交互密钥、ASCII 仓库路径，不支持主机证书/任意 shell 命令。
- 阶段 7 已接入分支列表、从当前/已有引用创建并切换、切换本地分支、设置上游、安全删除已合并分支；PR/MR 已支持 GitHub/GitLab 同仓库列表、预览、草稿创建与重复请求识别；共享账号支持管理员 PAT/SSH 向指定用户授权只读/可写。密钥轮换与终端复用已补齐，验收记录见下文。

## 验证方式

Go 测试使用临时 SQLite、合成身份和本机临时 Git 仓库/空 bare 远程，不访问真实用户仓库或凭证。`scripts/test-git.mjs` 使用合成 API 检查普通用户身份编辑、本地提交、失败重试、远程缓存说明和桌面/窄屏布局。真实 Linux Docker 与真实 GitHub/GitLab OAuth/推送尚需后续集成验收。

本地验收：`go build ./...`、`go test ./...`、`npm run check`、`npm run build` 与合成 Git 浏览器测试已通过；桌面/390px 弹窗截图已检查。此结论仅覆盖当前已实现部分，不代表后续网络认证链路已完成。

第二批本地验收：真实 Git smart HTTP（httptest TLS + git-http-backend + 临时 bare repo）验证首次推送、fetch、快进更新、脏工作区拒绝、过期预览拒绝、只读与地址变更拒绝。容器命令仍通过本机 fixture 执行，尚不代表 Linux Docker 网桥验收。浏览器合成 API 验证添加连接/默认/绑定/获取/预览确认推送和窄屏布局。

第二批检查结果：完整 Go build/test、相关包 race、前端构建、Git 浏览器回归与全站浏览器回归通过。未连接真实上游账号，未部署生产。

克隆采用临时目录传输和检出，完成后不覆盖重命名为正式目录并绑定 origin；已存在目录拒绝覆盖。独立连接探测只证明指定仓库可读取，不承诺写权限。真实 smart HTTP 测试和浏览器回归均已覆盖克隆。

第三批验证：模拟 GitHub/GitLab HTTPS 服务覆盖 PKCE、无 Cookie 拒绝、防重放、注销/应用变更拒绝、并发刷新只换一次、重新授权不换连接 ID、上游撤销与本地停用；Config 持久化及备份恢复测试覆盖应用密钥。Python 实进程验证 TERM 超时后的 KILL；模拟 Docker attach 验证取消 EOF 与等待退出。尚未连接真实注册应用或执行 Linux Docker 网桥验证。

OAuth 参数依据：[GitHub OAuth Apps 授权文档](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps)、[GitLab OAuth2 文档](https://docs.gitlab.com/api/oauth2/)。

第三批本地检查：完整 Go build/test、相关包 race、前端类型检查与构建、Git 浏览器回归和全站浏览器回归通过。凭证写入同事务核验原登录令牌，覆盖删除/重建同名用户时迟到回调不得落入新账号的边界。

第四批验证：真实 Go SSH 服务 + 临时 bare 仓库覆盖 clone/fetch/push、测试读取、错误主机 pin；公司 CA 验证未知证书拒绝，真实 yamux/Link 合成隧道覆盖本人/他人隔离、离线和禁用不回落直连、白名单拒绝。OAuth 通过同一合成隧道和公司 CA 完成回调交换。相关包 race 通过，前端手机 SSH 表单已检查。

本机 Linux Docker daemon 的 `TestGitContainerLive` 已通过（合成 Alpine Git/Python 镜像，UID 1000、环境隔离、hook 禁用、真实提交/diff、取消后的进程退出）。此测试不覆盖容器经宿主网桥访问真实 GitHub/GitLab；网桥与真实平台注册应用仍待端到端验收。新增 golang.org/x/crypto v0.54.0 许可已采集并通过 third-party 校验。

分支验证：真实临时 Git 仓库覆盖创建/切换/删除/上游、陈旧当前分支拒绝、脏工作区拒绝、未合并提交拒绝；浏览器覆盖创建并切换。接口均按空间属主校验并在仓库写锁下检查 expected HEAD/ref，不承诺与容器终端构成隔离事务。

第五批：新增 `gitaccess.Forge` 窄平台 API 客户端，禁止重定向和任意 API URL，GitLab 支持子组与服务子路径，GitHub 支持 Enterprise。PR/MR 创建前检查本地 HEAD、远程来源/目标 SHA、已有打开请求；不自动推送/合并，结果未知不自动重试。SSH 显式选同平台 HTTPS API 连接。GitLab OAuth API 范围由用户单独选择。

共享采用 schema 8 ACL，管理员只能共享自己的 PAT/SSH（OAuth 私有）；消费者有效写权限 = 连接可写且该用户授权可写，所有使用入口/传输 grant 都重查。共享连接不暴露密钥和名单；网络走操作者隧道，审计按操作者，撤权清理默认但保留失效绑定防静默切换。删除/重建用户名不继承 ACL。

验证覆盖：模拟 GitHub/GitLab 列表/预览/创建/重复请求、权限错误脱敏、目标变化和只读拒绝、API URL/链接和重定向校验；真实本地 Git remote 添加/更新/删除/绑定保护；共享读写权限、消费者不可管理、管理员不能借私有账号、撤权即时拒绝和用户名重建不继承。浏览器覆盖 PR/MR 显式预览确认、草稿、XSS 文本与窄屏布局，以及管理员使用授权表单。未向真实 GitHub/GitLab 创建请求。

第五批本地检查结果：完整 Go build/test、相关新增路径 race、前端类型检查/构建、Git 专项浏览器回归通过。普通消费者界面无编辑/停用/删除/授权按钮，共享只读禁用推送；后端独立拒绝绕过。真实上游 PR/MR 与共享服务账号现场联调尚未进行；终端复用与密钥轮换已在后续补齐。

## 密钥轮换与终端复用

离线维护命令 `agentbox git-key-rotate --config /path/to/config.json` 要求停止服务，并取得同一个 data_dir 独占锁。先验证所有连接和 OAuth 应用密文，再原子新增版本化密钥；连接在 SQLite 事务内重加密，应用经 Config.mutate 重写。中断后执行同一命令加 `--resume`，继续使用当前活动密钥。连接 ID、授权、绑定与修订不变。

旧 `master.key` 与版本化 `keyring.json` 都位于 `data/git-secrets/`，权限 0600；V1/V2 密文可以混存。轮换保留旧密钥，以便中断恢复及历史数据解密，**不等于销毁已泄露密钥**。系统备份包含整个目录；不要手工删除旧 key 或只复制数据库。备份恢复已覆盖旧格式与混合版本。

仓库「远程 → 终端授权」可签发 30 分钟授权，绑定当前登录、空间、仓库、remote、连接修订和绑定修订。默认允许 status/fetch/ff-only pull，勾选后允许 push-preview/push；不提供强制推送、分支删除、任意 Git argv 或平台 API 通道。创建上限每用户 8 个、实例 256 个。网页可列出和撤销授权，过期/撤销/服务停止关闭网桥并取消正在运行的请求。

授权在空间 home 下安装 `.agentbox-git/<id>/abox-git` 与 `grant.json`。仅包含短期网桥地址与能力令牌，长期 PAT/OAuth Token/SSH 私钥以及用户登录令牌不进空间。命令使用 Python 3 隔离模式，禁用环境代理和重定向。原生 `git push` 不会自动使用该授权。整个空间内同 UID 的程序/Agent 均可使用能力令牌；交互确认提示不是额外的安全边界。

终端命令复用网页执行、锁、额度/账号准入、审计与取消机制；固定 repo/remote 不接受客户端替换。推送使用预览的 HEAD、远程 SHA、ref，服务端与传输层复核。每次执行检查原登录、空间属主、连接权限与绑定，在仓库锁内及开启远程传输时再次校验。Ctrl-C 请求取消，但不能证明远端回滚。授权文件在失效后保留为惰性文件，服务重启不会恢复授权；不自动删除用户可能修改过的目录。

第六批验证：密钥轮换覆盖独占锁、legacy/版本化密钥、混合版本、保留旧密钥、缺失/损坏密钥拒绝覆盖、取消/恢复与 SQLite 事务回滚；系统备份恢复覆盖 V1/V2。终端授权覆盖真实 HTTP 控制请求、Python helper、固定参数、只读拒绝、登录/解绑/停用失效和显式撤销；浏览器覆盖创建、命令展示、撤销及 390px 布局。

`TestGitBridgeContainerLive` 已在 Linux Docker daemon 上通过 HTTPS 与 SSH 两条真实容器链路：合成远程、公司 CA/主机 pin、容器 clone/fetch/ff-only pull、网页 push-preview/push、容器内 Python 的 status/fetch/push-preview/push。测试容器没有宿主挂载，宿主目录发现使用临时占位目录，容器由测试 fixture 启动；不把此测试表述为完整生产启动/挂载验收。真实平台注册应用、企业 SSO 和公司内网的现场联调仍需部署环境配置。
