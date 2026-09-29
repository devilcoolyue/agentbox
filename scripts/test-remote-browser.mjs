import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// UI state regression with synthetic transport. Real RFB is exercised separately
// by test-remote-browser-live.py against Docker and the embedded noVNC module.
export async function remoteBrowserSmoke(page) {
 let running = false, unavailable = false, clipboard = '', delayResolve;
 const opened = [];
 await page.route('**/vendor/novnc/core/rfb.js', route => route.fulfill({contentType:'text/javascript',body:`
 export default class extends EventTarget {
 constructor(el,url){super();this.el=el;this.timer=setTimeout(()=>{el.textContent='Synthetic remote desktop';this.dispatchEvent(new Event('connect'));},20);}
 disconnect(){clearTimeout(this.timer);this.dispatchEvent(new Event('disconnect'));}
 set qualityLevel(value){this.el.dataset.quality=value;}
 set compressionLevel(value){this.el.dataset.compression=value;}
 focus(){} clipboardPasteFrom(){} sendKey(){}
 }`}));
 await page.route('**/api/sessions/*/browser', async route => {
  const method = route.request().method();
  if (method === 'POST') {
   if (unavailable) {await route.fulfill({status:409,json:{error:'当前镜像未安装浏览器'}});return;}
   running = true; opened.push(route.request().postDataJSON().url);
   if (delayResolve) await new Promise(resolve => { delayResolve(resolve); });
  } else if (method === 'DELETE') running = false;
  await route.fulfill({json:{available:!unavailable,running,browser:'Google Chrome',proxy:false}});
 });
 await page.route('**/api/sessions/*/browser/clipboard',async route=>{
  if(route.request().method()==='POST')clipboard=route.request().postDataJSON().text;
  await route.fulfill({json:{text:clipboard}});
 });
 await page.evaluate(()=>{location.hash='#/sessions/fixture-space/browser';});
 await page.locator('#tab-browser').waitFor({state:'visible'});
 await page.getByText('浏览器未启动 · 点击启动后继续上次的登录状态',{exact:true}).waitFor();
 unavailable = true;
 await page.locator('#browser-start').click();
 await page.locator('#browser-state').filter({hasText:'当前镜像未安装浏览器'}).waitFor();
 unavailable = false;
 await page.locator('[data-browser-url="https://claude.ai/"]').click();
 await page.locator('#browser-state').filter({hasText:'已连接'}).waitFor();
 assert.deepEqual(opened,['https://claude.ai/']);
 assert.equal(await page.locator('#browser-screen').getAttribute('data-quality'),'3');
 await page.locator('#browser-quality').evaluate(el=>{el.value='clear';el.dispatchEvent(new Event('change',{bubbles:true}));});
 assert.equal(await page.locator('#browser-screen').getAttribute('data-quality'),'9');
 assert.equal(await page.evaluate(()=>localStorage.getItem('agentbox_browser_quality')),'clear');
 await page.locator('#browser-quality').evaluate(el=>{el.value='smooth';el.dispatchEvent(new Event('change',{bubbles:true}));});
 await page.locator('#browser-url').fill('example.org/test?q=中文');
 await page.locator('#browser-open').click();
 await page.waitForFunction(()=>!document.querySelector('#browser-open').disabled);
 assert.equal(opened.at(-1),'https://example.org/test?q=中文');
 await page.locator('.browser-clipboard summary').click();
 await page.locator('#browser-clipboard').fill('中文输入 ✓');
 await page.locator('#browser-paste').click();
 await page.waitForResponse(r=>r.url().includes('/browser/clipboard')&&r.request().method()==='GET', {timeout:500}).catch(()=>{});
 await page.locator('#browser-copy').click();
 await page.waitForFunction(()=>document.querySelector('#browser-clipboard').value==='中文输入 ✓');
 assert.equal(clipboard,'中文输入 ✓');
 for (const theme of ["light","dark"]) {
 await page.evaluate(t=>document.documentElement.dataset.theme=t,theme);
 for (const width of [1440,900,390,320]) {
  await page.setViewportSize({width,height:900});
  await page.waitForTimeout(200);
  if (width<600 && await page.locator('#sidebar.open').count()) await page.locator('#btn-sidebar-close').click();
  await page.locator('.toast').waitFor({state:'hidden',timeout:10000}).catch(()=>{});
  assert.equal(await page.locator('body').evaluate(el=>el.scrollWidth<=innerWidth),true,'no page overflow');
  await mkdir('output/playwright',{recursive:true});
  await page.screenshot({path:`output/playwright/remote-browser-${theme}-${width}.png`});
 }
 }
 await page.evaluate(()=>document.documentElement.dataset.theme="light");
 await page.setViewportSize({width:1440,height:900});
 await page.locator('#browser-stop').click();
 await page.locator('#browser-state').filter({hasText:'浏览器已关闭'}).waitFor();
 assert.equal(running,false);
 // Late startup must not reconnect a desktop after switching workspace/tab.
 let release;
 delayResolve = resolve => {release=resolve;};
 await page.locator('#browser-start').click();
 await page.locator('[data-tab="files"]').click();
 await page.waitForFunction(()=>document.querySelector('#tab-browser').classList.contains('hidden'));
 release(); delayResolve=null;
 await page.waitForTimeout(100);
 assert.equal(await page.locator('#browser-screen').innerText(),'');
 await page.unroute('**/vendor/novnc/core/rfb.js');
 await page.unroute('**/api/sessions/*/browser');
 await page.unroute('**/api/sessions/*/browser/clipboard');
}
