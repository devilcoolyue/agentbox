// Disposable native/Linux production server and synthetic Docker sidecar.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { resolve, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createServer } from 'node:net';
import { randomBytes } from 'node:crypto';

export const sleep = ms => new Promise(r => setTimeout(r, ms));
export async function until(fn, message, timeout = 15000) {
 const deadline = Date.now() + timeout; let last;
 while (Date.now() < deadline) { try { const value = await fn(); if (value) return value; } catch (error) { last = error; } await sleep(50); }
 throw Error(message + (last ? ': ' + last.message : ''));
}
export async function harness() {
 const root = fileURLToPath(new URL('../', import.meta.url));
 const linux = process.argv.includes('--linux');
 const image = process.argv.includes('--image') ? process.argv[process.argv.indexOf('--image') + 1] : '';
 if (linux && !image) throw Error('--linux requires an explicit local --image');
 if (!linux && process.platform === 'linux' && process.getuid() !== 0) throw Error('Linux writes require root in a disposable runner; use --linux on a Docker-capable host.');
 const temp = await mkdtemp(join(tmpdir(), 'agentbox-chat-integration-'));
 const runID = randomBytes(6).toString('hex'), controlToken = randomBytes(24).toString('hex');
 const evidence = resolve(root, 'output/playwright/chat-integration-' + (linux ? 'linux-' : 'native-') + runID);
 await mkdir(evidence, { recursive: true });
 const childEnv = { ...process.env };
 for (const key of Object.keys(childEnv)) if (/^(DOCKER_|AGENTBOX_|CODEX_|CLAUDE_|ANTHROPIC_|OPENAI_|HTTP_PROXY$|HTTPS_PROXY$|ALL_PROXY$|http_proxy$|https_proxy$|all_proxy$)/.test(key)) delete childEnv[key];
 const processes = new Set(), containers = new Set();
 let serverProcess, engineProcess, base, engineBase, network, volume, cleaning;
 let serverLogs = '', engineLogs = '';
 async function freePort() { const socket = createServer(); await new Promise(r => socket.listen(0, '127.0.0.1', r)); const port = socket.address().port; await new Promise(r => socket.close(r)); return port; }
 const report = { mode: linux ? 'Linux containers' : process.platform, boundary: 'Real binary, SQLite, HTTP and WebSocket; synthetic Docker and Codex exec output, no provider', checks: [] };
 function command(bin, args, env = childEnv) {
  return new Promise((resolve, reject) => {
   const p = spawn(bin, args, { cwd: root, env, stdio: ['ignore', 'pipe', 'pipe'] }); processes.add(p);
   let out = '', err = ''; p.stdout.on('data', b => out += b); p.stderr.on('data', b => err += b);
   p.on('error', reject); p.on('exit', (code, signal) => { processes.delete(p); code === 0 ? resolve((out + err).trim()) : reject(Error(`${bin} ${args.join(' ')} exited ${code ?? signal}: ${err || out}`)); });
  });
 }
 const docker = (...args) => command('docker', args);
 const mount = () => ['-v', `${temp}:/bundle:ro`, '-v', `${volume}:/fixture`];
 async function ctl(path, body) {
  const res = await fetch(engineBase + '/control/' + path, { method: body === undefined ? 'GET' : 'POST', headers: { Authorization: 'Bearer ' + controlToken, 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(10000) });
  assert.equal(res.status, 200, 'fixture control ' + path); return res.json();
 }
 async function start() {
  if (linux) {
   const name = 'abox-chat-server-' + runID;
   if (containers.has(name)) await docker('start', name);
   else { const port = await freePort(); containers.add(name); await docker('run', '-d', '--name', name, '--network', network, '-p', `127.0.0.1:${port}:8180`, ...mount(), '-e', 'DOCKER_HOST=tcp://engine:8081', '-e', 'DOCKER_API_VERSION=1.45', '--entrypoint', '/bundle/agentbox', image, '-config', '/fixture/config.json'); }
   const address = 'http://' + (await docker('port', name, '8180/tcp')).trim();
   if (base) assert.equal(address, base, 'server restart changed the browser origin');
   base = address;
  } else {
   const p = spawn(join(temp, 'agentbox'), ['-config', join(temp, 'data/config.json')], { env: { ...childEnv, DOCKER_HOST: engineBase.replace('http:', 'tcp:'), DOCKER_API_VERSION: '1.45' }, stdio: ['ignore', 'pipe', 'pipe'] });
   serverProcess = p; processes.add(p); p.stdout.on('data', b => serverLogs += b); p.stderr.on('data', b => serverLogs += b); p.on('exit', () => processes.delete(p));
  }
  await until(async () => (await fetch(base + '/api/ping', { signal: AbortSignal.timeout(500) })).ok, 'real server did not become ready');
 }
 async function stop(crash = false) {
  if (linux) {
   if (crash) await docker('kill', '--signal', 'KILL', 'abox-chat-server-' + runID);
   else await docker('stop', '--time', '15', 'abox-chat-server-' + runID);
   if (!crash) assert.equal(await docker('inspect', '--format', '{{.State.ExitCode}}', 'abox-chat-server-' + runID), '0');
  } else if (serverProcess && serverProcess.exitCode === null && serverProcess.signalCode === null) {
   const p = serverProcess; p.kill(crash ? 'SIGKILL' : 'SIGTERM');
   await until(() => p.exitCode !== null || p.signalCode !== null, 'server did not exit', 18000);
   if (!crash) assert.equal(p.exitCode, 0, 'graceful server exit');
  }
 }
 async function cleanup() {
  if (cleaning) return cleaning;
  cleaning = (async () => {
   const cleanupErrors = [];
   const remove = async (...args) => { try { await docker(...args); } catch (error) { if (!/No such (container|volume|network)|not found/.test(error.message)) cleanupErrors.push(error.message); } };
   if (linux) {
    if (containers.has('abox-chat-server-' + runID)) serverLogs = await docker('logs', 'abox-chat-server-' + runID).catch(String);
    if (containers.has('abox-chat-engine-' + runID)) engineLogs = await docker('logs', 'abox-chat-engine-' + runID).catch(String);
    for (const name of containers) await remove('rm', '-f', name);
    if (volume) await remove('volume', 'rm', volume);
    if (network) await remove('network', 'rm', network);
   }
   const children = [...processes];
   for (const p of children) p.kill('SIGKILL');
   await Promise.all(children.map(p => until(() => p.exitCode !== null || p.signalCode !== null || !p.pid, 'child cleanup timeout', 5000).catch(error => cleanupErrors.push(error.message))));
   report.cleanup = cleanupErrors.length ? cleanupErrors : 'passed';
   if (cleanupErrors.length) report.status = 'failed';
   await writeFile(join(evidence, 'server.log'), serverLogs); await writeFile(join(evidence, 'engine.log'), engineLogs);
   await writeFile(join(evidence, 'report.json'), JSON.stringify(report, null, 2) + '\n');
   await rm(temp, { recursive: true, force: true });
   if (cleanupErrors.length) throw Error('fixture cleanup failed: ' + cleanupErrors.join('; '));
  })(); return cleaning;
 }
 for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => { void cleanup().finally(() => process.exit(1)); });
 try {
  let buildEnv = childEnv;
  if (linux) {
   await docker('image', 'inspect', image); const arch = await docker('info', '--format', '{{.Architecture}}');
   buildEnv = { ...childEnv, CGO_ENABLED: '0', GOOS: 'linux', GOARCH: { aarch64: 'arm64', x86_64: 'amd64' }[arch] || arch };
  }
  await Promise.all([command('go', ['build', '-o', join(temp, 'agentbox'), './cmd/agentbox'], buildEnv), command('go', ['build', '-o', join(temp, 'fixture'), './scripts/fixtures/chat-integration'], buildEnv)]);
  if (linux) {
   network = 'abox-chat-network-' + runID; volume = 'abox-chat-data-' + runID;
   await docker('network', 'create', network); await docker('volume', 'create', volume);
   await docker('run', '--rm', '--network', 'none', ...mount(), '--entrypoint', '/bundle/fixture', image, '-mode', 'seed', '-dir', '/fixture', '-listen', '0.0.0.0:8180');
   const name = 'abox-chat-engine-' + runID; containers.add(name);
   await docker('run', '-d', '--name', name, '--network', network, '--network-alias', 'engine', '-p', '127.0.0.1::8081', ...mount(), '--entrypoint', '/bundle/fixture', image, '-mode', 'engine', '-dir', '/fixture', '-listen', '0.0.0.0:8081', '-token', controlToken);
   engineBase = 'http://' + (await docker('port', name, '8081/tcp')).trim();
  } else {
   const socket = createServer(); await new Promise(r => socket.listen(0, '127.0.0.1', r)); const port = socket.address().port; await new Promise(r => socket.close(r));
   base = 'http://127.0.0.1:' + port;
   await command(join(temp, 'fixture'), ['-mode', 'seed', '-dir', join(temp, 'data'), '-listen', '127.0.0.1:' + port]);
   engineProcess = spawn(join(temp, 'fixture'), ['-mode', 'engine', '-dir', join(temp, 'data'), '-token', controlToken], { env: childEnv, stdio: ['ignore', 'pipe', 'pipe'] }); processes.add(engineProcess);
   let ready = ''; engineProcess.stdout.on('data', b => ready += b); engineProcess.stderr.on('data', b => engineLogs += b);
   await until(() => ready.includes('\n'), 'synthetic engine readiness'); engineBase = JSON.parse(ready.split('\n')[0]).url;
  }
  await start();
 } catch (error) { report.status = 'failed'; report.error = error.stack; await cleanup(); throw error; }
 return { get base() { return base; }, evidence, report, start, stop, ctl, cleanup };
}
