#!/usr/bin/env python3
"""Discover and run repository verification with durable, scoped evidence."""
from __future__ import annotations
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import time
import uuid
from datetime import datetime, timezone

from verification_catalog import STEPS, PROFILES, EXTERNAL, COVERED

ROOT = Path(__file__).resolve().parent.parent
BY_ID = {step['id']: step for step in STEPS}
BROWSER_ENV = {'AGENTBOX_PLAYWRIGHT_MODULE', 'AGENTBOX_BROWSER_CHANNEL'}
PATH_INPUTS = ('binary', 'client', 'fixture', 'artifacts', 'previous_package', 'package')


def clean_env(source=None):
    source = os.environ if source is None else source
    # Disable inherited opt-ins, helper-process modes and partial-browser flags.
    # Resource-bearing checks are enabled only by their catalog entry.
    return {k: v for k, v in source.items()
            if (not k.startswith('AGENTBOX_') or k in BROWSER_ENV)
            and k not in {'CODEX_LIVE_TEST', 'MARKET_LIVE_TEST', 'VITE_AGENTBOX_SMOKE',
                          'GOFLAGS', 'GOOS', 'GOARCH'}}


def source_identity(root=ROOT):
    try:
        revision = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True, stderr=subprocess.DEVNULL).strip()
        names = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'], cwd=root).split(b'\0')
        digest = hashlib.sha256()
        for raw in sorted(set(n for n in names if n)):
            name = os.fsdecode(raw)
            path = root / name
            digest.update(raw + b'\0')
            if path.is_symlink():
                digest.update(b'link\0' + os.fsencode(os.readlink(path)))
            elif path.is_file():
                digest.update(str(path.stat().st_mode & 0o777).encode() + b'\0')
                with path.open('rb') as f:
                    while chunk := f.read(1024 * 1024): digest.update(chunk)
            else:
                digest.update(b'missing')
        dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=root))
        return {'revision': revision, 'dirty': dirty, 'worktree_sha256': digest.hexdigest()}
    except (OSError, subprocess.CalledProcessError):
        return {'revision': None, 'dirty': None, 'worktree_sha256': None}


def tool_versions(steps, env):
    names = {s['argv'][0] for s in steps} & {'go','node','npm','cargo'}
    if any(s['id'].startswith('go.') for s in steps): names.add('go')
    if 'npm' in names: names.add('node')
    result = {}
    for name in sorted(names):
        try:
            command = [name, 'version' if name == 'go' else '--version']
            probe = subprocess.run(command,env=env,cwd=ROOT,capture_output=True,text=True,timeout=20)
            result[name] = probe.stdout.strip()[:500] if probe.returncode == 0 else 'unavailable'
        except (OSError,subprocess.TimeoutExpired): result[name] = 'unavailable'
    return result


def save_report(path, report):
    temp = path.with_suffix('.tmp')
    temp.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    temp.chmod(0o600)
    os.replace(temp, path)


def stop_process(process):
    if process.poll() is not None: return
    if os.name != 'posix':
        process.terminate()
        try: process.wait(timeout=10)
        except subprocess.TimeoutExpired: process.kill(); process.wait()
        return
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGKILL):
        try:
            if os.name == 'posix': os.killpg(process.pid, sig)
            else: process.terminate()
        except ProcessLookupError: return
        try:
            process.wait(timeout=10)
            return
        except subprocess.TimeoutExpired: pass


def execute(argv, cwd, env, log, timeout):
    """No shell evaluation; cancellation targets only this new process group."""
    with log.open('xb') as output:
        log.chmod(0o600)
        process = subprocess.Popen(argv, cwd=cwd, env=env, stdout=output,
                                   stderr=subprocess.STDOUT, start_new_session=os.name == 'posix')
        try:
            return process.wait(timeout=timeout), None
        except subprocess.TimeoutExpired:
            stop_process(process)
            return process.returncode, 'timed_out'
        except KeyboardInterrupt:
            stop_process(process)
            return process.returncode, 'cancelled'


def go_evidence(log):
    totals = {'passed': 0, 'failed': 0, 'skipped': 0}
    skipped = []
    cached = set()
    with log.open(errors='replace') as f:
        for line in f:
            try: event = json.loads(line)
            except (ValueError, TypeError): continue
            if not isinstance(event, dict): continue
            words = event.get('Output','').split()
            if not event.get('Test') and len(words)>=3 and words[0]=='ok' and words[1]==event.get('Package') and words[-1]=='(cached)':
                cached.add(event['Package'])
            if not event.get('Test'): continue
            action = event.get('Action')
            if action in ('pass', 'fail', 'skip'):
                totals[{'pass':'passed', 'fail':'failed', 'skip':'skipped'}[action]] += 1
                if action == 'skip': skipped.append({'package': event.get('Package'), 'test': event['Test']})
    return {'counts': totals, 'skipped_tests': skipped, 'cached_packages': sorted(cached)}


