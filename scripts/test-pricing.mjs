import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Exercise the real editor against synthetic admin API responses.
export async function pricingSmoke(page) {
 const rate = input => ({input,output:input*4,cache_read:input/10,cache_write:0});
 const entry = input => ({price:rate(input),source_url:'https://example.invalid/pricing',verified_at:'2026-09-26T08:00:00Z',notes:'标准档位 · <script>plain text</script>'});
 const origin = {catalog_url:'https://example.invalid/catalog.json',version:'fixture-v1',source_url:'https://example.invalid/pricing',verified_at:'2026-09-25T08:00:00Z'};
 let revision=1,failNext=false,applied=[];
 let active={revision:'1',prices:{'fixture-custom':rate(9),'fixture-follow':rate(2),'fixture-removed':rate(3)},managed:{'fixture-follow':origin,'fixture-removed':origin},catalog:{url:'https://example.invalid/catalog.json',auto_check:true,auto_apply:false},history:[]};
 const candidate={revision:'candidate-v2',catalog:{schema:1,version:'fixture-v2',published_at:'2026-09-26T08:00:00Z',issues:[{model:'fixture-incomplete',reason:'缺少缓存读取单价，保留现价'}],entries:{'fixture-custom':entry(1),'fixture-follow':entry(4),'fixture-new':entry(2)}},url:active.catalog.url,bundled:false,checked_at:Date.now(),attempted_at:Date.now(),error:''};
 const response=()=>({active,candidate,changes:[...Object.entries(candidate.catalog.entries).map(([model,candidate])=>({model,candidate,current:active.prices[model],kind:!active.prices[model]?'new':!active.managed[model]?'custom':JSON.stringify(active.prices[model])===JSON.stringify(candidate.price)?'current':'update'})),{model:'fixture-removed',kind:'removed',current:active.prices['fixture-removed']}],warnings:[{agent:'codex',model:'fixture-missing',kind:'unpriced',key:''},{agent:'codex',model:'fixture-fallback',kind:'fallback',key:'codex'}],warnings_truncated:false});
 const snapshot=()=>({id:active.revision,saved_at:Date.now(),reason:'测试变更',prices:structuredClone(active.prices),managed:structuredClone(active.managed)});
 const handler=async route=>{
  const req=route.request(),path=new URL(req.url()).pathname;
  if(req.method()!=='GET') {
   const data=req.postData()?req.postDataJSON():{};
   if(failNext){failNext=false;await route.fulfill({status:409,json:{error:'价目表已变化，请刷新后重新核对'}});return;}
   if(path.endsWith('/check')){candidate.error='获取或校验价格目录失败，已保留上次候选与生效价格';}
   else {
    assert.equal(data.revision,active.revision,'stale revision sent');
    const old=snapshot();
    if(path.endsWith('/apply')) {
     assert.equal(data.catalog_revision,candidate.revision);applied.push(data);
     for(const model of data.models){active.prices[model]=candidate.catalog.entries[model].price;active.managed[model]={...origin,version:'fixture-v2'};}
    } else if(path.endsWith('/restore')) {
     const old=active.history.find(row=>row.id===data.id);active.prices=old.prices;active.managed=old.managed;active.catalog.auto_apply=false;
    } else {
     if(data.prices){for(const [key,p] of Object.entries(data.prices)){if(JSON.stringify(p)!==JSON.stringify(active.prices[key]))delete active.managed[key];}active.prices=data.prices;}
     for(const key of data.custom_models||[])delete active.managed[key];
     if(data.catalog)active.catalog=data.catalog;
    }
    active.history.unshift(old);active.revision=String(++revision);
   }
  }
  await route.fulfill({json:response()});
 };
 await page.route('**/api/pricing**',handler);
 try {
  await page.evaluate(()=>location.hash='#/settings/pricing');
  await page.locator('#price-rows tr[data-key="fixture-follow"]').waitFor();
  const row = key => page.locator(`#price-rows tr[data-key="${key}"]`);
  assert.equal(await page.locator('#price-count').innerText(),'3');
  assert.match(await page.locator('#price-source-issues').innerText(),/fixture-incomplete.*缺少缓存/s);
  assert.match(await row('fixture-custom').innerText(),/自定义/);
  assert.match(await row('fixture-follow').innerText(),/跟随目录/);
  assert.match(await page.locator('#price-warnings').innerText(),/fixture-missing.*未定价/s);
  assert.match(await page.locator('#price-warnings').innerText(),/fixture-fallback.*兜底价/s);
  await page.locator('#price-preview').click();
  const pick = key => page.locator(`#price-changes input[value="${key}"]`);
  assert.equal(await pick('fixture-new').isChecked(),true);
  assert.equal(await pick('fixture-follow').isChecked(),true);
  assert.equal(await pick('fixture-custom').isChecked(),false);
  assert.equal(await pick('fixture-removed').isDisabled(),true);
  assert.match(await page.locator('#price-changes').innerText(),/\+100.0%/);
  assert.equal(await page.locator('#price-changes script').count(),0);
  await mkdir('output/playwright',{recursive:true});
  await page.locator('#toast.show').waitFor({state:'hidden'});
  for(const theme of ['light','dark']) {
   await page.evaluate(t=>document.documentElement.dataset.theme=t,theme);
   await page.setViewportSize({width:1440,height:1000});
   await page.locator('#set-content').evaluate(el=>el.scrollTo(0,0));
   await page.screenshot({path:`output/playwright/pricing-${theme}.png`,animations:'disabled'});
   await page.setViewportSize({width:390,height:844});
   if(await page.locator('#sidebar.open').count()) await page.locator('#btn-sidebar-close').click();
   await page.locator('#set-content').evaluate(el=>el.scrollTo(0,0));
   assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
   const bounds=await page.locator('.price-catalog').boundingBox();
   assert.ok(bounds.x>=0 && bounds.x+bounds.width<=390,'pricing panel outside viewport');
   await page.locator('.price-catalog').scrollIntoViewIfNeeded();
   await page.screenshot({path:`output/playwright/pricing-mobile-${theme}.png`,animations:'disabled'});
  }
  await page.setViewportSize({width:1440,height:1000});
  await page.locator('#price-apply').click();await page.locator('#ask-ok').click();
  await row('fixture-new').waitFor();
  assert.deepEqual(applied[0].models.sort(),['fixture-follow','fixture-new']);
  assert.deepEqual(applied[0].adopt_custom,[]);
  assert.equal(await row('fixture-custom').locator('input.input').inputValue(),'9');
  await pick('fixture-custom').check();await page.locator('#price-apply').click();await page.locator('#ask-ok').click();
  await page.waitForFunction(()=>document.querySelector('#price-rows tr[data-key="fixture-custom"] small').textContent.includes('跟随目录'));
  assert.deepEqual(applied[1].adopt_custom,['fixture-custom']);
  await row('fixture-follow').locator('input.input').fill('7');await page.locator('#price-save').click();
  await page.waitForFunction(()=>document.querySelector('#price-rows tr[data-key="fixture-follow"] small').textContent==='自定义');
  await row('fixture-new').getByRole('button',{name:'设为自定义'}).click();await page.locator('#price-save').click();
  await page.waitForFunction(()=>!document.querySelector('#price-save').disabled);
  assert.equal(active.managed['fixture-new'],undefined);
  const before=structuredClone(active.prices);
  await page.locator('#price-check').click();await page.locator('#price-catalog-error').filter({hasText:'失败'}).waitFor();
  assert.deepEqual(active.prices,before,'failed check changed rates');
  failNext=true;await row('fixture-follow').locator('input.input').fill('77');await page.locator('#price-save').click();
  await page.getByText('价目表已变化，请刷新后重新核对',{exact:true}).waitFor();
  assert.equal(await row('fixture-follow').locator('input.input').inputValue(),'77','conflict lost draft');
  await page.locator('#price-refresh').click();await page.locator('#ask-ok').click();
  await page.waitForFunction(()=>document.querySelector('#price-rows tr[data-key="fixture-follow"] input.input').value==='7');
  await page.locator('.price-history summary').click();
  await page.locator('#price-history button').first().click();await page.locator('#ask-ok').click();
  await page.waitForFunction(()=>document.querySelector('#price-rows tr[data-key="fixture-new"] small').textContent.includes('跟随目录'));
  await page.locator('#price-auto').uncheck();await page.locator('#price-source-save').click();
  await page.waitForFunction(()=>!document.querySelector('#price-source-save').disabled);
  assert.equal(active.catalog.auto_check,false);
  await page.locator('#price-modelsdev').click();
  assert.equal(await page.locator('#price-source').inputValue(),'https://models.dev/api.json');
  assert.equal(await page.locator('#price-auto').isChecked(),true);
  assert.equal(await page.locator('#price-auto-apply').isChecked(),false);
  assert.notEqual(active.catalog.url,'https://models.dev/api.json','preset changed configuration before saving');
  await page.locator('#price-source-save').click();
  await page.waitForFunction(()=>!document.querySelector('#price-source-save').disabled);
  assert.equal(active.catalog.url,'https://models.dev/api.json');
  await page.locator('#price-auto-apply').check();
  await page.locator('#price-source-save').click();
  await page.locator('#ask-ok').waitFor({state:'visible'});
  assert.equal(active.catalog.auto_apply,false,'auto-apply enabled before confirmation');
  const autoSaved = page.waitForResponse(r=>new URL(r.url()).pathname==='/api/pricing' && r.request().method()==='PUT');
  await page.locator('#ask-ok').click();
  await autoSaved;
  await page.waitForFunction(()=>!document.querySelector('#price-source-save').disabled);
  assert.equal(active.catalog.auto_apply,true);
  await page.locator('#price-history button').first().click();await page.locator('#ask-ok').click();
  await page.waitForFunction(()=>!document.querySelector('#price-auto-apply').checked);
  assert.equal(active.catalog.auto_apply,false,'rollback did not pause automatic updates');
  await page.locator('#price-auto').uncheck();
  assert.equal(await page.locator('#price-auto-apply').isChecked(),false);
  await page.evaluate(()=>document.documentElement.dataset.theme='light');
  console.log('Pricing: diff preview, custom protection/adoption, manual custom mode, failed check, conflicts, restore and responsive layout passed');
 } finally {await page.unroute('**/api/pricing**',handler);}
}
