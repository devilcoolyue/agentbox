import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';

// Shared synthetic browser suite: the composer's context meter (the web form of
// the CLI status line) from history and live events, plus the footer of a
// local slash command. No provider calls.
export async function chatContextSmoke(page, { setHistory, send, socket, setAgent }) {
 const meter = page.locator('#chat-context');
 const value = () => meter.locator('.ctx-value').innerText();
 const tip = () => meter.getAttribute('data-tip');
 const reload = async () => {
  const before = socket();
  await page.reload();
  await page.locator('#chat-log .turn, #chat-hero:not(.hidden)').first().waitFor();
  for (let i = 0; socket() === before; i++) { assert.ok(i < 100, 'chat socket did not reconnect'); await page.waitForTimeout(20); }
 };
 const turn = id => ({id,model:'claude-opus-5',effort:'',control:'effort'});
 const history = [
  {kind:'user',ts:'2026-10-14T08:00:00Z',text:'梳理一下认证模块',turn:turn('ctx-1')},
  // A subagent's own context must not move the meter.
  {kind:'event',ts:'2026-10-14T08:00:30Z',event:{type:'assistant',parent_tool_use_id:'toolu_sub',message:{id:'sub',model:'claude-haiku-4-5',content:[{type:'text',text:'sub'}],usage:{input_tokens:190000}}}},
  {kind:'event',ts:'2026-10-14T08:01:00Z',event:{type:'assistant',parent_tool_use_id:null,message:{id:'m1',model:'claude-opus-5',content:[{type:'text',text:'认证模块由三部分组成。'}],usage:{input_tokens:2000,cache_creation_input_tokens:8000,cache_read_input_tokens:80000,output_tokens:300}}}},
  {kind:'event',ts:'2026-10-14T08:01:01Z',event:{type:'result',subtype:'success',is_error:false,num_turns:2,modelUsage:{'claude-haiku-4-5':{contextWindow:100000},'claude-opus-5':{contextWindow:200000}}}},
  {kind:'status',ts:'2026-10-14T08:01:02Z',state:'idle'},
  // /context: the CLI answers by itself; nothing changes the meter or costs money.
  {kind:'user',ts:'2026-10-14T08:02:00Z',text:'/context',turn:turn('ctx-local')},
  {kind:'event',ts:'2026-10-14T08:02:01Z',event:{type:'assistant',parent_tool_use_id:null,message:{id:'local',model:'<synthetic>',content:[{type:'text',text:'## Context Usage\n\n**Tokens:** 90k / 200k (45%)'}],usage:{input_tokens:0,output_tokens:0}}}},
  {kind:'event',ts:'2026-10-14T08:02:01Z',event:{type:'result',subtype:'success',is_error:false,num_turns:0,total_cost_usd:0,modelUsage:{}}},
  {kind:'status',ts:'2026-10-14T08:02:02Z',state:'idle',local:true},
 ];
 setAgent('claude');
 setHistory(history,{'ctx-1':{turn_id:'ctx-1',cost_micro_usd:12000,source:'table'}});
 await reload();
 await meter.waitFor();
 assert.equal(await value(),'45%');
 assert.equal(await meter.getAttribute('data-level'),'ok');
 assert.match(await tip(),/90\.0K \/ 200\.0K tokens（45%）/);
 assert.match(await meter.getAttribute('aria-label'),/45%/);
 const footers = page.locator('.answer-footer');
 assert.equal(await footers.first().locator('.answer-cost').innerText(),'$0.0120');
 assert.equal(await footers.last().locator('.answer-cost').innerText(),'未调用模型');
 assert.match(await footers.last().locator('.answer-cost').getAttribute('data-tip'),/本地执行/);

 // Live: every new main request updates the meter; compaction waits for the next one.
 const start = (tokens, extra = {}) => send({type:'agent_event',event:{type:'stream_event',parent_tool_use_id:null,event:{type:'message_start',message:{model:'claude-opus-5',usage:{input_tokens:1000,cache_read_input_tokens:tokens-1000,output_tokens:1}}},...extra}});
 send({type:'user_message',text:'继续',turn:turn('ctx-live'),ts:'2026-10-14T08:03:00Z'});
 send({type:'status',state:'running'});
 start(160000);
 await page.waitForFunction(()=>document.querySelector('#chat-context .ctx-value')?.textContent==='80%');
 assert.equal(await meter.getAttribute('data-level'),'warn');
 start(500000, {parent_tool_use_id:'toolu_sub'});
 start(175000);
 await page.waitForFunction(()=>document.querySelector('#chat-context .ctx-value')?.textContent==='88%');
 assert.equal(await meter.getAttribute('data-level'),'critical');
 assert.match(await tip(),/自动压缩/);
 await mkdir('output/playwright',{recursive:true});
 await page.screenshot({path:'output/playwright/chat-context-critical.png',animations:'disabled'});
 send({type:'agent_event',event:{type:'system',subtype:'compact_boundary',compact_metadata:{trigger:'auto',pre_tokens:175000}}});
 await page.waitForFunction(()=>document.querySelector('#chat-context .ctx-value')?.textContent==='—');
 assert.match(await tip(),/压缩前 175\.0K tokens/);
 start(30000);
 await page.waitForFunction(()=>document.querySelector('#chat-context .ctx-value')?.textContent==='15%');
 send({type:'status',state:'idle'});

 // A first turn shows the size until a result reports the window.
 setHistory([
  {kind:'user',ts:'2026-10-14T09:00:00Z',text:'hi',turn:turn('ctx-new')},
  {kind:'event',ts:'2026-10-14T09:00:01Z',event:{type:'assistant',message:{id:'n1',model:'claude-sonnet-5',content:[{type:'text',text:'hello'}],usage:{input_tokens:12345}}}},
 ]);
 await reload();
 await meter.waitFor();
 assert.equal(await value(),'12.3K');
 assert.match(await tip(),/本轮结束后显示占比/);

 // Narrow screens keep the meter inside the composer without overflow.
 setHistory(history,{});
 await reload();
 await page.setViewportSize({width:390,height:844});
 assert.equal(await meter.isVisible(),true);
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
 const bar = await page.locator('.composer-bar').boundingBox(), box = await meter.boundingBox();
 assert.ok(box.x >= bar.x && box.x + box.width <= bar.x + bar.width + 0.5, 'meter overflows the composer bar');
 await page.screenshot({path:'output/playwright/chat-context-mobile.png',animations:'disabled'});
 await page.setViewportSize({width:1280,height:900});

 // /context draws the CLI's grid from its structured result; /compact marks the
 // boundary, hides the messages the CLI re-sends after it, and updates the meter.
 const usage = {model:'claude-opus-5',total_tokens:93300,raw_max_tokens:200000,percentage:47,
  categories:[{name:'System prompt',tokens:4000,kind:'used'},{name:'System tools',tokens:17600,kind:'used'},{name:'MCP tools (deferred)',tokens:1400,kind:'deferred'},
   {name:'Memory files',tokens:2600,kind:'used'},{name:'Skills',tokens:6000,kind:'used'},{name:'Messages',tokens:63100,kind:'used'},
   {name:'Autocompact buffer',tokens:33000,kind:'buffer'},{name:'Free space',tokens:73600,kind:'free'}],
  mcp_tools:[{name:'mcp__docs__read',server_name:'docs',tokens:300}],memory_files:[{path:'/home/agent/.claude/CLAUDE.md',type:'User',tokens:2600}],
  skills:[{name:'pdf',source:'plugin',tokens:500},{name:'<img src=x onerror=alert(1)>',source:'userSettings',tokens:10}]};
 const contextAnswer = {type:'assistant',uuid:'ctx-uuid-1',parent_tool_use_id:null,context_usage:usage,local_command_run:{command:'context',args:''},
  message:{id:'local-ctx',model:'<synthetic>',content:[{type:'text',text:'## Context Usage\n\n**Tokens:** 93.3k / 200k (47%)'}],usage:{input_tokens:0,output_tokens:0}}};
 setHistory([
  {kind:'user',ts:'2026-10-14T10:00:00Z',text:'/context',turn:turn('ctx-card')},
  {kind:'event',ts:'2026-10-14T10:00:01Z',event:contextAnswer},
  {kind:'event',ts:'2026-10-14T10:00:01Z',event:{type:'result',subtype:'success',is_error:false,num_turns:0,local_command:'context',modelUsage:{'claude-opus-5':{contextWindow:200000}}}},
  {kind:'status',ts:'2026-10-14T10:00:02Z',state:'idle',local:true},
  {kind:'user',ts:'2026-10-14T10:01:00Z',text:'/compact',turn:turn('ctx-compact')},
  {kind:'event',ts:'2026-10-14T10:01:01Z',event:{type:'system',subtype:'status',status:'compacting'}},
  {kind:'event',ts:'2026-10-14T10:01:20Z',event:{type:'system',subtype:'status',status:null,compact_result:'success'}},
  {kind:'event',ts:'2026-10-14T10:01:20Z',event:{type:'system',subtype:'compact_boundary',compact_metadata:{trigger:'manual',pre_tokens:95213,post_tokens:2802,preserved_messages:{uuids:['ctx-uuid-1'],all_uuids:['ctx-uuid-1']}}}},
  {kind:'event',ts:'2026-10-14T10:01:20Z',event:{type:'user',isSynthetic:true,message:{role:'user',content:'This session is being continued…'}}},
  {kind:'event',ts:'2026-10-14T10:01:20Z',event:contextAnswer},
  {kind:'event',ts:'2026-10-14T10:01:21Z',event:{type:'result',subtype:'success',is_error:false,num_turns:0,local_command:'compact'}},
  {kind:'status',ts:'2026-10-14T10:01:22Z',state:'idle',local:true},
 ],{'ctx-compact':{turn_id:'ctx-compact',cost_micro_usd:29000,source:'table'}});
 await reload();
 const card = page.locator('.ctx-card');
 await card.waitFor();
 assert.equal(await card.count(), 1, 'the re-sent /context after compaction is shown again');
 assert.equal(await card.locator('.ctx-cell').count(), 200);
 assert.equal(await card.locator('.ctx-cell[data-kind="buffer"]').count(), 33);
 assert.equal(await card.locator('.ctx-cell[data-kind="used"]').count(), 94);
 assert.equal(await card.locator('.ctx-amount').innerText(), '93.3K / 200.0K tokens（47%）');
 assert.deepEqual(await card.locator('.ctx-row-label').allInnerTexts(), ['系统提示词','系统工具','MCP 工具（按需加载）','记忆文件','技能','消息','自动压缩预留','可用空间']);
 assert.equal(await card.locator('.ctx-row').first().locator('.ctx-row-value').innerText(), '4.0K · 2%');
 assert.deepEqual(await card.locator('.ctx-more summary').allInnerTexts(), ['MCP 工具（1）','记忆文件（1）','技能（2）']);
 await card.locator('.ctx-more summary').last().click();
 assert.equal(await card.locator('img').count(), 0, 'skill names are text');
 assert.match(await card.locator('.ctx-more').last().innerText(), /<img src=x onerror=alert\(1\)>/);
 assert.equal(await card.locator('.ctx-more').first().locator('.ctx-item-name').textContent(), 'read');
 const compactChip = page.locator('.chip.compact');
 assert.equal(await compactChip.innerText(), '已压缩上下文 · 95.2K → 2.8K tokens');
 assert.equal(await footers.first().locator('.answer-cost').innerText(), '未调用模型');
 assert.equal(await footers.last().locator('.answer-cost').textContent(), '$0.0290', '/compact did call the model');
 assert.equal(await value(), '1%');
 assert.match(await tip(), /上下文已压缩：95\.2K → 2\.8K tokens（1%）/);
 await card.scrollIntoViewIfNeeded();
 await page.screenshot({path:'output/playwright/chat-context-card.png',animations:'disabled'});
 await page.setViewportSize({width:390,height:844});
 await card.scrollIntoViewIfNeeded();
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
 await page.screenshot({path:'output/playwright/chat-context-card-mobile.png',animations:'disabled'});
 await page.setViewportSize({width:1280,height:900});

 // Live: the working line says compacting instead of thinking.
 send({type:'user_message',text:'/compact',turn:turn('ctx-live-compact'),ts:'2026-10-14T10:02:00Z'});
 send({type:'status',state:'running'});
 send({type:'agent_event',event:{type:'system',subtype:'status',status:'compacting'}});
 await page.locator('.turn.working .work-label').filter({hasText:'正在压缩上下文…'}).waitFor();
 send({type:'agent_event',event:{type:'system',subtype:'status',status:null,compact_result:'failed',compact_error:'Not enough messages to compact.'}});
 await page.locator('.turn.working .work-label').filter({hasText:'思考中…'}).waitFor();
 await page.locator('.chip.err').filter({hasText:'上下文压缩失败：Not enough messages to compact.'}).waitFor();
 send({type:'status',state:'idle'});

 // Codex reports no context window: no meter.
 setAgent('codex');
 await reload();
 assert.equal(await meter.isVisible(),false);
 setHistory([]);
 console.log('Chat context: meter from history and live requests, subagents ignored, warn/critical, compaction, first-turn size, local /context footer, mobile, Codex hidden passed');
}
