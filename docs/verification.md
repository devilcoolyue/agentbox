# 统一验证入口与证据

[当前能力清单](capabilities.md) · [开发指南](development.md) · [里程碑记录](milestones/m1.md)

统一入口是 `python3 scripts/verify.py`，命令目录在 `scripts/verification_catalog.py`。它调用已有测试，不用新的浅层断言替代原回归；原脚本仍可单独运行。新增 `scripts/test-*` / `desktop/scripts/test-*` 文件必须登记为可运行项、组合测试的子模块或外部资源验收，目录自测会拒绝遗漏。

## 常用命令

准备 Python 3.10+、Go（版本见 go.mod）和 Node/npm；首次安装网页依赖用 `npm ci`。工具不会自动安装依赖或启动 Docker。

```bash
# 列出配置组、命令、资源类别和外部验收入口
python3 scripts/verify.py list
python3 scripts/verify.py list --json

# 仅预览命令，不执行、不生成“通过”报告
python3 scripts/verify.py run quick --dry-run

# 网页类型/完整产物/契约 + Go + 本地部署策略回归
python3 scripts/verify.py run quick

# 单独执行一组或几个检查
python3 scripts/verify.py run web
python3 scripts/verify.py run go
python3 scripts/verify.py run policy --step audit.licenses

# Playwright 安装步骤见开发指南；也可沿用已有两项环境变量
python3 scripts/verify.py run browser \
  --playwright-module /本机依赖目录/node_modules/playwright/index.mjs \
  --browser-channel chrome

# 只检查桌面前端和合成策略，不启动原生应用/安装包
npm --prefix desktop ci
python3 scripts/verify.py run desktop-unit
```

默认结果保存在 `output/verification/<UTC时间>-<随机ID>/`。用 `--report-dir 新目录` 指定其他位置；目录已存在会拒绝，避免覆盖失败证据。`--timeout 秒数` 可限制每条命令的时间，默认沿用子测试自身超时，不把复杂集成测试强行截成快速检查。

## 配置组与资源边界

| 组/入口 | 实际执行范围 | 不包含 |
| --- | --- | --- |
| `pr` | 与 quick 相同的快速 PR 入口；耗时以实际报告为准 | Docker/原生/人工门槛 |
| `linux-integration` | 定向 race（含 Shutdown）＋真实 Linux 聊天可靠性；需 --image 与 Playwright | 真实 CLI/provider 和 systemd |
| `release-full` | quick＋race/backup/聊天集成/候选 CLI/真实 systemd 恢复＋browser＋desktop-unit＋release；显式提供 Agent --image、--recovery-image 及全部候选资源 | 正式签名、公证、设备/付费/人工与未列入的专用矩阵 |
| `quick` | `web` + `go` + `policy` | 浏览器、Docker、原生安装、付费模型；“quick”不是固定 runner 的耗时承诺 |
| `web` | TS 类型、重新构建及产物字节/集合一致性、语言/错误/诊断及版本化聊天契约、草稿/待确认存储、历史/发送/设置生命周期 | 浏览器布局或真实服务端 |
| `go` | build/vet、除 server 外的包、Linux EPERM 闸门、完整 server 包 | 显式 opt-in 的 live/native 故障注入；实际跳过列表进入报告 |
| `policy` | 模拟部署、更新、安装/卸载、浏览器代理、镜像策略、用量修复和入口自测 | 真实 systemd、生产文件或真实安装器 |
| `browser` | 主控制台、首次使用、草稿/待确认恢复、百空间搜索筛选、Git、文件预览、网页/abox-link 三语；真实浏览器、合成 API | 真实 Docker、模型、桌面 GUI |
| `--step integration.chat` | 本机真实 Go 服务/SQLite/API/WS＋浏览器；执行端为合成 Docker/CLI | 非 root macOS 成功附件上传不适用（验证真实 chown 拒绝），由 Linux 矩阵覆盖 |
| `--step docker.chat-reliability` | 独立 Linux 服务进程/卷＋真实浏览器，崩溃/重启、确认丢失、附件及账本 | 合成执行端，不运行真实 CLI/provider，不挂 Docker socket；需显式本地测试镜像 |
| `--step docker.cli-candidate` | 同生产候选门槛，真实本地 CLI＋合成上游；network=none，清理本次容器 | 真实提供方/账号、npm 安装或修改当前镜像 |
| `--step ui.image-update-gate` | 候选状态/更新/回退专项，含诊断/错误前置场景、三语/窄屏；合成 API | Docker/实际更新 |
| `docker-core` | 文件系统/播种/服务端 Linux 回归、管理员恢复/PTY | 生产卷；测试镜像构建可能需依赖网络，运行容器禁外网 |
| `desktop-unit` | Vue/TS 构建、Vitest、候选/更新清单、原生/跨系统测试编排器的合成回归 | 正式安装、keyring、实机 GUI、原生 Rust 构建 |
| `release` | 已有发布包验证、隔离 Linux 服务/重启/安装验收（systemctl 为模拟） | 构建发布包、发布、生产部署、签名、公证 |
| `--step desktop.rust` | 当前 OS SDK 上的 Rust 单元测试，真实凭证库 opt-in 关闭 | 正式签名/更新或实体设备验收 |
| `--step audit.licenses` | vendored 哈希、实际链接模块与许可证目录 | 新一轮漏洞/密钥扫描或法律授权结论 |

