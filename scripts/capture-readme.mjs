#!/usr/bin/env node
// Render the shipped UI with synthetic, local-only API data. No Docker or model calls.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

export async function capture(page) {
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
    else if(path.endsWith('/models')) body={models:models.codex,default_reasoning:{support:'supported',control:'effort',levels:['low','medium','high','xhigh']},discovery:'available'};
    else if(path.endsWith('/history')) body={entries:history,thread,costs:{'demo-turn':{turn_id:'demo-turn',cost_micro_usd:101000,source:'table'}}};
    else if(path.endsWith('/chat/threads')) body={threads:[thread],active:thread.id};
    else if(path.endsWith('/git/status')) body={is_repo:true,repo:'',repos:[''],branch:'feat/workspace-search',files:[{path:'src/components/ProjectList.tsx',status:' M'},{path:'src/styles/workspace.css',status:' M'},{path:'src/filters.test.ts',status:'??',untracked:true}]};
    else if(path.endsWith('/git/diff')) {await route.fulfill({contentType:'text/plain',body:diff});return;}
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
    await page.locator('.tab[data-tab="term"]').click();
    await page.locator('#term-state[data-state="connected"]').waitFor();
    await page.locator('.xterm-screen canvas').first().waitFor();
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
  try {await capture(await browser.newPage());} finally {await browser.close();}
}
