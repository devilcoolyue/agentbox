#!/usr/bin/env node
// Chat Markdown rendering (formatText) in a real browser with the vendored KaTeX; static files only, no API, Docker or provider.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {launchBrowser} from './playwright-launch.mjs';

const root=resolve(fileURLToPath(new URL('../internal/web/static/',import.meta.url)));
const server=createServer(async(req,res)=>{
 try{
  const rel=new URL(req.url,'http://fixture').pathname.replace(/^\/_v\/[^/]+/,'');
  const file=resolve(root,'.'+(rel==='/'?'/index.html':rel));if(!file.startsWith(root+'/')){res.writeHead(403).end();return;}
  let data=await readFile(file);
  // chat-render.js needs the page's dialogs and KaTeX, not the app: drop main.js so nothing calls the API.
  if(rel==='/')data=data.toString().replace(/<script type="module" src="[^"]*\/js\/main.js"><\/script>/,'');
  res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(file)]||'application/octet-stream');res.end(data);
 }catch{res.writeHead(404).end();}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));
const browser=await launchBrowser();
try{
 const page=await browser.newPage();page.setDefaultTimeout(15000);
 const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto(`http://127.0.0.1:${server.address().port}/`);
 await page.waitForFunction(()=>!!window.katex);
 // Renders md and describes each block: lists by item, everything else by class and text.
 const render=md=>page.evaluate(async md=>{
  const {formatText}=await import('/js/chat-render.js');
  const box=document.createElement('div');box.appendChild(formatText(md));document.body.appendChild(box);
  const rawText=el=>[...el.childNodes].filter(n=>n.nodeType===3).map(n=>n.textContent).join('');
  const out=[...box.children].map(el=>el.matches('ul,ol')?{list:el.tagName.toLowerCase(),items:[...el.children].map(li=>({
   display:[...li.querySelectorAll(':scope > .math-display')].map(m=>({katex:!!m.querySelector('.katex-display'),error:!!m.querySelector('.katex-error'),tex:m.querySelector('annotation')?.textContent||''})),
   inline:li.querySelectorAll('.math-inline .katex').length,raw:rawText(li),
  }))}:{cls:el.className,katex:!!el.querySelector('.katex-display'),text:el.textContent});
  box.remove();return out;
 },md);

 // The reply that showed raw LaTeX: an indented $$ … $$ block continuing a bullet.
 let r=await render(String.raw`**常见变形**

- **权方和形式**（分式形式）：当 $b_i > 0$ 时，
  $$
  \frac{a_1^2}{b_1} + \cdots + \frac{a_n^2}{b_n} \ge \frac{(a_1+\cdots+a_n)^2}{b_1+\cdots+b_n}
  $$
- **积分形式**：$\left(\int_a^b f g\,dx\right)^2 \le \int_a^b f^2\,dx \cdot \int_a^b g^2\,dx$
- **一般内积空间**：$|\langle u,v\rangle|^2 \le \langle u,u\rangle\,\langle v,v\rangle$

结尾段落。`);
 assert.equal(r.length,3,JSON.stringify(r));
 assert.equal(r[1].list,'ul');
 assert.equal(r[1].items.length,3,'the display block must not split the list');
 const [first,second,third]=r[1].items;
 assert.equal(first.display.length,1);
 assert.deepEqual({katex:first.display[0].katex,error:first.display[0].error},{katex:true,error:false});
 assert.match(first.display[0].tex,/^\\frac\{a_1\^2\}\{b_1\}.*\\ge \\frac/);
 assert.equal(first.inline,1);
 assert.doesNotMatch(first.raw,/\$|\\frac/,'no delimiter or LaTeX may leak as text');
 for(const item of [second,third]){assert.equal(item.display.length,0);assert.equal(item.inline,1);assert.doesNotMatch(item.raw,/\$/);}
 assert.deepEqual(r[2],{cls:'md-p',katex:false,text:'结尾段落。'});

 // \[ … \] across lines, one-line $$ … $$, an ordered list, and a deeper indent all stay inside their item.
 r=await render(String.raw`1. 向量形式：
   \[
   |\vec a\cdot\vec b| \le |\vec a|\,|\vec b|
   \]
2. 单行：
    $$ x^2 + y^2 = z^2 $$
3. 收尾`);
 assert.equal(r.length,1,JSON.stringify(r));
 assert.equal(r[0].list,'ol');
 assert.deepEqual(r[0].items.map(i=>i.display.length),[1,1,0]);
 assert.ok(r[0].items.slice(0,2).every(i=>i.display[0].katex&&!i.display[0].error));
 assert.match(r[0].items[0].display[0].tex,/\\vec a\\cdot\\vec b/);
 assert.equal(r[0].items[1].display[0].tex,'x^2 + y^2 = z^2');
 assert.doesNotMatch(r[0].items.map(i=>i.raw).join(''),/\$|\\\[|\\\]/);

 // Ordinary continuation lines and dollar amounts are untouched; an unclosed $$ stays text and keeps the list going.
 r=await render(String.raw`- 普通续行
  第二行文字 $a+b$
- 价格 $5 和 $10 不是公式
- 没闭合：
  $$
  x + 1
- 下一项`);
 assert.equal(r.length,1,JSON.stringify(r));
 const items=r[0].items;
 assert.equal(items.length,4);
 assert.deepEqual(items.map(i=>i.display.length),[0,0,0,0]);
 assert.equal(items[0].inline,1);
 assert.equal(items[0].raw,'普通续行\n第二行文字 ');
 assert.deepEqual({inline:items[1].inline,raw:items[1].raw},{inline:0,raw:'价格 $5 和 $10 不是公式'});
 assert.equal(items[2].raw,'没闭合：\n$$\nx + 1');
 assert.equal(items[3].raw,'下一项');

 // Top-level display math keeps rendering as its own block.
 r=await render('前文\n\n$$\n\\sum_{i=1}^n a_i b_i\n$$\n\n后文');
 assert.deepEqual(r.map(b=>b.cls),['md-p','math-display','md-p']);
 assert.equal(r[1].katex,true);

 assert.deepEqual(errors,[]);
 console.log('Chat Markdown: display math inside list items ($$ and \\[ across lines, one-line $$, ordered/deeper indent), unclosed $$ fallback, plain continuations, dollar amounts and top-level blocks passed');
}finally{await browser.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
