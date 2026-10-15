import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { browserEngine } from './playwright-launch.mjs';

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
 // Touch has no hover: a tap shows the details, a second tap closes them, and
 // the composer keeps focus (the soft keyboard stays up). Real touch via CDP.
 if (browserEngine() === 'chromium') {
  const cdp = await page.context().newCDPSession(page);
  const center = await meter.boundingBox();
  const tap = async (x = center.x + center.width / 2, y = center.y + center.height / 2) => {
   await cdp.send('Input.dispatchTouchEvent', {type:'touchStart', touchPoints:[{x, y}]});
   await cdp.send('Input.dispatchTouchEvent', {type:'touchEnd', touchPoints:[]});
  };
  const bubble = page.locator('#tip');
  await page.locator('#chat-input').focus();
  await tap();
  await page.locator('#tip.show').filter({hasText:'上下文已用 90.0K / 200.0K tokens（45%）'}).waitFor();
  assert.equal(await page.evaluate(()=>document.activeElement?.id), 'chat-input', 'tapping the meter must not take focus from the composer');
  await page.waitForTimeout(3000); // longer than the long-press linger: a tapped tip stays to be read
  assert.equal(await bubble.evaluate(el=>el.classList.contains('show')), true);
  await page.evaluate(()=>document.getElementById('chat-log')?.dispatchEvent(new Event('scroll'))); // streaming auto-scroll elsewhere
  assert.equal(await bubble.evaluate(el=>el.classList.contains('show')), true, 'scrolling the message list does not move the meter');
  await tap();
  await page.waitForFunction(()=>!document.getElementById('tip').classList.contains('show'));
  await tap();
  await page.locator('#tip.show').waitFor();
  const bar = await page.locator('.composer-bar').boundingBox();
  await tap(bar.x + 8, bar.y - 40); // elsewhere
  await page.waitForFunction(()=>!document.getElementById('tip').classList.contains('show'));
  await cdp.detach();
 }
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

 // /usage draws the plan's limit rows from usage_report as bars (server order,
 // severity and scope), resets in the system time zone, and the text-only
 // "what's contributing" part parsed into rows; unknown lines stay text.
 const usageText = ['You are currently using your subscription to power your Claude Code usage','',
  'Current session: 47% used · resets Jan 1, 12am (UTC)','Current week (all models): 71% used · resets Oct 17, 9am (UTC)','',
  "What's contributing to your limits usage?",
  'Approximate, based on local sessions on this machine — does not include other devices or claude.ai. Behaviors are independent characteristics, not a breakdown.','',
  'Last 24h · 1258 requests · 35 sessions','  87% of your usage was at >150k context','',
  'Last 7d · 3729 requests · 81 sessions','  88% of your usage was at >150k context','  39% of your usage came from sessions active for 8+ hours',
  '  Top skills: /frontend-design 3%, /pdf 1%, +2 more','  12% of your usage did something new','<img src=x onerror=alert(1)> unknown footer'].join('\n');
 const usageReport = {session:{total_cost_usd:0,total_api_duration_ms:0,total_duration_ms:3247,total_lines_added:0,total_lines_removed:0,model_usage:{}},
  rate_limits:{limits:[
   {kind:'session',group:'session',percent:47,resets_at:'2020-01-01T00:00:00+00:00',scope:null,severity:'normal',is_active:false},
   {kind:'weekly_all',group:'weekly',percent:71.4,resets_at:'2099-10-17T09:00:00+00:00',scope:null,severity:'warning',is_active:true},
   {kind:'weekly_scoped',group:'weekly',percent:92,resets_at:null,scope:{model:{display_name:'Opus <b>5</b>'}},severity:'critical',is_active:false}],
   extra_usage:{is_enabled:true,monthly_limit:5000,used_credits:1250,utilization:25,currency:'usd'}}};
 setHistory([
  {kind:'user',ts:'2026-10-14T11:00:00Z',text:'/usage',turn:turn('usage-card')},
  {kind:'event',ts:'2026-10-14T11:00:01Z',event:{type:'assistant',uuid:'usage-uuid',parent_tool_use_id:null,usage_report:usageReport,local_command_run:{command:'usage',args:''},
   message:{id:'local-usage',model:'<synthetic>',content:[{type:'text',text:usageText}],usage:{input_tokens:0,output_tokens:0}}}},
  {kind:'event',ts:'2026-10-14T11:00:01Z',event:{type:'result',subtype:'success',is_error:false,num_turns:0,local_command:'usage',result:usageText,modelUsage:{}}},
  {kind:'status',ts:'2026-10-14T11:00:02Z',state:'idle',local:true},
 ]);
 await reload();
 const usageCard = page.locator('.usage-card');
 await usageCard.waitFor();
 assert.equal(await page.locator('#chat-log').getByText('You are currently using').count(), 0, 'the card replaces the text answer');
 assert.equal(await usageCard.locator('.usage-card-sub').innerText(), '正在使用订阅套餐额度');
 assert.deepEqual(await usageCard.locator('.usage-limit-label').allInnerTexts(), ['当前会话','本周（全部模型）','本周（Opus <b>5</b>）','本月额外用量']);
 assert.deepEqual(await usageCard.locator('.usage-limit-value').allInnerTexts(), ['已用 47%','已用 71%','已用 92%','$12.50 / $50.00']);
 assert.deepEqual(await usageCard.locator('.usage-limit').evaluateAll(rows => rows.map(r => r.dataset.level)), ['normal','warning','critical','normal']);
 assert.equal(await usageCard.locator('.usage-bar').nth(1).getAttribute('aria-valuenow'), '71');
 assert.equal(await usageCard.locator('.usage-bar').nth(1).getAttribute('aria-label'), '本周（全部模型）');
 const resets = await usageCard.locator('.usage-limit-reset').allInnerTexts();
 assert.equal(resets[0], '已于 01-01 00:00 重置', 'reset time in the system time zone (UTC here)');
 assert.match(resets[1], /^10-17 09:00 重置 · \d+ 天( \d+ 小时)?后$/);
 assert.equal(resets[2], '套餐额度用完后按量计费', 'no reset time: no reset line');
 assert.deepEqual(await usageCard.locator('.usage-period-head').allInnerTexts(), ['近 24 小时 · 1258 次请求 · 35 个会话','近 7 天 · 3729 次请求 · 81 个会话']);
 assert.deepEqual(await usageCard.locator('.usage-trait-pct').allInnerTexts(), ['87%','88%','39%','','12%']);
 assert.equal(await usageCard.locator('.usage-trait').nth(2).innerText(), '39%\n用量来自持续活跃 8 小时以上的会话');
 assert.deepEqual(await usageCard.locator('.usage-tag').allInnerTexts(), ['/frontend-design 3%','/pdf 1%','另有 2 个']);
 assert.equal(await usageCard.locator('.usage-trait').last().locator('.usage-trait-text').innerText(), 'did something new');
 assert.equal(await usageCard.locator('.usage-note').innerText(), '<img src=x onerror=alert(1)> unknown footer');
 assert.equal(await usageCard.locator('img, b').count(), 0, 'CLI and server text stays text');
 assert.equal(await footers.last().locator('.answer-cost').innerText(), '未调用模型');
 await usageCard.evaluate(card => { card.scrollIntoView({block:'start'}); document.getElementById('chat-log')?.scrollBy(0, -64); });
 await page.screenshot({path:'output/playwright/chat-usage-card.png',animations:'disabled'});
 await page.setViewportSize({width:390,height:844});
 await usageCard.scrollIntoViewIfNeeded();
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
 await page.screenshot({path:'output/playwright/chat-usage-card-mobile.png',animations:'disabled'});
 await page.setViewportSize({width:1280,height:900});

 // Codex reports no context window: no meter.
 setAgent('codex');
 await reload();
 assert.equal(await meter.isVisible(),false);
 setHistory([]);
 console.log('Chat context: meter from history and live requests, subagents ignored, warn/critical, compaction, first-turn size, touch tap details, local /context footer, /usage card, mobile, Codex hidden passed');
}
