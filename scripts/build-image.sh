#!/usr/bin/env bash
# Build the pinned baseline by default. Overrides must be explicit exact versions.
set -euo pipefail
cd "$(dirname "$0")/.."
requested_claude=${AGENTBOX_CLAUDE_VERSION:-}
requested_codex=${AGENTBOX_CODEX_VERSION:-}
requested_base=${AGENTBOX_BASE_IMAGE:-}
source images/agent/versions.env
CLAUDE_VERSION=${requested_claude:-$CLAUDE_VERSION}
CODEX_VERSION=${requested_codex:-$CODEX_VERSION}
BASE_IMAGE=${requested_base:-$BASE_IMAGE}
for version in "$CLAUDE_VERSION" "$CODEX_VERSION"; do
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][a-zA-Z0-9.-]+)?$ ]] || {
    echo "CLI version must be exact, not latest/range: $version" >&2; exit 1;
  }
done
image=${AGENTBOX_IMAGE:-agentbox-agent:latest}
# Also retain a versioned tag so replacing the compatibility alias doesn't
# immediately lose the previous image needed for rollback.
versioned=${AGENTBOX_VERSIONED_IMAGE:-agentbox-agent:claude-$CLAUDE_VERSION-codex-$CODEX_VERSION}
docker build \
  --build-arg "BASE_IMAGE=$BASE_IMAGE" \
  --build-arg "CLAUDE_VERSION=$CLAUDE_VERSION" \
  --build-arg "CODEX_VERSION=$CODEX_VERSION" \
  --label "agentbox.claude-code=$CLAUDE_VERSION" \
  --label "agentbox.codex=$CODEX_VERSION" \
  -t "$image" -t "$versioned" images/agent
