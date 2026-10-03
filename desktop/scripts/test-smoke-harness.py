#!/usr/bin/env python3
"""Validate smoke launching and process ownership without opening any app."""
import importlib.util
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('smoke_runner', Path(__file__).with_name('smoke.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class SmokeHarness(unittest.TestCase):
    def test_launchservices_is_limited_to_explicit_smoke_bundle_and_fixture_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            contents = root / 'Agentbox Smoke.app/Contents'
            (contents / 'MacOS').mkdir(parents=True)
            binary = contents / 'MacOS/agentbox-desktop'
            info = {'CFBundleIdentifier': 'io.github.devilcoolyue.agentbox.desktop.smoke', 'CFBundleExecutable': binary.name}
            (contents / 'Info.plist').write_bytes(plistlib.dumps(info))
            command, kind = runner.launch_command(binary, root / 'report.json', {'AGENTBOX_SMOKE_MODE': 'projects', 'UNRELATED_ENV': 'not forwarded'}, 'darwin')
            self.assertEqual(kind, 'launch-services')
            self.assertIn('AGENTBOX_SMOKE_MODE=projects', command)
            self.assertFalse(any('UNRELATED_ENV' in part for part in command))
            self.assertNotIn('--background', command)
            info['CFBundleIdentifier'] = 'io.github.devilcoolyue.agentbox.desktop'
            (contents / 'Info.plist').write_bytes(plistlib.dumps(info))
            with self.assertRaises(ValueError):
                runner.launch_command(binary, root / 'report.json', {}, 'darwin')

    def test_unbundled_ci_and_windows_keep_direct_execution(self):
        binary = Path('/isolated/build/agentbox-desktop')
        for system in ('darwin', 'win32'):
            self.assertEqual(runner.launch_command(binary, Path('/report.json'), {}, system), ([str(binary)], 'direct-executable'))

    def test_timeout_cleanup_refuses_a_reused_or_unrelated_pid(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / 'report.json'
            report.with_suffix('.pid').write_text('424242')
            binary = Path('/owned/Agentbox Smoke.app/Contents/MacOS/agentbox-desktop')
            wrong = subprocess.CompletedProcess([], 0, '/Applications/Agentbox.app/Contents/MacOS/agentbox-desktop\n')
            with patch.object(runner.subprocess, 'run', return_value=wrong), patch.object(runner.os, 'kill') as kill:
                with self.assertRaises(ValueError):
                    runner.stop_owned_application(binary, report)
                kill.assert_not_called()
            report.with_suffix('.pid').write_text('0')
            with self.assertRaises(ValueError):
                runner.stop_owned_application(binary, report)

    def test_locked_session_fails_before_launching_any_application(self):
        with patch.object(runner.sys, 'argv', ['smoke.py', '/isolated/agentbox-desktop']), \
                patch.object(runner, 'macos_session_diagnostics', return_value={'screen_locked': True}), \
                patch.object(runner.subprocess, 'Popen') as launch, patch('builtins.print'):
            with self.assertRaisesRegex(SystemExit, 'unlocked desktop'):
                runner.main()
            launch.assert_not_called()


if __name__ == '__main__':
    unittest.main()
