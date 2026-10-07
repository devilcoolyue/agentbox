#!/usr/bin/env bash
# Experimental opt-in CLI updates. Stable installations use build-image.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "${AGENTBOX_AUTO_UPDATE:-0}" != 1 ]]; then
  echo "自动追新默认关闭。确认要使用最新 CLI 时设置 AGENTBOX_AUTO_UPDATE=1。"
  exit 0
fi
verify_binary=${AGENTBOX_VERIFY_BINARY:-./agentbox}
[[ -x "$verify_binary" ]] || { echo "需要带 --check-agent-image 的新版 agentbox；未修改镜像。" >&2; exit 1; }
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
# Never let the build overwrite the active alias before behavior checks pass.
previous_id=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)
candidate="agentbox-agent:cli-candidate-$(date +%s)-$$"
AGENTBOX_IMAGE=$candidate AGENTBOX_VERSIONED_IMAGE=$candidate \
 AGENTBOX_CLAUDE_VERSION=$latest_claude AGENTBOX_CODEX_VERSION=$latest_codex ./scripts/build-image.sh
candidate_id=$(docker image inspect -f '{{.Id}}' "$candidate")
[[ "$candidate_id" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo "候选镜像 ID 无效，未切换。" >&2; exit 1; }
# An old binary rejects this unknown flag without starting a service. The
# command performs no config/database mutation and exits nonzero on any failure.
"$verify_binary" --check-agent-image "$candidate_id"
current_id=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)
[[ "$current_id" == "$previous_id" ]] || { echo "当前镜像在验证期间变化，未切换。" >&2; exit 1; }
docker tag "$candidate_id" "$image"
# Keep old version tags and layers for an operator-controlled rollback.
