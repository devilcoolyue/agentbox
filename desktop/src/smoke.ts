// Included only with VITE_AGENTBOX_SMOKE=1 and the native desktop-smoke feature.
import { invoke } from '@tauri-apps/api/core';
import { nextTick } from 'vue';
import { language } from './i18n';
import type { Terminal } from '@xterm/xterm';
import type { TerminalConnection } from './terminal-connection';
let projectsMode=false;
let compatibilityMode=false;
let attachmentPath:string|undefined;
const verifiedTerminals=new Set<string>();
const menuTerminals=new Map<string,Terminal>();
const stage=(stage:string)=>invoke('smoke_stage',{stage});
let reportedFailure=false;
let attachmentProbe:Promise<void>|undefined;
let renderedWindow:Promise<void>|undefined;

// WKWebView may suspend its performance clock while backgrounded. These are
// fixture wall-clock deadlines, including time spent behind a locked desktop.
async function until<T>(read: () => T | null | false | undefined): Promise<T> {
  const deadline=Date.now()+10_000;
  while(Date.now()<deadline) {
    const value = read(); if (value) return value;
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  throw new Error('Smoke UI condition timed out');
}
function fill(selector: string, value: string) {
  const input = document.querySelector<HTMLInputElement>(selector)!;
  input.value = value; input.dispatchEvent(new Event('input', { bubbles: true }));
}
function visibleButton(label: string, scope: ParentNode | null = document) {
  return Array.from(scope?.querySelectorAll<HTMLButtonElement>('button') || []).find(button =>
    button.textContent?.trim() === label && !button.disabled && button.getClientRects().length > 0);
}
async function selectWorkspaceTab(label: '终端'|'文件'|'同步'|'恢复记录') {
  const tab = await until(() => visibleButton(label, document.querySelector('.workspace-tabs')));
  tab.click();
  await nextTick();
  await until(() => tab.getAttribute('aria-current') === 'page');
}

async function expectWorkspaceStatus(status: 'stopped'|'running') {
  const label = status === 'running' ? '运行中' : '已停止';
  try {
    await until(() => {
      const header = document.querySelector('.workspace-status');
      const selected = document.querySelectorAll('.workspace-item.selected');
      const dot = selected[0]?.querySelector('.status-dot');
      return selected.length === 1 && header?.classList.contains(status)
        && header.textContent?.trim() === label && dot?.classList.contains(status)
        && dot.getAttribute('aria-label') === label;
    });
  } catch {
    throw new Error(`Workspace status did not become ${status} in both header and selected sidebar item`);
  }
  await stage(`workspace_${status}`);
}
export async function reportSmokeError(error: unknown) { if(reportedFailure)return;reportedFailure=true;await invoke('smoke_finish', { ok: false, message: String(error) }); }

export async function runSmoke() {
  try {
    // Native smoke fixtures use Chinese labels on every runner OS. This module
    // is excluded from normal builds and only runs in isolated smoke profiles.
    language.value = 'zh-CN';
    await nextTick();
    const config = await invoke<{server:string;projects:boolean;sync:boolean;compat:boolean;username:string|null;password:string|null}>('smoke_config');
    await stage('config');
    renderedWindow = requireRenderedWindow();
    await renderedWindow;
    const server=config.server;projectsMode=config.projects;compatibilityMode=config.compat;
    await until(() => document.querySelector('.login-form'));await stage('login_form');
    await probeAppZoom();
    fill('input[type=url]', server); fill('input[autocomplete=username]', config.username??(config.sync?'alice':'smoke')); fill('input[type=password]', config.password??'synthetic-password');
    config.password=null;
    const remember = document.querySelector<HTMLInputElement>('input[type=checkbox]')!;
    remember.checked = false; remember.dispatchEvent(new Event('change', { bubbles: true }));
    document.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await stage('login_submitted');
    const workspace = await until(() => document.querySelector<HTMLButtonElement>('.workspace-item'));
    workspace.click();await stage('workspace');
    if(config.sync){await runSyncSmoke();return;}
    // The synthetic session must expose its stopped login snapshot first.
    // The frozen-server fixture may already have a running real container.
    if(!compatibilityMode)await expectWorkspaceStatus('stopped');
    if(compatibilityMode&&(!document.querySelector('.sidebar-note')?.textContent?.includes('基础连接模式')||document.querySelector('.sync-workspace')))throw new Error('Legacy server capability fallback failed');
    if(projectsMode){
      (await until(()=>visibleButton('添加项目',document.querySelector('.project-toolbar')))).click();
      await until(()=>document.querySelector('.project-edit'));
      fill('.project-edit input','Smoke project');
      document.querySelector('.project-edit')!.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true}));
      await until(()=>!document.querySelector('.project-edit'));
      const create=await until(()=>visibleButton('新建 Shell',document.querySelector('.project-heading')));
      await stage('project_created');create.click();
      await until(()=>document.querySelectorAll('.terminal-tabs>div').length===1);await stage('first_terminal');
      // Attachment acceptance navigates to Files and back. Complete it before
      // creating another tab so two terminal probes cannot compete for main UI.
      await until(()=>!!attachmentProbe);await attachmentProbe;
      await selectWorkspaceTab('终端');
      const projectList=document.querySelector<HTMLDetailsElement>('.project-list')!;
      if(!projectList.open)projectList.querySelector<HTMLElement>('summary')!.click();
      await until(()=>create.getClientRects().length>0);
      await until(()=>!create.disabled);create.click();
      await until(()=>document.querySelectorAll('.terminal-tabs>div').length===2);await stage('second_terminal');
    }else (await until(() => document.querySelector<HTMLButtonElement>('.open-workspace button'))).click();
  } catch (error) { await reportSmokeError(error); }
}

