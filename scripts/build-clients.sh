#!/usr/bin/env bash
# 交叉编译 abox-link 客户端，放进 data/abox-link/ 供 Web UI 下载。
#
# 客户端与服务端是一对：新客户端的控制台要用 /api/tunnel/pair/redeem，旧服务端上没这个
# 接口。所以务必**先部署服务端**（deploy/deploy.sh）再跑这个脚本，反过来会让下载到新
# 客户端的用户配对失败。
#
# 先全部编译到暂存目录，全绿了再一次性换过去——半套二进制比旧的还糟。
set -euo pipefail
cd "$(dirname "$0")/.."

command -v go >/dev/null || export PATH=$PATH:/usr/local/go/bin

OUT=${AGENTBOX_CLIENT_OUTPUT:-data/abox-link}
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

PLATFORMS=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
  windows/amd64
)

echo "==> 编译"
for p in "${PLATFORMS[@]}"; do
  os=${p%/*} arch=${p#*/}
  name="abox-link-$os-$arch"
  [[ $os == windows ]] && name="$name.exe"
  # -s -w 去掉符号表和调试信息：用户要下载它，体积小一截。
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 \
    go build -trimpath -ldflags '-s -w' -o "$STAGE/$name" ./cmd/abox-link
  printf '    %-32s %s\n' "$name" "$(du -h "$STAGE/$name" | cut -f1)"
done

echo "==> 发布到 $OUT"
mkdir -p "$OUT"
chmod 0755 "$STAGE"/*
mv -f "$STAGE"/* "$OUT/"
ls -1 "$OUT"
