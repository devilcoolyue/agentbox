import importlib.util
from pathlib import Path
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

if __name__ == '__main__':
    unittest.main()
