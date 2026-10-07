#!/usr/bin/env python3
"""Check image defaults and opt-in update behavior without Docker/network calls."""
import os
from pathlib import Path
import subprocess
import tempfile

ROOT=Path(__file__).resolve().parent.parent
versions={line.split('=',1)[0]:line.split('=',1)[1] for line in (ROOT/'images/agent/versions.env').read_text().splitlines() if line and not line.startswith('#')}
dockerfile=(ROOT/'images/agent/Dockerfile').read_text()
for key,value in versions.items():
    if f'ARG {key}={value}\n' not in dockerfile:raise SystemExit('Dockerfile default differs: '+key)
with tempfile.TemporaryDirectory(prefix='agentbox-image-policy-') as tmp:
    tmp=Path(tmp);calls=tmp/'calls'
    for name in ['docker','curl']:
        path=tmp/name
        path.write_text('#!/bin/sh\nprintf "%s\\n" "'+name+' $*" >> "$TEST_CALLS"\n')
        path.chmod(0o755)
    env=dict(os.environ,PATH=str(tmp)+os.pathsep+os.environ['PATH'],TEST_CALLS=str(calls))
    for key in ['AGENTBOX_AUTO_UPDATE','CLAUDE_VERSION','CODEX_VERSION','BASE_IMAGE','AGENTBOX_CLAUDE_VERSION','AGENTBOX_CODEX_VERSION','AGENTBOX_BASE_IMAGE']:
        env.pop(key,None)
    subprocess.run(['bash','scripts/auto-update-image.sh'],cwd=ROOT,env=env,check=True)
    if calls.exists():raise SystemExit('Default update contacted Docker/network')
    subprocess.run(['bash','scripts/build-image.sh'],cwd=ROOT,env=dict(env, CODEX_VERSION='0.0.0-unrelated'),check=True)
    recorded=calls.read_text()
    for key,value in versions.items():
        if f'{key}={value}' not in recorded:raise SystemExit('Pinned argument missing')
    calls.unlink()
    result=subprocess.run(['bash','scripts/build-image.sh'],cwd=ROOT,env=dict(env,AGENTBOX_CLAUDE_VERSION='latest'),capture_output=True)
    if result.returncode==0 or calls.exists():raise SystemExit('Unpinned latest accepted')
    # Legacy opt-in must build a separate candidate and validate its immutable
    # ID before retagging. All external programs below are synthetic stubs.
    (tmp/'curl').write_text("#!/bin/sh\nprintf '%s\\n' '{\"version\":\"9.9.9\"}'\n")
    (tmp/'docker').write_text("""#!/bin/sh
printf '%s\\n' "docker $*" >> "$TEST_CALLS"
if [ "$1" = image ] && [ "$2" = inspect ]; then
  for last do :; done
  case "$last" in
    agentbox-agent:cli-candidate-*) printf 'sha256:%064d\\n' 2 ;;
    *) if [ "${TEST_RACE:-0}" = 1 ] && [ -f "$TEST_VERIFIED" ]; then printf 'sha256:%064d\\n' 3; else printf 'sha256:%064d\\n' 1; fi ;;
  esac
fi
""")
    verifier=tmp/'verifier'
    verifier.write_text("""#!/bin/sh
printf '%s\\n' "verify $*" >> "$TEST_CALLS"
[ "${TEST_PROBE_FAIL:-0}" != 1 ] || exit 1
[ "$1" = --check-agent-image ] || exit 1
: > "$TEST_VERIFIED"
""")
    verifier.chmod(0o755)
    for mode in ['passed','failed','changed']:
        calls.unlink(missing_ok=True)
        verified=tmp/'verified';verified.unlink(missing_ok=True)
        run_env=dict(env,AGENTBOX_AUTO_UPDATE='1',AGENTBOX_VERIFY_BINARY=str(verifier),TEST_VERIFIED=str(verified),TEST_PROBE_FAIL='1' if mode=='failed' else '0',TEST_RACE='1' if mode=='changed' else '0')
        result=subprocess.run(['bash','scripts/auto-update-image.sh'],cwd=ROOT,env=run_env,capture_output=True,text=True)
        if (result.returncode==0)!=(mode=='passed'):raise SystemExit('unexpected update status: '+mode+' '+result.stderr)
        rows=calls.read_text().splitlines()
        build=next(line for line in rows if line.startswith('docker build '))
        if ' -t agentbox-agent:latest ' in build:raise SystemExit('build replaced active alias before validation')
        tags=[line for line in rows if line.startswith('docker tag ')]
        if bool(tags)!=(mode=='passed'):raise SystemExit('unsafe alias switch: '+mode)
        if tags:
            verify=next(line for line in rows if line.startswith('verify '))
            if rows.index(verify)>rows.index(tags[0]) or 'sha256:'+str(2).zfill(64) not in tags[0]:raise SystemExit('unverified/mutable tag activation')
print('Image policy: pinned defaults, candidate validation, failed/racing updates preserve active alias')
