"""Container-only MCP operations. Input/output are bounded JSON, never raw errors.
No dependencies beyond Python 3; HTTP uses the container's proxy environment.
"""
import json, os, signal, subprocess, sys, threading, urllib.request, urllib.error

LIMIT = 1024 * 1024
children = []
class Failure(Exception):
    def __init__(self, status): self.status = status

def fail_timeout(*_): raise Failure('timeout')
def fail_cancel(*_): raise Failure('cancelled')

def cleanup():
    for p in children:
        try: os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError: pass
        try: p.wait(timeout=1)
        except subprocess.TimeoutExpired: pass

def launch(argv):
    try:
        p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, start_new_session=True)
    except FileNotFoundError: raise Failure('missing_command')
    children.append(p)
    return p

def canonical(d):
    if d is None: return None
    d = dict(d)
    d.setdefault('type', 'stdio')
    for key in ('args', 'env', 'headers', 'command', 'url'):
        if not d.get(key): d.pop(key, None)
    return d

def native(name):
    try:
        fd = os.open('/home/agent/.claude.json', os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd) as f: state = json.loads(f.read(LIMIT * 4 + 1))
        return state.get('mcpServers', {}).get(name)
    except FileNotFoundError: return None

def cli(args):
    p = launch(['claude', 'mcp', *args])
    # CLI configuration output can echo credentials. Drain it without retaining
    # it or relaying it back to the host/browser.
    p.stdin.close()
    while p.stdout.read(8192): pass
    if p.wait() != 0: raise Failure('apply_failed')

def apply(req):
    name, expected, desired = req['name'], req.get('expected'), req.get('desired')
    if canonical(native(name)) != canonical(expected): raise Failure('conflict')
    if expected is not None: cli(['remove', '-s', 'user', name])
    if desired is not None:
        # argv is passed directly to exec inside the owner's container. The host
        # Docker command only contains this fixed helper, never secret payloads.
        cli(['add-json', '-s', 'user', name, json.dumps(desired)])
    if canonical(native(name)) != canonical(desired): raise Failure('conflict')
    return {'status': 'applied'}

class Stdio:
    def __init__(self, d):
        env = os.environ.copy(); env.update(d.get('env', {}))
        try:
            self.p = subprocess.Popen([d['command'], *d.get('args', [])], env=env,
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                start_new_session=True)
        except FileNotFoundError: raise Failure('missing_command')
        children.append(self.p)
    def send(self, msg):
        self.p.stdin.write((json.dumps(msg)+'\n').encode()); self.p.stdin.flush()
        if 'id' not in msg: return None
        total = 0
        while True:
            line = self.p.stdout.readline(LIMIT + 1); total += len(line)
            if not line or total > LIMIT: raise Failure('protocol_error')
            obj = json.loads(line)
            if obj.get('id') == msg['id']: return obj

class HTTP:
    def __init__(self, d):
        self.url = d['url']; self.headers = dict(d.get('headers', {}))
        self.headers.update({'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream'})
        # Do not redirect bearer credentials to another endpoint.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs): return None
        self.opener = urllib.request.build_opener(NoRedirect)
    def send(self, msg):
        req = urllib.request.Request(self.url, data=json.dumps(msg).encode(), headers=self.headers, method='POST')
        try: response = self.opener.open(req, timeout=20)
        except urllib.error.HTTPError as e:
            raise Failure('authentication_required' if e.code in (401,403) else 'network_error')
        except (urllib.error.URLError, OSError): raise Failure('network_error')
        with response:
            sid = response.headers.get('Mcp-Session-Id')
            if sid: self.headers['Mcp-Session-Id'] = sid
            if 'id' not in msg: return None
            if 'text/event-stream' in response.headers.get('Content-Type', ''):
                total = 0; data = []
                while True:
                    line = response.readline(LIMIT+1); total += len(line)
                    if total > LIMIT: raise Failure('protocol_error')
                    if not line: raise Failure('protocol_error')
                    if line in (b'\n', b'\r\n'):
                        if data:
                            obj = json.loads('\n'.join(data)); data = []
                            if obj.get('id') == msg['id']: return obj
                    elif line.startswith(b'data:'): data.append(line[5:].decode().strip())
            raw = response.read(LIMIT+1)
            if len(raw)>LIMIT: raise Failure('protocol_error')
            obj = json.loads(raw)
            if obj.get('id') != msg['id']: raise Failure('protocol_error')
            return obj
    def close(self):
        if 'Mcp-Session-Id' not in self.headers: return
        try:
            with self.opener.open(urllib.request.Request(self.url, headers=self.headers, method='DELETE'), timeout=1): pass
        except Exception: pass

def check(d):
    transport = Stdio(d) if d.get('type', 'stdio') == 'stdio' else HTTP(d)
    counter = 0
    def rpc(method, params):
        nonlocal counter
        counter += 1
        obj = transport.send({'jsonrpc':'2.0','id':counter,'method':method,'params':params})
        if not isinstance(obj, dict) or obj.get('error') or 'result' not in obj: raise Failure('protocol_error')
        return obj['result']
    try:
        init = rpc('initialize', {'protocolVersion':'2025-03-26','capabilities':{},'clientInfo':{'name':'agentbox-check','version':'1'}})
        if init.get('protocolVersion') not in ('2024-11-05','2025-03-26','2025-06-18'): raise Failure('protocol_error')
        if isinstance(transport, HTTP): transport.headers['MCP-Protocol-Version'] = init['protocolVersion']
        transport.send({'jsonrpc':'2.0','method':'notifications/initialized'})
        tools = []; cursor = None
        if 'tools' in init.get('capabilities', {}):
            for _ in range(10):
                result = rpc('tools/list', {'cursor':cursor} if cursor else {})
                for tool in result.get('tools', []):
                    if len(tools)>=200: break
                    tools.append({'name':str(tool.get('name',''))[:128], 'description':str(tool.get('description',''))[:1000]})
                cursor = result.get('nextCursor')
                if not cursor or len(tools)>=200: break
        return {'status':'connected', 'tools':tools, 'truncated':bool(cursor)}
    finally:
        if isinstance(transport, HTTP): transport.close()

def main():
    signal.signal(signal.SIGALRM, fail_timeout)
    signal.signal(signal.SIGUSR1, fail_cancel)
    signal.alarm(28)
    req = json.loads(sys.stdin.buffer.readline(LIMIT+1))
    def cancelled():
        sys.stdin.buffer.read(1)
        os.kill(os.getpid(), signal.SIGUSR1)
    threading.Thread(target=cancelled, daemon=True).start()
    try:
        result = apply(req) if req['action']=='apply' else check(req['config'])
    except Failure as e: result = {'status':e.status}
    except Exception: result = {'status':'protocol_error'}
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGUSR1, signal.SIG_IGN)
        cleanup()
    print(json.dumps(result), flush=True)
    os._exit(0)

if __name__ == '__main__': main()
