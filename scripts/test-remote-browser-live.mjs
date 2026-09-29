import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
let raw=''; for await (const chunk of process.stdin) raw+=chunk;
const {base,password,session,token}=JSON.parse(raw);
const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
const browser=await chromium.launch({headless:true});
try {
 const page=await browser.newPage({viewport:{width:1600,height:1100}});
 const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto(`${base}/#/sessions/${session}/browser`);
 await page.locator('#login-user').fill('boxadmin');
 await page.locator('#login-pass').fill(password);
 await page.locator('#login-btn').click();
 await page.locator('#browser-state').filter({hasText:'已连接'}).waitFor({timeout:30000});
 await page.waitForFunction(()=>document.querySelector('#browser-screen canvas')?.width>1000);
 // Allow the initial full framebuffer to arrive before checking its pixels.
 await page.waitForFunction(()=>{
  const canvas=document.querySelector('#browser-screen canvas');
  const data=canvas.getContext('2d').getImageData(100,150,200,200).data;
  return data.some((v,i)=>i%4!==3&&v>100);
 });
 const canvas=page.locator('#browser-screen canvas');
 const rect=await canvas.boundingBox();
 await canvas.click({position:{x:rect.width*.25,y:rect.height*.38}});
 await page.locator('.browser-clipboard summary').click();
 await page.locator('#browser-clipboard').fill('真实远程桌面 ✓');
 await Promise.all([page.waitForResponse(r=>r.url().endsWith('/browser/clipboard')&&r.request().method()==='POST'),page.locator('#browser-paste').click()]);
 for(let i=0;i<50;i++){
  const response=await page.request.get(`${base}/api/sessions/${session}/file?path=browser-hits`,{headers:{Authorization:'Bearer '+token}});
  if((await response.text()).includes('/typed?value='+encodeURIComponent('真实远程桌面 ✓')))break;
  if(i===49)throw new Error('UTF-8 clipboard did not reach the real browser input');
  await page.waitForTimeout(100);
 }
 await page.waitForTimeout(300);
 await mkdir('output/playwright',{recursive:true});
 await page.screenshot({path:'output/playwright/remote-browser-live.png'});
 await page.setViewportSize({width:390,height:900});
 await page.waitForTimeout(200);
 if(await page.locator('#sidebar.open').count())await page.locator('#btn-sidebar-close').click();
 await page.waitForFunction(()=>!document.querySelector('#sidebar').classList.contains('open'));
 await page.waitForTimeout(300);
 await page.screenshot({path:'output/playwright/remote-browser-live-mobile.png'});
 assert.equal(await page.locator('body').evaluate(el=>el.scrollWidth<=innerWidth),true);
 assert.deepEqual(errors,[]);
 console.log('Real noVNC: authenticated canvas, desktop/mobile layout, clipboard controls passed');
} finally {await browser.close();}
