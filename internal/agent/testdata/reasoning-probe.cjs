// Runs only inside a disposable, network=none Linux container. The only HTTP
// endpoint is this in-container fake provider. All credentials are synthetic.
const fs = require('fs');
const http = require('http');
const { spawn } = require('child_process');
const readline = require('readline');

async function run(spec) {
  const home = fs.mkdtempSync('/tmp/agentbox-reasoning-');
  const cwd = home + '/workspace'; fs.mkdirSync(cwd);
  fs.mkdirSync(home + '/.claude'); fs.mkdirSync(home + '/.codex');
  const requests = [];
  const server = http.createServer((req, res) => {
    let raw = '';
    req.on('data', chunk => raw += chunk);
    req.on('end', () => {
      try {
        const body = JSON.parse(raw);
        if (body.messages || body.input) requests.push(body);
      } catch {}
      res.writeHead(400, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ type: 'error', error: { type: 'invalid_request_error', message: 'synthetic verification endpoint' } }));
    });
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const base = 'http://127.0.0.1:' + server.address().port;
  const env = { ...process.env, HOME: home, CODEX_HOME: home + '/.codex',
    ANTHROPIC_BASE_URL: base, ANTHROPIC_API_KEY: 'synthetic-key', OPENAI_API_KEY: 'synthetic-key',
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1', ...spec.env };
  if (spec.claudeSettings) fs.writeFileSync(home + '/.claude/settings.json', JSON.stringify(spec.claudeSettings));
  const config = (spec.codexConfig || '') + '\nmodel_provider="fixture"\n[model_providers.fixture]\nname="Fixture"\nbase_url="' + base + '/v1"\nwire_api="responses"\nenv_key="OPENAI_API_KEY"\n';
  fs.writeFileSync(home + '/.codex/config.toml', config);
  const args = spec.command || ['codex', 'app-server'];
  const child = spawn(args[0], args.slice(1), { env, cwd });
  let stderr = ''; child.stderr.on('data', chunk => stderr += chunk);
  let protocolError;
  if (spec.protocol) {
    let id = 0;
    const pending = new Map();
    readline.createInterface({ input: child.stdout }).on('line', line => {
      try { const m = JSON.parse(line); if (pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } } catch {}
    });
    const call = (method, params) => new Promise(resolve => {
      pending.set(++id, resolve); child.stdin.write(JSON.stringify({ id, method, params }) + '\n');
    });
    (async () => {
      await call('initialize', { clientInfo: { name: 'synthetic-probe', version: '1' } });
      child.stdin.write('{"method":"initialized"}\n');
      const thread = await call('thread/start', { cwd, model: spec.model, sandbox: 'danger-full-access', approvalPolicy: 'never' });
      if (thread.error) { protocolError = thread.error; child.kill(); return; }
      const result = await call('turn/start', { threadId: thread.result.thread.id, model: spec.model,
        ...(spec.effort ? { effort: spec.effort } : {}), ...(spec.summary ? { summary: spec.summary } : {}),
        input: [{ type: 'text', text: 'Reply hello. Do not use tools.' }] });
      if (result.error) { protocolError = result.error; child.kill(); }
    })().catch(e => { protocolError = e.message; child.kill(); });
  } else { child.stdout.resume(); child.stdin.end('Reply hello. Do not use tools.'); }
  const stop = setInterval(() => { if (requests.length) child.kill('SIGTERM'); }, 100);
  const timeout = setTimeout(() => child.kill('SIGKILL'), 20000);
  await new Promise(resolve => child.on('close', resolve));
  clearInterval(stop); clearTimeout(timeout);
  server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
  // Return only the model/control fields, never prompts, tools, or environment.
  console.log(JSON.stringify({ name: spec.name, requests: requests.map(b => ({ model: b.model,
    thinking: b.thinking, output_config: b.output_config, reasoning: b.reasoning })),
    error: requests.length ? undefined : { protocolError, stderr: stderr.slice(-1500) } }));
}
let input = '';
process.stdin.on('data', chunk => input += chunk);
process.stdin.on('end', async () => {
  try { for (const spec of JSON.parse(input)) await run(spec); }
  catch (e) { console.error(e); process.exitCode = 1; }
});
