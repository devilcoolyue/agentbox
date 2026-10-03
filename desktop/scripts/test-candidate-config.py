import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT=Path(__file__).with_name('candidate-config.py')
VERSION=json.loads((SCRIPT.parent.parent/'package.json').read_text())['version']
class CandidateTests(unittest.TestCase):
    def run_config(self,path,*options,**environment):
        env={key:value for key,value in os.environ.items() if not key.startswith(('APPLE_','TAURI_SIGNING_','AGENTBOX_UPDATER_','WINDOWS_CERTIFICATE_'))}
        env.update(environment)
        return subprocess.run([sys.executable,str(SCRIPT),'--version',VERSION,'--target','aarch64-apple-darwin','--output',str(path),*options],env=env,capture_output=True,text=True)
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
            config=json.loads(path.read_text())
            self.assertFalse(config['bundle']['createUpdaterArtifacts'])
            self.assertEqual(config['bundle']['windows']['nsis']['installMode'],'currentUser')
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
