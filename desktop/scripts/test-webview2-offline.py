#!/usr/bin/env python3
"""Record genuine Runtime preconditions; execute only inside an offline disposable guest.

This tool never removes WebView2, edits its registry, changes adapters/firewall,
or disconnects the GitHub host. A preinstalled Runtime is a blocked absence test.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

from webview2_support import (UNINSTALL_KEY, bounded_file, digest, network_inventory,
                              registry_values, require_offline, runtime_inventory,
                              runtime_is_absent, sdk_loader, write_report)


def validate_package_audit(package, report):
    actual = bounded_file(package)
    if (report.get('status') != 'passed' or report.get('scope') != 'actual_full_nsis_payload_audit'
            or report.get('package') != actual or report.get('target') != 'x86_64-pc-windows-msvc'
            or report.get('source_bytes_match') is not True or report.get('proprietary_notice_present') is not True
            or report.get('zero_version_repair_payload_matches') is not True
            or report.get('microsoft_signature', {}).get('status') != 'Valid'):
        raise ValueError('Require a successful actual-payload audit for this exact NSIS artifact')
    return actual


def install_command(package, directory):
    if any(character in str(path) for path in (package, directory) for character in ('"', '\r', '\n', '\0')):
        raise ValueError('Invalid NSIS path')
    # NSIS documents /D as the final, unquoted remainder even for spaced paths.
    # This is a Windows CreateProcess command line, never shell=True.
    return subprocess.list2cmdline([str(package), '/S']) + ' /D=' + str(directory)


def uninstaller_state():
    return registry_values(UNINSTALL_KEY, ['DisplayVersion', 'InstallLocation', 'UninstallString'])


def execute_guest(args, report):
    if os.environ.get('AGENTBOX_DISPOSABLE_WINDOWS_GUEST') != '1':
        raise ValueError('Explicit disposable-guest acknowledgement is required')
    if os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted':
        raise ValueError('Do not run the offline guest installer on the GitHub runner host')
    if not args.smoke_binary or not args.smoke_binary.is_file():
        raise ValueError('An explicit desktop-smoke build is required for actual WebView/visible-page verification')
    if any(key.startswith('AGENTBOX_SMOKE_') for key in os.environ):
        raise ValueError('Inherited smoke fixture overrides are not allowed')
    before_app = uninstaller_state()
    if any(row['exists'] for row in before_app):
        raise ValueError('Guest already has an Agentbox install; do not replace it in this test')
    report['app_registry_before'] = before_app
    require_offline(report['network_before'])
    if not runtime_is_absent(report['runtime_before']):
        raise ValueError('Runtime must be genuinely absent before offline installation')
    spec = importlib.util.spec_from_file_location('installed_probe', Path(__file__).with_name('test-installed.py'))
    installed_probe = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(installed_probe)
    with tempfile.TemporaryDirectory(prefix='agentbox-offline-guest-') as temporary:
        temporary = Path(temporary)
        destination = temporary / 'Agentbox 离线安装'
        destination.mkdir()
        installed = destination / 'agentbox-desktop.exe'
        try:
            subprocess.run(install_command(args.package, destination), shell=False, check=True, timeout=300)
            if not installed.is_file():
                raise ValueError('NSIS did not install the application')
            report['runtime_after_install'] = runtime_inventory(args.loader)
            runtime = report['runtime_after_install']
            if runtime_is_absent(runtime) or not runtime['loader']['version'] or not any(
                    row['values'].get('pv') not in (None, '', '0.0.0.0') for row in runtime['registry']):
                raise ValueError('Offline installer did not provide a registered, discoverable Runtime')
            report['installed_diagnostics'] = installed_probe.diagnostics(installed, temporary, 'installed')
            if report['installed_diagnostics']['version'] != args.expected_version:
                raise ValueError('Installed app version does not match the reviewed candidate')
            report['app_registry_installed'] = uninstaller_state()
            if not any(row['exists'] for row in report['app_registry_installed']):
                raise ValueError('NSIS did not register the application uninstaller')
            require_offline(network_inventory())
            smoke_report = temporary / 'webview-smoke.json'
            environment = {**os.environ, 'AGENTBOX_SMOKE_REPORT': str(smoke_report), 'AGENTBOX_SMOKE_MODE': 'legacy'}
            subprocess.run([str(args.smoke_binary.resolve())], env=environment, check=True, timeout=90)
            smoke = json.loads(smoke_report.read_text(encoding='utf-8'))
            stages = smoke_report.with_suffix('.stages').read_text(encoding='utf-8').splitlines()
            if (smoke.get('ok') is not True or smoke.get('os') != 'windows' or smoke.get('arch') != 'x86_64'
                    or not {'window_visible', 'terminal_echo', 'finishing'} <= set(stages)):
                raise ValueError('Actual Windows WebView and visible terminal flow did not pass')
            report['native_webview_smoke'] = {'sha256': digest(args.smoke_binary), 'result': smoke, 'stages': stages}
            report['network_after_gui'] = network_inventory()
            require_offline(report['network_after_gui'])
        finally:
            uninstallers = list(destination.glob('*ninstall*.exe'))
            if len(uninstallers) > 1:
                raise ValueError('Unexpected multiple uninstallers; discard the isolated guest')
            if uninstallers:
                subprocess.run([str(uninstallers[0]), '/S'], check=True, timeout=180)
            remaining = []
            for _ in range(50):
                remaining = [str(path.relative_to(destination)) for path in destination.rglob('*')
                             if path.is_file() or path.is_symlink()]
                if not remaining:
                    break
                time.sleep(0.2)
            report['remaining_app_files'] = remaining
            report['app_registry_after_uninstall'] = uninstaller_state()
            if remaining or report['app_registry_after_uninstall'] != before_app:
                raise ValueError('Application files or registry entries remain after uninstall; discard the guest')
            report['app_uninstall'] = 'passed'
    report['runtime_after_uninstall'] = runtime_inventory(args.loader)
    if runtime_is_absent(report['runtime_after_uninstall']) or not report['runtime_after_uninstall']['loader']['version']:
        raise ValueError('Agentbox uninstall removed or broke the shared WebView2 Runtime')
    report['shared_runtime_cleanup'] = 'retained; dispose the entire test guest after collecting evidence'
    report['network_final'] = network_inventory()
    require_offline(report['network_final'])
    report['status'] = 'passed'
    report['acceptance'] = 'missing_runtime_offline_install_and_native_webview'
    report['installed_normal_app_gui'] = 'not_tested; normal package diagnostics plus explicit smoke WebView were tested'


def main():
    if hasattr(sys.stdout,'reconfigure'):
        sys.stdout.reconfigure(encoding='utf-8')
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('package', type=Path)
    parser.add_argument('--package-audit', type=Path, required=True)
    parser.add_argument('--loader', type=Path, help='Trusted x64 WebView2Loader.dll; defaults to the exact Cargo.lock SDK version')
    parser.add_argument('--smoke-binary', type=Path)
    parser.add_argument('--expected-version', default=json.loads((Path(__file__).resolve().parents[1] / 'package.json').read_text(encoding='utf-8'))['version'])
    parser.add_argument('--run-in-disposable-offline-guest', action='store_true')
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    report = {'status': 'failed', 'acceptance': 'not_run', 'host_network_changed': False, 'runtime_removed': False}
    try:
        if os.name != 'nt':
            raise ValueError('This probe requires actual Windows')
        args.package = args.package.resolve()
        report['package'] = validate_package_audit(args.package, json.loads(args.package_audit.read_text(encoding='utf-8')))
        args.loader = args.loader or sdk_loader()
        report['runtime_before'] = runtime_inventory(args.loader)
        report['network_before'] = network_inventory()
        absent = runtime_is_absent(report['runtime_before'])
        report['runtime_genuinely_absent'] = absent
        if not absent:
            report.update(status='blocked', reason='A Runtime, preview browser, or runtime files already exist; no destructive removal is attempted')
        else:
            try:
                require_offline(report['network_before'])
            except ValueError as error:
                report.update(status='blocked', reason=str(error))
            else:
                report.update(status='ready', reason='Absence and disconnected guest verified; installation has not run')
        if args.run_in_disposable_offline_guest:
            if report['status'] != 'ready':
                raise ValueError('Missing-runtime/offline prerequisites are not satisfied')
            execute_guest(args, report)
    except Exception as error:
        report['status'] = 'failed'
        report['error'] = str(error)
    finally:
        write_report(args.report, report)
    print(json.dumps(report, ensure_ascii=False, indent=2))
    if args.run_in_disposable_offline_guest:
        return 0 if report['status'] == 'passed' else 1
    return 1 if report['status'] == 'failed' else 0


if __name__ == '__main__':
    raise SystemExit(main())
