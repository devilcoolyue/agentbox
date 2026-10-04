#!/usr/bin/env python3
"""Exercise macOS updater ENOSPC on a disposable 64 MiB image, never an installed app."""
import argparse
import json
import os
from pathlib import Path
import platform
import plistlib
import re
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--target', choices=['aarch64-apple-darwin', 'x86_64-apple-darwin'])
    args = parser.parse_args()
    if platform.system() != 'Darwin':
        raise SystemExit('The bounded updater disk-image probe requires macOS')
    native_target = 'aarch64-apple-darwin' if platform.machine() == 'arm64' else 'x86_64-apple-darwin'
    if args.target and args.target != native_target:
        raise SystemExit('Updater disk-image test must execute on its native target')
    root = Path(__file__).resolve().parents[2]
    report = {'macos_updater_enospc': 'failed', 'synthetic_bundle': True,
              'installed_application_touched': False, 'volume_size_mib': 64,
              'physical_power_loss_tested': False, 'arch': platform.machine()}
    with tempfile.TemporaryDirectory(prefix='agentbox-updater-space-') as directory:
        private = Path(directory).resolve()
        image = private / 'fixture.dmg'
        mount = private / 'mounted'
        mount.mkdir()
        device = None
        try:
            subprocess.run(['hdiutil', 'create', '-size', '64m', '-fs', 'HFS+', '-type', 'UDIF',
                            '-volname', 'AgentboxUpdaterTest', str(image)], check=True, timeout=30)
            attached = plistlib.loads(subprocess.check_output(['hdiutil', 'attach', '-nobrowse', '-mountpoint',
                                      str(mount), '-plist', str(image)], timeout=30))
            for item in attached.get('system-entities', []):
                if item.get('mount-point') and Path(item['mount-point']).resolve() == mount:
                    device = item.get('dev-entry')
            if not device or not re.fullmatch(r'/dev/disk\d+(?:s\d+)?', device):
                raise ValueError('Cannot identify the owned mounted image')
            fixture = mount / 'updater-fixture'
            fixture.mkdir()
            (fixture / '.agentbox-update-space-fixture').write_bytes(b'owned-updater-enospc-volume-v1')
            environment = {**os.environ, 'AGENTBOX_UPDATE_FULL_ROOT': str(fixture), 'CARGO_INCREMENTAL': '0'}
            command = ['cargo', 'test', '--locked', '--manifest-path', str(root / 'desktop/src-tauri/Cargo.toml'), '--lib']
            if args.target:
                command.extend(['--target', args.target])
            command.extend(['updates::macos::tests::real_disk_full_keeps_installed_bundle', '--', '--exact', '--ignored', '--nocapture'])
            subprocess.run(command, cwd=root, env=environment, check=True, timeout=300)
            report['macos_updater_enospc'] = 'passed'
        finally:
            try:
                # If parsing the attachment report failed, the exact mountpoint
                # we created is still safe authority for this image's cleanup.
                cleanup = device or (str(mount) if os.path.ismount(mount) else None)
                if cleanup:
                    detached = subprocess.run(['hdiutil', 'detach', cleanup], timeout=30, capture_output=True)
                    if detached.returncode:
                        subprocess.run(['hdiutil', 'detach', '-force', cleanup], check=True, timeout=30)
                    report['fixture_cleanup'] = 'passed'
                else:
                    report['fixture_cleanup'] = 'not_mounted'
            except Exception as error:
                report['macos_updater_enospc'] = 'failed'
                report['fixture_cleanup'] = 'failed'
                report['cleanup_error'] = str(error)
                raise
            finally:
                args.report.parent.mkdir(parents=True, exist_ok=True)
                args.report.write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
