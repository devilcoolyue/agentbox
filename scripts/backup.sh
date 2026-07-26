#!/usr/bin/env bash
# 备份 agentbox 关键状态：SQLite 数据库（用 .backup 取一致快照）、config.json、
# accounts/ 凭证。产物打包进 <data_dir>/backups/，保留最近 BACKUP_KEEP 份。
# 由 agentbox-backup.timer 每天调用；手动执行也安全。
#
# 备份含密钥（config.json、OAuth 凭证），产物权限 0600。设 BACKUP_REMOTE=
# user@host:/path 时额外用 rsync 推到异机（异地容灾）。
set -euo pipefail
cd "$(dirname "$0")/.."
APP_DIR=$(pwd -P)

DATA_DIR=$(python3 -c 'import json;print(json.load(open("config.json")).get("data_dir","data"))' 2>/dev/null || echo data)
[[ "$DATA_DIR" = /* ]] || DATA_DIR="$APP_DIR/$DATA_DIR"

DEST="$DATA_DIR/backups"
KEEP=${BACKUP_KEEP:-14}
mkdir -p "$DEST"

stamp=$(date +%Y%m%d-%H%M%S)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [[ -f "$DATA_DIR/state.db" ]]; then
  if command -v sqlite3 >/dev/null; then
    # .backup 对运行中的库是安全的一致快照；直接 cp WAL 有撕裂风险。
    sqlite3 "$DATA_DIR/state.db" ".backup '$work/state.db'"
  else
    echo "warning: sqlite3 not found, copying state.db directly (may be inconsistent)" >&2
    cp "$DATA_DIR/state.db" "$work/state.db"
  fi
fi
[[ -f "$APP_DIR/config.json" ]] && cp -a "$APP_DIR/config.json" "$work/"
[[ -d "$APP_DIR/accounts" ]] && cp -a "$APP_DIR/accounts" "$work/accounts"

archive="$DEST/agentbox-backup-$stamp.tar.gz"
tar -czf "$archive" -C "$work" .
chmod 600 "$archive"
size=$(stat -c %s "$archive" 2>/dev/null || stat -f %z "$archive")
echo "backup written: $archive ($size bytes)"

# 轮转：只保留最近 KEEP 份。
ls -1t "$DEST"/agentbox-backup-*.tar.gz 2>/dev/null | tail -n +"$((KEEP + 1))" | xargs -r rm --

if [[ -n "${BACKUP_REMOTE:-}" ]]; then
  rsync -az "$archive" "$BACKUP_REMOTE"/ && echo "pushed to $BACKUP_REMOTE"
fi
