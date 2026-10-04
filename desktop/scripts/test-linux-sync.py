#!/usr/bin/env python3
"""Portable native Go sidecar against an isolated Linux HTTP peer.
Use with the compiled TestClientLinuxPeer test binary, not a production server.
The peer must run with AGENTBOX_LINUX_PEER=1 and a fresh
AGENTBOX_LINUX_PEER_RUN matching the required --peer-run-id argument. This probe
always exercises a kernel-killed sidecar and real 30-second lease expiry.
"""
import argparse
import json
import os
import hashlib
import platform
import queue
import signal
import sqlite3
import threading
from pathlib import Path
import subprocess
import tempfile
import urllib.request
import urllib.parse
import time

CRASH_FILE = 'crash.bin'
CRASH_BEFORE = 'before cross-OS crash\r\n中文\0'.encode()
CRASH_UPLOAD = b'native sidecar publication\r\n\0'
CRASH_LOCAL_EDIT = 'local user edit after native process kill\r\n中文\0'.encode()
CRASH_REMOTE_EDIT = 'server user edit after native process kill\r\n中文\0'.encode()

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError('Isolated peer must not redirect requests')


def loopback_server(value):
    parsed = urllib.parse.urlsplit(value)
    if (parsed.scheme != 'http' or parsed.hostname != '127.0.0.1'
            or parsed.username is not None or parsed.password is not None
            or parsed.path or parsed.query or parsed.fragment
            or parsed.port is None or not 1 <= parsed.port <= 65535):
        raise ValueError('Only an explicit isolated http://127.0.0.1:PORT peer is allowed')
    return value


def read_events(stream, events):
    try:
        while True:
            line = stream.readline(1024 * 1024 + 1)
            if not line:
                events.put(EOFError('Native sidecar closed before its response'))
                return
            if len(line) > 1024 * 1024:
                raise ValueError('Native sidecar response exceeds fixture limit')
            events.put(json.loads(line))
    except Exception as error:
        events.put(error)


def next_event(events, timeout=45):
    try:
        value = events.get(timeout=timeout)
    except queue.Empty as error:
        raise TimeoutError('Native sidecar did not respond before fixture deadline') from error
    if isinstance(value, Exception):
        raise value
    return value


class NativeSidecar:
    def __init__(self, binary, server, state):
        self.server = server
        self.state = state
        self.sequence = 0
        self.terminated_by_test = False
        self.process = subprocess.Popen([str(binary.resolve()), '--desktop'], stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, encoding='utf-8')
        self.events = queue.Queue()
        self.reader = threading.Thread(target=read_events, args=(self.process.stdout, self.events), daemon=True)
        self.reader.start()
        try:
            ready = next_event(self.events, 15)
            if ready.get('version') != 1 or ready.get('type') != 'ready':
                raise ValueError('Native sidecar did not negotiate the desktop protocol')
        except Exception:
            self.close()
            raise

    def send(self, kind, **fields):
        self.sequence += 1
        request_id = 'cross-os-' + str(self.sequence)
        request = {'version': 1, 'id': request_id, 'type': kind, 'server': self.server, 'allow_http': True,
                   'token': 'client-fixture-alice', 'user': 'alice', 'state_dir': str(self.state), **fields}
        self.process.stdin.write(json.dumps(request) + '\n')
        self.process.stdin.flush()
        return request_id

    def exchange(self, kind, **fields):
        request_id = self.send(kind, **fields)
        result = next_event(self.events)
        if result.get('version') != 1 or result.get('id') != request_id:
            raise ValueError('Native sidecar response is not for this request')
        return result

    def call(self, kind, **fields):
        result = self.exchange(kind, **fields)
        if result.get('type') == 'error':
            raise ValueError('Native sidecar rejected ' + kind + ': ' + str(result.get('error')))
        return result

    def kill(self):
        if self.process.poll() is not None:
            raise ValueError('The publication sidecar already exited; no OS kill was exercised')
        # CPython invokes TerminateProcess(handle, 1) on Windows and SIGKILL on
        # POSIX. Do not use stdin EOF, protocol shutdown, or a graceful signal.
        self.process.kill()
        code = self.process.wait(timeout=10)
        expected = 1 if os.name == 'nt' else -signal.SIGKILL
        if code != expected:
            raise ValueError('The native sidecar did not exit with the expected kernel termination status')
        self.terminated_by_test = True
        return {'mechanism': 'TerminateProcess' if os.name == 'nt' else 'SIGKILL', 'exit_code': code,
                'pid': self.process.pid}

    def close(self):
        if self.process.stdin and not self.process.stdin.closed:
            try:
                self.process.stdin.close()
            except BrokenPipeError:
                pass
        try:
            try:
                code = self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)
                raise TimeoutError('Native sidecar did not exit on pipe closure')
            if code != 0 and not self.terminated_by_test:
                raise ValueError('Native sidecar exited unexpectedly')
        finally:
            self.reader.join(timeout=5)
            self.process.stdout.close()

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()


