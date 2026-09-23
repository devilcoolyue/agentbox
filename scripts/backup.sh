#!/usr/bin/env bash
# Scheduled system backup. The binary implements snapshots, manifests and
# verification; this wrapper adds retention and optional remote transfer.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
backup_bin=${AGENTBOX_BIN:-"$script_dir/../agentbox"}
backup_config=${AGENTBOX_CONFIG:-"$script_dir/../config.json"}
exec python3 - "$backup_bin" "$backup_config" "$@" <<'PY'
import json, os, pathlib, subprocess, sys

binary, config, *args = sys.argv[1:]
try:
    keep = int(os.environ.get('BACKUP_KEEP', '14'))
    if keep < 1:
        raise ValueError()
except ValueError:
    sys.exit('BACKUP_KEEP must be a positive integer')
result = subprocess.run([binary, 'backup', '--config', config, *args],
                        check=True, text=True, stdout=subprocess.PIPE)
report = json.loads(result.stdout)
archive = pathlib.Path(report['archive']).resolve()
checksum = pathlib.Path(str(archive) + '.sha256')
fd = os.open(checksum, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as stream:
    stream.write(report['sha256'] + '  ' + archive.name + '\n')
print(json.dumps(report, ensure_ascii=False), flush=True)
if remote := os.environ.get('BACKUP_REMOTE'):
    subprocess.run(['rsync', '-az', '--', str(archive), str(checksum), remote.rstrip('/') + '/'], check=True)
# Rotate only archives of this mode. System and full recovery points have
# independent retention; old-format archives are left alone for migration.
pattern = 'agentbox-backup-' + report['mode'] + '-*.tar.gz'
archives = sorted((p for p in archive.parent.glob(pattern) if p.is_file() and not p.is_symlink()),
                  key=lambda p: p.name, reverse=True)
for old in archives[keep:]:
    if old == archive:
        continue
    old.unlink()
    pathlib.Path(str(old) + '.sha256').unlink(missing_ok=True)
PY
