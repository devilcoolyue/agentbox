#!/usr/bin/env python3
"""Real systemd + migration + system/full recovery on disposable Linux data.

Requires a prebuilt recovery-drill image. Uses a privileged PRIVATE cgroup/PID
namespace and the Docker API socket for the product's real mount checks. No host
application data or host systemd/cgroup directories are mounted. Never publishes.
"""
import argparse
import contextlib
import datetime
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import random
import re
import shutil
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parent.parent
BASELINE = 'eb845db59e45ed042b1af9df44dae96f104bf43b'


def run(*args, **kw):
    return subprocess.check_output([str(x) for x in args], text=True, timeout=kw.pop('timeout', 120), **kw).strip()


def inside(args):
    if not Path('/.dockerenv').exists() or os.environ.get('AGENTBOX_RECOVERY_RUN')!=args.run_id or Path('/proc/1/comm').read_text().strip()!='systemd':
        raise RuntimeError('inside mode requires the owned Docker/systemd fixture')
    base = Path('/srv/agentbox-recovery-drill')
    if base.exists(): raise RuntimeError('drill requires a new container and directory')
    base.mkdir(mode=0o700)
    (base/'.synthetic-only').write_text(args.run_id)
    if Path('/proc/1/comm').read_text().strip() != 'systemd': raise RuntimeError('PID 1 is not real systemd')
    if Path('/usr/bin/systemctl').read_bytes()[:4] != b'\x7fELF': raise RuntimeError('systemctl is not a native binary')
    report = {'version': 1, 'run_id': args.run_id, 'status': 'running',
              'platform': platform.platform(), 'systemd': run('/usr/bin/systemctl','--version').splitlines()[0],
              'sessions': args.sessions, 'files_per_session': args.files_per_session, 'file_bytes': args.file_bytes,
              'backup_interval_seconds': args.backup_interval_seconds,
              'scope': 'synthetic data; real Linux/systemd/SQLite/Docker mount checks; no provider or production service',
              'steps': [], 'checks': []}
    target_report = Path('/report.json')
    def save(): target_report.write_text(json.dumps(report, ensure_ascii=False, indent=2)+'\n')
    def measured(name, work):
        started=time.monotonic()
        try: value=work()
        except Exception:
            report['steps'].append({'name': name, 'seconds': round(time.monotonic()-started,6), 'status': 'failed'});save();raise
        report['steps'].append({'name': name, 'seconds': round(time.monotonic()-started,6), 'status': 'passed'});save();return value
    spec=importlib.util.spec_from_file_location('release','/drill/release.py')
    release=importlib.util.module_from_spec(spec);spec.loader.exec_module(release)
    source=base/'source';source.mkdir();data=source/'data';data.mkdir()
    external=base/'external-credentials';external.mkdir();(external/'auth.json').write_text('{"synthetic":true}')
    config=source/'config.json'
    config.write_text(json.dumps({'auth_token':'synthetic-recovery-'+args.run_id,'listen':'127.0.0.1:8180',
        'data_dir':str(data),'timezone':'UTC','idle_timeout_min':0,'agent_image':'alpine:3.22',
        'container':{'network':'none'},'accounts':[{'id':'fixture','type':'codex','credentials_dir':str(external),
        'env':{'OPENAI_BASE_URL':'http://127.0.0.1:1','OPENAI_API_KEY':'synthetic-only'}}]}))
    config.chmod(0o600)
    app=base/'app';units=Path('/etc/systemd/system');unit=units/'agentbox.service'
    old_version='v0.0.1-drill';new_version='v0.0.2-drill'
    for version, executable in [(old_version,'old-agentbox'),(new_version,'agentbox')]:
        package=base/version;package.mkdir();shutil.copy2('/drill/'+executable,package/'agentbox')
        (package/'build.json').write_text(json.dumps({'program':'agentbox','os':'linux','version':version}))
        release.stage(package,app)
    unit.write_text(release.unit(app,config));run('systemctl','daemon-reload')
    def activate(version,cfg=config): return release.activate(app,version,cfg,base/'deployment-backups',units)
    def connect(path):return sqlite3.connect(path.as_uri()+'?mode=ro',uri=True)
    def schema(path):
        with contextlib.closing(connect(path)) as db:return db.execute('pragma user_version').fetchone()[0]
    def audit(path):
        with contextlib.closing(connect(path)) as db:
            assert db.execute('pragma integrity_check').fetchone()[0]=='ok'
            names=[row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
            tables={}
            for name in names:
                assert name.replace('_','').isalnum()
                rows=sorted(json.dumps(row,ensure_ascii=False,default=lambda b:b.hex()) for row in db.execute('SELECT * FROM "'+name+'"'))
                tables[name]={'rows':len(rows),'sha256':hashlib.sha256('\n'.join(rows).encode()).hexdigest()}
            balance=db.execute('SELECT balance_micro_usd FROM quotas WHERE user=?',('alice',)).fetchone()[0]
            ledger=db.execute('SELECT SUM(delta_micro_usd) FROM credit_ledger WHERE user=?',('alice',)).fetchone()[0]
            assert balance==ledger
            return {'tables':tables,'balance_micro_usd':balance}
    def inventory(root):
        result={}
        for path in sorted(root.rglob('*')):
            relative=path.relative_to(root).as_posix()
            if relative in ('state.db','state.db-wal','state.db-shm','agentbox.lock','client-instance-id') or relative.startswith('backups/'):continue
            if path.is_symlink():result[relative]={'link':os.readlink(path)}
            elif path.is_file():
                st=path.stat();result[relative]={'sha256':hashlib.sha256(path.read_bytes()).hexdigest(),'bytes':st.st_size,'mode':st.st_mode&0o777,'uid':st.st_uid,'gid':st.st_gid}
        return result
    def cli(*argv):return json.loads(run('/drill/agentbox',*argv))
    try:
        measured('old_service_start',lambda:activate(old_version))
        assert schema(data/'state.db')==9
        run('systemctl','stop','agentbox.service')
        # Seed only the marked disposable, stopped schema-9 database. Usage and
        # ledger rows commit together; there are no real credentials or prompts.
        stamp='2026-01-01T00:00:00Z';ids=['drill-'+args.run_id[:8]+'-'+str(i) for i in range(args.sessions)]
        with sqlite3.connect(data/'state.db') as db:
            password=db.execute("SELECT pass_hash FROM users WHERE name='boxadmin'").fetchone()[0]
            db.execute('INSERT INTO users(name,role,pass_hash,created_at) VALUES(?,?,?,?)',('alice','user',password,stamp))
            grant=1_000_000;count=args.sessions*10;spent=count*15
            db.execute('INSERT INTO quotas(user,balance_micro_usd,granted_micro_usd,spent_micro_usd,created_at,updated_at) VALUES(?,?,?,?,?,?)',('alice',grant-spent,grant,spent,stamp,stamp))
            db.execute('INSERT INTO credit_ledger(ts,user,ref,reason,delta_micro_usd,balance_after) VALUES(?,?,?,?,?,?)',(stamp,'alice','grant:synthetic','grant',grant,grant))
            for sid in ids:
                db.execute('INSERT INTO sessions(id,user,name,agent,account_id,status,default_model,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)',(sid,'alice','Synthetic workspace','codex','fixture','stopped','gpt-5.5',stamp,stamp))
            for i in range(count):
                row=db.execute('INSERT INTO usage_events(ts,user,session_id,thread_id,turn_id,agent,model,kind,cost_micro_usd) VALUES(?,?,?,?,?,?,?,?,?)',(stamp,'alice',ids[i%len(ids)],'t0','turn-'+str(i),'codex','gpt-5.5','chat',15)).lastrowid
                db.execute('INSERT INTO credit_ledger(ts,user,ref,reason,delta_micro_usd,balance_after) VALUES(?,?,?,?,?,?)',(stamp,'alice','usage:'+str(row),'usage',-15,grant-(i+1)*15))
        rng=random.Random(1234)
        for sid in ids:
            directory=data/'users/alice/sessions'/sid
            for part in ['workspace','home','chats']:(directory/part).mkdir(parents=True,exist_ok=True)
            for i in range(args.files_per_session):
                path=directory/'workspace'/('fixture-'+str(i)+'.bin');path.write_bytes(rng.randbytes(args.file_bytes));path.chmod(0o640);os.chown(path,1000,1000)
            (directory/'chats/t0.jsonl').write_text('{"kind":"user","text":"synthetic recovery fixture"}\n')
            (directory/'chats/active').write_text('t0')
            (directory/'home/.fixture').write_text('synthetic home')
        for relative in ['home-template/.bashrc','users/alice/home-template/.bashrc','users/alice/shared/.file/attachment.bin']:
            path=data/relative;path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(b'synthetic-only')
        report['source_files']=len(inventory(data));report['source_file_bytes']=sum(v.get('bytes',0) for v in inventory(data).values())
        measured('upgrade_9_to_current',lambda:activate(new_version))
        new_schema=schema(data/'state.db');assert new_schema==args.expected_schema;report['schema']={'old':9,'new':new_schema}
        run('systemctl','stop','agentbox.service')
        # A persisted accepted request must become uncertain on startup, never
        # execute or charge during the recovery drill.
        frozen=json.dumps({'scope':'a'*64,'thread_id':'t0','text':'synthetic pending request','model':'gpt-5.5','effort':'','effort_control':''},separators=(',',':'))
        with sqlite3.connect(data/'state.db') as db:
            db.execute('INSERT INTO chat_requests(user,user_created_at,session_id,request_id,thread_id,turn_id,fingerprint,request_json,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)',('alice',stamp,ids[0],args.run_id,'t0','turn-'+args.run_id,hashlib.sha256(frozen.encode()).hexdigest(),frozen,'accepted',stamp,stamp))
        measured('pending_recovery_start',lambda:run('systemctl','start','agentbox.service'))
        for _ in range(100):
            with contextlib.closing(connect(data/'state.db')) as db:
                pending=db.execute('SELECT state FROM chat_requests').fetchone()[0]
            if pending=='uncertain':break
            time.sleep(.05)
        assert pending=='uncertain'
        report['checks'].append('startup_pending_request_uncertain_without_execution')
        first_pid=run('systemctl','show','agentbox','-p','MainPID','--value')
        measured('systemd_restart',lambda:run('systemctl','restart','agentbox.service'))
        assert run('systemctl','is-active','agentbox')=='active'
        assert run('systemctl','show','agentbox','-p','MainPID','--value')!=first_pid
        baseline=audit(data/'state.db');files=inventory(data)
        report['data_audit']=baseline
        system_archive=base/'system.tar.gz';full_archive=base/'full.tar.gz'
        measured('system_backup_online',lambda:cli('backup','--config',config,'--output',system_archive))
        denied=subprocess.run(['/drill/agentbox','backup','--config',str(config),'--full','--output',str(base/'must-not-exist.tar.gz')],capture_output=True)
        assert denied.returncode!=0 and not (base/'must-not-exist.tar.gz').exists()
        report['checks'].append('full_backup_rejects_live_service_lock')
        measured('stop_for_full_backup',lambda:run('systemctl','stop','agentbox.service'))
        measured('full_backup_offline',lambda:cli('backup','--config',config,'--full','--output',full_archive))
        # Record controlled post-snapshot changes to make the loss window
        # visible: restoring either archive must exclude these later writes.
        after_stamp=datetime.datetime.now(datetime.timezone.utc).isoformat()
        with sqlite3.connect(data/'state.db') as db:
            usage_id=db.execute('INSERT INTO usage_events(ts,user,session_id,turn_id,agent,kind,cost_micro_usd) VALUES(?,?,?,?,?,?,?)',(after_stamp,'alice',ids[0],'post-backup-marker','codex','chat',15)).lastrowid
            db.execute('UPDATE quotas SET balance_micro_usd=balance_micro_usd-15,spent_micro_usd=spent_micro_usd+15 WHERE user=?',('alice',))
            db.execute('INSERT INTO credit_ledger(ts,user,ref,reason,delta_micro_usd,balance_after) VALUES(?,?,?,?,?,?)',(after_stamp,'alice','usage:'+str(usage_id),'usage',-15,baseline['balance_micro_usd']-15))
        marker=data/'users/alice/sessions'/ids[0]/'workspace/after-backup.txt';marker.write_text('synthetic post-backup change')
        report['post_backup_changes']={'at':after_stamp,'usage_rows':1,'ledger_rows':1,'project_files':1,'restored':False}
        before=app.joinpath('current').resolve()
        try:activate(old_version)
        except (ValueError,subprocess.CalledProcessError):pass
        else:raise AssertionError('old schema binary accepted incompatible activation')
        assert app.joinpath('current').resolve()==before
        report['checks'].append('old_binary_rollback_rejected_before_switch')
        report['backups']={}
        for mode,archive in [('system',system_archive),('full',full_archive)]:
            assert measured(mode+'_verify',lambda:cli('backup-verify',archive))['valid']
            destination=base/(mode+'-restored')
            measured(mode+'_restore',lambda:cli('restore','--to',destination,archive))
            assert audit(destination/'data/state.db')==baseline
            assert not (destination/'data'/marker.relative_to(data)).exists()
            denied_restore=subprocess.run(['/drill/agentbox','restore','--to',str(destination),str(archive)],capture_output=True)
            assert denied_restore.returncode!=0 and audit(destination/'data/state.db')==baseline
            restored=inventory(destination/'data')
            if mode=='full':assert restored==files
            else:
                assert all('/workspace/' not in path and '/chats/' not in path and '/shared/' not in path for path in restored)
                assert 'home-template/.bashrc' in restored and 'users/alice/home-template/.bashrc' in restored
            cfg=json.loads((destination/'config.json').read_text())
            credential=(destination/cfg['accounts'][0]['credentials_dir']).resolve()
            assert credential.is_relative_to(destination) and (credential/'auth.json').read_bytes()==(external/'auth.json').read_bytes()
            assert not (destination/'data/client-instance-id').exists()
            report['backups'][mode]={'archive_bytes':archive.stat().st_size,'restored_files':len(restored),'project_files_included':mode=='full','database_matches':True}
            # Actually start the restored copy via systemd and exercise health.
            # Never run the source and restored copies concurrently.
            unit.write_text(release.unit(app,destination/'config.json'));run('systemctl','daemon-reload')
            measured(mode+'_restored_start',lambda:activate(new_version,destination/'config.json'))
            assert run('systemctl','is-active','agentbox')=='active'
            assert audit(destination/'data/state.db')==baseline
            run('systemctl','stop','agentbox.service')
            report['checks'].append(mode+'_restored_service_healthy')
        timings={row['name']:row['seconds'] for row in report['steps']}
        report['recovery_observation']={mode:{'verify_restore_start_seconds':round(sum(timings[mode+suffix] for suffix in ['_verify','_restore','_restored_start']),6),
            'complete_project_recovery':mode=='full'} for mode in ['system','full']}
        report['rpo']={'schedule_assumption_seconds':args.backup_interval_seconds,'successful_backup_window_upper_bound_seconds':args.backup_interval_seconds+timings['full_backup_offline'],
            'system_project_rpo':'not_covered','warning':'Assumes scheduled backups succeed; not an agreed SLA or production measurement.'}
        report['checks']+=['database_integrity_and_ledger_balance','files_hash_mode_and_uid','system_backup_excludes_projects','restored_credentials_rebased','restore_existing_directory_rejected','post_backup_writes_absent_as_expected']
        report['status']='passed'
    except Exception:
        report['status']='failed';raise
    finally:
        report['journal']=run('journalctl','-u','agentbox.service','--no-pager','-n','15')
        save()


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image',required=True)
    parser.add_argument('--report-dir',type=Path)
    parser.add_argument('--sessions',type=int,default=32)
    parser.add_argument('--files-per-session',type=int,default=16)
    parser.add_argument('--file-bytes',type=int,default=4096)
    parser.add_argument('--backup-interval-seconds',type=int,default=86400)
    parser.add_argument('--inside',action='store_true',help=argparse.SUPPRESS)
    parser.add_argument('--run-id',default='',help=argparse.SUPPRESS)
    parser.add_argument('--expected-schema',type=int,default=12,help=argparse.SUPPRESS)
    args=parser.parse_args()
    if not (1<=args.sessions<=100 and 1<=args.files_per_session<=100 and 1<=args.file_bytes<=1<<20 and args.sessions*args.files_per_session*args.file_bytes<=128<<20 and args.backup_interval_seconds>0):parser.error('fixture scale must be bounded to 128 MiB')
    if args.inside:
        if len(args.run_id)!=32:parser.error('requires fixture run ID')
        inside(args);return
    from verify import source_identity, clean_env
    run_id=uuid.uuid4().hex;name='abox-recovery-'+run_id[:12]
    destination=args.report_dir or ROOT/'output/verification'/('recovery-drill-'+run_id[:12]);destination.mkdir(parents=True,exist_ok=False)
    evidence={'version':1,'status':'running','source':source_identity(),'run_id':run_id,'container':name,'baseline':BASELINE}
    def save(): (destination/'report.json').write_text(json.dumps(evidence,ensure_ascii=False,indent=2)+'\n')
    save();created=False
    try:
        image_id=run('docker','image','inspect',args.image,'--format','{{.Id}}');evidence['image_id']=image_id
        arch=run('docker','info','--format','{{.Architecture}}');goarch={'aarch64':'arm64','arm64':'arm64','x86_64':'amd64','amd64':'amd64'}[arch]
        evidence['linux_arch']=goarch
        with tempfile.TemporaryDirectory(prefix='agentbox-recovery-') as tmp:
            tmp=Path(tmp);frozen=tmp/'frozen';frozen.mkdir()
            archive=tmp/'source.tar';run('git','archive','--format=tar','--output='+str(archive),BASELINE,cwd=ROOT)
            with tarfile.open(archive) as tar:tar.extractall(frozen,filter='data')
            env=dict(clean_env(),GOOS='linux',GOARCH=goarch,CGO_ENABLED='0')
            for source,file in [(frozen,'old-agentbox'),(ROOT,'agentbox')]:
                with (destination/(file+'-build.log')).open('x') as log:
                    subprocess.run(['go','build','-trimpath','-o',str(tmp/file),'./cmd/agentbox'],cwd=source,env=env,stdout=log,stderr=subprocess.STDOUT,check=True,timeout=300)
                evidence[file+'_sha256']=hashlib.sha256((tmp/file).read_bytes()).hexdigest()
            # All flags refer only to a newly named container; no host app data,
            # host PID/cgroup namespace, systemctl, or global cleanup operation.
            run('docker','create','--name',name,'--network','none','--privileged','--cgroupns','private','--memory','768m','--pids-limit','256',
                '--tmpfs','/run','--tmpfs','/run/lock','--tmpfs','/tmp','--mount','type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock','--env','DOCKER_HOST=unix:///var/run/docker.sock','--env','DOCKER_TLS_VERIFY=','--env','DOCKER_CERT_PATH=','--env','AGENTBOX_RECOVERY_RUN='+run_id,image_id)
            created=True;run('docker','start',name)
            run('docker','exec',name,'mkdir','/drill')
            for source,target in [(tmp/'agentbox','agentbox'),(tmp/'old-agentbox','old-agentbox'),(ROOT/'deploy/release.py','release.py'),(Path(__file__),'drill.py')]:
                run('docker','cp',source,name+':/drill/'+target)
            command=['docker','exec',name,'python3','/drill/drill.py','--inside','--image',image_id,'--run-id',run_id,'--sessions',str(args.sessions),'--files-per-session',str(args.files_per_session),'--file-bytes',str(args.file_bytes),'--backup-interval-seconds',str(args.backup_interval_seconds),'--expected-schema',re.search(r'^const SchemaVersion = ([0-9]+)$',(ROOT/'internal/store/migrations.go').read_text(),re.M)[1]]
            with (destination/'drill.log').open('x') as log:
                result=subprocess.run(command,stdout=log,stderr=subprocess.STDOUT,timeout=360)
            run('docker','cp',name+':/report.json',destination/'scenario.json')
            if result.returncode:raise RuntimeError('recovery drill failed; see drill.log and scenario.json')
            scenario=json.loads((destination/'scenario.json').read_text());assert scenario['status']=='passed'
            evidence['status']='passed'
    except BaseException:
        evidence['status']='failed';raise
    finally:
        if created:
            cleanup=subprocess.run(['docker','rm','-f',name],capture_output=True,text=True,timeout=45)
            evidence['cleanup']='passed' if cleanup.returncode==0 else 'failed'
            if cleanup.returncode:evidence['status']='failed'
        evidence['source_after']=source_identity();evidence['source_changed']=evidence['source']!=evidence['source_after'];save()
        print('Recovery evidence:',destination,flush=True)
        if evidence.get('cleanup')=='failed':raise RuntimeError('owned recovery container cleanup failed')

if __name__=='__main__':main()
