#!/usr/bin/env python3
"""Run an explicit smoke build against its synthetic fixture, never production."""
import argparse
import ctypes
import json
import os
from pathlib import Path
import plistlib
import signal
import subprocess
import sys
import tempfile
import time


def macos_session_diagnostics():
    if sys.platform != 'darwin':
        return None
    cg = ctypes.CDLL('/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics')
    cf = ctypes.CDLL('/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation')
    cf.CFStringCreateWithCString.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_uint32]
    cf.CFStringCreateWithCString.restype = ctypes.c_void_p
    cf.CFDictionaryGetValue.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
    cf.CFDictionaryGetValue.restype = ctypes.c_void_p
    cf.CFBooleanGetValue.argtypes = [ctypes.c_void_p]
    cf.CFBooleanGetValue.restype = ctypes.c_bool
    cf.CFGetTypeID.argtypes = [ctypes.c_void_p]
    cf.CFGetTypeID.restype = ctypes.c_ulong
    cf.CFBooleanGetTypeID.restype = ctypes.c_ulong
    cf.CFRelease.argtypes = [ctypes.c_void_p]
    cg.CGSessionCopyCurrentDictionary.restype = ctypes.c_void_p
    session = cg.CGSessionCopyCurrentDictionary()
    result = {'console_session_present': bool(session), 'screen_locked': None, 'login_done': None}
    if session:
        for name, field in [('CGSSessionScreenIsLocked', 'screen_locked'), ('kCGSessionLoginDoneKey', 'login_done')]:
            key = cf.CFStringCreateWithCString(None, name.encode(), 0x08000100)
            value = cf.CFDictionaryGetValue(session, key)
            if value and cf.CFGetTypeID(value) == cf.CFBooleanGetTypeID():
                result[field] = cf.CFBooleanGetValue(value)
            cf.CFRelease(key)
        cf.CFRelease(session)
    cg.CGMainDisplayID.restype = ctypes.c_uint32
    display = cg.CGMainDisplayID()
    for name, field in [('CGDisplayIsActive', 'display_active'), ('CGDisplayIsOnline', 'display_online'), ('CGDisplayIsAsleep', 'display_asleep')]:
        function = getattr(cg, name)
        function.argtypes = [ctypes.c_uint32]
        function.restype = ctypes.c_uint32
        result[field] = bool(function(display))
    return result


def launch_command(binary, report, environment, platform=sys.platform):
    if platform != 'darwin' or binary.parent.name != 'MacOS' or binary.parent.parent.name != 'Contents':
        return [str(binary)], 'direct-executable'
    bundle = binary.parent.parent.parent
    info = plistlib.loads((bundle / 'Contents/Info.plist').read_bytes())
    if (bundle.suffix != '.app' or not info.get('CFBundleIdentifier', '').endswith('.desktop.smoke')
            or info.get('CFBundleExecutable') != binary.name):
        raise ValueError('Bundled smoke runner requires the explicit Agentbox Smoke application')
    # LaunchServices handles foreground activation and app registration. Its
    # process does not inherit this shell environment, so pass fixture-only vars.
    command = ['/usr/bin/open', '--wait-apps', '--new', '--stdout', str(report.with_suffix('.stdout.log')),
               '--stderr', str(report.with_suffix('.stderr.log'))]
    for name, value in sorted(environment.items()):
        if name.startswith('AGENTBOX_SMOKE_'):
            command.extend(['--env', name + '=' + value])
    command.extend(['-a', str(bundle)])
    return command, 'launch-services'


def stop_owned_application(binary, report):
    pidfile = report.with_suffix('.pid')
    if not pidfile.exists():
        return
    value = pidfile.read_text(encoding='utf-8').strip()
    if not value.isascii() or not value.isdigit() or int(value) < 2 or int(value) == os.getpid():
        raise ValueError('Invalid smoke fixture PID')
    pid = int(value)
    for attempt in range(31):
        inspected = subprocess.run(['/bin/ps', '-ww', '-p', str(pid), '-o', 'comm='], text=True, capture_output=True, timeout=5)
        if inspected.returncode != 0:
            return
        # Only the executable launched for this exact fixture may be terminated.
        if Path(inspected.stdout.strip()).resolve() != binary:
            raise ValueError('Smoke PID no longer belongs to the fixture executable')
        try:
            if attempt == 0:
                os.kill(pid, signal.SIGTERM)
            elif attempt == 30:
                os.kill(pid, signal.SIGKILL)
            time.sleep(0.1)
        except ProcessLookupError:
            return


def print_diagnostics(report):
    for suffix, label in (('.stages', 'Completed smoke stages'), ('.window.jsonl', 'Native window diagnostics')):
        path = report.with_suffix(suffix)
        print(label + ':', path.read_text(encoding='utf-8')[-16384:] if path.exists() else 'none', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('binary', type=Path)
    parser.add_argument('--mode', choices=['legacy', 'projects'], default='legacy')
    args = parser.parse_args()
    binary = args.binary.resolve()
    session = macos_session_diagnostics()
    if session:
        print('Native desktop session:', json.dumps(session), flush=True)
        if session['screen_locked'] is True:
            raise SystemExit('Native smoke requires an unlocked desktop; the current macOS session is locked')
    with tempfile.TemporaryDirectory(prefix='agentbox-desktop-smoke-') as directory:
        report = Path(directory) / 'report.json'
        environment = {**os.environ, 'AGENTBOX_SMOKE_REPORT': str(report), 'AGENTBOX_SMOKE_MODE': args.mode}
        command, launcher = launch_command(binary, report, environment)
        process = subprocess.Popen(command, env=environment)
        try:
            try:
                code = process.wait(timeout=60)
            except subprocess.TimeoutExpired:
                print_diagnostics(report)
                raise SystemExit('Native smoke timed out; no success report')
            if not report.exists():
                print_diagnostics(report)
                raise SystemExit(f'No smoke report; {launcher} exited {code}')
            result = json.loads(report.read_text(encoding='utf-8'))
            result['launcher'] = launcher
            print(json.dumps(result, ensure_ascii=False, indent=2))
            # open's status is not the application's status; the required native
            # success report remains authoritative for both launch mechanisms.
            if code != 0 or result.get('ok') is not True:
                print_diagnostics(report)
                raise SystemExit('Native desktop smoke failed')
        finally:
            try:
                if launcher == 'launch-services':
                    stop_owned_application(binary, report)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=10)


if __name__ == '__main__':
    main()
