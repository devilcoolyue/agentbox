"""Synthetic Codex exec/app-server test; run only inside the isolated container."""
import http.server, threading, json, subprocess, pathlib, os, time, socket, queue
calls = 0
class Fake(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_POST(self):
        global calls
        calls += 1
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        def send(kind, **kw):
            event = {'type': kind, **kw}
            self.wfile.write(('event: '+kind+'\ndata: '+json.dumps(event)+'\n\n').encode())
            self.wfile.flush()
        try:
            send('response.created', response={'id':'resp_fixture','object':'response','created_at':1,'status':'in_progress','model':'deepseek-v4.1-flash','output':[]})
            send('response.output_item.added', output_index=0, item={'id':'msg_fixture','type':'message','role':'assistant','status':'in_progress','content':[]})
            send('response.content_part.added', output_index=0, item_id='msg_fixture', content_index=0, part={'type':'output_text','text':'','annotations':[]})
            for _ in range(300):
                send('response.output_text.delta', item_id='msg_fixture', output_index=0, content_index=0, delta='Go.\n')
        except (BrokenPipeError, ConnectionResetError): pass
up = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Fake)
threading.Thread(target=up.serve_forever, daemon=True).start()
with socket.socket() as sock:
    sock.bind(('127.0.0.1',0)); port=sock.getsockname()[1]
env=dict(os.environ)
env['LOOP_GUARD_TOKEN']='Bearer synthetic-key'
env['LOOP_TEST_API_KEY']='synthetic-key'
gate=subprocess.Popen(['/tmp/abox-loop-guard','-listen',f'127.0.0.1:{port}','-upstream',f'http://127.0.0.1:{up.server_port}','-auth-header','Authorization','-dedicated-connections'],env=env,stderr=subprocess.PIPE)
child=None
try:
    for _ in range(50):
        try:
            with socket.create_connection(('127.0.0.1',port),timeout=.2): break
        except OSError: time.sleep(.05)
    p=pathlib.Path('/home/agent/.codex'); p.mkdir()
    (p/'config.toml').write_text(f'''model = "deepseek-v4.1-flash"
model_provider = "guard"
[model_providers.guard]
name = "synthetic"
base_url = "http://127.0.0.1:{port}/v1"
wire_api = "responses"
env_key = "LOOP_TEST_API_KEY"
supports_websockets = false
''')
    version=subprocess.check_output(['codex','--version'],text=True).strip()
    result=subprocess.run(['codex','exec','--json','--skip-git-repo-check','--sandbox','read-only','Reply briefly; do not use tools.'],env=env,cwd='/tmp',capture_output=True,text=True,timeout=55)
    outcomes=[json.loads(l) for l in result.stdout.splitlines() if l.startswith('{') and any(x in l for x in ['turn.failed','error','turn.completed'])]
    print(json.dumps({'mode':'exec','codex':version,'exit':result.returncode,'upstream_requests':calls,'outcomes':outcomes[-4:]},ensure_ascii=False),flush=True)
    assert calls==1 and result.returncode!=0 and 'recently looped' in result.stdout
    before=calls
    child=subprocess.Popen(['codex','app-server'],env=env,cwd='/tmp',stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
    messages=queue.Queue()
    def reader():
        for line in child.stdout:
            try: messages.put(json.loads(line))
            except ValueError: pass
    threading.Thread(target=reader,daemon=True).start()
    def send(v): child.stdin.write(json.dumps(v)+'\n'); child.stdin.flush()
    def call(i,method,params):
        send({'id':i,'method':method,'params':params})
        while True:
            v=messages.get(timeout=30)
            if v.get('id')==i:
                assert 'error' not in v, v.get('error')
                return v['result']
    call(1,'initialize',{'clientInfo':{'name':'loop-guard-fixture','version':'1'}})
    send({'method':'initialized'})
    thread=call(2,'thread/start',{'cwd':'/tmp','model':'deepseek-v4.1-flash','approvalPolicy':'never','sandbox':'read-only'})
    call(3,'turn/start',{'threadId':thread['thread']['id'],'input':[{'type':'text','text':'Synthetic app-server loop fixture; do not use tools.'}]})
    events=[]
    while True:
        v=messages.get(timeout=55)
        if v.get('method') in ('error','turn/completed'):events.append(v)
        if v.get('method')=='turn/completed':break
    print(json.dumps({'mode':'app-server','upstream_requests':calls-before,'outcomes':events},ensure_ascii=False),flush=True)
    assert calls-before==1 and v['params']['turn']['status']=='failed' and 'recently looped' in json.dumps(events)
finally:
    if child is not None:
        child.terminate();child.wait(timeout=5)
    gate.terminate();_,log=gate.communicate(timeout=5)
    print('gateway_log',log.decode().strip(),flush=True)
    up.shutdown()
