#!/usr/bin/env python3
"""Chrome/Chromium must reach an unresolvable host through its local adapter.
Disposable container, synthetic authenticated proxy, network=none, no user data.
"""
import argparse
from pathlib import Path
import subprocess
import tempfile

PROBE = r'''
import http.server,json,os,subprocess,threading,time
hits=[]
class Upstream(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  if self.headers.get('Proxy-Authorization')!='Basic Zml4dHVyZTpzZWNyZXQ=':
   self.send_error(407);return
  hits.append(self.path)
  body=b'<html><title>Browser proxy fixture</title><body>Proxy routing works</body></html>'
  self.send_response(200);self.send_header('Content-Type','text/html');self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
 def do_CONNECT(self):
  try:self.send_error(502)
  except (BrokenPipeError,ConnectionResetError):pass
 def log_message(self,*args):pass
server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Upstream)
threading.Thread(target=server.serve_forever,daemon=True).start()
env={**os.environ,'AGENTBOX_BROWSER_PROXY':'http://fixture:secret@127.0.0.1:'+str(server.server_port),'AGENTBOX_BROWSER_NETWORK_KEY':'synthetic-proxy'}
def command(action,data):
 result=subprocess.run(['python3','-I','/opt/agentbox/browser.py',action],input=json.dumps(data),text=True,capture_output=True,env=env,timeout=45)
 if result.returncode:raise RuntimeError(result.stdout)
 return json.loads(result.stdout)
try:
 state=command('start',{'url':'http://unresolvable.invalid/browser-proxy-regression'})
 assert state['running'] and state['proxy'],state
 for _ in range(100):
  if 'http://unresolvable.invalid/browser-proxy-regression' in hits:break
  time.sleep(.1)
 else:raise AssertionError('Chrome did not reach the authenticated proxy: '+repr(hits))
 print(state['browser']+': destination DNS delegated, loopback adapter reachable, upstream authentication passed',flush=True)
finally:
 command('stop',{});server.shutdown()
'''

def main():
 p=argparse.ArgumentParser(description=__doc__)
 p.add_argument('--image',default='agentbox-agent:browser')
 p.add_argument('--helper',type=Path,default=Path(__file__).resolve().parent.parent/'images/browser/browser.py')
 p.add_argument('--seccomp',type=Path,default=Path(__file__).resolve().parent.parent/'internal/dockerx/browser-seccomp.json')
 a=p.parse_args()
 cid=subprocess.check_output(['docker','create','--network','none','--init','--user','1000:1000',
  '--memory','2g','--cpus','2','--pids-limit','512','--security-opt','no-new-privileges:true',
  '--security-opt','seccomp='+str(a.seccomp.resolve()),'--tmpfs','/workspace:uid=1000,gid=1000',
  '--tmpfs','/home/agent:uid=1000,gid=1000',a.image,'sleep','infinity'],text=True).strip()
 try:
  with tempfile.TemporaryDirectory(prefix='browser-proxy-test-') as tmp:
   probe=Path(tmp)/'probe.py';probe.write_text(PROBE);probe.chmod(0o644)
   subprocess.run(['docker','cp',str(a.helper),cid+':/opt/agentbox/browser.py'],check=True)
   subprocess.run(['docker','cp',str(probe),cid+':/tmp/probe.py'],check=True)
   subprocess.run(['docker','start',cid],check=True,stdout=subprocess.DEVNULL)
   subprocess.run(['docker','exec',cid,'python3','-I','/tmp/probe.py'],check=True,timeout=100)
 finally:
  subprocess.run(['docker','rm','-f',cid],check=True,stdout=subprocess.DEVNULL)
if __name__=='__main__':main()
