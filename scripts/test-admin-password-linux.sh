#!/usr/bin/env bash
# Linux recovery acceptance: synthetic data, no host mounts or model requests.
set -euo pipefail
cd "$(dirname "$0")/.."
admin_test_image=${AGENTBOX_ADMIN_TEST_IMAGE:-agentbox-client-test:local}
admin_test_arch=$(docker image inspect "$admin_test_image" --format '{{.Architecture}}')
case "$admin_test_arch" in
  amd64|arm64) ;;
  *) echo "Unsupported test image architecture: $admin_test_arch" >&2; exit 1 ;;
esac
admin_test_dir=$(mktemp -d)
admin_test_container=
cleanup() {
  if [[ -n "$admin_test_container" ]]; then
    docker rm -f "$admin_test_container" >/dev/null
  fi
  rm -rf "$admin_test_dir"
}
trap cleanup EXIT
chmod 755 "$admin_test_dir"
CGO_ENABLED=0 GOOS=linux GOARCH="$admin_test_arch" go test -c -o "$admin_test_dir/agentbox.test" ./cmd/agentbox
CGO_ENABLED=0 GOOS=linux GOARCH="$admin_test_arch" go test -c -o "$admin_test_dir/store.test" ./internal/store
mkdir "$admin_test_dir/testdata"
cp cmd/agentbox/testdata/admin_password_terminal.py "$admin_test_dir/testdata/"
admin_test_container=$(docker create --network none --user 1000:1000 \
  --cap-drop ALL --security-opt no-new-privileges --workdir /tests \
  "$admin_test_image" sh -c 'command -v python3 >/dev/null && ./agentbox.test -test.run TestAdminPassword -test.v -test.timeout 60s && ./store.test -test.run "TestMaintenance|TestPasswordReset" -test.v -test.timeout 60s')
docker cp "$admin_test_dir/." "$admin_test_container:/tests"
docker start --attach "$admin_test_container"
test "$(docker inspect "$admin_test_container" --format '{{.State.ExitCode}}')" = 0
