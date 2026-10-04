# 桌面候选包、更新通道与验收

桌面与 Linux 服务端独立构建。桌面版本标签为 `desktop-vX.Y.Z`；稳定更新清单位于
`https://github.com/devilcoolyue/agentbox/releases/download/desktop-stable/latest.json`。
应用不读取 GitHub `releases/latest`，该入口仍留给旧服务端安装器及更新器。

## 构建和签名

`.github/workflows/desktop-release.yml` 手动构建 Windows x64、Mac ARM64、Mac Intel 三份候选包。
输入版本必须匹配 `desktop/package.json`、`desktop/src-tauri/Cargo.toml` 与 `tauri.conf.json`，
不在 CI 静默改源码版本。`signed=false` 构建未签名开发包，禁止嵌入正式更新公钥；
`signed=true` 缺少任何必要凭证立即失败，不降级为“签名成功”的开发包。

以下配置专用于本项目，不沿用 cursors_dashboard 或 fork 的身份：

| 类型 | GitHub 配置 |
| --- | --- |
| 更新验签公钥 | Variable `DESKTOP_UPDATER_PUBLIC_KEY` |
| 更新签名私钥及口令 | Secrets `DESKTOP_UPDATER_PRIVATE_KEY`、`DESKTOP_UPDATER_PRIVATE_KEY_PASSWORD` |
| Apple Developer ID | Secrets `DESKTOP_APPLE_CERTIFICATE`（base64 P12）、`DESKTOP_APPLE_CERTIFICATE_PASSWORD`、`DESKTOP_APPLE_SIGNING_IDENTITY` |
| Apple 公证 | Secrets `DESKTOP_APPLE_API_KEY`、`DESKTOP_APPLE_API_ISSUER`、`DESKTOP_APPLE_API_KEY_CONTENT`（P8 内容） |
| Windows Authenticode | Secrets `DESKTOP_WINDOWS_CERTIFICATE`（base64 PFX）、`DESKTOP_WINDOWS_CERTIFICATE_PASSWORD` |

Mac 构建后验证 codesign、Gatekeeper 与 stapled notarization。Windows 验证安装器 Authenticode。
Tauri 更新签名与上述操作系统签名是两回事，正式候选必须同时满足。Windows 安装使用每用户 NSIS，
WebView2 缺失时下载引导安装；离线 WebView2 场景仍须单独验收。

2026-10-04 已生成本项目独立更新签名身份，并配置 `DESKTOP_UPDATER_PRIVATE_KEY` secret 与
`DESKTOP_UPDATER_PUBLIC_KEY` variable。私钥保存在维护者本机仓库外的
`~/.config/agentbox/desktop-signing/updater.key`（0600），不得提交、打印或写入构建日志；应独立保管备份。
这份密钥未设置口令，对应 password secret 可为空。Apple Developer ID／公证与 Windows
代码签名证书仍未配置；开发包仍不嵌入正式更新公钥，不能据此宣称已完成签名发布。
已用该密钥实际签署隔离测试文件，独立 Ed25519 验证文件摘要及版本可信注释，并确认文件/版本
篡改均被拒绝；报告 `/tmp/agentbox-updater-signing-identity.json`，测试载荷和签名已清理。

公开发布及上传稳定清单仍是独立动作；工作流只输出 artifacts，权限 `contents:read`。
`desktop/scripts/release-manifest.py` 为三架构签名资源生成 `latest.json`、SHA256SUMS、
`release-request.json` 和 `channel-request.json`；后两者均显式 `make_latest:"false"`、`draft:true`。
资源文件名必须跨架构唯一。它验证组成和存在性，**不替代签名的密码学验证或安装测试**。
发布版本资产后才能切换稳定清单，清单中的 URL 固定指向版本标签，禁止指向可覆盖的裸下载地址。
发布前后应核对 GitHub `releases/latest` 仍为服务端 release，并实跑冻结旧安装器的解析结果。

## 应用内更新

界面提供手动检查、可选启动时检查、展示版本说明和二次确认安装。公钥在构建时由
`AGENTBOX_UPDATER_PUBLIC_KEY` 固定；renderer 无权修改公钥、清单端点或提供安装器路径。
只接受仓库内清单版本对应的精确 `desktop-v<version>` 标签下单个资源的 HTTPS 下载 URL，签名必须绑定清单宣布的版本
（`requireSignedVersion=true`）。下载限 512 MiB，失败保留旧应用，不自动重试安装。
安装时排除正在进行的同步和文件传输；下载验签成功后断开本机终端，远端 tmux 继续，
保留系统凭证及用户配置。应用包路径权限、公证、Windows 杀毒占用和更新中断仍须实际验收。

## 随包依赖和许可证

