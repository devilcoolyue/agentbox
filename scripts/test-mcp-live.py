#!/usr/bin/env python3
"""Disposable Docker MCP/Claude smoke. Synthetic data, network=none, no paid API.
Runs the embedded helper and the same Claude headless argv as agentbox.
"""
import argparse
import json
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--image', default='agentbox-agent:claude-2.1.280-codex-0.145.0')
args = parser.parse_args()
helper = Path(__file__).resolve().parents[1].joinpath('internal/mcpconfig/helper.py').read_text()
script = 'HELPER = ' + repr(helper) + '\n' + r'''
import json, os, pathlib, signal, subprocess, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
home=pathlib.Path('/home/agent');(home/'.claude').mkdir(exist_ok=True)
state=home/'.claude.json'
state.write_text(json.dumps({'hasCompletedOnboarding':True,'bypassPermissionsModeAccepted':True,'projects':{'/workspace':{'hasTrustDialogAccepted':True,'hasCompletedProjectOnboarding':True}},'mcpServers':{'unmanaged':{'type':'stdio','command':'true'}}}))
probe=home/'probe.py'
probe.write_text("""import json,sys
for line in sys.stdin:
 r=json.loads(line)
 with open('/tmp/mcp-methods','a') as f:f.write(r.get('method','')+'\\n')
 if 'id' not in r:continue
 m=r.get('method')
 if m=='initialize':result={'protocolVersion':r['params']['protocolVersion'],'capabilities':{'tools':{}},'serverInfo':{'name':'synthetic','version':'1'}}
 elif m=='tools/list':result={'tools':[{'name':'echo','description':'Return the synthetic probe marker','inputSchema':{'type':'object','properties':{}}}]}
 elif m=='tools/call':result={'content':[{'type':'text','text':'MCP_SYNTHETIC_OK'}]}
 else:result={}
 print(json.dumps({'jsonrpc':'2.0','id':r['id'],'result':result}),flush=True)
""")
def start(payload):
 p=subprocess.Popen(['python3','-I','-c',HELPER],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
 p.stdin.write(json.dumps(payload)+'\n');p.stdin.flush();return p
def result(p):
 raw=p.stdout.readline();p.wait(timeout=35)
 assert p.returncode==0,(p.returncode,p.stderr.read())
 return json.loads(raw)
def run(payload, expected):
 r=result(start(payload));assert r['status']==expected,r;return r
stdio={'type':'stdio','command':'python3','args':[str(probe)],'env':{'SYNTHETIC_TOKEN':'never-echo-this'}}
run({'action':'apply','name':'probe','expected':None,'desired':stdio},'applied')
assert json.loads(state.read_text())['hasCompletedOnboarding']
assert 'unmanaged' in json.loads(state.read_text())['mcpServers']
r=run({'action':'check','config':stdio},'connected');assert r['tools'][0]['name']=='echo'
assert 'tools/call' not in pathlib.Path('/tmp/mcp-methods').read_text()
run({'action':'apply','name':'probe','expected':None,'desired':stdio},'conflict')
run({'action':'check','config':{'type':'stdio','command':'/missing-command'}},'missing_command')

sessions=[]; model_calls=[]
class HTTP(BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_DELETE(self):self.send_response(204);self.end_headers()
 def do_POST(self):
  r=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
  if self.path.startswith('/v1/messages'):
   model_calls.append(r)
   called=any(isinstance(m.get('content'),list) and any(b.get('type')=='tool_result' and 'MCP_SYNTHETIC_OK' in json.dumps(b) for b in m['content']) for m in r.get('messages',[]))
   name='mcp__probe__echo'
   if not called: assert any(t.get('name')==name for t in r.get('tools',[])),r.get('tools')
   self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
   def ev(kind,data):self.wfile.write(('event: '+kind+'\ndata: '+json.dumps({'type':kind,**data})+'\n\n').encode());self.wfile.flush()
   ev('message_start',{'message':{'id':'msg_probe_'+str(len(model_calls)),'type':'message','role':'assistant','model':'claude-sonnet-4-6','content':[],'stop_reason':None,'stop_sequence':None,'usage':{'input_tokens':10,'output_tokens':0}}})
   if called:
    ev('content_block_start',{'index':0,'content_block':{'type':'text','text':''}})
    ev('content_block_delta',{'index':0,'delta':{'type':'text_delta','text':'MCP_SYNTHETIC_OK'}})
   else:
    ev('content_block_start',{'index':0,'content_block':{'type':'tool_use','id':'tool_probe','name':name,'input':{}}})
    ev('content_block_delta',{'index':0,'delta':{'type':'input_json_delta','partial_json':'{}'}})
   ev('content_block_stop',{'index':0})
   ev('message_delta',{'delta':{'stop_reason':'end_turn' if called else 'tool_use','stop_sequence':None},'usage':{'output_tokens':5}})
   ev('message_stop',{});return
  if self.path=='/denied':self.send_response(401);self.end_headers();return
  if 'id' not in r:self.send_response(202);self.end_headers();return
  if r['method']=='initialize':data={'protocolVersion':'2025-03-26','capabilities':{'tools':{}},'serverInfo':{'name':'http-probe','version':'1'}}
  else:
   assert self.headers.get('Mcp-Session-Id')=='synthetic-session'
   assert self.headers.get('MCP-Protocol-Version')=='2025-03-26'
   data={'tools':[{'name':'http-echo','description':'Synthetic'}]}
  obj={'jsonrpc':'2.0','id':r['id'],'result':data}
  raw=('event: message\ndata: '+json.dumps(obj)+'\n\n').encode() if self.path=='/sse' else json.dumps(obj).encode()
  self.send_response(200);self.send_header('Content-Type','text/event-stream' if self.path=='/sse' else 'application/json');self.send_header('Mcp-Session-Id','synthetic-session');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
http=ThreadingHTTPServer(('127.0.0.1',0),HTTP);threading.Thread(target=http.serve_forever,daemon=True).start()
base='http://127.0.0.1:'+str(http.server_port)
for path in ('/json','/sse'):
 r=run({'action':'check','config':{'type':'http','url':base+path}},'connected');assert r['tools'][0]['name']=='http-echo'
run({'action':'check','config':{'type':'http','url':base+'/denied','headers':{'Authorization':'Bearer never-echo-this'}}},'authentication_required')
print('PASS: native apply/conflict, stdio and JSON/SSE HTTP discovery, authentication and missing command',flush=True)
# Validate actual Claude tool invocation using a local synthetic Anthropic API.
env=os.environ.copy();env.update({'ANTHROPIC_API_KEY':'synthetic-only','ANTHROPIC_BASE_URL':base,'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC':'1','ENABLE_TOOL_SEARCH':'false'})
chat=subprocess.run(['claude','-p','--output-format','stream-json','--include-partial-messages','--verbose','--permission-mode','bypassPermissions','--model','claude-sonnet-4-6'],input='Call the probe echo tool once.',text=True,capture_output=True,env=env,timeout=50)
assert chat.returncode==0,(chat.returncode,chat.stdout[-3000:],chat.stderr[-1000:])
assert 'tools/call' in pathlib.Path('/tmp/mcp-methods').read_text(),chat.stdout[-3000:]
assert 'MCP_SYNTHETIC_OK' in chat.stdout and len(model_calls)>=2
print('PASS: Claude headless round trip called MCP tool through synthetic model',flush=True)
# Cancellation and the independent deadline must reap process groups.
hang=home/'hang.py';hang.write_text("import subprocess,os,time,pathlib\np=subprocess.Popen(['sleep','120'])\npathlib.Path('/tmp/mcp-pids').write_text(str(os.getpid())+' '+str(p.pid))\ntime.sleep(120)\n")
for cancel in (True,False):
 pathlib.Path('/tmp/mcp-pids').unlink(missing_ok=True)
 p=start({'action':'check','config':{'type':'stdio','command':'python3','args':[str(hang)]}})
 deadline=time.monotonic()+5
 while not pathlib.Path('/tmp/mcp-pids').exists():
  assert time.monotonic()<deadline
  time.sleep(.02)
 pids=pathlib.Path('/tmp/mcp-pids').read_text().split()
 if cancel:p.stdin.write('x');p.stdin.flush()
 r=result(p);assert r['status']==('cancelled' if cancel else 'timeout'),r
 for pid in pids:
  path=pathlib.Path('/proc')/pid/'stat'
  assert not path.exists() or path.read_text().split()[2]=='Z','live orphan '+pid
print('PASS: cancellation and timeout clean up child process groups',flush=True)
run({'action':'apply','name':'probe','expected':stdio,'desired':None},'applied')
assert 'probe' not in json.loads(state.read_text())['mcpServers']
assert 'unmanaged' in json.loads(state.read_text())['mcpServers']
print('PASS: managed deletion preserves unrelated native state',flush=True)
'''
subprocess.run(['docker','run','--rm','-i','--init','--network','none','--user','1000:1000',
                '--security-opt','no-new-privileges','--tmpfs','/home/agent:uid=1000,gid=1000',
                '--tmpfs','/workspace:uid=1000,gid=1000','--tmpfs','/tmp',args.image,'python3','-'],
               input=script,text=True,check=True,timeout=180)
