"""Linux project quota helper. JSON over stdin/stdout; never follows user links.

Runtime only initializes empty directories or checks already adopted roots.
--adopt is an offline operator operation, after all writers have been stopped.
"""
import ctypes
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import struct
import subprocess
import sys
import time

GETXATTR = 0x801c581f
SETXATTR = 0x401c5820
PROJINHERIT = 0x200

class Dqblk(ctypes.Structure):
    _fields_ = [(k, ctypes.c_uint64) for k in ('bhard', 'bsoft', 'space', 'ihard', 'isoft', 'inodes', 'btime', 'itime')] + [('valid', ctypes.c_uint32)]

def quota(device, project, limit=None):
    q = Dqblk()
    command = 0x800007
    if limit is not None:
        command = 0x800008
        q.bhard = (limit + 1023) // 1024
        # Bound inode consumption too. No soft grace period hiding hard limits.
        # Zero releases both limits (retired projects of deleted accounts).
        q.ihard = max(10000, min(1000000, limit // 4096)) if limit else 0
        q.valid = 1 | 4
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.quotactl(ctypes.c_int((command << 8) | 2), os.fsencode(device), ctypes.c_int(project), ctypes.byref(q)) != 0:
        err = ctypes.get_errno()
        raise OSError(err, 'project quota unavailable')
    return {'used_bytes': q.space, 'limit_bytes': q.bhard * 1024, 'files': q.inodes, 'file_limit': q.ihard}

def mount_device(policy):
    mount = policy['mount']
    # Exact mount, not a directory on root when the data disk is absent.
    records = [line.split() for line in Path('/proc/self/mountinfo').read_text().splitlines()]
    matches = [r for r in records if r[4] == mount]
    if len(matches) != 1:
        raise ValueError('required storage mount is absent or ambiguous')
    r = matches[0]
    sep = r.index('-')
    if r[sep + 1] not in ('ext4', 'xfs') or 'prjquota' not in (r[5] + ',' + r[sep + 3]).split(','):
        raise ValueError('required filesystem project quota mount option is absent')
    device = r[sep + 2]
    actual = os.stat(device)
    uuid = subprocess.check_output(['/usr/sbin/blkid', '-s', 'UUID', '-o', 'value', device], text=True).strip()
    if not stat.S_ISBLK(actual.st_mode) or uuid != policy['uuid']:
        raise ValueError('storage filesystem UUID changed')
    return device

def directory(path, create=False):
    """Open every component without following links, including ancestors."""
    if not os.path.isabs(path):
        raise ValueError('absolute path required')
    fd = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in Path(path).parts[1:]:
            if create:
                try:
                    os.mkdir(part, 0o700, dir_fd=fd)
                except FileExistsError:
                    pass
            new = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = new
        return fd
    except BaseException:
        os.close(fd)
        raise

def attributes(fd):
    return list(struct.unpack('IIIII8s', fcntl.ioctl(fd, GETXATTR, bytes(28))))

def assign(fd, project):
    data = attributes(fd)
    data[3] = project
    if stat.S_ISDIR(os.fstat(fd).st_mode):
        data[0] |= PROJINHERIT
    fcntl.ioctl(fd, SETXATTR, struct.pack('IIIII8s', *data))

def adopt_tree(fd, project):
    # Offline only; reject hardlinked regular files rather than changing an inode
    # also owned by another user. Symlinks are preserved and never dereferenced.
    assign(fd, project)
    for name in os.listdir(fd):
        st = os.stat(name, dir_fd=fd, follow_symlinks=False)
        if stat.S_ISLNK(st.st_mode):
            continue
        if not stat.S_ISREG(st.st_mode) and not stat.S_ISDIR(st.st_mode):
            raise ValueError('special file in quota adoption')
        child = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd)
        try:
            actual = os.fstat(child)
            if (actual.st_dev, actual.st_ino) != (st.st_dev, st.st_ino) or actual.st_dev != os.fstat(fd).st_dev:
                raise ValueError('directory changed during adoption')
            if stat.S_ISDIR(actual.st_mode):
                adopt_tree(child, project)
            else:
                if actual.st_nlink != 1:
                    raise ValueError('hardlink requires explicit migration review')
                assign(child, project)
        finally:
            os.close(child)

def ensure_root(path, project, adopt=False):
    fd = directory(path, create=True)
    try:
        data = attributes(fd)
        if adopt:
            adopt_tree(fd, project)
        elif data[3] != project or not data[0] & PROJINHERIT:
            if os.listdir(fd):
                raise ValueError('nonempty storage root requires offline quota adoption')
            assign(fd, project)
    finally:
        os.close(fd)

def charged(device, marker, saved):
    # A deleted account's limit is released, but its leftover files still
    # occupy the disk until an operator removes them.
    usage = quota(device, int(marker.stem))
    return usage['used_bytes'] if saved.get('retired') else usage['limit_bytes']

def write_marker(registry, project, record):
    temp = registry / (str(project) + '.tmp')
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as f:
        json.dump(record, f); f.flush(); os.fsync(f.fileno())
    os.replace(temp, registry / (str(project) + '.json'))
    d = directory(str(registry)); os.fsync(d); os.close(d)

def empty_root(path):
    try:
        fd = directory(path)
    except FileNotFoundError:
        return True
    try:
        return not os.listdir(fd)
    finally:
        os.close(fd)

def archive_marker(data, marker):
    # Same layout as the manual retirement: keep the record, out of the registry.
    archive = data / '.storage-projects-retired'
    os.close(directory(str(archive), create=True))
    os.replace(marker, archive / (marker.stem + '-' + time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()) + '.json'))
    d = directory(str(marker.parent)); os.fsync(d); os.close(d)