def resolve_step(step, values, env):
    def replace(value):
        if value.startswith('{') and value.endswith('}'):
            key = value[1:-1]
            if not values.get(key): raise ValueError('requires --' + key.replace('_', '-'))
            value = str(values[key])
            if key in PATH_INPUTS and not Path(value).exists():
                raise ValueError('missing input for --' + key.replace('_', '-'))
            if key in PATH_INPUTS: value = str(Path(value).resolve())
        return value
    argv = [replace(a) for a in step['argv']]
    overrides = {k: replace(v) for k, v in step.get('env', {}).items()}
    if not shutil.which(argv[0]): raise ValueError('missing executable: ' + argv[0])
    for name in step.get('requires', []):
        if not (ROOT / name).exists(): raise ValueError('missing prerequisite: ' + name + '; install dependencies first')
    if step.get('nonroot') and os.geteuid() == 0:
        raise ValueError('requires a non-root Linux user to observe real EPERM; do not skip this gate')
    return argv, dict(env, **overrides)


def run_steps(steps, destination, values, profile, *, root=ROOT, executor=execute):
    destination.mkdir(parents=True, exist_ok=False, mode=0o700)
    report_path = destination / 'report.json'
    report = {'format_version': 1, 'profile': profile, 'started_at': datetime.now(timezone.utc).isoformat(),
              'platform': platform.platform(), 'python': platform.python_version(),
              'source': source_identity(root), 'status': 'running',
              'scope_note': 'Only the selected commands. Skips and external/native/paid/manual gates are not acceptance evidence.',
              'steps': [dict(id=s['id'], category=s['category'], note=s['note'], status='not_run') for s in steps]}
    save_report(report_path, report)
    env = clean_env()
    if values.get('playwright_module'): env['AGENTBOX_PLAYWRIGHT_MODULE'] = str(Path(values['playwright_module']).resolve())
    if values.get('browser_channel'): env['AGENTBOX_BROWSER_CHANNEL'] = values['browser_channel']
    report['tools'] = tool_versions(steps, env)
    report['browser_configuration'] = {key:env[key] for key in BROWSER_ENV if key in env}
    result = 0
    started = time.monotonic()
    for step, entry in zip(steps, report['steps']):
        if step.get('platform') and sys.platform != step['platform']:
            entry.update(status='not_applicable', reason='platform-specific gate: ' + step['platform'])
            save_report(report_path, report)
            continue
        try:
            argv, child_env = resolve_step(step, values, env)
        except ValueError as error:
            entry.update(status='blocked', reason=str(error))
            result = 2
            print('BLOCKED ' + step['id'] + ': ' + str(error), flush=True)
            break
        entry.update(status='running', command=argv, log=step['id'] + '.log',
                     environment_overrides={key:child_env[key] for key in step.get('env',{})})
        save_report(report_path, report)
        print('RUN ' + step['id'], flush=True)
        before = time.monotonic()
        try:
            code, stop = executor(argv, root, child_env, destination / entry['log'], values.get('timeout'))
            entry.update(exit_code=code, elapsed_seconds=round(time.monotonic()-before, 3))
            entry['status'] = stop or ('passed' if code == 0 else 'failed')
            if step['id'].startswith('go.') and (destination / entry['log']).exists():
                entry['go_tests'] = go_evidence(destination / entry['log'])
            if entry['status'] != 'passed': result = 130 if stop == 'cancelled' else 1
        except OSError as error:
            entry.update(status='blocked', reason=type(error).__name__)
            result = 2
        save_report(report_path, report)
        print(entry['status'].upper() + ' ' + step['id'], flush=True)
        if result: break
    report.update(status='passed' if result == 0 else 'incomplete', exit_code=result,
                  elapsed_seconds=round(time.monotonic()-started, 3), source_after=source_identity(root))
    report['source_changed'] = report['source'] != report['source_after']
    for entry in report['steps']:
        if entry['status'] == 'not_run': entry['reason'] = 'stopped after earlier failure, missing prerequisite or cancellation'
    save_report(report_path, report)
    counts = {status:sum(s['status']==status for s in report['steps']) for status in {s['status'] for s in report['steps']}}
    print('Summary: ' + json.dumps(counts, sort_keys=True), flush=True)
    print('Report: ' + str(report_path), flush=True)
    return result


def web_snapshot(root=ROOT):
    files = sorted((root / 'internal/web/static/js').rglob('*.js')) + [root / 'internal/linkapp/static/i18n-core.js']
    return {str(path.relative_to(root)): hashlib.sha256(path.read_bytes()).hexdigest() for path in files if path.is_file()}


