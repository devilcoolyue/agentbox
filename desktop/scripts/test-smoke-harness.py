#!/usr/bin/env python3
"""Validate smoke launching and process ownership without opening any app."""
import importlib.util
import json
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, call, patch

spec = importlib.util.spec_from_file_location('smoke_runner', Path(__file__).with_name('smoke.py'))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class SmokeHarness(unittest.TestCase):
    def sync_environment(self, root):
        config={'server':'http://127.0.0.1:48321/'}
        for key in ('local','state','export'):
            path=root/(key+' 中文');path.mkdir();config[key]=str(path)
        return {'AGENTBOX_SMOKE_SYNC':json.dumps(config,ensure_ascii=False),
                'AGENTBOX_SMOKE_REPORT':str(root/'report.json'),'AGENTBOX_SMOKE_MODE':'projects'}

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

    def test_sync_fixture_preserves_go_paths_and_rejects_stale_or_remote_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory).resolve();environment=self.sync_environment(root)
            report,forwarded=runner.sync_fixture_environment(environment)
            self.assertEqual(report,root/'report.json')
            self.assertEqual(forwarded['AGENTBOX_SMOKE_SYNC'],environment['AGENTBOX_SMOKE_SYNC'])
            self.assertNotIn('AGENTBOX_SMOKE_MODE',forwarded)
            self.assertIn('AGENTBOX_SMOKE_MODE',environment)
            for server in ('https://example.com/','http://127.0.0.1/','http://user@127.0.0.1:1234/','http://127.0.0.1:1234/other','http://127.0.0.1:1234/?token=x'):
                config=json.loads(environment['AGENTBOX_SMOKE_SYNC']);config['server']=server
                with self.assertRaises(ValueError):runner.sync_fixture_environment({**environment,'AGENTBOX_SMOKE_SYNC':json.dumps(config)})
            with self.assertRaises(ValueError):runner.sync_fixture_environment({**environment,'AGENTBOX_SMOKE_COMPAT':'{}'})
            with self.assertRaises(ValueError):runner.sync_fixture_environment({**environment,'AGENTBOX_SMOKE_REPORT':'relative.json'})
            report.write_text('{"ok":true}',encoding='utf-8')
            with self.assertRaises(ValueError):runner.sync_fixture_environment(environment)
            report.unlink();report.with_suffix('.pid').write_text('424242',encoding='utf-8')
            with self.assertRaises(ValueError):runner.sync_fixture_environment(environment)

    def test_sync_main_reuses_existing_fixture_with_170_second_budget(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory).resolve();environment=self.sync_environment(root)
            with patch.object(runner.sys,'argv',['smoke.py','/isolated/Agentbox Smoke.app/Contents/MacOS/agentbox-desktop','--sync-fixture']), \
                    patch.dict(runner.os.environ,environment,clear=True), \
                    patch.object(runner,'macos_session_diagnostics',return_value=None), \
                    patch.object(runner,'run_smoke') as run:
                runner.main()
            self.assertEqual(run.call_args.args[1],root/'report.json')
            self.assertEqual(run.call_args.kwargs,{'timeout_seconds':170,'require_sync':True})
            self.assertEqual(run.call_args.args[2]['AGENTBOX_SMOKE_SYNC'],environment['AGENTBOX_SMOKE_SYNC'])

    def test_sync_timeout_cleans_owned_app_and_wait_wrapper(self):
        with tempfile.TemporaryDirectory() as directory:
            report=Path(directory)/'report.json';binary=Path('/isolated/smoke')
            process=Mock();process.wait.side_effect=[subprocess.TimeoutExpired(['open'],170),0];process.poll.return_value=None
            with patch.object(runner,'launch_command',return_value=(['open'],'launch-services')), \
                    patch.object(runner.subprocess,'Popen',return_value=process), \
                    patch.object(runner,'stop_owned_application') as stop, \
                    patch.object(runner,'print_diagnostics'):
                with self.assertRaisesRegex(SystemExit,'timed out'):
                    runner.run_smoke(binary,report,{},timeout_seconds=170,require_sync=True)
            stop.assert_called_once_with(binary,report)
            process.kill.assert_called_once()
            self.assertEqual(process.wait.call_args_list,[call(timeout=170),call(timeout=10)])

    def test_launcher_success_requires_native_sync_report_and_always_cleans_up(self):
        with tempfile.TemporaryDirectory() as directory:
            report=Path(directory)/'report.json';binary=Path('/isolated/smoke')
            for native,passes in [({'ok':True,'sync_mode':True},True),({'ok':True,'sync_mode':False},False),({'ok':False,'sync_mode':True},False)]:
                report.write_text(json.dumps(native),encoding='utf-8')
                process=Mock();process.wait.return_value=0;process.poll.return_value=0
                with patch.object(runner,'launch_command',return_value=(['open'],'launch-services')), \
                        patch.object(runner.subprocess,'Popen',return_value=process), \
                        patch.object(runner,'stop_owned_application') as stop, \
                        patch.object(runner,'print_diagnostics'),patch('builtins.print'):
                    if passes:self.assertTrue(runner.run_smoke(binary,report,{},require_sync=True)['ok'])
                    else:
                        with self.assertRaises(SystemExit):runner.run_smoke(binary,report,{},require_sync=True)
                stop.assert_called_once_with(binary,report)
                process.kill.assert_not_called()


if __name__ == '__main__':
    unittest.main()