“offline”表示测试使用本地/loopback 合成夹具且不调用模型，不保证在尚未准备 Go/Node 依赖的机器上完全断网也能安装工具链。缺工具、依赖、镜像、二进制或其他参数时会失败/阻塞并记录，不能以静默跳过代替检查。

Linux `go` 组保留生产属主约束：先以不能 chown 到 `1000:1000` 的普通用户跑 `go.chown-denial`，再由普通用户编译、以 `go test -exec 'sudo -n --'` 执行完整 server 测试。需要预先配置对应测试环境的非交互 sudo。root 不能证明真实 EPERM，因此整组会在该闸门阻塞；UID 1000 若本来有 chown 能力，该用例也会明确失败。不要为得到绿色结果降级生产权限检查。macOS 将这个 Linux 专属闸门记录为 `not_applicable`，不是已验收 Linux。

## Docker 与发布材料

资源参数必须显式提供，路径按调用者当前目录解析为绝对路径，采用 argv 传递，不拼 shell：

```bash
python3 scripts/verify.py run --step docker.reasoning --step docker.mcp \
  --image agentbox-agent:claude-2.1.280-codex-0.145.0

python3 scripts/verify.py run --step docker.mcp-server \
  --binary /临时构建/agentbox-linux --image agentbox-agent:latest

python3 scripts/verify.py run release \
  --artifacts /候选发布目录 --binary /临时构建/agentbox-linux \
  --image agentbox-agent:latest

python3 scripts/verify.py run --step docker.transparent \
  --binary /临时构建/agentbox-linux --client /临时构建/abox-link-linux \
  --fixture /临时构建/内网测试程序 --image agentbox-agent:latest \
  --network-image agentbox-network:dev
```

具体镜像、平台和夹具构建条件仍以原脚本及 [CLI 兼容矩阵](compatibility.md)、[透明网络验收](architecture/transparent-network.md) 为准。命令目录逐项标注资源，不能把“未提供这些资源”误读为对应能力已验收。`docker.reasoning` / `docker.mcp` 用真实 CLI 加模拟上游，不消耗模型额度。

原生 Smoke.app、Windows 安装器、跨 OS peer、磁盘镜像故障测试以及真正付费/联网检查在 `list` 的外部入口中列出，保持原参数和隔离要求，不提供自动 `all` 模式。特别是 `CODEX_LIVE_TEST=1` 会真实调用已登录的模型，需要单独明确授权；正式签名、最低系统、真实 IME/拖放/休眠和试用期需要设备/参与者，不能用合成脚本替代。

## 如何读报告

每个运行目录包含 `report.json` 和每步独立日志，运行中也会原子保存状态。记录开始时间、平台/工具版本、实际命令及受控环境覆盖、耗时、退出码、Git 修订和运行前后工作树摘要。

