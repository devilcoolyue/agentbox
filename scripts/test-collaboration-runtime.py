#!/usr/bin/env python3
"""Linux UID/chown + real server workflow, synthetic Docker/CLI; no provider/socket."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.request

def run(*args, **kwargs):
    return subprocess.run(list(map(str,args)),check=True,**kwargs)

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--image',required=True)
parser.add_argument('--browser',action='store_true')
parser.add_argument('--chat',action='store_true',help='Exercise the integrated chat composer against the real backend')
args=parser.parse_args()
if args.chat:args.browser=True
image=json.loads(subprocess.check_output(['docker','image','inspect',args.image]))[0]
if image['Os']!='linux' or image['Architecture'] not in ('arm64','amd64'):
    raise SystemExit('requires a local Linux image')
cid=None
network=None
try:
    with tempfile.TemporaryDirectory(prefix='agentbox-collaboration-') as tmp:
        binary=Path(tmp)/'collaboration.test'
        run('go','test','-c','-o',binary,'./internal/server',env=dict(os.environ,GOOS='linux',GOARCH=image['Architecture'],CGO_ENABLED='0'))
        network_args=['--network','none']
        if args.browser:
            network=subprocess.check_output(['docker','network','create','--opt','com.docker.network.bridge.enable_ip_masquerade=false','agentbox-collab-'+os.urandom(6).hex()],text=True).strip()
            network_args=['--network',network,'--publish','127.0.0.1::18888','--env','AGENTBOX_COLLABORATION_BROWSER_TEST=1']
        if args.chat:network_args+=['--env','AGENTBOX_COLLABORATION_CHAT_BROWSER_TEST=1']
        cid=subprocess.check_output(['docker','create',*network_args,'--user','0:0','--entrypoint','/bin/sh','--env','AGENTBOX_COLLABORATION_RUNTIME_TEST=1',image['Id'],'-c','umask 077; exec /collaboration.test "$@"','--','-test.run=^TestCollaborationRuntimeIntegration$/codex-claude$' if args.browser else '-test.run=^TestCollaborationRuntimeIntegration$','-test.v','-test.timeout=180s'],text=True).strip()
        run('docker','cp',binary,cid+':/collaboration.test')
        if args.browser:
            run('docker','start',cid,stdout=subprocess.DEVNULL)
            address=subprocess.check_output(['docker','port',cid,'18888'],text=True).strip()
            url='http://'+address
            for attempt in range(100):
                try:
                    with urllib.request.urlopen(url+'/fixture',timeout=1) as response:
                        if response.status==200:break
                except (OSError,urllib.error.URLError):time.sleep(.1)
            else:raise SystemExit('browser fixture did not start')
            run('node','scripts/test-collaboration-chat-e2e.mjs' if args.chat else 'scripts/test-collaboration-e2e.mjs',url,timeout=160)
            run('docker','wait',cid,timeout=20)
        else:run('docker','start','-a',cid,timeout=210)
        code=subprocess.check_output(['docker','inspect','-f','{{.State.ExitCode}}',cid],text=True).strip()
        if code!='0':raise SystemExit('collaboration runtime fixture failed (exit '+code+')')
finally:
    if cid:
        if args.browser:run('docker','logs',cid)
        run('docker','rm','-f',cid,stdout=subprocess.DEVNULL)
    if network:run('docker','network','rm',network,stdout=subprocess.DEVNULL)
