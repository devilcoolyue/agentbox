# CLI 与运行时兼容矩阵

推理参数回归可运行：

```bash
AGENTBOX_CLI_TEST_IMAGE=agentbox-agent:claude-2.1.280-codex-0.145.0 \
  go test ./internal/agent -run 'TestReasoningCLI' -v -count=1
```

该测试在本机 Linux Docker 容器以 UID 1000 执行，`network=none`，临时 home 和模拟 HTTP 上游只使用合成凭证，不消耗模型额度。覆盖 Claude 原生 effort、固定预算、默认设置继承，Codex app-server / exec 参数及默认继承，并单独验证 `config/read` + `model/list` 协议。它验证 CLI 发出的请求字段，不证明真实 provider 接受任意模型与档位组合。

## 固定基线

| 组件 | 默认版本 | 验证范围 |
| --- | --- | --- |
| Node 基础镜像 | 22.23.2-bookworm-slim，OCI index SHA-256 见 versions.env | 包含 amd64/arm64；固定 digest |
| 终端默认环境 | Debian 12、彩色 Bash / `ll`、完整 Vim | 系统配置不受空 home 挂载影响；真实浏览器验证方向键、退格、冒号命令、保存和 tmux 重连 |
| Claude Code | 2.1.280 | Linux arm64 固定镜像无凭证版本命令；合成会话启动/文件/Git 验证通过 |
| Codex CLI | 0.145.0 | Linux arm64 固定镜像版本命令；终端 rollout 解析按此版本公开结构及合成样本验证；未做本阶段在线推理 |
| claude-hud | 0.5.1 | vendored 文件与上游提交逐字节比对 |
| abox-link | 与服务端相同发布版本 | 先升级服务端；配对依赖 `/api/tunnel/pair/redeem` |

`images/agent/versions.env` 和 Dockerfile 的默认值必须一致。`scripts/build-image.sh` 默认固定 CLI 版本和基础镜像 digest；APT 包仍从 Debian 当前仓库获取，因此它是固定关键运行时的构建配方，不是完全可重现的系统镜像。安全补丁通过有意更新基线、测试和发布引入。

既有 `agentbox-agent:latest` 只是本地兼容别名，不代表构建脚本安装 npm latest。构建同时保留 `agentbox-agent:claude-<版本>-codex-<版本>` 标签，不自动清理旧层。镜像标签仍可被覆盖；需要保留精确镜像时记录 image ID 或自行 `docker save` 归档。

## 客户端镜像更新

v0.1.7 起，管理员可在「系统设置 → 容器与资源 → 客户端更新」手动检查、更新和回退，也可设置系统时区下的每日检查时间。自动更新默认关闭，Claude 默认 stable 渠道，Codex 默认保持当前版本；选择 latest 渠道或同时更新 Codex 需明确启用。基于当前镜像保留浏览器与自定义功能，通过 CLI 版本验证后才切换；更新后的版本不自动成为本表已验证的固定基线。回退会暂停自动更新，运行中的空间停止再启动后使用所选镜像。

## 旧部署的实验性追新

网页已管理镜像更新时，应保持旧 systemd 更新 timer 停用，避免两套调度同时改镜像。旧源码部署的手动追新仍默认关闭：

```bash
AGENTBOX_AUTO_UPDATE=1 ./scripts/auto-update-image.sh
```

源码安装脚本默认禁用每日更新 timer，包括之前已启用的 timer。明确要每日追新的管理员可用 `sudo env AGENTBOX_ENABLE_AUTO_UPDATE=1 ./deploy/install.sh`，或在安装后 `sudo systemctl enable --now agentbox-image-update.timer`。timer 服务显式设置 `AGENTBOX_AUTO_UPDATE=1`。实验版本不自动成为兼容矩阵中已验证的版本；运行中的容器继续使用原镜像。

## 回退镜像

先停止相关会话，再将系统设置中的 `agent_image` 指向保留的旧版本标签或 image ID，然后重启会话。确认 Docker 实际重建为所选镜像并测试续聊/终端。不要为节省磁盘自动 prune 回退所需镜像，也不要只替换 CLI 的宿主机文件就假定已运行的容器更新。

无凭证 `--version`、帮助和协议握手可以在本地验证；真实推理、OAuth 轮换和计费需要单独授权的在线测试。不要将构建成功描述为所有上游行为已验证。
