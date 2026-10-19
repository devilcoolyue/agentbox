#!/usr/bin/env node
// Capture the README screenshots and product tour from the shipped web UI with
// synthetic, local-only API data. No Docker, model calls or production access.
//
//   node scripts/capture-readme.mjs [--lang en|zh-CN|all] [--video] [--no-shots]
//
// Screenshots go to output/playwright/readme/<lang>/. --video also records the
// tour as screencast frames plus a timeline for scripts/render-readme-tour.mjs.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir, writeFile, rm } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const ROOT = resolve(fileURLToPath(new URL('../internal/web/static/', import.meta.url)));
export const OUTPUT = resolve('output/playwright/readme');
export const LANGS = ['en', 'zh-CN'];
const SKINS = ['amber', 'glass', 'cyberpunk', 'graphite', 'verdant', 'blueprint'];

/* ---------------- Synthetic data ----------------
 * Everything the page shows comes from here: one Claude workspace that runs a
 * full task, two others for the sidebar, and the records behind usage, skills,
 * MCP, accounts, the tunnel and extensions. Text is per language so each README
 * shows its own interface. */
export function fixture(lang) {
  const zh = lang === 'zh-CN';
  const pick = (en, cn) => zh ? cn : en;
  const now = Date.now();
  const iso = ms => new Date(ms).toISOString();
  const stamp = iso(now - 900000);
  const reasoning = {support:'supported',control:'effort',levels:['low','medium','high','xhigh','max']};
  const models = {
    claude:[{id:'claude-opus-5',label:'Opus 5',reasoning},{id:'claude-sonnet-5',label:'Sonnet 5',reasoning}],
    codex:[{id:'gpt-5.5',label:'GPT-5.5',reasoning:{support:'supported',control:'effort',levels:['low','medium','high','xhigh']}}],
  };
  const accounts = [
    {id:'claude-team',type:'claude',label:pick('Claude · Team Max','Claude · 团队订阅'),cred_status:'ok',auth_mode:'oauth',sessions:2,access:{mode:'all'},models:['claude-opus-5','claude-sonnet-5'],default_model:'claude-opus-5'},
    {id:'codex-team',type:'codex',label:pick('Codex · R&D','Codex · 产品研发'),cred_status:'ok',auth_mode:'oauth',sessions:1,access:{mode:'all'}},
    {id:'claude-relay',type:'claude',label:pick('Claude · API relay','Claude · 中转 API'),cred_status:'ok',auth_mode:'apikey',sessions:0,access:{mode:'users',users:['designer']}},
    {id:'codex-key',type:'codex',label:pick('Codex · API key','Codex · API Key'),cred_status:'ok',auth_mode:'apikey',sessions:0,access:{mode:'admin'}},
  ];
  const sessions = [
    {id:'demo-web',name:pick('Web app · Workspace search','产品官网 · 新版工作台'),agent:'claude',account_id:'claude-team',account_label:accounts[0].label,status:'running',default_model:'claude-opus-5',created_at:stamp},
    {id:'demo-api',name:pick('API service · Latency work','API 服务 · 性能优化'),agent:'codex',account_id:'codex-team',account_label:accounts[1].label,status:'running',default_model:'gpt-5.5',created_at:stamp},
    {id:'demo-docs',name:pick('Docs · Knowledge base','开发文档 · 知识库'),agent:'claude',account_id:'claude-team',account_label:accounts[0].label,status:'stopped',stop_reason:'idle',default_model:'claude-opus-5',created_at:stamp},
  ];
  const settings = {listen:'127.0.0.1:8180',agent_image:'agentbox-agent:latest',permission_mode:'bypassPermissions',max_upload_mb:512,idle_timeout_min:30,timezone:'Asia/Shanghai',
    container:{memory_mb:2048,cpus:2,pids_limit:512,network:'bridge'},resources:{max_running:12,max_running_per_user:3,min_free_bytes:10737418240},models,
    default_models:{claude:'claude-opus-5',codex:'gpt-5.5'},terminal_tips:{tips:[pick('Keep project files in /workspace and shared files in /shared','项目文件放在 /workspace，共享文件放在 /shared')],interval_sec:4,animation:'scroll'},
    tunnel:{enabled:true,transparent:true,proxy_bind:'172.17.0.1:1080',network_bind:'172.17.0.1:1082',network_image:'agentbox-network:latest'},proxy_bridge:{bind:'172.17.0.1:1081'},
    model_gateway:{bind:'172.17.0.1:1083',host:'172.17.0.1',dedicated_connections:false,active:true,needed:true},pricing:{}};

  // The task the tour hands to Claude, as stream-json events.
  const file = 'src/components/ProjectList.tsx';
  const task = {
    user: pick(`Add search and status filters to @${file} — keep the current design and add keyboard shortcuts.`,
      `给 @${file} 加上搜索和状态筛选，保留现有设计风格，并补上键盘操作。`),
    // Typed in three parts around the @ mention the tour picks from the menu.
    typed: pick(['Add search and status filters to ', '@Proj', '— keep the current design and add keyboard shortcuts.'],
      ['给 ', '@Proj', '加上搜索和状态筛选，保留现有设计风格，并补上键盘操作。']),
    title: pick('Workspace search and status filters', '工作台搜索与状态筛选'),
    thinking: pick('The list lives in ProjectList.tsx. I will read it, add a small ProjectFilters component for the search box and the status menu, keep the filter state in the URL so a reload keeps it, then run the type check and the tests.',
      '列表在 ProjectList.tsx 里。先读一遍，再加一个 ProjectFilters 组件放搜索框和状态菜单，筛选条件写进 URL 以便刷新后保留，最后跑类型检查和测试。'),
    todos: pick([['Read the current project list','Reading the current project list'],['Add search and status filters','Adding search and status filters'],['Run the type check and tests','Running the type check and tests']],
      [['阅读现有的项目列表','正在阅读项目列表'],['加上搜索与状态筛选','正在加上搜索与状态筛选'],['运行类型检查与测试','正在运行类型检查与测试']]),
    answer: pick(
      'Search and status filters are in, reusing the existing layout, spacing and theme colors.\n\n### What changed\n\n- **Find projects fast** — search by name and combine it with a status filter.\n- **Keyboard first** — press `/` to focus search, `Esc` to clear, arrow keys to move through results.\n- **Shareable state** — filters live in the URL, so a reload or a shared link keeps them.\n\n```ts\nconst visibleProjects = projects.filter(project =>\n  matchesQuery(project, query) && matchesStatus(project, status)\n);\n```\n\nThe type check and all 13 tests pass. Review the diff in **Changes** before you commit.',
      '搜索与状态筛选已完成，沿用了现有布局、间距和主题颜色。\n\n### 这次改了什么\n\n- **快速定位项目**：按名称搜索，可与运行状态组合筛选。\n- **键盘直接操作**：按 `/` 聚焦搜索，`Esc` 清空，方向键在结果间移动。\n- **刷新后继续工作**：筛选条件保存在 URL 中，刷新或分享链接都会保留。\n\n```ts\nconst visibleProjects = projects.filter(project =>\n  matchesQuery(project, query) && matchesStatus(project, status)\n);\n```\n\n类型检查与 13 个测试全部通过。提交前可以在「变更」页逐行检查。'),
    suggestion: pick('Commit these changes with a short message', '用一句简短的说明提交这些改动'),
  };
  const turn = {id:'demo-turn',model:'claude-opus-5',effort:'high',control:'effort'};
  const asst = (id, content, input) => ({type:'assistant',parent_tool_use_id:null,message:{id,model:'claude-opus-5',role:'assistant',content,
    usage:{input_tokens:1200,cache_creation_input_tokens:2400,cache_read_input_tokens:input,output_tokens:180}}});
  const use = (n, name, input) => ({type:'tool_use',id:'toolu_demo_'+n,name,input});
  const todos = done => task.todos.map(([content, activeForm], i) => ({content,activeForm,status:i < done ? 'completed' : i === done ? 'in_progress' : 'pending'}));
  const start = input => ({type:'stream_event',parent_tool_use_id:null,event:{type:'message_start',message:{model:'claude-opus-5',usage:{input_tokens:1200,cache_creation_input_tokens:2400,cache_read_input_tokens:input,output_tokens:1}}}});
  // Steps are [delay before it in ms, event]; stream deltas render through the page's own typewriter.
  const steps = [
    [500, start(18000)],
    [100, {type:'stream_event',parent_tool_use_id:null,event:{type:'content_block_start',index:0,content_block:{type:'thinking',thinking:''}}}],
    ...chunks(task.thinking, 6).map(text => [180, {type:'stream_event',parent_tool_use_id:null,event:{type:'content_block_delta',index:0,delta:{type:'thinking_delta',thinking:text}}}]),
    [250, {type:'stream_event',parent_tool_use_id:null,event:{type:'content_block_stop',index:0}}],
    [50, asst('msg_1', [{type:'thinking',thinking:task.thinking}], 18000)],
    [450, asst('msg_2', [use(1,'TodoWrite',{todos:todos(0)})], 21000)],
    [700, asst('msg_3', [use(2,'Read',{file_path:'/workspace/'+file})], 22500)],
    [650, asst('msg_4', [use(3,'Grep',{pattern:'useSearchParams',path:'/workspace/src'})], 27800)],
    [600, asst('msg_5', [use(4,'TodoWrite',{todos:todos(1)})], 28400)],
    [650, asst('msg_6', [use(5,'Write',{file_path:'/workspace/src/components/ProjectFilters.tsx',content:'…'})], 29100)],
    [650, asst('msg_7', [use(6,'Edit',{file_path:'/workspace/'+file,old_string:'…',new_string:'…'})], 33600)],
    [550, asst('msg_8', [use(7,'Edit',{file_path:'/workspace/src/styles/workspace.css',old_string:'…',new_string:'…'})], 36200)],
    [600, asst('msg_9', [use(8,'TodoWrite',{todos:todos(2)})], 38800)],
    [600, asst('msg_10', [use(9,'Bash',{command:'npm run check && npm test',description:pick('Type check and run tests','类型检查并运行测试')})], 39400)],
    'live',
    [1500, asst('msg_11', [use(10,'TodoWrite',{todos:todos(3)})], 42600)],
    [500, start(43800)],
    [100, {type:'stream_event',parent_tool_use_id:null,event:{type:'content_block_start',index:0,content_block:{type:'text',text:''}}}],
    ...chunks(task.answer, 14).map(text => [190, {type:'stream_event',parent_tool_use_id:null,event:{type:'content_block_delta',index:0,delta:{type:'text_delta',text}}}]),
    [300, {type:'stream_event',parent_tool_use_id:null,event:{type:'content_block_stop',index:0}}],
    [50, asst('msg_12', [{type:'text',text:task.answer}], 43800)],
    [150, {type:'result',subtype:'success',is_error:false,num_turns:12,duration_ms:46800,result:task.answer,
      modelUsage:{'claude-opus-5':{inputTokens:15600,outputTokens:3900,cacheReadInputTokens:382000,cacheCreationInputTokens:31000,contextWindow:200000,costUSD:0.61}}}],
    [100, {type:'prompt_suggestion',suggestion:task.suggestion}],
  ];
  const cost = {turn_id:turn.id,cost_micro_usd:612400,source:'table'};
  const duration = 48200;

  // /context: the CLI answers locally with its structured usage, drawn as a grid.
  const contextUsage = {model:'claude-opus-5',total_tokens:63800,raw_max_tokens:200000,percentage:32,
    categories:[{name:'System prompt',tokens:4100,kind:'used'},{name:'System tools',tokens:17600,kind:'used'},{name:'MCP tools',tokens:2300,kind:'used'},
      {name:'Memory files',tokens:2600,kind:'used'},{name:'Skills',tokens:3400,kind:'used'},{name:'Messages',tokens:33800,kind:'used'},
      {name:'Autocompact buffer',tokens:33000,kind:'buffer'},{name:'Free space',tokens:103200,kind:'free'}],
    mcp_tools:[{name:'mcp__workspace-files__read_file',server_name:'workspace-files',tokens:1400},{name:'mcp__team-docs__search',server_name:'team-docs',tokens:900}],
    memory_files:[{path:'/workspace/CLAUDE.md',type:'Project',tokens:1800},{path:'/home/agent/.claude/CLAUDE.md',type:'User',tokens:800}],
    skills:[{name:'code-review',source:'userSettings',tokens:1900},{name:'release-check',source:'projectSettings',tokens:1500}]};
  const contextSteps = [
    [400, {type:'assistant',uuid:'ctx-demo',parent_tool_use_id:null,context_usage:contextUsage,local_command_run:{command:'context',args:''},
      message:{id:'local-ctx',model:'<synthetic>',content:[{type:'text',text:'## Context Usage\n\n**Tokens:** 63.8k / 200k (32%)'}],usage:{input_tokens:0,output_tokens:0}}}],
    [80, {type:'result',subtype:'success',is_error:false,num_turns:0,local_command:'context',modelUsage:{'claude-opus-5':{contextWindow:200000}}}],
  ];
  const commands = [
    {name:'clear',description:'Start a new session',aliases:['reset','new'],builtin:true},
    {name:'compact',description:'Free up context by summarizing the conversation so far',argument_hint:'<optional custom summarization instructions>',builtin:true},
    {name:'context',description:'Show current context usage',builtin:true},
    {name:'code-review',description:pick('Review the current changes for correctness and test coverage','审查当前改动的正确性与测试覆盖')},
    {name:'cost',description:'Show the total cost and duration of the current session',builtin:true},
    {name:'release-check',description:pick('Check build output, migrations and rollback steps before a release','发布前核对构建产物、迁移与回退步骤')},
    {name:'usage',description:'Show plan usage limits',builtin:true},
  ];
  const workspaceFiles = [
    {path:'src',dir:true},{path:'src/components',dir:true},{path:'docs',dir:true},{path:'README.md'},{path:'package.json'},
    {path:'src/components/ProjectList.tsx'},{path:'src/components/ProjectCard.tsx'},{path:'src/components/ProjectGrid.tsx'},
    {path:'src/search.ts'},{path:'src/styles/workspace.css'},{path:'docs/projects.md'},
  ];

  // Usage: one row per model request, as the server records them.
  const rows = [];
  const add = (row, n) => { for (let i = 0; i < n; i++) rows.push({...row, ...row.each?.(i), each:undefined}); };
  let id = 0, t = now - 60000;
  const req = (base, n, kindExtra = {}) => {
    const turnID = 'usage-' + (++id);
    add({...base, ...kindExtra, turn_id:turnID, turn_requests:n, each:i => {
      const input = 900 + i * 140, cached = 18000 + i * 2600, out = 260 + (i * 97) % 600;
      const rate = base.agent === 'claude' ? {input:5,output:25,cache_read:0.5,cache_write:10} : {input:1.25,output:10,cache_read:0.125,cache_write:0};
      const write = base.agent === 'claude' ? 1200 - i * 40 : 0;
      t -= 9000 + (i * 3137) % 7000;
      return {id:rows.length + 1,ts:t,turn_seq:n - i,input_tokens:input,output_tokens:out,cache_read_tokens:cached,cache_write_tokens:write,total_tokens:input+out+cached+write,
        cost_micro_usd:Math.round(input*rate.input+out*rate.output+cached*rate.cache_read+write*rate.cache_write),
        ttft_ms:i === n - 1 ? 1300 + id * 70 : 0,wall_ms:i === n - 1 ? 38000 + id * 900 : 0,duration_ms:i === n - 1 ? 31000 + id * 700 : 0,
        rate:{...rate,key:base.model,basis:'table',snapshot:true}};
    }}, n);
  };
  const web = {user:'demo',session_id:'demo-web',session_name:sessions[0].name,agent:'claude',account_label:accounts[0].label,model:'claude-opus-5',kind:'chat',billing:'table',provider:'firstParty',effort:'high',effort_control:'effort'};
  const api = {user:'demo',session_id:'demo-api',session_name:sessions[1].name,agent:'codex',account_label:accounts[1].label,model:'gpt-5.5',kind:'chat',billing:'table',effort:'medium',effort_control:'effort'};
  req(web, 4);
  req({...web,kind:'suggestion',effort:'',effort_control:''}, 1);
  req(api, 3);
  req({...api,kind:'terminal',user:'designer'}, 2);
  req({...web,kind:'title',model:'claude-haiku-4-5',effort:'',effort_control:''}, 1);
  req(web, 2);
  const total = {rows:rows.length,turns:new Set(rows.map(r => r.turn_id)).size};
  for (const key of ['input_tokens','output_tokens','cache_read_tokens','cache_write_tokens','cost_micro_usd']) total[key] = rows.reduce((sum, row) => sum + row[key], 0);

  const skills = [
    {name:'code-review',description:pick('Review changes for correctness, edge cases and test coverage.','审查代码变更，重点检查正确性、边界条件与测试覆盖。'),source:'template'},
    {name:'api-design',description:pick('Keep API shapes, error responses and docs consistent.','统一 API 结构、错误响应和接口文档。'),source:'global'},
    {name:'release-check',description:pick('Check build output, migrations and rollback steps before a release.','发布前核对构建产物、迁移与回退步骤。'),source:'session'},
  ].map(s => ({...s,files:3,bytes:4096,updated_at:stamp}));
  const skillContent = pick(
    '# Code review\n\nRun a focused review before committing and return actionable findings.\n\n## Workflow\n\n1. Read the project conventions and confirm the goal and scope of the change.\n2. Walk the Git diff; check input validation, permissions and error paths.\n3. Run the relevant tests and record what passed and what is not covered.\n\n## Focus\n\n- **Correctness** — does the change do what was asked without breaking existing behavior?\n- **Maintainability** — are names, interfaces and error handling clear?\n- **Delivery** — do docs and tests follow the change?\n\n## Output\n\nList findings by impact with file locations, the reason and a suggestion. When there are none, state what was verified.',
    '# Code review\n\n在提交前做一次聚焦的代码审查，输出可操作的修改建议。\n\n## 工作流程\n\n1. 阅读项目约定，确认本次改动的目标与范围。\n2. 查看 Git diff，核对输入校验、权限与异常路径。\n3. 运行相关测试，记录验证结果与尚未覆盖的边界。\n\n## 审查重点\n\n- **正确性**：实现是否满足需求，是否保留原有行为。\n- **可维护性**：命名、接口和错误处理是否清晰。\n- **交付质量**：文档与测试是否跟随变更。\n\n## 输出\n\n按影响程度列出发现，附文件位置、原因与建议。没有发现时说明验证范围。');
  const readmeContent = pick(
    '# Workspace search\n\nDemo project for workspace search and status filters.\n\n## Quick start\n\n```sh\nnpm install\nnpm run dev\n```\n\n## Done\n\n- Search projects by name and combine it with a status filter.\n- Keyboard navigation and a shortcut to focus search.\n- Filters are kept in the URL and survive a reload.\n\n## Verify\n\n```sh\nnpm run check\nnpm test\n```\n\nOpen **Changes** to review the work, then commit it to the local repository.',
    '# Workspace search\n\n搜索与状态筛选的演示项目。\n\n## 快速开始\n\n```sh\nnpm install\nnpm run dev\n```\n\n## 已完成\n\n- 按项目名称搜索，组合运行状态筛选。\n- 键盘导航与快捷聚焦。\n- 筛选条件保存在 URL，刷新后保留。\n\n## 验证\n\n```sh\nnpm run check\nnpm test\n```\n\n打开「变更」页检查实现，再提交到本地仓库。');
  const diff = 'diff --git a/src/components/ProjectList.tsx b/src/components/ProjectList.tsx\nindex 1234567..abcdef0 100644\n--- a/src/components/ProjectList.tsx\n+++ b/src/components/ProjectList.tsx\n@@ -1,6 +1,8 @@\n import { useState } from \'react\';\n+import { useSearchParams } from \'react-router-dom\';\n import { ProjectGrid } from \'./ProjectGrid\';\n+import { ProjectFilters, matchesQuery, matchesStatus } from \'./ProjectFilters\';\n \n export function ProjectList({ projects }) {\n-  const [query, setQuery] = useState(\'\');\n+  const [params, setParams] = useSearchParams();\n+  const query = params.get(\'q\') ?? \'\';\n+  const status = params.get(\'status\') ?? \'all\';\n \n-  const visibleProjects = projects;\n+  const visibleProjects = projects.filter(project =>\n+    matchesQuery(project, query) &&\n+    matchesStatus(project, status)\n+  );\n \n   return (\n     <section className="project-list">\n+      <ProjectFilters\n+        query={query}\n+        status={status}\n+        onChange={next => setParams(next, { replace: true })}\n+      />\n       <ProjectGrid projects={visibleProjects} />\n     </section>\n   );\n';
  const extensionsList = [
    {id:'agentbox.loop-guard',version:'1.0.0',publisher:'agentbox',builtin:true,runtime:'builtin',
      name:{'zh-CN':'循环保护','zh-TW':'循環保護',en:'Loop guard'},
      description:{'zh-CN':'检测模型反复输出短句，中断这次回复并冷却相同请求。','zh-TW':'偵測模型反覆輸出短句。',en:'Interrupts a reply that keeps repeating short sentences and cools down identical retries.'},
      hooks:['model.request','model.stream'],permissions:['model.request.fingerprint','model.stream.text'],bindings:['account'],failure_policy:'fail_open',config_schema:{type:'object',properties:{}},
      errors:[],metrics:[{name:'trips',title:{'zh-CN':'中断次数',en:'Interruptions','zh-TW':'中斷次數'}}],
      setting:{enabled:true,order:0,config:{},permissions:['model.request.fingerprint','model.stream.text'],bindings:{mode:'selected',accounts:['claude-relay','codex-key']},failure_policy:'',revision:3},
      status:{state:'running',restarts:0,active_streams:1,started_at:iso(now-86400000),metrics:{trips:4}},requires:[{id:'model_gateway',bind:'172.17.0.1:1083',active:true}],
      accounts:accounts.map(a => ({id:a.id,label:a.label,type:a.type,eligible:a.auth_mode === 'apikey'}))},
    {id:'agentbox.webhook',version:'1.0.0',publisher:'agentbox',builtin:true,runtime:'builtin',
      name:{'zh-CN':'Webhook 通知','zh-TW':'Webhook 通知',en:'Webhook notifications'},
      description:{'zh-CN':'把空间生命周期与对话回合结果推送到飞书、钉钉、企业微信或通用 JSON 地址。','zh-TW':'推送空間與對話事件。',en:'Posts workspace lifecycle and chat turn results to Feishu, DingTalk, WeCom or a JSON endpoint.'},
      hooks:['workspace.lifecycle','chat.turn.ended'],permissions:['workspace.events','chat.turns'],bindings:['account','user'],failure_policy:'fail_open',config_schema:{type:'object',properties:{}},
      errors:[],metrics:[{name:'sent',title:{'zh-CN':'已送达',en:'Delivered','zh-TW':'已送達'}}],
      setting:{enabled:true,order:1,config:{},permissions:['workspace.events','chat.turns'],bindings:{mode:'all'},failure_policy:'',revision:2},
      status:{state:'running',restarts:0,active_streams:0,started_at:iso(now-86400000),metrics:{sent:128}},requires:[],
      accounts:accounts.map(a => ({id:a.id,label:a.label,type:a.type,eligible:true}))},
  ];
  return {lang, zh, pick, now, stamp, models, accounts, sessions, settings, task, turn, steps, cost, duration, contextSteps, commands, workspaceFiles,
    rows, total, skills, skillContent, readmeContent, diff, extensionsList};
}

