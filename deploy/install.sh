#!/usr/bin/env bash
# 安装/刷新 systemd 单元与 logrotate 配置，路径按当前仓库位置注入。
# 首次部署要跑，把仓库挪了位置也要重跑 —— 单元里的绝对路径正是靠这一步
# 保持与实际目录一致（曾因迁移后单元指向旧路径，旧实例占着端口，新实例
# 每 3 秒重启一次，持续了 31 小时）。
# 幂等，可反复执行；不会启动服务，启动交给 deploy.sh。
set -euo pipefail
cd "$(dirname "$0")/.."
APP_DIR=$(pwd -P)

UNIT_DIR=/etc/systemd/system

if [[ $EUID -ne 0 ]]; then
  echo "需要 root：sudo $0" >&2
  exit 1
fi

echo "安装路径: $APP_DIR"

for unit in agentbox.service \
  agentbox-image-update.service agentbox-image-update.timer \
  agentbox-backup.service agentbox-backup.timer; do
  sed "s|__APP_DIR__|$APP_DIR|g" "deploy/$unit" > "$UNIT_DIR/$unit"
  echo "  -> $UNIT_DIR/$unit"
done

install -m 0644 deploy/agentbox.logrotate /etc/logrotate.d/agentbox
echo "  -> /etc/logrotate.d/agentbox"

systemctl daemon-reload
systemctl enable agentbox.service agentbox-image-update.timer agentbox-backup.timer >/dev/null
systemctl start agentbox-image-update.timer agentbox-backup.timer

echo
echo "单元已安装并设为开机自启。接着跑 ./deploy/deploy.sh 构建并启动。"
