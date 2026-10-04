# Windows WebView2 离线安装与验收

Windows 完整 NSIS 安装包使用 `offlineInstaller`，缺少 WebView2 时可从包内安装微软的 Evergreen
Standalone Runtime，不需要在用户安装期间下载 bootstrapper。`candidate-config.py` 保留同一配置。
版本仍为 0.1.1，更新下载上限仍为 512 MiB；未来微软包增长导致超限时构建验收失败，不提高上限。

这项配置不是离线安装通过的证据。本机为 macOS，新增脚本尚未在 Windows 真包或缺失 Runtime 的
离线系统执行；必须分别取得下面的实际产物审计和 guest 安装报告。托管 Windows CI 的常规安装、
`--diagnostics-json` 或已有 Runtime 下的 WebView smoke 都不能替代“缺失 + 离线”的组合验收。

## 已核对的构建行为

审查依据为 package-lock 锁定的 Tauri CLI **2.12.1**，不是最新版文档的猜测：

- [Tauri Windows 安装文档](https://v2.tauri.app/distribute/windows-installer/)：offlineInstaller
  额外约 127 MB；这是参考值，实际文件大小由脚本读取。
- [2.12.1 下载实现](https://github.com/tauri-apps/tauri/blob/tauri-cli-v2.12.1/crates/tauri-bundler/src/bundle/windows/util.rs)：
  x64 选择 `https://go.microsoft.com/fwlink/?linkid=2124701`，重定向必须匹配微软
  `https://msedge.sf.dl.delivery.mp.microsoft.com/filestreamingservice/files/`；下载按 GUID 缓存。
  Tauri 在此处不额外验证 Authenticode，已有缓存会复用。
- [2.12.1 NSIS 模板](https://github.com/tauri-apps/tauri/blob/tauri-cli-v2.12.1/crates/tauri-bundler/src/bundle/windows/nsis/installer.nsi)：
  缺失时提取 `MicrosoftEdgeWebView2RuntimeInstaller.exe` 并执行 `/silent /install`。
  更新模式跳过 Runtime 安装；旧兼容更新器专用 NSIS 还会覆盖其 WebView 配置。因此完整首次安装包
  是离线补装 Runtime 的交付物，不能把仅更新包的配置或成功记录当成首次安装证明。
- [微软部署说明](https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution)：
  同时读取 HKCU 与 HKLM 的 `pv`，并可通过 WebView2Loader API 检测可用浏览器；API 也可能返回
  Edge Beta/Dev/Canary。安装器提升权限或系统已有 Edge Updater 时，Runtime 可能按机器安装，
  即使 Agentbox 自身采用 currentUser NSIS。

`desktop/vendor-notices/Microsoft-WebView2.txt` 随应用进入 `third-party/vendor/`。Runtime 为微软
专有组件，适用微软提供的条款与原始安装器内的许可/声明；**不计入 OSS inventory 的开源条目数**，
也不适用 Agentbox 的 Apache-2.0 授权。审计要求微软签名原件，不修改或重新签署该 payload。
Evergreen 会在联网后按微软机制更新；离线包不保证未来永远离线也能收到安全修复。

## 实际包审计：GitHub 托管 Windows 可直接执行

先完成正常 Windows x64 NSIS 构建，再运行：

```powershell
$packages = @(Get-ChildItem 'desktop/src-tauri/target/x86_64-pc-windows-msvc/release/bundle/nsis/*.exe')
if ($packages.Count -ne 1) { throw 'Expected exactly one full NSIS installer' }
python desktop/scripts/test-webview2-package.py $packages[0].FullName `
  --nsis-script 'desktop/src-tauri/target/x86_64-pc-windows-msvc/release/nsis/x64/installer.nsi' `
  --report "$env:RUNNER_TEMP/webview2-package.json"
if ($LASTEXITCODE -ne 0) { throw 'WebView2 payload audit failed' }
```

审计会实际用 7-Zip 提取包内 Runtime、Agentbox EXE 和微软 notice，核对：

1. 完整 NSIS 的 `ARCH=x64`、`offlineInstaller`，包内应用 PE 为 AMD64。
2. 实际包和每次提取的文件不超过 512 MiB；每项精确长度、SHA-256，包在审计期间未改变。
3. 包内 Runtime 与本次 Tauri 缓存输入逐字节相同，Microsoft Authenticode 状态为 Valid。
4. 从该 GUID 的微软 x64 原始 URL 重新流式读取并比对哈希，缓存文件被替换不能靠文件名过关。
5. 闭源 Runtime notice 确实存在于包内。

修复入口 `windows/webview2-hooks.nsh` 还嵌入同源 `AgentboxWebView2Repair.exe` 别名，审计要求它
与标准入口 payload 的长度、SHA-256 完全相同，再统一核验微软签名及来源。NSIS 会复用相同
数据块；实际包大小仍单独检查，不能根据这种优化假定永不超限。7-Zip 对 NSIS solid 归档通常
不给逐文件 Size，脚本因此限制实际提取的流，不把空 Size 误判为零字节或跳过大小限制。

报告保留实际来源 URL、供应商签名指纹、payload/NSIS 大小和哈希。Vendor EXE 的 ProductVersion
是安装器版本，不能伪装成安装后的浏览器 Runtime 版本。微软安装器外层可能是 x86 自解压 stub，
所以不能根据它的 PE Machine 宣称内层运行时架构；架构证据来自微软 X64 下载资源、生成脚本
和实际 Agentbox AMD64 文件。网络/签名/解包失败属于 **audit failed**，不能记为缺少 guest 而跳过。

## 修复 0.0.0.0 注册记录

微软把 `pv=0.0.0.0` 也定义为 Runtime 缺失。锁定 Tauri 模板只对空字符串启动 Runtime 安装，
所以 Agentbox 使用 `NSIS_HOOK_PREINSTALL` 补这个明确缺项：它在原 WebView2 Section 之后、
应用文件复制之前，分别读取 HKLM/HKCU 的 pv 和 Win32 返回状态。优先使用有效机器级版本，
否则使用有效用户级版本；机器残留 0.0.0.0 不能遮蔽正常 HKCU，也不能让非管理员成功写入
HKCU 后被误判失败。只有两个 Hive 均确认缺失、空或零时才运行上述同源微软安装器。
拒绝访问、值类型错误、缓冲区不足等未知读取状态不当成缺失。无法提取/启动、退出码非零
（含未知 HRESULT）、返回后仍无有效版本，
都中止应用安装。四段版本号须为 0～65535 的数字且至少一段非零，不用宽松前缀转换猜测成功。
hook 不自行写、删除或重命名任何 EdgeUpdate 注册表项，也不复制整个上游 NSIS 模板。

更新模式和专用 updater 配置不走这个首次安装修复；普通首次安装的空 key 仍由 Tauri 前一
Section 处理，hook 额外复核其最终注册值。需要真实 Runtime 的离线修复验收仍须由下述 guest
实际执行，hook 编译或控制流 fixture 成功不能代替真实微软安装器结果。

`test-webview2-hook.py` 用真实 NSIS 编译器验证主安装/更新上下文，并可在托管 Windows 实际
执行 21 个控制流 fixture：双 Hive 优先级、机器零值与有效用户值、非管理员仅修复用户级、
拒绝访问/超长/错误类型、供应商失败/未知 HRESULT/无法启动、提取失败时不运行残留 payload、
安装后仍缺失或版本损坏、两类更新模式等。fixture 仅在编译时把单 Hive I/O 换成临时 INI，
执行产品的同一双 Hive reader/selector，并
使用只写临时 marker 的合成 vendor EXE；不触碰宿主 WebView2，也不宣称安装真实 Runtime。

```powershell
python desktop/scripts/test-webview2-hook.py `
  --makensis "$env:LOCALAPPDATA/tauri/NSIS/makensis.exe" `
  --execute-fixtures --report "$env:RUNNER_TEMP/webview2-hook.json"
if ($LASTEXITCODE -ne 0) { throw 'Actual NSIS control-flow fixture failed' }
```

成功路径还验证所有调用方通用寄存器、栈和入口 Errors flag 被保留。另有只读原生 registry
probe 直接执行未替换的 RegGetValueW，并与 Python Windows 注册表 API 核对两 Hive 的原值
和状态，防止仅通过 INI fixture 掩盖真实 ABI/视图错误。

macOS 本地已通过真实 NSIS 3.13 的两种上下文、21 个 fixture 和原生 reader probe 编译；没有
执行 Windows EXE。CI 使用 Tauri 下载的锁定 NSIS 工具实跑，报告需明确 `executed=true`。

## 缺失前提检查：不破坏托管 runner

```powershell
python desktop/scripts/test-webview2-offline.py $packages[0].FullName `
  --package-audit "$env:RUNNER_TEMP/webview2-package.json" `
  --report "$env:RUNNER_TEMP/webview2-preflight.json"
if ($LASTEXITCODE -ne 0) { throw 'WebView2 prerequisite audit failed' }
```

此命令默认只读，使用 Cargo.lock 对应 `webview2-com-sys` 的 x64 WebView2Loader DLL；可以用
`--loader` 明确指定从同一受信 SDK 复制的 DLL。它记录 HKCU/HKLM 两种注册表视图、Loader API
实际结果、标准目录下的 Runtime/预览版文件，以及所有网络接口状态。访问拒绝、检测失败或格式
异常均失败，不当成“缺失”。只删注册表不能让文件与 Loader 检查一起变成缺失。

报告为 `blocked` 时，只表示前提尚不具备，例如已有 Runtime 或宿主网络仍连接；这不是离线
安装通过。默认前提采集退出 0 便于上传证据，真正的 guest 执行模式只有完整 `passed` 才退出 0。
托管镜像及软件内容会变化，应以现场报告为准。

不要卸载宿主共享 WebView2，不删除/重命名注册表伪造缺失，不给全机改防火墙、禁网卡或终止
Edge Update 服务。GitHub agent 必须保持联网以执行日志与 artifact 上传。`windows-latest` 的
Windows Server 镜像也不能假定支持 Windows Sandbox；未获得独立、可丢弃、真实缺失 Runtime
的 guest 时，验收保持 blocked。共享 Runtime 缺失环境是剩余外部条件，不隐藏 payload 审计错误。

## 真正的缺失 + 离线 guest 验收

使用新建快照的 Windows 10/11 测试 VM，或确实支持 Windows Sandbox 的 Windows 主机。**在
VM 管理层关闭 guest 网络**，宿主网络保持正常。Windows Sandbox 的配置应明确含
`<Networking>Disable</Networking>`；只映射只读输入目录及一个独立可写报告目录。不要在托管
runner 宿主通过禁用网卡来模拟这一步。

向 guest 的本地固定磁盘复制：Python 3.12、上述已审计 NSIS 及 JSON、`desktop/scripts/`、
`desktop/package.json`、Cargo.lock 对应的 x64 WebView2Loader.dll，以及同一 checkout 构建的
独立 desktop-smoke 可执行文件、相邻 sidecar 与构建所需资源。报告目录应独立映射回宿主。
然后在这个可丢弃 guest 内执行：

```powershell
$env:AGENTBOX_DISPOSABLE_WINDOWS_GUEST = '1'
python C:/agentbox-test/desktop/scripts/test-webview2-offline.py C:/agentbox-test/Agentbox-setup.exe `
  --package-audit C:/agentbox-test/webview2-package.json `
  --loader C:/agentbox-test/WebView2Loader.dll `
  --smoke-binary C:/agentbox-test/smoke/agentbox-desktop.exe `
  --expected-version 0.1.1 `
  --run-in-disposable-offline-guest `
  --report C:/agentbox-evidence/webview2-offline.json
if ($LASTEXITCODE -ne 0) { throw 'Offline guest acceptance did not pass' }
```

脚本首先确认 Runtime 真正不存在、没有活动的非 loopback 网卡、没有现有 Agentbox 安装。随后：

1. 从含中文/空格的临时目录静默安装实际 NSIS。
2. 再查真实 Runtime 注册表与 Loader，验证新 Runtime 可发现。
3. 已安装应用冷/重复运行 diagnostics，核对版本、随包 sidecar 和许可证资源。
4. 启动显式 smoke 构建，必须真实产生 `window_visible`、`terminal_echo`、`finishing` 和 Windows
   x64 成功报告；这一项才证明 WebView/页面/终端流真实运行，diagnostics 本身不创建 WebView。
5. 静默卸载 Agentbox，并比较安装前后的程序文件与卸载注册表；保留共享 WebView2，收集报告
   后丢弃整个 guest。过程中再次核对网络，任何失败不记成功。

这证明的是“正常包安装及诊断 + 同构建 WebView smoke”，**不等于正常安装包全部 GUI、IME、
升级、真实桌面最低系统版本或硬件验收**。guest 内新安装的共享 Runtime 不在宿主清理，也不会
通过脚本强制卸载。尚无真实 guest 报告时不能宣布这一门槛完成。

## CI 接入片段

下面步骤放在 Windows 完整 NSIS 构建之后，路径相对于 workflow 的 `working-directory: desktop`。
guest 运行不在此宿主步骤触发：

```yaml
- run: python scripts/test-webview2-harness.py
- name: Audit actual offline WebView2 payload and record Runtime preconditions
  if: runner.os == 'Windows'
  shell: pwsh
  run: |
    $packages = @(Get-ChildItem 'src-tauri/target/${{ matrix.target }}/release/bundle/nsis/*.exe')
    if ($packages.Count -ne 1) { throw 'Expected one full NSIS installer' }
    python scripts/test-webview2-hook.py --makensis "$env:LOCALAPPDATA/tauri/NSIS/makensis.exe" --execute-fixtures --report '${{ runner.temp }}/webview2-hook.json'
    if ($LASTEXITCODE -ne 0) { throw 'NSIS hook control-flow fixture failed' }
    python scripts/test-webview2-package.py $packages[0].FullName --nsis-script 'src-tauri/target/${{ matrix.target }}/release/nsis/x64/installer.nsi' --report '${{ runner.temp }}/webview2-package.json'
    if ($LASTEXITCODE -ne 0) { throw 'WebView2 payload audit failed' }
    python scripts/test-webview2-offline.py $packages[0].FullName --package-audit '${{ runner.temp }}/webview2-package.json' --report '${{ runner.temp }}/webview2-preflight.json'
    if ($LASTEXITCODE -ne 0) { throw 'WebView2 precondition audit failed' }
- uses: actions/upload-artifact@v4
  if: always() && runner.os == 'Windows'
  with:
    name: desktop-webview2-${{ matrix.target }}
    path: |
      ${{ runner.temp }}/webview2-package.json
      ${{ runner.temp }}/webview2-preflight.json
      ${{ runner.temp }}/webview2-hook.json
    if-no-files-found: warn
```

可在 macOS/Linux 本地运行 `python3 desktop/scripts/test-webview2-harness.py` 核对失败分支。这些
是合同/护栏测试，不能计为 Windows 安装或 GUI 验收。
