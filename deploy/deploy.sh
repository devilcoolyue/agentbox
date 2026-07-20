#!/usr/bin/env bash
# 构建 agentbox、替换二进制、重启服务并验证起来了。
# 单元文件不在这里管，那是 install.sh 的事（仓库挪位置后先跑它）。
set -euo pipefail
cd "$(dirname "$0")/.."
APP_DIR=$(pwd -P)

if [[ $EUID -ne 0 ]]; then
  echo "需要 root：sudo $0" >&2
  exit 1
fi

if [[ ! -f /etc/systemd/system/agentbox.service ]]; then
  echo "单元未安装，先跑 ./deploy/install.sh" >&2
  exit 1
fi

# 单元里的路径必须和当前目录一致，否则会重启出一个指向别处的实例。
unit_dir=$(sed -n 's|^WorkingDirectory=||p' /etc/systemd/system/agentbox.service)
if [[ "$unit_dir" != "$APP_DIR" ]]; then
  echo "单元指向 $unit_dir，当前仓库在 $APP_DIR" >&2
  echo "先跑 ./deploy/install.sh 刷新单元" >&2
  exit 1
fi

command -v go >/dev/null || export PATH=$PATH:/usr/local/go/bin
echo "==> 构建"
go build -o .agentbox.new ./cmd/agentbox

# 用 rename 换二进制：运行中的进程持有旧 inode 不受影响，且不会像
# 直接覆盖那样撞上 ETXTBSY。
if [[ -f agentbox ]]; then
  cp -p agentbox "agentbox.bak-$(date +%m%d-%H%M)"
  ls -1t agentbox.bak-* 2>/dev/null | tail -n +4 | xargs -r rm --
fi
chmod 0755 .agentbox.new
mv -f .agentbox.new agentbox
echo "==> 二进制已更新 ($(stat -c %s agentbox) 字节)"

echo "==> 重启服务"
systemctl restart agentbox

listen=$(python3 -c 'import json;print(json.load(open("config.json"))["listen"])' 2>/dev/null || echo '')
for i in $(seq 1 10); do
  sleep 1
  [[ "$(systemctl is-active agentbox)" == "active" ]] || continue
  [[ -z "$listen" ]] && break
  ss -ltn 2>/dev/null | grep -qF "$listen" && break
done

if [[ "$(systemctl is-active agentbox)" != "active" ]]; then
  echo
  echo "!! 启动失败，最近日志：" >&2
  tail -20 /var/log/agentbox.log >&2
  echo
  echo "回滚：mv -f \$(ls -1t agentbox.bak-* | head -1) agentbox && systemctl restart agentbox" >&2
  exit 1
fi

echo "==> 就绪"
systemctl status agentbox --no-pager | head -6
[[ -n "$listen" ]] && ss -ltnp 2>/dev/null | grep -F "$listen" || true
