#!/usr/bin/env node
// Real xterm -> Docker PTY -> tmux -> Vim; synthetic API, no accounts/provider calls.
// Build images/agent first; AGENTBOX_TERMINAL_IMAGE selects the image under test.
import {reduceMotion} from './playwright-launch.mjs';
import assert from 'node:assert/strict';
import { createServer, request } from 'node:http';
import { execFile, execFileSync } from 'node:child_process';
import { promisify } from 'node:util';
import { readFile, mkdir } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { pathToFileURL } from 'node:url';

const image = process.env.AGENTBOX_TERMINAL_IMAGE || 'agentbox-agent:latest';
const endpoint = process.env.DOCKER_HOST || execFileSync('docker', ['context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'], {encoding:'utf8'}).trim();
assert.ok(endpoint.startsWith('unix://'), 'This local integration test requires a Unix Docker socket');
const socketPath = endpoint.slice(7);
const docker = (path, body = {}) => new Promise((resolve, reject) => {
 const req = request({socketPath, path:'/v1.41'+path, method:'POST', headers:{'Content-Type':'application/json'}}, res => {
  let text=''; res.on('data', chunk => text+=chunk); res.on('end', () => {
   if (res.statusCode >= 300) reject(new Error(`${path}: ${res.statusCode} ${text}`));
   else resolve(text ? JSON.parse(text) : {});
  });
 });
 req.on('error', reject); req.end(JSON.stringify(body));
});
const container = execFileSync('docker', ['run','-d','--rm','--network=none',
 '--tmpfs','/home/agent:uid=1000,gid=1000','--tmpfs','/workspace:uid=1000,gid=1000',image], {encoding:'utf8'}).trim();
