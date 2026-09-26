#!/usr/bin/env bash
# Build the trusted network helper for the architecture of the Docker daemon.
set -euo pipefail
cd "$(dirname "$0")/.."
network_arch=$(docker info --format '{{.Architecture}}')
case "$network_arch" in
  aarch64|arm64) network_arch=arm64 ;;
  x86_64|amd64) network_arch=amd64 ;;
  *) echo "Unsupported Docker architecture: $network_arch" >&2; exit 1 ;;
esac
network_build=$(mktemp -d "${TMPDIR:-/tmp}/agentbox-network.XXXXXX")
trap 'rm -rf "$network_build"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH="$network_arch" go build -trimpath -o "$network_build/agentbox" ./cmd/agentbox
docker build -f images/network/Dockerfile -t "${AGENTBOX_NETWORK_IMAGE:-agentbox-network:latest}" "$network_build"
