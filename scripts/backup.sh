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

# 库跑在 WAL 模式下，主库文件可能几乎是空的（已提交的数据全在 -wal 里）。
# 因此必须走 SQLite 的在线备份 API 取一致快照：裸 cp state.db 会得到一个
# 结构完整但内容为空的库——备份包看着正常，恢复时才发现什么都没有。
# sqlite3 命令行不是必装的，缺了就用 python3 的 sqlite3 模块，同一套 API；
# python3 本来就是本脚本的前置（上面读 config.json 用的就是它）。
snapshot_db() { # $1=源库 $2=快照落点
  if command -v sqlite3 >/dev/null; then
    sqlite3 "$1" ".backup '$2'"
  else
    python3 - "$1" "$2" <<'PY'
import sqlite3, sys, urllib.parse
src = sqlite3.connect("file:" + urllib.parse.quote(sys.argv[1]) + "?mode=ro", uri=True)
dst = sqlite3.connect(sys.argv[2])
with dst:
    src.backup(dst)
dst.close()
src.close()
PY
  fi
}

# 核对快照确实带上了数据。只抓「线上有行、快照却是空表」这一种情况：那正是
# 裸 cp 丢 WAL 的症状，而放宽到「少几行」会被备份期间的并发写入误伤。
# 全新安装（线上本来就没数据）不会误报。
verify_db() { # $1=线上库 $2=快照
  python3 - "$1" "$2" <<'PY'
import sqlite3, sys, urllib.parse

def counts(path, ro):
    dsn = ("file:" + urllib.parse.quote(path) + "?mode=ro") if ro else path
    db = sqlite3.connect(dsn, uri=ro)
    have = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    out = {t: db.execute("SELECT COUNT(*) FROM " + t).fetchone()[0]
           for t in ("sessions", "users", "tokens", "usage_events",
                     "quotas", "credit_ledger") if t in have}
    db.close()
    return out

live, snap = counts(sys.argv[1], True), counts(sys.argv[2], False)
if missing := sorted(set(live) - set(snap)):
    sys.exit("backup verify: 快照缺表 " + ", ".join(missing))
if empty := sorted(t for t in live if live[t] > 0 and snap[t] == 0):
    sys.exit("backup verify: 快照里 " + ", ".join(empty) + " 是空表，线上有数据——备份无效")
print("backup verify: ok (" + ", ".join("%s=%d" % kv for kv in sorted(snap.items())) + ")")
PY
}

if [[ -f "$DATA_DIR/state.db" ]]; then
  snapshot_db "$DATA_DIR/state.db" "$work/state.db"
  verify_db "$DATA_DIR/state.db" "$work/state.db"
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
