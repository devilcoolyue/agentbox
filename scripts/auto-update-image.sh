#!/usr/bin/env bash
# 检查 npm 上的 Claude Code / Codex 新版本，有更新就重建 agent 镜像。
# 由 agentbox-image-update.timer 每天调用；手动执行也安全（无更新则直接退出）。
# 会话容器不会被打断：运行中的容器继续用旧镜像，下次停止再启动时换新。
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=agentbox-agent:latest

npm_latest() {
  curl -fsSL "https://registry.npmjs.org/$1/latest" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])'
}

latest_claude=$(npm_latest '@anthropic-ai/claude-code')
latest_codex=$(npm_latest '@openai/codex')

image_label() {
  docker inspect -f "{{ index .Config.Labels \"$1\" }}" "$IMAGE" 2>/dev/null || true
}

cur_claude=$(image_label agentbox.claude-code)
cur_codex=$(image_label agentbox.codex)

if [[ "$cur_claude" == "$latest_claude" && "$cur_codex" == "$latest_codex" ]]; then
  echo "image up to date: claude-code $cur_claude, codex $cur_codex"
  exit 0
fi

echo "rebuilding image: claude-code ${cur_claude:-?} -> $latest_claude, codex ${cur_codex:-?} -> $latest_codex"
docker build --pull \
  --build-arg "CLAUDE_VERSION=$latest_claude" \
  --build-arg "CODEX_VERSION=$latest_codex" \
  --label "agentbox.claude-code=$latest_claude" \
  --label "agentbox.codex=$latest_codex" \
  -t "$IMAGE" images/agent
echo "done: $IMAGE now has claude-code $latest_claude, codex $latest_codex"
