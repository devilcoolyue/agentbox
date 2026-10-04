# 桌面客户端实施记录

## 2026-10-04：发布 Desktop 0.1.2 公开测试版

已将本次新增桌面客户端发布到 GitHub Releases：
[desktop-v0.1.2](https://github.com/devilcoolyue/agentbox/releases/tag/desktop-v0.1.2)（Release ID `402907654`）。
固定源码为 `96149c57625734d1337f73b5265d6070891585a9`，三平台原始包来自
[run 37186392972](https://github.com/devilcoolyue/agentbox/actions/runs/37186392972)。六项 CI 均已通过，
包括三平台构建/安装/原生界面、服务端兼容、Windows→Linux 同步及 Linux 故障恢复，含 59 项前端测试。
Mac ARM64、Mac Intel、Windows x64 正常安装包随附校验和、来源记录与第三方许可说明，
共六份附件，远端文件大小和 SHA-256 与本地一致；安装包不包含 smoke 测试构建。
发布前对源码历史、跟踪源码与三架构自有制品运行 gitleaks v8.24.3，0 项命中；
扫描范围和微软 Runtime 的独立验证见开发验收表。
发布按未签名预发布处理，显式 `prerelease=true`、`--latest=false`；
不修改 `desktop-stable` 更新清单，本版手动下载安装。
发布前后服务端 `releases/latest` 均为 `v0.1.8`（Release ID `401867374`），
冻结旧安装器的发布前解析通过，桌面预发布未替换原服务端稳定入口。
本次不部署生产服务端，也不打开默认关闭的同步入口。0.1.1 的历史验证记录保持原样。

## 2026-10-04：0.1.2 修复工作空间运行状态滞后

用户实测发现兼容模式终端已连接且CLI正常显示，顶部及侧栏仍显示登录时的stopped快照。
原来空间列表只在登录或手动刷新时更新。现在终端连接/断开变化通知App读取服务端状态，
项目终端经ProjectWorkspace转发同一事件；可见时5秒轮询，窗口重新获焦/恢复可见时即时刷新。
请求串行并合并为一次尾随读取，避免旧请求覆盖连接后的状态；退出/重登的代次隔离旧结果。
顶部、侧栏和选中空间同步更新，组件key不变，刷新不会重连终端；断开也不会直接把空间标为停止。

验证：59项前端测试（含11项刷新并发/生命周期回归）、类型检查和构建、Go全仓build/test通过。
Playwright合成bridge实际页面覆盖忙时尾随刷新、两处状态一致、断开仍运行、获焦/轮询更新、
项目事件转发及不重建终端；截图`output/playwright/workspace-status-running.png`。
Mac ARM原生legacy/projects smoke通过，fixture初始为stopped，仅终端WebSocket接受后变running，
新增断言同时核对顶部文字和侧栏标记。测试不连接生产服务器，不代表Windows/Intel已运行此补丁。
版本统一提升到0.1.2；此前0.1.1六项CI的证据仍保留，不混作0.1.2的验收结果。

## 2026-10-04：功能开发与未签名自动化验收完成

最终功能提交 `926690df08edd84ab67bf67a805d526538c8eaed` 的
[run 37183406848](https://github.com/devilcoolyue/agentbox/actions/runs/37183406848) 六个job全部通过。
Windows已实跑新增sharing/lock重试与条件复核测试，45项Rust、系统凭证、NSIS安装/诊断/卸载和
两类原生界面测试全部通过；Mac ARM/Intel均通过打包、安装探针、三类原生界面与故障恢复。
Windows→真实Linux同步、冻结旧服务端/网页/abox-link兼容及Linux真实ENOSPC/SIGKILL也通过。

本地最终Go全仓build/test/vet通过（server248.557s），同步四包通过；前端48项测试、类型检查
和构建通过，Rust61项默认测试及Clippy通过。独立Go→Rust集成验证预览重试标记不会用于写入。
三平台0.1.1候选与原始报告保存在 `output/desktop-candidates/0.1.1-926690d/`，包含源码/run/
artifact来源和SHA-256。当前状态和明确未覆盖的正式签名、最低系统与物理交互见
[开发验收表](desktop-client-acceptance.md)。本次交付为未签名测试版，没有公开发布或修改生产配置。
下载后三包校验和已核对；两份DMG只读挂载及ad-hoc签名完整性检查通过。最终ARM64包与真实
0.1.0旧包在隔离目录再次完成升级验证，设备、绑定、基线与历史保留，用户安装和真实凭证未改。

## 2026-10-04：最后两项重试行为与三平台验收收尾

此前修复已由提交 `542ef9d9fd4bd5686e1cc4417a0054804bde7016` 的
[run 37182065422](https://github.com/devilcoolyue/agentbox/actions/runs/37182065422) 六个 job 全部通过：
Mac ARM/Intel 原生构建、安装探针、legacy/projects/sync UI，Windows 实际 NSIS 安装/卸载及
legacy/projects UI，Windows→WSL2 Linux 同步，以及 Linux 故障和旧服务端兼容回归。
Windows 原生界面报告的 cp1252 输出问题已修复并增加成功/失败中文报告回归。

原计划最后两项行为现已补齐：

- 持续同步只在只读预览遇到明确瞬时连接、断流、超时或 HTTP 408/429/500/502/503/504 后，
  按1/2/4/8秒最多重试四次。Go保留脱敏类型，Rust核对原命令，前端只在预览阶段接收该标记。
  每次重新生成预览，成功后重置预算；取消、换项目、退出取消等待。证书、鉴权、协议、冲突和
  任何写入失败仍暂停。真实Go→Rust用例验证同样503仅允许preview重试，apply不会获重试标记。
- Windows原子替换只对sharing/lock violation最多尝试六次、等待合计575ms。每次重新核对
  租约、根和父目录身份、目标条件；等待后还核对暂存文件身份及哈希，恢复副本只生成一次。
  `ACCESS_DENIED`及其他永久错误直接返回，取消不再发布，绝不先删除目标。原生Windows测试
  用暂存源文件句柄稳定触发共享冲突，并另验目标占用返回32/33或5时的各自安全行为。

前端48项测试、类型检查/构建、同步Go四包与新增定向回归、本地Rust原生桥接测试通过。
最终代码提交、候选包来源、完整测试及仍需外部资源的验收统一维护在
[开发验收表](desktop-client-acceptance.md)。以上原生UI使用独立合成测试构建，不等于用户安装包
在最低系统、真实IME/Finder/Explorer或正式签名升级环境中的验收。

## 2026-10-04：继续完成 P1～P5 开发缺口与未签名验收

本轮以用户确认的“先完成开发与未签名包验收”为边界，继续关闭实际代码缺口：

- 桌面与 abox-link 配对码绑定发行时的用户创建身份和密码快照；重置/删号重建后不能兑换。
  密码登录和配对兑换原子核对身份再发 token；HTTP 改密与撤销其他 token 同事务，失败回滚。
  满额时仍允许同用户替换自己的旧配对码。无 schema、URL 或 token 载体变化。
- 新增默认关闭的 `desktop_sync_enabled`，完整进入配置读写、mutate 与管理员 settings API。
  新引擎测试使用实际配置开启能力，不再在 HTTP wrapper 中伪造 sync=1；默认部署仍 sync=0。
- 原生剪贴板文件读取先校验32项/路径长度/总表示上限，严格处理Unicode、ANSI和文件URL，
  文件/图片/文本读取复核剪贴板修订。Mac私有pasteboard、Windows自有HGLOBAL测试隔离于
  用户剪贴板。前端统一路由拖放，隐藏页面/弹窗外未接收票据立即释放，原生投递失败也释放。
- 系统凭证保存、重开读取、服务器/用户隔离和删除使用独立validation namespace实际验收；
  凭证身份算法及正常服务名不变。应用二次启动恢复最小化窗口，本地后台诊断不再误报同步关闭。
- 更新排除同步、传输、选择、目录检查并先确认后台退出；失败保留已有终端，失效候选需重新检查。
  Mac保留原下载/版本签名验证，新增同目录准备收据、受限解包、bundle身份/架构检查、原子交换
  与旧包恢复引用。真64MiB镜像ENOSPC、真实子进程SIGKILL（解包后/交换后）和下一次更新恢复通过；
  不涉及用户安装或物理断电。报告 `/tmp/agentbox-updater-real-enospc.json`。
- 发布资源名、SemVer、512MiB限制、签名文本大小与客户端验证契约对齐。Windows测试程序
  统一嵌入Common Controls v6 manifest，避免只有主程序带声明；新增PE资源及导入检查。
  Intel原生同步fixture复用21次历史提交的StateStore，保留真实引擎/持久提交和原时间门槛，
  补阶段耗时日志；Windows/Intel修复效果待本轮后续CI。

本地已通过 Go 全仓 build/test/vet（server235.068s），账号/配对相关四包定向race，34项前端
测试与构建，Rust默认58项+两项独立SIGKILL测试、普通/smoke Clippy，9项候选/清单与8项启动器
回归，随包450项许可证收集。真实Linux Docker冻结schema9→10兼容探针新增中文/特殊字符
文件、shared/workspace下载ZIP与容器uid1000读取/属主，旧token/abox-link/tmux/备份/purge仍通过；
报告 `/tmp/agentbox-pairing-unicode-compat-20261004.json`。

先行CI `37172234744` 的Mac ARM完整job、server-compat、disk-full、Windows↔Linux同步均通过；
Windows遇到测试EXE缺少控件激活声明，Intel在sync_continuous后耗尽整体预算。本节列出的对应
修复须由新CI验证，不把已找到原因等同于通过。未公开发布、未部署生产；正式签名和最低系统/
物理交互验收仍按原资源约束单列。

## 2026-10-04：桌面界面对齐现有网页版

按用户确认，以 Agentbox 现有网页版为视觉基准重做桌面布局：复用原 Logo、94 个动作图标与
Claude/Codex 标识，配色采用网页原有深色/暖色浅色 tokens 和琥珀强调色。新增跟随系统主题，
保留已有显式主题偏好，终端在两种主题下都使用相同深色屏幕。

侧栏集中服务器、空间搜索/列表、账号与设置；主内容分为终端、文件、同步和恢复记录。
项目常用 AI/Shell 操作直接可见，设置/移除/远程结束移入菜单；终端的搜索、附件、粘贴、
更多菜单及暂存输入统一排版。文件页提供工作区/共享目录、面包屑、文件列表与独立上传确认。
外观、连接/配对/本地预检、版本更新集中到设置弹窗；保留启动自动检查和安装期间 busy 保护。

终端与同步组件跨标签/空间切换保留挂载。文件页按需加载；隐藏文件页时丢弃未执行的上传
预览，晚到结果不能弹出确认框抢焦点。弹窗支持 Tab 循环、Escape、焦点返回及 busy 关闭限制，
并覆盖 details summary。避免使用 Safari 15 缺少的 Array.at/原生 modal dialog 等接口。
800×560 小窗口与高缩放使用紧凑导航，短窗口压缩工具区以保留终端输入区域。

验证：Vue 类型检查、30 项前端测试通过；浏览器使用隔离 Tauri fixture 检查深浅主题、
1180×780/900×650/800×560 和 400×280 布局、终端/文件/设置/项目/上传弹窗；实际交互确认
标签切换不重连终端、隐藏预览被丢弃、设置焦点循环与 Escape 返回正确。浏览器 fixture
只用于展示和前端交互检查，不作为原生网络/凭证/系统手势验收依据。

Mac ARM64 原生 legacy/projects smoke 通过，覆盖登录、旧能力回退、两个独立终端、UTF-8
回显、resize/缩放、附件/目录上传及随包后台。同步 smoke 首次在测试历史准备阶段触发10秒
请求截止；已将21次真实 Preview/Apply 拆成21个有界请求，逐次核对累计数量。macOS 同步
测试复用 smoke.py 的 LaunchServices 启动及所属进程收尾，避免直接启动二进制时窗口无法
激活。完整场景在110秒时已通过历史分页/清理、导出、逐文件冲突和持续同步，尚未跑完后续
归档/取消；整体预算据实调整为180秒（Python170秒，预留清理），每请求/单步10秒与最终
历史断言保持不变。启动、配置校验、超时收尾与成功报告约束的8项 harness 测试通过。
这些调整只影响测试 fixture，不改变生产同步 API。

最终 `TestDesktopSyncNativeSmoke` 在 Mac ARM64 125.46 秒通过：真实 Vue→Rust→Go 链路完成
分页/容量、逐文件冲突、持续同步、未完成批次核对、异常归档、两类恢复清理/导出、取消和退出。
正常0.1.1应用已重新构建，前端不含 smoke 入口；ad-hoc签名完整性与打包ZIP校验通过。
包位于 `desktop/src-tauri/target/debug/bundle/macos/Agentbox.app`，ZIP位于
`output/Agentbox-0.1.1-macos-arm64-ui.zip`。此次界面验收范围为浏览器与本机Mac ARM64；
Windows/Mac Intel原生体验、最低系统与正式签名/公证仍按原计划待验收。生产服务端sync=0不变。

## 2026-10-04：首轮真实三平台 CI 与后续修复

首轮代码提交 `fad42f9` 已推送 `codex/desktop-validation-20261004`，实际运行
https://github.com/devilcoolyue/agentbox/actions/runs/37154757861 。冻结旧服务端/网页/abox-link
兼容 job 与真实 Linux Writer ENOSPC 通过；Mac Intel/macOS15.7.9 的 Go 图及38项Rust测试
通过。尚未完成三平台打包/GUI，不能将这些局部成功视为整轮通过。

本轮实跑发现并处理：

- Windows Python 默认cp1252解码Cargo UTF-8元数据失败；JSON子进程和文件读取已明确UTF-8，
  实际非ASCII子进程/中文路径回归通过。Windows native/NSIS阶段此前均未执行。
- Intel使用的Clippy1.99新增lint拒绝chunks_exact(12)，改为MSRV支持的as_chunks，10项图片
  回归通过；后续仍需CI验证。复制快捷键在无选区时保留系统剪贴板内容。
- Mac ARM卷校验拒绝runner测试目录，现有日志不足以确定具体字段；新增只读白名单环境报告，
  待下轮采集实际属性后判断，不放宽策略或跳过测试。
- Windows实际已启动WSL2/Ubuntu24.04/Linux6.18.33.2，但Windows回环连接超时，未开始同步。
  现增加Linux内部就绪探测、私有接口校验及受进程生命周期约束的回环TCP转发，不改宿主网络配置。
  12项探针测试与Linux启动脚本实跑通过，Windows效果仍待下轮。
- Linux原生强杀测试必须按服务端属主规则运行；CI原普通用户导致发布失败，现改为只读、禁外网
  的隔离root容器，预编译生产sidecar并校验入口/架构。真实Linux内核SIGKILL恢复30.86秒通过。

另新增整引擎真实4MiB tmpfs ENOSPC：prepared/started持久化失败得到SQLITE_FULL(13)，
远端apply为零、独立SQLite完整性/旧state保持；下载已落started并真实读取3757 HTTP字节后写入
失败，原文件/pending/基线保留。释放空间、核对/replan/重试及原副本导出通过。测试不注入假错误，
不填宿主盘；Linux ARM Docker实跑0.50秒。精确边界与命令见desktop-volume-policy.md。

Windows离线修复hook补上pv=0.0.0.0状态，分别核对机器/用户两个Hive；有效机器版本优先，
否则采用有效用户版本，只有双确定缺失才补装。未知访问/类型/容量错误不会被当成缺失，安装和
提取失败阻止App复制，正常返回保留寄存器/栈/Errors。实际NSIS编译通过两种安装上下文、21个
状态fixture和只读registry ABI探针；Windows实际执行及真实缺失Runtime的离线guest仍待验收。
实际包审计也适配了NSIS solid归档未知Size，流式提取仍限额/限时并核对两个payload同源、微软
签名及下载哈希。后续修复尚在独立验证分支，不是公开发布或生产部署。

## 2026-10-04：继续完成 P3/P4 补项与交付验收（进行中）

Mac 卷策略现要求 APFS/HFS、内核与系统元数据类型一致，并核对全部 APFS backing stores；
外置、网络、磁盘映像和未知卷拒绝，嵌套挂载无法绕过固定 Root guard。真实临时 HFS+ 映像
验收已通过并清理。每个新 Root 核验一次，逐文件热路径不运行 diskutil；1 万个 1 KiB 文件
完整扫描约 1.301 秒，为本机 APFS 热缓存基线，详见 desktop-volume-policy.md。
元数据查询沿请求/父 EOF context 取消，实际阻塞子进程取消测试通过；Mac Rust 侧车另用私有
进程组收尾，通过 WNOWAIT 保留 leader PID，清组后才回收，避免 PID 重用误杀。4 项真实进程
用例覆盖正常 EOF、超时、Drop、leader 先退出遗留 helper，独立进程不受影响。

独立服务器恢复入口已接通 Go API/IPC、Rust 与 Vue：每页50条、按原设备筛选、核对当前文件、
导出真实 before、逐条输入确认清理；本机旧历史丢失也可使用。uncertain 不推断完成，允许导出
核验成功的原内容但不能清理。retire 绑定原实例、属主、设备与当前比较摘要，保留永久 applied
收据，并持久化授权以支持丢响应/重启续做。已有本机 pending 引用会阻止清理，包括同实例换
域名/协议访问的情况。只读维护按 inspect 能力，清理另需 gc，正常 sync=0 不变。
新增真实 HTTP/IPC、跨 URL pending、设备筛选 I/O 回归通过；原恢复/清理组在 guard 优化后
227.65 秒通过，优化前进程曾600秒超时，不能把旧超时记录视为新版已消除全部性能风险。

P4 增加80%～200%整体界面缩放、偏好保存、平台快捷键及终端右键/更多菜单；组合输入保护、
Windows Ctrl+C中断、原生拖放坐标随缩放换算均已接入。前端29项测试与类型检查/构建通过。
原生首次测试证实缩放改变真实 WebView 尺寸且双终端输入回显成功，随后可见性门槛失败；
同步原生流程也在锁屏状态下超时未完成。启动器已补 LaunchServices、主线程激活和窗口诊断，
保留真实 visible/rAF 门槛，墙钟截止改 Date.now。只读 CoreGraphics 会话证据确认本机
screen_locked=true、display_asleep=false、login_done=true（/tmp/agentbox-smoke-session-environment.json），
当前不能把新增原生界面验收标为通过；解锁后还需完整重跑。

新增真实生产 abox-sync 的 SIGKILL 回归：HTTP写入及双方日志落盘后扣住响应再强杀，重启验证
pending、自然30秒租约过期、before导出、旧基线不推进、两端后续编辑保留，apply始终只发送一次。
最终 Context/FsType 版本57.02秒通过；这是内核强杀，不是 os.Exit/故障注入，也不代表物理断电。
Windows/macOS Intel/Linux 相关测试程序交叉编译通过；更新后的跨OS脚本已实际完成Mac ARM
到Docker Linux ARM同步，报告 /tmp/agentbox-cross-os-updated.json。Windows CI新增原生PE侧车
到WSL2 Linux ELF peer的严格联调，拒绝WSL1/错误架构/非Linux文件系统/跳过；尚待推送后实跑。

桌面版本升为0.1.1，计划对已安装的真实0.1.0开发包在隔离目录验证升级，用户的/Applications应用
尚未替换或重启。更新签名身份已生成并配置GitHub，已验证真实文件/版本签名及篡改拒绝；私钥在仓库外0600文件，Apple分发/公证与
Windows代码签名证书仍缺。开发包不启用正式更新。当前工作分支 codex/desktop-validation-20261004，
准备提交/推送到独立验证分支；最新refs、工作文件以及正常应用/侧车二进制字符串已扫描，无未审查
密钥命中。未生产部署或公开发布，远端三平台流水线尚待实跑。

后续性能补强通过一次受限祖先句柄遍历保留完整身份链与重叠检查，代表性真实绑定从8.800秒降至
0.365秒；原根/父目录替换、关闭句柄、APFS firmlink、映像覆盖、取消与限额回归通过，无全局缓存。
优化后的全仓 Go build/test/vet 通过（server179.391秒、syncclient41.527秒）；Rust38项测试、
普通与smoke all-targets Clippy通过。新增真实Mac原生侧车到Linux peer的强杀/重启故障链通过，
Windows同一脚本要求TerminateProcess退出、新PID、真实租约过期、恢复导出与apply不重放，仍待CI。

已使用真实已安装0.1.0开发包，对0.1.1普通包执行隔离完整.app替换与重启读回：设备/绑定/基线/
历史保留、schema5保持、中文路径及首次/重复诊断启动通过（/tmp/agentbox-installed-upgrade-0.1.1.json）。
这不是用户profile、GUI设置/keyring或应用内updater安装验证；正常包为ad-hoc、未公证。

最低版本静态审计修复两处crypto.randomUUID与三处Array.at调用，任务ID用getRandomValues生成
UUIDv4，无Math.random回退；Vite明确目标safari15。30项前端测试、类型检查与构建通过，仍不等于
macOS12.0实跑。Windows完整NSIS改为内嵌离线WebView2并附闭源Runtime声明，新增实际payload的
微软签名、SHA256、来源与512MiB上限审计及离线guest前置检查；10项harness通过。现有Runtime的
托管runner会明确报告缺失/离线验收blocked，不卸载共享Runtime或伪造缺失，完整离线guest验证仍待资源。

## 2026-10-04：当前剩余清单

以下为本轮继续开发前的状态快照；其后的完成情况见上方最新条目。较早记录中的“待实现”不代表当前仍缺失。
历史分页、逐文件冲突选择、跨空间持续同步、手动目录上传、本地及服务器恢复副本清理均已实现。

- P3 开发补项：Mac 可移动卷识别/限制尚不完整；本地历史丢失后的孤立远端日志缺少独立核对、
  导出和安全回收流程，不能猜测完成状态或直接删除操作目录。
- P4 开发/实测补项：已有主题、终端字号、搜索和平台复制粘贴快捷键；整体应用缩放尚无明确
  实现。右键菜单的可发现性需按系统实测，再确定补项。批量一键恢复属于增强项，当前支持核对和导出。
- 平台与真实交互：Windows x64、Mac Intel、最低系统及 WebView2；Finder/Explorer 拖放、文件/
  截图剪贴板、中文组合输入、多屏/DPI、休眠唤醒与子进程异常退出收尾。
- 同步稳定性：Windows↔Linux 实跑及跨平台编辑竞态、文件占用、断网、强杀、磁盘满/断电恢复矩阵，
  大型目录性能验收。Mac↔Linux 基础链路、Linux 局部真实 ENOSPC 与一个冻结旧版本兼容已通过，
  不能据此视为完整故障/全部历史兼容验收。
- 安装更新：正式旧包到新包、应用内更新安装、中断/空间不足、GUI 设置与系统凭证保留；
  当前安装探针和更新验签回归不替代这些验收。
- 正式交付：配置本项目 Apple/Windows/更新签名凭证，公证、三架构候选包与远端流水线实跑，
  独立桌面发布/稳定清单切换及服务端 releases/latest 兼容核验；整理提交尚未提交的代码和文档。

正常服务端 sync=0；补齐同步边界和验收后再单独开放和发布。当前未部署新服务端或公开发布桌面版。

## 2026-10-04：区分登录失败与登录态过期

桌面将账号登录接口的 401 误显示为“登录无效或已过期”，导致首次账号密码校验失败时提示不准确。
现改为固定的账号密码错误提示，配对接口的 401 单独提示配对码失效；登录后的身份/其他接口仍保留
登录态失效语义。新增真实 loopback HTTP 回归覆盖三种情况，不透传任意服务端错误正文。
此修正不改变认证协议、用户账号或密码；已安装应用仍可直接使用正确的网页登录账号登录。

本次新增 HTTP 回归通过，Rust fmt/Clippy、diff 检查、前端类型检查与普通构建通过。
Mac ARM64 普通开发包已重建（0.1.0，65.89 MiB，ad-hoc、未公证）；隔离安装探针通过中文/空格
路径、系统 PATH、首次与重复诊断启动，报告 `/tmp/agentbox-installed-login-fix.json`。
没有真实旧包，升级明确 skipped。新包位于 `desktop/src-tauri/target/debug/bundle/macos/Agentbox.app`，
未替换或重启用户正在测试的 `/Applications/Agentbox.app`，该已安装版本尚未包含新错误文案。

## 2026-10-04：冻结版本兼容、剪贴板解码边界与安装升级探针

新增 `test-server-compat.py`，默认用固定提交 `eb845db59e45ed042b1af9df44dae96f104bf43b`
的临时 git archive 构建旧 server/abox-link，与当前服务端在独立 Linux Docker 卷中联调。
已实跑 schema 9→10、旧 token/账号/文件/压缩下载/线程/终端、27 个旧静态资源 URL、旧 link
实际 TCP 字节及重启重连、优雅关闭后 tmux 保留、离线完整备份/验证/恢复/UID、旧二进制拒绝
schema 10 回退，以及 purge 清理项目/终端元数据。报告绑定基线提交及三个二进制 SHA256。
完整备份使用停止后的 fixture 副本，没有绕过挂载检查；这不是生产部署或全历史版本矩阵。

原生测试构建增加 `AGENTBOX_SMOKE_COMPAT`，只接受回环 fixture，新桌面已连接真实冻结旧服：
验证登录/基础模式降级、tmux 中文输出、stty 精确尺寸、附件与目录覆盖确认、HTTP 和容器实际
字节。首跑因测试容器 tmux 默认状态栏占一行而未满足尺寸断言，固定 fixture 的 status off 后
通过；产品终端逻辑没有为测试放宽。旧网页 Chromium 脚本只冻结静态资源，API/WS 直连新服，
实际加载 93 个冻结资源，登录/上传/编辑/下载/中文终端通过，脚本/HTTP 错误均为空。
最终使用最新 Smoke.app 的单轮完整流程全部通过，报告 `/tmp/agentbox-server-compat-full.json`。
浏览器驱动先后修正子元素点击、Playwright 对 sandbox iframe 的 service-worker 注入及 CDP
帧通知等待；保留真实脚本错误、HTTP、WebSocket 和无 service-worker 断言，没有屏蔽产品错误。

图片剪贴板改为原生读取 PNG/TIFF（Mac）及 PNG/DIB（Windows），同一字节快照先预检尺寸再解码。
输入 ≤128 MiB、像素 ≤32 Mpx、单边 ≤32768、解码像素 ≤128 MiB、输出 PNG ≤19 MiB。
修复 image 0.25.10 对 packed DIBV4/V5 bitfields 的 12 字节偏移问题，使用借用源数据的虚拟
BMP 头给出明确像素位置；手写标准样本覆盖 alpha/行序。BigTIFF 拒绝；库分配额度不是总 RSS
硬限制，Mac 系统提供 NSData 前的物化开销不能由此控制。Windows 新模块及测试已用真正
`x86_64-pc-windows-msvc` target 类型检查通过，未运行 Windows 程序。

安装脚本支持可选真实 `--previous` 包，旧 sidecar 建立隔离设备/绑定/基线/完成历史，新包必须
版本更高并保留记录与项目字节。候选工作流新增 `previous_tag`，固定来源/平台/精确版本和
SHA256 检查，保存来源与探针报告；缺少旧包明确跳过升级。Windows 仅允许托管临时 CI 账号
执行 NSIS，避免 /D 临时路径仍改写本机注册表。6 项探针回归（包含真实 sidecar 和合成 schema
4→5 迁移）通过；未执行真实旧安装包升级、GUI 设置/keyring 保留或 Tauri 更新安装。

最终校验：前端 24 项测试、类型检查/普通和测试构建，Rust 31 项测试、smoke all-targets Clippy/fmt，
许可证 450 项收集，发布清单/候选配置测试及工作流解析通过。Linux 兼容 CI 已接入真实旧网页
回归，未触发远程运行。最新 Mac ARM64 双独立终端/附件/目录上传原生 smoke 通过。
正常 Agentbox.app 已重建（65.88 MiB，ad-hoc、未公证），中文/空格路径、仅系统 PATH、首次与
重复启动安装探针通过；没有实际旧包，升级部分明确 skipped。普通 dist 已恢复，不含 smoke 模块。
Windows/Intel 实机、真实 Finder/Explorer/IME/休眠、正式旧包升级/中断/keyring 与签名公证仍未验收；
sync=0 保持，未提交/推送/部署/发布。本轮临时 Docker 容器与卷已清理，没有改动生产数据。

## 2026-10-04：服务器恢复内容与活动日志名额回收

新增 `GET /sync/storage` 和明确确认的 `POST /sync/operations/{id}/retire`，能力为
`sync_recovery_gc=1`，正常服务端仍 `sync=0`。桌面恢复历史显示服务器活动操作、恢复字节、永久
执行收据与逻辑元数据；按已完成批次预览路径、操作及旧内容容量，二次确认后逐项清理。
同实例、空间属主、原设备、原意图摘要及确认都必须匹配；不向替代实例发送旧 ID，旧能力不请求新接口。
空间中任何活动同步租约会阻止清理，unknown/pending/replan/abandoned 不可回收。
经完整核对并 finish 的 started 可凭持久化核验摘要清理，原 started 审计不被改写。

服务器先持久化 retiring，再核验并删除 before、同步目录，最后写 retired；永久保留原 ID 目录及
原 applied 收据、意图摘要，回退旧执行逻辑仍只返回历史结果，不会重新写工作区。回收释放旧内容和
活动操作名额，不删除永久审计元数据。配额为活动 1000、旧内容 256 MiB、永久收据 100000（retiring
预占）；最后一项满后拒绝新的回收，活动配额随后满会停止新增操作。按空间/根目录 inode 缓存容量，
日常增量维护，可能修改磁盘的失败失效；冷启动分批有界扫描，60s 超时，慢盘可能无法一轮完成。

本地状态升级 schema 5，保留 retiring/retired 意图与审计，schema 1–4 可迁移，旧 sidecar 拒绝新库。
网络动作前 FULL SQLite 落意图，响应丢失/取消后需重新加载 revision 并预览续做；不自动重试。
当前文件、同步基线、本地副本保留。已清理远端内容不能用于待定批次 finish 或恢复导出。无副本历史
也须等其所有远端操作退休后才可删除，避免先丢失回收入口；有原内容引用的审计行始终保留。
本地历史缺失的孤立远端日志没有纳入本轮清理，不能批量猜测其完成状态。

已验证：全仓 Go test/build/vet；服务端/协议定向 race（15.174s/1.662s）、客户端端到端清理 race，
包括错身份/旧能力/丢响应/SQLite重启/finish审计/六个持久化中断点/冷热容量一致/旧apply字段重放隔离。
六个中断点为代码注入故障，不是物理断电。Linux 隔离容器实际运行服务端和客户端清理用例通过；
Mac 原生 sidecar→Linux HTTP peer（每条命令重新启动 sidecar）完成双向同步、删除恢复、批次清理、
永久收据计数和基线不变验证。Windows syncclient 测试二进制交叉编译通过，尚未在 Windows 执行。
前端 24 项 Vitest、类型检查/构建、Rust 21 项测试及 smoke all-targets Clippy 通过。
Mac ARM64 原生同步 UI + race 已通过（19.94s），实际点击远端预览/永久确认并验证服务器 before
删除及原 ID 收据保留，随后本地副本、重绑、持续同步、冲突、取消和退出登录回归通过。首次运行完成
清理后被测试动作白名单遗漏拦截，补齐仅 smoke 命令后重跑成功；这不是产品接口故障。
正常 Mac ARM64 Agentbox.app 已以普通前端与最新 sidecar 重建（66.85 MiB，ad-hoc，未公证），
中文/空格路径、系统 PATH、首次与重复启动安装探针通过；dist 已恢复普通前端，不含 smoke 模块。
未提交、推送、部署或发布。完整旧版本兼容、Windows/Intel 实机、真实输入与安装升级/签名公证仍待验收。

## 2026-10-04：本地恢复副本清理、更新验签与下载故障回归

新增逐文件 `sync_recovery_discard`，由历史面板确认具体副本和 revision，经 Rust IPC 到 Go。
只有完整 verified 且非 replan/abandoned 的历史可清理，绑定 pending 时拒绝；不处理服务器副本。
先在 FULL SQLite 事务写 discarding，再校验原目录身份、固定路径、普通单链接文件、哈希和大小，
删除并同步恢复目录后写 discarded。清理途中取消/退出或末笔事务失败后保留意图，重新加载后可续做；
已缺失副本幂等完成。当前项目文件、基线、批次和原引用保留，历史明确标注原内容已清理。
本地 schema 1/2/3 迁移到 4，旧 sidecar 拒绝打开；服务端 schema 10 和 sync=0 不变。
远端恢复内容与幂等日志的安全回收仍待实现，不能把本次本地清理视为 P3 容量管理全部完成。

更新器下载 URL 收紧到清单版本对应的精确 desktop-v 标签，重新检查前清空上次候选；增加总下载
超时及完整返回后的容量复核，避免短响应抢在超限信号前完成。新增 mock Tauri runtime + 真实
loopback HTTP/锁定插件 minisign 测试，验证合法签名、内容篡改、签名缺少版本、声明版本不符、
截断、超限、停滞超时，以及失败后完整重试。仅保留合成公钥/载荷/签名，临时私钥已删除；未运行
安装器或访问 keyring，不代表已安装应用升级验收。

验证：全仓 Go test/build/vet 通过；清理边界和状态迁移定向 race 通过；Rust 21 项测试通过；
普通及 smoke all-targets Clippy 均通过；前端 typecheck/build 和 24 项 Vitest 通过；发布清单 2 项、候选配置 3 项测试及 450 项许可证收集通过。
新的 Mac ARM64 原生同步 UI + race 场景已通过（21.55s），包含导出、确认清理及审计行，继续覆盖
跨空间后台同步/冲突/异常批次/取消/退出登录。Linux 隔离容器实际执行新的文件系统清理测试通过，
Windows syncclient 测试二进制交叉编译通过；后者不等于 Windows 执行验收。

终端原生重跑先前卡在 WebView visibility gate：两个独立终端连接、输入和 buffer 回显成功，
WebView 未报告 visible，附件/目录上传检查尚未开始。现在将该门槛单独报告，保留渲染验收要求；
不能把同步 UI 通过用于抵消终端可见渲染失败。最新 projects 和 legacy 原生重跑均已通过：附件/目录上传、终端输入/resize、两个独立终端（projects）与旧能力降级（legacy）均成功。可见状态的间歇性失败原因仍未完全定位，不宣称已消除。

正常 Mac ARM64 Agentbox.app 已用最新 Go sidecar 和普通前端重建（66.76 MiB，ad-hoc，未公证）。
中文/空格安装路径、仅系统 PATH、首次及重复启动的安装探针通过，未测试真实 GUI/IME/升级。
普通前端 dist 已恢复，不包含 smoke 模块；未提交、推送、部署或发布。

本机本轮编译遇到真实宿主磁盘满（剩余 295 MiB）。使用 cargo clean 仅回收本项目可重建产物，
工具报告移除 10.1 GiB；未清理 Docker 镜像/卷或源码/用户数据，Docker 保持运行。随后编译恢复，验收结束前可用空间约 5.6 GiB；保留约 3.8 GiB 其余 Cargo 依赖产物。

## 2026-10-03：目录手动上传、跨空间监督与实际磁盘满

新增原生 `preview_file_upload/apply_file_upload/discard_file_upload`：只使用原生选中文件的
单次 ticket，读取固定字节快照，确认摘要包含文件内容/目标空间/范围/目录及目标列表；确认前重读列表，
变化即拒绝。预览标明同名覆盖、压缩包解压合并、取消或失败可能部分完成。旧上传接口无 CAS，不承诺消除
最后检查到发布间的外部编辑，也不声称产生同步恢复副本；从不传 clear。上传、同步和更新使用原生互斥。
同时修复文件浏览切换范围失败时不能把上一范围的旧列表留在新标题下。

持续同步的组件现在在跨空间切换时保留，原生任务通过前端共享 FIFO 调度，队列最多 64 项，取消排队任务
不会调用另一个项目的 sync_cancel。退出登录卸载全部监督器，启动时不自动开启。侧栏按空间显示状态。

新增 Linux 专属真实 ENOSPC 测试，只允许明确指定的 ≤8 MiB tmpfs。已在隔离的 4 MiB Linux Docker
内跑到真实 ENOSPC，覆盖写入失败保留原文件、恢复副本无法落盘时拒绝删除、释放空间后重试保留旧内容。
此用例进入 desktop CI 独立 Linux job，不触碰宿主项目或正常临时卷；不等于所有平台的断电/整引擎磁盘满验收。

验证进行中：Rust 17 项测试通过，含目录路由/原始字节/无 clear 请求和预览列表变化；前端 24 项 Vitest
通过，包含共享调度和排队取消。Mac ARM64 projects smoke 已通过两个终端、附件和目录上传 UI→native→HTTP，
系统 picker 的测试返回值来自仅 smoke feature 的临时文件。跨空间后台同步新原生场景 + race 已通过（17.16s）：开启后切到第二空间，再修改第一空间的文件，
仍完成传输；切回后冲突暂停、核对、归档、取消和退出登录继续通过。两份原生测试不能并行启动同一个
单实例 Smoke.app；一次并发运行缺报告/隐藏窗口超时已记录，随后同步场景串行重跑通过，终端场景重跑中。

## 2026-10-03：P4 文件能力与 P5 独立候选流程（验收进行中）

原生附件从 picker、系统文件剪贴板、图片剪贴板、窗口 Drop 获取，使用短时单次 ticket 绑定登录，
不提供 renderer 绝对路径读取。支持清单确认上传、逐文件结果、明确插入终端路径、取消与字节读取进度；
服务器仍复用原 images API。已上传部分保留可见结果，晚到/取消结果不进入新任务。附件限 19 MiB，
选择最多 32 个/64 MiB；PNG 转换由跨平台 clipboard-rs 完成。真实 Finder/Explorer 与系统截图验收待跑。

旧接口文件浏览、工作空间/共享范围和原生保存下载已接入；下载完整接收后 no-clobber 发布，限 64 MiB。
终端增加多行暂存输入、组合输入期间延迟 resize/字号变更、休眠后同终端重连；不重放按键或绕过策略关闭。

新增桌面专用更新器和三架构 `desktop-release.yml`。公钥编译时注入，固定 desktop-stable 清单，
下载 URL 限本仓库 desktop-v 标签且要求签名绑定版本；开发包默认更新不可用。候选流水线有签名凭证
前置检查、Mac codesign/Gatekeeper/stapler、Windows Authenticode 和含中文/空格安装路径探针，
仅上传 artifacts。清单装配器输出 draft/make_latest=false API 请求体，不执行发布。

Rust 新依赖许可证收集覆盖三目标图及生产 npm/Go 文本；crate 缺少随包文本时补固定源码来源和哈希。
本地已经通过首轮 14 项 Rust 测试、前端类型检查及新增附件队列测试；Mac ARM 原生 legacy smoke
确认附件 UI→原生上传→服务器响应，启动插件配置和隐藏窗口渲染问题已修复。最终回归仍在进行，
本条不是 P3/P4/P5 全部完成声明。

后续验证：最新 Mac ARM64 原生 projects smoke 通过，断言两个独立终端和附件上传字节；完整 P3
原生同步/恢复 smoke + race 通过（13.49s），真实 Docker PTY/tmux race 通过（2.42s）。新增
`TestClientLinuxPeer` 仅存在于测试二进制，Mac 原生 sidecar 与只读根、临时 Linux 容器通过真实
HTTP 完成 CRLF/中文/BOM/NUL、重启后基线、删除及远端原内容导出。初次容器缺 tzdata 启动失败，
已将时区数据仅嵌入测试 peer 后重跑通过。该场景不使用生产配置/凭证或宿主项目挂载，不等同完整故障矩阵。

最终 Rust 14 项 lib tests、普通和 smoke Clippy、前端 22 项 Vitest 已通过。全仓 Go test/build/vet 与原 Go 第三方库存检查也通过；最新正常 Agentbox.app（66.46 MiB、ad-hoc、未公证）已重建，其中文安装路径/系统 PATH/重复启动探针通过。Mac 中文路径、仅系统 PATH 的安装探针此前在新 Smoke.app 通过；
它不代表真实系统文件选择、剪贴板、IME 或更新安装验收。

实际外部状态：GitHub 仓库没有桌面签名 secrets、更新公钥 variable 或自托管 runner。Windows/Intel、
Linux 联调、真实剪贴板/IME/高 DPI、更新中断、最低系统测试与正式签名都尚未取得证据。sync=0 不变。

范围审计仍有软件补项：当前持续同步只在选中空间页面内运行，未实现跨空间长期监督；恢复容量只允许清理
无副本完成记录，尚未实现本地/远端实际恢复副本和服务器幂等日志的安全回收。手动上传目前走共享附件，
项目目录上传/覆盖预览仍需补齐。更新验签错误/中断和服务器冻结版本兼容矩阵也未通过完整验证，不能将
前文“P3 软件面收完”视为全计划完成。

## 2026-10-03：P3 历史容量、逐文件冲突、持续同步与未知批次

恢复历史现在使用绑定修订号固定游标分页，每页最多 20 个批次和 50 个文件，pending 固定排在第一页；
单页响应不会把 4 MiB 私有 IPC 上限或 renderer 列表撑爆。响应提供绑定/逻辑元数据容量和可访问本地恢复目录
占用；不可访问目录明确显示未知。清理只允许用户确认没有恢复副本的已完成历史行，保留基线、服务器收据和
实际文件；pending、replan、abandoned 和含恢复引用的批次不可清理。

自动同步计划支持逐文件选择本地或服务器版本，选择绑定原预览摘要，应用前完整重扫；旧目录/服务器变化会
拒绝选择或执行。新增当前工作空间内默认关闭的持续同步，活跃轮次约 1 秒、无变化指数退避到 30 秒，冲突、
首次确认、大批量删除、网络/容量错误和 pending 会暂停且不自动重试。

原目录或旧实例无法访问且存在 pending 时，新增“归档未核验批次”流程。用户需查看摘要并输入固定确认语句；
服务端仍可访问时先取得租约。结果写为 `abandoned`/未知，保留操作和恢复引用，不提交基线、不向替换实例
发送旧操作 ID；新绑定从空基线重新确认。

验证：Go syncclient/syncfs/server 定向 race 用例、全仓 Go tests/build/vet、Vue typecheck/build 与 16 项
Vitest、Rust fmt/lib tests 通过。新 Mac ARM64 Smoke.app 已通过历史翻页/容量展示、逐文件选择、持续同步、
未知结果归档和旧恢复/取消/退出登录回归；Smoke.app 仍是 ad-hoc 测试包，未公证。Windows、Mac Intel、
真实 picker、跨 Linux 服务端故障和正式安装发布仍待实机验收，服务端 `sync=0` 未改变。

## 2026-10-03：P3 绑定归档、重绑与身份变化处理

新增 `sync_archive`，使用界面显示的绑定 ID/修订号确认解绑。解绑只在本地事务内设置 archived，
保留旧身份、目录、基线与全部恢复记录，不修改项目文件；pending 一律拒绝。执行器与 StateStore
都禁止归档绑定开始新批次。注册新绑定时忽略已归档映射的写入排他限制，新绑定生成独立 ID、
不继承基线，必须重新预览和确认；两端不同内容重新出现首次来源冲突。

桌面选择器可查看已归档、已删除项目和服务器身份已变化的历史绑定。相同规范化地址/认证用户
允许发现本机旧身份记录，但旧实例的远端恢复请求不发送到新实例。本地副本仍需目录身份与哈希
验证。相同地址换身份也不能绕过未归档项目映射；旧 pending 不能靠换身份或选择新目录跳过。
目录移动/缺失但没有 pending 时可归档，然后用原生 picker 选择新目录；忽略规则变化也可通过
归档后新绑定重新确认范围，不会从旧规则推导删除。规则两端仍须一致。

本地 SQLite 升到 schema 2（服务端仍 schema 10）。迁移保留设备/已有绑定，旧 sidecar 再次
打开 schema 2 会拒绝，防止它忽略 archived 字段继续写入。归档仍计入 1024 绑定和 256 MiB
元数据上限，不自动清理。恢复导出目标继续排除历史绑定目录，避免覆盖历史现场。

验证：新增 6 项服务端生命周期用例和本地迁移/重启用例的 race 通过，覆盖修订过期、pending
拒绝、活动重叠、归档后恢复导出、独立新基线、目录移动、规则变化、服务器换身份、跨用户拒绝。
全仓 Go test/build/vet、许可证检查、Rust 8 项测试、普通/测试构建 Clippy 和前端 9 项 Vitest 通过。Windows x64 /
Mac Intel 的 syncclient 测试程序交叉编译通过，未原生运行。

新版 Mac ARM64 Smoke.app 原生同步/恢复验收通过（Go 测试进程带 race）：pending 时解绑按钮
禁用，确认解绑后不能预览，归档副本仍可导出；重新绑定并确认后，SQLite 同时保留旧归档与新
活动绑定。此前进度、过期确认、finish/replan、取消及退出登录均保持通过。测试包 ad-hoc、
未公证，仅测试 peer 开启 sync。真实 picker、Windows/Intel 与跨 Linux 服务端仍待实跑。

剩余边界：旧实例/本地目录丢失且存在 pending 时，本轮继续拒绝解绑，需恢复原环境后核对，
尚未提供“放弃不可核对批次”的独立流程；目录移动后旧本地副本不自动重新定位。正常前端产物
已恢复为无测试入口的构建。恢复记录分页/
容量管理、连续同步、冲突逐项处理、文件粘贴和正式发布仍未完成。sync=0 未改变，未部署、提交或发布。

## 2026-10-03：P3 有界同步进度与任务隔离

Go 执行器新增可选进度观察，报告认证、本地/远端扫描、规划、租约、执行、核对、提交和
恢复导出阶段，以及本地已哈希文件数/字节数、当前相对路径、单文件/批次读取字节和已核验
操作数。字节只表示读取传输内容，不表示服务器已发布；只有逐项持久化 verified 后增加
核验数，完整扫描和基线提交仍决定最终命令结果。恢复副本复制、网络协议开销不计入传输字节。

私有 stdio 新增可选 progress=true 与 sync_progress 事件；旧请求仍只返回最终结果，
服务端 HTTP 协议无变化。Go 保存一个最新快照，每 100ms 最多发送一次，结果之前可再刷一次；
生产者不等待管道。Rust 校验阶段/相对路径/计数/序号，15 分钟总截止不会因进度刷新，
最多接收 10000 条事件。原生 Channel 同时只保留一条未确认事件和一个最新快照，renderer
按原生任务 ID/序号 ACK；未接收进度不会阻止文件操作。

Vue 显示当前阶段、扫描计数、文件路径、单文件和批次字节、已核验操作条。独立任务对象
忽略乱序、重复、取消后和卸载后的事件，最终结果不从进度推断；退出登录关闭原生进度转发。
同时阻止组件在卸载后才完成项目查询时继续发起 sync_list。

验证：全仓 Go test/build/vet、许可证检查通过；新增真实 HTTP 成功/丢响应进度测试和
快照限流测试的 race 通过。Rust 8 项测试、Clippy、Vue 构建与 9 项 Vitest 通过；Rust 调用
新编译 sidecar 接收真实进度并继续解析最终结果。Windows x64/macOS Intel 的 syncclient
测试程序交叉编译通过，未实机运行。

新版 Mac ARM64 Smoke.app（ad-hoc、未公证）原生同步/恢复测试和 Go 测试进程的 race 通过。
测试服务将条件下载分段发送，WebView 在命令完成前确认字节进度介于 0 和总量之间；阻塞
清单时显示远端扫描，取消后进度消失，完整 finish/replan/导出/退出登录回归仍通过。
测试仍使用原生配置注入临时目录，不代表系统 picker、Windows、Intel 或 Mac↔Linux 验收。
正常前端产物已恢复；sync=0 未改变，无生产部署、提交或公开发布。

下一阶段：绑定生命周期（解绑/重绑、目录或实例身份变化后的重新确认）、恢复历史分页与容量
管理，以及 Windows↔Linux 实际联调。连续同步、逐项冲突解决、P4 文件粘贴和 P5 发布仍未完成。

## 2026-10-03：P3 同步与恢复的原生 WebView 验收

新增 opt-in `TestDesktopSyncNativeSmoke`，由 Go 测试启动真实服务端 HTTP 路由和显式
Smoke.app，Vue 点击实际控件，经 Tauri 原生命令和随包 Go sidecar 完成绑定、预览、执行、
核对和导出。只有测试 peer 将能力改为 sync=1，正常服务端仍为 0。

Mac ARM64 实跑及服务端测试进程的 race 检查通过，覆盖：

- 初次双向同步保留 CRLF、中文与二进制字节；预览之后改文件，旧确认被拒绝。
- 服务端已完成写入但返回 503 时保留 pending；核对后文件变化，旧核对摘要拒绝提交。
  重新核对后 finish 只提交基线，不再次发送写入。
- 部分完成的批次不能 finish，显式 replan 保留旧基线；下一轮只传输剩余文件。
- 本地和远端的覆盖前内容均能导出；项目目录和已有同名目标均拒绝。
- 取消、退出登录分别中断一个已到服务端的阻塞清单请求；退出后同步组件消失。

测试最后读取实际双端字节、独立导出目录及本地 SQLite 历史，确认两份原内容和 finish/replan
归档存在、pending 清除，并断言四次远端写入请求，没有用界面成功提示替代文件验证。
`desktop-smoke` feature 内的原生测试配置仅接受本机回环 URL，目录来自 Go 测试的临时目录；
renderer 只触发固定场景名。正常构建没有此模块/命令/目录注入，仍使用系统 picker。
此测试替代了选择器的目录返回值，**不代表系统选择器、IME、安装包或 Mac↔Linux 验收**。

全仓 Go test/build/vet、许可证检查、Rust 6 项测试、普通/测试 feature 的 Clippy、Vue 构建与
现有 6 项终端 Vitest 通过。旧服务端原生终端及项目双独立终端 smoke 均通过。同步和恢复使用新构建的 Mac ARM64
Smoke.app（ad-hoc、未公证）；正常前端产物已恢复。此前终端 smoke 超时的根因仍未证明，
本轮成功不消除历史记录。

桌面 CI 已加入 macOS ARM64/Intel 的原生同步用例及相关服务端源码触发路径，本轮没有运行
远端 CI。Windows 继续保留原生构建、便携 Go 测试与终端 smoke；此用例依赖 Unix 服务端测试包，
不能直接在 Windows runner 执行。Windows↔Linux 完整同步、真实 picker、详细进度、绑定生命周期、
连续同步及发布验收仍待完成。未生产部署、提交或公开发布。

## 2026-10-03：P3 待定批次核对与恢复副本导出

新增 Engine.ReviewPending/ResolvePending：重新获取租约、核对双端完整清单和远端收据，
确认摘要包含批次/本地修订/项目修订/清单摘要及逐项状态。确认时重新核对，旧摘要拒绝。
finish 仅在全树等于计划且所有远端写入均有 applied 收据时提交基线；uncertain 不能自动
转成功。replan 保留当前文件和旧基线，归档旧批次后允许重新预览，不掩盖后续双边编辑冲突。
归档与清除 pending/提交基线同事务，保留原操作状态及 resolution 摘要，不自动重放。

新增 RecoveryHistory/ExportRecovery，支持当前和归档批次的原内容候选；按批次/操作 ID
验证源路径及 hash/size。导出目录由原生 picker 提供，禁止与应用状态及任何绑定树重叠，
只新建不覆盖同名文件。服务器恢复引用下载时确认是否存在；本地恢复目录身份和文件哈希
变化均拒绝。项目映射已移除时仍可从桌面选择残留绑定查看恢复记录。

Tauri 私有协议新增 sync_review/resolve/history/export；Vue 提供逐项核对结果、两步确认、
批次历史和单文件导出。全量基线/旧字节不交给 renderer，凭证/目录选择沿用原生边界。
修复 sidecar 在返回响应后才清 busy 的小窗口，顺序发送下一命令不再偶发 sync_busy。
服务端 capability 保持 sync=0，完整集成与平台验收目标尚未完成。

验证：全仓 Go test/build/vet、相关 race、许可证检查通过；真实回环 HTTP 覆盖丢响应核对
无重放、部分批次不提交、旧摘要拒绝、活跃租约拒绝、收据错误/uncertain、replan 保留旧基线
及后续冲突、双端原内容导出、坏副本/跨用户/目标已存在拒绝，以及真实私有管道连续
review→resolve→history→export。Linux ARM64 隔离容器实跑这些用例通过；Windows x64、
macOS Intel 测试程序交叉编译通过，未实机运行。Rust 6 项测试、Clippy、Vue 构建与现有
6 项终端 Vitest 通过。新增恢复界面的完整原生交互仍待验收。

本轮重新打包 Mac ARM64 Smoke.app（ad-hoc、未公证）。原生 smoke 初轮出现 60 秒无报告
超时，阶段记录定位到空间选择/终端连接附近；增强测试专用阶段跟踪、Vue/WebView 异常捕获、
主动聚焦、真实时间截止与有界渲染等待。随后 legacy/projects 两种 smoke 均返回 ok=true（项目模式验证两路独立终端输入）；先前超时原因未
完全证明，不能抹去这项不稳定记录。原生恢复交互和 Windows 实机仍未验收。正常前端产物
已恢复为无 smoke 入口的构建；未生产部署、提交或公开发布。


## 2026-10-03：P3 私有 IPC 与桌面同步界面初版

新增 sync_bind/list/preview/apply 私有管道命令，命令/响应上限 4 MiB，超限不输出部分 JSON。
Rust 提供登录凭证、认证用户和专用 app_data_dir，本地路径来自系统 picker；前端不能指定
令牌/状态目录/本地路径。Go 重查认证身份和 sync capability，bind 核对远端项目目录。绑定
只返回摘要，重新启动 sidecar 后仍能列出已保存的绑定。正常服务端仍 sync=0，不开放入口。

原生层同一时刻只运行一个同步请求，取消/退出登录触发管道 EOF 并等待进程收尾；Go 使用
父生命周期 context 取消网络和执行器。界面包含项目选择、持久化绑定、具体变更与冲突列表、
双向/强制方向预览、精确摘要确认及取消。pending 暂停并显示待核对，不提供自动重试。
恢复处理、解绑重绑、逐项冲突解决和详细进度仍未完成，不能据此开启 sync=1。

验证：Go parent EOF/shutdown 中断在途 HTTP、身份不匹配不访问本地状态；Rust 调用实际编译
sidecar 完成绑定并重启 worker 后重新列出绑定（令牌只走请求头/私有管道）。Rust 6 项测试和
Clippy、Vue 类型检查/构建与现有 6 项终端 Vitest 通过。Linux ARM64 隔离容器实跑
TestSyncEngine 全通过（root 以支持服务端严格 chown）；本地文件和服务端都在该容器合成目录，
不是 Mac↔Linux 网络互通测试。Windows syncfs 测试交叉编译通过，未原生运行。

本地发布前还增加忽略规则复查，测试确认下载期间规则变动不覆盖新忽略文件。已补齐旧轮次
许可证清单并验证通过。尚未验收新增同步界面的真实 WebView 交互；桌面与生产未发布。

## 2026-10-03：P3 原生整项目执行器

新增 `syncclient.Engine`，把完整本地/远端清单、三方规划、server_id/user 绑定、项目修订、
租约和 `StateStore` 串成一次明确的预览执行流程：执行前重新扫描并重建 digest，批次先持久化，
目录按父先子后创建、文件传输、文件删除、目录按子先父后删除；每个操作按
`prepared → started → verified` 记录，最终两端完整重扫符合计划后才提交基线。

本地 Writer 新增租约守卫、恢复副本持久化回调、空目录 mkdir/rmdir 和项目根恢复区。恢复副本
不再放在待删除目录内，避免删除空目录时被自身备份挡住；Windows 使用限定类型的原生句柄
删除，不能把竞态文件当目录或把目录当文件删除。取消、租约失效、条件不匹配、丢失响应和
冲突都保留旧基线及 pending，不自动重放。

新增回环 HTTP 执行器测试覆盖双向中文/二进制文件、目录创建删除、恢复、规则变化、冲突、
旧预览、未开启 sync、取消、租约丢失和服务端已发布但响应丢失；Go 全仓 test/build/vet、
相关 race 和许可证校验通过。Windows x64/macOS Intel syncclient 交叉编译通过；Linux
隔离容器实跑底层状态与服务端身份测试通过。执行器尚未接入 Tauri IPC/UI，服务端 capability
继续为 `sync=0`，未生产部署。

## 2026-10-03：P3 安装身份与本地持久化

新增安装身份 `data/client-instance-id`，能力接口返回 server_id 和认证 user；同步 API 校验
实例请求头，Go Remote 可固定服务器与用户。正常重启保持身份，系统/完整备份恢复后生成
新身份，要求客户端重新确认基线；损坏身份不自动修复，原网页 API 不受影响，schema 仍为 10。

新增原生应用专用目录的 StateStore（本地 SQLite schema 1、FULL 事务），保存设备、绑定、
基线、pending 与完成批次，不保存令牌或文件内容。目录及祖先实际 ID 防止重叠映射；Begin
重建计划并校验确认摘要，操作先落盘 started，核对后 verified，完整重扫符合预期才同事务
提交基线与归档。崩溃后的 started 不自动重放，保留旧基线和操作 ID 等待核对。

验证：Go 全仓 build/test/vet、相关 race 通过；身份并发创建/损坏/链接、两类备份恢复新
身份、跨用户及换服务器拒绝通过。本地状态测试覆盖并发 Begin、过期预览、重叠目录、容量、
Windows 可执行位模型；真实子进程退出验证 started 保留及未提交事务回滚。Linux ARM64
隔离容器实跑全部 TestState 和 TestClientIdentity 通过。Windows x64/macOS Intel 测试程序
交叉编译通过，尚未原生运行；未接入执行器/IPC/UI，sync 仍为 0，未重建安装包或部署。

本轮收尾曾因系统拒绝访问文稿目录中断，恢复访问后补记本记录，并运行许可证收集脚本，
补齐桌面 SQLite 新链接的已有依赖 go-isatty、go-strftime；无新增依赖版本。
完整集成目标仍在进行，下一步为本地目录原语、恢复区调整与整项目执行器。

## 2026-10-03：P3 条件写入、远端目录与恢复日志

本轮新增 `POST /sync/apply`、操作状态 GET 和旧文件下载 GET；Go Remote 接入 Apply、
Operation、Recovery。apply 支持 replace/delete/mkdir/rmdir，before/after 限定目标类型与
hash/size/executable，元数据及租约令牌走请求头，原始文件体不改换行/编码。上传前检查租约，
接收不占空间锁，接收后与发布前重新检查租约/规则/修订/目录身份/预期目标。
新文件/目录不覆盖发布，rmdir 只删空目录，使用 AT_REMOVEDIR 防止竞态退化为 unlink。
Linux 新文件/目录严格 chown 1000:1000；Mac 非 root 开发检查允许 chown 不成功。

日志位于会话 `client-sync/<operationID>`（容器挂载之外），保存意图与覆盖/删除前原始字节，
不保存登录/租约令牌。记录及恢复副本 fsync 后才修改目标，落盘与结果复核后记录 applied。
applied 的重复请求只返回历史收据，不再操作当前文件；不同意图复用 ID 拒绝。
uncertain 不自动重放，需要查询状态/重扫；故障后不把整批假定成功。
状态和恢复内容仍按空间属主校验，不要求继续持有租约或项目映射；恢复只下载，不自动写回。
每空间 1000 条操作/256 MiB 旧内容，到限停写，暂不自动清理；日志进入全量备份，默认系统
备份不含这些工作区恢复数据。API/边界与中英文 README 已更新。

验证证据：

- Go 全仓 build/test/vet、相关 server/syncclient/syncproto/safefs race、许可证校验通过；
  文件安全回归覆盖 safefs、archivex、agent、credentials、workspace，并保留原业务测试。
- 真实回环 HTTP 创建/覆盖/删除、Go 客户端状态查询与恢复下载通过；失效租约、上传期间失去
  租约、错误 hash/长度、过期目标、规则/修订、链接/硬链接、跨用户、query 写令牌均拒绝。
- 重复写请求不覆盖后来的编辑，重复 delete/rmdir 不删除后来重建的文件/目录；同操作 ID 不同
  意图拒绝。测试重建租约注册表模拟重启后旧租约失效、新租约仍可读取磁盘收据；这不是进程
  强杀或真实服务重启验收。
- 覆盖前取消、发布后取消、并发编辑均保留可核对结果；uncertain 重试不重执行，恢复数据
  可取回。恢复容量及操作数量满额时停止修改；非空目录不递归删，目录换成文件的竞态不误删。
- 完整备份/恢复测试新增 uncertain 记录和 before 原始字节，确认内容及 0600 权限保留。
- Linux ARM64 隔离容器（network=none、只读根、仅挂测试二进制、合成 tmpfs 数据）实跑全部
  `TestSyncMutation*` 通过；另以 root 运行目录用例，确认实际 chown 后 UID/GID=1000/1000。
- Windows x64/macOS Intel syncclient 测试程序交叉编译通过，未实际运行这些平台；未重建桌面
  安装包（尚无新增 IPC/UI 入口），未生产部署、提交或公开发布。

剩余：稳定服务器身份、本地绑定/基线持久化、两端日志核对与整项目执行器、本地目录增删、
映射排他、续租/取消/崩溃收尾、恢复清理策略、冲突/恢复/强制预览 UI、真实跨平台验收。
租约仍不锁住外部 CLI/IDE，最后条件检查与 rename/unlink 之间仍有竞态，不宣称文件事务；
磁盘满、断电和完整进程崩溃验收未完成。`sync` capability 保持 0，完整集成目标继续进行。

## 2026-10-03：P3 条件下载与 Go 原生网络层

新增 `GET /api/sessions/{id}/sync/file`，要求项目 revision、忽略规则 hash 和预览中的
文件 hash/size/executable。读取复用 safefs 与空间生命周期锁，禁止穿越、链接/硬链接和
忽略项；完整验证后才发送内存快照，发送不持有空间锁。与清单共用两个全局读取槽，
单文件 64 MiB，下载内容缓冲合计上限 128 MiB，发送设置 60 秒写截止时间。
忽略规则文件也改为有界、核对 inode/属性/链接数的读取，不能借规则文件绕过链接检查。

`syncclient.Remote` 新增清单、租约获取/续租/释放和条件下载。保留 TLS 验证、禁重定向，
URL 不含凭证，Bearer/lease 只放请求头；清单验证结构与 digest，下载在 EOF 校验大小与
SHA-256。响应错误不转为空目录，不将原始 URL/响应正文返回 UI，不增加应用层自动重试。
这是可供执行器调用的底层模块，未增加 IPC/UI 入口、后台循环或开放 `sync=1`。

验证：

- macOS ARM64 真实回环 HTTP → 服务端条件下载 → 本地 Writer：中文/特殊字符文件名、
  BOM/CRLF/二进制、空文件、覆盖前恢复副本通过；同大小/mtime 修改仍返回 409。
- 损坏哈希、截断响应、错误类型/长度/ETag/编码、取消均不覆盖原本地文件、不遗留暂存；
  跨用户、失效租约、项目/规则修订变化、忽略项、符号链接与硬链接均拒绝。
- 发送响应头时改写源文件，响应仍为已验证快照；不跟随 HTTP 重定向、不接受自签名 TLS；
  清单非法或超限不返回可用空树。相关 race 测试通过。
- Go 全仓 build/test/vet 通过；追加规则文件加固后重跑 server/safefs/archivex/agent/
  credentials/workspace 回归、server race、全仓 build/vet 通过，许可证清单校验通过。
- Linux ARM64 隔离容器（network=none、只读根、仅挂载测试二进制、合成临时文件）实跑
  `TestSync*` 全通过。首轮 fixture 因精简镜像缺少时区库而初始化失败；使用 Go 标准
  `timetzdata` 构建标签嵌入时区后通过，未改变业务代码或测试断言来跳过失败。
- Windows x64/macOS Intel 的 syncclient 测试程序交叉编译通过；尚无这些平台的原生运行
  证据。未重建桌面安装包（本轮未改变桌面入口）、未生产部署、提交或公开发布。

下一步仍是服务端条件上传/删除、远端恢复与幂等日志，以及持久化绑定/基线和执行器；
整项目同步、跨平台安装验收与完整 P0–P5 集成目标仍未完成。

## 2026-10-03：P2 项目与独立终端增量实现

新增服务端能力、独立配对、项目 CRUD、终端资源 CRUD/WS，桌面按 capability 自动选择基础或
项目模式。项目支持 `.` 和已有子目录、名称修改、启动参数快照和修订检查；不移动旧目录，
不改变空间 Agent/账号复用或代理规则。接口详见 [desktop-api.md](../desktop-api.md)。

- SQLite schema 10 新增 client_projects/client_terminals。原 session 行不变，空间删除时
  同事务清理元数据；迁移到 10 后回退必须恢复 schema 9 兼容备份。
- 独立终端用稳定 ID 的独立 tmux socket；旧 `/term` 仍是默认 socket/main。准备进程经
  ExecCommandEnv 注入账号环境，等待 tmux 创建后才释放空间锁，再附加 PTY。关闭先持久化
  closing，阻止迟到连接/输入，失败保留记录重试，避免关闭成功后产生孤儿终端。
- 桌面支持项目创建/编辑/移除、AI/Shell 多标签、跨空间保留连接、断开与明确结束。
  原生网络开连接不持有全局状态锁，避免一个慢连接阻塞其他标签输入；退出清空在途句柄，
  迟到连接不能进入新的登录身份。正常关闭/撤权不重连；网络重连保持同一远端终端 ID。
- 配对码与 abox-link 使用独立存储，不依赖 tunnel 开关；桌面可生成码并在另一设备兑换。
  令牌仍留在原生进程/系统凭证库，码不写入 renderer 持久存储。

本地证据：Go 全仓 build/test/vet 通过；新增迁移与元数据、属主/写鉴权、链接/路径、配对
一次性与通道隔离测试通过。已有 Git ExecCommand 契约回归通过，未改变其默认环境。
新增系统备份/恢复测试确认项目、参数和 closing 状态保留；项目终端额度不足返回 4003、
closing 返回 409，均在无 Docker 的 fixture 上通过，证明拒绝发生在启动容器前。
Rust 5 项测试与 Clippy 通过，桌面构建与 6 项连接状态 Vitest 通过。

真实 Linux ARM64 Docker（使用隔离 fixture、network=none，无用户挂载/凭证/模型）：
两项目独立 shell 与网页默认终端同时工作、目录正确、环境不串用、断线保留 shell 变量、
resize、单终端结束不影响其他终端、账号撤权关闭均通过。测试替代了初始化挂载检查并省略
网络/MCP hook，因此不宣称验证了完整启动/旧发布二进制/服务重启或 purge 并发。

macOS ARM64 实际打包 WKWebView：legacy 模式与 projects 模式均 `ok=true`；后者通过界面
创建项目和两个终端标签，原生 fixture 确认收到两个独立 WS 路径的输入。跨平台 CI 已加入
两种模式，Linux CI 已加入真实 tmux fixture，但未运行 Windows/Intel CI，未公开发行或部署。

后续仍须补齐 P0/P1 的文件与凭证/平台验收、P2 完整启动/重启/purge/配对界面测试，再推进
P3 双向同步、P4 文件/截图粘贴与系统体验、P5 更新与发布验收。P2 也尚未宣布完全完成。

## 2026-10-03：P3 同步基础层与本地预检

本轮写入 `internal/syncproto`、`internal/syncclient` 和跨平台 `internal/syncfs`：

- 清单按原始字节 SHA-256，限制 10 万项、单文件 64 MiB、单次内容 2 GiB；扫描失败不返回
  部分清单。基线绑定服务器/用户/空间/项目/本地目录身份和忽略规则哈希。
- 三方计划区分本地/远端单边修改、删除与修改、首次来源选择、同内容收敛；双边不同修改、
  类型改变、目录保留子项等冲突会暂停项目。批量删除和强制方向必须确认当前预览 digest。
- `.agentboxignore` 使用受限 glob；默认忽略 `.git`、依赖、构建产物与 `.agentbox-sync`。
  忽略规则变化拒绝旧基线，不传播删除。
- 目录访问逐级固定 `os.Root`，Unix 使用 no-follow/硬链接检查，Windows 使用相对
  `NtCreateFile` 和 reparse/link 检查；扫描前后验证根身份。名称预检覆盖大小写、Unicode、
  Windows 保留名/非法字符/尾点空格/长度；不自动改名。
- 写入原语带 expected hash，随机临时文件、大小/哈希复核、同目录发布、覆盖/删除前恢复副本、
  恢复区数量/容量限制和取消处理。它不锁住外部 IDE/CLI，不声称严格事务隔离；执行器仍需
  在网络租约和服务端条件写接口之上实现。
- 桌面“检查本地目录”使用系统目录选择器，经私有 sidecar 管道检查；只返回摘要，不上传文件、
  不创建绑定、不触发写入。IPC 新增 `local_inspect_v1`，单 sidecar 检查可取消，EOF 后收尾。

验证证据：syncproto/syncclient/syncfs race 测试通过；覆盖同大小同 mtime 内容改动、链接/硬
链接、目录替换、Windows 名称冲突、三方冲突、批量删除、条件写入、并发编辑、失败传输和恢复
副本。Linux ARM64 与 Windows x64 syncfs 测试程序交叉编译通过；Go 全仓 test/vet/build、
许可证清单校验通过。macOS ARM64 原生 legacy/projects smoke 均 `ok=true`，额外验证本地预检、
中文终端、resize、独立标签和 sidecar 退出。Windows/macOS Intel 实机、Windows 文件锁实跑、
远端条件写入、网络传输与自动同步尚未验收；服务端 `sync` capability 保持 0。

补充推进：已新增只读 `GET /api/sessions/{id}/sync/manifest?project=...` 和
`POST /api/sessions/{id}/sync/lease`。清单复用服务端 safefs，扫描与空间生命周期串行，
最多两项并行扫描、60 秒超时；读取前后核对忽略规则与根目录身份，不创建目录、不启动容器。
租约有效期 30 秒，工作空间内父子目录互斥，校验设备、令牌与随机代次；进程重启使全部租约
失效，旧持有者不能续租或释放新持有者的租约。有租约时禁止修改/移除项目元数据。

新增 race 测试通过：远端清单原字节哈希、忽略目录、链接/硬链接拒绝、目录缺失不返回空树、
删除空间后拒绝迟到请求、跨用户和 query 写令牌拒绝、并发租约唯一持有者、过期与重启 fencing。
这些是协议基础接口，不是文件写入许可；条件写入/远端恢复/幂等日志仍待实现，不能宣称同步完成。

## 2026-10-02：P0/P1 进行中

基线 `eb845db`，依据[集成计划](desktop-client-integration.md)。本轮新增桌面目录、可移植 Go
后台和专用 CI；不修改服务端接口、数据库、账号模型、网页产物或生产部署配置。

已实现 Tauri 2/Vue/xterm 壳、原生 HTTP/WS 和凭证存储、旧服务端登录/空间列表/共享终端、
能力降级、输出确认和输入限额、重连与旧事件隔离，以及后台私有管道与平台进程监管。
CI 定义三个原生构建目标；合成协议测试和独立 WebView smoke 随代码提供。

本地验证（macOS ARM64）：

- `go build ./...`、`go test ./...`、`go vet ./...`、原网页 `npm run check` 通过。
- 桌面 `npm run build`、5 项 Vitest 通过：输出消费后确认、旧连接事件隔离、关闭码重连策略、
  关闭取消重连、输入上限和有序分块。
- Rust 合成 HTTP/WS 与后台测试通过：旧登录/用户/HTML 能力回退/列表/退出、Bearer 不进 URL、
  二进制输入与 resize、心跳/输出确认/撤权关闭码，以及编译后的 Go 后台握手和父管道 EOF。
- Rust Clippy（包含 smoke feature）通过；Rust 工具链下限按锁定依赖修正为 1.90。
- Go 后台构建了 Darwin ARM64、Darwin x64、Windows x64 三目标；后两者只有交叉编译证据。
- 独立 `Agentbox Smoke.app` 实际启动 WKWebView，在无 Vite 服务时完成界面登录、空间选择、
  原生 WS、xterm 中文 UTF-8 显示与输入、resize、随包后台启动，报告 `ok=true`；退出后
  进程检查没有残留本次 `agentbox-desktop/abox-sync`。这是合成回环服务，不是 Linux Docker。
- 原生 smoke 曾暴露 packaged build 未启用 custom-protocol 和测试插件 IPC 权限问题，
  已修复并重跑通过；测试命令最终仅编译进独立 smoke feature，正常包不含测试入口。
- 正常 `Agentbox.app` 开发包重新构建成功，检查前端产物不含 smoke 模块；实际启动后观察到
  随包 `abox-sync`，对本次应用进程执行强制终止，后台自动退出。此强杀验证仅在 macOS ARM64。

本地正常开发包位于 `desktop/src-tauri/target/debug/bundle/macos/Agentbox.app`，约 38 MiB，
ad-hoc 签名、未公证，不是正式发行包。启动开发源码见 [desktop/README.md](../../desktop/README.md)。

P0/P1 尚未宣布阶段完成；CI 配置不证明 Windows 已通过。没有运行真实 OS 凭证库写入测试、
Windows 安装器、Mac Intel 原生测试、输入法或最低系统验收。本轮未生产部署、提交或公开发布。

### 未完成验收与后续工作

- 原生凭证保存/恢复/撤销、多身份与异常路径测试；旧服务端文件浏览和手动传输。
- Windows/macOS Intel runner、真实安装包安装后测试、Windows 强杀父进程清理。
- 真旧版 Linux Docker 上的终端准入、网页接管/恢复、生命周期兼容测试。
- P0 文件系统能力、跨平台命名碰撞和原子替换验证。
- P2 配对/项目/独立终端/迁移；P3 条件写入/租约/同步/冲突恢复。
- P4 文件与截图粘贴、IME/高 DPI/休眠；P5 更新/许可证/安装/最低系统验收。

### v1 协议约定

能力 JSON：`{"protocol_version":1,"features":{"pairing":0,"project_terminals":0,"sync":0}}`。
0 表示不支持，非零值为该功能版本；未知字段忽略，未知主协议拒绝。服务端尚未增加此接口。
未来服务端返回非零能力也不能自动开放尚未实现的客户端操作。

本机 IPC 首行：`{"version":1,"type":"ready","capabilities":[]}`。每行一个 JSON，命令
含 `version/id/type`，现支持 `ping→pong`、`shutdown→stopped`，EOF 退出，大小上限 32 KiB。
未知版本/命令返回固定错误，不回显输入。stdout 仅协议，不输出令牌。本机 IPC 与 API 版本独立。