/* Split text into n roughly equal chunks on character boundaries. */
function chunks(text, n) {
  const chars = [...text], size = Math.ceil(chars.length / n), out = [];
  for (let i = 0; i < chars.length; i += size) out.push(chars.slice(i, i + size).join(''));
  return out;
}

/* ---------------- Local server and mocked API ---------------- */
export async function staticServer() {
  const server = createServer(async (req, res) => {
    try {
      const rel = decodeURIComponent(req.url.split('?')[0]).replace(/^\/_v\/[^/]+/, '');
      const path = resolve(ROOT, '.' + (rel === '/' ? '/index.html' : rel));
      if (!path.startsWith(ROOT + '/')) { res.writeHead(403).end(); return; }
      res.setHeader('Content-Type', ({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.woff2':'font/woff2','.png':'image/png'})[extname(path)] || 'application/octet-stream');
      res.end(await readFile(path));
    } catch { res.writeHead(404).end(); }
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  return {server, base:`http://127.0.0.1:${server.address().port}`,
    close: async () => { server.closeAllConnections(); await new Promise(r => server.close(r)); }};
}

export async function mockApi(context, fx, base, {animateTerminal}) {
  // The tour starts in an empty conversation of the Claude workspace.
  const empty = {entries:[], thread:{id:'tour-thread', title:'', turns:0}, costs:{}};
  const state = {missing:[], sockets:new Set(), receipts:new Map(), history:{'demo-web':empty}, sends:[], seq:0, onSend:null};
  const scope = 'a'.repeat(64);
  // Keep documentation captures entirely local, including unexpected asset requests.
  await context.route('**/*', route => new URL(route.request().url()).origin === base ? route.fallback() : route.abort('blockedbyclient'));
  const emit = (session, message) => { for (const s of state.sockets) if (s.session === session) s.ws.send(JSON.stringify(message)); };
  await context.routeWebSocket('**/api/sessions/*/chat?*', ws => {
    const socket = {ws, session:new URL(ws.url()).pathname.split('/')[3]};
    state.sockets.add(socket); ws.onClose(() => state.sockets.delete(socket));
    ws.send(JSON.stringify({type:'status',state:'idle'}));
  });
  await context.routeWebSocket('**/api/sessions/*/term?*', ws => {
    let started = false;
    ws.onMessage(() => {
      if (started) return; started = true;
      const prompt = '\x1b[32magent@agentbox\x1b[0m:\x1b[34m/workspace\x1b[0m$ ';
      const script = [
        ['out', '\x1b[2J\x1b[H\x1b[38;5;214m  /workspace  ·  ' + fx.pick('web-app', '产品官网') + '\x1b[0m\r\n\r\n' + prompt],
        ['type', 'npm run check'], ['out', '\r\n\r\n> workspace-web@1.4.0 check\r\n> tsc --noEmit\r\n\r\n'],
        ['wait', 500], ['out', '\x1b[32m✓\x1b[0m ' + fx.pick('Type check passed', 'TypeScript 类型检查通过') + '\r\n\r\n' + prompt],
        ['type', 'npm test'], ['out', '\r\n\r\n \x1b[46;30m RUN \x1b[0m  v3.2.0 /workspace\r\n\r\n'],
        ['line', ' \x1b[32m✓\x1b[0m src/search.test.ts \x1b[2m(6 tests)\x1b[0m 12ms\r\n'],
        ['line', ' \x1b[32m✓\x1b[0m src/filters.test.ts \x1b[2m(4 tests)\x1b[0m 8ms\r\n'],
        ['line', ' \x1b[32m✓\x1b[0m src/keyboard.test.ts \x1b[2m(3 tests)\x1b[0m 16ms\r\n\r\n'],
        ['line', ' Test Files  \x1b[32m3 passed\x1b[0m (3)\r\n      Tests  \x1b[32m13 passed\x1b[0m (13)\r\n   Duration  482ms\r\n\r\n' + prompt],
        ['type', 'git status --short'], ['out', '\r\n M src/components/ProjectList.tsx\r\n M src/styles/workspace.css\r\n?? src/components/ProjectFilters.tsx\r\n?? src/filters.test.ts\r\n\r\n' + prompt],
      ];
      if (!animateTerminal) { ws.send(Buffer.from(script.map(([kind, text]) => kind === 'wait' ? '' : text).join(''))); return; }
      (async () => {
        const pause = ms => new Promise(r => setTimeout(r, ms));
        for (const [kind, text] of script) {
          if (kind === 'wait') { await pause(text); continue; }
          if (kind === 'type') { await pause(350); for (const ch of text) { ws.send(Buffer.from(ch)); await pause(55); } await pause(250); continue; }
          if (kind === 'line') await pause(260);
          ws.send(Buffer.from(text));
        }
      })().catch(e => console.error("terminal mock:", e));
    });
  });
  const receipt = (id, patch) => {
    const r = {...state.receipts.get(id), ...patch}; r.revision = (r.revision || 0) + 1; r.updated_at = new Date().toISOString();
    state.receipts.set(id, r); emit(r.session_id, {type:'chat_request',version:1,receipt:r}); return r;
  };
  state.finish = id => receipt(id, {state:'completed'});
  await context.route('**/api/**', async route => {
    const req = route.request(), url = new URL(req.url()), path = url.pathname, method = req.method();
    const session = path.startsWith('/api/sessions/') ? path.split('/')[3] : '';
    let body, status = 200;
    if (path === '/api/ping') { await route.fulfill({status:204}); return; }
    else if (path === '/api/login') body = {token:'synthetic-docs-token',user:'demo',role:'admin'};
    else if (path === '/api/me') body = {user:'demo',role:'admin',timezone:'Asia/Shanghai',models:fx.models,quota:{metered:true,enforced:false,balance_micro_usd:42760000},terminal_tips:fx.settings.terminal_tips,
      draft_protocol:1,draft_scope:'c'.repeat(64),chat_protocol:1,chat_scope:scope,chat_questions:1,chat_followups:1,prompt_suggestions:1,chat_tasks:1};
    else if (path === '/api/onboarding') body = {version:1,can_configure:true,can_create:true,has_workspaces:true,accounts:fx.accounts.map(a => ({id:a.id,type:a.type,label:a.label,cred_status:a.cred_status})),default_models:fx.settings.default_models,container_resources:{cpus:2,memory_mb:2048,pids_limit:512}};
    else if (path === '/api/themes') body = {format:1,bases:SKINS,tokens:[],site:[],user:[],can_manage_site:true};
    else if (path === '/api/sessions') body = fx.sessions;
    else if (path === '/api/accounts') body = fx.accounts;
    else if (path === '/api/settings') body = fx.settings;
    else if (path === '/api/proxies') body = {proxies:[],bridge_up:true,bridge_host:'172.17.0.1'};
    else if (path === '/api/users') body = [{name:'demo',role:'admin'},{name:'designer',role:'user'}];
    else if (path === '/api/git/connections') body = [];
    else if (path === '/api/me/git') body = {user:'demo',name:'Demo',email:'demo@example.com'};
    else if (path === '/api/tunnel/clients') body = ['darwin-arm64','darwin-amd64','windows-amd64.exe','linux-amd64','linux-arm64'].map(platform => ({name:'abox-link-' + platform,size:12582912}));
    else if (path.startsWith('/api/updates')) body = {current_version:'v0.1.16',revision:'',built_at:'',latest_version:'v0.1.16',available:false,comparable:true};
    else if (path === '/api/usage') body = {total:fx.total,...fx.total,by_model:[],by_user:[]};
    else if (path === '/api/usage/events') body = {rows:fx.rows,total:fx.total,facets:{users:['demo','designer'],agents:['claude','codex'],models:['claude-haiku-4-5','claude-opus-5','gpt-5.5']},scope:'all',timezone:'Asia/Shanghai',order:'desc',limit:20,offset:0,sync:{last_scan_at:fx.now,last_success_at:fx.now,scanning:false,errors:0}};
    else if (path === '/api/tunnel/status') body = {enabled:true,transparent:true,client_transparent:true,connected:true,proxy_up:true,since:fx.now-3600000,remote:'192.0.2.10:53000',maps:[],rules:['10.20.0.0/16','gitlab.corp.example:443','db.corp.example:5432'],workspaces:fx.sessions.filter(s => s.status === 'running').map(s => ({session:s.id,name:s.name,ready:true}))};
    else if (path === '/api/extensions') body = {extensions:fx.extensionsList};
    else if (path.startsWith('/api/extensions/')) body = path.endsWith('/events') ? {events:[]} : path.endsWith('/logs') ? {logs:[]} : fx.extensionsList.find(e => path.includes(e.id));
    else if (path.endsWith('/models')) body = {models:fx.sessions.find(s => s.id === session)?.agent === 'codex' ? fx.models.codex : fx.models.claude,default_reasoning:{support:'supported',control:'effort',levels:['low','medium','high','xhigh']},client_effort:'high',discovery:'available'};
    else if (path.endsWith('/skills')) body = {skills:fx.skills};
    else if (path.includes('/skills/')) body = {...fx.skills[0],content:fx.skillContent,truncated:false,more:false,entries:[{path:'SKILL.md',size:1800,mtime:fx.stamp},{path:'references',dir:true,size:0,mtime:fx.stamp},{path:'references/checklist.md',size:1500,mtime:fx.stamp}]};
    else if (path.endsWith('/mcp')) body = {revision:1,user_revision:1,project_names:[],items:[
      {name:'workspace-files',source:'user',status:'applied',config:{type:'stdio',command:'npx',args:['-y','@modelcontextprotocol/server-filesystem','/workspace']}},
      {name:'team-docs',source:'session',status:'applied',config:{type:'http',url:'https://docs.example.com/mcp'}},
      {name:'design-assets',source:'user',status:'applied',disabled:true,config:{type:'http',url:'https://design.example.com/mcp'}},
    ]};
    else if (path.endsWith('/commands')) body = {state:'available',commands:fx.commands};
    else if (path.endsWith('/files/search')) {
      const q = (url.searchParams.get('q') || '').toLowerCase();
      body = {files:fx.workspaceFiles.filter(f => q ? f.path.toLowerCase().includes(q) : !f.path.includes('/')),truncated:false};
    }
    else if (path.endsWith('/chat/tasks')) body = {tasks:[]};
    else if (path.endsWith('/history')) {
      const h = state.history[session] || {entries:[],thread:{id:'empty-' + session,title:'',turns:0},costs:{}};
      body = {...h,active_thread:h.thread.id};
    }
    else if (path.endsWith('/chat/threads')) {
      const h = state.history[session];
      body = h ? {threads:[h.thread],active:h.thread.id} : {threads:[],active:''};
    }
    else if (path.includes('/chat/requests')) {
      const id = path.split('/')[6];
      if (!id) body = {version:1,scope,requests:[],pending:null};
      else if (method === 'PUT') {
        const input = req.postDataJSON();
        const h = state.history[session];
        const r = {request_id:id,session_id:session,thread_id:input.thread_id || h?.thread.id || '',turn_id:'tour-turn-' + (++state.seq),request:input,state:'running',revision:3,created_at:new Date().toISOString(),updated_at:new Date().toISOString()};
        state.receipts.set(id, r); state.sends.push({session,input,id});
        body = {version:1,receipt:r,replayed:false}; status = 202;
        setTimeout(() => state.onSend?.({session,input,id,turn:r.turn_id}), 30);
      } else body = {version:1,receipt:state.receipts.get(id)};
    }
    else if (path.endsWith('/git/status')) body = {is_repo:true,repo:'',repos:[''],branch:'feat/workspace-search',files:[{path:'src/components/ProjectList.tsx',status:' M'},{path:'src/styles/workspace.css',status:' M'},{path:'src/components/ProjectFilters.tsx',status:'??',untracked:true},{path:'src/filters.test.ts',status:'??',untracked:true}]};
    else if (path.endsWith('/git/diff')) { await route.fulfill({contentType:'text/plain',body:fx.diff}); return; }
    else if (path.endsWith('/file')) { await route.fulfill({contentType:'text/plain; charset=utf-8',body:fx.readmeContent}); return; }
    else if (path.endsWith('/files')) body = (url.searchParams.get('path') === 'src' ? ['components/','styles/','filters.test.ts','search.ts'] : ['src/','public/','docs/','package.json','README.md','tsconfig.json'])
      .map((name, i) => ({name:name.replace(/\/$/, ''),is_dir:name.endsWith('/'),size:name.endsWith('/') ? 4096 : 820 + i * 420,mode:name.endsWith('/') ? 'drwxr-xr-x' : '-rw-r--r--',mtime:fx.stamp}));
    else if (/^\/api\/sessions\/[^/]+$/.test(path)) body = fx.sessions.find(s => s.id === session);
    else { state.missing.push(path); await route.fulfill({status:404,json:{error:'No screenshot fixture: ' + path}}); return; }
    await route.fulfill({status,json:body}).catch(() => {});
  });
  return {state, emit};
}

/* ---------------- Demo cursor ----------------
 * Headless screencasts have no pointer. A manual popover follows real mouse
 * events (so it sits above dialogs too) and shows a ripple on press. */
export function cursorScript() {
  if (window !== window.top) return;
  const boot = () => {
    const c = document.createElement('div');
    c.setAttribute('popover', 'manual');
    c.style.cssText = 'position:fixed;inset:auto;left:0;top:0;margin:0;padding:0;border:0;background:none;overflow:visible;width:22px;height:22px;pointer-events:none;transform:translate(-80px,-80px);';
    c.innerHTML = '<svg width="22" height="22" viewBox="0 0 22 22" style="filter:drop-shadow(0 1px 2px rgba(0,0,0,.45))"><path d="M3 2l15 9.2-6.6 1.5 3.9 6.7-2.6 1.5-3.9-6.8L3 18.6z" fill="#fff" stroke="#111" stroke-width="1.3" stroke-linejoin="round"/></svg>';
    document.body.appendChild(c);
    const raise = () => { try { c.hidePopover(); c.showPopover(); } catch {} };
    raise();
    let x = -80, y = -80;
    addEventListener('mousemove', e => { x = e.clientX; y = e.clientY; c.style.transform = `translate(${x - 3}px,${y - 2}px)`; }, true);
    addEventListener('mousedown', () => {
      const r = document.createElement('div');
      r.style.cssText = `position:absolute;left:3px;top:2px;width:34px;height:34px;margin:-17px 0 0 -17px;border-radius:50%;background:rgba(238,179,76,.45);transform:scale(.3);opacity:1;transition:transform .45s ease-out,opacity .45s ease-out;`;
      c.prepend(r);
      requestAnimationFrame(() => requestAnimationFrame(() => { r.style.transform = 'scale(1.2)'; r.style.opacity = '0'; }));
      setTimeout(() => r.remove(), 600);
    }, true);
    // Dialogs and other popovers enter the top layer above the cursor; bring it back on top.
    new MutationObserver(raise).observe(document.documentElement, {subtree:true, attributes:true, attributeFilter:['open']});
    document.addEventListener('toggle', e => { if (e.target !== c) setTimeout(raise, 0); }, true);
  };
  if (document.body) boot(); else addEventListener('DOMContentLoaded', boot);
}

/* ---------------- Screencast recorder ---------------- */
export async function recorder(page, dir) {
  await rm(dir, {recursive:true, force:true});
  await mkdir(dir, {recursive:true});
  const cdp = await page.context().newCDPSession(page);
  const frames = [], writes = [];
  cdp.on('Page.screencastFrame', ({data, metadata, sessionId}) => {
    const file = resolve(dir, `f${String(frames.length).padStart(6, '0')}.jpg`);
    frames.push({file, t:metadata.timestamp});
    writes.push(writeFile(file, Buffer.from(data, 'base64')));
    cdp.send('Page.screencastFrameAck', {sessionId}).catch(() => {});
  });
  // Rendered at 2× pixel density and scaled down: sharper text than a 1× capture.
  await cdp.send('Page.startScreencast', {format:'jpeg', quality:93, maxWidth:1920, maxHeight:1280, everyNthFrame:1});
  return {
    stop: async () => { await cdp.send('Page.stopScreencast'); await Promise.all(writes); await cdp.detach(); return frames; },
  };
}

/* ---------------- The tour ----------------
 * One script drives both outputs: with `video` it runs in real time while the
 * recorder captures frames and chapters are marked; without it, waits shrink
 * to what rendering needs and screenshots are taken at the checkpoints. */
async function tour(page, fx, base, api, {video, shot, chapter}) {
  const wait = ms => page.waitForTimeout(video ? ms : Math.min(ms, 60));
  const hold = ms => video ? page.waitForTimeout(ms) : Promise.resolve();
  const point = async (locator, {dx = 0.5, dy = 0.5} = {}) => {
    const box = await locator.boundingBox();
    assert.ok(box, 'element has no box');
    return {x:box.x + box.width * dx, y:box.y + box.height * dy};
  };
  let mouse = {x:1300, y:900};
  const moveTo = async (locator, opts) => {
    const p = await point(locator, opts);
    if (video) {
      // Each step waits for a rendered frame while the screencast runs; keep moves short.
      const steps = Math.min(12, Math.max(5, Math.round(Math.hypot(p.x - mouse.x, p.y - mouse.y) / 60)));
      await page.mouse.move(p.x, p.y, {steps});
    } else await page.mouse.move(p.x, p.y);
    mouse = p;
  };
  const click = async (locator, opts) => { await moveTo(locator, opts); await wait(120); await page.mouse.down(); await page.mouse.up(); await wait(150); };
  const type = (text, delay = 30) => video ? page.keyboard.type(text, {delay}) : page.keyboard.insertText(text);
  const play = async (session, steps, onLive) => {
    for (const step of steps) {
      if (step === 'live') { await onLive?.(); continue; }
      const [delay, event] = step;
      await wait(delay * 0.8);
      api.emit(session, {type:'agent_event',event,ts:new Date().toISOString()});
    }
  };
  const sent = () => new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('the page did not send the message')), 15000);
    api.state.onSend = s => { api.state.onSend = null; clearTimeout(timer); resolve(s); };
  });
  const input = page.locator('#chat-input');

  // 01 · Hand off a task, with an @ file mention, and watch it stream.
  await chapter(fx.pick('Hand off a task', '下达任务'), fx.pick('Chat with Claude Code or Codex · @ files · live tool calls and tasks', '与 Claude Code / Codex 对话 · @ 引用文件 · 工具调用与任务实时可见'));
  await hold(900);
  await click(input);
  await type(fx.task.typed[0]);
  await type(fx.task.typed[1], 110);
  await page.locator('#chat-mentions .mention-row').first().waitFor();
  await page.waitForFunction(() => /ProjectList/.test(document.querySelector('#chat-mentions .mention-row[aria-selected="true"]')?.textContent || ''));
  await hold(900);
  await page.keyboard.press('Tab');
  await page.locator('#chat-mentions').waitFor({state:'hidden'});
  await type(fx.task.typed[2]);
  await hold(500);
  const firstSend = sent();
  await click(page.locator('#chat-send'));
  const s1 = await firstSend;
  // Out of the way of the stop button that replaces send.
  if (video) { await page.mouse.move(1180, 560, {steps:8}); mouse = {x:1180, y:560}; }
  assert.equal(s1.input.text, fx.task.user, 'the composed task');
  const turnStart = Date.now();
  api.emit('demo-web', {type:'status',state:'running',ts:new Date().toISOString()});
  api.emit('demo-web', {type:'user_message',text:s1.input.text,turn:{...fx.turn,id:s1.turn},ts:new Date().toISOString()});
  await page.locator('#chat-log .msg.user').first().waitFor();
  await play('demo-web', fx.steps, async () => {
    await page.locator('#chat-tasks:not(.hidden)').waitFor();
    await page.waitForFunction(() => document.querySelectorAll('#chat-log .chip.tool').length >= 9);
    if (shot) { await page.waitForTimeout(400); await shot('chat-live'); }
    api.emit('demo-web', {type:'thread_title',id:'tour-thread',title:fx.task.title});
  });
  api.emit('demo-web', {type:'turn_cost',cost:{...fx.cost,turn_id:s1.turn}});
  // The recording shows the timer counting, so the total must match it.
  const took = video ? Date.now() - turnStart : fx.duration;
  api.emit('demo-web', {type:'status',state:'idle',duration_ms:took,ts:new Date().toISOString()});
  api.state.finish(s1.id);
  await page.locator('#chat-log .answer-duration').first().waitFor();
  await page.waitForFunction(() => document.querySelector('#chat-send')?.classList.contains('stop') === false);
  await hold(2200);
  if (shot) {
    await page.mouse.move(1430, 950);
    await shot('chat-dark');
    await page.emulateMedia({colorScheme:'light'});
    await shot('chat-light');
    await page.emulateMedia({colorScheme:'dark'});
  }

  // 02 · Slash commands from the CLI, answered locally.
  await chapter(fx.pick('Run CLI commands', '使用 CLI 命令'), fx.pick('Type / for the workspace’s real Claude Code commands · /context needs no model call', '输入 / 列出空间里 Claude Code 的真实命令 · /context 不调用模型'));
  await click(input);
  await type('/con', 160);
  await page.waitForFunction(() => /context/.test(document.querySelector('#chat-commands .cmd-row[aria-selected="true"]')?.textContent || ''));
  await hold(1100);
  const secondSend = sent();
  await page.keyboard.press('Enter');
  const s2 = await secondSend;
  assert.equal(s2.input.text, '/context');
  api.emit('demo-web', {type:'status',state:'running',ts:new Date().toISOString()});
  api.emit('demo-web', {type:'user_message',text:'/context',turn:{...fx.turn,id:s2.turn},ts:new Date().toISOString()});
  await play('demo-web', fx.contextSteps);
  api.emit('demo-web', {type:'status',state:'idle',local:true,ts:new Date().toISOString()});
  api.state.finish(s2.id);
  const card = page.locator('#chat-log .ctx-card').last();
  await card.waitFor();
  await page.waitForTimeout(250);
  await card.evaluate(el => el.scrollIntoView({block:'end'}));
  await hold(2900);
  if (shot) { await page.mouse.move(1430, 950); await shot('chat-context'); }
  // Later visits load the conversation from history.
  api.state.history['demo-web'] = completedHistory(fx, true, took);

  // 03 · The original CLI in a terminal.
  await chapter(fx.pick('Use the original terminal', '使用原生终端'), fx.pick('Shell, Claude Code or Codex in a tmux session that survives reconnects', 'Shell、Claude Code、Codex 跑在持久 tmux 里，断线重连不丢'));
  await click(page.locator('.tab[data-tab="term"]'));
  await page.locator('#term-state[data-state="connected"]').waitFor();
  await page.locator('.xterm-screen').first().waitFor();
  await page.mouse.move(1430, 950); mouse = {x:1430, y:950};
  if (video) await page.waitForTimeout(6800);
  else await page.waitForTimeout(400);
  if (shot) await shot('terminal');

  // 04 · Review the diff.
  await chapter(fx.pick('Review every change', '逐行审查改动'), fx.pick('Git diff per file · commit, push and open a PR / MR from the browser', '按文件查看 Git diff · 在网页里提交、推送、发起 PR / MR'));
  await click(page.locator('.tab[data-tab="changes"]'));
  await click(page.locator('.change-row').first());
  await page.locator('#changes-diff').getByText('matchesQuery(project, query) &&', {exact:false}).waitFor();
  await hold(3200);
  if (shot) { await page.mouse.move(1430, 950); await shot('changes'); }

  // 05 · Files and rendered previews.
  await chapter(fx.pick('Browse and preview files', '浏览与预览文件'), fx.pick('Edit, upload and download · rendered Markdown and HTML', '编辑、上传、下载 · Markdown 与 HTML 渲染预览'));
  await click(page.locator('.tab[data-tab="files"]'));
  await page.locator('#files-list .file-row').first().waitFor();
  if (shot) {
    await page.getByRole('button', {name:fx.pick('Expand folder', '展开目录'), exact:true}).first().click();
    await page.getByText('search.ts', {exact:true}).waitFor();
    await page.mouse.move(1430, 950);
    await shot('files');
  }
  const readme = page.locator('#files-list').getByText('README.md', {exact:true});
  await moveTo(readme); await wait(200);
  await readme.dblclick();
  await page.locator('#fv-md').getByText('Workspace search', {exact:true}).waitFor();
  await hold(2600);
  if (shot) { await page.mouse.move(1430, 950); await shot('preview'); }
  await click(page.locator('#fv-close'));

  // 06 · Usage and costs.
  await chapter(fx.pick('Know what every request costs', '看清每次请求的花费'), fx.pick('One row per model request · tokens, cache, latency, effort and price snapshot', '每次模型请求一行 · token、缓存、延迟、推理强度与价格快照'));
  await click(page.locator('#btn-usagelog'));
  await page.locator('#usage-rows tr').first().waitFor();
  await page.mouse.move(1430, 950); mouse = {x:1430, y:950};
  await hold(3400);
  if (shot) await shot('usage');

  // 07 · Styles and themes.
  await chapter(fx.pick('Make it yours', '换上你喜欢的风格'), fx.pick('Six built-in styles × light / dark · custom themes · 简体 · 繁體 · English', '六种内置风格 × 浅色 / 深色 · 自定义主题 · 简体 · 繁體 · English'));
  await click(page.locator('#session-list .session-card[data-session-id="demo-web"]'));
  await click(page.locator('.tab[data-tab="chat"]'));
  await page.locator('#chat-log .ctx-card').waitFor();
  await click(page.locator('#btn-user-menu'));
  const skinRow = page.locator('#pref-skin .pref-trigger');
  await skinRow.waitFor();
  const choose = async skin => {
    if (await page.locator('#pref-skin .pref-menu-item').first().isHidden()) await click(skinRow);
    const item = page.locator(`#pref-skin .pref-menu-item[data-value="${skin}"]`);
    await click(item);
    await page.waitForFunction(skin => document.documentElement.dataset.skin === skin, skin);
  };
  for (const [skin, theme] of [['glass', 'light'], ['cyberpunk', 'dark'], ['graphite', 'light'], ['verdant', 'dark'], ['blueprint', 'light'], ['amber', 'dark']]) {
    if (video) await page.emulateMedia({colorScheme:theme});
    await choose(skin);
    await hold(skin === 'amber' ? 1400 : 700);
  }
  await page.keyboard.press('Escape');
  await page.emulateMedia({colorScheme:'dark'});
  await hold(400);
}

/* ---------------- Static screens ---------------- */
async function screens(page, fx, base, api, shot) {
  await page.evaluate(() => { location.hash = '#/sessions/demo-web/skills'; });
  await page.locator('.skill-row').filter({hasText:'code-review'}).click();
  await page.locator('#skills-detail').getByText('Code review', {exact:true}).waitFor();
  await shot('skills');
  await page.locator('.tab[data-tab="mcp"]').click();
  await page.locator('.mcp-row').first().waitFor();
  await shot('mcp');
  await page.evaluate(() => { location.hash = '#/settings/accounts'; });
  await page.locator('#sec-accounts').getByText(fx.accounts[0].label, {exact:true}).waitFor();
  await shot('accounts');
  await page.evaluate(() => { location.hash = '#/tunnel'; });
  await page.getByText('gitlab.corp.example:443', {exact:false}).first().waitFor();
  await page.locator('#tun-dl a').first().waitFor();
  await shot('tunnel');
  await page.evaluate(() => { location.hash = '#/extensions'; });
  await page.locator('#ext-list .ext-card').nth(1).waitFor();
  await shot('extensions');
}

/* Six styles side by side, from the same chat view. */
async function styles(page, fx, shot, out) {
  await page.evaluate(() => { location.hash = '#/sessions/demo-web/chat'; });
  await page.locator('#chat-log .answer-duration').first().waitFor();
  const tiles = [];
  for (const [skin, theme] of [['amber', 'dark'], ['glass', 'light'], ['cyberpunk', 'dark'], ['graphite', 'light'], ['verdant', 'dark'], ['blueprint', 'light']]) {
    await page.evaluate(skin => localStorage.setItem('agentbox_skin', skin), skin);
    await page.emulateMedia({colorScheme:theme});
    await page.reload();
    await page.locator('#chat-log .answer-duration').first().waitFor();
    await page.evaluate(() => document.fonts.ready);
    await page.mouse.move(1430, 950);
    const file = resolve(out, `style-${skin}.png`);
    await page.screenshot({path:file, animations:'disabled'});
    tiles.push({skin, theme, file, label:await page.evaluate(skin => document.querySelector(`#pref-skin .pref-menu-item[data-value="${skin}"] .pref-menu-label`)?.textContent || skin, skin)});
  }
  await page.evaluate(() => localStorage.setItem('agentbox_skin', 'amber'));
  await page.emulateMedia({colorScheme:'dark'});
  const collage = await page.context().newPage();
  await collage.setViewportSize({width:1440, height:700});
  const imgs = await Promise.all(tiles.map(async t => ({...t, src:'data:image/png;base64,' + (await readFile(t.file)).toString('base64')})));
  const mode = theme => fx.pick(theme === 'dark' ? 'Dark' : 'Light', theme === 'dark' ? '深色' : '浅色');
  await collage.setContent(`<!doctype html><meta charset="utf-8"><style>
    body{margin:0;background:#11141b;font:15px -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans CJK SC",sans-serif;color:#e6e9ef}
    .grid{display:grid;grid-template-columns:repeat(3,1fr);gap:22px;padding:26px}
    figure{margin:0}img{display:block;width:100%;border-radius:10px;box-shadow:0 0 0 1px rgba(255,255,255,.09),0 10px 30px rgba(0,0,0,.35)}
    figcaption{margin-top:10px;display:flex;gap:8px;align-items:baseline}b{font-weight:650}span{color:#a8b1c1;font-size:13px}
    </style><div class="grid">${imgs.map(t => `<figure><img src="${t.src}"><figcaption><b>${t.label}</b><span>${mode(t.theme)}</span></figcaption></figure>`).join('')}</div>`);
  await collage.waitForFunction(() => [...document.images].every(i => i.complete));
  await collage.evaluate(() => document.fonts.ready);
  await collage.locator('.grid').screenshot({path:resolve(out, 'styles.png')});
  await collage.close();
  console.log('Captured styles');
}

export async function login(page, base) {
  await page.goto(base + '/#/sessions/demo-web/chat');
  await page.locator('#login-user').fill('demo');
  await page.locator('#login-pass').fill('synthetic-password');
  await page.locator('#login-btn').click();
  await page.locator('#chat-input').waitFor();
  await page.evaluate(() => document.fonts.ready);
}

export async function newContext(browser, lang, opts) {
  const context = await browser.newContext({viewport:{width:1440, height:960}, deviceScaleFactor:2, locale:lang === 'en' ? 'en-US' : 'zh-CN', timezoneId:'Asia/Shanghai', ...opts});
  await context.addInitScript(lang => { if (window === window.top) localStorage.setItem('agentbox.language', lang); }, lang);
  // Headless Chromium draws nothing through xterm's WebGL renderer (the text is in
  // the buffer, the canvas stays blank); use the DOM renderer the console already
  // falls back to when WebGL is unavailable.
  await context.addInitScript(() => { Object.defineProperty(window, 'WebglAddon', {configurable:true, get:() => undefined, set:() => {}}); });
  // The terminal asks for JetBrains Mono first. Where it is not installed, the generic
  // monospace fallback (often a thin Courier clone) and a taller CJK fallback leave
  // glyphs clipped in the DOM rows; serve one installed mono font that covers both.
  await context.addInitScript(() => {
    const local = names => names.map(n => `local("${n}")`).join(',');
    const css = `@font-face{font-family:"JetBrains Mono";font-weight:400;src:local("JetBrains Mono"),${local(['Noto Sans Mono CJK SC', 'NotoSansMonoCJKsc-Regular', 'DejaVu Sans Mono', 'Liberation Mono'])}}`
      + `@font-face{font-family:"JetBrains Mono";font-weight:700;src:local("JetBrains Mono Bold"),${local(['Noto Sans Mono CJK SC Bold', 'NotoSansMonoCJKsc-Bold', 'DejaVu Sans Mono Bold', 'Liberation Mono Bold'])}}`;
    const add = () => { const s = document.createElement('style'); s.textContent = css; document.head.append(s); document.fonts.load('13px "JetBrains Mono"').catch(() => {}); };
    if (document.head) add(); else document.addEventListener('DOMContentLoaded', add);
  });
  return context;
}

export async function captureShots(browser, lang) {
  const fx = fixture(lang), out = resolve(OUTPUT, lang);
  await mkdir(out, {recursive:true});
  const {base, close} = await staticServer();
  const context = await newContext(browser, lang);
  const api = await mockApi(context, fx, base, {animateTerminal:false});
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  const shot = async name => {
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({path:resolve(out, name + '.png'), animations:'disabled'});
    console.log(`Captured ${lang}/${name}`);
  };
  try {
    await page.emulateMedia({colorScheme:'dark', reducedMotion:'reduce'});
    await login(page, base);
    await tour(page, fx, base, api, {video:false, shot, chapter:async () => {}});
    api.state.history['demo-web'] = completedHistory(fx);
    await screens(page, fx, base, api, shot);
    await styles(page, fx, shot, out);
    await page.setViewportSize({width:390, height:844});
    await page.evaluate(() => { location.hash = '#/sessions/demo-web/chat'; });
    await page.reload();
    await page.locator('#chat-log .answer-duration').first().waitFor();
    await page.locator('#chat-log').evaluate(el => { el.scrollTop = el.scrollHeight; });
    await shot('chat-mobile');
    assert.deepEqual(errors, [], 'Browser errors');
    assert.deepEqual([...new Set(api.state.missing)], [], 'Missing API fixtures');
  } finally {
    await context.close();
    await close();
  }
}

/* The finished task as stored history, for pages loaded after the tour. */
function completedHistory(fx, context = false, duration = fx.duration) {
  const ts = new Date(fx.now - 300000).toISOString();
  const turn = fx.turn;
  const events = steps => steps.filter(step => step !== 'live' && step[1].type !== 'stream_event').map(step => ({kind:'event',ts,event:step[1]}));
  const entries = [{kind:'user',ts,text:fx.task.user,turn}, ...events(fx.steps), {kind:'status',ts,state:'idle',duration_ms:duration}];
  if (context) entries.push({kind:'user',ts,text:'/context',turn:{...turn,id:'demo-context'}}, ...events(fx.contextSteps), {kind:'status',ts,state:'idle',local:true});
  return {entries, thread:{id:'tour-thread',title:fx.task.title,ts,updated:ts,turns:context ? 2 : 1,resumable:true}, costs:{[turn.id]:fx.cost}};
}

export async function captureVideo(browser, lang) {
  const fx = fixture(lang), out = resolve(OUTPUT, lang);
  await mkdir(out, {recursive:true});
  const {base, close} = await staticServer();
  const context = await newContext(browser, lang);
  await context.addInitScript(cursorScript);
  const api = await mockApi(context, fx, base, {animateTerminal:true});
  const page = await context.newPage();
  page.setDefaultTimeout(20000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  try {
    await page.emulateMedia({colorScheme:'dark', reducedMotion:'no-preference'});
    await login(page, base);
    await page.mouse.move(1300, 900);
    await page.waitForTimeout(800);
    const rec = await recorder(page, resolve(out, 'frames'));
    const chapters = [];
    const chapter = async (title, subtitle) => { chapters.push({title, subtitle, start:Date.now() / 1000}); };
    await tour(page, fx, base, api, {video:true, shot:null, chapter});
    const end = Date.now() / 1000;
    const frames = await rec.stop();
    await writeFile(resolve(out, 'tour-timeline.json'), JSON.stringify({lang, chapters, end, frames:frames.map(f => ({file:f.file, t:f.t})), synthetic:true}, null, 2) + '\n');
    console.log(`Recorded ${lang} tour: ${frames.length} frames, ${(end - chapters[0].start).toFixed(1)}s`);
    assert.deepEqual(errors, [], 'Browser errors');
    assert.deepEqual([...new Set(api.state.missing)], [], 'Missing API fixtures');
  } finally {
    await context.close();
    await close();
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const arg = name => { const i = process.argv.indexOf(name); return i > 0 ? process.argv[i + 1] : ''; };
  const langs = !arg('--lang') || arg('--lang') === 'all' ? LANGS : [arg('--lang')];
  for (const lang of langs) assert.ok(LANGS.includes(lang), 'unsupported --lang ' + lang);
  const {chromium} = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
  const browser = await chromium.launch({headless:true, ...(process.env.AGENTBOX_BROWSER_CHANNEL ? {channel:process.env.AGENTBOX_BROWSER_CHANNEL} : {})});
  try {
    for (const lang of langs) {
      if (!process.argv.includes('--no-shots')) await captureShots(browser, lang);
      if (process.argv.includes('--video')) await captureVideo(browser, lang);
    }
  } finally { await browser.close(); }
}
