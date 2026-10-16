#!/usr/bin/env python3
"""Real Codex against a synthetic gateway/upstream, with no external network."""
import argparse
from pathlib import Path
import subprocess
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True, type=Path, help='Linux gateway binary matching the image architecture')
    parser.add_argument('--image', required=True, help='Already-prepared local agent image; never pulled')
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    fixture = Path(__file__).parent / 'fixtures/loop-guard/codex-probe.py'
    name = 'abox-loop-guard-test-' + uuid.uuid4().hex[:12]
    subprocess.run(['docker', 'image', 'inspect', args.image], check=True, stdout=subprocess.DEVNULL)
    try:
        result = subprocess.run([
            'docker', 'run', '--rm', '--pull=never', '-i', '--name', name,
            '--network', 'none', '--user', '1000:1000',
            '--tmpfs', '/home/agent:uid=1000,gid=1000,mode=700',
            '--mount', f'type=bind,source={binary},target=/tmp/abox-loop-guard,readonly',
            '--entrypoint', 'python3', args.image, '-',
        ], input=fixture.read_bytes(), timeout=150)
        return result.returncode
    finally:
        # Only this invocation's random container can be removed. A failed
        # cleanup is an error, never a passing test with an orphaned CLI.
        remaining = subprocess.check_output([
            'docker', 'ps', '-aq', '--filter', f'name=^/{name}$',
        ], text=True).strip()
        if remaining:
            subprocess.run(['docker', 'rm', '-f', name], check=True)


if __name__ == '__main__':
    raise SystemExit(main())
