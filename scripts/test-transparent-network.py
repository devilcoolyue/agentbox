#!/usr/bin/env python3
"""Real Linux Docker TCP/DNS acceptance test, using disposable synthetic users.
No provider requests or real credentials. Build Linux agentbox/abox-link first.
"""
import argparse,json,secrets,subprocess,tempfile,time,urllib.request,urllib.error,uuid,hmac,hashlib
from pathlib import Path
p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--server',type=Path,required=True)
p.add_argument('--client',type=Path,required=True)
p.add_argument('--fixture',type=Path,required=True)
p.add_argument('--image',default='agentbox-agent:latest')
p.add_argument('--network-image',default='agentbox-network:dev')
p.add_argument('--custom-network',action='store_true')
a=p.parse_args()
test_network='bridge'
created=[];sessions=[];volume='agentbox-network-test-'+uuid.uuid4().hex

def docker(*args):return subprocess.check_output(['docker',*args],text=True,stderr=subprocess.STDOUT).strip()
def new(*args):
    cid=docker('create','--network',test_network,*args);created.append(cid);return cid

def wait(fn,seconds=30):
    end=time.monotonic()+seconds
    while True:
        try:return fn()
        except Exception:
            if time.monotonic()>end:raise
            time.sleep(.3)

try:
    if a.custom_network:
        test_network=volume+'-net';docker('network','create',test_network)
    docker('volume','create',volume)
    data=docker('volume','inspect','--format','{{.Mountpoint}}',volume)
    proxy=new('alpine:3','/opt/fixture','-listen',':1088','-proxy','-marker','account-exit')
    docker('cp',str(a.fixture.resolve()),proxy+':/opt/fixture');docker('start',proxy)
    proxy_ip=json.loads(docker('inspect',proxy))[0]['NetworkSettings']['Networks'][test_network]['IPAddress']
    server=new('--mount','type=volume,src='+volume+',dst='+data,'-v','/var/run/docker.sock:/var/run/docker.sock','-p','127.0.0.1::8180','alpine:3','sleep','infinity')
    docker('cp',str(a.server.resolve()),server+':/opt/agentbox');docker('start',server)
    info=json.loads(docker('inspect',server))[0]
    server_ip=info['NetworkSettings']['Networks'][test_network]['IPAddress']
    base='http://127.0.0.1:'+info['NetworkSettings']['Ports']['8180/tcp'][0]['HostPort']
    password=secrets.token_urlsafe(32);token=''
    with tempfile.TemporaryDirectory(prefix='agentbox-net-') as temp:
        cfg=Path(temp)/'config.json';cfg.write_text(json.dumps({
          'listen':'0.0.0.0:8180','timezone':'UTC','auth_token':password,'data_dir':data,'agent_image':a.image,
          'container':{'network':test_network,'memory_mb':512,'cpus':1,'pids_limit':128},
          'tunnel':{'enabled':True,'transparent':True,'proxy_bind':'0.0.0.0:1080','proxy_host':server_ip,'network_bind':'0.0.0.0:1082','network_image':a.network_image},
          'proxy_bridge':{'bind':'0.0.0.0:1081','host':server_ip},
          'proxies':[{'id':'fixture-exit','name':'Synthetic exit','scheme':'http','host':proxy_ip,'port':1088}],
          'accounts':[{'id':'fixture','type':'codex','label':'Synthetic','proxy_id':'fixture-exit'}]}))
        docker('cp',str(cfg),server+':/tmp/config.json')
        docker('exec','-d',server,'sh','-c','echo $$ >/tmp/server.pid; exec /opt/agentbox -config /tmp/config.json >>/tmp/server.log 2>&1')
    def request(path,body=None,method=None,as_token=None):
        raw=json.dumps(body).encode() if body is not None else None
        req=urllib.request.Request(base+path,data=raw,method=method,headers={'Authorization':'Bearer '+(token if as_token is None else as_token)})
        try:
            with urllib.request.urlopen(req,timeout=65) as res:
                raw=res.read();return json.loads(raw) if raw else None
        except urllib.error.HTTPError as err:
            raise RuntimeError(str(err)+": "+err.read().decode()) from err
    wait(lambda:request('/api/ping'))
    token=request('/api/login',{'username':'boxadmin','password':password})['token']
    user_tokens={}
    for user in ['alice','bob']:
        request('/api/users',{'username':user,'password':password,'role':'user'})
        user_tokens[user]=request('/api/login',{'username':user,'password':password})['token']
    clients={};workspaces={}
    for user in ['alice','bob']:
        client=new('--cap-add','NET_ADMIN','--add-host','db.corp:10.222.0.10','alpine:3','sh','-c',
          'ip addr add 10.222.0.10/32 dev lo; /opt/fixture -listen 10.222.0.10:8080 -marker '+user+' & /opt/fixture -listen 10.222.0.10:8081 -marker forbidden & exec /opt/abox-link --server http://'+server_ip+':8180 --user '+user+' --token '+user_tokens[user]+' --transparent --allow 10.222.0.0/24 --allow db.corp:8080')
        docker('cp',str(a.fixture.resolve()),client+':/opt/fixture');docker('cp',str(a.client.resolve()),client+':/opt/abox-link');docker('start',client);clients[user]=client
        def online():
            st=request('/api/tunnel/status',as_token=user_tokens[user]);assert st['connected'] and st['client_transparent'],st;return st
        wait(online)
        sess=request('/api/sessions',{'name':user+' network','agent':'codex','account_id':'fixture'},as_token=user_tokens[user]);sessions.append('agentbox-'+sess['id'])
        sess=request('/api/sessions/'+sess['id']+'/start',{},as_token=user_tokens[user]);workspaces[user]=sess
        created.append('agentbox-net-'+sess['container_id'][:12])
        docker('exec',sess['container_id'],'rm','-f','/home/agent/.codex/AGENTS.md')
    def curl(user,target,success=True):
        cid=workspaces[user]['container_id']
        cmd=['docker','exec','--user','1000:1000',cid,'curl','--noproxy','*','--connect-timeout','3','--max-time','8','-fsS',target]
        res=subprocess.run(cmd,text=True,capture_output=True)
        if success:assert res.returncode==0,(res.stderr,res.stdout);return res.stdout
        assert res.returncode!=0,'request unexpectedly succeeded';return res.stderr
    for user in ['alice','bob']:
        assert curl(user,'http://10.222.0.10:8080')==user
        assert curl(user,'http://db.corp:8080')==user
        # No dedicated env and no instructions are present in this exec.
        env=docker('exec',workspaces[user]['container_id'],'env')
        assert 'AGENTBOX_INTRANET' not in env
    print('PASS transparent IPv4, client-side domain resolution, same-address user isolation',flush=True)
    curl('alice','http://db.corp:8081',False)
    # Raw sockets/database-like clients must work without HTTP proxy support.
    raw=docker('exec','--user','1000:1000',workspaces['alice']['container_id'],'python3','-c',
      'import socket; s=socket.create_connection(("db.corp",8080),3); s.sendall(b"GET / HTTP/1.0\\r\\n\\r\\n"); print(s.recv(4096).decode())')
    assert '200 OK' in raw,raw
    print('PASS native TCP and restricted domain port',flush=True)
    docker('stop','--time','2',clients['alice']);curl('alice','http://10.222.0.10:8080',False)
    assert curl('bob','http://db.corp:8080')=='bob'
    docker('start',clients['alice']);wait(lambda:curl('alice','http://db.corp:8080'))
    print('PASS disconnect denies access, other user unaffected, reconnect restores access',flush=True)
    helper='agentbox-net-'+workspaces['alice']['container_id'][:12]
    docker('restart',helper);wait(lambda:curl('alice','http://db.corp:8080'))
    print('PASS helper restart restores transparent access',flush=True)
    secret=hmac.new(password.encode(),b'proxy-bridge:fixture',hashlib.sha256).hexdigest()[:32]
    proxy_url='http://fixture:'+secret+'@'+server_ip+':1081'
    def proxied(user,target,success=True,connect=False):
        cmd=['docker','exec','--user','1000:1000',workspaces[user]['container_id'],'curl','--noproxy','','--proxy',proxy_url,'--max-time','8','-fsS']
        if connect:cmd+=['--proxytunnel']
        res=subprocess.run(cmd+[target],text=True,capture_output=True)
        if success:assert res.returncode==0,res.stderr;return res.stdout
        assert res.returncode!=0,'unexpected proxy success'
    for user in ['alice','bob']:
        assert proxied(user,'http://db.corp:8080')==user
        assert proxied(user,'http://db.corp:8080',connect=True)==user
        assert proxied(user,'http://outside.invalid/')=='account-exit'
    docker('stop','--time','1',proxy)
    proxied('alice','http://outside.invalid/',False)
    assert proxied('alice','http://db.corp:8080')=='alice'
    docker('start',proxy)
    print('PASS HTTP/CONNECT account proxy selects owning user; public exit preserved; unavailable exit never falls back',flush=True)
    # Withdraw only the domain grant, retaining the IP grant. The domain's
    # cached fake IP must remain mapped but denied, not turn into a public DNS query.
    docker('stop','--time','1',clients['alice'])
    replacement=new('--cap-add','NET_ADMIN','--add-host','db.corp:10.222.0.10','alpine:3','sh','-c',
      'ip addr add 10.222.0.10/32 dev lo; /opt/fixture -listen 10.222.0.10:8080 -marker alice & exec /opt/abox-link --server http://'+server_ip+':8180 --user alice --token '+user_tokens['alice']+' --transparent --allow 10.222.0.0/24')
    docker('cp',str(a.fixture.resolve()),replacement+':/opt/fixture');docker('cp',str(a.client.resolve()),replacement+':/opt/abox-link');docker('start',replacement)
    clients['alice']=replacement
    wait(lambda:curl('alice','http://10.222.0.10:8080'))
    curl('alice','http://db.corp:8080',False);proxied('alice','http://db.corp:8080',False)
    assert curl('bob','http://db.corp:8080')=='bob'
    print('PASS domain grant withdrawal blocks direct and explicit-proxy requests, including cached virtual addresses',flush=True)
    docker('exec',server,'sh','-c','kill -TERM "$(cat /tmp/server.pid)"')
    def stopped():
        res=subprocess.run(['docker','exec',server,'sh','-c','kill -0 "$(cat /tmp/server.pid)"'],capture_output=True)
        assert res.returncode!=0
    wait(stopped)
    curl('bob','http://db.corp:8080',False)
    docker('exec','-d',server,'sh','-c','echo $$ >/tmp/server.pid; exec /opt/agentbox -config /tmp/config.json >>/tmp/server.log 2>&1')
    wait(lambda:request('/api/ping'))
    wait(lambda:curl('bob','http://db.corp:8080'),60)
    curl('alice','http://db.corp:8080',False)
    assert proxied('alice','http://outside.invalid/')=='account-exit'
    print('PASS server restart restores routes without restarting agent containers and retains revoked grants',flush=True)

    for user,sess in workspaces.items():
        request('/api/sessions/'+sess['id']+'/stop',{},as_token=user_tokens[user])
        request('/api/sessions/'+sess['id']+'?purge=1',method='DELETE',as_token=user_tokens[user])
    print('PASS workspace stop/delete cleanup',flush=True)
except Exception:
    if 'server' in locals():
        print(docker('exec',server,'cat','/tmp/server.log'))
    for cid in created:
        if cid.startswith('agentbox-net-') or cid in clients.values():
            try:print(docker('logs',cid))
            except Exception:pass
    raise
finally:
    # Discover helpers even when startup failed before its response arrived.
    for session in sessions:
        try:
            cid=docker('inspect','--format','{{.Id}}',session);created.append('agentbox-net-'+cid[:12])
        except Exception:pass
    for cid in reversed(created+sessions):subprocess.run(['docker','rm','-f',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    subprocess.run(['docker','volume','rm',volume],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    if test_network!='bridge':subprocess.run(['docker','network','rm',test_network],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
