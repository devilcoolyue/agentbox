#!/usr/bin/env bash
# Experimental opt-in CLI updates. Stable installations use build-image.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "${AGENTBOX_AUTO_UPDATE:-0}" != 1 ]]; then
  echo "自动追新默认关闭。确认要使用最新 CLI 时设置 AGENTBOX_AUTO_UPDATE=1。"
  exit 0
fi
npm_latest() {
  curl -fsSL --connect-timeout 10 --max-time 60 "https://registry.npmjs.org/$1/latest" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])'
}
image=${AGENTBOX_IMAGE:-agentbox-agent:latest}
latest_claude=$(npm_latest '@anthropic-ai/claude-code')
latest_codex=$(npm_latest '@openai/codex')
image_label() {
  docker inspect -f "{{ index .Config.Labels \"$1\" }}" "$image" 2>/dev/null || true
}
if [[ "$(image_label agentbox.claude-code)" == "$latest_claude" && "$(image_label agentbox.codex)" == "$latest_codex" ]]; then
  echo "image up to date: claude $latest_claude, codex $latest_codex"
  exit 0
fi
AGENTBOX_CLAUDE_VERSION=$latest_claude AGENTBOX_CODEX_VERSION=$latest_codex ./scripts/build-image.sh
# Keep old version tags and layers for an operator-controlled rollback.
