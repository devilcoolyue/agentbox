// Embedded candidate check. Run ONLY in a disposable network=none container.
// No host mounts, real credentials, or external providers. Stdout is a bounded
// machine report; raw CLI stderr is intentionally not returned.
const fs = require('node:fs');
const http = require('node:http');
const {spawn} = require('node:child_process');
const readline = require('node:readline');
const assert = require('node:assert/strict');
const root = fs.mkdtempSync('/tmp/agentbox-probe-');
const cwd = root + '/workspace'; fs.mkdirSync(cwd);
const marker = 'AGENTBOX_SYNTHETIC_OK';
const records = [], checks = [];
let mode = '', requests = [], held = false, mcpCalls = 0;
const mcpFile = root + '/mcp-calls';
const mcpScript = `const fs=require('fs');require('readline').createInterface({input:process.stdin}).on('line',line=>{const r=JSON.parse(line);if(r.id===undefined)return;let result={};if(r.method==='initialize')result={protocolVersion:r.params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'synthetic',version:'1'}};if(r.method==='tools/list')result={tools:[{name:'echo',description:'Synthetic echo',inputSchema:{type:'object',properties:{}}}]};if(r.method==='tools/call'){fs.appendFileSync(${JSON.stringify(mcpFile)},'call\\n');result={content:[{type:'text',text:'${marker}'}]};}console.log(JSON.stringify({jsonrpc:'2.0',id:r.id,result}));});`;
const mcpPath = root + '/mcp.cjs'; fs.writeFileSync(mcpPath, mcpScript);
const server = http.createServer((req, res) => {
 let raw = '';
 req.on('data', chunk => {raw += chunk; if(raw.length > 2*1024*1024) req.destroy();});
 req.on('end', () => {
  let body; try {body = JSON.parse(raw);} catch {res.writeHead(400).end();return;}
  if(!body.messages && !body.input) {res.writeHead(404).end();return;}
  requests.push(body);
  if(mode.endsWith('interrupt')) {held = true; return;}
  res.writeHead(200, {'content-type':'text/event-stream','cache-control':'no-cache'});
  const event = (name, value) => res.write('event: '+name+'\ndata: '+JSON.stringify(value)+'\n\n');
  if(body.messages) {
   const tool = (body.tools||[]).find(t => t.name.includes('mcp__probe__echo'));
   const called = JSON.stringify(body.messages).includes('tool_result');
   const useTool = mode === 'claude_turn' && !called;
   if(useTool && !tool) {res.end(); return;}
   const id = 'msg_probe_'+records.length+'_'+requests.length;
   event('message_start',{type:'message_start',message:{id,type:'message',role:'assistant',model:body.model,content:[],stop_reason:null,stop_sequence:null,usage:{input_tokens:10,output_tokens:0,cache_read_input_tokens:4,cache_creation_input_tokens:0}}});
   event('content_block_start',{type:'content_block_start',index:0,content_block:useTool?{type:'tool_use',id:'tool_probe',name:tool.name,input:{}}:{type:'text',text:''}});
   event('content_block_delta',{type:'content_block_delta',index:0,delta:useTool?{type:'input_json_delta',partial_json:'{}'}:{type:'text_delta',text:marker}});
   event('content_block_stop',{type:'content_block_stop',index:0});
   event('message_delta',{type:'message_delta',delta:{stop_reason:useTool?'tool_use':'end_turn',stop_sequence:null},usage:{output_tokens:2}});
   event('message_stop',{type:'message_stop'});
  } else {
   const item={id:'msg_probe',type:'message',role:'assistant',status:'completed',content:[{type:'output_text',text:marker,annotations:[]}]};
   event('response.created',{type:'response.created',response:{id:'resp_probe',object:'response',status:'in_progress',output:[]}});
   event('response.output_item.added',{type:'response.output_item.added',output_index:0,item:{...item,status:'in_progress',content:[]}});
   event('response.content_part.added',{type:'response.content_part.added',item_id:item.id,output_index:0,content_index:0,part:{type:'output_text',text:'',annotations:[]}});
   event('response.output_text.delta',{type:'response.output_text.delta',item_id:item.id,output_index:0,content_index:0,delta:marker});
   event('response.output_text.done',{type:'response.output_text.done',item_id:item.id,output_index:0,content_index:0,text:marker});
   event('response.content_part.done',{type:'response.content_part.done',item_id:item.id,output_index:0,content_index:0,part:item.content[0]});
   event('response.output_item.done',{type:'response.output_item.done',output_index:0,item});
   event('response.completed',{type:'response.completed',response:{id:'resp_probe',object:'response',status:'completed',output:[item],usage:{input_tokens:10,input_tokens_details:{cached_tokens:4},output_tokens:2,output_tokens_details:{reasoning_tokens:1},total_tokens:12}}});
  }
  res.end();
 });
});
const waitUntil = async (predicate, ms=20000) => {
 const end=Date.now()+ms;
 while(!predicate()) {assert.ok(Date.now()<end,'deadline'); await new Promise(r=>setTimeout(r,20));}
};
const homes = {};
let base;
function environment(kind) {
 if(!homes[kind]) {
  const home=root+'/'+kind;fs.mkdirSync(home);fs.mkdirSync(home+'/.claude');fs.mkdirSync(home+'/.codex');
  fs.writeFileSync(home+'/.claude.json',JSON.stringify({hasCompletedOnboarding:true,bypassPermissionsModeAccepted:true,projects:{[cwd]:{hasTrustDialogAccepted:true,hasCompletedProjectOnboarding:true}},mcpServers:{probe:{type:'stdio',command:'node',args:[mcpPath]}}}));
  fs.writeFileSync(home+'/.codex/config.toml','model_provider="fixture"\nmodel="gpt-5.5"\n[model_providers.fixture]\nname="Synthetic"\nbase_url="'+base+'/v1"\nwire_api="responses"\nenv_key="OPENAI_API_KEY"\n');
  homes[kind]=home;
 }
 return {PATH:'/usr/local/bin:/usr/bin:/bin',HOME:homes[kind],CODEX_HOME:homes[kind]+'/.codex',LANG:'C.UTF-8',TERM:'dumb',ANTHROPIC_API_KEY:'synthetic-only',ANTHROPIC_BASE_URL:base,OPENAI_API_KEY:'synthetic-only',CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:'1',DISABLE_AUTOUPDATER:'1',ENABLE_TOOL_SEARCH:'false'};
}
function launch(kind,args) {
 const child=spawn(args[0],args.slice(1),{cwd,env:environment(kind)});
 const lines=[];let size=0,closed=false,code,signal;
 const done=new Promise(resolve=>child.on('close',(n,s)=>{closed=true;code=n;signal=s;resolve();}));
 child.on('error',()=>{closed=true;});child.stderr.resume();
 readline.createInterface({input:child.stdout}).on('line',line=>{
  size+=line.length;if(size>2*1024*1024){child.kill('SIGKILL');return;}
  try {lines.push(JSON.parse(line));}catch { /* CLI informational lines are not evidence. */ }
 });
 const timer=setTimeout(()=>child.kill('SIGKILL'),40000);
 return {child,lines,done,get closed(){return closed;},get code(){return code;},get signal(){return signal;},async stop(){child.stdin.end();if(!closed){await Promise.race([done,new Promise(r=>setTimeout(r,1000))]);}if(!closed)child.kill('SIGKILL');await done;clearTimeout(timer);}};
}
async function cli(name,kind,command,resume='') {
 mode=name;requests=[];held=false;
 const proc=launch(kind,command);
 try {
  proc.child.stdin.end('Use the synthetic echo tool if available, then return the marker.');
  if(name.endsWith('interrupt')) {
   await waitUntil(()=>held||proc.closed);assert.ok(held,'provider not reached');proc.child.kill('SIGINT');
  }
  await proc.done;
  const terminal=proc.lines.findLast(e=>e.type==='result'||e.type==='turn.completed');
  if(!name.endsWith('interrupt')) {
   assert.equal(proc.code,0,'CLI failed');assert.ok(terminal,'missing terminal');
   assert.ok(JSON.stringify(proc.lines).includes(marker),'missing response');
  } else assert.ok(proc.code===130||proc.signal==='SIGINT'||terminal?.is_error===true,'interruption lacked SIGINT/terminal evidence');
  if(resume) assert.ok(requests.some(r=>JSON.stringify(r.messages||r.input).includes(marker)),'resume lost previous response');
  records.push({name,agent:kind,exit_code:proc.code,signal:proc.signal,lines:proc.lines.filter(e=>['result','stream_event','assistant','turn.completed'].includes(e.type)),requests:requests.length});
  checks.push(name);
  return terminal?.session_id||'';
 } finally {await proc.stop();server.closeAllConnections();}
}
async function protocol(name,resume='') {
 mode=name;requests=[];held=false;
 const proc=launch('codex',['codex','app-server']);
 const send=(id,method,params)=>proc.child.stdin.write(JSON.stringify({id,method,params})+'\n');
 const call=async(id,method,params)=>{send(id,method,params);await waitUntil(()=>proc.lines.some(m=>m.id===id)||proc.closed);const reply=proc.lines.find(m=>m.id===id);assert.ok(reply&&!reply.error,'RPC failed: '+method);return reply.result;};
 try {
  await call(1,'initialize',{clientInfo:{name:'agentbox-probe',version:'1'}});
  proc.child.stdin.write('{"method":"initialized"}\n');
  const thread=resume?await call(2,'thread/resume',{threadId:resume,model:'gpt-5.5'}):await call(3,'thread/start',{cwd,model:'gpt-5.5',sandbox:'danger-full-access',approvalPolicy:'never'});
  assert.ok(thread.thread?.id,'thread missing');if(resume)assert.equal(thread.thread.id,resume,'resume changed thread');
  const result=await call(4,'turn/start',{threadId:thread.thread.id,cwd,model:'gpt-5.5',input:[{type:'text',text:'Return the synthetic marker.'}],sandboxPolicy:{type:'dangerFullAccess'},approvalPolicy:'never'});
  assert.ok(result.turn?.id,'turn missing');
  if(name==='codex_interrupt') {await waitUntil(()=>held||proc.closed);assert.ok(held);await call(9,'turn/interrupt',{threadId:thread.thread.id,turnId:result.turn.id});}
  await waitUntil(()=>proc.lines.some(m=>m.method==='turn/completed')||proc.closed);
  const completed=proc.lines.find(m=>m.method==='turn/completed');
  assert.equal(completed?.params?.turn?.status,name==='codex_interrupt'?'interrupted':'completed');
  if(name==='codex_resume')assert.ok(requests.some(r=>JSON.stringify(r.input).includes(marker)),'resume lost previous response');
  records.push({name,agent:'codex',rpc:proc.lines,resume,requests:requests.length});checks.push(name);
  return thread.thread.id;
 } finally {await proc.stop();server.closeAllConnections();}
}
async function main(spec) {
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));base='http://127.0.0.1:'+server.address().port;
 const session=await cli('claude_turn','claude',spec.claude);
 assert.match(session,/^[a-f0-9-]{36}$/,'Claude session missing');
 mcpCalls=fs.existsSync(mcpFile)?fs.readFileSync(mcpFile,'utf8').trim().split('\n').length:0;
 assert.ok(mcpCalls>0,'MCP not called');checks.push('claude_mcp');
 await cli('claude_resume','claude',spec.claude_resume.map(arg=>arg.replaceAll('AGENTBOX_PROBE_RESUME',session)),session);
 await cli('claude_interrupt','claude',spec.claude);
 const thread=await protocol('codex_turn');checks.push('codex_handshake');
 await protocol('codex_resume',thread);await protocol('codex_interrupt',thread);
 await cli('codex_exec','codex',spec.codex);
 console.log(JSON.stringify({version:1,checks,records}));
}
function start(input) {main(JSON.parse(input)).catch(()=>{console.log(JSON.stringify({version:1,failed:mode||'setup'}));process.exitCode=1;}).finally(()=>{server.closeAllConnections();server.close();});}
if(process.argv[1]) start(process.argv[1]);
else {let input='';process.stdin.on('data',b=>{input+=b;if(input.length>16384)process.exit(1);});process.stdin.on('end',()=>start(input));}
