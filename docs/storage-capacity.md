# 本地存储容量管理

[存储里程碑](milestones/storage.md) · [共享工具](shared-tools.md) · [部署布局](architecture/deployment-layout.md)

本功能面向 Linux 单节点、独立发布目录部署。默认关闭；不接入 S3，也不自动迁移用户数据。

当前升级边界：配额部署所需的 systemd 存储附加配置会触发现有页面升级器对 DropInPaths 的拒绝；生产实测一键升级不可用。存储能力兼容门槛及通用页面迁移流程也尚未接入，见[里程碑 S1-09](milestones/storage.md#s1-09在线升级兼容与存量安装迁移待补)。

## 数据盘与硬配额

`storage_policy` 是操作员配置，修改后需重启。活跃数据和工具缓存必须位于指定挂载点下；启动在创建数据目录或打开数据库前核对挂载、文件系统 UUID 和项目配额能力。数据盘缺失时失败退出，不向系统盘同名目录写入。

| 字段 | 含义 |
| --- | --- |
| `mount` / `uuid` | 实际 ext4/XFS 挂载点及文件系统 UUID，不能是根分区；需 `prjquota` |
| `user_bytes` / `users` | 默认用户持久数据硬上限及按用户名覆盖，单位 bytes |
| `cache_bytes` / `cache_users` | 默认私有缓存硬上限及用户覆盖 |
| `budget_bytes` | 全实例已分配数据/缓存硬限额的总预算；0 不限制分配总量，生产建议明确配置 |
| `cache_max_age_days` | 可重建缓存的过期天数，0 不按期限清理 |
| `cache_target_bytes` | 可重建缓存的目标大小；0 时采用该用户缓存限额的 75% |
| `release_keep` | 发布目录保留最近 N 版；0 关闭，启用时至少 2 版 |

预算需要扣除 Docker、swap、公共工具、备份、上传暂存和磁盘保留空间。新用户分配超出总预算时拒绝准备其存储，不能静默退化为无限额；旧用户删除后保留的数据/缓存和项目记录继续占用预算，由操作员核对退役，不分配给同名新用户。

每用户持久数据和私有缓存使用不同的 filesystem project ID，身份包含实例及用户创建时间。同一 UID 1000 的不同用户仍独立计量。字节硬限额之外还设置 inode 上限（10,000～1,000,000，按容量计算），避免小文件耗尽整盘。管理员同样受限制，按需设置覆盖值。

普通用户可以清除 ext4 的 project inheritance，故容器同时阻止 `FS_IOC_FSSETXATTR` 和两种 ABI 的 `FS_IOC_SETFLAGS`，比较 ioctl 的低 32 位；其他文件操作与 Chromium 的沙箱保留。启用前必须停止旧空间，旧容器下次启动重建后才带保护；发现运行中旧容器时服务拒绝启用。容器里的 chattr 属性变更因此也会被拒绝。

服务端的 root 文件写入也必须受配额限制。systemd 使用 `CapabilityBoundingSet=~CAP_SYS_RESOURCE`，保留设置项目配额所需的能力；启动检查实际继承的 capability，不能只在文档中声明有限额。运行依赖 Python 3、blkid 和支持项目配额的 Linux 内核；缺条件失败，不按“软限额已启用”继续运行。

`resources.min_free_bytes` 继续用于启动准入，并在启用本功能时用于 HTTP 写入的并发空间预留。上传按请求长度为请求、压缩包和解压量预留空间；启用策略时解压上限同时受压缩文件大小的 8 倍（最少 1 MiB）及原有全局上限约束。未知请求长度按配置上限保守预留；用户目录下的私有暂存也计入配额，避免从 project 0 重命名进入用户目录时报 EXDEV。限额不会主动中止对话，但超过硬配额的文件写入会失败。磁盘统计的逻辑大小不等于内核分配块用量，不能把两者混作精确剩余额度。

## 操作流程

现用数据盘未启用 ext4 项目配额时，需先在隔离文件系统验证转换流程，再在维护窗口停用占用该盘的容器、服务和 swap，卸载后配置 quota/project 特性并完成文件系统检查，挂回 `prjquota`。不要在线修改繁忙文件系统或强制卸载。云盘扩容、fstab、systemd 挂载依赖由操作员维护，不由服务端自动改写。

数据迁移须先停服务和相关容器、备份、复制到不存在的新目录，核对字节校验和、权限、属主、mtime、符号链接和数据库；已停止容器仍保存旧挂载路径，要删除这些容器并在启动时重建。原目录保留到验收完成，不同时运行新旧实例。

准备好 `policy.json` 和复制后的目标目录后，用新二进制执行：

```sh
agentbox storage-config --config /etc/agentbox/config.json \
  --policy /path/to/policy.json --data-dir /data/agentbox \
  --min-free-bytes 3221225472
agentbox storage-prepare --config /etc/agentbox/config.json --adopt
```

`storage-config` 占用源/目标数据锁，验证没有运行容器挂载相关数据，再经 Config 的原子变更写回配置；只更新路径，不移动或删除文件，数据根下的凭证/缓存路径一并重定位。调整现有策略时省略 `--data-dir`。`storage-prepare` 在离线状态逐文件分配项目归属，拒绝符号链接跟随、特殊文件和硬链接；中断保留未完成标记，可离线重跑，运行时不接受部分准备的目录。停机后遗留的 CLI socket 需核对已无进程使用再处理，不能通用删除用户文件来通过验收。

systemd 应同时加上 `RequiresMountsFor` / 挂载单元依赖以及上述 capability 限制。迁移后核对真实容器的 workspace/home/shared 源、新建和上传、聊天历史、凭证、UID 1000 配额拒绝及跨用户隔离，再退役旧数据。这里只列流程，不提供可盲目应用于任意主机的磁盘格式化命令。

## 自动维护与监控

- 服务端每小时尝试回收 Go 编译缓存和 npm `_cacache`。持空间启动锁，只有所有已知空间容器都停止时执行，不主动停用户任务；按最新内容 mtime 和扫描大小决策，遍历不跟随链接且有数量/耗时上限。不回收私有模块、浏览器安装、profile、SDK、凭证和项目依赖。长期一直有空间运行时会跳过，硬配额仍生效。
- 独立发布目录可安装包内 `deploy/agentbox-storage-maintenance.service` 与 `.timer`，每天运行 `retention.py --apply`。先去掉 `--apply` 预览；持部署锁，保护 current、最近 N 版、顶层回退符号链接、进程引用及 `--protect <版本>`。备份、未知目录和暂存目录不在自动删除范围；旧部署暂存仍由操作员核对清理。
- 监控按文件系统去重，显示系统盘、用户数据和工具缓存对应盘的水位；同盘不重复计数。容量页按用户及用户数据/备份/公共工具/私有缓存/发布目录分类，显示策略。扫描有界、可能不完整，标记 partial；文件逻辑大小不是删除后一定能释放的容量。S3 尚未接入，不显示虚假的云端占用。
- 备份恢复会移除 `storage_policy`，新实例必须重新核对挂载并准备项目配额。保留旧二进制不意味着可直接回退：旧版不执行容器配额保护，回退需维护窗口和兼容配置/备份。

## 验证

```sh
python3 scripts/verify.py run quick
python3 scripts/verify.py run --step browser.browser
# 显式 Linux root；仅创建、挂载和销毁临时 loop 文件系统
python3 scripts/verify.py run --step linux.storage-quota
# 实际生产 seccomp profile，经 Docker 验证，私有 tmpfs，不挂用户目录
python3 scripts/verify.py run --step docker.storage-quota-profile --image <本地Agent镜像>
```

Linux 专项覆盖无配额 ext4 的离线转换、UID 1000 嵌套写满、继承标记绕过、去除 CAP_SYS_RESOURCE 后的服务端写入、同 UID 跨用户隔离、同名重建及 UUID 不匹配。原生子进程的 seccomp 测试不替代 Docker 配置或真实浏览器验收；生产候选另需使用实际 seccomp profile 的隔离 Chromium 沙箱与 Go 编译测试。