export async function probeTerminal(term: Terminal, connection: TerminalConnection, id='legacy') {
  try {
    await until(() => term.element?.closest('.terminal-pane')?.querySelector('.connection-status.connected'));
    await stage('terminal_connected');
    // Covers legacy, both independent project terminals and the real frozen
    // server. Reading the status once at login leaves this assertion stopped.
    await expectWorkspaceStatus('running');
    // Octal prefix prevents the command's own PTY echo from satisfying the
    // real-shell assertion. No model CLI is invoked by this disposable probe.
    connection.input(new TextEncoder().encode(compatibilityMode?"printf '\\101\\102\\117\\130-legacy-中文-✓\\n'; printf '\\101\\102\\117\\130-size:'; stty size\r":'echo 中文 ✓\r'));
    await until(() => {
      let echoed=false,sized=!compatibilityMode;
      for (let line = 0; line < term.buffer.active.length; line++) {
        const value=term.buffer.active.getLine(line)?.translateToString()||'';
        if(value.includes(compatibilityMode?'ABOX-legacy-中文-✓':'echo 中文 ✓'))echoed=true;
        if(compatibilityMode&&value.includes(`ABOX-size:${term.rows} ${term.cols}`))sized=true;
      }
      return echoed&&sized;
    });
    await stage('terminal_buffer_echo');
    // Both tabs share one native window; avoid competing activation callbacks.
    await renderedWindow;
    if (document.visibilityState !== 'visible') {
      renderedWindow = requireRenderedWindow();
      await renderedWindow;
    }
    await stage('terminal_focused');
    if (!term.element) throw new Error('Terminal not mounted');
    const element=term.element;
    menuTerminals.set(id, term);
    attachmentProbe??=(async()=>{
      const pane=element.closest('.terminal-pane')!;
      const transfer=new DataTransfer();transfer.items.add(new File(['attachment\r\n\0'],'fixture.bin',{type:'application/octet-stream'}));
      element.dispatchEvent(new ClipboardEvent('paste',{clipboardData:transfer,bubbles:true,cancelable:true}));
      const upload=await until(()=>Array.from(pane.querySelectorAll<HTMLButtonElement>('button')).find(b=>b.textContent==='确认上传附件'&&!b.disabled));
      upload.click();attachmentPath=await until(()=>{
        const path=pane.querySelector('.attachment-results code')?.textContent||'';
        return /^\/shared\/\.file\/[A-Za-z0-9._-]+\.bin$/.test(path)?path:null;
      });
      if(term.buffer.active.getLine(term.buffer.active.cursorY)?.translateToString().includes(attachmentPath))throw new Error('Attachment inserted without confirmation');
      await stage('attachment_uploaded');
      await selectWorkspaceTab('文件');
      const browser=await until(()=>{const element=document.querySelector<HTMLElement>('.file-browser');return element?.getClientRects().length?element:null;});
      const uploadDirectory=await until(()=>visibleButton('上传到此目录…',browser));
      await stage('directory_selected');uploadDirectory.click();
      // File upload confirmation is teleported to the window-level dialog host.
      const confirmation=await until(()=>{const element=document.querySelector<HTMLElement>('[aria-label="确认目录上传"]');return element?.getClientRects().length?element:null;});
      if(!confirmation.textContent?.includes('目标已有同名项'))throw new Error('Upload preview lost overwrite warning');
      await stage('directory_previewed');
      const confirm=await until(()=>visibleButton('确认上传并覆盖',confirmation));
      confirm.click();await until(()=>browser.textContent?.includes('文件已上传到目标目录')||browser.textContent?.includes('请刷新目录检查实际结果'));
      if(!browser.textContent?.includes('文件已上传到目标目录'))throw new Error('Directory upload failed: '+browser.textContent);
      await stage('directory_uploaded');
      await selectWorkspaceTab('终端');
      await until(()=>document.querySelector<HTMLElement>('.terminal-workspace')?.getBoundingClientRect().width);
    })();
    await attachmentProbe;
    await stage('terminal_echo');verifiedTerminals.add(id);
    if(verifiedTerminals.size===(projectsMode?2:1)){
      const visible=Array.from(menuTerminals.values()).find(candidate=>candidate.element?.getBoundingClientRect().width);
      if(!visible?.element)throw new Error('No visible terminal for menu probe');
      await probeTerminalMenu(visible, visible.element);
      await stage('finishing');await invoke('smoke_finish', { ok: true, attachmentPath, message: compatibilityMode?'Frozen server: native login/capability fallback, running workspace indicators, real tmux UTF-8 shell output and stty resize, confirmed attachment/directory upload with native byte readback passed.':projectsMode?'Project creation, two independent terminal tabs and stopped-to-running workspace indicators passed in the native WebView.':'Login, legacy fallback, workspace selection, stopped-to-running workspace indicators, native WebSocket, xterm UTF-8 rendering, input, resize and bundled sidecar passed.' });
    }
  } catch (error) { await reportSmokeError(error); }
}

