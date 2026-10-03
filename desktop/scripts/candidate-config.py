#!/usr/bin/env python3
"""Create the Tauri override for a release candidate; never invent identities."""
import argparse
import json
import os
from pathlib import Path
import re
import tomllib

DESKTOP=Path(__file__).resolve().parents[1]

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version',required=True)
    parser.add_argument('--target',choices=['aarch64-apple-darwin','x86_64-apple-darwin','x86_64-pc-windows-msvc'],required=True)
    parser.add_argument('--signed',action='store_true')
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?',args.version):raise SystemExit('Invalid version')
    versions=[json.loads((DESKTOP/'package.json').read_text())['version'],json.loads((DESKTOP/'src-tauri/tauri.conf.json').read_text())['version'],tomllib.loads((DESKTOP/'src-tauri/Cargo.toml').read_text())['package']['version']]
    if any(version!=args.version for version in versions):raise SystemExit('Version must match reviewed package.json, Cargo.toml and tauri.conf.json')
    bundle={'createUpdaterArtifacts':args.signed,'resources':{'../third-party/':'third-party/','../vendor-notices/':'third-party/vendor/'},'windows':{'nsis':{'installMode':'currentUser'},'webviewInstallMode':{'type':'offlineInstaller','silent':True}}}
    if args.signed:
        required=['TAURI_SIGNING_PRIVATE_KEY','AGENTBOX_UPDATER_PUBLIC_KEY']
        required+=['WINDOWS_CERTIFICATE_THUMBPRINT'] if 'windows' in args.target else ['APPLE_SIGNING_IDENTITY','APPLE_CERTIFICATE','APPLE_CERTIFICATE_PASSWORD','APPLE_API_KEY','APPLE_API_ISSUER','APPLE_API_KEY_PATH']
        missing=[key for key in required if not os.environ.get(key)]
        if missing:raise SystemExit('Signed candidate requires: '+', '.join(missing))
        if 'windows' in args.target:
            bundle['windows'].update({'certificateThumbprint':os.environ['WINDOWS_CERTIFICATE_THUMBPRINT'],'digestAlgorithm':'sha256','timestampUrl':'http://timestamp.digicert.com'})
        else:
            identity=os.environ['APPLE_SIGNING_IDENTITY']
            if not identity.startswith('Developer ID Application:'):raise SystemExit('Require an explicit Developer ID Application identity')
            bundle['macOS']={'signingIdentity':identity}
    else:
        # Never accidentally include a production update key in a test package.
        if os.environ.get('AGENTBOX_UPDATER_PUBLIC_KEY'):raise SystemExit('Unsigned candidate must not enable production updates')
    args.output.parent.mkdir(parents=True,exist_ok=True)
    config={'bundle':bundle}
    if args.signed:config['plugins']={'updater':{'pubkey':os.environ['AGENTBOX_UPDATER_PUBLIC_KEY'],'requireSignedVersion':True}}
    args.output.write_text(json.dumps(config,indent=2)+'\n')
    print('Prepared '+('signed' if args.signed else 'unsigned')+' desktop candidate configuration')

if __name__=='__main__':main()
