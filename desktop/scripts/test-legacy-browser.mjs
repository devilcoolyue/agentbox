#!/usr/bin/env node
// Frozen browser assets against the current real Linux HTTP/WebSocket server.
// The isolated compatibility runner supplies temporary credentials on stdin.
import assert from 'node:assert/strict';
import { readFile, realpath, mkdir, writeFile } from 'node:fs/promises';
import { resolve, extname, sep } from 'node:path';
import { pathToFileURL } from 'node:url';

let input='';
for await (const chunk of process.stdin) {
  input+=chunk;
  assert.ok(input.length<16384,'fixture input exceeds limit');
}
const config=JSON.parse(input);
input='';
const base=new URL(config.server);
assert.equal(base.protocol,'http:');
assert.equal(base.hostname,'127.0.0.1');
assert.ok(base.port&&base.pathname==='/'&&!base.username&&!base.password&&!base.search&&!base.hash);
assert.match(config.session,/^[a-zA-Z0-9_-]+$/);
const root=await realpath(config.static_root);
const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE?pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href:'playwright');
const browser=await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL?{channel:process.env.AGENTBOX_BROWSER_CHANNEL}:{})});
const errors=[],failures=[],consoleErrors=[];
const served=new Set();
const observed=new Set();
let terminalFrames=0;
const websocketPaths=[];
let page;
try {
  // This frozen frontend has no service worker. Playwright's block init script
  // itself throws when probing navigator.serviceWorker in its sandboxed preview
  // iframe, so use a fresh context and assert that no worker was registered.
  const context=await browser.newContext({viewport:{width:1440,height:960}});
  // Frozen HTML is fulfilled by Playwright, so Chrome 154 treats its address
  // space as public. Grant only this validated loopback fixture's permission;
  // otherwise local-network checks reject WS before any request reaches Go.
  await context.grantPermissions(['local-network-access'], {origin:base.origin});
  const workers=[];
  context.on('serviceworker',worker=>workers.push(worker.url()));
  page=await context.newPage();
  page.setDefaultTimeout(20000);
  page.on('pageerror',error=>errors.push(error.message));
  page.on('console',message=>{if(message.type()==='error')consoleErrors.push(message.text().replace(/([?&]token=)[^\s\"'&)]+/g,'$1[redacted]'));});
  page.on('response',response=>{
    const url=new URL(response.url());
    if(url.pathname.startsWith('/api/')) {
      observed.add(`${response.request().method()} ${url.pathname}`);
      if(response.status()>=400)failures.push(`${response.status()} ${url.pathname}`);
    }
  });
  page.on('websocket',socket=>{
    const path=new URL(socket.url()).pathname;
    websocketPaths.push(path);
    if(path===`/api/sessions/${config.session}/term`)
      socket.on('framereceived',()=>terminalFrames++);
  });
  await page.route('**/*',async route=>{
    const url=new URL(route.request().url());
    if(url.origin!==base.origin) {await route.abort();return;}
    // API and WS are never mocked. Only static assets are frozen, as with a
    // browser tab kept open across a server upgrade.
    if(url.pathname.startsWith('/api/')) {await route.continue();return;}
    if(route.request().method()!=='GET') {await route.abort();return;}
    const relative=decodeURIComponent(url.pathname).replace(/^\/_v\/[^/]+/,'');
    const path=resolve(root,'.'+(relative==='/'?'/index.html':relative));
    assert.ok(path.startsWith(root+sep),'static path escapes frozen tree');
    try {
      const actual=await realpath(path);
      assert.ok(actual.startsWith(root+sep),'static symlink escapes frozen tree');
      const body=await readFile(actual);
      served.add(relative);
      await route.fulfill({body,contentType:({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.woff2':'font/woff2'})[extname(actual)]||'application/octet-stream'});
    } catch(error) {
      failures.push(`frozen static missing: ${relative}`);
      await route.fulfill({status:404,body:'Missing frozen asset'});
    }
  });
  await page.goto(`${base}#/sessions/${config.session}/files`);
  await page.locator('#login-user').fill(config.username);
  await page.locator('#login-pass').fill(config.password);
  config.password='';
  await page.locator('#login-btn').click();
  await page.locator('#tab-files').waitFor({state:'visible'});
  assert.equal(await page.locator('#login-pass').inputValue(),'');
  const original='旧网页上传\r\nUTF-8 ✓\n';
  const filename='legacy-browser-中文.txt';
  await page.locator('#upload-input').setInputFiles({name:filename,mimeType:'text/plain',buffer:Buffer.from(original)});
  await page.locator('#files-list .flabel').filter({hasText:filename}).click();
  await page.locator('#fv-editor').waitFor({state:'visible'});
  // Textareas normalize CRLF in the DOM; raw bytes are checked on download.
  assert.equal(await page.locator('#fv-editor').inputValue(),original.replaceAll('\r\n','\n'));
  const changed='旧网页保存到新服务端 ✓\n';
  await page.locator('#fv-editor').fill(changed);
  await page.locator('#fv-save').click();
  await page.locator('#fv-state').filter({hasText:'已保存'}).waitFor();
  const downloaded=page.waitForEvent('download');
  await page.locator('#fv-download').click();
  const download=await downloaded;
  assert.equal(await download.failure(),null);
  assert.deepEqual(await readFile(await download.path()),Buffer.from(changed));
  await page.locator('#fv-close').click();
  await page.locator('[data-tab=term]').click();
  await page.locator('#term-state').filter({hasText:'已连接'}).waitFor();
  await page.locator('.xterm-helper-textarea').focus();
  // Keep the expected marker out of the PTY's command echo.
  await page.keyboard.type("printf '\\101\\102\\117\\130-browser-UTF8-'");
  await page.keyboard.insertText("'中文-✓\\n'");
  await page.keyboard.press('Enter');
  const stateURL=new URL('/_v/{{BUILD}}/js/state.js',base).href;
  await page.waitForFunction(async({stateURL})=>{
    const {S}=await import(stateURL);const t=S.term;if(!t)return false;
    return Array.from({length:t.buffer.active.length},(_,i)=>t.buffer.active.getLine(i)?.translateToString()||'').some(line=>line.includes('ABOX-browser-UTF8-中文-✓'));
  },{stateURL});
  // Browser rendering and CDP frame notifications use separate event queues.
  const frameDeadline=Date.now()+5000;
  while(!terminalFrames&&Date.now()<frameDeadline)await new Promise(resolve=>setTimeout(resolve,50));
  assert.ok(terminalFrames>0,`no real terminal websocket frames; paths=${JSON.stringify(websocketPaths)}`);
  for(const resource of ['/index.html','/js/main.js','/js/files.js','/js/term.js']) {
    // The root document is recorded by its requested slash.
    assert.ok(served.has(resource==='/index.html'?'/':resource),`frozen resource missing: ${resource}`);
  }
  assert.ok(observed.has('POST /api/login'));
  assert.ok(observed.has(`POST /api/sessions/${config.session}/upload`));
  assert.ok(observed.has(`PUT /api/sessions/${config.session}/file`));
  assert.deepEqual(errors,[],'browser script errors');
  assert.deepEqual(failures,[],'HTTP/static failures');
  assert.deepEqual(workers,[],'unexpected service worker bypasses frozen assets');
  console.log(JSON.stringify({legacy_browser:'passed',frozen_assets:served.size,real_api:true,real_terminal:true,upload_edit_download:true,model_calls:false}));
} catch (error) {
  const destination=resolve('output/playwright', 'legacy-browser-'+Date.now());
  await mkdir(destination,{recursive:true});
  await page?.screenshot({path:resolve(destination,'failure.png')});
  await writeFile(resolve(destination,'failure.json'),JSON.stringify({errors,failures,consoleErrors,websocketPaths,terminalFrames,terminalState:await page?.locator('#term-state').textContent().catch(()=>null)},null,2));
  console.error('Frozen browser evidence:',destination);
  throw error;
} finally {
  await browser.close();
}