def main():
    req = json.load(sys.stdin)
    policy = req['policy']
    device = mount_device(policy)
    if req.get('check_mount'):
        status = dict(line.split(':',1) for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
        if int(status['CapEff'].strip(),16) & (1 << 24):
            raise ValueError('drop CAP_SYS_RESOURCE from service to enforce quotas on server writes')
        quota(device, 0)
        return {'ready': True}
    if req.get('report'):
        # Read-only usage of every registered project for the monitor. Project 0
        # is everything outside user roots (images, swap, backups, releases).
        registry = Path(req['data']) / '.storage-projects'
        projects = []
        if registry.is_dir() and not registry.is_symlink():
            if registry.stat().st_uid != os.geteuid() or registry.stat().st_mode & 0o022:
                raise ValueError('quota registry is not service owned')
            for marker in sorted(registry.glob('*.json')):
                if marker.is_symlink() or not marker.stem.isdigit():
                    continue
                saved = json.loads(marker.read_text())
                projects.append(dict(quota(device, int(marker.stem)), project=int(marker.stem), retired=bool(saved.get('retired')),
                                     kind=str(saved.get('kind', '')), owner=str(saved.get('owner', '')), root=str(saved.get('root', ''))))
        return {'projects': projects, 'unassigned': quota(device, 0)}
    if not re.fullmatch('[a-f0-9]{64}', req['owner']) or not re.fullmatch('[a-z0-9][a-z0-9_-]{1,31}', req['user']):
        raise ValueError('invalid owner')
    data = Path(req['data'])
    registry = data / '.storage-projects'
    registry_fd = directory(str(registry), create=True)
    os.close(registry_fd)
    if registry.stat().st_uid != os.geteuid() or registry.stat().st_mode & 0o022:
        raise ValueError('quota registry is not service owned')
    lockfd = os.open(registry / '.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lockfd, 'r+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if req.get('retire'):
            # The account is gone: release its limits but keep the marker, so the
            # leftover files stay visible and block reuse of a non-empty root.
            retired = []
            for marker in sorted(registry.glob('*.json')):
                saved = json.loads(marker.read_text())
                if marker.is_symlink() or not marker.stem.isdigit() or saved.get('owner') != req['owner'] or saved.get('retired'):
                    continue
                quota(device, int(marker.stem), 0)
                write_marker(registry, marker.stem, dict(saved, retired=int(time.time())))
                retired.append(int(marker.stem))
            return {'retired': retired}
        result = {}
        plans = (
            ('data', data / 'users' / req['user'], policy.get('users', {}).get(req['user'], policy['user_bytes'])),
            ('cache', Path(req['tools']) / 'private' / req['owner'], policy.get('cache_users', {}).get(req['user'], policy.get('cache_bytes', 0))),
        )
        projected = {str(100000 + int(hashlib.sha256((kind + ':' + req['owner']).encode()).hexdigest()[:7],16)):limit for kind,root,limit in plans if kind!='cache' or req.get('tools')}
        if policy.get('budget_bytes',0):
            total = sum(projected.values())
            for marker in registry.glob('*.json'):
                if marker.stem not in projected:
                    total += charged(device, marker, json.loads(marker.read_text()))
            if total > policy['budget_bytes']:
                raise ValueError('allocated project quotas exceed storage budget')
        for kind, root, limit in plans:
            if kind == 'cache' and not req.get('tools'):
                continue
            project = 100000 + int(hashlib.sha256((kind + ':' + req['owner']).encode()).hexdigest()[:7], 16)
            marker = registry / (str(project) + '.json')
            record = {'owner': req['owner'], 'kind': kind, 'root': str(root)}
            if marker.exists():
                saved = json.loads(marker.read_text())
                if marker.is_symlink() or any(saved.get(k)!=v for k,v in record.items()):
                    raise ValueError('quota project collision or ownership changed')
                if not saved.get('ready') and '--adopt' not in sys.argv and root.exists() and any(root.iterdir()):
                    raise ValueError('interrupted quota preparation requires offline adoption')
            else:
                # Detect same pathname left by a removed/recreated account. Once
                # the files of a deleted account are gone, the name is free again.
                for other in list(registry.glob('*.json')):
                    saved = json.loads(other.read_text())
                    if saved['root'] == str(root) and saved['owner'] != req['owner']:
                        if not saved.get('retired') or not empty_root(str(root)):
                            raise ValueError('previous user data requires operator retirement')
                        archive_marker(data, other)
                fd = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
                with os.fdopen(fd, 'w') as f:
                    json.dump(dict(record,ready=False), f)
                    f.flush()
                    os.fsync(f.fileno())
                d = directory(str(registry))
                os.fsync(d)
                os.close(d)
            quota(device, project, limit)
            ensure_root(str(root), project, '--adopt' in sys.argv)
            write_marker(registry, project, dict(record, ready=True))
            result[kind] = quota(device, project)
        return result

if __name__ == '__main__':
    try:
        print(json.dumps(main()))
    except Exception as error:
        # No user contents, credentials, or arbitrary subprocess output.
        print(type(error).__name__ + ': ' + str(error), file=sys.stderr)
        sys.exit(1)
