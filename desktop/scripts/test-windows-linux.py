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
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import select
import socket
import subprocess
import sys
import threading
import time
import urllib.request
import uuid

LAUNCH_PEER = '''#!/bin/sh
set -eu
echo $$ > "$1/peer.pid"
exec env AGENTBOX_LINUX_PEER=1 AGENTBOX_LINUX_PEER_RUN="$2" TMPDIR="$1/data" "$1/peer.test" '-test.run=^TestClientLinuxPeer$' -test.v -test.timeout=6m
'''
PROBE_PEER = r'''#!/bin/bash
set -euo pipefail
exec 3<>/dev/tcp/127.0.0.1/8181
printf 'GET /fixture HTTP/1.0\r\nHost: 127.0.0.1\r\nAuthorization: Bearer client-fixture-alice\r\nX-Agentbox-Fixture-Run: %s\r\n\r\n' "$1" >&3
cat <&3
'''


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


def private_ipv4(value):
    address = ipaddress.ip_address(value)
    if address.version != 4 or not any(address in ipaddress.ip_network(network) for network in ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16')):
        raise ValueError('WSL relay target must be a discovered RFC1918 IPv4 address')
    return str(address)


def wsl_address(routes, interfaces):
    devices = {route.get('dev') for route in routes if route.get('dst') == 'default'}
    addresses = {private_ipv4(info['local']) for interface in interfaces if interface.get('ifname') in devices
                 for info in interface.get('addr_info', []) if info.get('family') == 'inet' and info.get('scope') == 'global'}
    if len(addresses) != 1:
        raise ValueError('Cannot identify one private address on the WSL default-route interface')
    return addresses.pop()


def verify_fixture(value, run_id):
    if value.get('run_id') != run_id or value.get('os') != 'linux' or value.get('arch') != 'amd64' or not value.get('workspace') or not value.get('project'):
        raise ValueError('Readiness response is not this Linux x86_64 fixture')
    return value


def parse_linux_probe(response, run_id):
    header, separator, body = response.partition('\r\n\r\n')
    if not separator or not header.startswith('HTTP/1.0 200 '):
        raise ValueError('Linux loopback fixture did not return HTTP 200')
    return verify_fixture(json.loads(body), run_id)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        raise ValueError('Fixture readiness must not redirect')


def windows_fixture(server, run_id):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    request = urllib.request.Request(server + '/fixture', headers={
        'Authorization': 'Bearer client-fixture-alice', 'X-Agentbox-Fixture-Run': run_id})
    with opener.open(request, timeout=3) as response:
        return verify_fixture(json.loads(response.read(16384)), run_id)


class LoopbackRelay:
    """A bounded, process-owned TCP bridge to the just-inspected WSL interface.

    No netsh/firewall settings or persistent listener is installed. The target
    port is fixed, and all clients use a fresh 127.0.0.1 ephemeral listener.
    """
    def __init__(self, address, connect=socket.create_connection):
        self.target = (private_ipv4(address), 8181)
        self.connect = connect
        self.stop = threading.Event()
        self.slots = threading.BoundedSemaphore(16)
        self.lock = threading.Lock()
        self.sockets = set()
        self.workers = []
        self.accepted = 0
        self.rejected = 0
        self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen(16)
        self.listener.settimeout(0.2)
        self.server = 'http://127.0.0.1:' + str(self.listener.getsockname()[1])
        self.thread = threading.Thread(target=self.accept, daemon=True)
        self.thread.start()

    def accept(self):
        while not self.stop.is_set():
            try:
                incoming, _address = self.listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            with self.lock:
                if self.stop.is_set() or not self.slots.acquire(blocking=False):
                    self.rejected += 1
                    incoming.close()
                    continue
                self.accepted += 1
                self.sockets.add(incoming)
                worker = threading.Thread(target=self.forward, args=(incoming,), daemon=True)
                self.workers.append(worker)
                worker.start()

    def forward(self, incoming):
        outgoing = None
        try:
            outgoing = self.connect(self.target, timeout=3)
            incoming.settimeout(3)
            outgoing.settimeout(3)
            with self.lock:
                self.sockets.add(outgoing)
            peers = {incoming: outgoing, outgoing: incoming}
            deadline = time.monotonic() + 120
            activity = time.monotonic()
            while not self.stop.is_set() and time.monotonic() < deadline and time.monotonic() - activity < 45:
                readable, _, _ = select.select(list(peers), [], [], 0.2)
                for source in readable:
                    content = source.recv(64 << 10)
                    if not content:
                        # A killed Windows sidecar must promptly close Linux's
                        # request context, including the deliberately held reply.
                        return
                    peers[source].sendall(content)
                    activity = time.monotonic()
        except (OSError, ValueError):
            pass
        finally:
            for stream in (incoming, outgoing):
                if stream is not None:
                    try:
                        stream.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
                    stream.close()
                    with self.lock:
                        self.sockets.discard(stream)
            self.slots.release()

    def close(self):
        self.stop.set()
        self.listener.close()
        with self.lock:
            streams = list(self.sockets)
        for stream in streams:
            try:
                stream.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            stream.close()
        deadline = time.monotonic() + 5
        self.thread.join(timeout=5)
        for worker in self.workers:
            worker.join(timeout=max(0, deadline - time.monotonic()))
        if self.thread.is_alive() or any(worker.is_alive() for worker in self.workers):
            raise RuntimeError('Owned WSL loopback relay did not stop')


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
    relay = None
    scripts = []
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
        # Script files avoid command-line quote reinterpretation across Windows,
        # wsl.exe and sh. Only owned paths and a generated UUID cross as argv.
        for name, content in [('launch-peer.sh', LAUNCH_PEER), ('probe-peer.sh', PROBE_PEER)]:
            script = args.report.with_suffix('.' + name)
            scripts.append(script)
            script.write_text(content, encoding='utf-8', newline='\n')
            execute('cp', '--', execute('wslpath', '-a', '-u', str(script.resolve())), directory + '/' + name)
        process = subprocess.Popen(base + ['sh', directory + '/launch-peer.sh', directory, run_id], stdout=log, stderr=subprocess.STDOUT)
        report['phase'] = 'linux_loopback_readiness'
        readiness_deadline = time.monotonic() + 30
        while True:
            if process.poll() is not None:
                raise RuntimeError('Linux peer exited before local readiness')
            try:
                inside = execute('timeout', '5', 'bash', directory + '/probe-peer.sh', run_id)
                report['linux_fixture'] = parse_linux_probe(inside, run_id)
                break
            except subprocess.CalledProcessError as error:
                if time.monotonic() >= readiness_deadline:
                    raise RuntimeError('Linux peer is not reachable inside WSL; Windows forwarding was not tested') from error
                time.sleep(0.2)
        report['linux_loopback_ready'] = True
        pid = execute('cat', directory + '/peer.pid')
        if not pid.isascii() or not pid.isdigit():
            raise ValueError('Invalid owned Linux peer PID')
        report['linux_peer_command'] = execute('ps', '-p', pid, '-o', 'args=')
        report['phase'] = 'windows_localhost_readiness'
        server = 'http://127.0.0.1:8181'
        try:
            outside = windows_fixture(server, run_id)
            report['windows_localhost_ready'] = True
            report['transport'] = 'wsl_automatic_localhost'
        except (OSError, ValueError) as error:
            report['windows_localhost_ready'] = False
            report['windows_localhost_error'] = str(error)
            report['phase'] = 'windows_wsl_relay_readiness'
            address = wsl_address(json.loads(execute('ip', '-j', '-4', 'route', 'show', 'default')),
                                  json.loads(execute('ip', '-j', '-4', 'address', 'show', 'up')))
            report['wsl_private_address'] = address
            relay = LoopbackRelay(address)
            server = relay.server
            outside = windows_fixture(server, run_id)
            report['transport'] = 'owned_loopback_relay_to_wsl'
        if outside != report['linux_fixture']:
            raise ValueError('Linux and Windows readiness reached different fixtures')
        report['windows_fixture_ready'] = True
        report['phase'] = 'native_windows_sync_and_kill'
        sync_report = args.report.with_suffix('.sync.json')
        subprocess.run([sys.executable, str(Path(__file__).with_name('test-linux-sync.py')),
                        '--server', server, '--sidecar', str(args.sidecar.resolve()),
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
        if relay:
            try:
                relay.close()
                report['relay'] = {'cleanup': 'passed', 'accepted': relay.accepted, 'rejected': relay.rejected}
            except Exception as error:
                report['relay'] = {'cleanup': 'failed', 'error': str(error)}
                report['windows_linux_sync'] = 'failed'
                failure = failure or error
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
            for script in scripts:
                script.unlink(missing_ok=True)
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
