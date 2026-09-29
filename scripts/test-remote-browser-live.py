#!/usr/bin/env python3
"""Real Docker/browser/RFB smoke using disposable synthetic data, no model calls.
Requires a Linux agentbox binary for the daemon architecture and browser image.
"""
import argparse
import json
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--binary', type=Path, required=True)
parser.add_argument('--image', default='agentbox-agent:browser')
a = parser.parse_args()

def docker(*args):
    return subprocess.check_output(['docker', *args], text=True).strip()

volume = 'agentbox-browser-test-' + uuid.uuid4().hex
server = None
containers = []
password = secrets.token_urlsafe(24)
token = ''
try:
    docker('volume', 'create', volume)
    data = docker('volume', 'inspect', '--format', '{{.Mountpoint}}', volume)
    with tempfile.TemporaryDirectory(prefix='agentbox-browser-test-') as tmp:
        tmp = Path(tmp)
        config = tmp / 'config.json'
        config.write_text(json.dumps({'listen':'0.0.0.0:8180','auth_token':password,
            'data_dir':data,'agent_image':a.image,'timezone':'UTC',
            'container':{'network':'none','memory_mb':2048,'cpus':2,'pids_limit':512},
            'accounts':[{'id':'synthetic','type':'codex','label':'Synthetic'}]}))
        server = docker('create','--mount','type=volume,src='+volume+',dst='+data,
            '--mount','type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock',
            '-p','127.0.0.1::8180','--entrypoint','/opt/agentbox','alpine:3','--config','/tmp/config.json')
        docker('cp',str(a.binary.resolve()),server+':/opt/agentbox')
        docker('cp',str(config),server+':/tmp/config.json')
        docker('start',server)
        port = json.loads(docker('inspect',server))[0]['NetworkSettings']['Ports']['8180/tcp'][0]['HostPort']
        base = 'http://127.0.0.1:'+port
        def request(path, body=None, method=None, override=None):
            raw=json.dumps(body).encode() if body is not None else None
            req=urllib.request.Request(base+path,data=raw,headers={'Authorization':'Bearer '+(token if override is None else override)},method=method)
            with urllib.request.urlopen(req,timeout=70) as response:
                return json.load(response)
        for _ in range(100):
            try:
                token=request('/api/login',{'username':'boxadmin','password':password})['token']
                break
            except (urllib.error.URLError,ConnectionError):
                time.sleep(.2)
        else:
            raise RuntimeError('test server unavailable')
        sess=request('/api/sessions',{'name':'Remote browser fixture','agent':'codex','account_id':'synthetic'})
        containers.append('agentbox-'+sess['id'])
        path='/api/sessions/'+sess['id']
        assert not request(path+'/browser')['running']
        assert request(path+'/browser',{},'POST')['running']
        cid=request(path)['container_id']
        inspect=json.loads(docker('inspect',cid))[0]
        assert not inspect['HostConfig']['Privileged']
        assert not inspect['HostConfig']['PortBindings']
        assert inspect['Config']['User']=='1000:1000'
        assert any(x.startswith('seccomp=') for x in inspect['HostConfig']['SecurityOpt'])
        # Fixture HTTP app stores an ordinary persistent login cookie. All page
        # loads stay on container loopback, with the workspace network disabled.
        fixture=tmp/'website.py'
        fixture.write_text('''from http.server import HTTPServer,BaseHTTPRequestHandler
from pathlib import Path
class Handler(BaseHTTPRequestHandler):
 def do_GET(self):
  Path('/workspace/browser-hits').open('a').write(self.path+' '+self.headers.get('Cookie','')+'\\n')
  self.send_response(200)
  if self.path=='/login': self.send_header('Set-Cookie','synthetic_login=yes; Max-Age=86400; Path=/')
  self.send_header('Content-Type','text/html; charset=utf-8');self.end_headers()
  self.wfile.write("""<html><body style="background:#f4f6fa;color:#13233a;font:24px sans-serif;padding:48px"><h1>Remote browser fixture</h1><p>Persistent login and UTF-8 clipboard</p><textarea oninput="fetch('/typed?value='+encodeURIComponent(this.value))" style="width:600px;height:180px;font-size:24px" autofocus></textarea></body></html>""".encode())
 def log_message(self,*args):pass
HTTPServer(('127.0.0.1',8765),Handler).serve_forever()
''')
        docker('cp',str(fixture),cid+':/tmp/website.py')
        def start_site():
            docker('exec','-d','--user','1000:1000',cid,'python3','/tmp/website.py')
        start_site()
        time.sleep(.3)
        request(path+'/browser',{'url':'http://127.0.0.1:8765/login'},'POST')
        for _ in range(50):
            try:
                hits=docker('exec',cid,'cat','/workspace/browser-hits')
                if '/login' in hits:break
            except subprocess.CalledProcessError:pass
            time.sleep(.2)
        else:raise AssertionError('browser did not load synthetic page')
        request(path+'/browser/clipboard',{'text':'中文剪贴板 ✓'},'POST')
        assert request(path+'/browser/clipboard')['text']=='中文剪贴板 ✓'
        assert not request(path+'/browser',method='DELETE')['running']
        assert request(path+'/browser',{'url':'http://127.0.0.1:8765/check'},'POST')['running']
        for _ in range(50):
            hits=docker('exec',cid,'cat','/workspace/browser-hits')
            if '/check synthetic_login=yes' in hits:break
            time.sleep(.2)
        else:raise AssertionError('login cookie lost on browser restart: '+hits)
        # End-to-end noVNC/UI connection, screenshot and clipboard paste into
        # Chrome's focused textarea. Credentials reach the test through stdin.
        result=subprocess.run(['node',str(Path(__file__).with_name('test-remote-browser-live.mjs'))],
            input=json.dumps({'base':base,'password':password,'session':sess['id'],'token':token}),text=True,check=True)
        request(path+'/stop',{},'POST')
        assert not request(path+'/browser')['running']
        request(path+'/start',{},'POST')
        start_site()
        request(path+'/browser',{'url':'http://127.0.0.1:8765/after-space-restart'},'POST')
        for _ in range(50):
            hits=docker('exec',cid,'cat','/workspace/browser-hits')
            if '/after-space-restart synthetic_login=yes' in hits:break
            time.sleep(.2)
        else:raise AssertionError('login cookie lost on workspace restart')
        # Browser endpoints are never public, even status.
        try:request(path+'/browser',override='invalid')
        except urllib.error.HTTPError as err:assert err.code==401
        else:raise AssertionError('unauthenticated browser access')
        request(path+'?purge=1',method='DELETE')
        print('Real browser: sandbox, no published ports, page loading, UTF-8 clipboard, RFB/UI, persistent cookie across browser/workspace restart, auth, purge passed')
except Exception as exc:
    if isinstance(exc, urllib.error.HTTPError):
        print(exc.read().decode())
    for cid in containers:
        subprocess.run(['docker','exec',cid,'cat','/tmp/agentbox-browser-1000/error'],check=False)
    if server:
        print(docker('logs','--tail','30',server))
    raise
finally:
    for cid in containers:
        subprocess.run(['docker','rm','-f',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    if server:
        subprocess.run(['docker','rm','-f',server],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    subprocess.run(['docker','volume','rm',volume],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