const readContainerFile = async path => {
 for(let attempt=0; ; attempt++) {
  try {return (await promisify(execFile)('docker',['exec',container,'cat',path],{encoding:'utf8'})).stdout;}
  catch(err){if(attempt>=20)throw err;await new Promise(r=>setTimeout(r,50));}
 }
};
const sockets = new Set(), failures = [], errors = [];
const root = resolve('internal/web/static');
const server = createServer(async (req,res) => {
 try {
  const rel = decodeURIComponent(req.url.split('?')[0]).replace(/^\/_v\/[^/]+/, '');
  const path = resolve(root, '.'+(rel==='/' ? '/index.html' : rel));
  if (!path.startsWith(root+'/')) {res.writeHead(403).end();return;}
  const data = await readFile(path);
  res.setHeader('Content-Type', ({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(path)] || 'application/octet-stream');
  res.end(data);
 } catch {res.writeHead(404).end();}
});
let browser, page;
try {
 await new Promise(r=>server.listen(0,'127.0.0.1',r));
 const {chromium} = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
 browser = reduceMotion(await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL ? {channel:process.env.AGENTBOX_BROWSER_CHANNEL}: {})}));
 page = await browser.newPage({viewport:{width:1440,height:960}});
 page.setDefaultTimeout(15000);
 page.on('pageerror', err=>errors.push(err.message));
 await page.addInitScript(()=>{if(window===window.top)localStorage.setItem('agentbox_token','synthetic-terminal-token');});
 await page.route('**/api/**', async route => {
  const path = new URL(route.request().url()).pathname;
  const session = {id:'terminal-fixture',name:'Terminal fixture',agent:'claude',account_id:'fixture',account_label:'Fixture',status:'running',default_model:'fixture'};
  let body = {};
  if(path==='/api/me') body={user:'fixture',role:'user',timezone:'Asia/Shanghai',models:{claude:[],codex:[]},quota:{metered:false}};
  else if(path==='/api/sessions') body=[session];
  else if(path==='/api/accounts') body=[];
  else if(path.endsWith('/history')) body={entries:[],thread:null,costs:{}};
  else if(path.endsWith('/models')) body={models:[]};
  else if(path==='/api/usage/events') body={rows:[],total:{cost_micro_usd:0,input_tokens:0,output_tokens:0}};
  await route.fulfill({json:body});
 });
 await page.routeWebSocket('**/api/sessions/*/chat?*', () => {});
 let connections=0;
 await page.routeWebSocket('**/api/sessions/*/term?*', async ws => {
  let socket, closed=false, pending=[];
  const created = docker(`/containers/${container}/exec`, {User:'1000:1000',Tty:true,AttachStdin:true,AttachStdout:true,AttachStderr:true,
   WorkingDir:'/workspace',Env:['TERM=xterm-256color'],Cmd:['/bin/bash','-c','exec tmux -u new-session -A -D -s main']});
  // Serialize resize controls just as the Go read loop does; never lose onopen's first size.
  let control = Promise.resolve();
  ws.onMessage(data => {
   if (typeof data==='string') {
    const msg=JSON.parse(data);
    if(msg.type==='resize') control=control.then(async()=>{
     const {Id}=await created;
     await attached;
     await docker(`/exec/${Id}/resize?w=${msg.cols}&h=${msg.rows}`);
    }).catch(err=>failures.push(err.message));
   } else if(socket) socket.write(data); else pending.push(data);
  });
  ws.onClose(()=>{closed=true;socket?.destroy();});
  const attached = created.then(({Id})=>new Promise((resolve,reject)=>{
   const req=request({socketPath,path:`/v1.41/exec/${Id}/start`,method:'POST',headers:{'Content-Type':'application/json',Connection:'Upgrade',Upgrade:'tcp'} });
   req.on('upgrade',(_res,conn,head)=>{
    socket=conn;sockets.add(conn);connections++;
    if(closed){conn.destroy();resolve();return;}
    if(head.length)ws.send(head);
    conn.on('data',d=>{if(!closed)ws.send(d);});
    conn.on('close',()=>{sockets.delete(conn);if(!closed)ws.close({code:1000,reason:'process exited'});});
    conn.on('error',err=>failures.push(err.message));
    for(const data of pending)conn.write(data);pending=[];
    resolve();
   });
   req.on('response',res=>{res.resume();reject(new Error(`PTY attach HTTP ${res.statusCode}`));});
   req.on('error',reject);req.end(JSON.stringify({Tty:true,Detach:false}));
  }));
  await attached.catch(err=>failures.push(err.message));
 });
 await page.goto(`http://127.0.0.1:${server.address().port}/#/sessions/terminal-fixture/term`);
 const stateURL='/_v/{{BUILD}}/js/state.js';
 await page.evaluate(async url=>{window.terminalState=(await import(url)).S;},stateURL);
 const waitScreen = async text => {
  await page.waitForFunction(({text})=>{
   const t=window.terminalState.term;if(!t)return false;const b=t.buffer.active;
   return Array.from({length:t.rows},(_,i)=>b.getLine(b.viewportY+i)?.translateToString(true)||'').join('\n').includes(text);
  },{url:stateURL,text});
 };
 await waitScreen('/workspace$');
 assert.equal(await page.evaluate(()=>{
  const b=window.terminalState.term.buffer.active;
  return b.getLine(b.baseY+b.cursorY).getCell(0).getFgColor();
 }),2,'shell username should use ANSI green');
 await page.locator('.xterm-helper-textarea').focus();
 const command = async text=>{await page.keyboard.type(text);await page.keyboard.press('Enter');};
 const assertCursor = async () => {
  await page.waitForFunction(()=>{
   const canvas=document.querySelector('.xterm-screen').getBoundingClientRect();
   return canvas.bottom<=document.querySelector('#term-mount').getBoundingClientRect().bottom+1;
  });
  const c=await page.evaluate(async url=>{
   const t=(await import(url)).S.term;
   const canvas=document.querySelector('.xterm-screen').getBoundingClientRect();
   const mount=document.querySelector('#term-mount').getBoundingClientRect();
   return {hidden:t._core.coreService.isCursorHidden,y:t.buffer.active.cursorY,rows:t.rows,bottom:canvas.bottom,mountBottom:mount.bottom};
  },stateURL);
  assert.equal(c.hidden,false,'Vim cursor hidden');assert.ok(c.y<c.rows,'cursor outside terminal');
  assert.ok(c.bottom<=c.mountBottom+1,`last terminal row is clipped: ${JSON.stringify(c)}`);
 };
 await command('ll');await waitScreen('total');
 for(const editor of ['vi','vim']) {
  await command(`${editor} /workspace/${editor}.txt`);
  await page.waitForFunction(()=>window.terminalState.term.buffer.active.type==='alternate');
  await command(`:call writefile([string(&compatible), &backspace, string(&showmode), &mouse], "/workspace/${editor}-options.txt")`);
  const options=await readContainerFile(`/workspace/${editor}-options.txt`);
  assert.equal(options,'0\nindent,eol,start\n1\n\n','system Vim defaults were overridden');
  await page.keyboard.press('i');await waitScreen('-- INSERT --');
  await page.keyboard.type('helloX');await page.keyboard.press('Backspace');
  await page.keyboard.press('ArrowLeft');await page.keyboard.type('!');
  await page.keyboard.press('Escape');await page.keyboard.type(':');
  await page.waitForFunction(()=>{
   const t=window.terminalState.term;return t.buffer.active.getLine(t.rows-1)?.translateToString(true)===':';
  },stateURL);
  await assertCursor();
  if(editor==='vi') {
   // Reattach while a fullscreen editor is waiting for an Ex command.
   const before=connections;
   await page.locator('#term-reconnect').click();
   const deadline=Date.now()+10000;
   while(connections<=before && Date.now()<deadline) await new Promise(r=>setTimeout(r,50));
   assert.ok(connections>before,'new PTY did not attach');
   await page.waitForFunction(()=>window.terminalState.term.buffer.active.type==='alternate');
   await page.locator('.xterm-helper-textarea').focus();
   await page.setViewportSize({width:1000,height:720});
   await page.waitForFunction(()=>{const t=window.terminalState.term;return t.buffer.active.cursorY===t.rows-1;});
   await assertCursor();
   await mkdir('output/playwright',{recursive:true});
   await page.screenshot({path:'output/playwright/terminal-vim-command.png'});
  }
  await command('wq');await waitScreen('/workspace$');
  const saved=execFileSync('docker',['exec',container,'cat',`/workspace/${editor}.txt`],{encoding:'utf8'});
  assert.equal(saved,'hell!o\n',`${editor} input differs from saved file`);
 }
 assert.ok(connections>=2,'reconnect did not attach a new PTY');
 await page.setViewportSize({width:1440,height:960});
 await command('clear; ll; printf "TERMINAL_READY\\n"');
 await page.waitForFunction(()=>{
  const t=window.terminalState.term,b=t.buffer.active;
  return Array.from({length:t.rows},(_,i)=>b.getLine(b.viewportY+i)?.translateToString(true)).includes('TERMINAL_READY');
 });
 await page.screenshot({path:'output/playwright/terminal-shell.png'});
 assert.deepEqual(failures,[]);assert.deepEqual(errors,[]);
 console.log('Terminal: empty home, colored prompt, ll, vi/vim insert/arrows/backspace/colon/save, visible cursor, resize and tmux reconnect passed');
} catch(err) {
 console.error('Bridge errors:',failures,'Page errors:',errors);
 if(page){
  console.error(await page.locator('body').innerText());
  await mkdir('output/playwright',{recursive:true});
  await page.screenshot({path:'output/playwright/terminal-failure.png'});
 }
 throw err;
} finally {
 await browser?.close();
 for(const socket of sockets)socket.destroy();
 server.closeAllConnections();await new Promise(r=>server.close(r));
 execFileSync('docker',['rm','-f',container],{stdio:'ignore'});
}