def fixture_request(opener, server, run_id, action, method='GET'):
    if action not in ('status', 'prepare', 'arm', 'edit') or method not in ('GET', 'POST') or not run_id:
        raise ValueError('Invalid fixed crash fixture control')
    request = urllib.request.Request(server + '/fixture/crash/' + action, method=method,
                                    data=b'' if method == 'POST' else None,
                                    headers={'Authorization': 'Bearer client-fixture-alice', 'X-Agentbox-Fixture-Run': run_id})
    with opener.open(request, timeout=5) as response:
        value = json.loads(response.read(16384))
    if value.get('run_id') != run_id or value.get('error'):
        raise ValueError('Unexpected crash fixture response: ' + str(value.get('error', 'run identity changed')))
    return value


def wait_fixture(read, predicate, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status = read()
        if predicate(status):
            return status
        time.sleep(0.1)
    raise TimeoutError('Crash fixture did not reach the required real process/filesystem boundary')


def stored_state(directory, binding, batch=None):
    # Inspect durable metadata without initializing, migrating or writing it.
    database = (directory / 'sync.db').resolve().as_uri() + '?mode=ro'
    connection = sqlite3.connect(database, uri=True, timeout=5)
    try:
        row = connection.execute('SELECT state FROM bindings WHERE id=?', (binding,)).fetchone() if batch is None else connection.execute('SELECT state FROM batches WHERE binding=? AND id=?', (binding, batch)).fetchone()
        if row is None:
            raise ValueError('Expected durable sidecar state was not stored')
        return json.loads(row[0])
    finally:
        connection.close()


def kernel_kill_recovery(args, opener, local, state, private, binding):
    control = lambda action='status', method='GET': fixture_request(opener, args.server, args.peer_run_id, action, method)
    control('prepare', 'POST')
    (local / CRASH_FILE).write_bytes(CRASH_BEFORE)
    with NativeSidecar(args.sidecar, args.server, state) as first:
        initial = first.call('sync_preview', binding_id=binding, direction='automatic')['preview']
        if initial['plan']['operations'] or initial['plan']['conflicts']:
            raise ValueError('The crash scenario did not start from equal byte trees')
        first.call('sync_apply', binding_id=binding, preview=initial, confirmation=initial['plan']['digest'])
        baseline = stored_state(state, binding)
        if not baseline.get('baseline') or baseline.get('pending'):
            raise ValueError('The crash scenario did not establish a durable baseline')
        apply_before = control()['apply_calls']
        (local / CRASH_FILE).write_bytes(CRASH_UPLOAD)
        preview = first.call('sync_preview', binding_id=binding, direction='automatic')['preview']
        if [(op['kind'], op['path']) for op in preview['plan']['operations']] != [('upload', CRASH_FILE)]:
            raise ValueError('Expected precisely one real remote replacement before the kill')
        control('arm', 'POST')
        first.send('sync_apply', binding_id=binding, preview=preview, confirmation=preview['plan']['digest'])
        publication = wait_fixture(control, lambda result: result['published'], 30)
        if (publication['before_hash'] != hashlib.sha256(CRASH_BEFORE).hexdigest()
                or publication['current_hash'] != hashlib.sha256(CRASH_UPLOAD).hexdigest()
                or publication['apply_calls'] != apply_before + 1 or not publication['lease_active']
                or publication['lease_ttl_seconds'] != 30):
            raise ValueError('The Linux peer did not confirm real publication and retained before bytes')
        started = stored_state(state, binding)
        pending = started.get('pending')
        operation = publication['operation_id']
        if (not pending or len(pending['operations']) != 1 or pending['operations'][0]['id'] != operation
                or pending['operations'][0]['status'] != 'started' or started.get('baseline') != baseline['baseline']):
            raise ValueError('The live sidecar did not persist started intent while keeping its old baseline')
        killed = first.kill()
        disconnected = wait_fixture(control, lambda result: result['disconnected'], 5)
        if not disconnected['lease_active'] or stored_state(state, binding) != started:
            raise ValueError('The killed process lost pending state or performed graceful lease cleanup')
    print('Kernel kill reached: Linux applied journal/before persisted; native response unacknowledged.', flush=True)
    (local / CRASH_FILE).write_bytes(CRASH_LOCAL_EDIT)
    control('edit', 'POST')
    with NativeSidecar(args.sidecar, args.server, state) as second:
        if second.process.pid == killed['pid']:
            raise ValueError('Recovery did not execute in a new native OS process')
        bindings = second.call('sync_list')['bindings']
        if len(bindings) != 1 or bindings[0]['id'] != binding or not bindings[0]['pending']:
            raise ValueError('The restarted native process did not recover its pending batch')
        refused = second.exchange('sync_apply', binding_id=binding, preview=preview, confirmation=preview['plan']['digest'])
        if refused.get('type') != 'error' or refused.get('error') != 'sync_pending' or control()['apply_calls'] != apply_before + 1:
            raise ValueError('The restarted process replayed or accepted an ambiguous operation')
        refused = second.exchange('sync_review', binding_id=binding)
        if refused.get('type') != 'error' or not control()['lease_active']:
            raise ValueError('Recovery bypassed the killed process still-active lease')
        lease_wait = time.monotonic()
        expired = wait_fixture(control, lambda result: not result['lease_active'], publication['lease_ttl_seconds'] + 5)
        lease_wait = time.monotonic() - lease_wait
        review = second.call('sync_review', binding_id=binding)['review']
        if review['can_finish'] or review['final_matches'] or len(review['items']) != 1:
            raise ValueError('Post-crash user edits were incorrectly considered completed')
        item = review['items'][0]
        if (item['id'] != operation or item['recorded'] != 'started' or item['receipt'] != 'applied'
                or item['current'] != 'diverged' or not item['recovery']):
            raise ValueError('Recovery review lost the original applied receipt or durable before bytes')
        refused = second.exchange('sync_resolve', binding_id=binding, action='finish', confirmation=review['digest'])
        if refused.get('type') != 'error' or refused.get('error') != 'sync_pending':
            raise ValueError('Diverged post-crash files were silently committed')
        destination = private / 'crash export'; destination.mkdir()
        exported = second.call('sync_export', binding_id=binding, batch_id=pending['id'], operation_id=operation, export_directory=str(destination))
        if (destination / exported['filename']).read_bytes() != CRASH_BEFORE:
            raise ValueError('The restarted sidecar could not export exact original recovery bytes')
        resolved = second.call('sync_resolve', binding_id=binding, action='replan', confirmation=review['digest'])
        if resolved['binding']['pending']:
            raise ValueError('Explicit replan did not archive the pending batch')
        final = stored_state(state, binding)
        archived = stored_state(state, binding, pending['id'])
        if (final.get('pending') or final['baseline'] != baseline['baseline']
                or archived['resolution']['action'] != 'replan' or archived['operations'] != pending['operations']):
            raise ValueError('Replan changed the original baseline or discarded started-operation evidence')
        replanned = second.call('sync_preview', binding_id=binding, direction='automatic')['preview']
        final_peer = control()
        if (len(replanned['plan']['conflicts']) != 1 or replanned['plan']['conflicts'][0]['path'] != CRASH_FILE
                or replanned['plan']['operations'] or final_peer['apply_calls'] != apply_before + 1
                or final_peer['current_hash'] != hashlib.sha256(CRASH_REMOTE_EDIT).hexdigest()
                or final_peer['before_hash'] != hashlib.sha256(CRASH_BEFORE).hexdigest()
                or (local / CRASH_FILE).read_bytes() != CRASH_LOCAL_EDIT):
            raise ValueError('Recovery replayed an operation or overwrote a post-crash user edit')
        return {'status': 'passed', **killed, 'restarted_pid': second.process.pid,
                'real_linux_publication': True, 'durable_started_intent': True,
                'replay_rejected': True, 'apply_calls_delta': final_peer['apply_calls'] - apply_before,
                'real_lease_expiry': not expired['lease_active'], 'lease_ttl_seconds': publication['lease_ttl_seconds'], 'lease_wait_seconds': round(lease_wait, 3),
                'before_exported': True, 'baseline_unchanged': True, 'both_user_edits_preserved': True,
                'archived_action': 'replan', 'physical_power_loss_tested': False}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server',required=True,type=loopback_server)
    parser.add_argument('--sidecar',type=Path,required=True)
    parser.add_argument('--report',type=Path)
    parser.add_argument('--require-native-os',choices=['Windows','Darwin','Linux'])
    parser.add_argument('--peer-run-id',required=True,help='Must match this disposable peer process AGENTBOX_LINUX_PEER_RUN')
    args=parser.parse_args()
    if args.require_native_os and platform.system()!=args.require_native_os:
        raise ValueError('This probe must execute on the requested native operating system')
    request=urllib.request.Request(args.server+'/fixture',headers={'Authorization':'Bearer client-fixture-alice','X-Agentbox-Fixture-Run':args.peer_run_id})
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
    for attempt in range(60):
        try:
            with opener.open(request,timeout=2) as response:fixture=json.loads(response.read(16384))
            break
        except OSError:
            if attempt==59:raise
            time.sleep(0.2)
    if fixture.get('os')!='linux' or not fixture.get('arch'):
        raise ValueError('The peer did not attest an actual Linux runtime')
    if args.peer_run_id and fixture.get('run_id')!=args.peer_run_id:
        raise ValueError('The responding peer does not belong to this isolated test run')
    with tempfile.TemporaryDirectory(prefix='agentbox-cross-os-') as temporary:
        private=Path(temporary).resolve();local=private/'项目 files';local.mkdir()
        state=private/'state';state.mkdir(mode=0o700)
        (local/'local.bin').write_bytes(b'local\r\n\x00\xef\xbb\xbf')
        def command(kind,**kwargs):
            with NativeSidecar(args.sidecar,args.server,state) as native:
                return native.call(kind,**kwargs)
        saved=command('sync_bind',directory=str(local),binding={'workspace':fixture['workspace'],'project':fixture['project'],'project_path':'.'})['binding']
        preview=command('sync_preview',binding_id=saved['id'],direction='automatic')['preview']
        assert len(preview['plan']['operations'])==2
        command('sync_apply',binding_id=saved['id'],preview=preview,confirmation=preview['plan']['digest'])
        assert (local/'remote.txt').read_bytes()=='linux\r\n中文\0'.encode()
        # A process restart must recover the same baseline and exact byte trees.
        preview=command('sync_preview',binding_id=saved['id'],direction='automatic')['preview']
        assert not preview['plan']['operations'] and not preview['plan']['conflicts'] and preview['baseline_current']
        (local/'local.bin').unlink()
        preview=command('sync_preview',binding_id=saved['id'],direction='automatic')['preview']
        assert [op['kind'] for op in preview['plan']['operations']]==['delete_remote']
        command('sync_apply',binding_id=saved['id'],preview=preview,confirmation=preview['plan']['digest'])
        history=command('sync_history',binding_id=saved['id'])['page']
        assert history['history'][0]['files'][0]['side']=='remote'
        export=private/'export';export.mkdir()
        item=history['history'][0]
        result=command('sync_export',binding_id=saved['id'],batch_id=item['id'],operation_id=item['files'][0]['id'],export_directory=str(export))
        assert (export/result['filename']).read_bytes()==b'local\r\n\x00\xef\xbb\xbf'
        assert history['remote_storage_status']=='available'
        assert history['remote_storage']['active_operations']==2
        # Each command starts a new native process. Explicitly retire the two
        # completed batches while preserving receipts and exported recovery bytes.
        for batch in history['history']:
            if not batch['remote_cleanable']:continue
            current=command('sync_history',binding_id=saved['id'])['page']
            cleanup=command('sync_remote_cleanup_review',binding_id=saved['id'],batch_id=batch['id'],revision=current['revision'])['cleanup']
            command('sync_remote_cleanup',binding_id=saved['id'],batch_id=batch['id'],revision=current['revision'],confirmation=cleanup['digest'])
        final=command('sync_history',binding_id=saved['id'])['page']
        assert final['remote_storage']['active_operations']==0
        assert final['remote_storage']['retained_receipts']==2
        assert final['remote_storage']['recovery_bytes']==0
        assert final['history'][0]['files'][0]['state']=='retired'
        preview=command('sync_preview',binding_id=saved['id'],direction='automatic')['preview']
        assert not preview['plan']['operations'] and not preview['plan']['conflicts'] and preview['baseline_current']
        assert (export/result['filename']).read_bytes()==b'local\r\n\x00\xef\xbb\xbf'
        crash=kernel_kill_recovery(args,opener,local,state,private,saved['id'])
        report={'cross_os_sync':'passed','native_os':platform.system(),'native_arch':platform.machine(),'native_release':platform.release(),'peer':fixture['os'],'peer_arch':fixture['arch'],'sidecar_sha256':hashlib.sha256(args.sidecar.read_bytes()).hexdigest(),'binary_bytes':True,'restart_baseline':True,'deletion_recovery':True,'remote_cleanup_and_retained_receipts':True,'kernel_kill_recovery':crash}
        if args.report:args.report.write_text(json.dumps(report,indent=2)+'\n',encoding='utf-8')
        print(json.dumps(report))


if __name__ == '__main__':
    main()
