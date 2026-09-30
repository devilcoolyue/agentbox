#!/usr/bin/env python3
"""Upgrade policy and lifecycle tests. Network, systemd and model calls are synthetic."""
import hashlib
import contextlib
import importlib.util
import io
import json
import os
import sqlite3
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('update', Path(__file__).resolve().parent.parent / 'deploy/update.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class Upgrades(unittest.TestCase):
    def test_activation_keeps_backup_before_switch_and_checks_schema_before_stop(self):
        with tempfile.TemporaryDirectory() as tmp:
            app = Path(tmp).resolve()
            data = app / 'data'; data.mkdir()
            with contextlib.closing(sqlite3.connect(data / 'state.db')) as db: db.execute('pragma user_version=1')
            for version in ('v2.0.0-dev.1', 'v1.1.0'):
                folder = app / 'releases' / version; folder.mkdir(parents=True)
                (folder / 'agentbox').write_text('fixture')
            (app / 'current').symlink_to('releases/v2.0.0-dev.1')
            config = app / 'config.json'
            config.write_text(json.dumps({'data_dir': str(data)}))
            (app / 'agentbox.service').write_text(m.release.unit(app, config))
            events = []
            supported_schema = 0
            def run(*args, **kwargs):
                events.append(tuple(map(str, args)))
                if len(args) > 1 and args[1] == 'check-config':
                    return json.dumps({'schema_version': supported_schema, 'compatibility_epoch': 1})
                if len(args) > 1 and args[1] == 'backup':
                    Path(args[-1]).write_bytes(b'verified fixture backup')
                if len(args) > 1 and args[1] == 'backup-verify':
                    self.assertEqual(Path(args[-1]).read_bytes(), b'verified fixture backup')
                    self.assertEqual((app / 'current').resolve(), app / 'releases/v2.0.0-dev.1')
                if args == ('systemctl', 'start', 'agentbox.service'):
                    self.assertEqual((app / 'current').resolve(), app / 'releases/v1.1.0')
            with patch.object(m.release, 'run', side_effect=run), patch.object(m.release, 'restore_context'), \
                 patch.object(m.release.urllib.request, 'build_opener') as opener:
                opener.return_value.open.return_value.__enter__.return_value.status = 200
                with self.assertRaises(ValueError):
                    m.release.activate(app, 'v1.1.0', config, data / 'backups', app)
                self.assertFalse(any(event[0] == 'systemctl' for event in events))
                supported_schema = 1
                phases = []
                m.release.activate(app, 'v1.1.0', config, data / 'backups', app, progress=phases.append)
                self.assertEqual(phases, ['checking', 'stopping', 'backup', 'switching', 'restarting', 'health'])
                stop = events.index(('systemctl', 'stop', 'agentbox.service'))
                backup = next(i for i, event in enumerate(events) if len(event) > 1 and event[1] == 'backup')
                verify = next(i for i, event in enumerate(events) if len(event) > 1 and event[1] == 'backup-verify')
                start = events.index(('systemctl', 'start', 'agentbox.service'))
                self.assertTrue(stop < backup < verify < start)

    def probe_fixture(self, current, revision):
        with tempfile.TemporaryDirectory() as tmp:
            app = Path(tmp).resolve()
            package = app / 'releases' / current
            package.mkdir(parents=True)
            (app / 'current').symlink_to(package)
            config = app / 'config.json'
            (package / 'build.json').write_text(json.dumps({'program': 'agentbox', 'version': current,
                'os': 'linux', 'arch': 'amd64', 'revision': revision}))
            unit_text = m.release.unit(app, config)
            props = 'MainPID=123\nFragmentPath=/etc/systemd/system/agentbox.service\nDropInPaths='
            original_read, original_is_dir = m.Path.read_text, m.Path.is_dir
            def read(path, *args, **kwargs):
                return unit_text if str(path) == '/etc/systemd/system/agentbox.service' else original_read(path, *args, **kwargs)
            with patch.object(m.platform, 'system', return_value='Linux'), patch.object(m.os, 'geteuid', return_value=0), \
                 patch.object(m, 'architecture', return_value='amd64'), patch.object(m.shutil, 'which', return_value='/usr/bin/systemd-run'), \
                 patch.object(m.Path, 'is_dir', autospec=True, side_effect=lambda path: True if str(path) == '/run/systemd/system' else original_is_dir(path)), \
                 patch.object(m.Path, 'read_text', autospec=True, side_effect=read), \
                 patch.object(m.Path, 'resolve', autospec=True, side_effect=lambda path: package / 'agentbox' if str(path) == '/proc/123/exe' else Path(os.path.realpath(path))), \
                 patch.object(m, 'command', return_value=props) as command:
                m.probe(app, config, 123, current)
                command.return_value = props.replace('MainPID=123', 'MainPID=456')
                with self.assertRaises(ValueError): m.probe(app, config, 123, current)
                command.return_value = props + '/etc/systemd/system/agentbox.service.d/custom.conf'
                with self.assertRaises(ValueError): m.probe(app, config, 123, current)
                command.return_value = props
                unit_text += '# custom service\n'
                with self.assertRaises(ValueError): m.probe(app, config, 123, current)
                unit_text = m.release.unit(app, config)
                (app / 'current').unlink()
                (app / 'current').symlink_to(app / 'releases/other')
                with self.assertRaises(ValueError): m.probe(app, config, 123, current)

    def test_probe_checks_actual_service_and_layout(self):
        for current, revision in [('v1.0.0', 'fixture'), ('dev', 'fixture+dirty'),
                                  ('v2.0.0-dev.1', 'fixture'), ('v1.0.0', 'fixture+dirty')]:
            with self.subTest(current=current, revision=revision):
                self.probe_fixture(current, revision)

    def test_development_can_switch_to_stable(self):
        for current, revision in [('dev', 'fixture'), ('v2.0.0-dev.1', 'fixture'),
                                  ('v2.0.0-rc.1', 'fixture'), ('v2.0.0', 'fixture+dirty'),
                                  ('v1.0.0', 'fixture+dirty'), ('v2.0.0+dirty', 'fixture')]:
            with self.subTest(current=current, revision=revision), tempfile.TemporaryDirectory() as tmp, \
                 patch.object(m, 'probe'), patch.object(m, 'command') as command:
                app = Path(tmp)
                for target in ('../../bad', 'v1.0.0-rc.1', 'https://evil.invalid'):
                    with self.assertRaises(ValueError):
                        m.start(app, app / 'config', 123, current, target, revision)
                command.assert_not_called()
                result = m.start(app, app / 'config', 123, current, 'v1.0.0', revision)
                self.assertEqual(result['job']['from_version'], current)
                self.assertEqual(result['job']['version'], 'v1.0.0')
                self.assertEqual(result['job']['phase'], 'queued')
                self.assertEqual(command.call_args.args[0], 'systemd-run')

    def test_download_hosts_and_redirects(self):
        for url in ('http://github.com/a', 'https://github.com.evil.invalid/a',
                    'https://github.com@127.0.0.1/a', 'file:///etc/passwd',
                    'https://github.com:444/a', 'https://127.0.0.1/a'):
            with self.assertRaises(ValueError):
                m.validate_url(url)
            with self.assertRaises(ValueError):
                m.TrustedRedirect().redirect_request(None, None, 302, '', {}, url)
        for host in ('github.com', 'api.github.com', 'release-assets.githubusercontent.com'):
            m.validate_url('https://' + host + '/fixture')

    def test_checksums_required_exact_and_unique(self):
        with tempfile.TemporaryDirectory() as tmp:
            sums = Path(tmp) / 'SHA256SUMS'
            for text in ('', 'wrong  package.tar.gz', 'ok  other.tar.gz',
                         'ok  package.tar.gz\nok  package.tar.gz'):
                sums.write_text(text)
                with self.assertRaises(ValueError):
                    m.verify_checksum(sums, 'package.tar.gz', 'ok')
            sums.write_text('ok  package.tar.gz\nother  other.tar.gz\n')
            m.verify_checksum(sums, 'package.tar.gz', 'ok')

    def archive(self, path, entries):
        with tarfile.open(path, 'w:gz') as tar:
            for name, kind, data in entries:
                entry = tarfile.TarInfo(name)
                entry.type = kind
                entry.mode = 0o6755
                if kind == tarfile.REGTYPE:
                    entry.size = len(data)
                tar.addfile(entry, io.BytesIO(data) if kind == tarfile.REGTYPE else None)

    def test_archive_boundaries(self):
        for entries in [
            [('pkg/../../outside', tarfile.REGTYPE, b'x')],
            [('/tmp/absolute', tarfile.REGTYPE, b'x')],
            [('other/file', tarfile.REGTYPE, b'x')],
            [('pkg/link', tarfile.SYMTYPE, b'')],
            [('pkg/link', tarfile.LNKTYPE, b'')],
            [('pkg/device', tarfile.CHRTYPE, b'')],
            [('pkg/a', tarfile.REGTYPE, b'x'), ('pkg/a', tarfile.REGTYPE, b'x')],
        ]:
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                self.archive(root / 'archive', entries)
                with self.assertRaises(ValueError):
                    m.unpack(root / 'archive', root / 'out', 'pkg')
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.archive(root / 'archive', [('pkg/bin', tarfile.REGTYPE, b'fixture')])
            package = m.unpack(root / 'archive', root / 'out', 'pkg')
            self.assertEqual((package / 'bin').read_bytes(), b'fixture')
            self.assertEqual((package / 'bin').stat().st_mode & 0o7777, 0o755)
            with patch.object(m, 'MAX_UNPACKED', 2), self.assertRaises(ValueError):
                m.unpack(root / 'archive', root / 'limited', 'pkg')

    def test_duplicate_start_and_manual_deploy_lock(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(m, 'probe'):
            app = Path(tmp)
            calls = []
            def command(*args):
                calls.append(args)
                return 'active' if args[0] == 'systemctl' else ''
            with patch.object(m, 'command', side_effect=command):
                one = m.start(app, app / 'config', 123, 'v1.0.0', 'v1.1.0')
                two = m.start(app, app / 'config', 123, 'v1.0.0', 'v1.1.0')
                self.assertEqual(one['job']['id'], two['job']['id'])
                launch = [c for c in calls if c[0] == 'systemd-run']
                self.assertEqual(len(launch), 1)
                self.assertIn('--property=RuntimeMaxSec=1800', launch[0])
                self.assertIn('--unit=agentbox-upgrade-' + one['job']['id'], launch[0])
                with self.assertRaises(ValueError):
                    m.start(app, app / 'config', 123, 'v1.0.0', 'v1.2.0')
                m.save(app, one['job'], 'failed')
                with m.release.locked(app / '.deploy.lock'), self.assertRaises(BlockingIOError):
                    m.start(app, app / 'config', 123, 'v1.0.0', 'v1.1.0')

    def test_no_downgrade_or_arbitrary_target(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(m, 'probe'), patch.object(m, 'command') as command:
            for version in ('../../bad', 'v0.9.0', 'v1.0.0', 'v1.1.0-rc.1', 'https://evil.invalid'):
                with self.assertRaises(ValueError):
                    m.start(Path(tmp), Path(tmp) / 'config', 123, 'v1.0.0', version)
            command.assert_not_called()

    def test_interrupted_worker_and_uncertain_submission(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(m, 'probe'):
            app = Path(tmp)
            with patch.object(m, 'command', side_effect=subprocess.TimeoutExpired('systemd-run', 10)):
                with self.assertRaises(ValueError):
                    m.start(app, app / 'config', 123, 'v1.0.0', 'v1.1.0')
            self.assertEqual(m.read_job(app)['phase'], 'queued')
            with patch.object(m, 'command', return_value='active'):
                self.assertEqual(m.reconcile(app)['phase'], 'queued')
            with patch.object(m, 'command', return_value='inactive'):
                self.assertEqual(m.reconcile(app)['phase'], 'failed')

    def run_fixture(self, fault=None, current="v1.0.0"):
        with tempfile.TemporaryDirectory() as tmp:
            app = Path(tmp).resolve() / 'app'
            old = app / 'releases' / current
            old.mkdir(parents=True)
            (app / 'current').symlink_to(old)
            config = app / 'config.json'
            config.write_text(json.dumps({'data_dir': str(app / 'data')}))
            job = {'id': 'a' * 32, 'version': 'v1.1.0', 'from_version': current}
            m.save(app, job, 'queued')
            name = 'agentbox_v1.1.0_linux_amd64'
            package = Path(tmp) / name
            (package / 'deploy').mkdir(parents=True)
            (package / 'clients').mkdir()
            (package / 'agentbox').write_text('#!/bin/sh\necho fixture\n')
            (package / 'agentbox').chmod(0o755)
            (package / 'deploy/release.py').write_text('fixture')
            (package / 'deploy/update.py').write_text('fixture')
            for client in m.release.CLIENT_NAMES:
                (package / 'clients' / client).write_text('client fixture')
            (package / 'build.json').write_text(json.dumps({'program': 'agentbox', 'version': 'v1.1.0',
                'os': 'linux', 'arch': 'amd64', 'revision': 'fixture'}))
            archive = Path(tmp) / 'fixture.tar.gz'
            with tarfile.open(archive, 'w:gz') as tar:
                tar.add(package, arcname=name)
            archive_data = archive.read_bytes()
            digest = hashlib.sha256(archive_data).hexdigest()
            base = f'https://github.com/{m.REPO}/releases/download/v1.1.0/'
            assets = [{'name': n, 'browser_download_url': base + n, 'size': len(archive_data)}
                      for n in (name + '.tar.gz', 'SHA256SUMS')]
            if fault == 'asset':
                assets[0]['browser_download_url'] = 'https://github.com/other/repo/evil'
            def download(url, dest, limit):
                if '/releases/tags/' in url:
                    dest.write_text(json.dumps({'tag_name': 'v1.1.0', 'assets': assets}))
                elif url.endswith('SHA256SUMS'):
                    dest.write_text(('wrong' if fault == 'checksum' else digest) + '  ' + name + '.tar.gz')
                else:
                    dest.write_bytes(archive_data)
                return digest
            phases = []
            def activate(app, version, config, backup_dir, unit_dir, progress):
                for phase in ('checking', 'stopping', 'backup', 'switching', 'restarting', 'health'):
                    phases.append(phase)
                    progress(phase)
                    if fault == phase:
                        raise ValueError('synthetic failure')
                    if phase == 'switching':
                        m.release.switch(app, version)
            with patch.object(m, 'probe'), patch.object(m, 'architecture', return_value='amd64'), \
                 patch.object(m, 'download', side_effect=download), \
                 patch.object(m.release, 'activate', side_effect=activate) as activation, \
                 patch.object(m.release, 'run') as run, patch.object(m, 'command', side_effect=lambda *args: '123' if args[0] == 'systemctl' else 'wrong version' if fault == 'binary' else 'agentbox v1.1.0 (commit fixture, built fixture)'), \
                 patch.object(m.Path, 'resolve', autospec=True, side_effect=lambda path: app / 'releases/v1.1.0/agentbox' if str(path) == '/proc/123/exe' else Path(os.path.realpath(path))):
                if fault:
                    with self.assertRaises(ValueError):
                        m.perform(app, config, 100, current, job['id'])
                    self.assertEqual(m.read_job(app)['phase'], 'failed')
                else:
                    m.perform(app, config, 100, current, job['id'])
                    self.assertEqual(m.read_job(app)['phase'], 'succeeded')
                if fault in ('checksum', 'asset', 'binary'):
                    activation.assert_not_called()
                    run.assert_not_called()
                if fault in ('checking', 'checksum', 'asset', 'binary', 'backup'):
                    self.assertEqual((app / 'current').resolve(), old)
                restarts = [c for c in run.call_args_list if c.args == ('systemctl', 'start', 'agentbox.service')]
                self.assertEqual(len(restarts), 1 if fault == 'backup' else 0)
                if fault == 'health':
                    self.assertEqual((app / 'current').resolve(), app / 'releases/v1.1.0')
                if not fault:
                    self.assertEqual(phases, ['checking', 'stopping', 'backup', 'switching', 'restarting', 'health'])

    def test_verified_upgrade_and_failures(self):
        for current in ('v1.0.0', 'dev', 'v2.0.0-dev.1'):
            for fault in (None, 'asset', 'checksum', 'binary', 'checking', 'backup', 'health'):
                with self.subTest(current=current, fault=fault):
                    self.run_fixture(fault, current)

    def test_staged_retry_never_overwrites_changed_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            left, right = Path(tmp) / 'left', Path(tmp) / 'right'
            left.mkdir(); right.mkdir()
            (left / 'file').write_text('fixture'); (right / 'file').write_text('fixture')
            self.assertTrue(m.same_package(left, right))
            (right / 'file').write_text('changed')
            self.assertFalse(m.same_package(left, right))
            (right / 'link').symlink_to('/etc/passwd')
            with self.assertRaises(ValueError):
                m.same_package(left, right)


if __name__ == '__main__':
    unittest.main()