| 状态 | 含义 |
| --- | --- |
| `passed` | 该命令退出 0，仅证明命令标注的范围 |
| `failed` | 已运行且退出非零；查看对应日志 |
| `blocked` | 缺少必要工具/参数/身份，未执行该命令 |
| `not_applicable` | 当前平台不适用，不能算另一平台通过 |
| `timed_out` / `cancelled` | 执行未完成，不能算通过 |
| `not_run` | 前序失败或取消后未再执行 |

任何失败、阻塞、超时、取消都返回非零，整份报告标记 `incomplete`。Go JSON 中的测试 `skip` 单独记录名称与计数，`cached_packages` 标出复用 Go 测试缓存的包；即使命令退出 0，也不证明这些用例已跑过。平台不同、资源不同的报告不能直接合并成“完整矩阵通过”。

工具会清除继承的 `AGENTBOX_*` 测试/辅助进程开关、`CODEX_LIVE_TEST`、`MARKET_LIVE_TEST`、部分浏览器选择开关、`GOFLAGS/GOOS/GOARCH` 等，只保留浏览器依赖路径/渠道，再应用当前目录条目明确指定的环境变量，避免一次普通运行意外变成付费或部分测试。需要原始 opt-in 测试时走列出的独立入口。

网页产物检查以运行前的工作树为基准，不要求开发中所有文件已提交。它同时核对重新构建前后的完整文件集合/内容，以及每个 TS 源对应的 JS；发现差异会失败并保留实际构建产物供审查。它不自动提交文件，也不允许已删除源码的遗留 JS 混过去。

`source_changed=true` 表示运行期间工作树发生变化，需核对改动范围，不能声称最终树的每一行都已被该轮测试覆盖。source 摘要指运行测试脚本的工作树，不证明 `--binary` 或 `--artifacts` 一定由它构建；候选版本仍需看发布脚本的清单、哈希及构建出处。

取消会先向所启动的进程组发中断信号，给脚本清理时间，再有界终止。硬终止不证明 Docker/原生外部资源一定收尾完成；核对选定脚本的日志和资源，不进行全局 prune。验证日志可能包含本机路径和测试输出，**不是**面向用户的脱敏诊断报告。本地报告不会自动上传；CI 只上传对应 job 的验证目录，原有密钥扫描的私有输出单独处理。

CI 的网页、Go、部署策略和许可证检查已改为调用同一入口，并在失败时保留结果。Docker、原生、发布包和秘密扫描工作流仍保持各自资源及权限边界；M4 已提供三层命名组及定向 race，本机固定 Linux runner 的连续 20 次已实测通过，托管 CI 采样仍待运行。新增 CI 未在本轮触发；首批实际证据见 [M4](milestones/m4.md)。


## M3 联合可靠性

真实服务进程＋浏览器的可重复矩阵见 [fixture 说明](../scripts/fixtures/chat-integration/README.md) 与 [M3 记录](milestones/m3.md)。先通过 `web.artifacts`，测试再构建并使用原二进制内嵌前端；不要在构建/浏览器读产物期间重编译 TS。

```sh
python3 scripts/verify.py run --step docker.chat-reliability --image alpine:3.22 \
  --playwright-module /本机依赖目录/node_modules/playwright/index.mjs --timeout 600
```

该入口新建并清理自己的测试容器、网络和卷，不触碰其他容器；信号中断也进入清理，清理失败报告为失败。合成日志与逐回合回执/用量/账本保留在 `output/playwright/chat-integration-*/`。CI 的独立 `chat-reliability` job 同时保存这些材料与统一报告；新增工作流不等于已在托管 CI 跑过。

浏览器失败报告同时保留页面异常、最近 100 条失败请求的路径（不含查询串）和登录/主界面的隐藏状态；只记录令牌是否存在，不记录令牌值。这些仅是隔离夹具证据，不是生产诊断导出。`browser.chat-outbox` 的跨线程场景主动挂起新建响应，确认旧编辑器仍可就绪后才释放响应；必须等新线程历史应用完成再检查旧消息不可重试，不能以输入框可见代替线程切换完成。

