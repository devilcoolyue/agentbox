#!/usr/bin/env python3
"""Real agentbox HTTP -> Docker MCP integration, disposable data and no model API."""
import argparse, json, secrets, subprocess, tempfile, time, urllib.request, urllib.error, uuid
from pathlib import Path
p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--binary',type=Path,required=True)
p.add_argument('--image',default='agentbox-agent:claude-2.1.280-codex-0.145.0')
a=p.parse_args()
def docker(*args): return subprocess.check_output(['docker',*args],text=True).strip()
volume='agentbox-mcp-test-'+uuid.uuid4().hex
server=None;containers=[];token='';password=secrets.token_urlsafe(24)
try:
 docker('volume','create',volume)
 data=docker('volume','inspect','--format','{{.Mountpoint}}',volume)
 with tempfile.TemporaryDirectory(prefix='agentbox-mcp-test-') as directory:
  tmp=Path(directory)
  cfg=tmp/'config.json';cfg.write_text(json.dumps({'listen':'0.0.0.0:8180','auth_token':password,'data_dir':data,'agent_image':a.image,'timezone':'UTC','tunnel':{'enabled':False},'container':{'network':'none','memory_mb':1024,'cpus':2,'pids_limit':128},'accounts':[{'id':'fixture','type':'claude','label':'Synthetic'}]}))
  server=docker('create','--mount','type=volume,src='+volume+',dst='+data,'--mount','type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock','-p','127.0.0.1::8180','--entrypoint','/opt/agentbox','alpine:3','--config','/tmp/config.json')
  docker('cp',str(a.binary.resolve()),server+':/opt/agentbox');docker('cp',str(cfg),server+':/tmp/config.json');docker('start',server)
  port=json.loads(docker('inspect',server))[0]['NetworkSettings']['Ports']['8180/tcp'][0]['HostPort'];base='http://127.0.0.1:'+port
  def request(path,body=None,method=None):
   raw=json.dumps(body).encode() if body is not None else None
   req=urllib.request.Request(base+path,data=raw,headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'},method=method)
   with urllib.request.urlopen(req,timeout=70) as response:return json.load(response)
  for _ in range(100):
   try:token=request('/api/login',{'username':'boxadmin','password':password})['token'];break
   except (urllib.error.URLError,ConnectionError):time.sleep(.2)
  else:raise RuntimeError('server unavailable')
  sess=request('/api/sessions',{'name':'MCP fixture','agent':'claude','account_id':'fixture'});path='/api/sessions/'+sess['id'];containers.append('agentbox-'+sess['id'])
  # Configure a self-contained synthetic stdio server before starting the workspace.
  code="""import json,sys
for line in sys.stdin:
 r=json.loads(line)
 if 'id' not in r:continue
 if r['method']=='initialize':v={'protocolVersion':r['params']['protocolVersion'],'capabilities':{'tools':{}},'serverInfo':{'name':'fixture','version':'1'}}
 elif r['method']=='tools/list':v={'tools':[{'name':'echo','description':'Synthetic','inputSchema':{'type':'object'}}]}
 else:v={}
 print(json.dumps({'jsonrpc':'2.0','id':r['id'],'result':v}),flush=True)
"""
  definition={'type':'stdio','command':'python3','args':['-c',code],'env':{'TOKEN':'synthetic-secret'}}
  request('/api/mcp/probe',{'revision':0,'entry':{'config':definition}},'PUT')
  v=request('/api/mcp');assert 'synthetic-secret' not in json.dumps(v)
  request(path+'/start',{},'POST');cid=request(path)['container_id']
  def native():return json.loads(docker('exec',cid,'cat','/home/agent/.claude.json'))
  assert native()['mcpServers']['probe']['env']['TOKEN']=='synthetic-secret'
  result=request(path+'/mcp/probe/check',{},'POST');assert result['status']=='connected',result
  assert result['tools'][0]['name']=='echo'
  print('PASS: HTTP API -> workspace startup -> native Claude config -> Docker helper tools discovery',flush=True)
  # An already-running workspace synchronizes the next use, preserving secrets.
  definition['env']['TOKEN']='__AGENTBOX_KEEP_SECRET__';definition['args']=['-u','-c',code]
  request('/api/mcp/probe',{'revision':1,'entry':{'config':definition}},'PUT');request(path+'/start',{},'POST')
  assert native()['mcpServers']['probe']['args'][0]=='-u'
  assert native()['mcpServers']['probe']['env']['TOKEN']=='synthetic-secret'
  # CLI external change is detected; explicit adoption preserves the native entry.
  docker('exec','--user','1000:1000',cid,'claude','mcp','remove','-s','user','probe')
  docker('exec','--user','1000:1000',cid,'claude','mcp','add','-s','user','probe','--','true')
  v=request(path+'/mcp');item=next(x for x in v['items'] if x['name']=='probe');assert item['status']=='conflict'
  request(path+'/start',{},'POST');assert native()['mcpServers']['probe']['command']=='true'
  request(path+'/mcp/probe/adopt',{'revision':v['revision'],'native_revision':item['native_revision']},'POST')
  v=request(path+'/mcp');assert v['items'][0]['status']=='applied'
  # Remove the override to return to user defaults, then delete the user default.
  request(path+'/mcp/probe',{'revision':v['revision']},'DELETE');request(path+'/start',{},'POST')
  assert native()['mcpServers']['probe']['command']=='python3'
  request('/api/mcp/probe',{'revision':2},'DELETE');request(path+'/start',{},'POST')
  assert 'probe' not in native().get('mcpServers',{})
  request(path+'?purge=1',method='DELETE')
  print('PASS: running-workspace refresh, secret retention, external-edit conflict, explicit adoption, inheritance restore and deletion',flush=True)
except Exception as exc:
 if isinstance(exc,urllib.error.HTTPError):print(exc.read().decode())
 if server:print(docker('logs','--tail','20',server))
 raise
finally:
 for cid in containers:
  subprocess.run(['docker','rm','-f',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 if server:subprocess.run(['docker','rm','-f',server],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 subprocess.run(['docker','volume','rm',volume],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
