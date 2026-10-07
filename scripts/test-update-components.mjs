import assert from 'node:assert/strict';
import {mkdir} from 'node:fs/promises';

export async function updateComponentsSmoke(page){
 let reads=0,fail=false,hold=false;const pending=[],writes=[];
 let image={reference:'fixture:image',id:'sha256:fixture',claude:'2.1.280<script>metadata</script>',codex:'0.145.0',state:'labels_only',observed_at:Date.now()};
 const handler=async route=>{
  reads++;
  const body={version:1,observed_at:Date.now(),server:{version:'v0.1.10',schema:11,candidate_compatibility:'preflight_required'},image:structuredClone(image),desktop:{installed_version_state:'browser_unknown',server_protocol:1,sync_enabled:false}};
  if(hold)await new Promise(resolve=>pending.push(resolve));
  await route.fulfill(fail?{status:503,json:{error:'synthetic read failed'}}:{json:body}).catch(()=>{});
 };
 const requests=request=>{const path=new URL(request.url()).pathname;if(request.method()==='POST'&&(/^\/api\/(updates|image-updates)\//.test(path)))writes.push(path);};
 page.on('request',requests);
 await page.route('**/api/updates/components',handler);
 const ready=()=>page.waitForFunction(()=>!document.querySelector('#component-refresh').disabled);
 try{
  for(const locale of ['zh-CN','zh-TW','en']){
   await page.evaluate(async locale=>{(await import('/_v/{{BUILD}}/js/i18n.js')).i18n.setLanguage(locale);location.hash='#/settings/about';},locale);
   await page.locator('#sec-about').waitFor({state:'visible'});await page.locator('#component-refresh').click();await ready();
   assert.ok((await page.locator('#component-server-version').innerText()).includes('schema 11'));
   assert.ok((await page.locator('#component-image-version').innerText()).includes('<script>metadata</script>'));
   assert.equal(await page.locator('#component-image-version script').count(),0);
   assert.ok((await page.locator('#component-desktop-version').innerText()).length>20);
   const label=locale==='en'?'Desktop app':locale==='zh-TW'?'桌面應用程式':'桌面应用';
   assert.ok((await page.locator('.component-table').innerText()).includes(label));
   const title=await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/i18n.js')).t('系统设置'));
   assert.equal(await page.locator('#topbar-title').innerText(),title,'mobile title retained the prior language');
   fail=true;await page.locator('#component-refresh').click();await ready();
   assert.equal(await page.locator('#component-image-version').innerText(),'','failed read retained a current-version claim');
   assert.ok((await page.locator('#component-read-status').innerText()).includes('synthetic read failed'));fail=false;
   image.state='changed';await page.locator('#component-refresh').click();await ready();
   assert.ok(!(await page.locator('#component-image-version').innerText()).includes('2.1.280'));
   image.state='unavailable';await page.locator('#component-refresh').click();await ready();
   assert.ok((await page.locator('#component-image-state').innerText()).length>20);
   image.state='labels_only';await page.locator('#component-refresh').click();await ready();
  }
  await mkdir('output/playwright',{recursive:true});
  await page.locator('.update-components').scrollIntoViewIfNeeded();await page.screenshot({path:'output/playwright/update-components-desktop.png',animations:'disabled'});
  await page.setViewportSize({width:390,height:844});
  assert.equal(await page.locator('.component-table').evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'component comparison overflows');
  await page.locator('.update-components').scrollIntoViewIfNeeded();await page.screenshot({path:'output/playwright/update-components-mobile.png',animations:'disabled'});
  await page.setViewportSize({width:1280,height:900});
  // Leaving aborts a slow observation; its result must not overwrite a new read.
  hold=true;const sent=page.waitForRequest(r=>new URL(r.url()).pathname==='/api/updates/components');
  await page.locator('#component-refresh').click();await sent;
  await page.locator('#component-image-open').click();await page.locator('#sec-container').waitFor({state:'visible'});
  hold=false;image.reference='newer:image';for(const release of pending.splice(0))release();
  await page.evaluate(()=>location.hash='#/settings/about');await page.locator('#sec-about').waitFor({state:'visible'});
  await page.locator('#component-image-version').filter({hasText:'newer:image'}).waitFor();
  assert.deepEqual(writes,[],'local observations submitted an updater action');assert.ok(reads>=16);
  console.log('Browser: update component scope, three locales, unavailable/stale image states, text safety, narrow layout and read cancellation passed');
 }finally{
  for(const release of pending.splice(0))release();page.off('request',requests);
  await page.unroute('**/api/updates/components',handler);
  await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/i18n.js')).i18n.setLanguage('zh-CN');location.hash='#/settings/accounts';});
  await page.locator('#sec-accounts').waitFor({state:'visible'});
 }
}
