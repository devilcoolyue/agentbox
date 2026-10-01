#!/usr/bin/env node
// Git UI regression against synthetic API responses. No Docker or real Git credentials.
import assert from 'node:assert/strict';
import { assertActionIcons } from './test-actions.mjs';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

export async function smoke(page) {
 const root = resolve(fileURLToPath(new URL('../internal/web/static/', import.meta.url)));
 const server = createServer(async (req,res) => {
  try {
   const rel=decodeURIComponent(req.url.split('?')[0]).replace(/^\/_v\/[^/]+/,'');
   const path=resolve(root,'.'+(rel==='/'?'/index.html':rel));
   if(!path.startsWith(root+'/')) {res.writeHead(403).end();return;}
   res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(path)]||'application/octet-stream');
   res.end(await readFile(path));
  } catch {res.writeHead(404).end();}
 });
 await new Promise(r=>server.listen(0,'127.0.0.1',r));
 const base=`http://127.0.0.1:${server.address().port}`;
 const backToWorkspace=async()=>{await page.goto(base+'/#/sessions/fixture/changes');await page.locator('#tab-changes').waitFor({state:'visible'});};
 const errors=[];page.on('pageerror',e=>errors.push(e.stack || e.message));
 let role='user',oauthStarts=0,oauthWrites=0;let oauthApps=[{id:'oauth-fixture',label:'Company GitLab OAuth',provider:'gitlab',base_url:'https://git.example.com',enabled:true,client_id:'fixture-client',redirect_url:base+'/api/git/oauth/callback',revision:1}];
 let profile={user:'fixture',name:'fixture',email:'fixture@localhost',updated_at:'0001-01-01T00:00:00Z'};
 let writes=0, commits=0, failProfile=false, failCommit=true;
 let connections=[], bindings=[], defaultConnection='', fetches=0, pushes=0;
 let terminalGrants=[],terminalCreates=0;
 let shareWrites=0;let shares=[];
 let reviewCreates=0;const reviews=[];
 let branchActions=0;const branchRows=[{name:'main',head:'1'.repeat(40),upstream:'origin/main',current:true,remote:false},{name:'origin/main',head:'1'.repeat(40),upstream:'',current:false,remote:true}];
 let previewRequests=0, holdNextFetch=false, activeGit=[], finishCancelled;
 const state={is_repo:true,repo:'project',repos:['project'],branch:'main',head:'1'.repeat(40),upstream:'origin/main',ahead:2,behind:1,tracking_known:true,remotes:[{name:'origin',url:'https://git.example.com/team/project.git',push:false}],files:[{path:'example.txt',status:' M',untracked:false}]};
 await page.routeWebSocket('**/api/sessions/*/chat?*',()=>{});
 await page.route('**/api/**', async route=>{
  const path=new URL(route.request().url()).pathname;
  let body={};
  if(path==='/api/login') body={token:'fixture-token'};
  else if(path==='/api/me') body={user:'fixture',role,timezone:'UTC',quota:{metered:false},models:{},terminal_tips:null};
  else if(path==='/api/updates' || path==='/api/updates/check') body={current_version:'dev',revision:'unknown',built_at:'unknown',latest_version:'',checked_at:0,attempted_at:Date.now(),error:'',notes:'',available:false,comparable:false};
  else if(path==='/api/me/git') {
   if(failProfile) {await route.fulfill({status:500,json:{error:'fixture profile failure'}});return;}
   if(route.request().method()==='PUT'){writes++;profile={...profile,...route.request().postDataJSON()};}
   body=profile;
  }
  else if(path==='/api/git/oauth/apps') {
   if(route.request().method()==='PUT'){oauthWrites++;const data=route.request().postDataJSON();assert.equal(data.client_secret,'synthetic-app-secret');const {client_secret,...metadata}=data;oauthApps.push({...metadata,id:'new-oauth',revision:1});body={ok:true};}
   else body=oauthApps;
  }
  else if(path==='/api/git/oauth/start') {
   oauthStarts++;assert.equal(route.request().postDataJSON().app_id,'oauth-fixture');
   assert.equal(route.request().postDataJSON().read_only,true);body={url:'https://git.example.com/oauth/authorize?state=synthetic-state',scope:'read_user read_repository'};
  }
  else if(path==='/api/git/connections') {
   if(route.request().method()==='POST') {
    const value=route.request().postDataJSON();
    if(value.auth_type==='ssh'){
     assert.equal(value.network.route,'tunnel');assert.equal(value.username,'git');assert.equal(value.private_key,'synthetic-private-key');assert.equal(value.host_key,'ssh-ed25519 synthetic-host-key');
     const {private_key,passphrase,host_key,token,...metadata}=value;
     const connection={...metadata,id:'ssh-fixture',owner:'fixture',revision:1,enabled:true,auth_type:'ssh',host_fingerprint:'SHA256:fixture'};connections.push(connection);await route.fulfill({json:connection});return;
    }
    assert.equal(value.token,'synthetic-browser-token');
    const {token,...metadata}=value;const connection={...metadata,id:'git-fixture',owner:'fixture',revision:1,enabled:true,auth_type:'pat'};connections.push(connection);body=connection;
   } else body=connections;
  }
  else if(path==='/api/users') body=[{name:'fixture',role:'admin'},{name:'alice',role:'user'}];
  else if(path==='/api/git/connections/git-fixture/shares') {
   if(route.request().method()==='PUT'){const value=route.request().postDataJSON();assert.equal(value.revision,connections[0].revision);assert.deepEqual(value.users,[{user:'alice',write:false}]);shareWrites++;shares=value.users;connections[0].revision++;}
   body={users:shares,revision:connections[0].revision};
  }
  else if(path==='/api/git/connections/git-fixture') {
   const value=route.request().postDataJSON();assert.equal(value.revision,connections[0].revision);
   const {revision,...patch}=value;connections[0]={...connections[0],...patch,revision:revision+1};body=connections[0];
  }
  else if(path==='/api/me/git/default') {
   if(route.request().method()==='PUT')defaultConnection=route.request().postDataJSON().connection_id;
   body={connection_id:defaultConnection};
  }
  else if(path.endsWith('/git/default')) body={connection_id:''};
  else if(path.endsWith('/git/bindings')) {
   if(route.request().method()==='PUT') {
    const value=route.request().postDataJSON();assert.equal(value.remote,'origin');assert.equal(value.repo,'project');
    bindings=[{...value,session_id:'fixture',revision:1,url:'https://git.example.com/team/project.git'}];
   }
   body=bindings;
  }
  else if(path.endsWith('/git/clone')) {
   assert.deepEqual(route.request().postDataJSON(),{connection_id:'git-fixture',url:'https://git.example.com/team/clone.git',directory:'cloned-project'});
   state.repo='cloned-project';state.repos=['project','cloned-project'];body={ok:true,repo:'cloned-project',warning:''};
  }
  else if(path==='/api/git/operations') body={rows:[{id:1,started_at:new Date().toISOString(),finished_at:new Date().toISOString(),actor:'fixture',session_id:'fixture',repo:'project',connection_id:'git-fixture',operation:'fetch',target:'origin <script>literal</script>',result:'success'}],active:activeGit,next_before:0};
  else if(path.startsWith('/api/git/operations/') && path.endsWith('/cancel')) {
   activeGit[0].cancel_requested=true;finishCancelled();body={cancel_requested:true};
  }
  else if(path.endsWith('/git/terminal')) {
   if(route.request().method()==='POST'){
    const value=route.request().postDataJSON();assert.equal(value.repo,'project');assert.equal(value.remote,'origin');assert.equal(value.write,false);terminalCreates++;
    const grant={id:'fixture-grant',...value,expires_at:new Date(Date.now()+1800000).toISOString(),command:'/home/agent/.agentbox-git/fixture-grant/abox-git'};terminalGrants.push(grant);body=grant;
   }else body=terminalGrants;
  }
  else if(path.endsWith('/git/terminal/fixture-grant')) {assert.equal(route.request().method(),'DELETE');terminalGrants=[];body={revoked:true};}
  else if(path.endsWith('/git/fetch')) {
   fetches++;
   if(holdNextFetch){
    holdNextFetch=false;
    activeGit=[{request_id:route.request().headers()['x-git-request-id'],id:1,operation:'fetch',session_id:'fixture',started_at:new Date().toISOString(),elapsed_ms:1000,phase:'transferring',received_bytes:1024,sent_bytes:100,cancel_requested:false}];
    await new Promise(resolve=>{finishCancelled=resolve;});activeGit=[];
    await route.fulfill({status:409,json:{error:'synthetic operation cancelled'}});return;
   }
   body={ok:true,fetched_at:new Date().toISOString()};
  }
  else if(path.endsWith('/git/push-preview')) {
   previewRequests++;body={repo:'project',remote:'origin',url:'https://git.example.com/team/project.git',connection:'Company Git',ref:'refs/heads/main',expected_head:'a'.repeat(40),expected_remote_head:'b'.repeat(40),commits:'aaaaaaa fixture commit',new_branch:false};
  }
  else if(path.endsWith('/git/push')) {
   pushes++;const value=route.request().postDataJSON();assert.equal(value.ref,'refs/heads/main');assert.equal(value.expected_head,'a'.repeat(40));assert.equal(value.expected_remote_head,'b'.repeat(40));body={ok:true,pushed:true};
  }
  else if(path==='/api/sessions') body=[{id:'fixture',name:'Git demo',agent:'codex',account_id:'fixture',account_label:'Fixture account',status:'stopped',default_model:'fixture'}];
  else if(path==='/api/accounts') body=[];
  else if(path.endsWith('/git/reviews')) {
   if(route.request().method()==='POST'){
    const request=route.request().postDataJSON();assert.equal(request.source,'feature/browser');assert.equal(request.target,'main');assert.equal(request.expected_head,'1'.repeat(40));assert.equal(request.expected_target,'2'.repeat(40));reviewCreates++;
    const review={number:9,title:request.title,url:'https://git.example.com/team/project/pull/9',source:request.source,target:request.target,state:'open',draft:request.draft};reviews.push(review);body={review,existing:false};
   }else body={provider:'github',project:'team/project',connection_id:'git-fixture',read_only:false,rows:reviews,page:1,has_more:false,default_branch:'main',source_branch:'feature/browser',head:'1'.repeat(40)};
  }
  else if(path.endsWith('/git/review-preview')){
   const request=route.request().postDataJSON();body={provider:'github',project:'team/project',connection_id:'git-fixture',source:{name:request.source,sha:'1'.repeat(40),protected:false},target:{name:request.target,sha:'2'.repeat(40),protected:true},title:request.title,body:request.body,draft:request.draft,existing:[]};
  }
  else if(path.endsWith('/git/diff') || path.endsWith('/git/file')) { await route.fulfill({body:'diff --git a/example.txt b/example.txt\n@@ -1 +1 @@\n-old value\n+new value\n',contentType:'text/plain'});return; }
  else if(path.endsWith('/git/status')) body=state;
  else if(path.endsWith('/git/branches')) {
   if(route.request().method()==='POST') {
    const value=route.request().postDataJSON();branchActions++;assert.equal(value.expected_head,'1'.repeat(40));assert.equal(value.expected_branch,'main');assert.equal(value.action,'create');assert.equal(value.name,'feature/browser');
    branchRows[0].current=false;branchRows.push({name:value.name,head:'1'.repeat(40),upstream:'',current:true,remote:false});state.branch=value.name;body={ok:true};
   }else body={branches:branchRows,state:{branch:state.branch,head:'1'.repeat(40),detached:false,unborn:false},dirty:false,truncated:false};
  }

  else if(path.endsWith('/git/commit')) {
   commits++;assert.deepEqual(route.request().postDataJSON(),{message:'fixture change',repo:'project'});
   if(failCommit){failCommit=false;await route.fulfill({status:400,json:{error:'fixture commit failed'}});return;}
   state.files=[];body={sha:'a'.repeat(40),pushed:false,output:'local success'};
  }
  else if(path.endsWith('/history')) body={entries:[],thread:null,costs:{}};
  else if(path.endsWith('/models')) body={models:[],default_reasoning:{support:'unknown'},discovery:'stopped'};
  else if(path==='/api/tunnel/status') body={enabled:false};
  else if(path.includes('/git/')) throw new Error('unexpected Git operation: '+path);
  await route.fulfill({json:body});
 });
 try {
  page.setDefaultTimeout(10000);
  await page.setViewportSize({width:1440,height:960});
  await page.goto(base+'/#/sessions/fixture/changes');
  await page.locator('#login-user').fill('fixture');await page.locator('#login-pass').fill('fixture-password');await page.locator('#login-btn').click();
  await page.locator('#tab-changes').waitFor({state:'visible'});
  await page.locator('#btn-changes-commit:not([disabled])').waitFor();
  assert.match(await page.locator('#changes-remote-state').innerText(),/领先 2 \/ 落后 1.*本地缓存/);
  assert.equal(await page.locator('#btn-settings').isVisible(),false);
  await mkdir('output/playwright',{recursive:true});
  for (const width of [360,390,430,768,1280,1440]) {
   await page.setViewportSize({width,height:900});
   for (const theme of ['light','dark']) {
    await page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
    assert.ok(await page.locator('.changes-bar').evaluate(e=>e.scrollWidth<=e.clientWidth),'changes toolbar overflow');
    await page.screenshot({animations:'disabled',path:`output/playwright/responsive-changes-${width}-${theme}.png`});
    if(width<=760) {
     await page.locator('#changes-tracking').click();
     assert.equal(await page.locator('#changes-remote-detail').isVisible(),true);
     await page.locator('#changes-tracking').click();
     await page.locator('#changes-more').click();
     assert.equal(await page.locator('#btn-changes-discard-all').isVisible(),true);
     await page.keyboard.press('Escape');
     assert.equal(await page.locator('#changes-more-panel').isVisible(),false);
     await page.locator('.change-row').first().click();
     await page.locator('#changes-diff .diff').waitFor();
     assert.equal(await page.locator('#changes-list').isVisible(),false);
     assert.equal(await page.locator('.changes-bar').isVisible(),false);
     await page.screenshot({animations:'disabled',path:`output/playwright/responsive-diff-${width}-${theme}.png`});
     await page.locator('#changes-back').click();
     assert.equal(await page.locator('#changes-list').isVisible(),true);
    }
   }
  }
  await page.setViewportSize({width:1440,height:960});
  await page.locator('#btn-git-management').click(); // sidebar tool, next to usage and settings
  await page.locator('#view-git').waitFor({state:'visible'});
  assert.match(page.url(), /#\/git\/guide$/);
  assert.equal(await page.locator('dialog[open]').count(),0,'Git management opened a modal');
  await page.locator('[data-git-go=profile]').click();
  await page.goBack();await page.locator('.git-guide').waitFor();
  await page.goForward();
  const profileDialog=page.locator('#git-profile');
  await profileDialog.locator('fieldset:not([disabled])').waitFor();
  await profileDialog.locator('[name=name]').fill('Alice Example');await profileDialog.locator('[name=email]').fill('alice@example.com');
  await profileDialog.locator('[type=submit]').click();await profileDialog.locator('[role=status]').filter({hasText:'已保存'}).waitFor();
  await backToWorkspace();
  assert.equal(writes,1);
  await page.locator('#btn-changes-commit').click();
  await page.locator('#git-commit-ok:not([disabled])').waitFor();
  assert.match(await page.locator('#git-commit-identity').innerText(),/Alice Example <alice@example.com>/);
  assert.match(await page.locator('#git-commit-target').innerText(),/Git demo · project/);
  await page.locator('#git-commit-msg').fill('fixture change');await page.locator('#git-commit-ok').click();
  await page.locator('#git-commit-error').filter({hasText:'fixture commit failed'}).waitFor();
  assert.equal(await page.locator('#git-commit-msg').inputValue(),'fixture change');
  await page.locator('#git-commit-ok').click();await page.locator('#dlg-git-commit').waitFor({state:'hidden'});
  await page.locator('#changes-list').filter({hasText:'没有未提交的改动'}).waitFor();
  assert.equal(commits,2);assert.equal(await page.locator('#btn-changes-commit').isDisabled(),true);
  await page.locator('#btn-changes-branches').click();
  const branchesDialog=page.locator('#dlg-git-branches');
  await branchesDialog.locator('[type=submit]:not([disabled])').waitFor();
  await branchesDialog.locator('[name=name]').fill('feature/browser');
  await branchesDialog.locator('[type=submit]').click();
  await branchesDialog.locator('[data-current]').filter({hasText:'feature/browser'}).waitFor();
  assert.equal(branchActions,1);await assertActionIcons(page);
  await branchesDialog.locator('[data-close]').click();
  if (await page.locator('#changes-more').isVisible()) await page.locator('#changes-more').click();
  await page.locator('#btn-changes-profile').click();await profileDialog.locator('fieldset:not([disabled])').waitFor();
  assert.equal(await profileDialog.locator('[name=email]').inputValue(),'alice@example.com');
  await mkdir('output/playwright',{recursive:true});
  await page.screenshot({animations:'disabled',path:'output/playwright/git-profile-desktop.png'});
  await backToWorkspace();
  await page.setViewportSize({width:390,height:844});
  if (await page.locator('#changes-more').isVisible()) await page.locator('#changes-more').click();
  await page.locator('#btn-changes-profile').click();await profileDialog.locator('fieldset:not([disabled])').waitFor();
  assert.equal(await profileDialog.evaluate(el=>el.scrollWidth<=el.clientWidth),true,'profile page horizontal overflow');
  await page.screenshot({animations:'disabled',path:'output/playwright/git-profile-mobile.png'});
  await backToWorkspace();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true,'page horizontal overflow');
  await page.setViewportSize({width:1440,height:960});
  await page.locator('#btn-changes-remote').click();
  const remoteDialog=page.locator('#dlg-git-remote');
  await remoteDialog.locator('[data-manage]:not([disabled])').waitFor();
  await remoteDialog.locator('[data-manage]').click();
  const manager=page.locator('#git-connections');
  assert.equal(await page.locator('dialog[open]').count(),0,'workspace remote dialog remained over Git page');
  await page.reload();
  await manager.waitFor();
  assert.match(page.url(), /#\/git\/connections$/);
  await manager.locator('[data-list]').filter({hasText:'尚未添加'}).waitFor();
  assert.equal(await manager.locator('[data-oauth-apps]').isVisible(),false,'ordinary user sees OAuth application editing');
  await manager.locator('[data-oauth]').click();
  const oauthDialog=page.locator('#dlg-git-oauth');
  await oauthDialog.locator('[type=submit]:not([disabled])').waitFor();
  await oauthDialog.locator('[name=label]').fill('My company OAuth');
  await oauthDialog.locator('[type=submit]').click();
  await oauthDialog.locator('[data-link]').waitFor({state:'visible'});
  assert.equal(oauthStarts,1);assert.match(await oauthDialog.locator('[data-link] a').getAttribute('href'),/^https:\/\/git\.example\.com\/oauth\/authorize/);
  await oauthDialog.locator('[data-close]').click();
  await manager.locator('[data-add]').click();
  const editor=page.locator('#dlg-git-connection-edit');
  assert.equal(await page.locator('dialog[open]').count(),0,'connection editor opened a modal');
  await editor.locator('[name=label]').fill('Company Git');
  await editor.locator('[name=base_url]').fill('https://git.example.com');
  await editor.locator('[name=token]').fill('synthetic-browser-token');
  await editor.locator('[name=read_only]').uncheck();
  await editor.locator('[type=submit]').click();await editor.waitFor({state:'detached'});
  await manager.locator('.git-connection-row').waitFor();
  assert.equal(await manager.innerText().then(t=>t.includes('synthetic-browser-token')),false);
  await manager.getByRole('button',{name:'设为默认',exact:true}).click();
  await manager.locator('strong').filter({hasText:'默认'}).waitFor();
  await page.screenshot({animations:'disabled',path:'output/playwright/git-connections-desktop.png'});
  await manager.locator('[data-add]').click();
  await editor.locator('[name=label]').fill('Company SSH');
  await editor.locator('.select-trigger').first().click();
  await page.getByRole('option',{name:'SSH 私钥',exact:true}).click();
  await editor.locator('[name=base_url]').fill('ssh://git.example.com');
  await editor.locator('[name=private_key]').fill('synthetic-private-key');
  await editor.locator('[name=host_key]').fill('ssh-ed25519 synthetic-host-key');
  await editor.locator('label').filter({has:page.locator('select[name=route]')}).locator('.select-trigger').click();
  await page.getByRole('option',{name:'我的内网隧道',exact:true}).click();
  await page.setViewportSize({width:390,height:844});
  await page.screenshot({animations:'disabled',path:'output/playwright/git-ssh-editor-mobile.png'});
  await editor.locator('[type=submit]').click();await editor.waitFor({state:'detached'});
  await manager.locator('strong').filter({hasText:'Company SSH'}).waitFor();
  assert.equal((await manager.innerText()).includes('synthetic-private-key'),false);
  assert.match(await manager.innerText(),/SHA256:fixture/);
  await page.setViewportSize({width:1440,height:960});
  await backToWorkspace();
  await page.locator('#btn-changes-remote').click();
  await remoteDialog.locator('[data-reload]').click();
  await remoteDialog.locator('[data-bind]:not([disabled])').waitFor();
  await remoteDialog.locator('[data-bind]').click();
  await remoteDialog.locator('[data-fetch]:not([disabled])').waitFor();
  await remoteDialog.locator('[data-fetch]').click();
  await remoteDialog.locator('[data-state]').filter({hasText:'已获取最新远程分支'}).waitFor();
  assert.equal(fetches,1);assert.equal(pushes,0,'fetch unexpectedly pushed');await assertActionIcons(page);
  holdNextFetch=true;
  await remoteDialog.locator('[data-fetch]').click();
  await remoteDialog.locator('.git-operation-progress button:not([disabled])').waitFor();
  assert.match(await remoteDialog.locator('.git-operation-progress p').innerText(),/传输数据/);
  await remoteDialog.locator('.git-operation-progress button').click();
  await remoteDialog.locator('[data-error]').filter({hasText:'请求取消'}).waitFor();
  assert.equal(fetches,2);assert.equal(pushes,0,'cancelling fetch pushed');
  await remoteDialog.locator('[data-operations]').click();
  const operations=page.locator('#dlg-git-operations');
  await operations.locator('[data-history]').filter({hasText:'获取 · 成功'}).waitFor();
  assert.match(await operations.locator('[data-history]').innerText(),/<script>literal<\/script>/);
  assert.equal(await operations.locator('script').count(),0);
  await operations.locator('[data-close]').click();

  await remoteDialog.locator('[data-preview]').click();
  await remoteDialog.locator('[data-preview-box]').waitFor({state:'visible'});
  assert.equal(previewRequests,1);assert.equal(pushes,0,'preview unexpectedly pushed');
  assert.match(await remoteDialog.locator('[data-state]').innerText(),/refs\/heads\/main/);
  await remoteDialog.locator('[data-push]').click();
  await remoteDialog.locator('[data-state]').filter({hasText:'已推送'}).waitFor();assert.equal(pushes,1);
  await remoteDialog.locator('[data-terminal]').click();
  const terminalDialog=page.locator('#dlg-git-terminal');
  await terminalDialog.locator('[data-create]:not([disabled])').waitFor();
  assert.equal(await terminalDialog.locator('[data-write]').isChecked(),false);
  await terminalDialog.locator('[data-create]').click();await terminalDialog.locator('pre').waitFor();
  await assertActionIcons(page);assert.equal(terminalCreates,1);assert.match(await terminalDialog.locator('pre').innerText(),/abox-git fetch/);
  assert.doesNotMatch(await terminalDialog.locator('pre').innerText(),/abox-git push/);
  await page.setViewportSize({width:390,height:844});
  assert.equal(await terminalDialog.evaluate(el=>el.scrollWidth<=el.clientWidth),true);
  await page.screenshot({animations:'disabled',path:'output/playwright/git-terminal-mobile.png'});
  await terminalDialog.getByRole('button',{name:'撤销授权'}).click();
  await terminalDialog.locator('[data-list]').filter({hasText:'当前没有终端授权'}).waitFor();
  await terminalDialog.locator('[data-close]').click();
  await remoteDialog.locator('[data-reviews]').click();
  const reviewDialog=page.locator('#dlg-git-reviews');
  await reviewDialog.locator('[type=submit]:not([disabled])').waitFor();
  await reviewDialog.locator('[name=title]').fill('PR <script>literal</script>');
  await reviewDialog.locator('[name=body]').fill('Detailed change description');
  await reviewDialog.locator('[type=submit]').click();
  await reviewDialog.locator('[data-preview]').waitFor({state:'visible'});
  assert.equal(reviewCreates,0,'preview created review');
  assert.match(await reviewDialog.locator('[data-summary]').innerText(),/受保护目标分支/);
  assert.equal(await reviewDialog.locator('script').count(),0);
  await page.setViewportSize({width:390,height:844});
  await page.screenshot({animations:'disabled',path:'output/playwright/git-review-preview-mobile.png'});
  assert.equal(await reviewDialog.evaluate(el=>el.scrollWidth<=el.clientWidth),true);
  await reviewDialog.locator('[data-create]').click();
  await reviewDialog.locator('[data-list]').filter({hasText:'PR <script>literal</script>'}).waitFor();assert.equal(reviewCreates,1);
  await reviewDialog.locator('[data-close]').click();
  await page.setViewportSize({width:390,height:844});
  await page.screenshot({animations:'disabled',path:'output/playwright/git-remote-mobile.png'});
  assert.equal(await remoteDialog.evaluate(el=>el.scrollWidth<=el.clientWidth),true);
  await remoteDialog.locator('[data-close]').click();
  await page.locator('#changes-more').click();
  await page.locator('#btn-changes-clone').click();
  const cloneDialog=page.locator('#dlg-git-clone');
  await cloneDialog.locator('[type=submit]:not([disabled])').waitFor();
  await cloneDialog.locator('[name=url]').fill('https://git.example.com/team/clone.git');
  await cloneDialog.locator('[name=directory]').fill('cloned-project');
  await cloneDialog.locator('[type=submit]').click();await cloneDialog.waitFor({state:'detached'});
  await page.waitForFunction(()=>document.querySelector('#changes-repo').value==='cloned-project');
  failProfile=true;
  if (await page.locator('#changes-more').isVisible()) await page.locator('#changes-more').click();
  await page.locator('#btn-changes-profile').click();await profileDialog.locator('[role=alert]').filter({hasText:'fixture profile failure'}).waitFor();
  assert.equal(await profileDialog.locator('[type=submit]').isDisabled(),true);
  await backToWorkspace();
  role='admin';failProfile=false;
  await page.setViewportSize({width:1440,height:960});await page.reload();
  await page.locator('#btn-changes-remote:not([disabled])').waitFor();await page.locator('#btn-changes-remote').click();
  await page.locator('#dlg-git-remote [data-manage]:not([disabled])').waitFor();await page.locator('#dlg-git-remote [data-manage]').click();
  // Low-frequency connection actions are in the row menu, with delete last.
  await manager.locator('.git-connection-row').filter({hasText:'Company Git'}).locator('.more-btn').click();
  assert.equal(await page.locator('.menu-pop [role=menuitem]').last().innerText(),'删除连接');
  await page.getByRole('menuitem',{name:'使用授权',exact:true}).click();
  const sharesDialog=page.locator('#dlg-git-shares');await sharesDialog.locator('[data-save]:not([disabled])').waitFor();
  await sharesDialog.getByLabel('alice',{exact:true}).check();await sharesDialog.locator('[data-save]').click();await sharesDialog.waitFor({state:'detached'});assert.equal(shareWrites,1);await assertActionIcons(page);
  await manager.locator('[data-oauth-apps]').click();
  const appsDialog=page.locator('#dlg-git-oauth-apps');await appsDialog.locator('[data-add]').click();
  const appEditor=page.locator('#dlg-git-oauth-app-edit');
  await appEditor.locator('[name=label]').fill('Personal GitHub OAuth');await appEditor.locator('[name=client_id]').fill('new-client');await appEditor.locator('[name=client_secret]').fill('synthetic-app-secret');
  await appEditor.locator('[type=submit]').click();await appEditor.waitFor({state:'detached'});
  await appsDialog.locator('strong').filter({hasText:'Personal GitHub OAuth'}).waitFor();assert.equal(oauthWrites,1);
  assert.equal((await appsDialog.innerText()).includes('synthetic-app-secret'),false);
  await page.setViewportSize({width:390,height:844});
  await page.screenshot({animations:'disabled',path:'output/playwright/git-oauth-apps-mobile.png'});
  assert.equal(await appsDialog.evaluate(el=>el.scrollWidth<=el.clientWidth),true);
  await appsDialog.locator('[data-close]').click();await backToWorkspace();
  role='user';connections[0].owner='service-admin';connections[0].managed=false;connections[0].read_only=true;
  await page.setViewportSize({width:1440,height:960});await page.reload();
  await page.locator('#btn-changes-remote:not([disabled])').waitFor();await page.locator('#btn-changes-remote').click();
  await page.locator('#dlg-git-remote [data-manage]:not([disabled])').waitFor();
  assert.equal(await page.locator('#dlg-git-remote [data-preview]').isDisabled(),true,'read-only shared connection allows push preview');
  await page.locator('#dlg-git-remote [data-manage]').click();
  const sharedRow=manager.locator('.git-connection-row').filter({hasText:'共享自 service-admin'});await sharedRow.waitFor();
  for(const label of ['编辑','停用','删除','使用授权'])assert.equal(await sharedRow.getByRole('button',{name:label,exact:true}).count(),0,'shared recipient can manage '+label);
  // A recipient's row menu offers nothing that manages the owner's connection.
  if(await sharedRow.locator('.more-btn').count()){
   await sharedRow.locator('.more-btn').click();
   for(const label of ['停用','删除连接','使用授权'])assert.equal(await page.getByRole('menuitem',{name:label,exact:true}).count(),0,'shared recipient menu offers '+label);
   await page.keyboard.press('Escape');
  }
  await backToWorkspace();
  await page.locator('#btn-git-management').click();
  for(const width of [1440,390]) {
   await page.setViewportSize({width,height:900});
   await page.screenshot({animations:'disabled',path:`output/playwright/git-guide-${width}.png`});
   assert.equal(await page.locator('#git-content').evaluate(el=>el.scrollWidth<=el.clientWidth),true,'guide horizontal overflow');
  }
  await page.setViewportSize({width:1440,height:960});
  await page.locator('#git-nav [data-git-sec=connections]').click();
  await manager.locator('[data-operations]').click();
  assert.equal(await page.locator('dialog[open]').count(),0,'operation history opened a modal');
  await page.locator('#git-nav [data-git-sec=profile]').click();
  assert.equal(await page.locator('#dlg-git-operations').count(),0,'history survives section navigation');
  await page.locator('#git-nav [data-git-sec=connections]').click();
  await manager.locator('[data-add]').click();
  await editor.locator('[name=token]').fill('discard-on-navigation');
  await page.locator('#git-nav [data-git-sec=guide]').click();
  assert.equal(await editor.count(),0,'credential form survives section navigation');
  await assertActionIcons(page);
  assert.deepEqual(errors,[]);
  console.log('Git UI: independent management page, guide, history navigation, reload, inline editors and cleanup, ordinary-user profile, persistence, local commit identity and scope, failure retry, clean tree, cached remote status, mobile layout, HTTPS connection creation/default/binding/fetch, explicit push preview/confirmation, live cancellation/history, ordinary-user OAuth, admin OAuth applications, PR/MR explicit creation, shared ACL and consumer restrictions passed');
 } finally { await page.unroute('**/api/**');server.closeAllConnections();await new Promise(r=>server.close(r)); }
}
if(process.argv[1] && resolve(process.argv[1])===fileURLToPath(import.meta.url)) {
 const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE?pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href:'playwright');
 const browser=await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL?{channel:process.env.AGENTBOX_BROWSER_CHANNEL}:{})});
 try {await smoke(await browser.newPage());} finally {await browser.close();}
}
