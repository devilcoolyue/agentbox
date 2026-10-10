#!/usr/bin/env python3
"""Opt-in official Go/Playwright download + shared read-only execution acceptance."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT=Path(__file__).resolve().parent.parent
spec=importlib.util.spec_from_file_location('tools',ROOT/'scripts/tool-storage.py')
tools=importlib.util.module_from_spec(spec);spec.loader.exec_module(tools)
def run(*args,**kw):return subprocess.run(list(map(str,args)),check=True,**kw)
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--image',required=True);p.add_argument('--go-version',default='go1.26.9');p.add_argument('--playwright-version',default='1.58.2');args=p.parse_args()
info=json.loads(subprocess.check_output(['docker','image','inspect',args.image]))[0]
assert info['Os']=='linux' and info['Architecture'] in ('arm64','amd64')
with tempfile.TemporaryDirectory(prefix='agentbox-public-tool-acceptance-') as td:
 root=Path(td)
 run('python3',ROOT/'scripts/tool-storage.py','--root',root,'install-go','--version',args.go_version,'--arch',info['Architecture'])
 run('python3',ROOT/'scripts/tool-storage.py','--root',root,'install-playwright','--image',info['Id'],'--version',args.playwright_version,'--browsers','chromium')
 platform=next((root/'public/playwright').iterdir()).name
 cache='/var/cache/agentbox-user/'+platform
 cid=subprocess.check_output(['docker','create','--network','bridge','--user','1000:1000','--security-opt','no-new-privileges','--security-opt','seccomp='+str(ROOT/'internal/dockerx/browser-seccomp.json'),'--shm-size','256m','--mount','type=bind,src='+str(root/'public')+',dst=/opt/agentbox-tools,readonly','--tmpfs','/var/cache/agentbox-user:rw,nosuid,nodev,uid=1000,gid=1000,size=768m','--env','HOME=/tmp','--env','GOCACHE='+cache+'/go-build','--env','GOMODCACHE='+cache+'/go-mod','--env','PLAYWRIGHT_BROWSERS_PATH='+cache+'/playwright','--env','PLAYWRIGHT_SKIP_BROWSER_GC=1','--env','CGO_ENABLED=0','--env','GOTOOLCHAIN=local','--entrypoint','sleep',info['Id'],'infinity'],text=True).strip()
 try:
  run('docker','start',cid,stdout=subprocess.DEVNULL)
  run('docker','exec',cid,'python3','-I','-c',(ROOT/'internal/dockerx/tool_storage.py').read_text(),platform)
  run('docker','exec',cid,'npm','install','--ignore-scripts','--no-audit','--no-fund','--prefix','/tmp/acceptance','playwright@'+args.playwright_version,timeout=180)
  # No network during install reuse/launch/compile. Any attempted redownload fails.
  run('docker','network','disconnect','bridge',cid)
  run('docker','exec',cid,'node','/tmp/acceptance/node_modules/playwright/cli.js','install','chromium',timeout=90)
  run('docker','exec',cid,'node','-e',"const fs=require('fs');const {chromium}=require('/tmp/acceptance/node_modules/playwright');(async()=>{if(!fs.realpathSync(chromium.executablePath()).startsWith('/opt/agentbox-tools/'))throw Error('not shared');const b=await chromium.launch({chromiumSandbox:true});const p=await b.newPage();await p.setContent('<title>shared-browser-ok</title>');if(await p.title()!=='shared-browser-ok')throw Error('render failed');console.log('shared-browser-ok');await b.close();})().catch(e=>{console.error(e);process.exit(1)});",timeout=90)
  run('docker','exec',cid,'python3','-I','-c',"from pathlib import Path;Path('/tmp/main.go').write_text('package main\\nimport \"fmt\"\\nfunc main(){fmt.Println(\"shared-go-ok\")}\\n')")
  run('docker','exec',cid,'/opt/agentbox-tools/go/linux-'+info['Architecture']+'/'+args.go_version+'/bin/go','run','/tmp/main.go',timeout=180)
  tools.verify(root)
 finally:run('docker','rm','-f',cid,stdout=subprocess.DEVNULL)
print('Official Go and Playwright shared-tool runtime acceptance passed; all temporary resources removed')
