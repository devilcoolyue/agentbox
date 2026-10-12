// Browser UI -> real Go HTTP/store/scheduler/usage/files; only Docker/CLI are
// synthetic. The Linux owner is started by test-collaboration-runtime.py.
import assert from 'node:assert/strict';
import {mkdir} from 'node:fs/promises';
import {launchBrowser,browserEngine} from './playwright-launch.mjs';
const base=process.argv[2];if(!/^http:\/\/127\.0\.0\.1:\d+$/.test(base||''))throw Error('isolated fixture URL required');
const browser=await launchBrowser();const page=await browser.newPage({viewport:{width:1200,height:1000}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
const text=source=>page.evaluate(async source=>(await import('/js/i18n.js')).t(source),source);
const click=async source=>page.getByRole('button',{name:await text(source),exact:true}).click();
const waitButton=async source=>page.getByRole('button',{name:await text(source),exact:true}).waitFor({timeout:45000});
const evidence=async()=>{const r=await fetch(base+'/fixture/evidence');return r.json();};
try{
 await page.goto(base+'/fixture');await page.locator('#btn-collaboration:not([hidden])').click();
 const dialog=page.locator('dialog[open]');await dialog.waitFor();
 await page.locator('.collaboration-settings>summary').click();
 await page.locator('[data-workflow-mode]').selectOption('reviewed',{force:true});
 const fields=dialog.locator('fieldset');await fields.nth(0).getByLabel(await text('模型'),{exact:true}).fill('planner-fixture');await fields.nth(1).getByLabel(await text('模型'),{exact:true}).fill('worker-fixture');
 await dialog.getByLabel(await text('任务需求'),{exact:true}).fill('Implement feature from the browser');
 await dialog.getByLabel(await text('验证命令（可选）'),{exact:true}).fill('test -f source.txt');
 await click('交换主辅');await click('开始协作');await waitButton('确认计划并执行');
 assert.deepEqual(await evidence(),{model_calls:1,usage_rows:1,ledger_spends:1,balance:9990});
 // Apply a new role generation before accepting the plan. Replanning costs one
 // turn; old participants must not execute the retired plan.
 await page.locator('.collaboration-settings>summary').click();
 await click('交换主辅');await click('应用主辅配置');
 await page.waitForFunction(()=>!document.querySelector('form[aria-busy]'));
 await page.waitForFunction(async()=>{const r=await fetch('/fixture/evidence');return (await r.json()).model_calls===2;});await waitButton('确认计划并执行');
 await click('确认计划并执行');await waitButton('下载协作结果');
 assert.deepEqual(await evidence(),{model_calls:4,usage_rows:4,ledger_spends:4,balance:9960});
 await click('查看文件变更');await page.locator('.collaboration-evidence').getByRole('button',{name:/source\.txt/}).click();
 await page.waitForFunction(()=>document.querySelector('.collaboration-diff')?.textContent.includes('+implemented'));
 const diff=await page.locator('.collaboration-diff').innerText();assert.ok(diff.includes('-original'));
 const download=page.waitForEvent('download');await click('下载协作结果');const result=await download;assert.match(result.suggestedFilename(),/^collaboration-[a-f0-9]{32}\.zip$/);assert.equal(await result.failure(),null);
 const runID=await page.evaluate(()=>Array.from(document.querySelectorAll('select')).find(s=>Array.from(s.options).some(o=>/Implement feature/.test(o.textContent)))?.value);assert.match(runID,/^[a-f0-9]{32}$/);
 await page.reload();await page.locator('#btn-collaboration:not([hidden])').click();
 await page.waitForFunction(id=>Array.from(document.querySelectorAll("select")).some(s=>Array.from(s.options).some(o=>o.value===id)),runID);
 await page.evaluate(async id=>{const s=Array.from(document.querySelectorAll('select')).find(s=>Array.from(s.options).some(o=>o.value===id));(await import('/js/select.js')).setSelectValue(s,id);s.dispatchEvent(new Event('change',{bubbles:true}));},runID);
 await waitButton('下载协作结果');assert.equal((await evidence()).model_calls,4,'reload repeated model calls');
 await page.setViewportSize({width:390,height:844});await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/collaboration-e2e-${browserEngine()}.png`});
 assert.ok(await dialog.evaluate(e=>e.scrollWidth<=e.clientWidth+1));
 await page.locator('.collaboration-more>summary').click();
 await click('删除任务和副本');await dialog.getByLabel(await text('我确认删除任务副本'),{exact:true}).check();await click('确认删除');
 await page.waitForFunction(()=>!document.querySelector('form[aria-busy]'));
 await page.getByRole('status').getByText(await text('任务已删除'),{exact:true}).waitFor();
 const token='client-fixture-alice';const deleted=await fetch(`${base}/api/collaboration/runs/${runID}`,{headers:{Authorization:`Bearer ${token}`}});const body=await deleted.json();assert.equal(body.run.state,'deleted');assert.equal(body.run.request,undefined);
 assert.equal((await fetch(`${base}/api/collaboration/runs/${runID}/download`,{headers:{Authorization:`Bearer ${token}`}})).status,409);
 assert.deepEqual(await evidence(),{model_calls:4,usage_rows:4,ledger_spends:4,balance:9960});assert.deepEqual(errors,[]);
 console.log('Real backend collaboration browser passed: create, swap, reconfigure/replan, plan approval, execution, verification before review, diff, ZIP, reload, delete, four calls and exactly-once settlement.');
}catch(e){console.error(await page.locator('body').innerText());console.error(errors);throw e;}finally{await browser.close();await fetch(base+'/fixture/done',{method:'POST'});}
