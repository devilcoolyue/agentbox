import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Shared synthetic browser suite: exercises history and real WS rendering,
// including stream replacement and the Codex exec replay path. No provider calls.
export async function chatFooterSmoke(page, { setHistory, send }) {
 const answer = '已把回答信息收拢到底部，阅读正文时更清爽。\n\n### 实现要点\n\n- 保存每轮的模型与推理设置\n- 复制正文，保留 **Markdown**\n\n```ts\nconst model = "gpt-5.5";\nconsole.log(model);\n```';
 const makeTurn = (id, model, effort, extra = {}) => ({id,model,effort,control:'effort',...extra});
 const history = [
  {kind:'user',ts:'2026-09-24T08:00:00Z',text:'旧版记录还能查看吗？'},
  {kind:'event',ts:'2026-09-24T08:01:00Z',event:{type:'assistant',message:{model:'claude-history',content:[{type:'text',text:'可以阅读旧记录。缺少的信息会明确标出。'}]}}},
  {kind:'event',ts:'2026-09-24T08:01:01Z',event:{type:'result',subtype:'success',total_cost_usd:0.0125}},
  {kind:'user',ts:'2026-09-26T08:20:00Z',text:'请改进回答底部的信息与操作。',turn:makeTurn('saved','gpt-5.5','high')},
  {kind:'event',ts:'2026-09-26T08:21:00Z',event:{type:'item.completed',item:{type:'reasoning',text:'PRIVATE THINKING'}}},
  {kind:'event',ts:'2026-09-26T08:21:01Z',event:{type:'item.started',item:{type:'command_execution',command:'PRIVATE TOOL LOG'}}},
  {kind:'event',ts:'2026-09-26T08:22:00Z',event:{type:'item.completed',item:{type:'agent_message',text:answer}}},
  {kind:'event',ts:'2026-09-26T08:22:01Z',event:{type:'turn.completed',usage:{input_tokens:820,output_tokens:215}}},
  {kind:'status',ts:'2026-09-26T08:22:02Z',state:'idle'},
 ];
 const reload = async () => { await page.reload(); await page.locator('.answer-footer').last().waitFor(); };
 setHistory(history,{saved:{turn_id:'saved',cost_micro_usd:31400,source:'table'}});
 await reload();
 const footers = page.locator('.answer-footer');
 // Only time and amount stay visible; model, reasoning and billing notes are in the details.
 const details = f => f.locator('.answer-details').evaluate(e=>e.textContent);
 assert.equal(await footers.count(),2);
 assert.match(await details(footers.first()),/模型claude-history/);
 assert.match(await details(footers.first()),/推理强度未记录/);
 assert.match(await details(footers.last()),/模型gpt-5.5/);
 assert.match(await details(footers.last()),/推理强度高/);
 assert.match(await footers.last().innerText(),/2026-09-26 08:22/);
 assert.doesNotMatch(await footers.last().innerText(),/gpt-5.5/,'model belongs in the details');
 assert.equal(await footers.last().locator('.answer-cost').isVisible(),true);
 assert.equal(await footers.last().locator('.answer-cost').innerText(),'$0.0314');
 assert.match(await footers.last().locator('.answer-cost').getAttribute('data-tip'),/按单价估算/);
 assert.equal(await footers.first().locator('.answer-cost').innerText(),'$0.0125');
 assert.match(await footers.first().locator('.answer-cost').getAttribute('data-tip'),/客户端上报/);
 await footers.last().getByRole('button',{name:'回答详情',exact:true}).click();
 assert.equal(await footers.last().locator('.chip.result').isVisible(),true);
 assert.match(await footers.last().innerText(),/入 820 · 出 215 tokens/);
 await footers.last().getByRole('button',{name:'回答详情',exact:true}).click();
 await page.context().grantPermissions(['clipboard-read','clipboard-write']);
 await footers.last().getByRole('button',{name:'复制回答',exact:true}).click();
 await footers.last().getByRole('button',{name:'已复制回答',exact:true}).waitFor();
 assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),answer);
 // The footer uses persisted values, even while the composer has another model.
 assert.doesNotMatch(await page.locator('#btn-pick').innerText(),/gpt-5.5/);
 const savedFooter = await footers.last().innerText();
 await reload();
 assert.equal(await footers.last().innerText(),savedFooter);
 await mkdir('output/playwright',{recursive:true});
 for (const theme of ['light','dark']) {
  await page.evaluate(t=>document.documentElement.dataset.theme=t,theme);
  await page.setViewportSize({width:1280,height:900});
  await footers.last().scrollIntoViewIfNeeded();
  await page.mouse.move(0,0);
  await page.screenshot({path:`output/playwright/answer-footer-${theme}.png`,animations:'disabled'});
  await page.setViewportSize({width:390,height:844});
  await footers.last().scrollIntoViewIfNeeded();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
  const box = await footers.last().boundingBox();
  assert.ok(box.x>=0 && box.x+box.width<=390);
  await page.screenshot({path:`output/playwright/answer-footer-mobile-${theme}.png`,animations:'disabled'});
 }
 await page.setViewportSize({width:1280,height:900});
 await page.evaluate(()=>document.documentElement.dataset.theme='light');

 for(const [cost,label] of [
  [{source:'provider',cost_micro_usd:97500},'$0.0975'],
  [{source:'table',cost_micro_usd:1100},'$0.0011'],
  [{source:'table',cost_micro_usd:0},'$0.0000'],
  [{source:'table',cost_micro_usd:1},'$0.000001'],
  [{source:'unpriced',cost_micro_usd:0},'未定价'],
  [{source:'unknown',cost_micro_usd:0},'费用未记录'],
  [{source:'mixed',cost_micro_usd:2000,partial:true},'≥ $0.0020'],
 ]) {
  setHistory(history,{saved:{turn_id:'saved',...cost}});
  await reload();
  assert.equal(await footers.last().locator('.answer-cost').innerText(),label);
  assert.equal(await footers.last().locator('.answer-cost').isVisible(),true);
  assert.equal(await footers.last().locator('.answer-details').isVisible(),false);
 }

 // Explicit budget, inherited defaults, unsupported models, missing metadata.
 for (const [turn,label] of [
  [makeTurn('budget','claude-fixture','high',{control:'budget',budget_tokens:24000}), '推理强度思考预算 24,000 tokens'],
  [makeTurn('default','fixture',''), '推理强度默认'],
  [makeTurn('unsupported','fixture-none','',{unsupported:true}), '推理强度不支持调整'],
  [undefined, '推理强度未记录'],
 ]) {
  setHistory([{kind:'user',ts:'2026-09-26T08:00:00Z',text:'fixture',turn},{kind:'event',ts:'2026-09-26T08:01:00Z',event:{type:'item.completed',item:{type:'agent_message',text:'fixture answer'}}}]);
  await reload();
  assert.match(await details(footers.last()),new RegExp(label));
  if(!turn) assert.match(await details(footers.last()),/模型未记录/);
 }
 // Truncated histories receive context separately; don't invent a user bubble.
 setHistory([{kind:'turn_context',turn:makeTurn('long','historic-long-model','low')}, {kind:'event',ts:'2026-09-26T09:00:00Z',event:{type:'item.completed',item:{type:'agent_message',text:'long turn'}}}]);
 await reload();
 assert.match(await details(footers.last()),/historic-long-model/);
 assert.equal(await page.locator('#chat-log .msg.user').count(),0);

 // Stream -> completed event must replace temporary text, not copy it twice.
 const emit = msg => send(msg);
 emit({type:'user_message',text:'live question',turn:makeTurn('live','requested-model','medium'),ts:'2026-09-26T10:00:00Z'});
 emit({type:'status',state:'running'});
 emit({type:'agent_event',event:{type:'stream_event',event:{type:'content_block_start',content_block:{type:'text'}}}});
 emit({type:'agent_event',ts:'2026-09-26T10:01:00Z',event:{type:'stream_event',event:{type:'content_block_delta',delta:{type:'text_delta',text:'live **answer**'}}}});
 emit({type:'agent_event',event:{type:'stream_event',event:{type:'content_block_stop'}}});
 emit({type:'agent_event',ts:'2026-09-26T10:01:00Z',event:{type:'assistant',message:{model:'reported-model',content:[{type:'text',text:'live **answer**'}]}}});
 emit({type:'status',state:'idle',ts:'2026-09-26T10:01:01Z'});
 await footers.last().locator('.answer-details .ad-v').filter({hasText:'reported-model'}).waitFor({state:'attached'});
 assert.equal(await footers.last().locator('.answer-cost').innerText(),'费用待结算');
 emit({type:'turn_cost',cost:{turn_id:'other-thread',cost_micro_usd:999000,source:'table'}});
 emit({type:'turn_cost',cost:{turn_id:'live',cost_micro_usd:42300,source:'provider'}});
 await footers.last().locator('.answer-cost').filter({hasText:'$0.0423'}).waitFor();
 assert.equal(await page.locator('#chat-log .turn').last().locator('.msg.agent').count(),1);
 await footers.last().getByRole('button',{name:'复制回答',exact:true}).click();
 await footers.last().getByRole('button',{name:'已复制回答',exact:true}).waitFor();
 assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),'live **answer**');
 assert.match(await details(footers.last()),/请求的是 requested-model/);

 emit({type:'user_message',text:'replay question',turn:makeTurn('replay','exec-model','xhigh')});
 emit({type:'status',state:'running'});
 emit({type:'agent_event',ts:'2026-09-26T11:00:00Z',event:{type:'item.completed',item:{type:'agent_message',text:'Replay answer one.'}}});
 emit({type:'agent_event',ts:'2026-09-26T11:00:01Z',event:{type:'item.completed',item:{type:'agent_message',text:'Replay answer two.'}}});
 emit({type:'status',state:'idle'});
 await page.waitForFunction(()=>document.querySelector('#chat-log .turn:last-child')?.textContent.includes('Replay answer two.') && !document.querySelector('#chat-log .streaming'));
 assert.match(await details(footers.last()),/exec-model/);
 assert.match(await details(footers.last()),/推理强度极高/);
 emit({type:'turn_cost',cost:{turn_id:'replay',cost_micro_usd:1234,source:'table'}});
 await footers.last().locator('.answer-cost').filter({hasText:'$0.0012'}).waitFor();
 await footers.last().getByRole('button',{name:'复制回答',exact:true}).click();
 await footers.last().getByRole('button',{name:'已复制回答',exact:true}).waitFor();
 assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),'Replay answer one.\n\nReplay answer two.');

 // A denied clipboard must not show success if the legacy fallback also fails.
 await reload();
 await page.evaluate(()=>{
  Object.defineProperty(navigator.clipboard,'writeText',{configurable:true,value:async()=>{throw Error('denied');}});
  document.execCommand=()=>false;
 });
 await footers.last().getByRole('button',{name:'复制回答',exact:true}).click();
 await page.getByText('复制失败，请选择回答后手动复制',{exact:true}).waitFor();
 assert.equal(await footers.last().getByRole('button',{name:'已复制回答',exact:true}).count(),0);
 setHistory([]);
 await page.reload();
 await page.locator('#chat-input').waitFor({state:'visible'});
 console.log('Answer footer: visible committed cost and sources, legacy CLI costs, unpriced/partial/zero cost, live settlement, persisted settings/time, history window, clipboard Markdown/failure, streaming replacement, exec replay, desktop/mobile light/dark passed');
}
