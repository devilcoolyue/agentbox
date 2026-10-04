#!/usr/bin/env node
// Render the shipped UI with synthetic, local-only API data. No Docker or model calls.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

export async function capture(page, { tour = false, recordingStart = 0 } = {}) {
  const root = resolve(fileURLToPath(new URL('../internal/web/static/', import.meta.url)));
  const output = resolve('output/playwright/readme');
  await mkdir(output, { recursive: true });
  const server = createServer(async (req, res) => {
    try {
      const rel = decodeURIComponent(req.url.split('?')[0]).replace(/^\/_v\/[^/]+/, '');
      const path = resolve(root, '.' + (rel === '/' ? '/index.html' : rel));
      if (!path.startsWith(root + '/')) { res.writeHead(403).end(); return; }
      res.setHeader('Content-Type', ({'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.woff2':'font/woff2'})[extname(path)] || 'application/octet-stream');
      res.end(await readFile(path));
    } catch { res.writeHead(404).end(); }
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  const base = `http://127.0.0.1:${server.address().port}`;
  // Keep documentation captures entirely local, including unexpected asset requests.
  await page.route('**/*', route => new URL(route.request().url()).origin === base
    ? route.continue() : route.abort('blockedbyclient'));
  page.setDefaultTimeout(15000);
  const now = Date.now();
  const stamp = new Date(now - 600000).toISOString();
  const errors = [], missing = [];
  page.on('pageerror', e => errors.push(e.message));
  const models = {claude:[{id:'claude-opus-5',label:'Opus 5'}],codex:[{id:'gpt-5.5',label:'GPT-5.5',reasoning:{support:'supported',control:'effort',levels:['low','medium','high','xhigh']}}]};
  const accounts = [
    {id:'claude-team',type:'claude',label:'Claude · 团队订阅',cred_status:'ok',auth_mode:'oauth',sessions:1,access:{mode:'all'}},
    {id:'codex-team',type:'codex',label:'Codex · 产品研发',cred_status:'ok',auth_mode:'oauth',sessions:2,access:{mode:'all'}},
  ];
  const sessions = [
    {id:'demo-web',name:'产品官网 · 新版工作台',agent:'codex',account_id:'codex-team',account_label:accounts[1].label,status:'running',default_model:'gpt-5.5',created_at:stamp},
    {id:'demo-api',name:'API 服务 · 性能优化',agent:'claude',account_id:'claude-team',account_label:accounts[0].label,status:'running',default_model:'claude-opus-5',created_at:stamp},
    {id:'demo-docs',name:'开发文档 · 知识库',agent:'codex',account_id:'codex-team',account_label:accounts[1].label,status:'stopped',default_model:'gpt-5.5',created_at:stamp},
  ];
  const settings = {listen:'127.0.0.1:8180',agent_image:'agentbox-agent:latest',permission_mode:'bypassPermissions',max_upload_mb:512,idle_timeout_min:30,timezone:'Asia/Shanghai',container:{memory_mb:2048,cpus:2,pids_limit:512,network:'bridge'},resources:{max_running:12,max_running_per_user:3,min_free_bytes:10737418240},models,default_models:{claude:'claude-opus-5',codex:'gpt-5.5'},terminal_tips:{tips:['项目文件放在 /workspace，共享文件放在 /shared'],interval_sec:4,animation:'scroll'},tunnel:{enabled:true,transparent:true,proxy_bind:'172.17.0.1:1080',network_bind:'172.17.0.1:1082',network_image:'agentbox-network:latest'},proxy_bridge:{bind:'172.17.0.1:1081'},pricing:{}};
  const thread = {id:'demo-thread',title:'实现工作台搜索与筛选',ts:stamp,updated:stamp,turns:1,resumable:true};
  const turn = {id:'demo-turn',model:'gpt-5.5',effort:'high',control:'effort'};
  const history = [
    {kind:'user',ts:stamp,text:'帮我完善工作台的搜索和状态筛选，保留现有设计风格，并补上键盘操作。',turn},
    {kind:'event',ts:stamp,event:{type:'item.completed',item:{type:'command_execution',command:'rg --files src/components && npm run check',aggregated_output:'类型检查通过',exit_code:0,status:'completed'}}},
    {kind:'event',ts:stamp,event:{type:'item.completed',item:{type:'agent_message',text:'已完成搜索与状态筛选，沿用了现有布局、间距和主题颜色。\n\n### 这次可以做什么\n\n- **快速定位项目**：按名称搜索，与运行状态组合筛选。\n- **键盘直接操作**：按 `/` 聚焦搜索，`Esc` 清空，方向键选择结果。\n- **刷新后继续工作**：筛选状态保留在 URL 中，链接可以直接分享。\n\n```ts\nconst visibleProjects = projects.filter(project =>\n  matchesQuery(project, query) && matchesStatus(project, status)\n);\n```\n\n类型检查与筛选用例均已通过。你可以在「变更」页查看实现，再决定是否提交。'}}},
    {kind:'event',ts:stamp,event:{type:'turn.completed',usage:{input_tokens:18400,cached_input_tokens:12000,output_tokens:2100}}},
    {kind:'status',ts:stamp,state:'idle'},
  ];
  const rows = Array.from({length:8}, (_, i) => {
    const claude = i % 3 === 1, input = 6400 + i * 800, outputTokens = 2100 + i * 120, cached = 12000 + i * 2000;
    const cost = input * 5 + cached * 0.5 + outputTokens * 30;
    return {id:i+1,ts:now-i*420000,user:i%3===2?'designer':'demo',session_id:claude?'demo-api':'demo-web',session_name:claude?sessions[1].name:sessions[0].name,turn_id:`usage-${i}`,agent:claude?'claude':'codex',account_label:accounts[claude?0:1].label,model:claude?'claude-opus-5':'gpt-5.5',kind:i===3?'terminal':i===6?'title':'chat',billing:claude?'provider':'table',provider:claude?'firstParty':'',input_tokens:input,output_tokens:outputTokens,cache_read_tokens:cached,cache_write_tokens:0,total_tokens:input+outputTokens+cached,cost_micro_usd:cost,duration_ms:18400+i*1100,wall_ms:21000+i*1100,ttft_ms:1100+i*130,rate:{input:5,output:30,cache_read:0.5,cache_write:0,key:claude?'claude-opus-5':'gpt-5.5',basis:claude?'reference':'table',snapshot:true}};
  });
  const total = {rows:rows.length,turns:rows.length};
  for (const key of ['input_tokens','output_tokens','cache_read_tokens','cache_write_tokens','cost_micro_usd']) total[key]=rows.reduce((sum,row)=>sum+row[key],0);
  const skills = [
    {name:'code-review',description:'审查代码变更，重点检查正确性、边界条件与测试覆盖。',source:'template'},
    {name:'api-design',description:'统一 API 结构、错误响应和接口文档。',source:'global'},
    {name:'release-check',description:'发布前核对构建产物、迁移与回退步骤。',source:'session'},
  ].map(s=>({...s,files:3,bytes:4096,updated_at:stamp}));
  const skillContent = '# Code review\n\n在提交前做一次聚焦的代码审查，输出可操作的修改建议。\n\n## 工作流程\n\n1. 阅读项目约定，确认本次改动的目标与范围。\n2. 查看 Git diff，核对输入校验、权限与异常路径。\n3. 运行相关测试，记录验证结果与尚未覆盖的边界。\n\n## 审查重点\n\n- **正确性**：实现是否满足需求，是否保留原有行为。\n- **可维护性**：命名、接口和错误处理是否清晰。\n- **交付质量**：文档与测试是否跟随变更。\n\n## 输出\n\n按影响程度列出发现，附文件位置、原因与建议。没有发现时说明验证范围。';
  const readmeContent = '# Workspace search\n\n搜索与状态筛选的演示项目。\n\n## 快速开始\n\n```sh\nnpm install\nnpm run dev\n```\n\n## 已完成\n\n- 按项目名称搜索，组合运行状态筛选。\n- 键盘导航与快捷聚焦。\n- 筛选条件保存在 URL，刷新后保留。\n\n## 验证\n\n```sh\nnpm run check\nnpm test\n```\n\n打开「变更」页检查实现，再提交到本地仓库。';
  const diff = 'diff --git a/src/components/ProjectList.tsx b/src/components/ProjectList.tsx\nindex 1234567..abcdef0 100644\n--- a/src/components/ProjectList.tsx\n+++ b/src/components/ProjectList.tsx\n@@ -12,8 +12,18 @@ export function ProjectList({ projects }) {\n   const [query, setQuery] = useState(\'\');\n+  const [status, setStatus] = useState(\'all\');\n \n-  const visibleProjects = projects;\n+  const visibleProjects = projects.filter(project =>\n+    matchesQuery(project, query) &&\n+    matchesStatus(project, status)\n+  );\n \n   return (\n     <section className="project-list">\n+      <ProjectFilters\n+        query={query}\n+        status={status}\n+        onQueryChange={setQuery}\n+        onStatusChange={setStatus}\n+      />\n       <ProjectGrid projects={visibleProjects} />\n     </section>\n   );\n';
  await page.routeWebSocket('**/api/sessions/*/chat?*', ws => ws.send(JSON.stringify({type:'status',state:'idle'})));
  await page.routeWebSocket('**/api/sessions/*/term?*', ws => {
    let sent = false;
    ws.onMessage(() => {
      if (sent) return; sent=true;
      ws.send(Buffer.from('\x1b[2J\x1b[H\x1b[38;5;214m  /workspace  ·  产品官网\x1b[0m\r\n\r\n\x1b[32magent@agentbox\x1b[0m:\x1b[34m/workspace\x1b[0m$ npm run check\r\n\r\n> workspace-web@1.0.0 check\r\n> tsc --noEmit\r\n\r\n\x1b[32m✓\x1b[0m TypeScript 类型检查通过\r\n\r\n\x1b[32magent@agentbox\x1b[0m:\x1b[34m/workspace\x1b[0m$ npm test\r\n\r\n \x1b[46;30m RUN \x1b[0m  v3.2.0 /workspace\r\n\r\n \x1b[32m✓\x1b[0m src/search.test.ts (6 tests) 12ms\r\n \x1b[32m✓\x1b[0m src/filters.test.ts (4 tests) 8ms\r\n \x1b[32m✓\x1b[0m src/keyboard.test.ts (3 tests) 16ms\r\n\r\n Test Files  \x1b[32m3 passed\x1b[0m (3)\r\n      Tests  \x1b[32m13 passed\x1b[0m (13)\r\n   Duration  482ms\r\n\r\n\x1b[32magent@agentbox\x1b[0m:\x1b[34m/workspace\x1b[0m$ git status --short\r\n M src/components/ProjectList.tsx\r\n M src/styles/workspace.css\r\n?? src/filters.test.ts\r\n\r\n\x1b[32magent@agentbox\x1b[0m:\x1b[34m/workspace\x1b[0m$ '));
    });
  });
  await page.route('**/api/**', async route => {
    const url = new URL(route.request().url()), path=url.pathname;
    let body;
    if(path==='/api/ping') { await route.fulfill({status:204}); return; }
    else if(path==='/api/login') body={token:'synthetic-docs-token',user:'demo',role:'admin'};
    else if(path==='/api/me') body={user:'demo',role:'admin',timezone:'Asia/Shanghai',models,quota:{metered:true,enforced:false,balance_micro_usd:42760000},terminal_tips:settings.terminal_tips};
    else if(path==='/api/sessions') body=sessions;
    else if(path==='/api/accounts') body=accounts;
    else if(path==='/api/settings') body=settings;
    else if(path==='/api/proxies'||path==='/api/users') body=[];
    else if(path==='/api/tunnel/clients') body=['darwin-arm64','darwin-amd64','windows-amd64.exe','linux-amd64','linux-arm64'].map(platform=>({name:'abox-link-'+platform,size:12582912}));
    else if(path.startsWith('/api/updates')) body={current_version:'dev',revision:'',built_at:'',latest_version:'',available:false,comparable:false};
    else if(path==='/api/usage') body={total,...total,by_model:[],by_user:[]};
    else if(path==='/api/usage/events') body={rows,total,facets:{users:['demo','designer'],agents:['claude','codex'],models:['claude-opus-5','gpt-5.5']},scope:'all',timezone:'Asia/Shanghai',order:'desc',limit:20,offset:0,sync:{last_scan_at:now,last_success_at:now,scanning:false,errors:0}};
    else if(path==='/api/tunnel/status') body={enabled:true,transparent:true,client_transparent:true,connected:true,proxy_up:true,since:now-3600000,remote:'192.0.2.10:53000',maps:[],rules:['10.20.0.0/16','gitlab.corp.example:443','db.corp.example:5432'],workspaces:sessions.filter(s=>s.status==='running').map(s=>({session:s.id,name:s.name,ready:true}))};
    else if(path.endsWith('/models')) body={models:path.includes('demo-api')?models.claude:models.codex,default_reasoning:{support:'supported',control:'effort',levels:['low','medium','high','xhigh']},discovery:'available'};
    else if(path.endsWith('/skills')) body={skills};
    else if(path.includes('/skills/')) body={...skills[0],content:skillContent,truncated:false,more:false,entries:[{path:'SKILL.md',size:1800,mtime:stamp},{path:'references',dir:true,size:0,mtime:stamp},{path:'references/checklist.md',size:1500,mtime:stamp}]};
    else if(path.endsWith('/mcp')) body={revision:1,user_revision:1,project_names:[],items:[
      {name:'workspace-files',source:'user',status:'applied',config:{type:'stdio',command:'npx',args:['-y','@modelcontextprotocol/server-filesystem','/workspace']}},
      {name:'team-docs',source:'session',status:'applied',config:{type:'http',url:'https://docs.example.com/mcp'}},
      {name:'design-assets',source:'user',status:'applied',disabled:true,config:{type:'http',url:'https://design.example.com/mcp'}},
    ]};
    else if(path.endsWith('/history')) body={entries:history,thread,costs:{'demo-turn':{turn_id:'demo-turn',cost_micro_usd:101000,source:'table'}}};
    else if(path.endsWith('/chat/threads')) body={threads:[thread],active:thread.id};
    else if(path.endsWith('/git/status')) body={is_repo:true,repo:'',repos:[''],branch:'feat/workspace-search',files:[{path:'src/components/ProjectList.tsx',status:' M'},{path:'src/styles/workspace.css',status:' M'},{path:'src/filters.test.ts',status:'??',untracked:true}]};
    else if(path.endsWith('/git/diff')) {await route.fulfill({contentType:'text/plain',body:diff});return;}
    else if(path.endsWith('/file')) {await route.fulfill({contentType:'text/plain; charset=utf-8',body:readmeContent});return;}
    else if(path.endsWith('/files')) body=(url.searchParams.get('path')==='src'?['components/','styles/','filters.test.ts','search.ts']:['src/','public/','docs/','package.json','README.md','tsconfig.json']).map((name,i)=>({name:name.replace(/\/$/,''),is_dir:name.endsWith('/'),size:name.endsWith('/')?4096:820+i*420,mode:name.endsWith('/')?'drwxr-xr-x':'-rw-r--r--',mtime:stamp}));
    else if(/^\/api\/sessions\/[^/]+$/.test(path)) body=sessions.find(s=>path.endsWith(s.id));
    else {missing.push(path);await route.fulfill({status:404,json:{error:'No screenshot fixture: '+path}});return;}
    await route.fulfill({json:body});
  });
  try {
    await page.setViewportSize({width:1440,height:960});
    await page.emulateMedia({colorScheme:'dark',reducedMotion:'reduce'});
    await page.goto(base+'/#/sessions/demo-web/chat');
    await page.locator('#login-user').fill('demo');
    await page.locator('#login-pass').fill('synthetic-password');
    await page.locator('#login-btn').click();
    await page.locator('.answer-footer').waitFor();
    await page.evaluate(()=>document.fonts.ready);
    if (tour) {
      const chapters = [];
      const scene = async (en, cn, action, hold = 4100) => {
        chapters.push({en,cn,start:(performance.now()-recordingStart)/1000});
        await action();
        await page.mouse.move(1420,930,{steps:12});
        await page.waitForTimeout(hold);
      };
      await scene('01  Assign a task', '对话 · 在同一个空间继续工作', async()=>{});
      await scene('02  Use the original terminal', '终端 · 运行命令，检查测试结果', async()=>{
        await page.locator('.tab[data-tab="term"]').click();
        await page.locator('#term-state[data-state="connected"]').waitFor();
        await page.locator('.xterm-screen').first().waitFor();
      });
      await scene('03  Review the changes', '变更 · 逐文件检查 Git diff', async()=>{
        await page.locator('.tab[data-tab="changes"]').click();
        await page.locator('.change-row').first().click();
        await page.locator('#changes-diff').getByText('matchesQuery(project, query) &&',{exact:false}).waitFor();
      });
      await scene('04  Browse and preview files', '文件 · 阅读成果，再决定如何交付', async()=>{
        await page.locator('.tab[data-tab="files"]').click();
        await page.locator('#files-list').getByText('README.md',{exact:true}).dblclick();
        await page.locator('#fv-md').getByText('Workspace search',{exact:true}).waitFor();
      });
      await page.locator('#fv-close').click();
      await scene('05  Reuse skills and templates', '技能 · 把工作方法复用到新空间', async()=>{
        await page.goto(base+'/#/sessions/demo-api/skills');
        await page.locator('.skill-row').filter({hasText:'code-review'}).click();
        await page.locator('#skills-detail').getByText('Code review',{exact:true}).waitFor();
      });
      await scene('06  Connect tools with MCP', 'MCP · 用户默认配置与空间覆盖', async()=>{
        await page.locator('.tab[data-tab="mcp"]').click();
        await page.locator('.mcp-row').first().waitFor();
      });
      await scene('07  Understand usage and costs', '用量 · 查看模型、费用与延迟', async()=>{
        await page.goto(base+'/#/usage');
        await page.locator('#usage-rows tr').first().waitFor();
      });
      await scene('08  Reach your private services', '内网 · abox-link 按白名单连接', async()=>{
        await page.goto(base+'/#/tunnel');
        await page.getByText('gitlab.corp.example:443',{exact:false}).first().waitFor();
      });
      const end = (performance.now()-recordingStart)/1000;
      await writeFile(resolve(output,'tour-timeline.json'),JSON.stringify({chapters,end,synthetic:true},null,2)+'\n');
      assert.deepEqual(errors,[], 'Browser errors');
      assert.deepEqual([...new Set(missing)],[], 'Missing API fixtures');
      return;
    }
    const shot = async name => {
      await page.evaluate(()=>document.fonts.ready);
      await page.mouse.move(0,0);
      await page.screenshot({path:resolve(output,name+'.png'),animations:'disabled'});
      console.log('Captured '+name);
    };
    await shot('chat-dark');
    await page.emulateMedia({colorScheme:'light'});
    await shot('chat-light');
    await page.emulateMedia({colorScheme:'dark'});
    await page.locator('.tab[data-tab="changes"]').click();
    await page.locator('.change-row').first().click();
    await page.locator('#changes-diff').getByText('matchesQuery(project, query) &&',{exact:false}).waitFor();
    await shot('changes');
    await page.locator('.tab[data-tab="files"]').click();
    await page.locator('#files-list .file-row').first().waitFor();
    await page.getByRole('button',{name:'展开目录',exact:true}).first().click();
    await page.getByText('search.ts',{exact:true}).waitFor();
    await shot('files');
    await page.locator('#files-list').getByText('README.md',{exact:true}).dblclick();
    await page.locator('#fv-md').getByText('Workspace search',{exact:true}).waitFor();
    await shot('preview');
    await page.locator('#fv-close').click();
    await page.locator('.tab[data-tab="term"]').click();
    await page.locator('#term-state[data-state="connected"]').waitFor();
    await page.locator('.xterm-screen').first().waitFor();
    await page.waitForTimeout(300); // Let xterm paint the synthetic PTY frame.
    await shot('terminal');
    await page.goto(base+'/#/usage');
    await page.locator('#usage-rows tr').first().waitFor();
    await shot('usage');
    await page.goto(base+'/#/tunnel');
    await page.getByText('gitlab.corp.example:443',{exact:false}).first().waitFor();
    await page.locator('#tun-dl a').first().waitFor();
    await shot('tunnel');
    await page.goto(base+'/#/settings/accounts');
    await page.locator('#sec-accounts').getByText(accounts[0].label,{exact:true}).waitFor();
    await shot('accounts');
    await page.goto(base+'/#/sessions/demo-api/skills');
    await page.locator('.skill-row').filter({hasText:'code-review'}).click();
    await page.locator('#skills-detail').getByText('Code review',{exact:true}).waitFor();
    await shot('skills');
    await page.locator('.tab[data-tab="mcp"]').click();
    await page.locator('.mcp-row').first().waitFor();
    await shot('mcp');
    await page.setViewportSize({width:390,height:844});
    await page.goto(base+'/#/sessions/demo-web/chat');
    await page.locator('.answer-footer').waitFor();
    await shot('chat-mobile');
    assert.deepEqual(errors,[], 'Browser errors');
    assert.deepEqual([...new Set(missing)],[], 'Missing API fixtures');
  } finally {
    await page.unrouteAll({behavior:'wait'});
    server.closeAllConnections();
    await new Promise(r=>server.close(r));
  }
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const {chromium} = await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE ? pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href : 'playwright');
  const browser = await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL?{channel:process.env.AGENTBOX_BROWSER_CHANNEL}:{})});
  try {
    const context = await browser.newContext({deviceScaleFactor:2,locale:'zh-CN'});
    await capture(await context.newPage());
    await context.close();
    if (process.argv.includes('--video')) {
      const recording = await browser.newContext({viewport:{width:1440,height:960},locale:'zh-CN',recordVideo:{dir:resolve('output/playwright/readme/video'),size:{width:1440,height:960}}});
      const recordingStart = performance.now();
      const page = await recording.newPage();
      await capture(page,{tour:true,recordingStart});
      const video = page.video();
      await recording.close();
      await video.saveAs(resolve('output/playwright/readme/tour.webm'));
    }
  } finally {await browser.close();}
}
