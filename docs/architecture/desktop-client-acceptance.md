# 桌面客户端开发与验收记录

此表按当前实现维护，历史过程见 [实施记录](desktop-client-progress.md)。用户本轮确认的交付范围是
完成客户端开发与未签名包验收；正式签名、公证和缺少设备的人工验收单独保留。

当前状态：功能开发和本地自动化收尾已完成，Windows/macOS 三架构最终候选包验证进行中。
正常部署的同步入口默认关闭，管理员可通过 `desktop_sync_enabled` 明确启用；此次没有修改生产配置。

## 阶段交付

| 阶段 | 已实现 | 自动化证据 | 尚需外部验收 |
| --- | --- | --- | --- |
| P0 | 同仓独立 Tauri/Vue/xterm 客户端、Go 私有管道、平台文件访问与协议门控 | Go/Rust/前端检查；三目标工作流；固定目录、命名、卷与取消回归 | 最低 Windows/macOS 版本 |
| P1 | 旧服务端登录、系统凭证、空间列表、共享终端、文件浏览/传输、连接恢复 | 冻结旧服务端/旧网页/abox-link兼容；原生 legacy smoke；隔离系统凭证 CRUD | 已有真实用户 keyring 在正式签名升级中的延续 |
| P2 | 配对、项目映射、AI/Shell 多标签、生命周期与 schema10 | 两个独立终端原生 smoke；真实 Docker/tmux；配对/改密原子性与并发 race；备份/升级兼容 | 生产部署后的受控联调 |
| P3 | 双向/单向预览、逐文件冲突、连续同步、分页容量、清理/导出、异常归档、独立远端恢复 | Mac/Windows→真实Linux同步；内核强杀、丢响应、条件写入、ENOSPC；真实 Vue→Rust→Go 同步 smoke | 物理掉电和更广泛设备/安全软件组合 |
| P4 | 网页一致界面、主题/缩放、搜索、右键/快捷键、文件/图片粘贴、拖放票据、暂存输入、休眠重连 | 浏览器布局/焦点/组合输入事件；原生 UTF-8/resize；私有 pasteboard 与自有 HGLOBAL；有界格式解析 | Finder/Explorer实际手势、系统中文候选窗、多屏/DPI、实际休眠 |
| P5 | 独立候选构建、更新通道/签名校验、安装前收尾、Mac原子替换与恢复、离线WebView2、许可证清单 | 隔离真实0.1.0→0.1.1.app替换；签名/篡改/中断下载；真实更新ENOSPC/SIGKILL恢复；候选/安装探针 | Apple/Windows正式证书、公证、正式签名包应用内更新、真正无WebView2的离线Windows |

“自动化通过”不替代最后一列。默认测试中有明确 opt-in/ignored 的用例，只有设置实际资源并
单独运行成功才计入证据；不会把跳过或设置声明记为实机通过。

## 本地验证

- Go 全仓 `go build ./...`、`go test ./...`、`go vet ./...` 通过。
- 账号/配对/密码重置四包定向 race 通过；冻结 schema9→10 的真实 Linux Docker 兼容新增
  中文、空格、特殊字符文件名、shared/workspace 下载和容器文件属主验证。
- 前端48项测试与类型检查/构建通过。浏览器用合成 Tauri transport 验证可见终端接收拖放、
  隐藏终端拒收并释放票据、已有选择保留；组合输入期间 Escape 不关闭设置，普通 Escape 可关闭。
  这些是浏览器事件验证，不是实际 OS 输入法/文件拖放手势验收。
- 预览瞬时故障通过Go类型、Rust原命令和前端阶段三层约束，按1/2/4/8秒有界重试；实际
  Go→Rust桥接确认同样503不会给写入命令重试标记。Windows sharing/lock violation有界重试
  每次重验租约、目录、目标及暂存内容；权限拒绝仍立即失败。其原生句柄回归纳入Windows CI。
- Rust普通测试、普通/smoke Clippy通过；Mac私有pasteboard文件/PNG/TIFF与修订检查通过。
  原生系统凭证的合成数据仅进入独立 validation service，并在测试后删除。
- 64MiB 独立 HFS+ 镜像实际写满后更新解包返回 ENOSPC，旧应用完整，测试卷已卸载。
  在解包后和原子交换后强杀受控子进程，退出信号均为9，下一次显式更新成功。
- 实际已安装0.1.0包仅被读取并复制到隔离目录；0.1.1整包替换/重启诊断通过，设备身份、绑定、
  基线、历史和 schema5 均保留。未改用户 `/Applications/Agentbox.app` 或真实配置/凭证。

报告：`/tmp/agentbox-pairing-unicode-compat-20261004.json`、
`/tmp/agentbox-updater-real-enospc.json`、`/tmp/agentbox-installed-completion-0.1.1.json`。

## 候选包与发布边界

开发工作流通过 `build-candidate.py` 运行与独立手动候选工作流相同的未签名配置及打包过程。
签名参数仅在 `signed=true` 时传入；候选生成不会创建公开 Release、更新稳定清单或覆盖
服务端 `releases/latest`。正式候选工作流在合入默认分支后可手动触发，详细过程见
[桌面候选包与更新](desktop-release.md)。

正式发布前仍须提供并验证 Apple Developer ID/公证与 Windows 代码签名资源，以及上述人工/
最低系统矩阵。更新私钥已独立配置，但它不能替代操作系统签名。同步是否启用和生产部署是
管理员的独立操作，不随本地开发包自动发生。

手动上传单批限19MiB、附件单项19MiB；手动下载及同步单文件限64MiB。macOS同步目录需位于
可核验的内置固定磁盘。以上限制会明确拒绝超限操作；不承诺任意大小文件或外置磁盘同步。