def web_artifacts(root=ROOT, build=subprocess.run):
    before = web_snapshot(root)
    result = build(['npm','run','build'], cwd=root, env=clean_env())
    if result.returncode: return result.returncode
    after = web_snapshot(root)
    expected = {'internal/web/static/js/' + str(p.relative_to(root / 'web/src').with_suffix('.js'))
                for p in (root / 'web/src').rglob('*.ts') if not p.name.endswith('.d.ts')}
    expected.add('internal/linkapp/static/i18n-core.js')
    changed = sorted(p for p in before.keys() | after.keys() if before.get(p) != after.get(p))
    unmatched = sorted(expected ^ after.keys())
    if changed or unmatched:
        print(json.dumps({'changed_build_outputs': changed, 'source_output_inventory_mismatch': unmatched}))
        print('Build outputs are left for review. Rebuild/check in the required files, then rerun verification.')
        return 1
    print('Generated JS bytes and inventory match the starting worktree and TypeScript sources.')
    return 0


def shell_syntax(root=ROOT, check=subprocess.run):
    for path in ('install.sh','scripts/test-filesystem-linux.sh','scripts/test-admin-password-linux.sh'):
        result = check(['bash','-n',path],cwd=root,env=clean_env())
        if result.returncode: return result.returncode
    return 0


def helper(name):
    env = clean_env()
    if name == '_web_artifacts': return web_artifacts()
    if name == '_shell': return shell_syntax()
    fresh=name.endswith('_fresh')
    if fresh:name=name.removesuffix('_fresh')
    if name == '_go_nonserver':
        packages = subprocess.check_output(['go','list','./...'],cwd=ROOT,env=env,text=True).splitlines()
        argv = ['go','test','-json', *(['-count=1'] if fresh else []), *[p for p in packages if p != 'agentbox/internal/server']]
    elif name == '_race_chat':
        selection = '^(TestChat|TestLifecycle|TestClose|TestShutdown|TestStartupRecoversChat|TestAdapterTerminal|TestFlush)'
        # Compile and run ordinary packages unprivileged. Only server tests need
        # Linux chown capabilities, identical to the full Go profile boundary.
        common = ['go','test','-json','-race','-count=1']
        code = subprocess.run(common + ['./internal/chat','./internal/store','./internal/usage','./internal/agent','-run',selection],cwd=ROOT,env=env).returncode
        if code: return code
        argv = common.copy()
        if sys.platform == 'linux' and os.geteuid() != 0: argv += ['-exec','sudo -n --']
        argv += ['./internal/server','-run',selection]
    elif name == '_go_server':
        argv = ['go','test','-json', *(['-count=1'] if fresh else [])]
        if sys.platform == 'linux' and os.geteuid() != 0: argv += ['-exec','sudo -n --']
        argv += ['./internal/server']
    else: raise ValueError(name)
    return subprocess.run(argv,cwd=ROOT,env=env).returncode


def main():
    if len(sys.argv) == 2 and sys.argv[1] in ('_go_nonserver','_go_server','_go_nonserver_fresh','_go_server_fresh','_race_chat','_web_artifacts','_shell'):
        return helper(sys.argv[1])
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    listing = sub.add_parser('list', help='list resource categories, profiles and commands')
    listing.add_argument('--json', action='store_true')
    run = sub.add_parser('run', help='run only the named profile/steps and preserve evidence')
    run.add_argument('profile', nargs='?', choices=list(PROFILES))
    run.add_argument('--step', action='append', default=[], choices=list(BY_ID))
    run.add_argument('--dry-run', action='store_true')
    run.add_argument('--report-dir', type=Path, help='new directory; existing evidence is never overwritten')
    run.add_argument('--timeout', type=float, help='optional per-command timeout in seconds')
    for arg in ('binary','client','fixture','artifacts','image','recovery-image','network-image','playwright-module','browser-channel','previous-package','package'):
        run.add_argument('--'+arg)
    args = parser.parse_args()
    if args.command == 'list':
        catalog = {'profiles':PROFILES,'steps':STEPS,'external':EXTERNAL,'covered_by':COVERED}
        if args.json: print(json.dumps(catalog,ensure_ascii=False,indent=2))
        else:
            for name, ids in PROFILES.items(): print(name + ': ' + ', '.join(ids))
            for s in STEPS: print(f"{s['id']} [{s['category']}] {' '.join(s['argv'])}\n  {s['note']}")
            for path, note in EXTERNAL.items(): print(path + ' [external entry] ' + note)
        return 0
    ids = list(dict.fromkeys(PROFILES.get(args.profile, []) + args.step))
    if not ids: parser.error('choose a profile or --step; no implicit full/native/paid run')
    if args.timeout is not None and args.timeout <= 0: parser.error('--timeout must be positive')
    steps = [BY_ID[key] for key in ids]
    if args.dry_run:
        print(json.dumps({'profile':args.profile,'steps':steps,'executed':False},ensure_ascii=False,indent=2))
        return 0
    destination = args.report_dir or ROOT / 'output/verification' / (datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')+'-'+uuid.uuid4().hex[:8])
    values = vars(args) | {'python':sys.executable}
    try: return run_steps(steps,destination.resolve(),values,args.profile or 'selected')
    except FileExistsError: parser.error('--report-dir already exists; choose a new directory to preserve evidence')


if __name__ == '__main__':
    def interrupted(_sig, _frame): raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(main())
