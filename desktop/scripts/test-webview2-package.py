#!/usr/bin/env python3
"""Audit the actual full NSIS payload; this is not a missing-runtime install test."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PureWindowsPath
import shutil
import subprocess
import tempfile
import threading
import time
import urllib.request

from webview2_support import (MAX_PACKAGE, EMBEDDED_FILENAME, VENDOR_FILENAME, SOURCE_PREFIX,
                              SELECTION_URL, authenticode, bounded_file, digest, nsis_source,
                              pe_machine, write_report)


def archive_entry(listing, basename):
    entries = []
    for block in listing.replace('\r\n', '\n').split('\n\n'):
        fields = dict(line.split(' = ', 1) for line in block.splitlines() if ' = ' in line)
        if PureWindowsPath(fields.get('Path', '')).name != basename:
            continue
        size = fields.get('Size', '')
        if not size.isascii() or not size.isdecimal() or not 0 < int(size) <= MAX_PACKAGE:
            raise ValueError('Embedded artifact has an invalid or excessive size')
        entries.append((fields['Path'], int(size)))
    if len(entries) != 1:
        raise ValueError('Expected exactly one embedded artifact: ' + basename)
    return entries[0]


def extract(seven_zip, package, entry, destination):
    name, expected = entry
    process = subprocess.Popen([str(seven_zip), 'e', '-so', str(package), name],
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    timer = threading.Timer(90, process.kill)
    timer.start()
    count = 0
    try:
        with destination.open('xb') as output:
            while chunk := process.stdout.read(1024 * 1024):
                count += len(chunk)
                if count > expected or count > MAX_PACKAGE:
                    raise ValueError('Archive extraction exceeded advertised size')
                output.write(chunk)
        if process.wait(timeout=5) != 0 or count != expected:
            raise ValueError('Archive extraction failed or was truncated')
    finally:
        timer.cancel()
        if process.poll() is None:
            process.kill()
        process.wait(timeout=5)
        process.stdout.close()


def verify_microsoft_source(url, expected_digest, expected_bytes):
    if not url.startswith(SOURCE_PREFIX) or not url.endswith('/' + VENDOR_FILENAME):
        raise ValueError('Vendor source must be the Microsoft x64 standalone installer URL')
    hasher = hashlib.sha256()
    count, deadline = 0, time.monotonic() + 180
    with urllib.request.urlopen(url, timeout=30) as response:
        if response.geturl() != url or response.status != 200:
            raise ValueError('Unexpected Microsoft source redirect or HTTP status')
        while chunk := response.read(1024 * 1024):
            count += len(chunk)
            if count > expected_bytes or time.monotonic() > deadline:
                raise ValueError('Microsoft source exceeded the expected size or deadline')
            hasher.update(chunk)
    if count != expected_bytes or hasher.hexdigest() != expected_digest:
        raise ValueError('Bundled vendor payload differs from its Microsoft download source')


def audit(package, script, seven_zip, notice):
    original = bounded_file(package)
    source_path, source_url = nsis_source(script.read_text(encoding='utf-8-sig'))
    source = Path(source_path)
    source_metadata = bounded_file(source)
    listing = subprocess.run([str(seven_zip), 'l', '-slt', '-sccUTF-8', str(package)], capture_output=True,
                             text=True, encoding='utf-8', timeout=45, check=True).stdout
    if len(listing) > 16 * 1024 * 1024:
        raise ValueError('NSIS listing exceeded limit')
    with tempfile.TemporaryDirectory(prefix='agentbox-webview2-audit-') as temporary:
        temporary = Path(temporary)
        runtime = temporary / EMBEDDED_FILENAME
        executable = temporary / 'agentbox-desktop.exe'
        bundled_notice = temporary / 'Microsoft-WebView2.txt'
        for basename, target in [(EMBEDDED_FILENAME, runtime), ('agentbox-desktop.exe', executable),
                                 ('Microsoft-WebView2.txt', bundled_notice)]:
            extract(seven_zip, package, archive_entry(listing, basename), target)
        runtime_metadata = bounded_file(runtime)
        if runtime_metadata != source_metadata:
            raise ValueError('NSIS embedded Runtime differs from the reviewed Tauri source file')
        if pe_machine(executable) != 0x8664:
            raise ValueError('Packaged Agentbox executable is not Windows x64')
        signature = authenticode(runtime)
        if digest(bundled_notice) != digest(notice):
            raise ValueError('Missing or changed Microsoft proprietary Runtime notice')
        verify_microsoft_source(source_url, runtime_metadata['sha256'], runtime_metadata['bytes'])
    if bounded_file(package) != original:
        raise ValueError('NSIS artifact changed during its audit')
    return {
        'status': 'passed', 'scope': 'actual_full_nsis_payload_audit', 'package': original,
        'target': 'x86_64-pc-windows-msvc', 'runtime_installer': runtime_metadata,
        'microsoft_signature': signature, 'selection_url': SELECTION_URL,
        'source_url': source_url, 'source_bytes_match': True,
        'architecture_evidence': 'NSIS ARCH=x64, Microsoft X64 installer URL, packaged app PE=AMD64',
        'runtime_version': None, 'runtime_version_note': 'Installer ProductVersion is not the installed browser Runtime version',
        'proprietary_notice_present': True, 'updater_download_limit_bytes': MAX_PACKAGE,
        'missing_runtime_installation': 'not_run', 'offline_installation': 'not_run', 'gui': 'not_run',
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('package', type=Path)
    parser.add_argument('--nsis-script', type=Path, required=True, help='target/<triple>/release/nsis/x64/installer.nsi')
    parser.add_argument('--seven-zip', type=Path)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    report = {'status': 'failed', 'scope': 'actual_full_nsis_payload_audit'}
    try:
        if os.name != 'nt':
            raise ValueError('Microsoft Authenticode validation requires actual Windows')
        seven_zip = args.seven_zip or Path(os.environ['ProgramFiles']) / '7-Zip/7z.exe'
        if not seven_zip.is_file():
            located = shutil.which('7z')
            if not located:
                raise ValueError('7-Zip is required to inspect the actual NSIS payload')
            seven_zip = Path(located)
        notice = Path(__file__).resolve().parents[1] / 'vendor-notices/Microsoft-WebView2.txt'
        report = audit(args.package.resolve(), args.nsis_script.resolve(), seven_zip, notice)
    except Exception as error:
        report['error'] = str(error)
    finally:
        write_report(args.report, report)
    print(json.dumps(report, indent=2))
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