async function requireRenderedWindow() {
  let config = await invoke<{window: unknown}>('smoke_config');
  const deadline = Date.now() + 10_000;
  let nextActivation = Date.now() + 2_000;
  while (document.visibilityState !== 'visible' && Date.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, 50));
    if (Date.now() >= nextActivation) {
      config = await invoke<{window: unknown}>('smoke_config');
      nextActivation = Date.now() + 2_000;
    }
  }
  if (document.visibilityState !== 'visible') {
    throw new Error(`WebView visibility gate failed: ${JSON.stringify({visibility: document.visibilityState, focus: document.hasFocus(), viewport: [innerWidth, innerHeight], native: config.window})}; an interactive visible desktop is required`);
  }
  await Promise.race([
    (async()=>{await new Promise(requestAnimationFrame);await new Promise(requestAnimationFrame);})(),
    new Promise((_,reject)=>setTimeout(()=>reject(new Error(`WebView did not render frames; visibility=${document.visibilityState}`)),3_000)),
  ]);
  if (document.visibilityState !== 'visible') throw new Error('WebView became hidden before rendering acceptance');
  await stage('window_visible');
}

async function probeAppZoom() {
  const control = (label: string) => document.querySelector<HTMLButtonElement>(`.app-zoom button[aria-label="${label}"]`)!;
  control('恢复界面缩放至 100%').click();
  await until(() => control('恢复界面缩放至 100%').textContent === '100%');
  const originalWidth = window.innerWidth;
  control('放大界面').click();
  await until(() => control('恢复界面缩放至 100%').textContent === '110%' && window.innerWidth < originalWidth);
  if (localStorage.getItem('agentbox.zoom') !== '110') throw new Error('Successful native zoom was not saved');
  window.dispatchEvent(new KeyboardEvent('keydown', { key: '0', metaKey: /Mac/.test(navigator.platform), ctrlKey: !/Mac/.test(navigator.platform), bubbles: true, cancelable: true }));
  await until(() => control('恢复界面缩放至 100%').textContent === '100%' && Math.abs(window.innerWidth - originalWidth) <= 1);
  await stage('application_zoom');
}

