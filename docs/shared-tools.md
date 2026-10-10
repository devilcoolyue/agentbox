# 共享工具与用户缓存

服务端可将 Go SDK、Playwright/Puppeteer 浏览器分发文件集中到数据盘。功能默认关闭；
配置并重启服务端后，运行中的空间继续原样使用，停止后再次启动才采用新挂载和环境。
不要求重新构建 Agent 镜像。空间的项目依赖仍由项目管理。

## 存储与权限

```text
/data/agentbox-tools/                  管理员指定的专用目录
  public/                             容器只读挂载到 /opt/agentbox-tools
    go/linux-amd64/go1.26.9/
    playwright/linux-amd64-debian-12/chromium-1208/
    puppeteer/linux-amd64-debian-12/chrome/linux-154.0.8037.57/
  private/<用户身份摘要>/cache/          仅对应用户可写挂载到 /var/cache/agentbox-user
    linux-amd64-debian-12/
      go-build/  go-mod/  npm/  playwright/  puppeteer/
  staging/                            发布临时目录，不挂入用户容器
```

用户身份摘要包含实例身份、用户名、用户创建时间。同一用户的空间复用缓存；其他用户、
同名重建用户以及恢复到新实例的用户不会继承它。删除用户保留旧缓存供管理员核对，
不会把它交给重建账号。公共目录及缓存父目录由服务端用户拥有，缓存叶目录属于 UID/GID
1000；容器只挂载自己的叶目录，不能选择或替换其他用户的挂载源。不能直接给所有容器
挂同一个可写 `/shared`。这项功能明确允许同一用户的不同空间共享缓存内容，不提供这些
空间之间的缓存隔离。

Go 的 `GOCACHE`、`GOMODCACHE` 和 npm 下载缓存都按用户隔离；私有模块不进入公共库。
浏览器 Cookie、profile、账号凭证、项目文件和聊天历史仍在原空间，不能迁入公共库。
Go 工具链按 Linux 架构区分；浏览器还按发行版及版本区分，不能将 Debian 的 WebKit
二进制直接用于其他 ABI。

## 发布工具

在 **Docker 与服务端所在的 Linux 主机**执行，工具目录选择有空余空间的数据盘。
以下路径只是示例，不会自动假定所有机器都有 `/data`：

```sh
python3 scripts/tool-storage.py --root /data/agentbox-tools init
python3 scripts/tool-storage.py --root /data/agentbox-tools install-go \
  --version go1.26.9 --arch amd64
python3 scripts/tool-storage.py --root /data/agentbox-tools install-playwright \
  --image <已有的不可变Agent镜像ID或本地标签> --version 1.58.2 \
  --browsers chromium webkit
python3 scripts/tool-storage.py --root /data/agentbox-tools install-puppeteer \
  --image <已有的Agent镜像> --version <项目使用的确切npm版本>
python3 scripts/tool-storage.py --root /data/agentbox-tools verify
```

发布器持独占锁，将内容先放到私有暂存目录，核验后原子发布，版本目录不可覆盖。Go
下载使用官方版本目录的 SHA256，并检查 VERSION、ELF 架构和归档路径。离线发布可提供
`--archive FILE --sha256 EXPECTED`；校验值必须来自可信发布源，不能仅对不可信文件现算
一个摘要就声称验证了来源。

浏览器由临时 Docker 容器中的确切版本官方 npm 包下载；npm 安装禁用生命周期脚本，
随后显式调用相应官方浏览器下载器。临时容器不挂宿主目录、账号或 Docker socket，不用
用户已有缓存作为公共发布源。npm 包按 registry integrity 校验，浏览器下载依赖官方
下载器的 HTTPS 来源；发布时记录 package-lock 摘要及本地完整文件树摘要。后者用于检测
发布后的改动，不等同于上游签名。发布相同 revision 时，若字节不同会拒绝覆盖。

镜像必须本地已有、包含 Node/npm、为 Linux，且没有 VOLUME/ONBUILD。下载过程需要网络；
发布只更新公共工具目录，不改生产镜像、配置或运行中的容器。工具库保留多个版本，公共
目录应先在无用户使用的维护窗口核对引用再退役版本；目前不自动删除公共版本。

## 启用

操作员在配置文件增加以下字段，服务端重启后生效：

```json
"tool_storage": {
  "root": "/data/agentbox-tools",
  "go_version": "go1.26.9"
}
```

