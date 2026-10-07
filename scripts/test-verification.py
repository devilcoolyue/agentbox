#!/usr/bin/env python3
"""Runner behavior tests. Uses only synthetic commands and temporary directories."""
import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import verify
from verification_catalog import STEPS, PROFILES, EXTERNAL, COVERED


class VerificationTests(unittest.TestCase):
    def entry(self, name, script):
        return dict(id=name, category='offline', argv=[sys.executable,'-c',script], note='synthetic fixture')

    def run_fixture(self, steps, **options):
        with tempfile.TemporaryDirectory() as base:
            root=Path(base); destination=root/'evidence'
            with contextlib.redirect_stdout(io.StringIO()):
                code=verify.run_steps(steps,destination,{'python':sys.executable,**options},'fixture',root=root)
            report=json.loads((destination/'report.json').read_text())
            logs={p.name:p.read_text() for p in destination.glob('*.log')}
            return code,report,logs

    def test_failure_keeps_logs_and_does_not_run_later_steps(self):
        code,report,logs=self.run_fixture([
            self.entry('first','print("synthetic failure"); raise SystemExit(7)'),
            self.entry('later','raise RuntimeError("must not execute")')])
        self.assertEqual(code,1)
        self.assertEqual(report['status'],'incomplete')
        self.assertEqual([s['status'] for s in report['steps']],['failed','not_run'])
        self.assertEqual(report['steps'][0]['exit_code'],7)
        self.assertIn('synthetic failure',logs['first.log'])
        self.assertNotIn('later.log',logs)

    def test_missing_input_is_blocked_not_passed_or_skipped(self):
        step=dict(id='needs-binary',category='docker',argv=[sys.executable,'{binary}'],note='fixture')
        code,report,logs=self.run_fixture([step])
        self.assertEqual(code,2)
        self.assertEqual(report['steps'][0]['status'],'blocked')
        self.assertIn('--binary',report['steps'][0]['reason'])
        self.assertEqual(logs,{})

    def test_timeout_reaps_process_and_reports_incomplete(self):
        code,report,_=self.run_fixture([self.entry('wait','import time; time.sleep(60)')],timeout=0.1)
        self.assertEqual(code,1)
        self.assertEqual(report['steps'][0]['status'],'timed_out')
        self.assertIsNotNone(report['steps'][0]['exit_code'])

    def test_cancellation_preserves_evidence(self):
        with tempfile.TemporaryDirectory() as base:
            def interrupted(argv,cwd,env,log,timeout):
                log.write_text('synthetic interrupted command')
                return -2,'cancelled'
            destination=Path(base)/'report'
            with contextlib.redirect_stdout(io.StringIO()):
                code=verify.run_steps([self.entry('cancel','')],destination,{},'fixture',root=Path(base),executor=interrupted)
            self.assertEqual(code,130)
            self.assertEqual(json.loads((destination/'report.json').read_text())['steps'][0]['status'],'cancelled')

    def test_linux_server_keeps_privileged_execution_and_denial_gate(self):
        with patch.object(verify.sys,'platform','linux'), patch.object(verify.os,'geteuid',return_value=1001,create=True), patch.object(verify.subprocess,'run',return_value=subprocess.CompletedProcess([],0)) as run:
            self.assertEqual(verify.helper('_go_server'),0)
            self.assertEqual(run.call_args.args[0],['go','test','-json','-exec','sudo -n --','./internal/server'])
        with patch.object(verify.os,'geteuid',return_value=0,create=True), patch.object(verify.shutil,'which',return_value='/synthetic/go'):
            with self.assertRaisesRegex(ValueError,'non-root'):
                verify.resolve_step(verify.BY_ID['go.chown-denial'],{},verify.clean_env())

    def test_race_uses_linux_ownership_and_explicit_tiers(self):
        with patch.object(verify.sys,'platform','linux'), patch.object(verify.os,'geteuid',return_value=1001,create=True), patch.object(verify.subprocess,'run',return_value=subprocess.CompletedProcess([],0)) as run:
            self.assertEqual(verify.helper('_race_chat'),0)
            argv=run.call_args.args[0]
            self.assertIn('-race',argv)
            self.assertEqual(argv[argv.index('-exec')+1],'sudo -n --')
            self.assertNotIn('./internal/chat',argv)
            self.assertIn('./internal/server',argv)
            ordinary=run.call_args_list[0].args[0]
            self.assertIn('./internal/chat',ordinary)
            self.assertNotIn('-exec',ordinary)
        self.assertEqual(PROFILES['pr'],PROFILES['quick'])
        self.assertIn('race.chat',PROFILES['linux-integration'])
        self.assertIn('docker.chat-reliability',PROFILES['linux-integration'])
        self.assertIn('backup',PROFILES['release-full'])

    def test_environment_cannot_enable_paid_live_or_partial_tests(self):
        env=verify.clean_env({'PATH':'tools','CODEX_LIVE_TEST':'1','MARKET_LIVE_TEST':'1',
            'AGENTBOX_CLI_TEST_IMAGE':'real-image','AGENTBOX_SYNC_PROCESS_TEST':'1',
            'AGENTBOX_BROWSER_ONLY_MCP':'1','AGENTBOX_TEST_MIGRATION_CRASH':'private-database',
            'AGENTBOX_PLAYWRIGHT_MODULE':'fixture-module','GOFLAGS':'-tags=custom','GOOS':'windows'})
        self.assertEqual(env,{'PATH':'tools','AGENTBOX_PLAYWRIGHT_MODULE':'fixture-module'})

    def test_existing_evidence_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as root:
            path=Path(root)/'report';path.mkdir();(path/'report.json').write_text('original')
            with self.assertRaises(FileExistsError): verify.run_steps([],path,{},'fixture')
            self.assertEqual((path/'report.json').read_text(),'original')

    def test_go_json_reports_skipped_tests_as_skipped(self):
        with tempfile.TemporaryDirectory() as base:
            log=Path(base)/'go.log';log.write_text('\n'.join(json.dumps(e) for e in [
                {'Action':'skip','Package':'fixture','Test':'LiveModel'},
                {'Action':'pass','Package':'fixture','Test':'Offline'},
                {'Action':'pass','Package':'fixture'},
                {'Action':'output','Package':'fixture','Output':'ok\tfixture\t(cached)\n'}]))
            result=verify.go_evidence(log)
            self.assertEqual(result['counts'],{'passed':1,'failed':0,'skipped':1})
            self.assertEqual(result['cached_packages'],['fixture'])
            self.assertEqual(result['skipped_tests'],[{'package':'fixture','test':'LiveModel'}])

    def test_artifact_gate_detects_stale_and_orphaned_outputs_in_dirty_tree(self):
        with tempfile.TemporaryDirectory() as base:
            root=Path(base)
            for directory in ['web/src','internal/web/static/js','internal/linkapp/static']:(root/directory).mkdir(parents=True)
            (root/'web/src/main.ts').write_text('synthetic source')
            js=root/'internal/web/static/js/main.js';js.write_text('stale')
            (root/'internal/linkapp/static/i18n-core.js').write_text('fixture')
            def build(*_args,**_kwargs):js.write_text('fresh');return subprocess.CompletedProcess([],0)
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(verify.web_artifacts(root,build),1)
                self.assertEqual(verify.web_artifacts(root,build),0)
                (root/'internal/web/static/js/orphan.js').write_text('removed source')
                self.assertEqual(verify.web_artifacts(root,build),1)
            self.assertEqual(js.read_text(),'fresh','build result should remain reviewable')

    def test_shell_gate_checks_all_scripts_not_only_first_bash_argument(self):
        with tempfile.TemporaryDirectory() as base:
            root=Path(base);(root/'scripts').mkdir()
            (root/'install.sh').write_text('true\n')
            (root/'scripts/test-filesystem-linux.sh').write_text('true\n')
            (root/'scripts/test-admin-password-linux.sh').write_text('if true; then\n')
            def quiet(*args,**kwargs): return subprocess.run(*args,**kwargs,capture_output=True)
            self.assertNotEqual(verify.shell_syntax(root,quiet),0)

    def test_test_scripts_have_explicit_entry_or_parent_and_no_paid_default(self):
        known={arg for s in STEPS for arg in s['argv'] if arg.startswith(('scripts/','desktop/scripts/'))}
        known.update(EXTERNAL);known.update(COVERED)
        discovered={str(p.relative_to(verify.ROOT)) for root in ['scripts','desktop/scripts']
                    for p in (verify.ROOT/root).glob('test-*') if p.suffix in ('.py','.mjs','.sh')}
        self.assertEqual(sorted(discovered-known),[], 'classify every new verification script')
        ids={s['id'] for s in STEPS};self.assertEqual(len(ids),len(STEPS))
        for values in PROFILES.values(): self.assertTrue(set(values)<=ids)
        self.assertTrue(set(COVERED.values())<=ids)
        self.assertFalse(any('CODEX_LIVE_TEST' in s.get('env',{}) for s in STEPS))
        for path in EXTERNAL:self.assertTrue((verify.ROOT/path).exists(),path)


if __name__=='__main__': unittest.main()
