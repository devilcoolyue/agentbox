# 真实 systemd 升级与恢复夹具

从仓库根目录执行：

```sh
docker build -t agentbox-recovery-systemd:local scripts/fixtures/recovery-drill
python3 scripts/verify.py run --step docker.recovery-drill \
  --recovery-image agentbox-recovery-systemd:local --timeout 600

# 自定义合成规模；单独入口使用 --image
python3 scripts/test-recovery-drill.py --image agentbox-recovery-systemd:local \
  --sessions 32 --files-per-session 16 --file-bytes 4096 \
  --backup-interval-seconds 86400
```

需要 Python 3、Go、Git、可用的 Linux Docker daemon（private cgroup 支持）以及本地冻结提交 `eb845db59e45ed042b1af9df44dae96f104bf43b`。镜像构建下载 Ubuntu 22.04 的 systemd/Python；运行不拉取 Agent 镜像或调用模型。真实会话执行/旧客户端兼容分别由聊天联合矩阵与 `desktop.compat` 覆盖。

脚本构建冻结 schema 9 与当前 Linux 二进制，记录源码摘要、二进制哈希、不可变镜像 ID。v0.0.1-drill/v0.0.2-drill 仅为夹具版本标签，不是公开发布版。schema 目标取自 checkout 的 `store.SchemaVersion`，不是已迁移数据库。

## 资源与隔离

每次创建唯一 `abox-recovery-<run>` 容器：privileged、private PID/cgroup、network=none、768 MiB 内存、256 PID 上限和临时运行目录；PID 1 必须为真实 `/lib/systemd/systemd`，内部入口还校验 Docker 标记与 run ID。使用真实 `release.stage/activate/unit` 和 systemctl，不执行宿主 systemctl。

挂载 `/var/run/docker.sock` 供产品原有完整备份挂载检查使用；这意味着测试代码拥有 daemon 权限。只在受控开发/CI daemon 上执行。没有宿主应用数据、凭证、systemd 或 cgroup 目录挂载；源码、程序与脚本以 docker cp 放入自有容器。合成数据只在标记过且停服的夹具库中写入。最终仅删除本次容器，不使用 prune；清理失败为测试失败。

## 检查与报告

检查真实 systemd 升级、PID 变化的重启、待处理聊天转 uncertain、在线系统备份、在线完整备份锁拒绝、离线完整备份、旧版激活拒绝、拒绝覆盖恢复目录，以及系统/完整恢复各自启动健康。比对所有业务表的规范摘要、账本余额、凭证路径重定位与普通文件 SHA-256/mode/UID/GID。系统备份明确排除项目、聊天历史、共享目录内容；完整备份包含这些文件。备份后新增一条用量/账本及一个文件，断言恢复副本均没有这些新增数据。

统一报告在 `output/verification/<run>/`，详细证据在 `output/verification/recovery-drill-<run>/`：`report.json` 保存构建来源/容器清理，`scenario.json` 保存规模、逐步耗时、核对结果及 systemd 日志。失败证据保留，不覆盖旧目录；测试日志不是用户脱敏诊断。

“恢复耗时”只合计校验备份、恢复至新目录、启动至健康，不含发现故障、决策或异机传输。`--backup-interval-seconds` 是排程假设；窗口估计以备份持续成功为条件，不是约定 RPO。2026-10-05 本机 Linux arm64/systemd 249 的 32 空间、611 文件、2,099,370 文件字节演练：系统恢复 1.326332 秒，完整恢复 2.963020 秒。生产规模/磁盘/网络与管理员 RTO/RPO 仍需独立验收。
