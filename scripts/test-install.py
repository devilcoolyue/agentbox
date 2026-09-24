#!/usr/bin/env python3
"""Installer regression tests with synthetic archives and simulated systemd/Docker."""
import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / 'deploy'))
spec = importlib.util.spec_from_file_location('bootstrap', ROOT / 'deploy/bootstrap.py')
bootstrap = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = bootstrap
spec.loader.exec_module(bootstrap)
EXTRACTOR = (ROOT / 'install.sh').read_text().split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]


class Archives(unittest.TestCase):
    def extract(self, members=None, checksum='valid'):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            name = 'agentbox_v1.0.0_linux_amd64.tar.gz'
            folder = name[:-7]
            with tarfile.open(root / name, 'w:gz') as archive:
                for member in members or [(folder + '/agentbox', tarfile.REGTYPE, '')]:
                    entry = tarfile.TarInfo(member[0]); entry.type = member[1]; entry.linkname = member[2]
                    entry.mode = 0o755
                    data = b'synthetic executable' if entry.isfile() else b''
                    entry.size = len(data)
                    archive.addfile(entry, io.BytesIO(data))
            digest = hashlib.sha256((root / name).read_bytes()).hexdigest()
            line = digest + '  ' + name + '\n'
            if checksum == 'tampered': line = '0' * 64 + '  ' + name + '\n'
            if checksum == 'missing': line = digest + '  unrelated.tar.gz\n'
            if checksum == 'duplicate': line += line
            (root / 'SHA256SUMS').write_text(line)
            result = subprocess.run([sys.executable, '-c', EXTRACTOR, str(root), name], capture_output=True, text=True)
            if result.returncode == 0:
                self.assertEqual((root / folder / 'agentbox').read_bytes(), b'synthetic executable')
                self.assertEqual((root / folder / 'agentbox').stat().st_mode & 0o777, 0o755)
            return result

    def test_selected_asset_verified_and_executable_preserved(self):
        result = self.extract()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_bad_checksum_stops_before_extraction(self):
        for kind in ('tampered', 'missing', 'duplicate'):
            with self.subTest(kind=kind): self.assertNotEqual(self.extract(checksum=kind).returncode, 0)

    def test_unsafe_archives_rejected(self):
        root = 'agentbox_v1.0.0_linux_amd64/'
        for name, kind, target in [
            ('../escape', tarfile.REGTYPE, ''), ('/tmp/escape', tarfile.REGTYPE, ''),
            (root + '../escape', tarfile.REGTYPE, ''), ('other/file', tarfile.REGTYPE, ''),
            (root + 'link', tarfile.SYMTYPE, '/etc'), (root + 'link', tarfile.LNKTYPE, '/etc/passwd'),
            (root + 'fifo', tarfile.FIFOTYPE, ''),
        ]:
            with self.subTest(name=name, kind=kind):
                self.assertNotEqual(self.extract([(name, kind, target)]).returncode, 0)
        member = (root + 'agentbox', tarfile.REGTYPE, '')
        self.assertNotEqual(self.extract([member, member]).returncode, 0)


