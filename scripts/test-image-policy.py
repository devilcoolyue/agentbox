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
print('Image policy: pinned defaults and no default network/update calls verified')
