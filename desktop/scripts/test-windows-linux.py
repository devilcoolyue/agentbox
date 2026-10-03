#!/usr/bin/env python3
"""Run the native Windows sidecar against a real WSL2 Linux test process.

Only a disposable GitHub-hosted Windows runner is accepted. The peer and all
server data are copied into an owned Linux /tmp directory, never a DrvFS mount.
WSL1, missing WSL2 support, an unexpected peer, or a skipped test are failures.
This is cross-kernel CI, not minimum-Windows-version or physical-PC acceptance.
The launcher creates the run identity and passes it to both the Linux fixture
and test-linux-sync.py; a real Windows TerminateProcess recovery result is
required in addition to ordinary synchronization and recovery cleanup.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import uuid


def decoded(data):
    # wsl.exe management commands often emit UTF-16 even through redirected
    # pipes, while --exec forwards the Linux command's UTF-8 bytes unchanged.
    encoding = 'utf-16' if data.startswith((b'\xff\xfe', b'\xfe\xff')) else 'utf-16-le' if b'\0' in data else 'utf-8'
    return data.decode(encoding).strip().lstrip('\ufeff')


def validate_wsl(distribution, listing, uname, filesystem):
    row = next((line for line in listing.splitlines()
                if re.match(r'^\s*\*?\s*' + re.escape(distribution) + r'\s+', line)), '')
    if not re.search(r'\s2\s*$', row):
        raise ValueError('The selected Linux distribution must be registered as WSL2')
    parts = uname.split()
    if len(parts) != 3 or parts[0] != 'Linux' or parts[2] != 'x86_64' or 'microsoft-standard-wsl2' not in parts[1].lower():
        raise ValueError('Actual uname must identify the x86_64 WSL2 Linux kernel')
    if filesystem not in ('ext2/ext3', 'ext4', 'btrfs', 'xfs', 'tmpfs'):
        raise ValueError('The peer must use a native Linux filesystem, not a Windows/DrvFS mount')


def private_directory(value):
    if not re.fullmatch(r'/tmp/agentbox-windows-peer\.[A-Za-z0-9]{8,16}', value):
        raise ValueError('Unexpected private Linux fixture directory')
    return value


def validate_binaries(peer, sidecar):
    linux = peer.read_bytes()
    windows = sidecar.read_bytes()
    if linux[:6] != b'\x7fELF\x02\x01' or int.from_bytes(linux[18:20], 'little') != 62:
        raise ValueError('The Linux test peer must be an ELF x86_64 executable')
    offset = int.from_bytes(windows[60:64], 'little')
    if (windows[:2] != b'MZ' or windows[offset:offset + 4] != b'PE\0\0'
            or int.from_bytes(windows[offset + 4:offset + 6], 'little') != 0x8664):
        raise ValueError('The native sidecar must be a Windows PE x86_64 executable')
    return {'linux_peer_sha256': hashlib.sha256(linux).hexdigest(),
            'windows_sidecar_sha256': hashlib.sha256(windows).hexdigest()}


def validate_sync_report(report):
    if (report.get('cross_os_sync') != 'passed' or report.get('native_os') != 'Windows'
            or report.get('peer') != 'linux' or report.get('peer_arch') != 'amd64'):
        raise ValueError('Native Windows to Linux integration evidence is incomplete')
    crash = report.get('kernel_kill_recovery', {})
    if (crash.get('status') != 'passed' or crash.get('mechanism') != 'TerminateProcess'
            or crash.get('exit_code') != 1 or crash.get('apply_calls_delta') != 1
            or crash.get('lease_ttl_seconds') != 30 or crash.get('archived_action') != 'replan'
            or not crash.get('pid') or not crash.get('restarted_pid') or crash['pid'] == crash['restarted_pid']
            or any(crash.get(field) is not True for field in ('real_linux_publication', 'durable_started_intent',
                'replay_rejected', 'real_lease_expiry', 'before_exported', 'baseline_unchanged', 'both_user_edits_preserved'))):
        raise ValueError('Windows TerminateProcess recovery evidence is incomplete')


def run(args):
    if (os.name != 'nt' or platform.system() != 'Windows'
            or os.environ.get('GITHUB_ACTIONS') != 'true'
            or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted'):
        raise ValueError('Requires an actual disposable GitHub-hosted Windows runner')
    if not re.fullmatch(r'[A-Za-z0-9._-]+', args.distribution):
        raise ValueError('Invalid WSL distribution name')
    args.report.parent.mkdir(parents=True, exist_ok=True)
    report = {'windows_linux_sync': 'failed', 'native_os': platform.system(),
              'native_arch': platform.machine(), 'native_release': platform.release(),
              'distribution': args.distribution, 'physical_pc_tested': False,
              'minimum_os_tested': False, 'windows_local_filesystem': 'native runner temporary directory'}
    base = ['wsl.exe', '--distribution', args.distribution, '--user', 'root', '--exec']

    def execute(*command):
        completed = subprocess.run(base + list(command), capture_output=True, check=True, timeout=30)
        return decoded(completed.stdout)

    def management(*command):
        return decoded(subprocess.run(['wsl.exe', *command], capture_output=True, check=True, timeout=30).stdout)

    directory = None
    process = None
    log = None
    failure = None
    try:
        report.update(validate_binaries(args.peer, args.sidecar))
        report['wsl_version'] = management('--version')
        report['wsl_distribution_list'] = management('--list', '--verbose')
        report['linux_uname'] = execute('uname', '-srm')
        report['linux_tmp_filesystem'] = execute('stat', '-f', '-c', '%T', '/tmp')
        report['linux_distribution'] = execute('cat', '/etc/os-release')
        validate_wsl(args.distribution, report['wsl_distribution_list'], report['linux_uname'], report['linux_tmp_filesystem'])
        directory = private_directory(execute('mktemp', '-d', '/tmp/agentbox-windows-peer.XXXXXXXX'))
        execute('mkdir', '-m', '700', directory + '/data')
        source = execute('wslpath', '-a', '-u', str(args.peer.resolve()))
        execute('cp', '--', source, directory + '/peer.test')
        execute('chmod', '700', directory + '/peer.test')
        linux_hash = execute('sha256sum', directory + '/peer.test').split()[0]
        if linux_hash != report['linux_peer_sha256']:
            raise ValueError('Linux peer changed while copying into the isolated filesystem')
        run_id = str(uuid.uuid4())
        log = args.report.with_suffix('.peer.log').open('wb')
        # Fixed shell program; paths and the random run ID are positional argv.
        # exec keeps the recorded PID bound to the exact private peer binary.
        launch = ('set -eu; echo $$ > "$1/peer.pid"; '
                  'exec env AGENTBOX_LINUX_PEER=1 AGENTBOX_LINUX_PEER_RUN="$2" '
                  'TMPDIR="$1/data" "$1/peer.test" '
                  '-test.run=^TestClientLinuxPeer$ -test.v -test.timeout=6m')
        process = subprocess.Popen(base + ['sh', '-c', launch, 'agentbox-peer', directory, run_id], stdout=log, stderr=subprocess.STDOUT)
        sync_report = args.report.with_suffix('.sync.json')
        subprocess.run([sys.executable, str(Path(__file__).with_name('test-linux-sync.py')),
                        '--server', 'http://127.0.0.1:8181', '--sidecar', str(args.sidecar.resolve()),
                        '--require-native-os', 'Windows', '--peer-run-id', run_id,
                        '--report', str(sync_report)], check=True, timeout=180)
        report['sync'] = json.loads(sync_report.read_text(encoding='utf-8'))
        validate_sync_report(report['sync'])
        if process.poll() is not None:
            raise ValueError('The Linux peer exited before the integration run completed')
        report['windows_linux_sync'] = 'passed'
    except Exception as error:
        failure = error
        report['error'] = str(error)
    finally:
        try:
            if process is not None and process.poll() is None:
                pid = execute('cat', directory + '/peer.pid')
                if not pid.isascii() or not pid.isdigit() or execute('readlink', '/proc/' + pid + '/exe') != directory + '/peer.test':
                    raise ValueError('Cannot identify the owned Linux peer for cleanup')
                execute('kill', '-TERM', '--', pid)
                process.wait(timeout=15)
            if directory:
                execute('rm', '-rf', '--', private_directory(directory))
            report['fixture_cleanup'] = 'passed'
        except Exception as error:
            report['fixture_cleanup'] = 'failed'
            report['cleanup_error'] = str(error)
            report['windows_linux_sync'] = 'failed'
            failure = failure or error
        finally:
            if process is not None and process.poll() is None:
                process.kill()
                process.wait(timeout=15)
            if log:
                log.close()
            args.report.write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    if failure:
        raise failure
    print(json.dumps(report, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--peer', type=Path, required=True)
    parser.add_argument('--sidecar', type=Path, required=True)
    parser.add_argument('--distribution', default='Ubuntu-24.04')
    parser.add_argument('--report', type=Path, required=True)
    run(parser.parse_args())


if __name__ == '__main__':
    main()
