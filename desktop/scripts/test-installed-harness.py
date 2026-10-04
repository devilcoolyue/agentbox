#!/usr/bin/env python3
"""Upgrade-harness regression checks; optional real bundled sidecar integration."""
import argparse
from contextlib import closing
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch


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

    def empty_registry(self):
        return [{'hive':hive,'view':view,'key':probe.UNINSTALL_KEY,'exists':False,'values':{}}
                for hive in ('HKCU','HKLM') for view in ('32','64')]

    def owned_registry(self, directory):
        rows=self.empty_registry()
        rows[0].update(exists=True,values={'InstallLocation':f'"{directory}"',
                                          'UninstallString':f'"{directory / "uninstall.exe"}"'})
        return rows

    def test_windows_install_passes_nsis_unquoted_unicode_tail_to_createprocess(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory=Path(temporary).resolve()/'Agentbox 中文 安装';directory.mkdir()
            package=directory.parent/'installer 中文 package.exe'
            executable=directory/'agentbox-desktop.exe';executable.write_bytes(b'fixture')
            guard=Mock()
            # Observe the actual subprocess argument, not just the string helper.
            with patch.object(probe.os,'name','nt'), patch.object(probe.subprocess,'run') as execute:
                self.assertEqual(probe.install(package,directory,guard),executable)
            guard.prepare.assert_called_once_with(package)
            guard.verify_executable.assert_called_once()
            command=execute.call_args.args[0]
            self.assertIsInstance(command,str)
            self.assertEqual(command.split(' /D=',1)[1],str(directory))
            self.assertFalse(execute.call_args.kwargs['shell'])
            self.assertEqual(command,subprocess.list2cmdline([str(package),'/S'])+' /D='+str(directory))
            self.assertNotEqual(command,subprocess.list2cmdline([str(package),'/S','/D='+str(directory)]))

    def test_preexisting_registration_is_rejected_before_any_installer_or_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory=Path(temporary).resolve()
            rows=self.empty_registry();rows[3]['exists']=True
            with patch.object(probe,'app_registry',return_value=rows), patch.object(probe.subprocess,'run') as execute:
                with self.assertRaisesRegex(ValueError,'already registered'):
                    probe.WindowsInstallGuard(directory)
                execute.assert_not_called()

    def test_owned_previous_and_new_payloads_share_one_guard_and_clean_up(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory=Path(temporary).resolve()
            empty=self.empty_registry();current=copy.deepcopy(empty)
            with patch.object(probe,'app_registry',side_effect=lambda:copy.deepcopy(current)):
                guard=probe.WindowsInstallGuard(directory)
                # Both packages belong to this probe; a registered old fixture
                # must not be mistaken for an application that predates it.
                current[:]=self.owned_registry(directory)
                old={'bytes':3,'sha256':hashlib.sha256(b'old').hexdigest()}
                new={'bytes':3,'sha256':hashlib.sha256(b'new').hexdigest()}
                with patch.object(probe,'package_executable',side_effect=[({'package':'old'},old),({'package':'new'},new)]), \
                        patch.object(probe,'bounded_file',side_effect=[{'package':'old'},{'package':'new'}]):
                    guard.prepare(Path('previous.exe'));guard.prepare(Path('current.exe'))
                self.assertEqual(guard.payloads,[old,new])
                executable=directory/'agentbox-desktop.exe';executable.write_bytes(b'new')
                uninstaller=directory/'uninstall.exe';uninstaller.write_bytes(b'fixture uninstaller')
                def remove(*args,**kwargs):
                    self.assertEqual(args[0],[str(uninstaller),'/S'])
                    self.assertFalse(kwargs['shell'])
                    executable.unlink();uninstaller.unlink();current[:]=empty
                with patch.object(probe.subprocess,'run',side_effect=remove) as execute:
                    guard.cleanup()
                    execute.assert_called_once()
                self.assertEqual(guard.report['cleanup'],'passed')

    def test_cleanup_refuses_changed_payload_external_location_and_registry_commands(self):
        for mutation in ('payload','location','hive','command','directory'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary:
                root=Path(temporary).resolve();directory=root/'owned';directory.mkdir()
                with patch.object(probe,'app_registry',return_value=self.empty_registry()):
                    guard=probe.WindowsInstallGuard(directory)
                if mutation=='directory':
                    directory.rename(root/'original');directory.mkdir()
                executable=directory/'agentbox-desktop.exe';executable.write_bytes(b'candidate')
                (directory/'uninstall.exe').write_bytes(b'fixture uninstaller')
                guard.payloads=[probe.bounded_file(executable)]
                rows=self.owned_registry(directory)
                if mutation=='payload':executable.write_bytes(b'foreign')
                elif mutation=='location':rows[0]['values']['InstallLocation']=str(root/'preexisting-unregistered')
                elif mutation=='hive':rows[0]['hive']='HKLM'
                elif mutation=='command':rows[0]['values']['UninstallString']+=' /arbitrary'
                with patch.object(probe,'app_registry',return_value=rows), patch.object(probe.subprocess,'run') as execute:
                    with self.assertRaises((ValueError,FileNotFoundError)):
                        guard.cleanup()
                    execute.assert_not_called()
                    self.assertFalse(guard.report['unexpected_locations_followed'])
                self.assertTrue(executable.exists())

    def test_missing_install_records_fallback_location_without_following_it(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary).resolve();directory=root/'owned';directory.mkdir()
            with patch.object(probe,'app_registry',return_value=self.empty_registry()):
                guard=probe.WindowsInstallGuard(directory)
            foreign=root/'previous unregistered';foreign.mkdir()
            marker=foreign/'agentbox-desktop.exe';marker.write_bytes(b'leave alone')
            rows=self.owned_registry(foreign)
            with patch.object(probe,'app_registry',return_value=rows), patch.object(probe.subprocess,'run') as execute:
                self.assertIn('observed installer registry',str(guard.missing()))
                self.assertEqual(guard.report['registry_after_attempt'],rows)
                with self.assertRaises(FileNotFoundError):guard.cleanup()
                execute.assert_not_called()
            self.assertEqual(marker.read_bytes(),b'leave alone')

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
            with closing(sqlite3.connect(state / 'sync.db')) as database, database:
                database.execute('PRAGMA user_version=4')
            before = probe.state_snapshot(state)
            result = probe.verify_retention(SIDECAR, state, local, binding_id, before, server)
            self.assertEqual(result['schema_after'], original['schema'])
            self.assertTrue(result['schema_migrated'])
            with closing(sqlite3.connect(state / 'sync.db')) as database, database:
                database.execute("UPDATE metadata SET value=? WHERE key='device'", ('b' * 32,))
            with self.assertRaisesRegex(ValueError, 'device, binding, baseline or history'):
                probe.verify_retention(SIDECAR, state, local, binding_id, before, server)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--sidecar', type=Path)
    args, remaining = parser.parse_known_args()
    SIDECAR = args.sidecar.resolve() if args.sidecar else None
    unittest.main(argv=[__file__, *remaining])