`desktop/scripts/collect-licenses.py` 从锁定的 Cargo 三目标依赖图、生产 npm 依赖以及现有
Go 许可证库存收集文本，输出 `desktop/third-party/inventory.json` 和 `NOTICES.txt`，随候选包打包。
Cargo 收集包括 build dependencies；不把 npm 测试工具列为客户端运行依赖。缺少声明/文本会失败。
部分 crate 发布包没有许可证文件，其固定源码提交处的文本保存在 `third_party/desktop/`，附 URL、
commit 与 SHA256；objc2 上游许可说明引用的标准 MIT 文本也一并保留。应在依赖升级后重跑并审查。

## 安装探针与实际验收的区别

`desktop/scripts/test-installed.py` 将 Mac .app 复制到含中文/空格的目录，或实际静默安装 Windows
NSIS，再用仅含系统目录的 PATH 两次执行 `--diagnostics-json`。诊断入口检查许可证资源、随包
sidecar 启动/协议/收尾，不读取 keyring；报告使用 create-new，不能覆盖已有文件。Windows 的
临时安装路径仍会涉及用户注册表，所以探针要求 `--disposable-windows-user` 且在 GitHub 托管
临时账号中执行，结束后卸载并验证程序移除。

传入 `--previous <旧包>` 才执行旧包到新包验证：旧包真实 sidecar 在隔离 loopback 服务和专用
SQLite 中创建设备身份、绑定、基线及完成历史，新包必须版本更高、架构一致，并能重复读取这些
记录，项目字节必须相同。未提供旧包明确报告 `upgrade: skipped`。Mac 验证完整 .app 替换，
Windows 验证 NSIS 覆盖安装；这两者**都不等于应用内更新器安装、GUI 设置、keyring 或中断恢复验收**。

候选工作流可选 `previous_tag`，`prepare-previous.py` 只接受本仓库精确 `desktop-vX.Y.Z` 标签，
检查版本、平台、下载大小、SHA256SUMS 并受限解包。来源与安装报告随 artifact 保存。哈希校验
不替代操作系统签名验证；没有正式旧包时不能用两个相同开发包伪造升级成功。
`test-installed-harness.py --sidecar <随包程序>` 可验证探针自身及合成旧 schema 迁移，需与真实安装证据区分。

## 跨操作系统同步与进程强杀

`.github/workflows/desktop.yml` 的 `windows-linux-sync` 使用 Windows 原生 sidecar 和 WSL2 中
实际运行的 Linux 测试进程。`test-windows-linux.py` 校验 PE/ELF 架构，记录 Windows 版本、
`wsl --version`、发行版 WSL 版本、实际 Linux `uname`、临时目录文件系统和两份二进制 SHA256；
必须为 WSL2，服务端数据位于 Linux `/tmp`，不得降级为 WSL1 或把 Windows 挂载目录当 Linux 文件系统。
只允许 GitHub 托管临时 Windows 账号运行该 launcher。结果、独立同步报告和 peer 日志保存为 artifacts。

就绪检查先在 WSL 内部通过固定脚本请求带 run ID 的 `/fixture`，再检查 Windows→localhost。
WSL 的自动 localhost 转发不可用时，launcher 只从本次 WSL 默认网络接口获取私有 IPv4，
启动 Python 进程内的 `127.0.0.1` 随机端口 TCP 转发到固定端口 8181；最多 16 个连接，
连接/空闲/总存活时间都有上限，结束时关闭监听器、连接和线程，不修改 netsh 或防火墙配置。
客户端仍运行在 Windows、文件仍位于 Windows，服务端仍运行在真实 Linux 内核和文件系统。
内外两次就绪响应必须属于同一 run ID/空间/项目，报告明确记录失败层和实际传输路径。

共同探针 `desktop/scripts/test-linux-sync.py` 验证双向二进制字节、重启基线、删除恢复、原内容导出
和永久收据清理，然后执行真实进程强杀：Linux 的生产 HTTP handler 已发布替换、保存 before 和
applied 日志后，测试包装层扣住响应；Python `Popen.kill()` 在 Windows 调用 `TerminateProcess`
（核对退出码 1），在 macOS/Linux 发送 `SIGKILL`（核对退出码 -9）。新 sidecar 必须保留 SQLite
started 意图并拒绝重放，等待真实 30 秒租约自然过期后核对、导出 before、显式 replan；两端崩溃后
用户编辑、旧基线、原操作 ID 都保留，服务器 apply 计数只增加一次。测试不调用时钟注入或强制释放租约。

控制端点只存在于 `TestClientLinuxPeer` 测试二进制，使用合成属主 token 和每轮随机 run ID。
只接受固定 `crash.bin` 的 prepare/arm/status/edit，没有任意路径、任意字节、命令或进程参数。
启动 peer 必须设置 `AGENTBOX_LINUX_PEER=1`、非空且本轮唯一的 `AGENTBOX_LINUX_PEER_RUN`；
Docker/WSL 入口只供本轮回环测试，不连接生产服务。已有隔离 peer 时可运行：

