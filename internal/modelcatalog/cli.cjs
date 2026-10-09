// Lists the models the image's CLIs ship with. Run ONLY in a disposable
// network=none container without credentials: neither CLI is signed in, so
// both answer from their built-in catalogs. Stdout is one bounded JSON line;
// CLI stderr is never read or returned.
'use strict';
const fs = require('node:fs');
const {spawn} = require('node:child_process');
const readline = require('node:readline');
const LIMIT = 200;
const text = (v, n) => typeof v === 'string' ? v.slice(0, n) : '';
const list = v => Array.isArray(v) ? v.slice(0, 16) : [];
const base = {PATH: '/usr/local/bin:/usr/bin:/bin', LANG: 'C.UTF-8', DISABLE_AUTOUPDATER: '1', DISABLE_TELEMETRY: '1', DISABLE_ERROR_REPORTING: '1', CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1'};

function session(cmd, args, start, onMessage, ms) {
  return new Promise(resolve => {
    const home = fs.mkdtempSync('/tmp/agentbox-catalog-');
    for (const dir of ['.codex', '.claude']) fs.mkdirSync(home + '/' + dir);
    const env = {...base, HOME: home, CODEX_HOME: home + '/.codex', CLAUDE_CONFIG_DIR: home + '/.claude'};
    let child = null, settled = false;
    const finish = result => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try { if (child) child.kill('SIGKILL'); } catch {}
      resolve(result);
    };
    const timer = setTimeout(() => finish({error: 'timeout'}), ms);
    try {
      child = spawn(cmd, args, {cwd: home, env, stdio: ['pipe', 'pipe', 'ignore']});
    } catch {
      finish({error: 'spawn_failed'});
      return;
    }
    child.on('error', () => finish({error: 'spawn_failed'}));
    child.on('close', () => finish({error: 'exited'}));
    child.stdin.on('error', () => {});
    const send = value => child.stdin.write(JSON.stringify(value) + '\n');
    readline.createInterface({input: child.stdout}).on('line', line => {
      let message;
      try { message = JSON.parse(line); } catch { return; }
      try {
        const result = onMessage(message, send);
        if (result) finish(result);
      } catch {
        finish({error: 'protocol'});
      }
    });
    start(send);
  });
}

function claude() {
  const id = 'agentbox_catalog';
  return session('claude', ['-p', '--input-format', 'stream-json', '--output-format', 'stream-json', '--verbose'],
    send => send({type: 'control_request', request_id: id, request: {subtype: 'initialize'}}),
    m => {
      if (m.type !== 'control_response' || !m.response || m.response.request_id !== id) return null;
      if (m.response.subtype !== 'success') return {error: 'initialize'};
      const models = (m.response.response || {}).models;
      if (!Array.isArray(models)) return {error: 'models'};
      return {models: models.slice(0, LIMIT).map(x => ({
        value: text(x.value, 128), resolvedModel: text(x.resolvedModel, 128),
        displayName: text(x.displayName, 128), description: text(x.description, 300),
        supportsEffort: x.supportsEffort === true, supportedEffortLevels: list(x.supportedEffortLevels).map(l => text(l, 16)),
      }))};
    }, 30000);
}

function codex() {
  const models = [];
  let page = 0, cursor = '';
  const next = send => send({jsonrpc: '2.0', id: 2 + page, method: 'model/list', params: {limit: 100, includeHidden: true, ...(cursor ? {cursor} : {})}});
  return session('codex', ['app-server'],
    send => send({jsonrpc: '2.0', id: 1, method: 'initialize', params: {clientInfo: {name: 'agentbox-catalog', version: '1'}}}),
    (m, send) => {
      if (m.id === 1) {
        if (m.error) return {error: 'initialize'};
        send({jsonrpc: '2.0', method: 'initialized'});
        next(send);
        return null;
      }
      if (m.id !== 2 + page) return null;
      if (m.error || !m.result || !Array.isArray(m.result.data)) return {error: 'model_list'};
      for (const x of m.result.data) {
        if (models.length >= LIMIT) break;
        models.push({
          model: text(x.model || x.id, 128), displayName: text(x.displayName, 128), hidden: x.hidden === true,
          efforts: list(x.supportedReasoningEfforts).map(e => text(e && e.reasoningEffort, 16)),
        });
      }
      const more = typeof m.result.nextCursor === 'string' ? m.result.nextCursor : '';
      if (!more || more === cursor || ++page >= 10) return {models};
      cursor = more;
      next(send);
      return null;
    }, 30000);
}

Promise.all([claude(), codex()]).then(([c, x]) => {
  process.stdout.write(JSON.stringify({version: 1, claude: c, codex: x}) + '\n', () => process.exit(0));
});
