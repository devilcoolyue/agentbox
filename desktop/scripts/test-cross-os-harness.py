#!/usr/bin/env python3
"""Local guard tests for the Windows/WSL2 launcher; never claims Windows execution."""
import importlib.util
import io
from pathlib import Path
import queue
import tempfile
import unittest
from unittest.mock import Mock, patch


def load(filename, name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


launcher = load('test-windows-linux.py', 'windows_linux_launcher')
probe = load('test-linux-sync.py', 'linux_sync_probe')


class CrossOSGuards(unittest.TestCase):
    def test_actual_wsl2_kernel_and_native_linux_filesystem_are_required(self):
        listing = '  NAME              STATE           VERSION\n* Ubuntu-24.04      Running         2'
        uname = 'Linux 6.6.87.2-microsoft-standard-WSL2 x86_64'
        launcher.validate_wsl('Ubuntu-24.04', listing, uname, 'ext2/ext3')
        for altered in (listing.replace('Running         2', 'Running         1'), listing.replace('Ubuntu-24.04', 'Ubuntu-22.04')):
            with self.assertRaises(ValueError):
                launcher.validate_wsl('Ubuntu-24.04', altered, uname, 'ext2/ext3')
        for kernel in ('Linux 4.4.0-19041-Microsoft x86_64', 'Linux 6.6.87.2-microsoft-standard-WSL2 aarch64', 'Darwin 24.6.0 x86_64'):
            with self.assertRaises(ValueError):
                launcher.validate_wsl('Ubuntu-24.04', listing, kernel, 'ext2/ext3')
        for filesystem in ('9p', 'drvfs', 'fuseblk', 'ntfs'):
            with self.assertRaises(ValueError):
                launcher.validate_wsl('Ubuntu-24.04', listing, uname, filesystem)

    def test_windows_wsl_management_and_linux_output_encodings(self):
        text = 'Ubuntu-24.04 运行中 2'
        for encoding in ('utf-8', 'utf-16', 'utf-16-le'):
            self.assertEqual(launcher.decoded(text.encode(encoding)), text)

    def test_no_non_loopback_credential_target_or_redirect_is_accepted(self):
        self.assertEqual(probe.loopback_server('http://127.0.0.1:8181'), 'http://127.0.0.1:8181')
        for target in ('http://127.0.0.1:8181@evil.example', 'http://127.0.0.1:8181.evil.example',
                       'http://localhost:8181', 'https://127.0.0.1:8181', 'http://127.0.0.1:8181/path',
                       'http://127.0.0.1:8181?next=evil', 'http://127.0.0.1:8181#x',
                       'http://127.0.0.1:0', 'http://127.0.0.1:65536'):
            with self.subTest(target=target), self.assertRaises(ValueError):
                probe.loopback_server(target)
        with self.assertRaises(ValueError):
            probe.NoRedirect().redirect_request(None, None, 302, None, {}, 'https://evil.example')

    def test_cleanup_is_limited_to_launcher_owned_linux_directory_shape(self):
        self.assertEqual(launcher.private_directory('/tmp/agentbox-windows-peer.Ab123456'), '/tmp/agentbox-windows-peer.Ab123456')
        for path in ('/tmp', '/', '/tmp/agentbox-windows-peer.Ab123456/../../', '/mnt/c/agentbox-windows-peer.Ab123456', '/tmp/agentbox-windows-peer.*'):
            with self.assertRaises(ValueError):
                launcher.private_directory(path)

    def test_cross_built_binary_cannot_be_used_in_place_of_native_windows_sidecar(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            elf = bytearray(64)
            elf[:6] = b'\x7fELF\x02\x01'
            elf[18:20] = (62).to_bytes(2, 'little')
            pe = bytearray(128)
            pe[:2] = b'MZ'
            pe[60:64] = (80).to_bytes(4, 'little')
            pe[80:84] = b'PE\0\0'
            pe[84:86] = (0x8664).to_bytes(2, 'little')
            linux = directory / 'linux.test'; linux.write_bytes(elf)
            windows = directory / 'abox-sync.exe'; windows.write_bytes(pe)
            report = launcher.validate_binaries(linux, windows)
            self.assertEqual(len(report['linux_peer_sha256']), 64)
            with self.assertRaises(ValueError):
                launcher.validate_binaries(linux, linux)
            with self.assertRaises(ValueError):
                launcher.validate_binaries(windows, windows)

    def test_closed_hung_or_oversized_sidecar_response_fails_instead_of_hanging(self):
        events = queue.Queue()
        with self.assertRaises(TimeoutError):
            probe.next_event(events, 0.01)
        probe.read_events(io.StringIO(''), events)
        with self.assertRaises(EOFError):
            probe.next_event(events)
        probe.read_events(io.StringIO('x' * (1024 * 1024 + 1)), events)
        with self.assertRaises(ValueError):
            probe.next_event(events)
        probe.read_events(io.StringIO('{"type":"ready","text":"中文"}\n'), events)
        self.assertEqual(probe.next_event(events)['text'], '中文')

    def test_kernel_termination_requires_expected_os_exit_and_a_live_process(self):
        for native_os, code, mechanism in [('nt', 1, 'TerminateProcess'), ('posix', -9, 'SIGKILL')]:
            native = probe.NativeSidecar.__new__(probe.NativeSidecar)
            native.process = Mock()
            native.process.pid = 54321
            native.process.poll.return_value = None
            native.process.wait.return_value = code
            with patch.object(probe.os, 'name', native_os), patch.object(probe.signal, 'SIGKILL', 9, create=True):
                self.assertEqual(native.kill()['mechanism'], mechanism)
                native.process.kill.assert_called_once()
                native.process.terminate.assert_not_called()
                native.process.poll.return_value = 0
                with self.assertRaises(ValueError):
                    native.kill()
                native.process.poll.return_value = None
                native.process.wait.return_value = 0
                with self.assertRaises(ValueError):
                    native.kill()

    def test_missing_or_non_windows_crash_evidence_cannot_pass_wsl_report(self):
        report = {'cross_os_sync': 'passed', 'native_os': 'Windows', 'peer': 'linux', 'peer_arch': 'amd64'}
        with self.assertRaises(ValueError):
            launcher.validate_sync_report(report)
        crash = {'status': 'passed', 'mechanism': 'TerminateProcess', 'exit_code': 1, 'apply_calls_delta': 1,
                 'lease_ttl_seconds': 30, 'archived_action': 'replan', 'pid': 123, 'restarted_pid': 456,
                 'real_linux_publication': True, 'durable_started_intent': True, 'replay_rejected': True,
                 'real_lease_expiry': True, 'before_exported': True, 'baseline_unchanged': True, 'both_user_edits_preserved': True}
        report['kernel_kill_recovery'] = crash
        launcher.validate_sync_report(report)
        for field, value in [('mechanism', 'SIGKILL'), ('apply_calls_delta', 2), ('lease_ttl_seconds', 0),
                             ('baseline_unchanged', False), ('both_user_edits_preserved', False), ('restarted_pid', 123)]:
            with self.subTest(field=field), self.assertRaises(ValueError):
                launcher.validate_sync_report({**report, 'kernel_kill_recovery': {**crash, field: value}})

    def test_fixture_controls_accept_no_arbitrary_paths_or_missing_run_identity(self):
        opener = Mock()
        for action, run_id in [('../../anything', 'run-id'), ('command', 'run-id'), ('arm', '')]:
            with self.assertRaises(ValueError):
                probe.fixture_request(opener, 'http://127.0.0.1:8181', run_id, action, 'POST')
        opener.open.assert_not_called()


if __name__ == '__main__':
    unittest.main()
