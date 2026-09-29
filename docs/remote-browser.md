# 远程浏览器（第一版）

工作空间的「浏览器」页签提供服务器上的完整浏览器桌面：地址栏、多标签、网页登录和文件下载。可以打开 Claude、ChatGPT、GitHub 或任意 HTTP/HTTPS 网站。网页登录与账号池的 CLI OAuth 授权各自独立；当前版本不自动捕获授权码。网页里的模型调用使用网站自身的订阅/额度，不进入 Agentbox 的 CLI token 用量流水。

## 启用

管理员在运行工作空间容器的 Linux Docker 主机上构建可选镜像：

```bash
./scripts/build-image.sh
./scripts/build-browser-image.sh
```

在「系统设置 → 容器」把镜像设为 `agentbox-agent:browser`。已有空间需要停止后重新启动，才会使用新镜像。推荐每个空间至少 2 GiB 内存、2 CPU、512 PID；浏览器与 CLI 共享空间资源和并发配额。原有轻量镜像不会安装浏览器，打开功能时会给出明确提示。

Linux amd64 默认安装 Google Chrome for Testing **154.0.8037.57**，可通过 `AGENTBOX_CHROME_VERSION` 指定其他确切版本后重新构建。Chrome for Testing 是 Google 提供的 Chrome 二进制，本项目不自动更新它；管理员应定期重建以获得安全更新。`AGENTBOX_BROWSER_BASE_IMAGE` 可指定已有 Agent 镜像，`AGENTBOX_BROWSER_IMAGE` 可指定输出标签。不要把原有 CLI 镜像自动更新任务指向浏览器标签；升级基础镜像后再重建浏览器镜像。

Google 没有提供 Linux arm64 的 Chrome for Testing 二进制。ARM 镜像明确改用构建时 Debian 安全仓库的 Chromium，界面显示真实名称，不伪称 Chrome。

## 使用

顶部使用紧凑工具栏：左侧播放/停止图标控制连接，中间输入网址并按回车或右箭头打开，右侧品牌图标打开 Claude / ChatGPT，末尾调整画质与全屏。图标支持悬停提示与键盘访问；窄屏网址栏自动换行。

- 「连接浏览器」启动或连接现有桌面；Claude、ChatGPT 快捷按钮和网址输入框会打开新标签页。Chrome 自己的地址栏可以导航当前标签页。
- 浏览器登录状态、Cookie、历史、书签按工作空间保存在 `home/.agentbox-browser/`。同一账号下不同空间也各自隔离。
- 切换页签或关闭控制台只断开画面，不退出浏览器。连接期间持有空间活动引用；断开后沿用空间的空闲回收策略。
- 「关闭浏览器」保留配置；停止空间会一起停止浏览器；删除空间时仅 `purge=1` 连配置一起删除。
- 下载默认进入 `/workspace/Downloads`，从「文件 → Downloads」下载到本机。上传到工作区的文件，也可以在 Chrome 的文件选择器里选择。
- 中文或复杂文字使用底部「剪贴板」：填入文字、在远程页面选中输入框，再点「发送并粘贴」。远程执行复制后点击「读取远程复制」，文字出现在文本框中。该通道使用 UTF-8，不依赖 VNC 的旧式 Latin-1 剪贴板；不会自动读取本机剪贴板。
- 画质默认「流畅」，降低图像编码质量以减少带宽；阅读细小文字可切换「均衡」或「清晰」，即时生效并在当前设备记忆，不需要重启浏览器。压缩等级保持较低，避免额外挤占服务器 CPU。
- 桌面采集使用 X DAMAGE 跟踪变化区域，Chrome 使用容器的 256 MiB `/dev/shm`，避免强制改用磁盘临时目录。
- 支持全屏与按视口缩放。桌面固定 1440×900，手机端适合查看和简单操作。

## 网络与权限

浏览器使用空间所属账号的出口代理。Chrome 不支持在代理参数中直接使用 Basic 密码，因此容器内的仅回环监听适配器转发到既有账号代理桥。上游不可用时返回错误，不改走直连；关闭 QUIC 和非代理 WebRTC UDP，避免常见的代理绕行。账号代理绑定变更后需重新连接，启动会重建浏览器进程以应用网络设置。内网访问继续受工作空间的现有网络策略控制。

所有 HTTP/剪贴板/WebSocket 接口要求当前用户拥有空间，且仍可使用关联账号；额度检查与终端一致。长连接每 30 秒重查登录、账号权限和网络设置。VNC 只绑定容器回环地址，经 Docker exec 和已鉴权的 WebSocket 转发；不映射 VNC 端口，不开放 Chrome 调试接口。

容器仍为 UID 1000、`no-new-privileges`、原有挂载与资源限制。浏览器镜像额外采用允许用户命名空间的窄 seccomp 配置，Chrome 自身沙箱保持开启，不使用 `--no-sandbox` 或 privileged 模式。主机若禁用非特权用户命名空间，浏览器启动会失败，需要管理员调整主机策略。

浏览器与同空间的 Agent/终端同 UID，**不是同空间程序之间的凭证隔离边界**。网页登录资料属于空间 home，完整备份需使用 `--full`；基础备份不包含这些资料。服务器管理员能够访问这些文件。

本版不提供声音转发、本机摄像头/麦克风映射、浏览器配置迁移或指纹浏览器兼容。网站登录、验证码与通行密钥仍取决于网站支持；本机硬件通行密钥不会被自动转发。固定浏览器配置和出口不能保证与原指纹浏览器一致，也不能保证任何网站一定允许登录。

## 开发验收

```bash
go build ./...
go test ./...
npm run check
npm run build
python3 scripts/test-browser-runtime.py
python3 scripts/test-browser-proxy-live.py --image agentbox-agent:browser
python3 scripts/verify-third-party.py
AGENTBOX_PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs node scripts/test-browser.mjs

# 按本机 Docker daemon 架构交叉编译，例为 arm64；全部为临时合成数据，无模型请求。
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/agentbox-browser-test ./cmd/agentbox
AGENTBOX_PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs \
  python3 scripts/test-remote-browser-live.py --binary /tmp/agentbox-browser-test --image agentbox-agent:browser
```

真实测试检查沙箱配置、不发布端口、页面加载、UTF-8 剪贴板、noVNC 画面、浏览器/空间重启后的 Cookie、鉴权及删除。桌面截图在 `output/playwright/remote-browser-live*.png`。第三方 noVNC 1.6.0 保留原始源码与许可，哈希在 `third_party/vendor.json`。

代理回归必须包含实际 Chrome：`--host-resolver-rules` 会作用于代理的 IP 字面量，必须排除 `127.0.0.1`，否则即使本机代理正常，浏览器仍报 `ERR_PROXY_CONNECTION_FAILED`。真实代理测试使用 `network=none` 和不可解析域名，验证请求实际到达合成认证代理。
