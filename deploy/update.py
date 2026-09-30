#!/usr/bin/env python3
"""Console upgrades for release.py installations; workers run in a separate systemd unit."""
import argparse
import contextlib
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request
import uuid

# Explicit sibling import also works under python3 -I (no cwd/PYTHONPATH imports).
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('release', Path(__file__).with_name('release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)

REPO = 'devilcoolyue/agentbox'
STABLE = re.compile(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)')
VERSION = re.compile(r'v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?')
TERMINAL = ('succeeded', 'failed')
MAX_ARCHIVE = 1024 * 1024 * 1024
MAX_UNPACKED = 4 * MAX_ARCHIVE
PHASES = {
    'queued': '升级任务已提交', 'downloading': '正在下载发布包',
    'verifying': '正在校验发布包', 'staging': '正在安装新版本',
    'checking': '正在检查配置和数据库兼容性', 'stopping': '正在停止服务',
    'backup': '正在备份并验证数据', 'switching': '正在切换版本',
    'restarting': '正在启动新版本', 'health': '正在验证服务状态',
    'succeeded': '升级完成', 'failed': '升级失败，请查看升级任务日志',
}


def command(*args):
    return subprocess.check_output([str(a) for a in args], text=True, timeout=10).strip()


def architecture():
    return {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}.get(platform.machine())


def probe(app, config, pid, current):
    if platform.system() != 'Linux' or os.geteuid() != 0 or not Path('/run/systemd/system').is_dir():
        raise ValueError('一键升级需要以 root 运行的 Linux/systemd 发布安装。')
    if not architecture() or (current != 'dev' and not VERSION.fullmatch(current)):
        raise ValueError('当前架构或版本标识不支持一键升级，请使用手工升级。')
    if not shutil.which('systemd-run'):
        raise ValueError('服务器缺少 systemd-run，请使用手工升级。')
    package = app / 'releases' / current
    if not (app / 'current').is_symlink() or (app / 'current').resolve() != package:
        raise ValueError('当前服务不是标准版本目录安装，请使用手工升级。')
    meta = release.read(package / 'build.json')
    if (meta.get('version') != current or meta.get('program') != 'agentbox'
            or meta.get('os') != 'linux' or meta.get('arch') != architecture()
            or not meta.get('revision')):
        raise ValueError('当前安装的构建信息不匹配，请使用手工升级。')
    unit = Path('/etc/systemd/system/agentbox.service')
    if unit.read_text() != release.unit(app, config):
        raise ValueError('服务单元经过自定义或使用旧布局，请使用手工升级。')
    props = dict(line.split('=', 1) for line in command(
        'systemctl', 'show', 'agentbox.service', '-p', 'MainPID', '-p', 'FragmentPath',
        '-p', 'DropInPaths').splitlines() if '=' in line)
    if (props.get('MainPID') != str(pid) or props.get('FragmentPath') != str(unit)
            or props.get('DropInPaths') or Path(f'/proc/{pid}/exe').resolve() != package / 'agentbox'):
        raise ValueError('正在运行的服务与发布安装不一致，请使用手工升级。')


def state_path(app):
    return app / '.update' / 'state.json'


def save(app, job, phase, error=''):
    job.update(phase=phase, message=PHASES[phase], error=error, updated_at=int(time.time() * 1000))
    release.atomic_json(state_path(app), job)


def read_job(app):
    path = state_path(app)
    if not path.exists():
        return None
    job = release.read(path)
    if not re.fullmatch(r'[0-9a-f]{32}', job.get('id', '')) or job.get('phase') not in PHASES:
        raise ValueError('升级状态文件损坏，请检查服务器。')
    return job


def reconcile(app):
    job = read_job(app)
    if job and job['phase'] not in TERMINAL:
        active = command('systemctl', 'show', 'agentbox-upgrade-' + job['id'] + '.service',
                         '-p', 'ActiveState', '--value')
        if active not in ('active', 'activating', 'reloading'):
            # The worker may have published its final result while systemctl ran.
            job = read_job(app)
            if job['phase'] not in TERMINAL:
                save(app, job, 'failed', '升级任务已退出或服务器曾重启，请查看任务日志后重试。')
    return job


def status(app, config, pid, current):
    try:
        probe(app, config, pid, current)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        return {'supported': False, 'reason': str(error), 'job': None}
    with release.locked(app / '.update-control.lock'):
        return {'supported': True, 'reason': '', 'job': reconcile(app)}


def start(app, config, pid, current, version, revision=''):
    probe(app, config, pid, current)
    source, target = VERSION.fullmatch(current), STABLE.fullmatch(version)
    development = current == 'dev' or (source and (source[4] or 'dirty' in current or 'dirty' in revision))
    if not target or (not development and (not source or tuple(map(int, target.groups())) <= tuple(map(int, source.groups()[:3])))):
        raise ValueError('只能升级到更新的正式版本，请重新检查更新。')
    with release.locked(app / '.update-control.lock'):
        previous = reconcile(app)
        if previous and previous['phase'] not in TERMINAL:
            if previous['version'] != version:
                raise ValueError('已有其他版本的升级任务正在执行。')
            return {'supported': True, 'reason': '', 'job': previous}
        # Fail before queuing when a manual deployment holds the lock.
        with release.locked(app / '.deploy.lock'):
            pass
        job = {'id': uuid.uuid4().hex, 'version': version, 'from_version': current,
               'started_at': int(time.time() * 1000)}
        save(app, job, 'queued')
        try:
            command('systemd-run', '--quiet', '--collect', '--unit=agentbox-upgrade-' + job['id'],
                    '--property=Type=exec', '--property=RuntimeMaxSec=1800',
                    '--property=TimeoutStopSec=30', '--property=UMask=0077',
                    sys.executable, '-I', Path(__file__).resolve(), '--app', app,
                    '--config', config, '--pid', pid, '--current', current, 'run', '--job', job['id'])
        except (OSError, subprocess.SubprocessError):
            # systemd-run may have timed out after successfully handing off.
            # Keep queued state; status reconciles against the independent unit.
            raise ValueError('升级提交结果尚未确认，请刷新任务状态，不要手工替换程序。')
        return {'supported': True, 'reason': '', 'job': read_job(app)}


def validate_url(url):
    u = urllib.parse.urlsplit(url)
    if (u.scheme != 'https' or u.netloc not in ('api.github.com', 'github.com',
            'release-assets.githubusercontent.com', 'objects.githubusercontent.com')
            or u.username or u.password):
        raise ValueError('Untrusted release download URL')


class TrustedRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        validate_url(newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def download(url, destination, limit):
    validate_url(url)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), TrustedRedirect())
    accept = 'application/vnd.github+json' if urllib.parse.urlsplit(url).hostname == 'api.github.com' else 'application/octet-stream'
    req = urllib.request.Request(url, headers={'User-Agent': 'agentbox-upgrade', 'Accept': accept})
    total = 0
    digest = hashlib.sha256()
    with opener.open(req, timeout=30) as response, destination.open('xb') as out:
        while True:
            block = response.read(1024 * 1024)
            if not block:
                break
            total += len(block)
            if total > limit:
                raise ValueError('Release download exceeds size limit')
            out.write(block)
            digest.update(block)
    return digest.hexdigest()


def verify_checksum(path, name, digest):
    matches = [line.split()[0] for line in path.read_text().splitlines()
               if len(line.split()) == 2 and line.split()[1] == name]
    if matches != [digest]:
        raise ValueError('Missing, duplicate or mismatched SHA256 checksum')


def unpack(archive, destination, name):
    seen = set()
    total = 0
    with tarfile.open(archive, 'r:gz') as tar:
        for entry in tar:
            path = PurePosixPath(entry.name)
            if (path.is_absolute() or '..' in path.parts or not path.parts or path.parts[0] != name
                    or '\\' in entry.name or str(path) in seen or len(seen) >= 30000
                    or not (entry.isdir() or entry.isfile()) or entry.size < 0):
                raise ValueError('Unsafe release archive entry')
            seen.add(str(path))
            total += entry.size
            if total > MAX_UNPACKED:
                raise ValueError('Unpacked release exceeds size limit')
            target = destination.joinpath(*path.parts)
            if entry.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with tar.extractfile(entry) as source, target.open('xb') as out:
                    shutil.copyfileobj(source, out, 1024 * 1024)
                target.chmod(entry.mode & 0o755)
    return destination / name


def same_package(left, right):
    # Retry after staging must not overwrite an immutable release or trust a
    # partially changed directory. Compare every path, type, mode and file hash.
    def inventory(root):
        result = {}
        for path in root.rglob('*'):
            if path.is_symlink() or not (path.is_file() or path.is_dir()):
                raise ValueError('Unexpected installed release entry')
            digest = ''
            if path.is_file():
                with path.open('rb') as stream:
                    digest = release.file_digest(stream)
            result[str(path.relative_to(root))] = (path.is_dir(), path.stat().st_mode & 0o777, digest)
        return result
    return inventory(left) == inventory(right)


def perform(app, config, pid, current, job_id):
    job = read_job(app)
    if not job or job['id'] != job_id or job['phase'] != 'queued':
        raise ValueError('Upgrade job is no longer pending')
    version = job['version']
    old = (app / 'current').resolve()
    try:
        with release.locked(app / '.deploy.lock'):
            probe(app, config, pid, current)
            with tempfile.TemporaryDirectory(prefix='.download-', dir=app) as tmp:
                tmp = Path(tmp)
                save(app, job, 'downloading')
                metadata = tmp / 'release.json'
                download(f'https://api.github.com/repos/{REPO}/releases/tags/{version}', metadata, 2 * 1024 * 1024)
                info = release.read(metadata)
                if info.get('tag_name') != version or info.get('draft') or info.get('prerelease'):
                    raise ValueError('Expected a public stable release')
                name = f'agentbox_{version}_linux_{architecture()}'
                base = f'https://github.com/{REPO}/releases/download/{version}/'
                assets = info.get('assets', [])
                for filename in (name + '.tar.gz', 'SHA256SUMS'):
                    matches = [a for a in assets if a.get('name') == filename]
                    if len(matches) != 1 or matches[0].get('browser_download_url') != base + filename:
                        raise ValueError('Missing or unexpected release asset')
                archive_size = next(a['size'] for a in assets if a['name'] == name + '.tar.gz')
                if not 0 < archive_size <= MAX_ARCHIVE:
                    raise ValueError('Invalid release archive size')
                if shutil.disk_usage(app).free < archive_size * 5 + 256 * 1024 * 1024:
                    raise ValueError('Insufficient disk space to download and stage release')
                sums, archive = tmp / 'SHA256SUMS', tmp / (name + '.tar.gz')
                download(base + 'SHA256SUMS', sums, 1024 * 1024)
                digest = download(base + archive.name, archive, MAX_ARCHIVE)
                save(app, job, 'verifying')
                verify_checksum(sums, archive.name, digest)
                package = unpack(archive, tmp, name)
                meta = release.read(package / 'build.json')
                if (meta.get('program') != 'agentbox' or meta.get('version') != version
                        or meta.get('os') != 'linux' or meta.get('arch') != architecture()
                        or not meta.get('revision') or 'dirty' in meta['revision']):
                    raise ValueError('Release build metadata mismatch')
                # All bundled clients must be present before stopping the service.
                for item in ('agentbox', 'deploy/release.py', 'deploy/update.py', *('clients/' + n for n in release.CLIENT_NAMES)):
                    if not (package / item).is_file():
                        raise ValueError('Incomplete release package: ' + item)
                binary_version = command(package / 'agentbox', '--version')
                if not binary_version.startswith(f'agentbox {version} (commit {meta["revision"]}, built '):
                    raise ValueError('Executable version differs from release metadata')
                save(app, job, 'staging')
                target = app / 'releases' / version
                if target.exists():
                    if not same_package(package, target):
                        raise ValueError('Installed version differs from verified package; inspect manually')
                else:
                    release.stage(package, app)
            # Recheck the serving process after download, before any downtime.
            probe(app, config, pid, current)
            cfg = release.read(config)
            data = release.resolve(config.parent, cfg.get('data_dir', 'data'))
            release.activate(app, version, config, data / 'backups', Path('/etc/systemd/system'),
                             progress=lambda phase: save(app, job, phase))
            # HTTP readiness alone could belong to a different process.
            new_pid = command('systemctl', 'show', 'agentbox.service', '-p', 'MainPID', '--value')
            if Path(f'/proc/{new_pid}/exe').resolve() != app / 'releases' / version / 'agentbox':
                raise ValueError('Running executable does not match the requested version')
            save(app, job, 'succeeded')
    except Exception:
        # Before the symlink changes, no new binary has opened/migrated SQLite.
        # Restart the original service after a failed backup; never auto-rollback
        # once the new binary may have migrated the database.
        if job['phase'] in ('stopping', 'backup', 'switching') and (app / 'current').resolve() == old:
            with contextlib.suppress(Exception):
                release.run('systemctl', 'start', 'agentbox.service')
        phase = PHASES[job['phase']]
        save(app, job, 'failed', phase + '时失败。请查看升级任务日志；版本切换后不会自动回退数据库。')
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--app', required=True)
    parser.add_argument('--config', required=True)
    parser.add_argument('--pid', required=True, type=int)
    parser.add_argument('--current', required=True)
    parser.add_argument('--revision', default='')
    sub = parser.add_subparsers(dest='command', required=True)
    sub.add_parser('status')
    sub.add_parser('start').add_argument('--version', required=True)
    sub.add_parser('run').add_argument('--job', required=True)
    args = parser.parse_args()
    app, config = release.absolute(args.app), release.absolute(args.config)
    if args.command == 'run':
        perform(app, config, args.pid, args.current, args.job)
    else:
        try:
            if args.command == 'start':
                result = start(app, config, args.pid, args.current, args.version, args.revision)
            else:
                result = status(app, config, args.pid, args.current)
        except BlockingIOError:
            result = {'error': '已有部署或升级操作正在执行，请稍后刷新。'}
        except (ValueError, OSError, subprocess.SubprocessError) as error:
            print(str(error), file=sys.stderr)
            result = {'error': '无法提交或读取升级任务，请刷新状态并检查服务器日志。'}
        print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    main()
