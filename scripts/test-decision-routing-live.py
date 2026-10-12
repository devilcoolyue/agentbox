#!/usr/bin/env python3
"""Explicitly authorized Mindshub Jev routing evaluation; stdin key, synthetic tasks only."""
import argparse
from datetime import datetime, timezone
import getpass
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import uuid

from verify import clean_env, source_identity

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--run-live', action='store_true', required=True,
                    help='Explicitly allow eight real Jev calls; never part of default checks')
parser.add_argument('--case', default='', help='Run only this named case when diagnosing a failure')
args = parser.parse_args()
key = getpass.getpass('Mindshub API key (memory only): ') if sys.stdin.isatty() else sys.stdin.readline().strip()
if not key or len(key) > 4096 or any(c in key for c in '\r\n\0'):
    raise SystemExit('Missing or invalid stdin credential')
report_dir = Path('output/verification') / ('jev-routing-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + uuid.uuid4().hex[:8])
report_dir.mkdir(parents=True, exist_ok=False)
identity = source_identity()
with tempfile.TemporaryDirectory(prefix='agentbox-jev-routing-') as tmp:
    binary = Path(tmp) / 'decision.test'
    subprocess.run(['go', 'test', '-c', '-o', str(binary), './internal/decision'], env=clean_env(), check=True)
    env = dict(clean_env(), AGENTBOX_JEV_ROUTING_LIVE_TEST='1', AGENTBOX_JEV_ROUTING_REPORT=str((report_dir / 'routing.json').resolve()), AGENTBOX_JEV_ROUTING_CASE=args.case)
    result = subprocess.run([str(binary), '-test.run=^TestJevMindshubLiveRouting$', '-test.v', '-test.timeout=150s'], input=key+'\n', text=True, capture_output=True, env=env)
    safe_output = (result.stdout + result.stderr).replace(key, '[REDACTED]')
    (report_dir / 'run.log').write_text(safe_output)
    summary = {'source_before': identity, 'source_after': source_identity(), 'exit_code': result.returncode,
               'credentials': 'stdin only; not persisted', 'report': 'routing.json', 'log': 'run.log'}
    (report_dir / 'report.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2)+'\n')
    print(safe_output, end='')
    print('Evidence:', report_dir)
    raise SystemExit(result.returncode)
