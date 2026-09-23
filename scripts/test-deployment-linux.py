#!/usr/bin/env python3
"""Linux metadata + installation/upgrade/rollback/migration smoke.

Runs in a disposable Docker container with synthetic data. systemctl is simulated
but the binary, HTTP, SQLite, full backup Docker checks and restore are real.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tempfile
import time
from types import SimpleNamespace
from unittest.mock import patch
import urllib.request


def inside():
 test_dir=Path(__file__).resolve().parent
 spec=importlib.util.spec_from_file_location('release',test_dir/'release.py')
 m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
 base=Path(tempfile.mkdtemp(prefix='deployment-fixture-'))
 source=base/'old';data=source/'data';data.mkdir(parents=True)
 cred=base/'external';cred.mkdir();(cred/'auth.json').write_text('{"synthetic":true}')
 home=data/'users/boxadmin/sessions/s1/home';home.mkdir(parents=True)
 workspace=home.parent/'workspace';workspace.mkdir();file=workspace/'fixture.txt';file.write_text('retained')
 os.chown(file,1000,1000);os.chmod(file,0o640);os.utime(file,(1600000000,1600000000))
 (workspace/'link').symlink_to('/shared/fixture.txt');os.lchown(workspace/'link',1000,1000)
 chats=home.parent/'chats';chats.mkdir();(chats/'t1.jsonl').write_text('{"synthetic":true}\n')
 template=data/'home-template';template.mkdir();(template/'.bashrc').write_text('fixture')
 cfg={'auth_token':'deployment-synthetic-password','listen':'127.0.0.1:18473','data_dir':str(data),'accounts':[{'id':'fixture','type':'codex','credentials_dir':str(cred)}]}
 config=source/'config.json';config.write_text(json.dumps(cfg))
 with sqlite3.connect(data/'state.db') as conn:
  conn.execute('create table sessions(id text primary key,user text,name text,agent text,account_id text,container_id text,status text,chat_session text,created_at text,updated_at text)')
  conn.execute("insert into sessions values('s1','boxadmin','fixture','codex','fixture','old-container','stopped','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')")
 args=SimpleNamespace(source_config=str(config),config=str(base/'etc/config.json'),data=str(base/'state'),cache=str(base/'cache'),binary=str(test_dir/'agentbox'),backup=str(base/'backups/full.tar.gz'),apply=True,reflink=os.environ.get('AGENTBOX_TEST_REFLINK')=='1',snapshot_backup=os.environ.get('AGENTBOX_TEST_SNAPSHOT')=='1')
 actual_run=m.run;process=None;app=base/'app';units=base/'units';units.mkdir()
 def run(*argv,capture=False):
  nonlocal process
  if argv[0]=='systemctl':
   if argv[1]=='stop':
    if process is not None:process.terminate();process.wait(timeout=15);process=None
   elif argv[1]=='start':
    process=subprocess.Popen([str(app/'current/agentbox'),'--config',args.config],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
   elif argv[1]=='is-active' and (process is None or process.poll() is not None):raise RuntimeError('service not active')
   return ''
  return actual_run(*argv,capture=capture)
 try:
  # The runner stores the synthetic source in its own writable layer. No daemon
  # container mounts that directory; the Go full backup independently checks this.
  with patch.object(m,'mounted_containers',return_value=[]),patch.object(m,'run',side_effect=run):
   m.migrate(args)
   target=Path(args.data);new=target/'users/boxadmin/sessions/s1/workspace/fixture.txt'
   stat=new.stat();assert (stat.st_uid,stat.st_gid,stat.st_mode&0o777,int(stat.st_mtime))==(1000,1000,0o640,1600000000),stat
   assert new.read_text()=='retained' and file.exists()
   assert (new.parent/'link').is_symlink() and os.readlink(new.parent/'link')=='/shared/fixture.txt'
   migrated=m.read(args.config)
   assert Path(migrated['accounts'][0]['credentials_dir'],'auth.json').read_text()=='{"synthetic":true}'
   assert (target/'users/boxadmin/sessions/s1/chats/t1.jsonl').exists()
   with sqlite3.connect(target/'state.db') as conn:assert conn.execute('select container_id,status from sessions').fetchone()==('','stopped')
   package=base/'package';package.mkdir();shutil.copy2(test_dir/'agentbox',package/'agentbox')
   for version in ['v0.0.1','v0.0.2']:
    (package/'build.json').write_text(json.dumps({'version':version,'program':'agentbox','os':'linux'}));m.stage(package,app)
   (units/'agentbox.service').write_text(m.unit(app,Path(args.config)))
   for version in ['v0.0.1','v0.0.2','v0.0.1']:
    m.activate(app,version,Path(args.config),base/'backups',units)
    assert (app/'current').resolve().name==version
   with urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:18473/api/login',data=json.dumps({'username':'boxadmin','password':cfg['auth_token']}).encode(),headers={'Content-Type':'application/json'})) as res:
    token=json.load(res)['token']
   with urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:18473/api/sessions',headers={'Authorization':'Bearer '+token})) as res:assert json.load(res)[0]['id']=='s1'
   assert len(list((base/'backups').glob('before-*'))) == 2
   run('systemctl','stop')
   # Future schema rollback must fail before activation, retaining current link.
   with sqlite3.connect(target/'state.db') as conn:conn.execute('pragma user_version=99')
   try:m.activate(app,'v0.0.2',Path(args.config),base/'backups',units)
   except ValueError:pass
   else:raise AssertionError('future schema accepted')
   assert (app/'current').resolve().name=='v0.0.1'
   print('Linux deployment: offline migration/UID/GID/mode/mtime/links/history/creds, install, upgrade, compatible rollback and future-schema refusal passed; systemctl simulated')
 finally:
  if process is not None:process.terminate();process.wait(timeout=15)
  shutil.rmtree(base)


def main():
 p=argparse.ArgumentParser(description=__doc__);p.add_argument('--inside',action='store_true');p.add_argument('--reflink',action='store_true');p.add_argument('--binary');p.add_argument('--image',default='agentbox-agent:claude-2.1.280-codex-0.145.0');a=p.parse_args()
 if a.inside:inside();return
 if not a.binary:p.error('--binary required')
 root=Path(__file__).resolve().parent.parent
 cid=subprocess.check_output(['docker','run','-d','-e','AGENTBOX_TEST_REFLINK='+('1' if a.reflink else '0'),'--user','0:0','--network','none','--mount','type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock',a.image,'sleep','infinity'],text=True).strip()
 try:
  for src,dst in [(Path(a.binary),'/tmp/agentbox'),(root/'deploy/release.py','/tmp/release.py'),(Path(__file__),'/tmp/test.py')]:subprocess.run(['docker','cp',str(src),cid+':'+dst],check=True)
  subprocess.run(['docker','exec',cid,'python3','/tmp/test.py','--inside'],check=True)
 finally:subprocess.run(['docker','rm','-f',cid],check=True,stdout=subprocess.DEVNULL)

if __name__=='__main__':main()
