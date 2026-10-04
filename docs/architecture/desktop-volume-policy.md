# 桌面同步的本地卷策略

macOS 的同步根必须位于可核验的内置、不可移除、本地磁盘。网络卷、USB／Thunderbolt
外置盘（即使自报为固定介质）、可弹出介质、磁盘映像和不能完整识别的卷均拒绝。
首版只接受 APFS／HFS；内核文件系统类型必须与该卷 `diskutil` 返回的类型一致，未知文件系统
不能仅凭自报本地卷及内置磁盘来源获得许可。
该限制作用于 `syncfs.Open`，因此预检、绑定、扫描、执行和采用同一边界的恢复目录都适用；
没有通过手动确认绕过限制的入口。各平台已有卷拒绝路径统一返回 `ErrUnsupportedVolume`，
预检转换为 `inspection_unsupported_volume`，不会输出系统元数据或绝对路径。服务端 `sync=0` 不受此修改影响。

## macOS 判定

1. `statfs` 核对 `MNT_LOCAL`，取内核报告的实际 `/dev/disk…` 设备，不根据 `/Volumes`
   路径前缀判断。设备名只允许 `disk` 后接数字及分区段。
2. 使用固定、系统提供的 `/usr/sbin/diskutil info -plist /dev/disk…` 读取 Disk Arbitration／
   IOKit 的机器可读元数据。没有 shell，不搜索 PATH，不传用户目录，不解析本地化终端文本。
   侧车继续用 `CGO_ENABLED=0`，不新增框架动态调用库。
3. 卷、APFS 的全部物理 store、对应整盘都必须明确报告 `Internal=true`、`Removable=false`、
   `Ejectable=false`；其他可移除／系统映像标志如存在也必须为 false。回包的设备 ID／节点
   必须匹配查询对象。缺字段、类型不符、未知设备、重复 store、失败或超时均拒绝。
4. APFS 的合成卷本身可以是 Virtual，但必须核对全部 `APFSPhysicalStores`。Fusion 多盘容器
   中任一外置 store 都会使整卷被拒绝。物理 store 和整盘明确为 Virtual 时拒绝；整盘还必须
   有原生 `IODeviceTree:/…` 路径及已知内部总线（Apple Fabric、PCI／PCI-Express、SATA／ATA、
   ATAPI、SCSI／SAS、NVMe）。未知总线、USB、FireWire、Thunderbolt、Disk Image 不放行。
5. 不能只看 `VirtualOrPhysical`：本机 Apple Silicon 内部 SSD 的该字段实际为 Unknown。
   内置标志、不可移除标志、设备树和内部总线的组合提供正向证据；Unknown 本身不提供许可。

整个新根的元数据查询从当前请求上下文派生，共用 5 秒截止时间，进程收尾额外至多等待 1 秒；单份输出上限 1 MiB，
XML 深度上限 16、token 上限 32768、APFS store 上限 32。标准 XML 解析器不解析外部实体。
这里只运行 `diskutil info`，不改变磁盘、挂载或用户文件。

实际侧车请求通过 `OpenContext`、`OpenStateContext`、`RegisterContext` 以及祖先／导出目录处理
传递父管道 EOF 的取消；不会把某次请求上下文保存在长期 StateStore 中。查询取消时杀死并回收
当前命令，不能等后台的独立超时耗尽才响应用户取消。无上下文兼容方法只用于独立调用者，桌面请求
不走这些方法。真实阻塞子进程取消测试已通过，不能把它等同于任意文件系统内核调用都可取消。

## 句柄与性能

每次新建 `Root` 都重新查系统元数据；不按 `diskN` 建全局成功缓存，避免拔插后设备名复用。
查询前后的挂载 ID／来源需一致，最终固定目录句柄也需匹配已批准挂载。

批准信息只作为不可变 guard 附属于当前固定 `Root`。`CheckIdentity` 重新逐级固定原路径，
先复查原 pin 仍有效，以 `fstatfs` 核对两个句柄的原挂载 ID、来源及本地标志，再比较原目录身份；逐文件操作不重复启动
`diskutil`。子目录继承 guard，每次进入都核对句柄所属挂载。挂载于项目内部的另一卷
（包括另一块内置卷）会被拒绝，扫描不返回部分清单。

2026-10-04 本机 ARM64 的 200 次 pinned identity 基准约为 139 µs/次；这是当前目录深度及
机器的观测值，不是平台性能承诺。新根开始时的只读系统查询仍有约数百毫秒成本。

同机真实 APFS 临时目录的 1 万个 1 KiB 文件（100 个目录、10,240,000 字节）扫描基线：
新根核验 0.398 秒，完整扫描并验证全部文件哈希 1.301 秒，约 7,688 文件/秒、7.51 MiB/秒。
扫描期间采样 heap 峰值约 4.66 MiB，进程 peak RSS（含造数）约 13.56 MiB；累计分配约
367.4 MiB，说明仍有逐文件哈希缓冲区的分配成本。文件在扫描前刚生成，操作系统缓存为热；
这不是冷盘、百万文件、并行 IDE 修改或网络端到端性能结论。测量仅使用自建临时文件并已清理，
原始结果保存在本机 `/tmp/agentbox-mac-volume-10000-scan.json`。

## 验证范围

- 策略用例覆盖内部 APFS、Intel SATA／PCI-Express、合成 APFS、Fusion 全 store、USB
  固定／可移除盘、弹出介质、虚拟盘／磁盘映像、网络卷、未知／缺失元数据、取消、异常回包。
