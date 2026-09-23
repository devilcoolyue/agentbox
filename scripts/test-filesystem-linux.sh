#!/usr/bin/env bash
# Run filesystem, archive, seeding and server tests in an isolated Linux
# container without mounting source, credentials or user workspaces.
set -euo pipefail
cd "$(dirname "$0")/.."

test_image=${AGENTBOX_FS_TEST_IMAGE:-agentbox-git-test:fs}
test_stage=$(mktemp -d)
test_container=
cleanup() {
  if [[ -n "$test_container" ]]; then
    docker rm -f "$test_container" >/dev/null
  fi
  rm -rf "$test_stage"
}
trap cleanup EXIT

test_os=$(docker info --format '{{.OSType}}')
test_arch=$(docker info --format '{{.Architecture}}')
[[ "$test_os" == linux ]] || { echo "需要 Linux Docker daemon" >&2; exit 1; }
case "$test_arch" in
  aarch64|arm64) test_arch=arm64 ;;
  x86_64|amd64) test_arch=amd64 ;;
  *) echo "暂不支持 Docker 架构: $test_arch" >&2; exit 1 ;;
esac

docker build -t "$test_image" -f internal/dockerx/testdata/git.Dockerfile internal/dockerx/testdata
for package in safefs archivex agent server backup; do
  GOOS=linux GOARCH="$test_arch" CGO_ENABLED=0 \
    go test -c -o "$test_stage/$package.test" "./internal/$package"
done

test_container=$(docker run -d --network none --security-opt no-new-privileges \
  --memory 512m --pids-limit 256 "$test_image")
for package in safefs archivex agent server backup; do
  docker cp "$test_stage/$package.test" "$test_container:/tmp/$package.test"
  echo "Linux tests: $package"
  docker exec "$test_container" "/tmp/$package.test" -test.timeout=90s
done
