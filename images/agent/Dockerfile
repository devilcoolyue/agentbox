# agentbox agent 镜像：内置 Claude Code 与 Codex CLI 的最小开发环境。
# 会话容器统一以 uid 1000 (agent) 运行，宿主机挂载目录也按此属主。
ARG BASE_IMAGE=node:22.23.2-bookworm-slim@sha256:48e4b67d85f87bd551df43704e24d252f56cc5f8e9718841aace50f19948f0f9
FROM ${BASE_IMAGE}

RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl wget ca-certificates procps ripgrep jq unzip zip less \
      vim openssh-client python3 build-essential tmux \
    && rm -rf /var/lib/apt/lists/*

# 会话 home 是宿主机的空目录挂载，不能把默认配置只放进 /home/agent。
# 系统配置先加载，用户自己的 .bashrc / .vimrc 仍可覆盖。
COPY bashrc /etc/agentbox.bashrc
RUN printf '\n. /etc/agentbox.bashrc\n' >> /etc/bash.bashrc
COPY vimrc /etc/vim/vimrc.local

# 终端持久化：网页终端 exec 统一 attach 到 tmux 会话（见 termws.go），断线重连后
# claude/codex 原样还在。配置追求"隐形"，用户无感知（无状态栏、无前缀键）。
COPY tmux.conf /etc/tmux.conf

# 默认版本与 versions.env 保持一致；追新需显式启用。
ARG CLAUDE_VERSION=2.1.280
ARG CODEX_VERSION=0.145.0
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_VERSION} @openai/codex@${CODEX_VERSION} \
    && npm cache clean --force

# claude-hud 状态栏（自包含 dist，无运行时依赖）；会话 home 里的
# .claude/settings.json 由服务端在会话初始化时指向这里
COPY claude-hud /opt/claude-hud

# node 基础镜像自带 uid 1000 的 node 用户，换成 agent
RUN userdel -r node && useradd -m -u 1000 -s /bin/bash agent

# CLI 装在 /usr/local（root 属主），agent 用户升级不了；版本统一靠重建镜像
ENV HOME=/home/agent \
    LANG=C.UTF-8 \
    DISABLE_AUTOUPDATER=1

WORKDIR /workspace
CMD ["sleep", "infinity"]