- macOS ARM64 全部 `syncfs` 用例及本机内部 APFS 原生检查通过。
- 真实临时 HFS+ 磁盘映像验收通过：不能作为根，嵌套挂入已打开的内部项目也被拒绝；验收
  自建 32 MiB 临时镜像，结束卸载并删除，不格式化或修改现有磁盘。运行方式：

  ```sh
  AGENTBOX_TEST_DARWIN_DISK_IMAGE=1 go test ./internal/syncfs \
    -run TestDarwinDiskImageAndNestedMountRejected -count=1 -v
  ```

- Darwin amd64、Windows amd64、Linux arm64 的 `syncfs` 测试程序交叉编译通过，不能据此
  声称 Intel／Windows 实跑完成。USB／Thunderbolt 实体设备拔插与 Intel 实机仍需验收。
- Linux 原有网络文件系统检查和 Windows 固定本地盘检查保持原策略。Linux 可移动块设备的
  识别不在本次 macOS 修改范围内；不因测试容器的 overlay/tmpfs 改写受控 Linux 测试限制。

这些检查不是外部 IDE 的事务锁，也不把管理员可修改内核挂载状态的环境视为安全边界。

## Linux 整引擎容量故障证据

2026-10-04，本机 Docker 的 Linux ARM64 内核实跑
`TestSyncEngineRealENOSPC` 通过（0.50 秒）。测试位于
`internal/server/client_engine_enospc_linux_test.go`，使用实际 Engine、SQLite 和回环 TCP Go
服务端路由。它不仅测试 Writer，也检查持久化意图、HTTP 请求以及核对／重新规划后的恢复。

故障只能在显式批准的独立 tmpfs 上制造。入口同时要求
`AGENTBOX_ENGINE_FULL_TEST=1`、`AGENTBOX_ENGINE_FULL_TEST_ROOT` 为无链接的绝对目录、
执行用户为容器内 root、文件系统为 tmpfs、容量不超过 8 MiB、与父目录属于不同挂载且初始为空。
每次填充前再次检查挂载身份，并限制最多写入 8 MiB。填充文件使用真实非稀疏写入，必须观察到
内核 `ENOSPC` 且该挂载可用块为零才继续测试；没有注入错误、修改 SQLite 页数限制或填宿主盘。

本次使用 4 MiB 故障 tmpfs，观察到以下结果：

- **prepared 意图无法保存**：先填满状态库所在挂载，再执行上传。真实写入 4,165,632 字节后
  得到 `ENOSPC`；SQLite 返回 `SQLITE_FULL`（13）。没有 pending 或基线变更，HTTP apply
  请求为零；本地新内容与服务器旧内容均保留。释放填充文件后可正常预览并上传。
- **started 意图无法保存**：通过只读进度观察到 prepared 已提交后填满状态盘，同样观察到
  内核 `ENOSPC` 和 SQLite 13。已保存的操作仍为 prepared，HTTP apply 为零。释放空间后
  pending 仍阻止盲目重放，核对结果为 not_attempted；显式 replan 后新的确认上传成功，最终
  只有一次 HTTP apply。两个场景都用独立只读 SQLite 连接检查完整数据库及已提交状态，
  `PRAGMA integrity_check` 为 ok。
- **真实 HTTP 下载期间项目盘满**：服务端收到下载请求时先从 SQLite 核对 started 已提交，
  再把项目所在 tmpfs 填满。实际填入 4,190,208 字节；客户端确已读取 3,757 字节 HTTP 响应，
  随后本地写入返回 `ENOSPC`。原文件、started pending 和旧基线保留。释放空间后核对显示
  目标仍为 before，错误的 finish 被拒绝；显式 replan 后再次下载成功，并能导出逐字节一致的
  原内容恢复副本。全程没有远端 apply；共两次真实下载请求。

HTTP 已读取字节数取决于传输分段，测试只要求大于零，不把读取进度当成发布成功。进度／HTTP
观察仅用于协调填充时点，不返回模拟错误。上述结果证明受控文件系统容量耗尽时的行为，
**不代表物理断电、存储设备断连或 Windows 磁盘满验收**。

在 Linux 构建测试程序后，可用以下隔离方式复现（Linux 主机架构决定测试二进制架构）：

```sh
CGO_ENABLED=0 go test -c -o "$RUNNER_TEMP/engine-full.test" ./internal/server

docker run --rm --read-only --network none \
  --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  --security-opt no-new-privileges --user 0:0 \
  --tmpfs /tmp:rw,nosuid,nodev,size=128m,mode=1777 \
  --tmpfs /sync-full:rw,nosuid,nodev,size=4m,mode=0700 \
  --mount "type=bind,src=$RUNNER_TEMP/engine-full.test,dst=/probe,readonly" \
  -e AGENTBOX_ENGINE_FULL_TEST=1 \
  -e AGENTBOX_ENGINE_FULL_TEST_ROOT=/sync-full \
  alpine:3 /probe -test.run '^TestSyncEngineRealENOSPC$' \
  -test.count=1 -test.timeout=2m -test.v
```

服务端夹具沿用真实 workspace UID/GID 规则，容器明确授予上面列出的 CHOWN 等文件能力；
root 文件系统与测试程序挂载只读，普通夹具与故障卷使用两个独立 tmpfs，网络仅使用容器回环。
