#!/usr/bin/env python3
import os
from pathlib import Path
import subprocess
import sys
root=Path(__file__).resolve().parents[1]
configuration=Path(os.environ['RUNNER_TEMP'])/'desktop-candidate.json'
command=[sys.executable,str(root/'scripts/candidate-config.py'),'--version',os.environ['DESKTOP_VERSION'],'--target',os.environ['DESKTOP_TARGET'],'--output',str(configuration)]
if os.environ.get('DESKTOP_SIGNED')=='true':command.append('--signed')
subprocess.run(command,check=True)
npm='npm.cmd' if os.name=='nt' else 'npm'
subprocess.run([npm,'run','tauri','--','build','--target',os.environ['DESKTOP_TARGET'],'--bundles',os.environ['DESKTOP_BUNDLES'],'--config',str(configuration)],cwd=root,check=True)
