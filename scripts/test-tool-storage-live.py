#!/usr/bin/env python3
"""Actual Linux mount/chown regression in a private Docker volume; no models."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import uuid

def run(*args, **kw):return subprocess.run(list(map(str,args)),check=True,**kw)
p=argparse.ArgumentParser(description=__doc__);p.add_argument('--image',required=True);args=p.parse_args()
info=json.loads(subprocess.check_output(['docker','image','inspect',args.image]))[0]
arch=info['Architecture'];assert info['Os']=='linux' and arch in ('amd64','arm64')
volume='agentbox-tools-test-'+uuid.uuid4().hex
cid=None
try:
 run('docker','volume','create',volume,stdout=subprocess.DEVNULL)
 mount=json.loads(subprocess.check_output(['docker','volume','inspect',volume]))[0]['Mountpoint']
 with tempfile.TemporaryDirectory(prefix='agentbox-tools-test-') as td:
  binary=Path(td)/'tools.test'
  run('go','test','-c','-o',binary,'./internal/dockerx',env=dict(os.environ,GOOS='linux',GOARCH=arch,CGO_ENABLED='0'))
  cid=subprocess.check_output(['docker','create','--network','none','--user','0:0','--entrypoint','/tmp/tools.test','--mount','type=volume,src='+volume+',dst='+mount,'--mount','type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock','--env','DOCKER_HOST=unix:///var/run/docker.sock','--env','AGENTBOX_TOOL_STORAGE_TEST_IMAGE='+info['Id'],'--env','AGENTBOX_TOOL_STORAGE_TEST_ROOT='+mount+'/fixture',info['Id'],'-test.run=^TestToolStorageLive$','-test.v','-test.timeout=240s'],text=True).strip()
  run('docker','cp',binary,cid+':/tmp/tools.test')
  run('docker','start','-a',cid,timeout=300)
  assert subprocess.check_output(['docker','inspect','-f','{{.State.ExitCode}}',cid],text=True).strip()=='0','Linux tool-storage test failed'
finally:
 if cid:run('docker','rm','-f',cid,stdout=subprocess.DEVNULL)
 run('docker','volume','rm',volume,stdout=subprocess.DEVNULL)
