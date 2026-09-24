#!/usr/bin/env python3
"""Run a packaged server + synthetic workspace against a local Linux Docker daemon.

Requires an already-built agent image. Mounts the Docker socket into the trusted
server test container and uses a disposable named volume, never real data or
credentials. Session containers have network=none; no model requests are made.
"""
import argparse
import json
from pathlib import Path
import secrets
import socket
import base64
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('artifacts', type=Path, nargs='?')
p.add_argument('--binary', type=Path, help='test an already cross-compiled Linux binary')
p.add_argument('--usage', action='store_true', help='verify Codex terminal backfill and persisted pricing with synthetic rollout')
p.add_argument('--restart', action='store_true', help='verify shutdown with an open chat WebSocket and restart against the same data')
p.add_argument('--image', required=True)
a=p.parse_args()
def docker(*args):
    return subprocess.check_output(['docker',*args],text=True).strip()
arch={'aarch64':'arm64','arm64':'arm64','x86_64':'amd64'}[docker('info','--format','{{.Architecture}}')]
if not a.artifacts and not a.binary:p.error('provide artifacts or --binary')
archive=next(a.artifacts.glob('agentbox_*_linux_'+arch+'.tar.gz')) if a.artifacts else None
volume='agentbox-release-test-'+uuid.uuid4().hex
server=None
sessions=[]
password=secrets.token_urlsafe(32)
try:
    docker('volume','create',volume)
    data=docker('volume','inspect','--format','{{.Mountpoint}}',volume)
    with tempfile.TemporaryDirectory(prefix='agentbox-server-smoke-') as tmp:
        tmp=Path(tmp)
        if a.binary:
            binary=a.binary.resolve()
        else:
            with tarfile.open(archive) as tar:tar.extractall(tmp,filter='data')
            binary=next(tmp.glob('*/agentbox'))
        config=tmp/'fixture.json'
        config.write_text(json.dumps({'listen':'0.0.0.0:8180','auth_token':password,
            'data_dir':data,'timezone':'UTC','agent_image':a.image,
            'container':{'network':'none','memory_mb':512,'cpus':1,'pids_limit':128},
            'accounts':[{'id':'synthetic','type':'codex','label':'Synthetic test account'}]}))
        server=docker('create','--mount','type=volume,src='+volume+',dst='+data,
            '--mount','type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock',
            '-p','127.0.0.1::8180','--entrypoint','/opt/agentbox','alpine:3',
            '--config','/tmp/config.json')
        docker('cp',str(binary),server+':/opt/agentbox')
        docker('cp',str(config),server+':/tmp/config.json')
        docker('start',server)
        bindings=json.loads(docker('inspect',server))[0]['NetworkSettings']['Ports']['8180/tcp']
        base='http://127.0.0.1:'+bindings[0]['HostPort']
        token=''
        def request(path,body=None,method=None):
            headers={'Authorization':'Bearer '+token}
            raw=json.dumps(body).encode() if body is not None else None
            req=urllib.request.Request(base+path,data=raw,headers=headers,method=method)
            with urllib.request.urlopen(req,timeout=65) as res:
                content=res.read()
                return json.loads(content) if res.headers.get_content_type()=='application/json' else content
        deadline=time.monotonic()+30
        while True:
            try:request('/api/ping');break
            except (urllib.error.URLError,ConnectionError):
                if time.monotonic()>deadline:raise
                time.sleep(.2)
        token=request('/api/login',{'username':'boxadmin','password':password})['token']
        assert request('/api/me')['role']=='admin'
        assert b'AGENTBOX' in request('/')
        sess=request('/api/sessions',{'name':'packaged smoke','agent':'codex','account_id':'synthetic'})
        # Register cleanup by deterministic container name before start can fail.
        sessions.append('agentbox-'+sess['id'])
        started=request('/api/sessions/'+sess['id']+'/start',{},'POST')
        cid=started['container_id'];sessions.append(cid)
        inspect=json.loads(docker('inspect',cid))[0]
        assert inspect['HostConfig']['NetworkMode']=='none'
        mounts={m['Destination']:m['Source'] for m in inspect['Mounts']}
        assert set(mounts)=={'/workspace','/home/agent','/shared'}
        assert all(v.startswith(data+'/') for v in mounts.values())
        assert docker('exec','--user','1000:1000',cid,'id','-u')=='1000'
        docker('exec','--user','1000:1000',cid,'git','-C','/workspace','init','-q')
        # Real HTTP file write and real container Git preparation, no provider calls.
        request('/api/sessions/'+sess['id']+'/file?path=smoke.txt',{'content':'packaged server works\n'},'PUT')
        status=request('/api/sessions/'+sess['id']+'/git/status')
        assert 'smoke.txt' in json.dumps(status),status
        assert 'packaged server works' in docker('exec','--user','1000:1000',cid,'cat','/workspace/smoke.txt')
        request('/api/settings',{'resources':{'max_running':1,'max_running_per_user':1,'min_free_bytes':0}},'PUT')
        extra=request('/api/sessions',{'name':'capacity smoke','agent':'codex','account_id':'synthetic'})
        sessions.append('agentbox-'+extra['id'])
        try:
            request('/api/sessions/'+extra['id']+'/start',{},'POST')
            raise AssertionError('container cap did not reject startup')
        except urllib.error.HTTPError as err:
            assert err.code==429,err
        request('/api/sessions/'+extra['id']+'?purge=1',method='DELETE')
        diagnostics=request('/api/diagnostics')
        compatibility=json.loads(docker('exec',server,'/opt/agentbox','check-config','--config','/tmp/config.json'))
        assert diagnostics['schema_version']==compatibility['schema_version'] and 'config_path' not in diagnostics
        assert password not in json.dumps(diagnostics) and 'synthetic' not in json.dumps(diagnostics)
        assert 'disk_available' in request('/api/storage')
        if a.usage:
            request('/api/settings',{'pricing':{'codex':{'input':2,'output':3}}},'PUT')
            transcript_dir='/home/agent/.codex/sessions/2026/09/23'
            docker('exec','--user','1000:1000',cid,'mkdir','-p',transcript_dir)
            fixture=Path(__file__).resolve().parent.parent/'internal/usage/testdata/codex-terminal-0.145.0.jsonl'
            docker('cp',str(fixture),cid+':'+transcript_dir+'/rollout-fixture.jsonl')
            deadline=time.monotonic()+20
            while True:
                events=request('/api/usage/events?session='+sess['id'])
                rows=events['rows']
                if len(rows)==3: break
                if time.monotonic()>deadline: raise AssertionError(events)
                time.sleep(.3)
            assert len(rows)==3,events
            costs={r['id']:r['cost_micro_usd'] for r in rows}
            assert all(r['rate']['snapshot'] and r['rate']['input']==2 for r in rows),rows
            request('/api/settings',{'pricing':{'codex':{'input':99,'output':99}}},'PUT')
            rows=request('/api/usage/events?session='+sess['id'])['rows']
            assert {r['id']:r['cost_micro_usd'] for r in rows}==costs
            assert all(r['rate']['input']==2 for r in rows)
            print('Codex terminal: synthetic rollout yielded 3 rows; saved prices survive table edits')
        if a.restart:
            docker('exec','--user','1000:1000',cid,'tmux','new-session','-d','-s','lifecycle-smoke','sleep 120')
            ws=socket.create_connection(('127.0.0.1',int(bindings[0]['HostPort'])),timeout=3)
            key=base64.b64encode(secrets.token_bytes(16)).decode()
            ws.sendall((f"GET /api/sessions/{sess['id']}/chat HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer {token}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
            header=b''
            while b'\r\n\r\n' not in header:header+=ws.recv(4096)
            assert header.startswith(b'HTTP/1.1 101'),header[:100]
            docker('stop','--time','12',server)
            state=json.loads(docker('inspect',server))[0]['State']
            assert state['ExitCode']==0, state
            while ws.recv(4096):pass
            ws.close()
            assert json.loads(docker('inspect',cid))[0]['State']['Running']
            docker('start',server)
            bindings=json.loads(docker('inspect',server))[0]['NetworkSettings']['Ports']['8180/tcp']
            base='http://127.0.0.1:'+bindings[0]['HostPort']
            deadline=time.monotonic()+15
            while True:
                try:request('/api/ping');break
                except (urllib.error.URLError,ConnectionError):
                    if time.monotonic()>deadline:raise
                    time.sleep(.2)
            assert request('/api/sessions/'+sess['id'])['status']=='running'
            docker('exec','--user','1000:1000',cid,'tmux','has-session','-t','lifecycle-smoke')
            print('SIGTERM: open WebSocket closed, exit=0, lock reacquired on restart, session/tmux preserved')
        request('/api/sessions/'+sess['id']+'/stop',{},'POST')
        assert json.loads(docker('inspect',cid))[0]['State']['Running'] is False
        request('/api/sessions/'+sess['id']+'?purge=1',method='DELETE')
        print('Packaged server: login, static UI, create/start, UID/mounts, file/Git, stop/delete passed')
finally:
    for cid in set(sessions):
        subprocess.run(['docker','rm','-f',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    if server:
        subprocess.run(['docker','rm','-f',server],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    subprocess.run(['docker','volume','rm',volume],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
