#!/usr/bin/env python3
"""Explicit live Mindshub + real Linux backend/browser; synthetic coding CLIs.

The API key is read from stdin (one raw key line or {"api_key":"..."}) and sent
only through docker attach stdin. It is never placed in files, argv or env.
"""
import argparse
import datetime
import getpass
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def run(*args, **kwargs):
    return subprocess.run(list(map(str, args)), check=True, **kwargs)


def capture(*args, quiet=False):
    return subprocess.check_output(list(map(str, args)), text=True,
                                   stderr=subprocess.DEVNULL if quiet else None).strip()


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--run-live', action='store_true', help='Explicitly permit one live Mindshub routing request')
parser.add_argument('--image', required=True, help='Prepared local Linux image with a CA certificate bundle')
parser.add_argument('--output', help='New report directory; existing directories are refused')
parser.add_argument('--timeout-ms', type=int, default=15000)
parser.add_argument('--expect-timeout', action='store_true', help='Verify real deadline fallback, normally with --timeout-ms 100')
args = parser.parse_args()
if not args.run_live:
    parser.error('--run-live is required; this command contacts the real Mindshub service')
if not 100 <= args.timeout_ms <= 30000:
    parser.error('--timeout-ms must be between 100 and 30000')

root = Path(__file__).resolve().parent.parent
os.chdir(root)
image = json.loads(capture('docker', 'image', 'inspect', args.image))[0]
if image['Os'] != 'linux' or image['Architecture'] not in ('arm64', 'amd64'):
    raise SystemExit('requires a prepared local Linux arm64/amd64 image')
stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
output = Path(args.output or f'output/verification/decision-live-{stamp}').resolve()
output.mkdir(mode=0o700, parents=True, exist_ok=False)

try:
    raw = getpass.getpass('Mindshub API key (stdin only): ') if sys.stdin.isatty() else sys.stdin.readline(16385)
    if raw.lstrip().startswith('{'):
        key = json.loads(raw)['api_key']
    else:
        key = raw.strip()
    if not isinstance(key, str) or not key or len(key) > 8192 or any(c in key for c in '\r\n\0'):
        raise ValueError()
except (ValueError, KeyError, TypeError):
    raise SystemExit('provide a valid Mindshub API key on stdin') from None
raw = None

cid = network = process = url = None
started = time.monotonic()
result = {'version': 1, 'decision_backend': 'live_mindshub', 'cli_backend': 'synthetic',
          'timeout_ms': args.timeout_ms, 'expect_timeout': args.expect_timeout, 'status': 'failed'}


def safe(text):
    return text.replace(key, '[redacted]')


try:
    with tempfile.TemporaryDirectory(prefix='agentbox-decision-live-') as tmp:
        binary = Path(tmp) / 'collaboration.test'
        env = dict(os.environ, GOOS='linux', GOARCH=image['Architecture'], CGO_ENABLED='0')
        run('go', 'test', '-c', '-o', binary, './internal/server', env=env)
        # This explicit live fixture permits HTTPS egress; the original offline
        # runner retains its non-masquerading bridge and never reads this key.
        network = capture('docker', 'network', 'create', 'agentbox-decision-live-' + os.urandom(6).hex())
        cid = capture('docker', 'create', '--interactive', '--network', network,
                      '--publish', '127.0.0.1::18888', '--user', '0:0', '--entrypoint', '/bin/sh',
                      '--env', 'AGENTBOX_COLLABORATION_RUNTIME_TEST=1',
                      '--env', 'AGENTBOX_COLLABORATION_BROWSER_TEST=1',
                      '--env', 'AGENTBOX_COLLABORATION_CHAT_BROWSER_TEST=1',
                      '--env', 'AGENTBOX_DECISION_LIVE_TEST=1', image['Id'],
                      '-c', 'umask 077; exec /collaboration.test "$@"', '--',
                      '-test.run=^TestCollaborationRuntimeIntegration$/codex-claude-decision_live$',
                      '-test.v', '-test.timeout=300s')
        run('docker', 'cp', binary, cid + ':/collaboration.test', stdout=subprocess.DEVNULL)
        process = subprocess.Popen(['docker', 'start', '--attach', '--interactive', cid],
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        process.stdin.write((json.dumps({'api_key': key, 'timeout_ms': args.timeout_ms,
                                        'expect_timeout': args.expect_timeout}) + '\n').encode())
        process.stdin.close()
        process.stdin = None
        for _ in range(150):
            if process.poll() is not None:
                raise RuntimeError('live fixture exited before its browser endpoint was ready')
            try:
                address = capture('docker', 'port', cid, '18888', quiet=True)
                if address:
                    url = 'http://' + address
                    with urllib.request.urlopen(url + '/fixture', timeout=1) as response:
                        if response.status == 200:
                            break
            except (OSError, urllib.error.URLError, subprocess.CalledProcessError):
                pass
            time.sleep(.1)
        else:
            raise RuntimeError('live fixture did not become ready')
        browser_args = ['node', 'scripts/test-decision-live.mjs', url, str(output)]
        if args.expect_timeout:
            browser_args.append('--expect-timeout')
        run(*browser_args, timeout=240)
        logs, _ = process.communicate(timeout=30)
        (output / 'server.log').write_text(safe(logs.decode(errors='replace')))
        if process.returncode != 0 or capture('docker', 'inspect', '-f', '{{.State.ExitCode}}', cid) != '0':
            raise RuntimeError('live fixture backend assertions failed; see the sanitized server log')
        result['status'] = 'passed'
except Exception as error:
    result['error'] = safe(str(error))
    raise
finally:
    cleanup_errors = []
    if cid:
        try:
            run('docker', 'rm', '-f', cid, stdout=subprocess.DEVNULL)
        except Exception as error:
            cleanup_errors.append(safe(str(error)))
    if process:
        try:
            logs, _ = process.communicate(timeout=15)
            if logs:
                (output / 'server.log').write_text(safe(logs.decode(errors='replace')))
        except Exception as error:
            cleanup_errors.append(safe(str(error)))
    if network:
        try:
            run('docker', 'network', 'rm', network, stdout=subprocess.DEVNULL)
        except Exception as error:
            cleanup_errors.append(safe(str(error)))
    result['elapsed_seconds'] = round(time.monotonic() - started, 3)
    result['cleanup'] = 'passed' if not cleanup_errors else cleanup_errors
    if cleanup_errors:
        result['status'] = 'failed'
    (output / 'report.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n')
    key = ''
    print(f"Live Mindshub decision / synthetic CLI report: {output}")
    if cleanup_errors:
        raise SystemExit('live fixture cleanup failed')
