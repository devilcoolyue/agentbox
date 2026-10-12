// Real browser -> production Go handlers/decision adapter -> live Mindshub.
// Docker/Claude/Codex outputs remain explicitly synthetic fixture boundaries.
import assert from 'node:assert/strict';
import {mkdir,writeFile} from 'node:fs/promises';
import {join} from 'node:path';
import {launchBrowser,browserEngine,cancelledFetchError} from './playwright-launch.mjs';

const [base,output,...options]=process.argv.slice(2),timeout=options.includes('--expect-timeout');
if(!/^http:\/\/127\.0\.0\.1:\d+$/.test(base||'')||!output)throw Error('owned loopback fixture and report directory required');
const browser=await launchBrowser(),page=await browser.newPage({viewport:{width:1440,height:1000}}),errors=[];
page.on('pageerror',e=>{if(!cancelledFetchError(e))errors.push(e.message)});page.setDefaultTimeout(30000);
const authorization={Authorization:'Bearer client-fixture-alice'};
const api=async path=>{const r=await fetch(base+path,{headers:authorization});assert.equal(r.status,200,path);return r.json()};
const evidence=()=>api('/fixture/evidence');
const click=label=>page.getByRole('button',{name:label,exact:true}).click();
const expectedWorker=timeout?'worker-fixture':'economy',expectedBalance=timeout?9870:9969;
let runID,createdBody,createPath;
page.on('request',request=>{if(request.method()==='PUT'&&/\/api\/collaboration\/runs\/[a-f0-9]{32}$/.test(new URL(request.url()).pathname)){createPath=new URL(request.url()).pathname;createdBody=request.postData()}});
const report={decision_backend:'live_mindshub',cli_backend:'synthetic',expect_timeout:timeout};
try{
 await page.addInitScript(()=>{if(window.top===window)localStorage.setItem('agentbox.language','zh-CN')});
 await page.goto(base+'/fixture/chat');await page.waitForFunction(()=>window.ready&&document.getElementById('chat-execution-mode').classList.contains('select-native'));
 await page.locator('#chat-execution-mode').selectOption('collaboration',{force:true});await page.locator('#chat-collaboration-setup').click();
 const settings=page.locator('.collaboration-composer'),roles=settings.locator('fieldset');
 await roles.nth(0).getByLabel('模型',{exact:true}).fill('planner-fixture');await roles.nth(1).getByLabel('模型',{exact:true}).fill('worker-fixture');
 await roles.nth(1).locator('[data-role-model-mode]').selectOption('auto',{force:true});await settings.locator('[data-use-jev]').check();
 const verify=settings.getByLabel('验证命令（可选）',{exact:true});await verify.evaluate(e=>e.closest('details').open=true);await verify.fill('test -f source.txt');
 await page.locator('#chat-input').fill('这是一个简单的局部文本替换任务：只把 source.txt 里的 original 改成 implemented，其他文件保持原样。单个文件的固定字符串替换，没有额外逻辑；通过已有测试确认内容即可。');
 await page.locator('#chat-send').click();await page.getByRole('button',{name:'确认计划并执行',exact:true}).waitFor({timeout:60000});
 runID=await page.locator('[data-collaboration-run]').getAttribute('data-collaboration-run');assert.match(runID,/^[a-f0-9]{32}$/);
 const routed=await api(`/api/collaboration/runs/${runID}`);report.after_routing=routed;
 assert.equal(routed.decision.state,'frozen');assert.equal(routed.decision.attempt.provider,'mindshub');assert.equal(routed.cost.decision_calls,1);
 assert.equal(routed.run.request.roles.executor.model,'worker-fixture');assert.equal(routed.run.request.decision.roles.coordinator.mode,'fixed');assert.equal(routed.run.request.decision.roles.executor.mode,'auto');assert.equal(routed.run.request.decision.provider,'jev');
 assert.equal(routed.decision.result.decisions.coordinator.source,'manual');assert.equal(routed.decision.result.roles.coordinator.model,'planner-fixture');
 assert.equal(routed.decision.result.roles.executor.model,expectedWorker);assert.equal(routed.decision.result.decisions.executor.source,timeout?'fallback':'jev');
 assert.equal(routed.cost.decision_partial,timeout);assert.equal(routed.decision.attempt.state,timeout?'unknown':'settled');
 const frozen=JSON.stringify(routed.decision.result),initialEvidence=await evidence();report.initial_evidence=initialEvidence;
 assert.equal(initialEvidence.model_calls,1);assert.equal(initialEvidence.decision_calls,1);assert.equal(initialEvidence.decision_usage_rows,timeout?0:1);
 await click('补充要求');await page.locator('#chat-input').fill('补充：保留原项目 Git 信息，仅完成原定的简单文本替换。');await page.locator('#chat-send').click();
 await page.waitForFunction(async()=>{const r=await fetch('/fixture/evidence');return (await r.json()).model_calls===2});
 await page.locator('.collaboration-thread-body[data-config-version="2"][data-plan-version="2"] button').filter({hasText:'确认计划并执行'}).waitFor();await click('确认计划并执行');
 await page.getByRole('button',{name:'应用到当前项目',exact:true}).waitFor({timeout:60000});
 const completed=await api(`/api/collaboration/runs/${runID}`);report.before_apply=completed;
 assert.equal(JSON.stringify(completed.decision.result),frozen);assert.equal(completed.configuration.roles.executor.model,expectedWorker);
 const finished=await evidence();report.before_apply_evidence=finished;
 assert.equal(finished.model_calls,4);assert.deepEqual(finished.executed_models,['planner-fixture','planner-fixture',expectedWorker,'planner-fixture']);
 assert.equal(finished.decision_calls,1);assert.equal(finished.usage_rows,timeout?4:5);assert.equal(finished.ledger_spends,4);assert.equal(finished.balance,expectedBalance);assert.equal(finished.cli_cost_micro_usd,10000-expectedBalance);
 const decisionRows=finished.usage.filter(r=>r.kind==='decision');assert.equal(decisionRows.length,timeout?0:1);
 if(!timeout){assert.equal(decisionRows[0].provider,'mindshub');assert.equal(decisionRows[0].model,'jev-1.13.0');assert.ok(decisionRows[0].input_tokens>0);assert.equal(decisionRows[0].cost_micro_usd,0)}
 assert.ok(createdBody&&createPath);const replay=await fetch(base+createPath,{method:'PUT',headers:{...authorization,'Content-Type':'application/json'},body:createdBody});assert.equal(replay.status,200);assert.equal((await replay.json()).replayed,true);
 await page.reload();await page.waitForFunction(()=>window.ready);await page.getByRole('button',{name:'应用到当前项目',exact:true}).waitFor();assert.equal(await page.locator('[data-collaboration-run]').count(),1);
 await click('应用到当前项目');await page.locator('#dlg-ask[open]').waitFor();await page.locator('#ask-ok').click();await page.getByText('已应用到当前项目',{exact:true}).first().waitFor();
 const file=await page.evaluate(async()=>{const{S}=await import('/js/state.js');const r=await fetch(`/api/sessions/${S.current.id}/file?path=source.txt`,{headers:{Authorization:`Bearer ${S.token}`}});if(!r.ok)throw Error('file inspection failed');return r.text()});assert.ok(file.includes('implemented'));
 const application=await api(`/api/collaboration/runs/${runID}/application`);assert.equal(application.application.state,'completed');report.application=application;
 const applied=await evidence();report.after_apply_evidence=applied;assert.deepEqual(applied,finished);
 report.after_apply=await api(`/api/collaboration/runs/${runID}`);assert.equal(JSON.stringify(report.after_apply.decision.result),frozen);
 await mkdir('output/playwright',{recursive:true});const screenshot=`output/playwright/decision-live-${timeout?'timeout':'selection'}-${browserEngine()}.png`;
 await page.screenshot({path:screenshot,fullPage:true});report.screenshot=screenshot;assert.deepEqual(errors,[]);report.status='passed';
 console.log(`Live Mindshub ${timeout?'deadline fallback':'model selection'} passed through real browser/backend/apply; synthetic CLI: 4 calls, selected ${expectedWorker}, one decision attempt, balance ${expectedBalance}.`);
}catch(error){report.status='failed';report.error=error.message;report.page_errors=errors;try{report.final_evidence=await evidence();if(runID)report.final_run=await api(`/api/collaboration/runs/${runID}`)}catch{}throw error;
}finally{await writeFile(join(output,'browser-evidence.json'),JSON.stringify(report,null,2)+'\n');await browser.close();await fetch(base+'/fixture/done',{method:'POST'});}
