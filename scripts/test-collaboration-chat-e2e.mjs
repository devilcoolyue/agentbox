// Isolated real Go server/SQLite/scheduler; only the CLI/Docker outputs are synthetic.
import assert from 'node:assert/strict';
import {mkdir} from 'node:fs/promises';
import {launchBrowser,browserEngine,cancelledFetchError} from './playwright-launch.mjs';
const base=process.argv[2];if(!/^http:\/\/127\.0\.0\.1:\d+$/.test(base||''))throw Error('isolated loopback fixture required');
const browser=await launchBrowser(),page=await browser.newPage({viewport:{width:1440,height:1000}}),errors=[];page.on('pageerror',e=>{if(!cancelledFetchError(e))errors.push(e.stack)});page.setDefaultTimeout(20000);
const evidence=async()=>fetch(base+'/fixture/evidence').then(r=>r.json());
const click=label=>page.getByRole('button',{name:label,exact:true}).click();
try{
 await page.addInitScript(()=>{try{localStorage.setItem('agentbox.language','zh-CN')}catch{}});
 await page.goto(base+'/fixture/chat');await page.waitForFunction(()=>window.ready&&document.getElementById('chat-execution-mode').classList.contains('select-native'));
 await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();
 await page.locator('[data-workflow-mode]').selectOption('reviewed',{force:true});
 const fields=page.locator('.collaboration-composer fieldset');await fields.nth(0).getByLabel('模型',{exact:true}).fill('planner-fixture');await fields.nth(1).getByLabel('模型',{exact:true}).fill('worker-fixture');await page.locator('.collaboration-composer').getByLabel('验证命令（可选）',{exact:true}).evaluate(e=>{e.closest('details').open=true});await page.locator('.collaboration-composer').getByLabel('验证命令（可选）',{exact:true}).fill('test -f source.txt');
 await fields.nth(1).locator('[data-role-model-mode]').selectOption('auto',{force:true});
 await page.locator('#chat-input').fill('Implement feature inside this conversation');await page.locator('#chat-send').click();await page.getByRole('button',{name:'确认计划并执行',exact:true}).waitFor({timeout:45000});
 assert.equal(await page.locator('dialog[open]').count(),0);assert.equal((await evidence()).model_calls,1);
 await click('补充要求');await page.locator('#chat-input').fill('Also keep the original Git metadata');await page.locator('#chat-send').click();
 await page.waitForFunction(async()=>{const e=await fetch('/fixture/evidence').then(r=>r.json());return e.model_calls===2});await page.locator('.collaboration-thread-body[data-config-version="2"][data-plan-version="2"] button').filter({hasText:'确认计划并执行'}).waitFor();await click('确认计划并执行');
 await page.getByRole('button',{name:'应用到当前项目',exact:true}).waitFor({timeout:45000});assert.deepEqual(await evidence(),{model_calls:4,usage_rows:4,ledger_spends:4,balance:9960});
 const run=await page.locator('[data-collaboration-run]').getAttribute('data-collaboration-run');
 const routed=await fetch(`${base}/api/collaboration/runs/${run}`,{headers:{Authorization:'Bearer client-fixture-alice'}}).then(r=>r.json());assert.equal(routed.run.request.decision.version,1);assert.equal(routed.run.request.decision.roles.executor.mode,'auto');assert.equal(routed.decision.state,'frozen');assert.equal(routed.decision.result.decisions.coordinator.source,'manual');assert.ok(['builtin','fallback'].includes(routed.decision.result.decisions.executor.source));assert.equal(routed.cost.decision_calls,0);
 await page.reload();await page.waitForFunction(()=>window.ready);await page.getByRole('button',{name:'应用到当前项目',exact:true}).waitFor();assert.equal(await page.locator('[data-collaboration-run]').count(),1);
 await click('应用到当前项目');await page.locator('#dlg-ask[open]').waitFor();await page.locator('#ask-ok').click();await page.getByText('已应用到当前项目',{exact:true}).first().waitFor();
 const result=await page.evaluate(async()=>{const {S}=await import('/js/state.js');const r=await fetch(`/api/sessions/${S.current.id}/file?path=source.txt`,{headers:{Authorization:`Bearer ${S.token}`}});if(!r.ok)throw Error('file read failed');return r.text()});assert.ok(JSON.stringify(result).includes('implemented'));
 const state=await fetch(`${base}/api/collaboration/runs/${run}/application`,{headers:{Authorization:'Bearer client-fixture-alice'}}).then(r=>r.json());assert.equal(state.application.state,'completed');assert.deepEqual(await evidence(),{model_calls:4,usage_rows:4,ledger_spends:4,balance:9960});
 // The next task uses the same composer and real server, with one executor.
 // No synthetic browser response fabricates completion or a model review.
 await page.locator('.collaboration-detail').getByRole('button',{name:'关闭',exact:true}).click();
 await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();
 await page.locator('[data-workflow-mode]').selectOption('direct',{force:true});
 await fields.nth(1).locator('[data-role-model-mode]').selectOption('fixed',{force:true});
 await page.locator('#chat-input').fill('Implement the next bounded task directly');await page.locator('#chat-send').click();
 const directCard=page.locator('[data-collaboration-run]').filter({hasText:'执行完成，结果待应用'});
 await directCard.waitFor({timeout:45000});await directCard.getByText('未做独立模型评审',{exact:true}).waitFor();
 assert.equal(await page.getByRole('button',{name:'确认计划并执行',exact:true}).count(),0);
 const directRun=await directCard.getAttribute('data-collaboration-run');
 const directView=await fetch(`${base}/api/collaboration/runs/${directRun}`,{headers:{Authorization:'Bearer client-fixture-alice'}}).then(r=>r.json());
 assert.deepEqual(directView.run.request.workflow,{version:1,mode:'direct'});assert.equal(directView.run.plan_version,0);assert.equal(directView.run.request.decision,undefined);
 assert.equal(directView.step_usage.length,1);assert.equal(directView.step_usage[0].kind,'execute');assert.equal(directView.step_usage[0].role,'executor');assert.equal(directView.cost.turns,1);
 assert.deepEqual(await evidence(),{model_calls:5,usage_rows:5,ledger_spends:5,balance:9950});
 await page.reload();await page.waitForFunction(()=>window.ready);await directCard.getByText('未做独立模型评审',{exact:true}).waitFor();
 assert.deepEqual(await evidence(),{model_calls:5,usage_rows:5,ledger_spends:5,balance:9950});
 await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/collaboration-chat-real-${browserEngine()}.png`,fullPage:true});assert.deepEqual(errors,[]);
 console.log('Real chat collaboration passed: reviewed task with supplement/replan/apply, then direct task with one executor and no model review, reload, five total model invocations and exactly-once ledger. CLI/provider outputs are synthetic.');
}catch(e){console.error(await page.locator('body').innerText());console.error(errors);throw e;}finally{await browser.close();await fetch(base+'/fixture/done',{method:'POST'});}
