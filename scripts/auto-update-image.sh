#!/usr/bin/env bash
# 检查 npm 上的 Claude Code / Codex 新版本，有更新就重建 agent 镜像。
# 由 agentbox-image-update.timer 每天调用；手动执行也安全（无更新则直接退出）。
# 会话容器不会被打断：运行中的容器继续用旧镜像，下次停止再启动时换新。
#
# 即使 CLI 版本没变，镜像超过 IMAGE_MAX_AGE_DAYS 天也会强制 --pull 重建一次，
# 借此吸收基础镜像（node:22-bookworm-slim）的安全更新。
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE=agentbox-agent:latest
MAX_AGE_DAYS=${IMAGE_MAX_AGE_DAYS:-7}

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
built=$(image_label agentbox.built)
now=$(date +%s)

stale=0
if [[ -z "$built" ]] || (( now - built > MAX_AGE_DAYS * 86400 )); then
  stale=1
fi

if [[ "$cur_claude" == "$latest_claude" && "$cur_codex" == "$latest_codex" && "$stale" -eq 0 ]]; then
  echo "image up to date: claude-code $cur_claude, codex $cur_codex"
  exit 0
fi

if [[ "$stale" -eq 1 && "$cur_claude" == "$latest_claude" && "$cur_codex" == "$latest_codex" ]]; then
  echo "rebuilding image (base refresh, > ${MAX_AGE_DAYS}d old): claude-code $cur_claude, codex $cur_codex"
else
  echo "rebuilding image: claude-code ${cur_claude:-?} -> $latest_claude, codex ${cur_codex:-?} -> $latest_codex"
fi

docker build --pull \
  --build-arg "CLAUDE_VERSION=$latest_claude" \
  --build-arg "CODEX_VERSION=$latest_codex" \
  --label "agentbox.claude-code=$latest_claude" \
  --label "agentbox.codex=$latest_codex" \
  --label "agentbox.built=$now" \
  -t "$IMAGE" images/agent

# 重建会把旧镜像层变成 dangling，长期堆积会吃满磁盘；每次构建后清理一次。
docker image prune -f >/dev/null && echo "pruned dangling images"
echo "done: $IMAGE now has claude-code $latest_claude, codex $latest_codex"
