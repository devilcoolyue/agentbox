#!/usr/bin/env python3
"""Upgrade-harness regression checks; optional real bundled sidecar integration."""
import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import tarfile
import tempfile
import unittest
from unittest.mock import patch


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


probe = load('test-installed')
previous = load('prepare-previous')
SIDECAR = None


class UpgradeHarnessTests(unittest.TestCase):
    def test_versions_reject_same_downgrade_and_bad_prerelease(self):
        versions = ['0.1.0-alpha.2', '0.1.0-alpha.10', '0.1.0-beta', '0.1.0', '0.1.1']
        self.assertEqual(sorted(versions, key=probe.semver), versions)
        self.assertEqual(probe.semver('0.1.0+old'), probe.semver('0.1.0+new'))
        for value in ('v0.1.0', '01.1.0', '0.1.0-01', '0.1.0-alpha..1'):
            with self.assertRaises(ValueError):
                probe.semver(value)

    def test_windows_install_requires_disposable_hosted_user(self):
        with patch.object(probe.os, 'name', 'nt'), patch.dict(os.environ, {}, clear=True):
            with self.assertRaises(ValueError):
                probe.check_windows_isolation(True)
            with patch.dict(os.environ, {'GITHUB_ACTIONS': 'true', 'RUNNER_ENVIRONMENT': 'github-hosted'}):
                with self.assertRaises(ValueError):
                    probe.check_windows_isolation(False)
                probe.check_windows_isolation(True)

    def test_previous_source_is_bound_to_repository_tag_and_platform(self):
        url = 'https://github.com/devilcoolyue/agentbox/releases/download/desktop-v0.1.0/Agentbox.app.tar.gz'
        manifest = {'version': '0.1.0', 'platforms': {'darwin-aarch64': {'url': url}}}
        self.assertEqual(previous.select_asset(manifest, 'desktop-v0.1.0', 'aarch64-apple-darwin'), 'Agentbox.app.tar.gz')
        for bad in (url.replace('devilcoolyue', 'stranger'), url.replace('desktop-v0.1.0', 'desktop-v0.0.9'), url + '?token=x', url.replace('Agentbox.app', '../Agentbox.app'), url.replace('.app.tar.gz', '.exe')):
            manifest['platforms']['darwin-aarch64']['url'] = bad
            with self.assertRaises(ValueError):
                previous.select_asset(manifest, 'desktop-v0.1.0', 'aarch64-apple-darwin')
        for tag in ('v0.1.0', 'desktop-stable', '../desktop-v0.1.0', 'desktop-v0.1.0?x'):
            with self.assertRaises(ValueError):
                previous.validate_tag(tag)

    def test_checksums_are_mandatory_unique_and_exact(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'Agentbox.exe'
            path.write_bytes(b'package')
            checksum = hashlib.sha256(b'package').hexdigest() + '  Agentbox.exe'
            previous.verify_checksum(path, checksum)
            for invalid in ('', checksum + '\n' + checksum, checksum.replace('Agentbox.exe', 'Other.exe'), '0' * 64 + '  Agentbox.exe'):
                with self.assertRaises(ValueError):
                    previous.verify_checksum(path, invalid)

    def test_archive_rejects_traversal_and_escaping_link(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for number, (name, link) in enumerate((('../outside', None), ('Agentbox.app/escape', '../../outside'))):
                archive = root / f'bad-{number}.tar.gz'
                with tarfile.open(archive, 'w:gz') as bundle:
                    info = tarfile.TarInfo(name)
                    if link:
                        info.type = tarfile.SYMTYPE
                        info.linkname = link
                    else:
                        info.size = 1
                    bundle.addfile(info, None if link else io.BytesIO(b'x'))
                with self.assertRaises((ValueError, tarfile.FilterError)):
                    previous.extract_app(archive, root / f'extract-{number}')
            self.assertFalse((root / 'outside').exists())

    def test_real_sidecar_retains_old_schema_and_detects_state_change(self):
        if SIDECAR is None:
            self.skipTest('Supply --sidecar for native IPC/SQLite migration check')
        with tempfile.TemporaryDirectory() as temporary, probe.fixture_peer() as server:
            root = Path(temporary).resolve()
            state, local, binding_id, original = probe.seed_state(SIDECAR, root, server)
            # This is deliberately a synthetic schema-4 migration fixture, not
            # evidence that an actual previous desktop package was installed.
            with sqlite3.connect(state / 'sync.db') as database:
                database.execute('PRAGMA user_version=4')
            before = probe.state_snapshot(state)
            result = probe.verify_retention(SIDECAR, state, local, binding_id, before, server)
            self.assertEqual(result['schema_after'], original['schema'])
            self.assertTrue(result['schema_migrated'])
            with sqlite3.connect(state / 'sync.db') as database:
                database.execute("UPDATE metadata SET value=? WHERE key='device'", ('b' * 32,))
            with self.assertRaisesRegex(ValueError, 'device, binding, baseline or history'):
                probe.verify_retention(SIDECAR, state, local, binding_id, before, server)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--sidecar', type=Path)
    args, remaining = parser.parse_known_args()
    SIDECAR = args.sidecar.resolve() if args.sidecar else None
    unittest.main(argv=[__file__, *remaining])
