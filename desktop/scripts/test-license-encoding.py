#!/usr/bin/env python3
"""Exercise the license collector with UTF-8 metadata and a cp1252 subprocess locale."""
import importlib.util
import json
import locale
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('license_collector', Path(__file__).with_name('collect-licenses.py'))
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)


class LicenseEncoding(unittest.TestCase):
    def test_non_ascii_metadata_paths_and_crlf_notices_survive_windows_locale(self):
        with tempfile.TemporaryDirectory(prefix='agentbox-license-') as temporary:
            root = Path(temporary) / '依赖 中文'
            desktop = root / 'desktop'
            crate = root / '许可证'
            crate.mkdir(parents=True)
            desktop.mkdir()
            (root / 'third_party').mkdir()
            (root / 'third_party/go-modules.json').write_text('[]', encoding='utf-8')
            (desktop / 'package-lock.json').write_text('{"packages":{}}', encoding='utf-8')
            original = 'Copyright 依赖作者\r\nPermission is granted.\r\n'.encode()
            (crate / 'LICENSE').write_bytes(original)
            metadata = {'packages': [{'id': 'dep', 'name': 'sample', 'version': '1.0.0', 'license': 'MIT',
                          'manifest_path': str(crate / 'Cargo.toml'), 'source': 'registry', 'description': '中文作者'}],
                        'resolve': {'root': 'root', 'nodes': [
                            {'id': 'root', 'deps': [{'pkg': 'dep', 'dep_kinds': [{'kind': None}]}]},
                            {'id': 'dep', 'deps': []}]}}
            fixture = root / 'metadata.json'
            fixture.write_text(json.dumps(metadata, ensure_ascii=False), encoding='utf-8')
            child = root / 'emit.py'
            child.write_text('import pathlib,sys\nsys.stdout.buffer.write(pathlib.Path(sys.argv[1]).read_bytes())\n', encoding='utf-8')
            original_command = subprocess.check_output
            original_read = Path.read_text

            def cargo(_command, **options):
                return original_command([sys.executable, str(child), str(fixture)], **options)

            def read_utf8(path, *args, **kwargs):
                self.assertEqual(kwargs.get('encoding'), 'utf-8', str(path))
                return original_read(path, *args, **kwargs)

            with patch.object(collector, 'DESKTOP', desktop), patch.object(collector, 'ROOT', root), \
                    patch.object(collector, 'OUT', desktop / 'third-party'), \
                    patch.object(collector.subprocess, 'check_output', side_effect=cargo), \
                    patch.object(locale, 'getencoding', return_value='cp1252'), \
                    patch.object(Path, 'read_text', read_utf8):
                collector.main()
            inventory = json.loads((desktop / 'third-party/inventory.json').read_text(encoding='utf-8'))
            self.assertEqual(len(inventory), 1)
            notices = (desktop / 'third-party/NOTICES.txt').read_bytes()
            self.assertIn(original, notices)
            self.assertNotIn(b'\r\r\n', notices)


if __name__ == '__main__':
    unittest.main()
