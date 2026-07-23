# agentbox agent 镜像：内置 Claude Code 与 Codex CLI 的最小开发环境。
# 会话容器统一以 uid 1000 (agent) 运行，宿主机挂载目录也按此属主。
FROM node:22-bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git curl wget ca-certificates procps ripgrep jq unzip zip less \
      vim-tiny openssh-client python3 build-essential tmux \
    && rm -rf /var/lib/apt/lists/*

# 终端持久化：网页终端 exec 统一 attach 到 tmux 会话（见 termws.go），断线重连后
# claude/codex 原样还在。配置追求"隐形"，用户无感知（无状态栏、无前缀键）。
COPY tmux.conf /etc/tmux.conf

# 版本由 scripts/auto-update-image.sh 探测后经 build-arg 钉死：版本一变
# 只有这一层缓存失效；手动 docker build 不带参数则装 latest
ARG CLAUDE_VERSION=latest
ARG CODEX_VERSION=latest
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
