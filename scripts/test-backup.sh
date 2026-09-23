#!/usr/bin/env bash
# Build and exercise the shipped CLI on synthetic WAL data. No live instance,
# real credentials, model requests, service restart or Docker mutation involved.
set -euo pipefail
cd "$(dirname "$0")/.."
backup_test_dir=$(mktemp -d)
trap 'rm -rf "$backup_test_dir"' EXIT
go build -o "$backup_test_dir/agentbox" ./cmd/agentbox
python3 - "$backup_test_dir" <<'PY'
import json, os, pathlib, sqlite3, subprocess, sys

base = pathlib.Path(sys.argv[1])
binary = base / 'agentbox'
source = base / 'source'
data = source / 'state'
data.mkdir(parents=True)
credentials = base / 'external-credentials'
credentials.mkdir()
(credentials / 'auth.json').write_text('synthetic-test-credential')
workspace = data / 'users/alice/sessions/s1/workspace'
workspace.mkdir(parents=True)
(workspace / 'README.txt').write_text('workspace data')
template = data / 'users/alice/home-template'
template.mkdir(parents=True)
(template / '.bashrc').write_text('template data')
config = source / 'config.json'
config.write_text(json.dumps({'data_dir': 'state', 'accounts': [
    {'id': 'test', 'type': 'codex', 'credentials_dir': str(credentials)}]}))
db = sqlite3.connect(data / 'state.db')
db.execute('PRAGMA journal_mode=WAL')
db.execute('CREATE TABLE test_data(value TEXT)')
db.execute("INSERT INTO test_data VALUES ('committed-in-wal')")
db.commit()

def run(*args):
    return json.loads(subprocess.check_output([str(binary), *map(str,args)], text=True))

report = run('backup', '--config', config)
archive = pathlib.Path(report['archive'])
assert archive.stat().st_mode & 0o777 == 0o600
assert run('backup-verify', archive)['valid']
target = base / 'restored'
run('restore', '--to', target, archive)
restored = json.loads((target / 'config.json').read_text())
assert restored['data_dir'] == 'data'
assert (target / restored['accounts'][0]['credentials_dir'] / 'auth.json').read_text() == 'synthetic-test-credential'
assert (target / 'data/users/alice/home-template/.bashrc').read_text() == 'template data'
assert not (target / 'data/users/alice/sessions').exists()
with sqlite3.connect(target / 'data/state.db') as restored_db:
    assert restored_db.execute('SELECT value FROM test_data').fetchone() == ('committed-in-wal',)
assert not (target / 'data/state.db-wal').exists()
result = subprocess.run([str(binary), 'restore', '--to', str(target), str(archive)], capture_output=True)
assert result.returncode != 0, 'restore overwrote an existing target'
# Exercise the scheduled wrapper, including checksum generation and retention.
env = os.environ.copy()
env.update(AGENTBOX_BIN=str(binary), AGENTBOX_CONFIG=str(config), BACKUP_KEEP='1')
env.pop('BACKUP_REMOTE', None)
subprocess.run(['./scripts/backup.sh'], check=True, env=env, stdout=subprocess.PIPE)
remaining = list((data / 'backups').glob('agentbox-backup-system-*.tar.gz'))
assert len(remaining) == 1
assert pathlib.Path(str(remaining[0]) + '.sha256').is_file()
if os.environ.get('AGENTBOX_BACKUP_DOCKER_TEST') == '1':
    # This reads the daemon's mount inventory; it never stops containers.
    full = run('backup', '--config', config, '--full')
    full_target = base / 'full-restored'
    run('restore', '--to', full_target, full['archive'])
    assert (full_target / 'data/users/alice/sessions/s1/workspace/README.txt').read_text() == 'workspace data'
    print('full backup CLI: stopped-container verification and complete workspace restore passed')
db.close()
print('backup CLI: WAL snapshot, external credentials, templates, verification, restore and retention passed')
PY