class Installation(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.layout = bootstrap.Layout(root / 'app', root / 'etc/config.json', root / 'data', root / 'cache', root / 'units')
        self.package = root / 'package'; self.package.mkdir()
        (self.package / 'build.json').write_text(json.dumps(dict(program='agentbox', os='linux', arch='amd64', version='v1.2.3', revision='clean')))
        (self.package / 'config.example.json').write_text((ROOT / 'config.example.json').read_text())
        for path in ['agentbox', 'deploy/release.py', 'scripts/build-image.sh', 'images/agent/versions.env']:
            file = self.package / path; file.parent.mkdir(exist_ok=True, parents=True); file.touch()
        self.calls = []

    def fake_run(self, *args, **kwargs):
        self.calls.append(args)
        if args[:2] == ('systemctl', 'show'): return ''
        if args[:3] == ('docker', 'network', 'inspect'):
            return json.dumps([{'IPAM': {'Config': [{'Gateway': '172.23.0.1'}]}}])
        if 'check-config' in args:
            config = json.loads(Path(args[-1]).read_text())
            self.assertEqual(config['accounts'], [])
            self.assertEqual(config['proxies'], [])
            self.assertEqual(config['proxy_bridge']['bind'], '172.23.0.1:1081')
        if 'install' in args:
            self.layout.app.mkdir(); self.layout.data.mkdir()
        return ''

    def execute(self, build_error=None, run=None):
        output = io.StringIO()
        with patch.object(bootstrap.release, 'run', side_effect=run or self.fake_run), \
             patch.object(bootstrap.subprocess, 'run', side_effect=build_error) as build, \
             patch.object(bootstrap, 'available'), patch.object(bootstrap.platform, 'machine', return_value='x86_64'), \
             contextlib.redirect_stdout(output):
            bootstrap.install(self.package, '0.0.0.0:8180', self.layout)
        return output.getvalue(), build

    def test_new_install_has_private_config_and_pinned_image(self):
        output, build = self.execute()
        cfg = json.loads(self.layout.config.read_text())
        self.assertEqual(cfg['agent_image'], 'agentbox-agent:v1.2.3')
        self.assertGreaterEqual(len(cfg['auth_token']), 40)
        self.assertEqual(self.layout.config.stat().st_mode & 0o777, 0o600)
        self.assertEqual(self.layout.config.parent.stat().st_mode & 0o777, 0o700)
        self.assertIn(cfg['auth_token'], output)
        self.assertIn('http://<服务器IP>:8180', output)
        self.assertEqual(build.call_args.kwargs['env']['AGENTBOX_VERSIONED_IMAGE'], cfg['agent_image'])
        self.assertTrue(any('activate' in call for call in self.calls))
        with self.assertRaises(ValueError): self.execute()
        self.assertEqual(json.loads(self.layout.config.read_text()), cfg)

    def test_existing_paths_and_service_never_overwritten(self):
        for path in (self.layout.data, self.layout.config.parent, self.layout.app, self.layout.cache):
            path.mkdir()
            with self.assertRaises(ValueError): self.execute()
            self.assertEqual(list(path.iterdir()), [])
            path.rmdir()
        with self.assertRaises(ValueError): self.execute(run=lambda *a, **kw: '/legacy/agentbox.service')
        self.assertFalse(self.layout.config.exists())

    def test_build_failure_does_not_leave_installation_paths(self):
        with self.assertRaises(subprocess.CalledProcessError):
            self.execute(build_error=subprocess.CalledProcessError(1, 'docker build'))
        for path in (self.layout.app, self.layout.config.parent, self.layout.data, self.layout.cache):
            self.assertFalse(path.exists())

    def test_activation_failure_retains_config_for_recovery(self):
        def fail_activate(*args, **kwargs):
            if 'activate' in args: raise subprocess.CalledProcessError(1, args)
            return self.fake_run(*args, **kwargs)
        with self.assertRaises(subprocess.CalledProcessError): self.execute(run=fail_activate)
        self.assertTrue(self.layout.config.is_file())
        self.assertTrue(self.layout.app.is_dir())

    def test_invalid_listen_rejected(self):
        for value in ('localhost:8180', '0.0.0.0:0', '0.0.0.0:65536', '::1:8180', '127.0.0.1:abc'):
            with self.subTest(value=value), self.assertRaises(ValueError): bootstrap.listen_address(value)
        self.assertEqual(bootstrap.listen_address('[::]:8180')[1], 8180)

    def test_busy_port_rejected(self):
        import socket
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0)); listener.listen()
            with self.assertRaises(OSError): bootstrap.available('127.0.0.1:' + str(listener.getsockname()[1]))


if __name__ == '__main__':
    unittest.main()
