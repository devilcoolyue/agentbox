#!/usr/bin/env python3
"""Opt-in Linux root project-quota test on a disposable loop filesystem only."""
import errno
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

if sys.platform != 'linux' or os.geteuid() != 0:
    raise SystemExit('BLOCKED: Linux root and loop mount capability required')
spec = importlib.util.spec_from_file_location('quota', Path(__file__).resolve().parents[1] / 'internal/storagepolicy/quota.py')
q = importlib.util.module_from_spec(spec)
spec.loader.exec_module(q)

def run(*args, **kw):
    return subprocess.run(list(map(str, args)), check=True, **kw)

with tempfile.TemporaryDirectory(prefix='agentbox-quota-test-') as td:
    root = Path(td)
    root.chmod(0o755)
    image = root / 'disk.img'
    with image.open('wb') as f:
        f.truncate(96 << 20)
    mount = root / 'mount'
    mount.mkdir()
    run('mkfs.ext4', '-q', '-F', image)
    run('tune2fs', '-O', 'project,quota', '-Q', 'prjquota', image, stdout=subprocess.DEVNULL)
    check=subprocess.run(['e2fsck','-f','-p',str(image)])
    assert check.returncode in (0,1), 'filesystem conversion check failed'
    mounted = False
    try:
        run('mount', '-o', 'loop,prjquota', image, mount)
        mounted = True
        device = subprocess.check_output(['findmnt', '-no', 'SOURCE', str(mount)], text=True).strip()
        uuid = subprocess.check_output(['blkid', '-s', 'UUID', '-o', 'value', device], text=True).strip()
        policy = {'mount': str(mount), 'uuid': uuid, 'user_bytes': 2 << 20, 'cache_bytes': 1 << 20}
        assert q.mount_device(policy) == device
        try:
            q.mount_device(dict(policy, uuid='00000000-0000-0000-0000-000000000000'))
            raise AssertionError('wrong UUID accepted')
        except ValueError:
            pass
        data = mount / 'data'
        data.mkdir(mode=0o755)
        req = {'policy': policy, 'data': str(data), 'tools': '', 'user': 'alice', 'owner': 'a' * 64}
        result = subprocess.check_output([sys.executable, str(Path(q.__file__))], input=json.dumps(req), text=True)
        assert json.loads(result)['data']['limit_bytes'] == 2 << 20
        userdir = data / 'users/alice'
        (data / 'users').chmod(0o755)
        userdir.chmod(0o755)
        # Same UID as the real containers, nested directories must inherit quota.
        workspace = userdir / 'workspace'
        workspace.mkdir()
        os.chown(workspace, 1000, 1000)
        script = '''import os,errno,fcntl,struct,sys,ctypes
# Apply the same three masked ioctl rules as quotaContainerOptions in a
# disposable child. No filter is installed in the operator or server process.
lib=ctypes.CDLL('libseccomp.so.2',use_errno=True)
lib.seccomp_init.argtypes=[ctypes.c_uint32];lib.seccomp_init.restype=ctypes.c_void_p
lib.seccomp_syscall_resolve_name.argtypes=[ctypes.c_char_p]
lib.seccomp_rule_add_array.argtypes=[ctypes.c_void_p,ctypes.c_uint32,ctypes.c_int,ctypes.c_uint,ctypes.c_void_p]
lib.seccomp_load.argtypes=[ctypes.c_void_p]
class Arg(ctypes.Structure):
 _fields_=[('arg',ctypes.c_uint),('op',ctypes.c_int),('a',ctypes.c_uint64),('b',ctypes.c_uint64)]
ctx=lib.seccomp_init(0x7fff0000)
for command in (0x401c5820,0x40086602,0x40046602):
 arg=Arg(1,7,0xffffffff,command)
 assert lib.seccomp_rule_add_array(ctx,0x50000|13,lib.seccomp_syscall_resolve_name(b'ioctl'),1,ctypes.byref(arg))==0
assert lib.seccomp_load(ctx)==0
p=sys.argv[1]
os.mkdir(p+'/nested')
fd=os.open(p+'/nested',os.O_RDONLY)
a=list(struct.unpack('IIIII8s',fcntl.ioctl(fd,0x801c581f,bytes(28))))
try:
 a[0]&=~0x200
 fcntl.ioctl(fd,0x401c5820,struct.pack('IIIII8s',*a))
 raise AssertionError('inheritance guard missing')
except PermissionError:pass
os.close(fd)
try:
 with open(p+'/nested/full','wb',buffering=0) as f:
  for i in range(8):f.write(b'x'*(1<<20))
 raise AssertionError('quota bypassed')
except OSError as e:
 assert e.errno==errno.EDQUOT,e
print('uid1000_nested_write_EDQUOT')
'''
        run(sys.executable, '-c', script, workspace, user=1000, group=1000, extra_groups=[])
        # The service runs as root without CAP_SYS_RESOURCE; HTTP writes must
        # respect the same quota as container writes.
        run('setpriv', '--bounding-set=-sys_resource', sys.executable, '-c',
            "import sys,errno;\ntry:\n f=open(sys.argv[1],'wb',buffering=0);f.write(b'x'*(4<<20));raise AssertionError('server bypassed quota')\nexcept OSError as e:assert e.errno==errno.EDQUOT,e", workspace/'server-write')
        # A different project with the same UID must still be writable.
        req.update(user='bob', owner='b' * 64)
        run(sys.executable, q.__file__, input=json.dumps(req), text=True)
        bob = data / 'users/bob'
        bob.chmod(0o755)
        os.chown(bob,1000,1000)
        run(sys.executable,'-c',"import pathlib,sys;pathlib.Path(sys.argv[1]).write_bytes(b'ok')",bob/'ok',user=1000,group=1000,extra_groups=[])
        req.update(owner='c' * 64)
        failure = subprocess.run([sys.executable, q.__file__], input=json.dumps(req), text=True, capture_output=True)
        assert failure.returncode != 0 and 'previous user' in failure.stderr
        # Deleting bob releases both kernel limits; his leftover file keeps the
        # name reserved, and once removed a new bob gets a fresh project.
        helper = lambda body: json.loads(subprocess.check_output([sys.executable, q.__file__], input=json.dumps(body), text=True))
        retired = helper({'policy': policy, 'data': str(data), 'user': 'retired', 'owner': 'b' * 64, 'retire': True})['retired']
        usage = q.quota(device, retired[0]) if len(retired) == 1 else {}
        assert usage.get('used_bytes', 0) > 0 and usage['limit_bytes'] == 0 and usage['file_limit'] == 0, (retired, usage)
        report = helper({'policy': policy, 'data': str(data), 'report': True})
        assert [p['retired'] for p in report['projects'] if p['project'] == retired[0]] == [True]
        failure = subprocess.run([sys.executable, q.__file__], input=json.dumps(req), text=True, capture_output=True)
        assert failure.returncode != 0 and 'previous user' in failure.stderr
        (bob / 'ok').unlink()
        assert helper(req)['data']['limit_bytes'] == 2 << 20
        assert len(list((data / '.storage-projects-retired').glob('*.json'))) == 1
        print('PASS: kernel limit, inheritance/bypass, same-UID isolation, identity reuse, retirement and mount UUID')
    finally:
        if mounted:
            run('umount', mount)
