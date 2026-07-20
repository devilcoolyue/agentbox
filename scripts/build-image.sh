#!/usr/bin/env bash
# 构建 agentbox agent 镜像（会话容器用）
set -euo pipefail
cd "$(dirname "$0")/.."
docker build -t agentbox-agent:latest images/agent
