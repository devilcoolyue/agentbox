import json
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT=Path(__file__).with_name('candidate-config.py')
VERSION=json.loads((SCRIPT.parent.parent/'package.json').read_text(encoding='utf-8'))['version']
spec=importlib.util.spec_from_file_location('candidate_config',SCRIPT)
candidate=importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)
class CandidateTests(unittest.TestCase):
    def test_version_grammar_is_valid_semver_without_ambiguous_components(self):
        for version in ['01.2.3','1.02.3','1.2.03','1.2.3-01','1.2.3-rc..1','1.2.3-','١.2.3']:
            self.assertFalse(candidate.valid_version(version),version)
        for version in ['0.1.1','1.2.3-rc.1','1.2.3-0','1.2.3-01a']:
            self.assertTrue(candidate.valid_version(version),version)
    def run_config(self,path,*options,**environment):
        env={key:value for key,value in os.environ.items() if not key.startswith(('APPLE_','TAURI_SIGNING_','AGENTBOX_UPDATER_','WINDOWS_CERTIFICATE_'))}
        env.update(environment)
        env['PYTHONIOENCODING']='utf-8'
        return subprocess.run([sys.executable,str(SCRIPT),'--version',VERSION,'--target','aarch64-apple-darwin','--output',str(path),*options],env=env,capture_output=True,text=True,encoding='utf-8')
    def test_signed_candidate_never_silently_falls_back(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'candidate.json'
            result=self.run_config(path,'--signed')
            self.assertNotEqual(result.returncode,0)
            self.assertFalse(path.exists())
            self.assertIn('Signed candidate requires',result.stderr)
    def test_development_candidate_has_no_updater_artifacts(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'candidate.json'
            result=self.run_config(path)
            self.assertEqual(result.returncode,0,result.stderr)
            config=json.loads(path.read_text(encoding='utf-8'))
            self.assertFalse(config['bundle']['createUpdaterArtifacts'])
            self.assertEqual(config['bundle']['windows']['nsis']['installMode'],'currentUser')
            self.assertEqual(config['bundle']['windows']['nsis']['installerHooks'],'windows/webview2-hooks.nsh')
            self.assertEqual(config['bundle']['windows']['webviewInstallMode'],{'type':'offlineInstaller','silent':True})
            self.assertEqual(config['bundle']['resources']['../vendor-notices/'],'third-party/vendor/')
            self.assertNotIn('plugins',config)
    def test_key_cannot_accidentally_enable_development_updates(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'candidate.json'
            result=self.run_config(path,AGENTBOX_UPDATER_PUBLIC_KEY='synthetic-key')
            self.assertNotEqual(result.returncode,0)
            self.assertFalse(path.exists())

if __name__=='__main__':unittest.main()
