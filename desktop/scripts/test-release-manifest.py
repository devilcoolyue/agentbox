import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('release', Path(__file__).with_name('release-manifest.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)

class ManifestTests(unittest.TestCase):
    def test_channels_do_not_use_server_latest(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            assets = {target: target + suffix for target, suffix in release.TARGETS.items()}
            for name in assets.values():
                (directory / name).write_bytes(b'fixture only')
                (directory / (name + '.sig')).write_text('synthetic signature (assembly test only)')
            manifest, request, channel, sums = release.prepare(directory, '0.2.0', assets)
            self.assertEqual(request['make_latest'], 'false')
            self.assertEqual(channel['make_latest'], 'false')
            self.assertTrue(request['draft'])
            self.assertEqual(len(manifest['platforms']), 3)
            self.assertEqual(len(sums.splitlines()), 6)
            for platform in manifest['platforms'].values():
                self.assertIn('/releases/download/desktop-v0.2.0/', platform['url'])
                self.assertNotIn('/latest/', platform['url'])
            (directory / (assets['windows-x86_64'] + '.sig')).unlink()
            with self.assertRaises(ValueError):
                release.prepare(directory, '0.2.0', assets)
    def test_missing_architecture_and_bad_version_rejected(self):
        for version, assets in [('v1.0.0', {}), ('1.0.0', {}), ('../main', {})]:
            with self.assertRaises(ValueError):
                release.prepare(Path('.'), version, assets)

    def test_versions_and_asset_names_match_native_client_contract(self):
        for version in ['01.2.3', '1.02.3', '1.2.03', '1.2.3-01', '1.2.3-rc..1', '1.2.3-', '١.2.3']:
            self.assertFalse(release.valid_version(version), version)
        for version in ['0.1.1', '1.2.3-rc.1', '1.2.3-0', '1.2.3-01a']:
            self.assertTrue(release.valid_version(version), version)
        for name in ['Agentbox space.app.tar.gz', 'Agentbox%20.app.tar.gz', 'Agentbox?.app.tar.gz', 'Agentbox#.app.tar.gz', '中文.app.tar.gz']:
            assets = {target: target + suffix for target, suffix in release.TARGETS.items()}
            assets['darwin-aarch64'] = name
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, 'filename'):
                release.prepare(Path('.'), '1.2.3', assets)

    def test_oversized_payload_is_rejected_before_hashing_or_publishing(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            assets = {target: target + suffix for target, suffix in release.TARGETS.items()}
            first = assets['darwin-aarch64']
            with (directory / first).open('wb') as stream:
                stream.truncate(release.DOWNLOAD_LIMIT + 1)
            (directory / (first + '.sig')).write_text('synthetic signature', encoding='utf-8')
            with self.assertRaisesRegex(ValueError, 'download contract'):
                release.prepare(directory, '1.2.3', assets)

    def test_cli_writes_unicode_notes_as_utf8_with_stable_newlines(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            assets = {target: target + suffix for target, suffix in release.TARGETS.items()}
            for name in assets.values():
                (directory / name).write_bytes(b'fixture')
                (directory / (name + '.sig')).write_text('synthetic signature', encoding='utf-8')
            mapping = directory / 'assets.json'; mapping.write_text(json.dumps(assets), encoding='utf-8')
            notes = directory / 'notes.txt'; notes.write_text('更新说明：保留配置\n', encoding='utf-8')
            env = {**os.environ, 'PYTHONUTF8': '0', 'PYTHONCOERCECLOCALE': '0', 'LC_ALL': 'C'}
            subprocess.run([sys.executable, str(Path(__file__).with_name('release-manifest.py')), '--directory', str(directory),
                            '--version', '1.2.3', '--assets', str(mapping), '--notes', str(notes)], env=env, check=True, capture_output=True)
            generated = (directory / 'latest.json').read_bytes()
            self.assertEqual(json.loads(generated)['notes'], '更新说明：保留配置\n')
            self.assertNotIn(b'\r\n', generated)

if __name__ == '__main__':
    unittest.main()
