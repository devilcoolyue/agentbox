import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

export async function imageUpdateSmoke(page) {
 const settings = await page.evaluate(async () => (await import('/_v/{{BUILD}}/js/api.js')).api('/settings'));
 let saves = 0, checks = 0, updates = 0, rollbacks = 0, reads = 0;
 let fail = false;
 let view = {settings:{enabled:false,channel:'stable',time:'04:00',update_codex:false},agent_image:settings.agent_image,previous_image:'',timezone:'Asia/Shanghai',status:{running:false,phase:'',action:'',error:'',log:'',current:{claude:'',codex:''},target:{claude:'',codex:''}}};
 const handler = async route => {
  const req = route.request(), path = new URL(req.url()).pathname;
  if (path === '/api/settings') {
   if (req.method() === 'PUT') { saves++; view.settings = req.postDataJSON().image_updates; }
   await route.fulfill({json:{...settings,agent_image:view.agent_image,image_updates:view.settings}});return;
  }
  if (req.method() === 'POST') {
   if (path.endsWith('/check')) {
    checks++; view.status = {...view.status,phase:'done',available:true,current:{claude:'2.1.280',codex:'0.156.1'},target:{claude:'2.1.285',codex:'0.156.1'},log:'<script>untrusted npm output</script>'};
   } else if (path.endsWith('/update')) {
    updates++;view.status = {...view.status,running:true,phase:'building',action:'update',error:''};
   } else if (path.endsWith('/rollback')) {
    rollbacks++;view.agent_image='old-id';view.settings.enabled=false;view.status={...view.status,running:false,phase:'done',action:'rollback',error:''};
   }
  } else {
   reads++;
   if (view.status.running) {
    if (fail) view.status={...view.status,running:false,phase:'failed',error:'构建失败，保留当前镜像'};
    else {view.agent_image='agentbox-agent:cli-test';view.previous_image='old-id';view.status={...view.status,running:false,phase:'done',available:false};}
   }
  }
  await route.fulfill({json:view});
 };
 await page.route('**/api/image-updates**',handler);
 await page.route('**/api/settings',handler);
 const select = async (id, label) => {
  await page.locator(`#${id} + button`).click();
  await page.getByRole('option',{name:label,exact:true}).click();
 };
 try {
  await page.evaluate(()=>location.hash='#/settings/accounts');
  await page.locator('#sec-accounts').waitFor({state:'visible'});
  await page.locator('#set-nav [data-sec="container"]').click();
  await page.waitForFunction(()=>!document.querySelector('#image-update-check').disabled);
  assert.equal(await page.locator('#image-update-rollback').isDisabled(),true);
  await select('image-update-channel','latest — 最新版');
  await select('image-update-enabled','开启 — 每日检查并应用');
  await page.locator('#image-update-time').fill('05:30');
  await page.locator('#image-update-time').blur();
  assert.equal(await page.locator('#image-update-check').isDisabled(),true,'unsaved policy used for check');
  await page.locator('#image-update-save').click();
  await page.waitForFunction(()=>!document.querySelector('#image-update-check').disabled);
  assert.equal(saves,1);assert.equal(view.settings.channel,'latest');assert.equal(view.settings.time,'05:30');
  await page.locator('#image-update-check').click();
  await page.locator('#image-update-status').filter({hasText:'有可用更新'}).waitFor();
  assert.equal(checks,1);assert.equal(updates,0);
  assert.equal(await page.locator('#image-update-log script').count(),0);
  await page.locator('#image-update-update').click();
  await page.locator('#ask-ok').click();
  await page.waitForFunction(()=>document.querySelector('#image-update-active').textContent.includes('cli-test'));
  assert.equal(updates,1);assert.equal(await page.locator('#image-update-rollback').isDisabled(),false);
  await page.locator('#image-update-rollback').click();
  await page.locator('#ask-ok').click();
  await page.waitForFunction(()=>document.querySelector('#image-update-schedule').textContent.includes('自动更新已关闭'));
  assert.equal(rollbacks,1);assert.equal(await page.locator('#image-update-enabled').inputValue(),'off');
  fail=true;
  await page.locator('#image-update-update').click();
  await page.locator('#ask-ok').click();
  await page.locator('#image-update-status').filter({hasText:'构建失败'}).waitFor();
  assert.equal(view.agent_image,'old-id');
  await mkdir('output/playwright',{recursive:true});
  await page.locator('#image-update-card').scrollIntoViewIfNeeded();
  await page.screenshot({path:'output/playwright/image-updates-desktop.png'});
  await page.setViewportSize({width:390,height:844});
  await page.waitForFunction(()=>document.querySelector('#sidebar').getBoundingClientRect().right <= 1);
  await page.locator('#image-update-card').scrollIntoViewIfNeeded();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true,'mobile overflow');
  await page.screenshot({path:'output/playwright/image-updates-mobile.png',animations:'disabled'});
  await page.setViewportSize({width:1280,height:900});
  await page.evaluate(()=>location.hash='#/settings/accounts');
  await page.locator('#sec-accounts').waitFor({state:'visible'});
  const before=reads;await page.waitForTimeout(3200);assert.equal(reads,before,'poller kept running after navigation');
  console.log('Browser: CLI update settings, channel, scheduling, check/update/rollback, failure, text escaping, mobile layout and polling cleanup passed');
 } finally {
  await page.unroute('**/api/image-updates**',handler);await page.unroute('**/api/settings',handler);
 }
}
