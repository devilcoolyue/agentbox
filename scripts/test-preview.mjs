#!/usr/bin/env node
// Source preview regression with synthetic files; no Docker or credentials.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

export async function smoke(page) {
 await page.addInitScript(() => { if (window === window.top && /^https?:$/.test(location.protocol)) localStorage.setItem("agentbox.language", "zh-CN"); });
 const root = resolve(fileURLToPath(new URL('../internal/web/static/', import.meta.url)));
 const server = createServer(async (req, res) => {
  try {
   const rel = decodeURIComponent(req.url.split('?')[0]).replace(/^\/_v\/[^/]+/, '');
   const path = resolve(root, '.' + (rel === '/' ? '/index.html' : rel));
   if (!path.startsWith(root + '/')) { res.writeHead(403).end(); return; }
   let data = await readFile(path);
   // Isolate the preview module from unrelated application startup/API calls.
   if (rel === '/') data = data.toString().replace(/<script type="module" src="[^\"]*\/js\/main.js"><\/script>/, '');
   res.setHeader('Content-Type', ({'.js':'text/javascript','.css':'text/css','.html':'text/html'})[extname(path)] || 'application/octet-stream');
   res.end(data);
  } catch { res.writeHead(404).end(); }
 });
 await new Promise(r => server.listen(0, '127.0.0.1', r));
 const base = `http://127.0.0.1:${server.address().port}`;
 const errors = []; page.on('pageerror', e => errors.push(e.message));
 let content = '', saved = '';
 const documents = new Map(), requestedFiles = [];
 const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="96" height="20"><rect width="96" height="20" fill="#9a701d"/><text x="8" y="14" fill="white" font-size="12">Fixture badge</text></svg>';
 await page.route('https://img.shields.io/**', route=>route.fulfill({body:svg,contentType:'image/svg+xml'}));
 await page.route('**/api/**', async route => {
  const url = new URL(route.request().url());
  if (url.pathname.endsWith('/file')) {
   if (route.request().method() === 'PUT') { saved = route.request().postData(); await route.fulfill({json:{ok:true}}); }
   else {
    const file = url.searchParams.get('path'); requestedFiles.push({file,scope:url.searchParams.get('scope')});
    const images = ['internal/web/static/img/logo.svg','docs/images/chat-light.png','docs/images/chat-dark.png'];
    if(images.includes(file)) await route.fulfill({body:await readFile(resolve(root,'../../..',file)),contentType:file.endsWith('.svg')?'image/svg+xml':'image/png'});
    else if(file?.endsWith('.svg')) await route.fulfill({body:svg,contentType:'image/svg+xml'});
    else await route.fulfill({body:documents.get(file)||content,contentType:'text/plain'});
   }
  } else if (url.pathname.endsWith('/preview')) await route.fulfill({json:{url:'/fixture-preview'}});
  else await route.fulfill({json:[]});
 });
 await page.route('**/fixture-preview*', route => route.fulfill({contentType:'text/html',body:'<!doctype html><title>Fixture</title>'}));
 const open = async (name, text, scope = 'workspace') => {
  content = text;
  await page.evaluate(async ({name, size, scope}) => {
   const {S} = await import('/js/state.js'); S.current = {id:'fixture',agent:'codex'}; S.token = 'synthetic';
   const {decorateIcons} = await import('/js/icons.js'); decorateIcons();
   const {openPreview} = await import('/js/preview.js');
   await openPreview(name, {name,size,mtime:'',mode:'0644',is_dir:false}, scope);
  }, {name,size:Buffer.byteLength(text),scope});
 };
 const close = () => page.locator('#fv-close').click();
 try {
  await page.setViewportSize({width:1280,height:900});
  await page.goto(base);
  await page.evaluate(() => document.documentElement.dataset.theme='dark');
  const source = '// 文件预览：支持中文与缩进\nexport function greet(name: string) {\n\tconst message = "你好，" + name;\n\treturn message;\n}\n';
  await open('example.ts', source);
  assert.equal(await page.locator('#fv-editor').inputValue(),source);
  assert.equal(await page.locator('#fv-language').innerText(),'TypeScript');
  assert.match(await page.locator('#fv-position').innerText(),/共 6 行/);
  assert.ok(await page.locator('#fv-highlight .token.keyword').count());
  assert.equal(await page.locator('#fv-full').isVisible(),true);
  await page.locator('#fv-editor').click();
  await page.locator('#fv-editor').evaluate(e=>e.setSelectionRange(0,0));
  await page.locator('#fv-editor').press('ArrowDown');
  assert.match(await page.locator('#fv-position').innerText(),/第 2 行/);
  await page.locator('#fv-editor').press('Tab');
  assert.equal(await page.locator('#fv-save').isEnabled(),true);
  const edited = await page.locator('#fv-editor').inputValue();
  await page.locator('#fv-save').click();
  assert.equal(saved,edited,'save must use raw source, without line numbers or markup');
  await page.waitForFunction(() => document.querySelector('#fv-highlight').textContent === document.querySelector('#fv-editor').value + '\n');
  await page.waitForFunction(() => document.querySelector('#fv-highlight .token.keyword'));
  await mkdir('output/playwright',{recursive:true});
  await page.screenshot({path:'output/playwright/source-preview-dark.png'});
  await page.evaluate(() => document.documentElement.dataset.theme='light');
  await page.screenshot({path:'output/playwright/source-preview-light.png'});
  await page.setViewportSize({width:390,height:844});
  assert.ok(await page.locator('#fv-editor').evaluate(e=>e.clientWidth>200));
  assert.ok(await page.locator('#dlg-file').evaluate(e=>e.getBoundingClientRect().right<=innerWidth));
  assert.ok(await page.evaluate(()=>document.querySelector('.fv-title').getBoundingClientRect().bottom<=document.querySelector('.fv-actions').getBoundingClientRect().top),'mobile title and actions must not overlap');
  await page.screenshot({path:'output/playwright/source-preview-mobile.png'});
  await page.emulateMedia({forcedColors:'active'});
  assert.notEqual(await page.locator('#fv-editor').evaluate(e=>getComputedStyle(e).color),'rgba(0, 0, 0, 0)');
  await page.emulateMedia({forcedColors:'none'});
  await close();
  const html = '<!DOCTYPE html>\n<html lang="zh-CN">\n<head>\n  <meta charset="UTF-8">\n  <meta name="viewport" content="width=device-width, initial-scale=1.0">\n  <title>源码预览 · SVG 动画</title>\n  <style>\n    body { margin: 0; color: #123456; }\n  </style>\n</head>\n<body>\n' + '<div class="stage">中文与嵌套 <span>标签</span></div>\n'.repeat(60) + '</body>\n</html>\n';
  await open('fixture.html',html);
  await page.locator('#fv-mode-src').click();
  // Compare actual glyph positions with an unstyled text mirror, not just the
  // parent's line-height: global .tag badge styles can distort nested tokens.
  const checkGlyphs = async () => {
   const differences = await page.locator('#fv-highlight').evaluate(highlight => {
    const plain = highlight.cloneNode(false); plain.removeAttribute('id');
    plain.textContent = highlight.textContent; plain.style.visibility='hidden';
    highlight.parentElement.append(plain);
    plain.scrollTop=highlight.scrollTop; plain.scrollLeft=highlight.scrollLeft;
    const rects = root => {
     const walker=document.createTreeWalker(root,NodeFilter.SHOW_TEXT), result=[];
     let node;
     while ((node=walker.nextNode())) {
      for(let i=0;i<node.length;i++) {
       if (/\s/.test(node.data[i])) continue;
       const range=document.createRange(); range.setStart(node,i); range.setEnd(node,i+1);
       const r=range.getBoundingClientRect(); result.push({char:node.data[i],x:r.x,y:r.y});
      }
     }
     return result;
    };
    const expected=rects(plain), actual=rects(highlight);
    plain.remove();
    return actual.flatMap((r,i)=>Math.abs(r.x-expected[i].x)>0.5 || Math.abs(r.y-expected[i].y)>0.5 ? [{i,actual:r,expected:expected[i]}] : []).slice(0,3);
   });
   assert.deepEqual(differences,[],'syntax tokens must preserve exact source glyph positions');
  };
  await page.setViewportSize({width:1280,height:900});
  for (const theme of ['light','dark']) {
   await page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
   await checkGlyphs();
   await page.locator('#fv-editor').evaluate(e=>{
    e.focus();e.setSelectionRange(e.value.indexOf('  <meta name'),e.value.indexOf('  <meta name'));
    e.dispatchEvent(new Event('select'));
   });
   assert.match(await page.locator('#fv-position').innerText(),/第 5 行，第 1 列/);
   await page.screenshot({path:`output/playwright/source-preview-html-${theme}.png`});
  }
  await page.setViewportSize({width:390,height:844});
  await page.locator('#fv-editor').evaluate(e=>{e.scrollTop=450;e.scrollLeft=150;e.dispatchEvent(new Event('scroll'));});
  await checkGlyphs();
  await close();
  await page.setViewportSize({width:1280,height:900});
  const long = Array.from({length:450},(_,i)=>`const line${i} = "${'x'.repeat(180)}";`).join('\n')+'\n';
  await open('long.js',long);
  await page.locator('#fv-editor').evaluate(e=>{e.scrollTop=7000;e.scrollLeft=650;e.dispatchEvent(new Event('scroll'));});
  assert.deepEqual(await page.evaluate(()=>{
   const e=document.querySelector('#fv-editor'),h=document.querySelector('#fv-highlight');
   return [e.scrollTop===h.scrollTop,e.scrollLeft===h.scrollLeft,e.clientWidth===h.clientWidth];
  }),[true,true,true]);
  assert.ok(await page.locator('#fv-gutter span').count()<45,'gutter must only render visible lines');
  assert.ok(Number(await page.locator('#fv-gutter span').first().innerText())>300);
  // Browser-computed line box must match the textarea after horizontal/vertical scrolling.
  assert.equal(await page.locator('#fv-editor').evaluate(e=>getComputedStyle(e).lineHeight),await page.locator('#fv-highlight').evaluate(e=>getComputedStyle(e).lineHeight));
  await page.locator('#fv-full').click();
  await page.waitForFunction(()=>document.querySelector('#fv-highlight').clientWidth===document.querySelector('#fv-editor').clientWidth);
  await close();
  await open('empty.txt','');
  assert.equal(await page.locator('#fv-position').innerText(),'第 1 行，第 1 列 · 共 1 行');
  await close();
  await open('windows.go','package main\r\n\r\nfunc main() {}\r\n');
  assert.match(await page.locator('#fv-position').innerText(),/共 4 行/);
  await close();
  await open('README.md','# 标题\n\n<script>window.previewInjected = true</script>\n');
  await page.locator('#fv-mode-src').click();
  assert.equal(await page.locator('#fv-source').isVisible(),true);
  assert.equal(await page.locator('#fv-highlight script').count(),0);
  assert.equal(await page.evaluate(()=>window.previewInjected),undefined);
  await page.locator('#fv-mode-view').click();
  assert.equal(await page.locator('#fv-mdwrap').isVisible(),true);
  for (const width of [360,390,430,768,1280,1440]) {
   await page.setViewportSize({width,height:900});
   for (const theme of ['dark','light']) {
    await page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
    assert.ok(await page.locator('.fv-head').evaluate(e=>e.scrollWidth<=e.clientWidth),'preview toolbar overflow');
    if(width<=760) {
     assert.ok((await page.locator('.fv-head').boundingBox()).height<=124,'mobile preview header too tall');
     await page.locator('#fv-more').click();
     assert.equal(await page.locator('#fv-download').isVisible(),true);
     assert.equal(await page.locator('#fv-auto').isVisible(),true);
     await page.locator('#fv-auto').uncheck();
     await page.keyboard.press('Escape');
     assert.equal(await page.locator('#dlg-file').isVisible(),true,'Escape in menu closed preview');
     await page.locator('#fv-more').click();
     assert.equal(await page.locator('#fv-auto').isChecked(),false);
     await page.locator('#fv-auto').check();
     await page.locator('#fv-name').click();
     assert.equal(await page.locator('#fv-more-panel').isVisible(),false);
    }
    await page.screenshot({animations:'disabled',path:`output/playwright/responsive-preview-${width}-${theme}.png`});
   }
  }
  await close();
  // Actual README: HTML layout, nested badge links, picture sources and GFM tables.
  const readme = await readFile(resolve(root,'../../../README.md'),'utf8');
  await page.setViewportSize({width:1280,height:900});
  await page.evaluate(()=>document.documentElement.dataset.theme='light');
  await open('README.md',readme);
  assert.equal(await page.locator('#fv-md h1').first().innerText(),'agentbox');
  assert.match(await page.locator('#fv-md > div[align="center"]').first().evaluate(e=>getComputedStyle(e).textAlign),/center/);
  assert.equal(await page.locator('#fv-md a > img').count()>=4,true,'nested badge links');
  assert.ok(await page.locator('#fv-md table').count());
  assert.ok(await page.locator('#fv-md br').count());
  assert.ok(!(await page.locator('#fv-md').innerText()).includes('<div align='));
  assert.ok(!(await page.locator('#fv-md').innerText()).includes('![Release]'));
  assert.equal(await page.locator('#fv-md img[alt="agentbox logo"]').evaluate(e=>e.getBoundingClientRect().width),96);
  await page.locator('#fv-md img[alt="agentbox logo"]').evaluate(img=>img.decode());
  const hash=await page.evaluate(()=>location.hash);
  await page.locator('#fv-md a').filter({hasText:'Quick start'}).first().click();
  assert.ok(await page.locator('#fv-mdwrap').evaluate(e=>e.scrollTop>100));assert.equal(await page.evaluate(()=>location.hash),hash);
  await page.locator('#fv-mdwrap').evaluate(e=>e.scrollTop=0);
  await page.screenshot({path:'output/playwright/markdown-readme-light.png'});
  await page.evaluate(()=>document.documentElement.dataset.theme='dark');
  await page.screenshot({path:'output/playwright/markdown-readme-dark.png'});
  await page.setViewportSize({width:390,height:844});
  assert.ok(await page.locator('#fv-mdwrap').evaluate(e=>e.scrollWidth<=e.clientWidth));
  await page.screenshot({path:'output/playwright/markdown-readme-mobile.png'});
  documents.set('README_CN.md','# 中文文档\n\n跳转成功');
  await page.locator('#fv-md a').filter({hasText:'简体中文'}).first().click();
  await page.waitForFunction(()=>document.querySelector('#fv-md h1')?.textContent==='中文文档');
  await close();
  // Raw HTML is display content, never an application control or executable document.
  const hostile = '# Safe\n\n<script>window.previewInjected=true</script>\n<iframe src="/api/me"></iframe><style>body{display:none}</style><form id="fv-save"><input name="location" value="bad"></form>\n<div class="hidden" id="chat-send" style="position:fixed" onclick="window.previewInjected=true">Visible text</div>\n<img src="bad.svg" onerror="window.previewInjected=true"><img src="/api/me"><img src="../../../escape.svg">\n<a href="javascript:window.previewInjected=true">Unsafe</a><a href="/api/me">API</a>\n<svg onload="window.previewInjected=true"><a href="javascript:alert(1)">svg</a></svg>\n\n- [x] Done\n- [ ] Todo\n\n```html\n<div class="hidden">literal</div>\n```';
  await open('docs/safe.md',hostile);
  assert.equal(await page.locator('#fv-md script, #fv-md iframe, #fv-md style, #fv-md form, #fv-md svg, #fv-md [onclick], #fv-md [onerror], #fv-md [name], #fv-md [id]').count(),0);
  assert.equal(await page.locator('#fv-md input:enabled').count(),0);assert.equal(await page.locator('#fv-md input[type="checkbox"]').count(),2);
  assert.equal(await page.locator('#fv-md a[href]').count(),0);
  assert.equal(await page.locator('#fv-md .hidden').count(),0);
  assert.equal(await page.locator('#fv-md pre').innerText(),'<div class="hidden">literal</div>\n');
  assert.equal(await page.evaluate(()=>window.previewInjected),undefined);
  assert.equal(await page.locator('#fv-save').count(),1);
  assert.ok(!requestedFiles.some(r=>r.file?.includes('escape.svg')));
  await close();
  documents.set('docs/next.md','# Next\n\nTarget');
  await open('docs/start.md','[Next](next.md)\n\n![Local](assets/logo%20small.svg)','shared');
  await page.locator('#fv-md img').evaluate(img=>img.decode());
  assert.ok(requestedFiles.some(r=>r.file==='docs/assets/logo small.svg'&&r.scope==='shared'));
  await page.locator('#fv-mode-src').click();await page.locator('#fv-editor').fill('[Next](next.md)\n\nUnsaved');await page.locator('#fv-mode-view').click();
  await page.locator('#fv-md a').click();await page.locator('#dlg-ask').waitFor({state:'visible'});await page.locator('#ask-cancel').click();assert.equal(await page.locator('#fv-name').innerText(),'start.md');
  await page.locator('#fv-md a').click();await page.locator('#ask-ok').click();
  await page.waitForFunction(()=>document.querySelector('#fv-md h1')?.textContent==='Next');
  assert.ok(requestedFiles.some(r=>r.file==='docs/next.md'&&r.scope==='shared'));
  await close();
  await open('large.json','{\n'+ '  "value": 1,\n'.repeat(20000)+'}\n');
  assert.match(await page.locator('#fv-language').innerText(),/已简化着色/);
  assert.match(await page.locator('#fv-position').innerText(),/共 20003 行/);
  assert.ok(await page.locator('#fv-gutter span').count()<45);
  await close();
  assert.deepEqual(errors,[]);
  console.log('Preview: syntax, HTML glyph alignment, line counts, current line, raw save, Tab, dark/light/mobile, long-line scroll, fullscreen, empty/CRLF/large files, Markdown switching and escaping passed');
 } finally { await page.unroute('https://img.shields.io/**'); await page.unroute('**/api/**'); await page.unroute('**/fixture-preview*'); server.closeAllConnections(); await new Promise(r=>server.close(r)); }
}
if (process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)) {
 const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
 const browser=await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL ? {channel:process.env.AGENTBOX_BROWSER_CHANNEL}: {})});
 try { await smoke(await browser.newPage()); } finally { await browser.close(); }
}