`root` 必须是独立的绝对路径，不能放入用户数据目录；Go 版本可省略，此时保留镜像和
用户原来的 Go 查找方式，只启用用户缓存和浏览器共享。指定的 SDK 必须先发布；缺失时
拒绝启用该空间的新环境，且在删除旧的已停止容器之前检查。正常运行中的空间不会因此
被重建。公共版本更新后，空间下次启动会补齐版本链接。

工具环境在容器创建时统一注入，网页 Agent、交互终端和非交互命令均可使用；不只修改
`.bashrc`。Go SDK bin 目录放在镜像 PATH 前面，Go 自动下载的额外工具链仍进入用户自己的
模块缓存。项目/用户显式覆盖这些环境变量时，以其覆盖为准。

Playwright 的 `PLAYWRIGHT_BROWSERS_PATH` 指向用户自己的可写目录，其中公共 revision
以符号链接指向只读库。安装器可写自己的 `.links`；缺失 revision 会正常下载到该用户
目录。`PLAYWRIGHT_SKIP_BROWSER_GC=1` 防止一个空间的安装器清理另一个空间仍在使用的版本。
Puppeteer 使用同样方式设置 `PUPPETEER_CACHE_DIR`。项目里的 Playwright/Puppeteer npm 包
保持自己的版本；不同 browser revision 不强行合并。

## 旧空间与清理

首次采用新挂载只切换后续缓存写入，不自动复制大量旧编译缓存，也不自动删除 home 中
任何旧目录。旧副本保留用于核对；确认公共工具能执行、依赖能正常恢复、相关任务结束后，
再由管理员选择清理：

- `home/.cache/go-build` 是旧编译缓存；新缓存位于用户数据盘目录。Go 自带缓存老化机制，
  不承诺硬容量上限。重建需要时间。
- `home/.npm/_cacache` 是旧下载缓存，不包括项目 node_modules 或登录配置。
- `home/.cache/ms-playwright`、`home/.cache/puppeteer` 中仅已在公共库提供并核验过的 revision
  才是去重候选；其他版本保留。浏览器登录资料 `home/.agentbox-browser` 不能当安装缓存删除。
- `home/go/pkg/mod` 可能包含暂时无法重新下载的私有依赖；首次不自动迁移或删除。
- `home/sdk` 可能被脚本写死绝对路径引用，不能仅因 PATH 已更新就删除。

用户缓存目录仍会增长，特别是未发布的浏览器版本；设置 `PLAYWRIGHT_SKIP_BROWSER_GC`
保证共享安全的同时，需要管理员定期核对。清理用户缓存前应暂停该用户相关构建；不要
对全站目录执行全局 prune，也不要在公共程序仍被使用时直接删版本目录。

公共工具和外部缓存不包含在系统/完整备份中；它们按安装清单重建。恢复会移除
`tool_storage` 配置，避免挂到原实例的宿主目录；配置新路径并重新发布工具后再启用。
旧版服务端不认识新的容器环境变量，回退前应停用该功能并在维护窗口重建相关空间。

## 验证

```sh
python3 scripts/verify.py run --step policy.tool-storage
python3 scripts/verify.py run --step docker.tool-storage --image <本地Agent镜像> --timeout 360
```

真实 Docker 回归运行在独立 Linux 卷中，覆盖只读公共工具、同用户复用、跨用户隔离、
私有下载目录可写、SDK PATH、运行中不重建和停止后移除旧挂载。测试 SDK/browser 为合成
文件，验证挂载和路径契约；不替代实际 SDK 编译、各版本 Playwright 下载/启动或浏览器
系统依赖验收。下载器离线单测检查摘要、不可变发布和路径/链接边界，不访问模型。

可选网络验收（会下载官方 Go/Playwright，使用本机临时目录，结束自动清理）：

```sh
python3 scripts/verify.py run --step docker.tool-storage-runtime --image <本地浏览器Agent镜像> --timeout 1500
```

该入口发布实际 SDK/browser 后，以 UID 1000、只读公共挂载和生产浏览器 seccomp 执行；
安装 npm 测试包后断开容器网络，再验证 `playwright install chromium` 复用现有版本、
保留 Chromium 沙箱的页面渲染以及 Go 编译。ARM64 Debian 12 上 Go 1.26.9、Playwright
1.58.2 已做本地实际下载与运行验证；生产 amd64 及其他 Playwright/Puppeteer 版本仍需
选定版本后验收，不将合成测试作为全版本兼容承诺。
