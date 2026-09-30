// MCP UI regression using synthetic API responses; called by test-browser.mjs.
import assert from 'node:assert/strict';
import {mkdir} from 'node:fs/promises';
import {assertActionIcons} from './test-actions.mjs';
export async function mcpSmoke(page) {
 let revision=1, writes=0, checks=0, imports=0, adoptions=0, failNextWrite=true;
 let entries=[{name:'probe',source:'user',status:'applied',config:{type:'http',url:'https://example.com/mcp',headers:{Authorization:'__AGENTBOX_KEEP_SECRET__'}}},
 {name:'native',source:'native',status:'unmanaged',config:{type:'stdio',command:'python3'},native:{type:'stdio',command:'python3'},native_revision:'synthetic-fingerprint'}];
 const handler=async route=>{
  const u=new URL(route.request().url()), method=route.request().method();
  if (!u.pathname.includes('/mcp')) {await route.fallback();return;}
  const data=method==='GET'?{}:(route.request().postDataJSON()||{}); let body={ok:true};
  if(method==='GET') body={revision,user_revision:1,items:entries,project_names:['project-probe']};
  else if(u.pathname.endsWith('/check')) {checks++;body={status:'connected',checked_at:new Date().toISOString(),tools:[{name:'echo',description:'<script>untrusted MCP description</script>'}]};}
  else if(u.pathname.endsWith('/adopt')) {assert.equal(data.native_revision,'synthetic-fingerprint');adoptions++;entries=entries.map(e=>e.name==='native'?{...e,source:'session',status:'applied'}:e);revision++;}
  else if(u.pathname.endsWith('/import')) {imports++;assert.ok(data.mcpServers.upload);entries.push({name:'upload',source:'session',status:'pending',config:data.mcpServers.upload});revision++;}
  else if(method==='PUT') {
   if(failNextWrite){failNextWrite=false;await route.fulfill({status:409,json:{error:'配置已变化，请刷新后重试'}});return;}
   writes++; assert.equal(data.revision,revision);
   const name=decodeURIComponent(u.pathname.split('/').pop());
   if(name==='probe') assert.equal(data.entry.config.headers.Authorization,'__AGENTBOX_KEEP_SECRET__','masked secret overwritten');
   entries=entries.filter(e=>e.name!==name);entries.push({name,...data.entry,source:'session',status:'pending'});revision++;
  }
  else if(method==='DELETE') {entries=entries.filter(e=>e.name!==u.pathname.split('/').pop());revision++;}
  await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(body)});
 };
 await page.route('**/api/**',handler);
 try {
  await page.reload();
  await page.locator('#app').waitFor({state:'visible'});
  await page.evaluate(()=>{location.hash='#/sessions/fixture-space/mcp';});
  await page.locator('#mcp-list .mcp-row').first().waitFor();
  assert.equal(await page.locator('#tab-mcp').isVisible(),true);
  const heights=await page.locator('.mcp-toolbar').evaluate(el=>[...el.querySelectorAll('.btn,.select-trigger')].map(e=>e.getBoundingClientRect().height));
  assert.ok(Math.max(...heights)-Math.min(...heights)<=1,'toolbar controls must have equal heights');
  assert.equal(await page.locator('#tab-btn-mcp svg').getAttribute('data-icon-name'),'plug');
  await page.locator('#mcp-add').click();
  assert.equal(await page.locator('#mcp-editor').evaluate(el=>el.matches(':modal')),true);
  await page.keyboard.press('Escape');
  await page.locator('#mcp-editor').waitFor({state:'hidden'});
  assert.equal(await page.locator('#mcp-add').evaluate(el=>el===document.activeElement),true,'modal must restore focus');
  const probe=()=>page.locator('.mcp-row').filter({has:page.locator('strong',{hasText:/^probe$/})});
  await probe().getByRole('button',{name:'编辑空间覆盖'}).click();
  assert.ok((await page.locator('#mcp-secrets').inputValue()).includes('••••••'));
  await page.locator('#mcp-type + .select-trigger').click();
  await page.locator('#mcp-editor .select-panel').waitFor({state:'visible'});
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('#mcp-editor').evaluate(el=>el.open),true,'dropdown Escape must not close dialog');
  await page.locator('#mcp-url').fill('https://example.com/updated-mcp');
  await page.locator('#mcp-form button[type=submit]').click();
  await page.locator('#mcp-form-error').waitFor({state:'visible'});
  assert.equal(await page.locator('#mcp-url').inputValue(),'https://example.com/updated-mcp');
  assert.equal(await page.locator('#mcp-editor').evaluate(el=>el.open),true,'failed save must retain modal draft');
  await page.locator('#mcp-save').click();
  await page.waitForFunction(()=>!document.querySelector('#mcp-editor').open);
  assert.equal(writes,1);
  await probe().getByRole('button',{name:'测试连接'}).click();
  await probe().getByText('1 个工具').waitFor();assert.equal(checks,1);
  await probe().locator('details').last().locator('summary').click();
  assert.equal(await probe().locator('script').count(),0);
  assert.ok((await probe().textContent()).includes('<script>untrusted MCP description</script>'));
  const native=page.locator('.mcp-row').filter({has:page.locator('strong',{hasText:/^native$/})});
  await native.getByRole('button',{name:'导入并接管终端配置'}).click();
  await page.locator('#ask-ok').click();
  await native.getByRole('button',{name:'编辑',exact:true}).waitFor();assert.equal(adoptions,1);
  await page.locator('#mcp-import-file').setInputFiles({name:'mcp.json',mimeType:'application/json',buffer:Buffer.from(JSON.stringify({mcpServers:{upload:{command:'node',args:['/home/agent/server.js']}}}))});
  await page.locator('#dlg-ask').waitFor({state:'visible'});
  assert.equal(imports,0,'import ran before preview confirmation');await page.locator('#ask-ok').click();
  await page.locator('.mcp-row strong').filter({hasText:/^upload$/}).waitFor();assert.equal(imports,1);
  await page.locator('#mcp-add').click();await page.locator('#mcp-name').fill('stdio-demo');
  await page.locator('#mcp-command').fill('python3');await page.locator('#mcp-args').fill('["/home/agent/demo.py"]');await page.locator('#mcp-secrets').fill('{"TOKEN":"synthetic-only"}');
  await mkdir('output/playwright',{recursive:true});
  await page.locator('.toast.show').waitFor({state:'hidden'});
  await assertActionIcons(page,'#tab-mcp');
  await assertActionIcons(page,'#mcp-editor');
  await page.mouse.move(10,10);
  await page.screenshot({animations:'disabled',path:'output/playwright/mcp-desktop.png'});
  await page.evaluate(()=>document.documentElement.dataset.theme='dark');
  await page.mouse.move(10,10);
  await page.screenshot({animations:'disabled',path:'output/playwright/mcp-desktop-dark.png'});
  await page.evaluate(()=>document.documentElement.dataset.theme='light');
  await page.setViewportSize({width:390,height:844});
  await page.waitForFunction(()=>document.querySelector('#sidebar').getBoundingClientRect().right<=1);
  assert.equal(await page.locator('#mcp-editor').evaluate(el=>el.matches(':modal')),true);
  await page.locator('#mcp-name').scrollIntoViewIfNeeded();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true,'MCP mobile horizontal overflow');
  const footer=await page.locator('.mcp-dialog-footer').boundingBox();
  assert.ok(footer.y>=0&&footer.y+footer.height<=844,'modal footer must remain visible');
  await page.mouse.move(10,10);
  await page.screenshot({animations:'disabled',path:'output/playwright/mcp-mobile.png'});
  await page.evaluate(()=>document.documentElement.dataset.theme='dark');
  await page.mouse.move(10,10);
  await page.screenshot({animations:'disabled',path:'output/playwright/mcp-mobile-dark.png'});
  await page.evaluate(()=>document.documentElement.dataset.theme='light');
  await page.locator('#mcp-form button[type=submit]').click();
  await page.locator('.mcp-row strong').filter({hasText:/^stdio-demo$/}).waitFor();assert.equal(writes,2);
  await page.setViewportSize({width:1280,height:900});
  await page.locator('#mcp-editor').waitFor({state:'hidden'});
  await page.locator('.toast.show').waitFor({state:'hidden'});
  await page.mouse.move(10,10);
  await page.screenshot({animations:'disabled',path:'output/playwright/mcp-list.png'});
  // Repeated app initialization must not duplicate handlers.
  await page.reload();await page.locator('#mcp-list .mcp-row').first().waitFor();
  await probe().getByRole('button',{name:'测试连接'}).click();await probe().getByText('1 个工具').waitFor();assert.equal(checks,2);
  console.log('MCP browser: masked editing, inheritance, adoption, import preview, tool escaping, mobile layout and reload passed');
 } finally {await page.unroute('**/api/**',handler);}
}
