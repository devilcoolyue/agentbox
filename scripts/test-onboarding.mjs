#!/usr/bin/env node
// First-use UI against synthetic APIs. No Docker, credentials or model calls.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import {resolve,extname} from 'node:path';
import {fileURLToPath,pathToFileURL} from 'node:url';
import {diagnosticMessages} from '../internal/web/static/js/diagnostic-messages.js';

const root=resolve(fileURLToPath(new URL('../internal/web/static/',import.meta.url)));
const server=createServer(async(req,res)=>{
  try{
    const rel=decodeURIComponent(req.url.split('?')[0]).replace(/^\/_v\/[^/]+/,'');
    const path=resolve(root,'.'+(rel==='/'?'/index.html':rel));
    if(!path.startsWith(root+'/')){res.writeHead(403).end();return;}
    res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[extname(path)]||'application/octet-stream');
    res.end(await readFile(path));
  }catch{res.writeHead(404).end();}
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));
const base=`http://127.0.0.1:${server.address().port}`;
const {chromium}=await import(process.env.AGENTBOX_PLAYWRIGHT_MODULE?pathToFileURL(process.env.AGENTBOX_PLAYWRIGHT_MODULE).href:'playwright');
const browser=await chromium.launch({headless:true,...(process.env.AGENTBOX_BROWSER_CHANNEL?{channel:process.env.AGENTBOX_BROWSER_CHANNEL}:{})});
try{
 for(const locale of ['zh-CN','zh-TW','en'])for(const role of ['admin','user']){
  const context=await browser.newContext({locale:'en-US',viewport:{width:1280,height:900}});
  const page=await context.newPage();page.setDefaultTimeout(12000);
  await page.addInitScript(locale=>{if(window===window.top&&/^https?:$/.test(location.protocol))localStorage.setItem('agentbox.language',locale);},locale);
  const errors=[];page.on('pageerror',e=>{errors.push(e.stack||e.message);console.error('Onboarding page error:',e.stack||e.message);});
  let accounts=[],spaces=[],created=0,modelTurns=0,accountFailure=false,setupFailure=false,holdAccounts=false;
  let loseCreateAck=false,loseImportAck=false,uncertainImport=false,failUpload=false,failGit=false,uploadCalls=0,gitCalls=0;
  const creations=new Map();
  const pendingAccountReads=[];const releaseAccounts=()=>pendingAccountReads.splice(0).forEach(release=>release());
  const models={claude:[{id:'claude-fixture',label:'Claude fixture'}],codex:[{id:'codex-first',label:'First'},{id:'codex-second',label:'Second'}]};
  const settings={listen:'127.0.0.1:8180',agent_image:'fixture',permission_mode:'default',max_upload_mb:20,idle_timeout_min:30,timezone:'UTC',
    models,default_models:{claude:'claude-fixture',codex:'codex-first'},container:{memory_mb:512,cpus:1,pids_limit:128,network:'none'},
    resources:{max_running:0,max_running_per_user:0,min_free_bytes:0},terminal_tips:{tips:[],interval_sec:0,animation:'none'},tunnel:{enabled:false},proxy_bridge:{bind:'127.0.0.1:1081'},pricing:{}};
  let sentTask;
  await page.routeWebSocket('**/api/sessions/*/chat?*',ws=>{ws.send(JSON.stringify({type:'status',state:'idle'}));ws.onMessage(raw=>{
    const message=JSON.parse(String(raw));if(message.type!=='user_message')return;
    modelTurns++;sentTask=message;
    ws.send(JSON.stringify(message));
    ws.send(JSON.stringify({type:'agent_event',event:{type:'item.completed',item:{type:'agent_message',text:'Synthetic task response'}}}));
    ws.send(JSON.stringify({type:'status',state:'idle'}));
  });});
  await page.routeWebSocket('**/api/diagnostics/ws?*',ws=>ws.send(JSON.stringify({type:'diagnostic',id:'websocket',state:'passed',code:'websocket_ok',operation_id:new URL(ws.url()).searchParams.get('connection_id')})));
  await page.route('**/api/**',async route=>{
    const path=new URL(route.request().url()).pathname,method=route.request().method();let body={};
    if(path==='/api/login')body={token:'synthetic-'+role};
    else if(path==='/api/me')body={user:'fixture',role,timezone:'UTC',models,quota:{metered:false}};
    else if(path==='/api/onboarding'){
      if(setupFailure){await route.fulfill({status:503,json:{error:'synthetic setup read failed'}});return;}
      body={version:1,can_configure:role==='admin',can_create:accounts.length>0,has_workspaces:spaces.length>0,
        accounts:accounts.map(a=>({id:a.id,type:a.type,label:a.label,credentials_present:a.cred_status==='ok'})),
        default_models:role==='admin'?settings.default_models:Object.fromEntries(Object.entries(settings.default_models).filter(([agent])=>accounts.some(a=>a.type===agent))),container_resources:{cpus:settings.container.cpus,memory_mb:settings.container.memory_mb,pids_limit:settings.container.pids_limit}};
    }else if(path==='/api/session-creations')body={version:1,actor_key:(role==='admin'?'a':'b').repeat(64),creations:[...creations.values()].filter(c=>c.state==='ready')};
    else if(path.startsWith('/api/session-creations/')){
      const id=path.split('/')[3];let receipt=creations.get(id);
      if(method==='PUT'){
        const data=route.request().postDataJSON();
        if(!receipt){
          assert.ok(accounts.some(a=>a.id===data.account_id&&a.type===data.agent));created++;
          const session={...data,id:'created-'+created,default_model:settings.default_models[data.agent],status:'stopped',account_label:'Fixture account',updated_at:new Date().toISOString()};spaces.push(session);
          receipt={request_id:id,request:data,session,git_connection_id:data.git_connection_id,state:'ready',workspace_exists:true,busy:false,imports:[],created_at:new Date().toISOString(),container_resources:{cpus:settings.container.cpus,memory_mb:settings.container.memory_mb,pids_limit:settings.container.pids_limit}};creations.set(id,receipt);
        }
        if(loseCreateAck){loseCreateAck=false;await route.abort();return;}
        body=receipt.session;
      }else if(!receipt){await route.fulfill({status:404,json:{error:'not found'}});return;}
      else if(method==='POST' && (path.endsWith('/upload')||path.endsWith('/git'))){
        const upload=path.endsWith('/upload'),query=new URL(route.request().url()).searchParams;
        const input=upload?{attempt_id:query.get('attempt_id'),directory:query.get('directory')}:route.request().postDataJSON();
        if(upload)uploadCalls++;else gitCalls++;
        const prior=receipt.imports.find(i=>i.attempt_id===input.attempt_id);
        if(prior)body=prior;
        else{const failed=upload?failUpload:failGit;failUpload=false;failGit=false;
          body={attempt_id:input.attempt_id,kind:upload?'upload':'git',directory:input.directory,state:uncertainImport?'uncertain':failed?'failed':'succeeded',result:{directory:input.directory,files:1},created_at:new Date().toISOString()};receipt.imports.unshift(body);uncertainImport=false;}
        if(body.state==='succeeded'&&loseImportAck){loseImportAck=false;await route.abort();return;}
      }
      else if(method==='POST'){const action=route.request().postDataJSON().action;if(action==='review')receipt.imports[0].state='reviewed';else receipt.state=action==='finish'?'complete':'abandoned';body={ok:true};}
      else body=receipt;
    }else if(path==='/api/accounts'){
      if(method==='POST'){const a={...route.request().postDataJSON(),cred_status:'missing',sessions:0};accounts.push(a);body=a;}
      else{
        if(holdAccounts){const prior=[...accounts];await new Promise(r=>pendingAccountReads.push(r));await route.fulfill({json:prior}).catch(()=>{});return;}
        if(accountFailure){await route.fulfill({status:503,json:{error:'synthetic account read failed'}});return;}
        body=accounts;
      }
    }else if(path.endsWith('/apikey')){accounts.find(a=>a.id===path.split('/')[3]).cred_status='ok';body={ok:true};}
    else if(path==='/api/sessions'){
      if(method==='POST'){
        const data=route.request().postDataJSON();created++;
        assert.ok(accounts.some(a=>a.id===data.account_id&&a.type===data.agent),'submitted unavailable account');
        body={...data,id:'created-'+created,default_model:settings.default_models[data.agent],status:'stopped',account_label:'Fixture account',updated_at:new Date().toISOString()};spaces.push(body);
      }else body=spaces;
    }else if(path==='/api/settings'){
      if(method==='PUT'){const patch=route.request().postDataJSON();if(patch.default_models)patch.default_models={...settings.default_models,...patch.default_models};Object.assign(settings,patch);}body=settings;
    }else if(path==='/api/diagnostics'){
      body={version:1,scope:'instance',checked_at:Date.now(),operation_id:'0123456789abcdef0123456789abcdef',checks:[
        ['configuration','passed','config_ok'],['docker','passed','docker_ok'],['agent_image','passed','image_ok'],['data_permissions','passed','write_ok'],['container_ownership','passed','ownership_ok'],['data_disk','passed','disk_ok'],['websocket','not_checked','websocket_not_checked'],['model','not_checked','model_not_checked']
      ].map(([id,state,code])=>({id,state,code,message:diagnosticMessages[code][0],hint:diagnosticMessages[code][1]}))};
    }else if(path==='/api/proxies')body={proxies:[],bridge_up:false};
    else if(path==='/api/git/connections')body=[{id:'git-fixture',label:'Fixture Git',enabled:true,provider:'gitlab',auth_type:'pat'}];
    else if(path==='/api/users')body=[];
    else if(path.endsWith('/files')){
      const receipt=[...creations.values()].find(c=>c.session.id===path.split('/')[3]);
      body=receipt?.imports.some(i=>i.state==='succeeded')?[{name:'project',is_dir:true,size:0,mode:'drwxr-xr-x',mtime:new Date().toISOString()}]:[];
    }
    else if(path==='/api/me/git/default')body={connection_id:''};
    else if(path.endsWith('/models'))body={models:models.codex,default_reasoning:{support:'unknown',control:'effort'},discovery:'available'};
    else if(path.endsWith('/history'))body={entries:[],thread:null,costs:{}};
    else if(path.endsWith('/chat/threads'))body={threads:[],active:null};
    else if(path==='/api/usage/events')body={total:{cost_micro_usd:0},rows:[]};
    else if(path==='/api/tunnel/status')body={enabled:false};
    else if(path.startsWith('/api/updates'))body={current_version:'dev',revision:'0123456789abcdef',built_at:'',latest_version:'',available:false,comparable:false,release_url:'',notes:'',error:'',checked_at:Date.now(),attempted_at:0,supported:false};
    else if(path==='/api/pricing')body={rows:[],revision:0,candidate:null};
    await route.fulfill({json:body});
  });
  const ready=()=>page.waitForFunction(()=>!document.querySelector('#setup-refresh').disabled);
  const replayGuide=async()=>{
    await page.locator(await page.locator('#btn-wb-more').isVisible()?'#btn-wb-more':'#btn-kebab').click();
    const label=await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/i18n.js')).t('使用指引'));
    await page.getByRole('menuitem',{name:label,exact:true}).click();
    await page.waitForFunction(()=>document.querySelector('#workspace-guide').matches(':popover-open')&&!document.querySelector('#guide-example').disabled);
  };

  const home=async()=>{await page.locator('#btn-home').click();await ready();};
  try{
    await page.goto(base);await page.locator('#login-user').fill('fixture');await page.locator('#login-pass').fill('synthetic-password');await page.locator('#login-btn').click();
    await page.locator('#app').waitFor({state:'visible'});await ready();
    assert.equal(await page.locator('#home-setup').getAttribute('open')!==null,true,'first user guide should open');
    assert.equal(await page.locator('#empty-new').isDisabled(),true);
    assert.equal(await page.locator('#setup-steps > li').count(),role==='admin'?4:3);
    await page.locator('#btn-new').click();
    await page.waitForFunction(()=>!document.querySelector('#new-refresh').disabled);
    assert.equal(await page.locator('#new-fields').isVisible(),false);
    assert.equal(await page.locator('#new-ok').isDisabled(),true);
    assert.equal(await page.locator('#new-configure').isVisible(),role==='admin');
    await page.locator('#new-form').evaluate(form=>form.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true})));
    assert.equal(created,0,'no-account state submitted a create request');
    await page.locator('#new-cancel').click();
    if(role==='admin'){
      await page.locator('#setup-steps > li').nth(0).getByRole('button').click();
      await page.waitForFunction(()=>document.querySelector('.diagnostics-dialog button[data-icon="download"]')?.disabled===false);
      await page.locator('.diagnostics-dialog button[data-icon="close"]').click();
      assert.ok(!(await page.locator('#setup-steps').innerText()).includes('model available'));
      await page.locator('#setup-steps > li').nth(1).getByRole('button').click();await page.locator('#sec-accounts').waitFor({state:'visible'});
      await page.locator('#btn-acct-add').click();await page.locator('#acct-form label.agent-codex').click();
      await page.locator('#acct-id').fill('codex-fixture');await page.locator('#acct-label').fill('Fixture account');
      await page.locator('#auth-mode-key').click();await page.locator('#auth-apikey').fill('synthetic-key');await page.locator('#auth-savekey').click();
      await page.locator('#dlg-auth').waitFor({state:'hidden'});await home();
      await page.locator('#setup-steps > li').nth(2).getByRole('button').click();await page.locator('#sec-models').waitFor({state:'visible'});
      await page.locator('#mdl-codex-default + .select-trigger').click();
      await page.getByRole('option',{name:'Second · codex-second',exact:true}).click();
      await page.waitForFunction(()=>!document.querySelector('#mdl-codex-default').disabled);await home();
      await page.locator('#setup-steps').filter({hasText:'codex-second'}).waitFor();
    }else{
      assert.equal(await page.locator('#setup-steps button[data-icon="settings"]').count(),0);
      accounts=[{id:'codex-fixture',type:'codex',label:'Fixture account',cred_status:'ok',sessions:0}];
      await page.locator('#setup-refresh').click();await ready();
      await page.locator('#setup-steps').filter({hasText:'codex-first'}).waitFor();
      assert.ok(!(await page.locator('#setup-steps').innerText()).includes('claude-fixture'));
    }
    assert.equal(await page.locator('#empty-new').isDisabled(),false);
    // Account read failures cannot use S.accounts as an authorization fallback.
    accountFailure=true;await page.locator('#btn-new').click();await page.waitForFunction(()=>!document.querySelector('#new-refresh').disabled);
    assert.equal(await page.locator('#new-ok').isDisabled(),true);assert.equal(await page.locator('#new-fields').isVisible(),false);
    accountFailure=false;await page.locator('#new-refresh').click();await page.locator('#new-name').fill('First project');
    assert.equal(await page.locator('#new-form input[value="codex"]').isChecked(),true);
    assert.equal(await page.locator('#new-form input[value="claude"]').isDisabled(),true);
    // A revocation while the form is open must not silently switch/submit.
    const original=accounts;accounts=[];await page.locator('#new-ok').click();
    await page.locator('#new-error').waitFor({state:'visible'});assert.equal(created,0);assert.equal(await page.locator('#new-ok').isDisabled(),true);
    accounts=original;await page.locator('#new-refresh').click();await page.locator('#new-name').fill('First project');
    await page.locator('#new-form').evaluate(form=>{form.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true}));form.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true}));});
    await page.waitForFunction(()=>location.hash.includes('/sessions/created-1/chat'));
    assert.equal(created,1,'rapid submission created duplicates');assert.equal(modelTurns,0,'setup invoked a model');
    assert.equal(spaces[0].default_model,role==='admin'?'codex-second':'codex-first');
    await page.waitForFunction(()=>document.querySelector('#workspace-guide').matches(':popover-open')&&!document.querySelector('#guide-example').disabled);
    assert.ok((await page.locator('#guide-config').innerText()).includes(spaces[0].default_model));
    await page.locator('#guide-next').click();assert.equal(await page.locator('#workspace-guide').getAttribute('data-step'),'1');
    await page.locator('#guide-back').click();assert.equal(await page.locator('#workspace-guide').getAttribute('data-step'),'0');
    await page.keyboard.press('Escape');assert.equal(await page.locator('#workspace-guide').isVisible(),false);
    await page.reload();await page.locator('#app').waitFor({state:'visible'});
    assert.equal(await page.locator('#workspace-guide').isVisible(),false,'tour reopened after skipping and reload');
    await page.emulateMedia({reducedMotion:'reduce'});await replayGuide();
    assert.equal(await page.locator('#workspace-guide').evaluate(el=>getComputedStyle(el).animationName),'none');
    await page.locator('#guide-next').click();await page.locator('#guide-next').click();await page.locator('#guide-next').click();
    await page.emulateMedia({reducedMotion:'no-preference'});
    assert.equal(await page.locator('#workspace-guide').isVisible(),false,'finished tour still occupies the workspace');
    if(locale==='en'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/guide-dismissed-${role}.png`,animations:'disabled'});}
    await replayGuide();
    await page.locator('#guide-example').click();
    assert.ok((await page.locator('#chat-input').inputValue()).includes('/workspace'));
    assert.equal(modelTurns,0,'example must not auto-send');
    await page.locator('#chat-input').fill('My edited first task');
    await replayGuide();await page.locator('#guide-example').click();
    assert.equal(await page.locator('#chat-input').inputValue(),'My edited first task','example overwrote user input');
    await page.locator('#chat-input').fill('');
    // Guide navigation resets a previous shared-directory selection.
    await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/state.js')).S.fileScope='shared';});
    await replayGuide();await page.locator('#guide-next').click();await page.locator('#guide-files').click();await page.waitForFunction(()=>location.hash.endsWith('/files'));
    assert.equal(await page.evaluate(async()=>(await import('/_v/{{BUILD}}/js/state.js')).S.fileScope),'workspace');
    await replayGuide();await page.locator('#guide-next').click();await page.locator('#guide-next').click();
    const archiveDownload=page.waitForEvent('download');
    await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/state.js')).S.fileScope='shared';});
    await page.locator('#guide-download').click();
    const archiveURL=new URL((await archiveDownload).url());assert.equal(archiveURL.searchParams.get('scope'),null,'guide downloaded shared files');assert.ok(archiveURL.pathname.endsWith('/sessions/created-1/archive'));
    await page.locator('#guide-changes').click();await page.waitForFunction(()=>location.hash.endsWith('/changes'));
    const choose=async(id,value)=>{
      const index=await page.locator('#'+id).evaluate((el,value)=>[...el.options].findIndex(o=>o.value===value),value);
      await page.locator('#'+id+' + .select-trigger').click();await page.getByRole('option').nth(index).click();
    };
    // A lost creation reply followed by upload failure still reuses one space.
    await page.locator('#btn-new').click();await page.locator('#new-name').fill('Uploaded project');
    assert.ok((await page.locator('#new-config-summary').innerText()).includes('512'));
    await choose('new-source-kind','upload');await choose('new-upload-mode','files');
    await page.locator('#new-project-file').setInputFiles({name:'hello.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic project')});
    if(locale==='en'&&role==='admin'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:'output/playwright/creation-upload-desktop.png',animations:'disabled'});}
    loseCreateAck=true;failUpload=true;
    await page.locator('#new-ok').click();await page.locator('#new-error').waitFor({state:'visible'});
    assert.equal(created,2);await page.waitForFunction(()=>document.querySelector('#new-core').disabled);assert.equal(await page.locator('#new-name').isDisabled(),true);
    await page.locator('#new-ok').click();await page.locator('#new-error').waitFor({state:'visible'});
    assert.equal(created,2);assert.equal(uploadCalls,1);
    await page.locator('#new-ok').click();await page.waitForFunction(()=>location.hash.includes('/sessions/created-2/files'));
    assert.equal(created,2);assert.equal(uploadCalls,2);
    // Git authentication retry and a lost successful import reply survive reload.
    await page.locator('#btn-new').click();await page.locator('#new-name').fill('Git project');
    await choose('new-source-kind','git');await page.locator('#new-git-url').fill('https://git.example.invalid/team/repo.git');
    failGit=true;
    await page.locator('#new-ok').click();await page.locator('#new-error').waitFor({state:'visible'});
    assert.equal(created,3);assert.equal(gitCalls,1);
    loseImportAck=true;await page.locator('#new-ok').click();await page.locator('#new-error').waitFor({state:'visible'});
    assert.equal(created,3);assert.equal(gitCalls,2);
    await page.reload();await page.locator('#app').waitFor({state:'visible'});await page.locator('#btn-new').click();
    await page.waitForFunction(()=>!document.querySelector('#new-ok').disabled);
    assert.equal(await page.locator('#new-git-url').inputValue(),'','Git URL must not be kept in browser storage');
    await page.locator('#new-ok').click();await page.waitForFunction(()=>location.hash.includes('/sessions/created-3/changes'));
    assert.equal(created,3);assert.equal(gitCalls,2,'successful import was executed again after reload');
    // Unknown publication stays blocked until the user inspects and reviews it.
    await page.locator('#btn-new').click();await page.locator('#new-name').fill('Review project');
    await choose('new-source-kind','upload');await choose('new-upload-mode','files');
    await page.locator('#new-project-file').setInputFiles({name:'review.txt',mimeType:'text/plain',buffer:Buffer.from('synthetic uncertain')});
    uncertainImport=true;await page.locator('#new-ok').click();
    await page.waitForFunction(()=>!document.querySelector('#new-query').disabled&&document.querySelector('#new-ok').disabled);
    if(locale==='en'&&role==='admin'){await page.locator('#new-progress').scrollIntoViewIfNeeded();await page.screenshot({path:'output/playwright/creation-review-desktop.png',animations:'disabled'});}
    const beforeReview=uploadCalls;
    await page.locator('#new-form').evaluate(form=>form.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true})));
    assert.equal(uploadCalls,beforeReview,'unknown outcome was retried without review');
    await page.locator('#new-inspect').click();await page.waitForFunction(()=>location.hash.includes('/sessions/created-4/files'));
    await page.locator('#btn-new').click();await page.waitForFunction(()=>!document.querySelector('#new-review').disabled);
    await page.locator('#new-review').click();await page.waitForFunction(()=>!document.querySelector('#new-ok').disabled);
    await choose('new-source-kind','empty');await page.locator('#new-ok').click();
    await page.waitForFunction(()=>location.hash.includes('/sessions/created-4/chat'));
    assert.equal(created,4);assert.equal(uploadCalls,beforeReview,'review rewrote project data');
    // Configuration drift allocates at most one space and pauses before importing.
    await page.locator('#btn-new').click();await page.locator('#new-name').fill('Changed configuration');
    await choose('new-source-kind','upload');await choose('new-upload-mode','files');
    await page.locator('#new-project-file').setInputFiles({name:'config.txt',mimeType:'text/plain',buffer:Buffer.from('config drift')});
    const oldModel=settings.default_models.codex;
    settings.default_models.codex=oldModel==='codex-first'?'codex-second':'codex-first';settings.container.memory_mb=1024;
    await page.locator('#new-ok').click();await page.locator('#new-error').waitFor({state:'visible'});
    assert.equal(created,5);assert.equal(uploadCalls,beforeReview,'config drift started an import before review');
    assert.ok((await page.locator('#new-config-summary').innerText()).includes(settings.default_models.codex));
    assert.ok((await page.locator('#new-config-summary').innerText()).includes('1024'));
    await page.locator('#new-ok').click();await page.waitForFunction(()=>location.hash.includes('/sessions/created-5/files'));
    assert.equal(created,5);assert.equal(uploadCalls,beforeReview+1);
    assert.equal(await page.locator('#workspace-guide').isVisible(),false,'tour repeated for a later workspace');await replayGuide();
    await page.waitForFunction(()=>document.querySelector('#workspace-guide').matches(':popover-open')&&!document.querySelector('#guide-example').disabled);
    if(locale==='en'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/first-task-${role}-desktop.png`,animations:'disabled'});}
    await page.setViewportSize({width:390,height:844});
    if(await page.locator('#sidebar.open').count())await page.locator('#btn-sidebar-close').click();
    await page.waitForFunction(()=>{const r=document.querySelector('#workspace-guide').getBoundingClientRect();return r.left>=0&&r.right<=innerWidth&&r.top>=0&&r.bottom<=innerHeight;});
    assert.equal(await page.locator('#workspace-guide').evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'task guide overflows on narrow screen');
    if(locale==='en')await page.screenshot({path:`output/playwright/first-task-${role}-mobile.png`,animations:'disabled'});
    await page.locator('#guide-example').focus();await page.keyboard.press('Enter');
    assert.ok((await page.locator('#chat-input').inputValue()).includes('/workspace/project'));
    assert.equal(modelTurns,0,'import guide sent task automatically');
    await page.locator('#chat-input').fill('My explicit edited task');await page.locator('#chat-send').click();
    await page.locator('#chat-log').filter({hasText:'Synthetic task response'}).waitFor();
    assert.equal(modelTurns,1);assert.equal(sentTask.text,'My explicit edited task');
    assert.equal(sentTask.model,spaces[4].default_model,'task used a different model from the saved configuration');
    await page.setViewportSize({width:1280,height:900});
    // A delayed response from a closed dialog must not populate the next one.
    holdAccounts=true;const pending=page.waitForRequest(r=>new URL(r.url()).pathname==='/api/accounts');
    await page.locator('#btn-new').click();await pending;await page.locator('#new-cancel').click();
    holdAccounts=false;accounts=[];
    await page.locator('#btn-new').click();await page.waitForFunction(()=>!document.querySelector('#new-refresh').disabled);
    releaseAccounts();
    assert.equal(await page.locator('#new-ok').isDisabled(),true);
    assert.equal(await page.locator('#new-fields').isVisible(),false);
    accounts=original;await page.locator('#new-refresh').click();await page.locator('#new-name').fill('Preserved draft');
    await page.locator('#new-cancel').click();assert.equal(created,5);

    await home();
    if(!(await page.locator('#home-setup').evaluate(el=>el.open)))await page.locator('#home-setup summary').click();
    setupFailure=true;await page.locator('#setup-refresh').click();await ready();
    assert.equal(await page.locator('#empty-new').isDisabled(),true,'failed setup refresh retained a ready CTA');
    setupFailure=false;await page.locator('#setup-refresh').click();await ready();
    await page.setViewportSize({width:390,height:844});
    if(await page.locator('#sidebar.open').count())await page.locator('#btn-sidebar-close').click();
    await page.locator('#home-setup').evaluate(el=>el.open=true);
    assert.equal(await page.locator('#empty').evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'guide overflows on narrow screen');
    if(await page.locator('#toast').count())await page.locator('#toast').waitFor({state:'hidden'});
    if(locale==='en'){await mkdir('output/playwright',{recursive:true});await page.screenshot({path:`output/playwright/onboarding-${role}-mobile.png`,animations:'disabled'});}
    await page.locator('#setup-steps > li').last().getByRole('button').click();
    await page.locator('#new-name').fill('Mobile private draft');
    await choose('new-source-kind','upload');
    await page.locator('#new-ok').scrollIntoViewIfNeeded();
    assert.equal(await page.locator('#new-ok').isVisible(),true,'creation action is unreachable on narrow screen');
    assert.equal(await page.locator('#dlg-new').evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'create form overflows on narrow screen');
    if(locale==='en')await page.screenshot({path:`output/playwright/creation-upload-${role}-mobile.png`,animations:'disabled'});
    await page.keyboard.press('Escape');await page.locator('#dlg-new').waitFor({state:'hidden'});
    await page.evaluate(async()=>{(await import('/_v/{{BUILD}}/js/state.js')).emit('unauthorized');});
    await page.locator('#login').waitFor({state:'visible'});
    assert.equal(await page.locator('#new-name').inputValue(),'','logout retained another user draft');
    assert.equal(await page.locator('#new-account option').count(),0,'logout retained account choices');
    assert.equal(await page.locator('#new-form input[value="claude"]').isChecked(),true,'logout retained agent choice');
    assert.equal(await page.locator('#setup-steps').innerText(),'','logout retained preparation information');
    assert.equal(await page.locator('#guide-config').innerText(),'','logout retained workspace guidance');
    assert.equal(await page.locator('#chat-input').inputValue(),'','logout retained an example draft');
    assert.equal(modelTurns,1,'navigation invoked another model turn');
    assert.deepEqual(errors,[]);
    console.log(`Onboarding ${role}/${locale}: scoped guide, configuration links, fresh accounts, no-account/read-failure/revocation gates and single submission passed`);
  }finally{releaseAccounts();await context.close();}
 }
}finally{await browser.close();server.closeAllConnections();await new Promise(r=>server.close(r));}