## 版本化契约

`contracts/chat-errors-v1.schema.json` 仅覆盖本轮高频变动的错误和持久聊天；`python3 scripts/generate-contracts.py` 生成 TS 声明，`web.contracts` 检查是否同步生成及运行时的正反例，Go `internal/protocol` 核对真实 JSON 字段/状态/封装。桌面 Rust 与 Vitest 也读取错误目录样本。它不生成全量 OpenAPI，不用 schema 版本替代能力协商或数据库 schema。

冻结升级入口 `desktop.compat` 的预期 schema 默认读取 checkout 的 `store.SchemaVersion`；直接用原脚本检验其他候选时可传 `--expected-schema`。预期不能取自已经迁移的数据库，否则未执行迁移也会被误判通过。失败的冻结网页留截图及不含 token 的错误状态，旧客户端实际 API/WS 始终连接隔离真实 Go 服务。

## 真实 systemd 恢复演练

```sh
docker build -t agentbox-recovery-systemd:local scripts/fixtures/recovery-drill
python3 scripts/verify.py run --step docker.recovery-drill \
  --recovery-image agentbox-recovery-systemd:local --timeout 600
```

该入口需要 Linux Docker daemon 和预构建镜像；构建夹具镜像需要下载 Ubuntu 包。`--recovery-image` 与候选 CLI/兼容矩阵的 Agent `--image` 分开，不能互换。脚本构建冻结 schema 9 基线与当前 Linux 二进制，在自有 privileged 容器内使用真正的 systemd PID 1；独立 PID/cgroup、network=none，不挂宿主应用数据、systemd/cgroup 目录。Docker socket 仅用于现有产品的实际挂载检查，但赋予夹具访问 daemon 的权限，需在受控测试环境运行。清理仅针对本次命名容器。

演练检查升级、重启、待处理聊天回执恢复、在线系统备份与离线完整备份、拒绝旧二进制降级、恢复到新目录及启动健康，并核对 SQLite 各表摘要/账本余额与文件哈希/属主。备份后新增记录和项目文件用于显式展示恢复丢失窗口。系统备份不包含项目内容，不能用其计时表示完整项目恢复。详细参数、报告和测量口径见 [夹具说明](../scripts/fixtures/recovery-drill/README.md)。

`release.deployment` 仍是实际 Linux＋模拟 systemctl；`docker.recovery-drill` 才是真实 systemd 自动化。Release 工作流已接入后者，新增工作流尚未在托管 CI 执行。

## 固定 runner 稳定性采样

```sh
# 同一次 Linux runner 分配内执行；要求普通用户及非交互 sudo 满足 Go 属主测试
python3 scripts/verification_stability.py --runs 20 \
  --output output/verification/stability-local
```

手动工作流 `.github/workflows/verification-stability.yml` 在 Ubuntu 24.04、Node 22 和 go.mod 工具链上执行同一命令，并加 `--require-ci`。安装工具/依赖时间不计入 PR 检查时间。Go 测试用 `-count=1` 禁用结果缓存，允许编译缓存预热。每次完整 PR profile 均写独立报告，失败不替换重跑；汇总记录成功样本的中位数/P95/最大值、全部样本的失败率，以及出现不同结果的步骤（仅供调查，不能直接断言为偶发缺陷）。

少于 20 次、非 Linux、源码/工具/平台变化、缺步骤、存在未运行项、Go 结果缓存或零执行测试，以及任一次超过 600 秒，均不能获得通过验收。取消、缺条件或源码变动提前结束，仍保留已有样本。完整聚合测试 `policy.verification-stability` 只验证这些规则；2026-10-05 已在本机固定 Linux arm64 runner 完成 20 次真实样本：中位 47.265 秒、P95 141.524 秒、最长 381.923 秒、失败率 0。完整报告为 `output/verification/stability-20261005/`，见[交付记录](milestones/delivery-20261005.md)；远端工作流尚未触发。