```sh
# AGENTBOX_LINUX_PEER_RUN 必须与正在运行的本轮 Linux 测试 peer 完全一致。
python3 desktop/scripts/test-linux-sync.py \
  --server http://127.0.0.1:8181 --sidecar /path/to/native/abox-sync \
  --require-native-os Darwin --peer-run-id "$AGENTBOX_LINUX_PEER_RUN" \
  --report /tmp/agentbox-cross-os.json
```

Windows CI 由 `test-windows-linux.py --peer <Linux测试程序> --sidecar <Windows程序> --report <报告>`
自动生成并传递 run ID，无需手工填写。`test-cross-os-harness.py` 只验证探针边界，不作为 Windows
运行证据。2026-10-04 本机 Mac ARM64→Docker Linux ARM64 强杀链路实跑通过，报告
`/tmp/agentbox-cross-os-kernel-kill.json`；Windows 对应 `TerminateProcess` 链路仍须以远端实际报告为准。
上述进程终止不是物理断电；也不替代 GUI、最低系统版本、休眠和物理设备验收。

## 冻结旧版本兼容回归

`desktop/scripts/test-server-compat.py` 默认从固定提交
`eb845db59e45ed042b1af9df44dae96f104bf43b`（v0.1.8 源码、schema 9）生成临时 git archive，
构建旧服务端、旧 abox-link 和当前服务端。隔离 Linux Docker 卷内验证升级到 schema 10、旧登录
令牌/账号/HTTP 文件与终端协议、隧道实际字节转发、服务重启、完整备份恢复及 purge。
完整备份在停止 fixture 后保留元数据复制到独立维护容器，不豁免正常备份的挂载安全检查。

```sh
# 仓库根目录；镜像需事先构建，脚本不拉取镜像，不使用真实账号/空间，不调用模型。
docker build -t agentbox-client-test:local -f internal/server/testdata/client.Dockerfile internal/server/testdata
python3 desktop/scripts/test-server-compat.py --image agentbox-client-test:local --report /tmp/server-compat.json
```

`--browser-smoke` 使用 Playwright Chromium 加载冻结网页资源，HTTP/WS 实际连接新服务端，验证
登录、文件上传/编辑/下载和终端；可通过 `AGENTBOX_PLAYWRIGHT_MODULE` 指定已安装模块路径。
`--desktop-smoke <Smoke.app 内可执行程序>` 在升级前连接冻结旧服务端，验证基础模式降级、
真实 tmux 中文输出/尺寸及确认上传后的字节。仅显式测试构建允许回环 fixture 配置，正常产品没有此入口。
Linux CI 已接入服务端与旧网页回归；原生桌面部分需有图形会话的 Mac/Windows 测试环境。
报告记录基线提交和三个二进制 SHA256；这覆盖一个冻结版本，不能代表所有历史服务端与全网页功能。

2026-10-04 本机 Mac ARM64→隔离 Linux ARM64 已完成上述单轮全链：新桌面→冻结旧服、
冻结旧网页与旧 abox-link→新服，以及迁移/重启/备份恢复/purge 均通过。正常 Mac 包隔离安装
探针也通过，真实旧包升级仍为 skipped。新增图片模块用 Windows Rust target 类型检查通过；
这些结果均不替代 Windows/Intel 实机、真实系统剪贴板/输入法或签名发布验收。

正式验收仍需记录：

- Windows 10 22H2/11、Mac Intel/ARM64 和 macOS 最低候选版本；真实 Finder/Explorer 文件/截图粘贴和拖放。
- Windows↔Linux、Mac↔Linux 文件同步，编辑竞态、磁盘满、断网、强杀、休眠和恢复。
- 新桌面→冻结旧服务端、旧网页/abox-link→新服务端；Linux Docker 终端启停/purge/重启。
- 从正式旧安装包升级到新安装包，更新中断/签名错误/版本不符/空间不足，凭证与配置延续。
- App 首次启动与重复启动、100%/150%/200% DPI、多显示器、中文/emoji/组合输入，WebView2 缺失与离线安装。

2026-10-03 本机已有 macOS ARM64 原生 UI/安装探针及 Mac sidecar↔隔离 Linux HTTP peer 的同步证据；仓库暂无上述签名 secrets/variable 或自托管实机 runner。
工作流存在不等于远端执行过；未发布任何 release，`sync=0` 未更改。完整目标仍未通过最终验收。


2026-10-04 新增更新器回归：使用真实 loopback HTTP、合成签名和锁定的 Tauri updater 插件，验证合法
签名、内容篡改、缺失签名版本、声明版本不符、截断传输、超限、停滞超时及失败后完整重试。Tauri 应用
runtime 为 mock，不运行安装器或读取 keyring；这不是已安装应用升级或正式签名的验收证据。
