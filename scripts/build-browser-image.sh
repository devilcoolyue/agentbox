#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
docker build \
  --build-arg "AGENT_IMAGE=${AGENTBOX_BROWSER_BASE_IMAGE:-agentbox-agent:latest}" \
  --build-arg "CHROME_VERSION=${AGENTBOX_CHROME_VERSION:-154.0.8037.57}" \
  -t "${AGENTBOX_BROWSER_IMAGE:-agentbox-agent:browser}" images/browser