async function probeTerminalMenu(term: Terminal, element: HTMLElement) {
  const pane = element.closest('.terminal-pane')!;
  element.dispatchEvent(new MouseEvent('contextmenu', { clientX: 0, clientY: 0, bubbles: true, cancelable: true }));
  const menu = await until(() => pane.querySelector<HTMLElement>('[role="menu"]'));
  const select = Array.from(menu.querySelectorAll<HTMLButtonElement>('button')).find(button => button.textContent === '全选终端输出')!;
  select.click(); await until(() => term.hasSelection());
  if (pane.querySelector('[role="menu"]')) throw new Error('Terminal menu did not close after action');
  pane.querySelector<HTMLButtonElement>('[aria-label="打开终端菜单"]')!.click();
  const reopened = await until(() => pane.querySelector<HTMLElement>('[role="menu"]'));
  Array.from(reopened.querySelectorAll<HTMLButtonElement>('button')).find(button => button.textContent === '取消选择')!.click();
  await until(() => !term.hasSelection());
  await stage('terminal_context_menu');
}

// Click the actual Vue controls; all sync work still goes through production IPC.
async function runSyncSmoke() {
  await selectWorkspaceTab('同步');
  const panel=()=>document.querySelector('.sync-workspace');
  const text=()=>panel()?.textContent||'';
  const button=(label:string, scope:ParentNode|null=panel())=>Array.from(scope?.querySelectorAll<HTMLButtonElement>('button')||[]).find(b=>b.textContent?.trim()===label&&!b.disabled);
  const click=async(label:string)=>{(await until(()=>button(label))).click();};
  const idle=async()=>{await nextTick();await until(()=>!text().includes('正在处理…'));};
  const action=(action:string)=>invoke<{blocked?:number;done?:boolean;seeded?:number;total?:number}>('smoke_sync_action',{action});
  const expectText=(value:string)=>until(()=>text().includes(value));
  const preview=async()=>{await click('检查变更');await expectText('项变更');await idle();};
  const apply=async(expectProgress=false)=>{
    await click('确认执行以上变更');
    if(expectProgress){
      await until(()=>{const bar=panel()?.querySelector<HTMLProgressElement>('progress[aria-label="已读取传输字节"]');return bar&&bar.value>0&&bar.value<bar.max;});
      if(text().includes('同步完成'))throw new Error('In-flight transfer reported complete');
      await stage('sync_progress');
    }
    await idle();
  };
  const resolve=async(label:string)=>{await click(label);await until(()=>panel()?.querySelector('[role=alertdialog]'));await click('确认');await idle();};

  await click('选择本地目录并绑定');await until(()=>panel()?.querySelector('.sync-path'));await idle();await stage('sync_bound');
  await preview();await action('stale');await apply();await expectText('记录或预览已过期');await stage('sync_stale_rejected');
  await preview();await apply(true);await expectText('同步完成');await stage('sync_applied');

  await action('lost');await preview();await apply();await expectText('上次同步未完成核对');
  await idle();if(button('解除绑定'))throw new Error('Pending batch allowed archive');
  await click('核对未完成批次');await expectText('可以确认完成');await idle();
  await action('review_stale');await resolve('确认这批已完成');await expectText('记录或预览已过期');
  if(!panel()?.querySelector('.sync-pending'))throw new Error('Stale review cleared pending');
  await action('review_restore');await click('核对未完成批次');await expectText('可以确认完成');await resolve('确认这批已完成');
  await expectText('同步基线已提交');await stage('sync_finished');

  await action('download');await preview();await apply();await expectText('同步完成');
  await action('partial');await preview();await apply();await expectText('上次同步未完成核对');
  await click('核对未完成批次');await expectText('还不能确认为整批完成');await idle();
  if(button('确认这批已完成'))throw new Error('Partial batch offered finish');
  await resolve('保留文件并结束旧批次');await expectText('原基线和恢复记录已保留');
  await preview();await expectText('1 项变更，0 个冲突');await apply();await expectText('同步完成');await stage('sync_replanned');

  // Preserve all 21 real Engine commits without placing the entire setup under
  // a single native fixture request's 10-second network deadline.
  for(let seeded=1;seeded<=21;seeded++){
    const result=await action('history_seed');
    if(result.seeded!==seeded||result.total!==21)throw new Error('History fixture did not commit exactly the expected batch');
  }
  await click('刷新');await idle();
  await click('解除绑定');await until(()=>panel()?.querySelector('[aria-label="确认解除同步绑定"]'));
  await click('确认解除绑定');await expectText('此绑定已归档');await idle();
  if(button('检查变更'))throw new Error('Archived binding allowed preview');
  await stage('sync_archived');
  await click('查看恢复副本');await until(()=>panel()?.querySelector('.sync-recovery details'));await idle();
  await expectText('本机全部绑定');
  panel()!.querySelector<HTMLDetailsElement>('.sync-recovery details')!.open=true;
  await click('清理无副本历史');await until(()=>panel()?.querySelector('[aria-label="清理无副本历史"]'));
  await click('确认清理历史');await idle();await expectText('共 25 批');await stage('sync_history_cleanup');

  await click('下一页');await expectText('第 2 页');await idle();
  await click('上一页');await expectText('第 1 页');await idle();
  let exported=0,pages=0;
  do {
    pages++;
    const batches=Array.from(panel()!.querySelectorAll<HTMLDetailsElement>('.sync-recovery details')).filter(details=>details.querySelector('li'));
    for(const details of batches) {
      details.open=true;
      const exportButton=()=>button('导出原内容',details);
      await action('export_overlap');(await until(exportButton)).click();await idle();
      if(text().includes('副本已导出'))throw new Error('Export into mapped tree accepted');
      await action('export_safe');(await until(exportButton)).click();await expectText('副本已导出');await idle();
      (await until(exportButton)).click();await idle();
      if(text().includes('副本已导出'))throw new Error('Existing export overwritten');
      exported++;
    }
    if(!button('下一页'))break;
    await click('下一页');await expectText(`第 ${pages+1} 页`);await idle();
  } while(pages<5);
  if(pages!==2||exported!==2)throw new Error('Paginated history lost local or remote recovery');
  await stage('sync_history_pages');
  await stage('sync_exported');
  // Dispose only after both recovery copies were exported and verified by the
  // server fixture. The UI must retain a visible audit row after deletion.
  while(button('上一页')){await click('上一页');await idle();}
  let disposal=button('清理本地副本');
  while(!disposal&&button('下一页')){await click('下一页');await idle();disposal=button('清理本地副本');}
  if(!disposal)throw new Error('Completed local recovery cannot be reviewed for disposal');
  disposal.click();await until(()=>panel()?.querySelector('[aria-label="清理本地恢复副本"]'));
  await click('确认永久清理此副本');await idle();await expectText('本地恢复副本已清理');
  let disposed=text().includes('原内容已清理（保留历史引用）');
  while(!disposed&&button('下一页')){await click('下一页');await idle();disposed=text().includes('原内容已清理（保留历史引用）');}
  if(!disposed)throw new Error('Disposed recovery lost its audit row');
  await stage('sync_recovery_discarded');
  while(button('上一页')){await click('上一页');await idle();}
  const remoteBatch=()=>Array.from(panel()!.querySelectorAll<HTMLDetailsElement>('.sync-recovery details')).find(details=>Array.from(details.querySelectorAll('li')).some(row=>row.textContent?.includes('服务器'))&&button('预览服务器清理',details));
  while(!remoteBatch()&&button('下一页')){await click('下一页');await idle();}
  const retirement=remoteBatch();if(!retirement)throw new Error('Missing remote recovery cleanup review');
  retirement.open=true;button('预览服务器清理',retirement)!.click();
  await until(()=>panel()?.querySelector('[aria-label="清理服务器同步历史"]'));await idle();
  await expectText('服务器会保留永久执行收据');await click('确认永久清理服务器历史');
  await idle();await expectText('服务器清理已完成');await action('verify_remote_cleanup');
  await stage('sync_remote_cleaned');
  await click('重新绑定此项目');await click('选择本地目录并绑定');
  await until(()=>panel()?.querySelector('.sync-path'));await idle();
  await preview();await expectText('0 项变更，0 个冲突');await apply();await expectText('同步完成');
  await stage('sync_rebound');
  await action('choices');await preview();await expectText('2 个冲突');
  for(const [name,value] of [['choice-local.txt','local'],['choice-remote.txt','remote']]) {
    const select=panel()!.querySelector<HTMLSelectElement>(`select[aria-label="冲突来源 ${name}"]`);
    if(!select)throw new Error('Per-file conflict selector missing');
    select.value=value;select.dispatchEvent(new Event('change',{bubbles:true}));
  }
  await click('根据逐项选择生成预览');await idle();await expectText('2 项变更，0 个冲突');
  await apply();await expectText('同步完成');await stage('sync_choices');
  await click('开启持续同步');await expectText('等待下一轮检查');
  const workspaces=Array.from(document.querySelectorAll<HTMLButtonElement>('.workspace-item'));
  if(workspaces.length!==2)throw new Error('Missing second workspace for background sync');
  workspaces[1].click();await until(()=>panel()?.getBoundingClientRect().width===0);
  await action('continuous');
  const continuousDeadline=Date.now()+15_000;
  while(!(await action('continuous_done')).done){if(Date.now()>continuousDeadline)throw new Error('Continuous sync did not transfer');await new Promise(r=>setTimeout(r,100));}
  workspaces[0].click();await until(()=>panel()!.getBoundingClientRect().width>0);
  await expectText('等待下一轮检查');
  await action('continuous_conflict');await expectText('持续同步已暂停');await idle();
  await expectText('1 个冲突');
  await action('continuous_restore');await click('开启持续同步');await expectText('等待下一轮检查');
  await click('停止持续同步');await idle();await expectText('持续同步已停止');await stage('sync_continuous');
  await action('abandon_lost');await preview();await apply();await expectText('上次同步未完成核对');
  await click('无法核对：归档此绑定');await until(()=>panel()?.querySelector('[aria-label="归档未核验批次"]'));await idle();
  if(button('确认归档未知结果'))throw new Error('Unknown batch did not require explicit acknowledgment');
  fill('input[aria-label="归档确认文字"]','归档未核验批次');await click('确认归档未知结果');await idle();await expectText('结果未知');
  await click('查看恢复副本');await idle();await expectText('未核验归档（结果未知）');
  await click('重新绑定此项目');await click('选择本地目录并绑定');await until(()=>panel()?.querySelector('.sync-path'));await idle();
  await preview();await expectText('0 项变更，0 个冲突');await apply();await expectText('同步完成');await stage('sync_abandoned');
  await action('orphan_seed');
  await selectWorkspaceTab('恢复记录');
  const orphan = await until(() => document.querySelector<HTMLDetailsElement>('.remote-recovery'));
  orphan.open = true;
  fill('.remote-recovery form input', 'orphan-fixture-device');
  (await until(() => button('读取记录', orphan))).click();
  await until(() => orphan.querySelector('.recovery-list')?.textContent?.includes('old.txt'));
  (await until(() => button('核对记录', orphan))).click();
  await until(() => orphan.querySelector('.recovery-review')?.textContent?.includes('符合该次操作之后'));
  if(button('永久清理此条原内容', orphan))throw new Error('Orphan cleanup allowed without typed confirmation');
  (await until(() => button('导出原内容…', orphan))).click();
  await until(() => orphan.textContent?.includes('原内容已导出为'));
  fill('.remote-recovery .recovery-review input', '永久清理原内容');
  (await until(() => button('永久清理此条原内容', orphan))).click();
  await until(() => orphan.textContent?.includes('服务器原内容已清理，执行收据保留'));
  if(button('导出原内容…', orphan))throw new Error('Retired orphan still offered export');
  await stage('orphan_recovery_managed');
  await selectWorkspaceTab('同步');
  async function blockPreview(count:number) {
    await action('block');await click('检查变更');
    const deadline=Date.now()+10_000;
    while((await action('blocked')).blocked!==count){if(Date.now()>deadline)throw new Error('Preview did not reach blocked server');await new Promise(r=>setTimeout(r,50));}
    await until(()=>panel()?.querySelector('.sync-progress')?.textContent?.includes('正在检查服务器文件'));
  }
  await blockPreview(1);await click('取消');await idle();if(panel()?.querySelector('.sync-progress'))throw new Error('Progress remained after cancel');await stage('sync_canceled');
  await blockPreview(2);
  (await until(()=>document.querySelector<HTMLButtonElement>('[aria-label="账号与连接设置"]'))).click();
  (await until(()=>visibleButton('退出登录',document.querySelector('.settings-signout')))).click();await until(()=>document.querySelector('.login-form'));
  if(panel())throw new Error('Sync UI survived logout');await stage('sync_logged_out');
  await action('verify');await stage('finishing');
  await invoke('smoke_finish',{ok:true,message:'Native Vue/Rust/Go sync: pagination/capacity, per-file conflicts, opt-in continuous sync, pending reconciliation, recovery exports, cancel and logout passed.'});
}
