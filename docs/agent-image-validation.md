# 候选 Agent 镜像行为验证

M4-04 的开发源码能力，尚未发布。镜像版本号检查和行为验证分别记录；通过合成检查不会把候选版本自动提升为[已验证基线](compatibility.md)，也不证明真实账号、OAuth、任意模型或第三方 MCP 可用。

## 更新路径

网页手动更新、每日自动更新以及回退共用同一门槛：

1. 检查当前镜像与策略；更新时在当前不可变镜像 ID 上构建独立候选，保留浏览器和定制层。
2. 核对候选 CLI 版本与浏览器标记，按候选 **image ID** 创建隔离验证容器。
3. 验证实际 CLI 行为，并将捕获的事件交给现有 Go Adapter/用量解析器检查。未知终结或用量格式使候选失败，不按 0 消耗当作通过。
4. 保存并同步验证结果文件和父目录，再通过 `Config.SwitchAgentImage` 比较原镜像/策略并切换到验证过的不可变 ID。验证期间修改配置、取消、报告不匹配/缺项、保存或清理失败都不切换。

运行中的空间继续使用原容器；停止再启动后才采用新镜像。回退也验证历史镜像，并在成功后暂停自动更新。没有新版可构建时不重复验证；“立即检查”只读取版本信息，不显示行为验证通过。手动编辑镜像设置/自行执行 Docker tag 属于管理员直接配置，不能据此获得已验证声明。

## 11 项检查

| 检查 | 实际执行与判断 |
| --- | --- |
| Claude 完成、续聊、中断 | 使用产品 headless argv 和 PID 包装；续聊使用实际 session ID，检查前轮回复进入下一请求；保持上游未结束后发送 SIGINT，要求退出信号/130 或失败终结证据 |
| Claude MCP | 临时 stdio MCP 经 initialize/tools/list/tools/call，合成上游触发实际 echo 调用；检查工具调用记录 |
| Claude 用量 | 实际 assistant/stream_event 输入原消息 ID 去重解析器，核对输入/缓存/输出合成计数；不使用累计 result 报价代替单轮用量 |
| Codex 握手、完成、续聊、中断 | 实际 initialize/thread/start/thread/resume/turn/start/turn/interrupt；续聊必须保留线程与前轮内容，中断须有 interrupted 终结；捕获的 RPC 经现有 Go app-server 驱动器回放检查 |
| Codex exec | 使用产品 exec argv，要求成功退出、回复标记与终结事件 |
| Codex 用量 | app-server/exec 输出经现有归一化解析，检查未缓存输入、缓存读、输出；reasoning 不重复计入输出 |

归一化结果仅写入内存记录器，不接入实例 store、usage_events 或额度账本。未识别事件会产生 `usage_contract_failed` / `terminal_contract_failed` 等固定代码，并保留具体检查阶段。服务日志的 `image_validation_failed` 只记录候选 ID、协议版本、阶段和代码，不输出原始 CLI stderr/提示词。

## 隔离、期限与清理

验证容器无外网（network=none）、不挂 Docker socket、账号、工作区或宿主目录；固定 UID/GID 1000、只读根文件系统、drop ALL capabilities、no-new-privileges、init。候选镜像的自定义 ENV 被清空，子 CLI 再使用显式白名单与合成凭证；home/workspace/tmp 均为临时目录。带声明式 VOLUME 的镜像拒绝探测，避免隐式挂载改变边界。

每次验证限 4 分钟，容器内单进程与握手另有限时；限制 1 GiB 内存、2 CPU、128 PID、tmpfs 容量和输出大小。服务原更新任务仍有 30 分钟总期限。正常结束、错误和取消均清理本次容器/匿名卷；清理错误作为失败返回，不能激活。硬杀服务进程可能绕过清理，遗留容器带 `agentbox.cli-probe=1` 标签，应核对归属后处理，不能全局 prune。

## 结果与独立命令

管理员 `/api/image-updates` 的 status 新增 `validation`：协议 version=1、image_id、checked_at、checks，以及可选 failure/stage。只持久化白名单结果，不保存原始 CLI 事件。网页三语显示未验证/验证中/通过/失败，并保留版本检查与真实账号可用性的区别。

可用新版二进制单独检查本地镜像，不需配置文件、数据库或运行中的服务：

```sh
./agentbox --check-agent-image agentbox-agent:claude-2.1.280-codex-0.145.0
```

命令输出 JSON，失败返回非零；不会拉镜像、修改配置或切换当前镜像。使用 flag 形式是为了让旧二进制在初始化前拒绝未知参数，避免被误当成启动服务。

旧 `scripts/auto-update-image.sh` 仍默认关闭。显式启用后先构建唯一候选标签，调用上述验证命令，最后才按已验证的 image ID 更新原标签；`AGENTBOX_VERIFY_BINARY` 可指定新版二进制，缺失/不支持时保留旧镜像。切换前复查旧标签是否变化；Docker 标签没有 CAS，不能让这个旧脚本与其他镜像写入者并发执行。网页管理实例继续停用旧 timer。失败候选和旧镜像保留供排查/回退，不自动 prune。

## 验证与证据

```sh
python3 scripts/verify.py run --step docker.cli-candidate \
  --image agentbox-agent:claude-2.1.280-codex-0.145.0 --timeout 300
python3 scripts/verify.py run --step ui.image-update-gate \
  --playwright-module /已安装目录/node_modules/playwright/index.mjs
```

普通 Go 测试包含协议/用量格式变异、缺项、跨镜像报告、取消、配置并发修改、持久化失败、回退拒绝和 Docker 隔离/清理回归。`policy.image-policy` 用合成命令验证旧脚本不能在验证前覆盖当前标签。`release-full` 已包含候选检查；Release candidate 工作流在构建固定运行时后执行同一门槛并保留失败报告，当前没有触发远端工作流。

本轮本地报告及适用范围见 [M4 记录](milestones/m4.md)。
