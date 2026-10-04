<script setup lang="ts">
import { newTaskId } from './task-id';
import { Channel } from '@tauri-apps/api/core';
import { byteLabel } from './sync-task';
import { listen, type UnlistenFn } from '@tauri-apps/api/event';
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { SearchAddon } from '@xterm/addon-search';
import '@xterm/xterm/css/xterm.css';
import { TerminalConnection } from './terminal-connection';
import { bridge, errorMessage } from './bridge';
import { AttachmentQueue, type AttachmentInput, type AttachmentResult } from './attachment-queue';
import { terminalShortcut } from './terminal-shortcuts';
import UiIcon from './UiIcon.vue';

const props = defineProps<{ session: string; terminal?: string; fontSize: number; light: boolean }>();
const host = ref<HTMLDivElement>();
const message = ref('尚未连接');
const connected = ref(false);
const uploadBusy = ref(false);
const uploadCurrent = ref(0);
const uploadTotal = ref(0);
const uploadName = ref('');
const uploadBytes=ref(0);const uploadSize=ref(0);
let uploadGeneration=0;
const pendingFiles=ref<AttachmentInput[]>([]);
const uploadedFiles=ref<AttachmentResult[]>([]);
const attachmentMessage=ref('');
const attachmentTask=newTaskId();
let alive=true;
const unlisten:UnlistenFn[]=[];
const searchText = ref('');
const showSearch = ref(false);
const searchInput = ref<HTMLInputElement>();
const draft=ref('');
const showDraft=ref(false);
const draftInput = ref<HTMLTextAreaElement>();
const hasSelection = ref(false);
const contextMenu = ref<HTMLDivElement>();
const moreButton = ref<HTMLButtonElement>();
const menuPosition = ref<{ x: number; y: number }>();
const mac = /Mac/.test(navigator.platform);
const copyKeys = mac ? '⌘ C' : 'Ctrl Shift C';
const pasteKeys = mac ? '⌘ V' : 'Ctrl Shift V';
let composing=false;
let pendingResize=false;
let wakeTimer:ReturnType<typeof setInterval>|undefined;
let lastWakeCheck=Date.now();
const term = new Terminal({ cursorBlink: true, scrollback: 5000, fontSize: props.fontSize, fontFamily: 'Menlo, Consolas, "Microsoft YaHei", monospace', allowProposedApi: false });
const fit = new FitAddon();
const search = new SearchAddon();
const connection = new TerminalConnection((bytes, done) => term.write(bytes, done), (text, ready) => { message.value = text; connected.value = ready; }, () => ({ cols: term.cols, rows: term.rows }));
let observer: ResizeObserver;
function resize() { if(composing){pendingResize=true;return;}pendingResize=false; if (host.value?.clientWidth) { fit.fit(); connection.resize(); } }
function theme() {
  const style = getComputedStyle(host.value || document.documentElement);
  const token = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback;
  // Agent TUIs use ANSI colors designed for a dark screen in both app themes.
  term.options.theme = {
    background: token('--term-bg', '#0d1016'), foreground: token('--term-fg', '#d6dae3'),
    cursor: token('--term-cursor', '#e8a33d'), selectionBackground: token('--term-sel', '#2e3646'),
  };
}
async function toggleSearch() {
  showSearch.value = !showSearch.value;
  await nextTick();
  if (!alive) return;
  if (showSearch.value) searchInput.value?.focus();
  else term.focus();
}
function searchKey(event: KeyboardEvent) {
  if (event.key === 'Escape' && !event.isComposing && event.keyCode !== 229) {
    event.preventDefault();
    void toggleSearch();
  }
}
async function toggleDraft() {
  showDraft.value = !showDraft.value;
  await nextTick();
  if (!alive) return;
  if (showDraft.value) draftInput.value?.focus();
  else term.focus();
}
async function copy() {
  const selection = term.getSelection();
  if (!selection) return;
  try { await navigator.clipboard.writeText(selection); }
  catch (error) { message.value = errorMessage(error); }
}
function closeMenu(focus = false) {
  menuPosition.value = undefined;
  if (focus && alive) term.focus();
}
async function openMenu(x?: number, y?: number) {
  const bounds = host.value?.getBoundingClientRect();
  if (!bounds?.width || !bounds.height || composing) return;
  menuPosition.value = { x: x ?? bounds.left + 20, y: y ?? bounds.top + 20 };
  await nextTick();
  const menu = contextMenu.value;
  if (!menu || !menuPosition.value) return;
  menuPosition.value = {
    x: Math.max(4, Math.min(menuPosition.value.x, window.innerWidth - menu.offsetWidth - 4)),
    y: Math.max(4, Math.min(menuPosition.value.y, window.innerHeight - menu.offsetHeight - 4)),
  };
  menu.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
}
function context(event: MouseEvent) {
  event.preventDefault(); event.stopImmediatePropagation();
  if (event.clientX === 0 && event.clientY === 0) void openMenu();
  else void openMenu(event.clientX, event.clientY);
}
function toolbarMenu(event: MouseEvent) {
  if (menuPosition.value) { closeMenu(true); return; }
  const bounds = (event.currentTarget as HTMLButtonElement).getBoundingClientRect();
  void openMenu(bounds.right - 252, bounds.bottom + 6);
}
function menuAction(action: 'copy' | 'paste' | 'select' | 'clear' | 'draft' | 'reconnect') {
  closeMenu(true);
  if (action === 'copy') void copy();
  else if (action === 'paste') void paste();
  else if (action === 'select') term.selectAll();
  else if (action === 'clear') term.clearSelection();
  else if (action === 'draft') void toggleDraft();
  else reconnect();
}
function menuKey(event: KeyboardEvent) {
  if (event.key === 'Escape' || event.key === 'Tab') {
    event.preventDefault(); closeMenu(true); return;
  }
  const buttons = Array.from(contextMenu.value?.querySelectorAll<HTMLButtonElement>('button:not(:disabled)') || []);
  const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
  let index: number;
  if (event.key === 'ArrowDown') index = (current + 1) % buttons.length;
  else if (event.key === 'ArrowUp') index = (current - 1 + buttons.length) % buttons.length;
  else if (event.key === 'Home') index = 0;
  else if (event.key === 'End') index = buttons.length - 1;
  else return;
  event.preventDefault(); buttons[index]?.focus();
}
function dismissMenu(event: PointerEvent) {
  if (moreButton.value?.contains(event.target as Node)) return;
  if (!contextMenu.value?.contains(event.target as Node)) closeMenu();
}
function viewportChanged() { closeMenu(); requestAnimationFrame(resize); }
async function paste() {
  try {
    const result=await bridge.invoke<{text:string|null;files:(AttachmentInput & {ticket:string})[]}>('read_attachment_clipboard');
    if(!alive){await releaseFiles(result.files);return;}
    if(result.files.length)setFiles(result.files);else if(result.text!==null)term.paste(result.text);
  }catch(error){if(alive)attachmentMessage.value=errorMessage(error);}
}
function reconnect() { term.reset(); void connection.open(props.session, props.terminal); }
const attachments = new AttachmentQueue(async input => {
 const generation=++uploadGeneration;uploadBytes.value=0;uploadSize.value=0;
 const events=new Channel<{bytes:number;total:number|null}>();events.onmessage=event=>{
  if(alive&&uploadBusy.value&&generation===uploadGeneration){uploadBytes.value=event.bytes;uploadSize.value=event.total||0;}
 };
 return bridge.invoke<AttachmentResult>('upload_attachment', {
  request:{session:props.session,task:attachmentTask,ticket:input.ticket,filename:input.name,mime:input.type,bytes:input.bytes},events,
 });
});
async function uploadFiles(files: readonly AttachmentInput[]) {
  if (!files.length || uploadBusy.value) return;
  uploadBusy.value = true; attachmentMessage.value=''; uploadCurrent.value = 0; uploadTotal.value = files.length; uploadName.value = '';
  try {
    await attachments.run(files, (current, total, name) => {
      if(alive){uploadCurrent.value = current; uploadTotal.value = total; uploadName.value = name;}
    },result=>{if(alive)uploadedFiles.value.push(result);});
    if(alive)attachmentMessage.value='附件已上传，可将路径插入终端；不会自动执行命令。附件沿用服务器 48 小时清理规则。';
  } catch (error) {
    if(alive)attachmentMessage.value=error instanceof Error?error.message:errorMessage(error);
  } finally {
    await releaseFiles(files);
    if(alive){pendingFiles.value=[];uploadBusy.value=false;uploadName.value='';}
  }
}
async function releaseFiles(files: readonly AttachmentInput[]){
 const tickets=files.flatMap(f=>f.ticket?[f.ticket]:[]);
 if(tickets.length)await bridge.invoke('release_attachments',{tickets}).catch(()=>{});
}
function setFiles(files:AttachmentInput[]){
 if(uploadBusy.value){void releaseFiles(files);return;}
 void releaseFiles(pendingFiles.value);
 pendingFiles.value=files;attachmentMessage.value='附件已选择，确认上传到当前空间的共享附件目录。';
}
async function chooseFiles() {
 try{const files=await bridge.invoke<AttachmentInput[]>('choose_attachments');if(alive)setFiles(files);else await releaseFiles(files);}
 catch(error){if(alive)attachmentMessage.value=errorMessage(error);}
}
function pasteFiles(event: ClipboardEvent) {
 const files=Array.from(event.clipboardData?.files||[]);
 if(files.length){event.preventDefault();event.stopImmediatePropagation();setFiles(files);}
}
function cancelUpload() {
 uploadGeneration++;attachments.cancel();
 void bridge.invoke('cancel_attachment',{task:attachmentTask}).catch(()=>{});
 if(!uploadBusy.value){void releaseFiles(pendingFiles.value);pendingFiles.value=[];}
}
function insertAttachment(path:string){term.paste(path+' ');term.focus();}
async function listenForFiles(){
 const stop=await listen<{files:AttachmentInput[];x:number;y:number}>('attachment-drop',({payload})=>{
  const rect=host.value?.getBoundingClientRect();
  if(alive&&rect&&rect.width>0&&rect.height>0&&payload.x>=rect.left&&payload.x<=rect.right&&payload.y>=rect.top&&payload.y<=rect.bottom)setFiles(payload.files);
 });
 if(alive)unlisten.push(stop);else stop();
 const stopErrors=await listen<{message:string}>('attachment-drop-error',({payload})=>{
  if(alive&&host.value?.clientWidth)attachmentMessage.value=payload.message;
 });
 if(alive)unlisten.push(stopErrors);else stopErrors();
}
onMounted(() => {
  term.loadAddon(fit); term.loadAddon(search); theme(); term.open(host.value!); fit.fit();
  term.onData(text => connection.input(new TextEncoder().encode(text)));
  term.onBinary(text => connection.input(Uint8Array.from(text, c => c.charCodeAt(0))));
  term.onSelectionChange(() => { hasSelection.value = term.hasSelection(); });
  term.attachCustomKeyEventHandler(event => {
    const action = terminalShortcut(event, mac, composing);
    if (!action) return true;
    if (event.type === 'keydown') {
      event.preventDefault();
      if (action === 'copy') void copy();
      else if (action === 'paste') void paste();
      else void openMenu();
    }
    return false;
  });
  window.addEventListener('pointerdown', dismissMenu, true);
  window.addEventListener('resize', viewportChanged);
  window.addEventListener('agentbox-zoom', viewportChanged);
  window.addEventListener('blur', viewportChanged);
  void listenForFiles();
  wakeTimer=setInterval(()=>{const now=Date.now();if(now-lastWakeCheck>45_000){connection.resumeAfterSleep();resize();}lastWakeCheck=now;},10_000);
  observer = new ResizeObserver(resize); observer.observe(host.value!);
  void connection.open(props.session, props.terminal);
  if (import.meta.env.VITE_AGENTBOX_SMOKE === '1') void import('./smoke').then(module => module.probeTerminal(term, connection, props.terminal));
});
watch(() => props.fontSize, size => { if(composing){pendingResize=true;return;}term.options.fontSize = size; resize(); });
watch(() => props.light, theme);
function compositionEnd(){composing=false;if(pendingResize){term.options.fontSize=props.fontSize;resize();}}
function insertDraft(){
 if(new TextEncoder().encode(draft.value).length>120*1024){attachmentMessage.value='暂存输入最多 120 KiB，请分段插入';return;}
 term.paste(draft.value);term.focus();
}
onBeforeUnmount(() => {
  alive=false;clearInterval(wakeTimer);cancelUpload();void releaseFiles(pendingFiles.value);unlisten.forEach(stop=>stop()); observer?.disconnect(); connection.close(); term.dispose();
  window.removeEventListener('pointerdown', dismissMenu, true);
  window.removeEventListener('resize', viewportChanged);
  window.removeEventListener('agentbox-zoom', viewportChanged);
  window.removeEventListener('blur', viewportChanged);
});
</script>

<template>
  <section class="terminal-pane">
    <div class="terminal-tools">
      <span class="connection-status" :class="{ connected }" role="status" :title="message">
        <span class="connection-dot" aria-hidden="true"></span><span>{{ message }}</span>
      </span>
      <div class="terminal-actions" aria-label="终端工具">
        <button v-if="!connected" class="terminal-icon-button reconnect-button" aria-label="重新连接" title="重新连接" @click="reconnect"><UiIcon name="refresh" :size="16"/><span class="terminal-sr-only">重新连接</span></button>
        <button class="terminal-icon-button" :class="{ active: showSearch }" aria-label="搜索终端输出" title="搜索终端输出" :aria-expanded="showSearch" @click="toggleSearch"><UiIcon name="search" :size="16"/></button>
        <button class="terminal-icon-button" :disabled="uploadBusy" aria-label="上传文件" title="上传文件或截图" @click="chooseFiles"><UiIcon name="paperclip" :size="16"/><span class="terminal-sr-only">上传文件</span></button>
        <button class="terminal-icon-button" aria-label="粘贴" :title="'粘贴文字或附件 · '+pasteKeys" @click="paste"><UiIcon name="paste" :size="16"/><span class="terminal-sr-only">粘贴</span></button>
        <span class="terminal-tool-divider" aria-hidden="true"></span>
        <button ref="moreButton" class="terminal-icon-button" :class="{ active: !!menuPosition }" aria-label="打开终端菜单" title="更多终端操作 · Shift F10" :aria-expanded="!!menuPosition" aria-haspopup="menu" @click="toolbarMenu"><UiIcon name="more" :size="18"/><span class="terminal-sr-only">更多…</span></button>
      </div>
    </div>
    <form v-if="showSearch" class="terminal-search" role="search" @submit.prevent="search.findNext(searchText)" @keydown="searchKey">
      <UiIcon name="search" :size="15"/>
      <input ref="searchInput" v-model="searchText" aria-label="搜索终端输出" placeholder="搜索终端输出" autocomplete="off" spellcheck="false">
      <button type="button" class="terminal-icon-button" :disabled="!searchText" aria-label="上一个匹配" title="上一个匹配" @click="search.findPrevious(searchText)"><UiIcon name="chevron-up" :size="16"/></button>
      <button class="terminal-icon-button" :disabled="!searchText" aria-label="查找" title="下一个匹配 · Enter"><UiIcon name="chevron" :size="16"/><span class="terminal-sr-only">查找</span></button>
      <button type="button" class="terminal-icon-button" aria-label="关闭搜索" title="关闭搜索 · Esc" @click="toggleSearch"><UiIcon name="close" :size="15"/></button>
    </form>
    <div ref="host" class="terminal-host" @contextmenu.capture="context" @paste.capture="pasteFiles" @compositionstart.capture="composing=true" @compositionend.capture="compositionEnd"></div>
    <div v-if="menuPosition" ref="contextMenu" class="terminal-menu" role="menu" aria-label="终端菜单" :style="{ left: menuPosition.x+'px', top: menuPosition.y+'px' }" @keydown="menuKey">
      <button role="menuitem" :disabled="!hasSelection" @click="menuAction('copy')"><UiIcon name="copy" :size="15"/><span>复制所选文本</span><kbd>{{ copyKeys }}</kbd></button>
      <button role="menuitem" @click="menuAction('paste')"><UiIcon name="paste" :size="15"/><span>粘贴文字或附件</span><kbd>{{ pasteKeys }}</kbd></button>
      <div class="terminal-menu-divider" role="separator"></div>
      <button role="menuitem" @click="menuAction('select')"><UiIcon name="list-check" :size="15"/><span>全选终端输出</span></button>
      <button role="menuitem" :disabled="!hasSelection" @click="menuAction('clear')"><UiIcon name="close" :size="15"/><span>取消选择</span></button>
      <button role="menuitemcheckbox" :aria-checked="showDraft" @click="menuAction('draft')"><UiIcon name="edit" :size="15"/><span>暂存输入</span><UiIcon v-if="showDraft" class="menu-checked" name="check" :size="14"/></button>
      <div class="terminal-menu-divider" role="separator"></div>
      <button role="menuitem" @click="menuAction('reconnect')"><UiIcon name="refresh" :size="15"/><span>重新连接</span></button>
    </div>
    <div v-if="showDraft" class="terminal-draft">
      <div class="terminal-section-heading"><span><UiIcon name="edit" :size="14"/>暂存输入</span><button class="terminal-icon-button" aria-label="关闭暂存输入" title="关闭暂存输入" @click="toggleDraft"><UiIcon name="close" :size="14"/></button></div>
      <textarea ref="draftInput" v-model="draft" aria-label="终端暂存输入" placeholder="编写多行内容，再插入当前终端" maxlength="122880"></textarea>
      <div class="terminal-draft-actions"><span>最多 120 KiB</span><button class="terminal-text-button" :disabled="!draft" @click="draft=''">清空</button><button class="terminal-primary-button" :disabled="!connected||!draft" @click="insertDraft"><UiIcon name="terminal" :size="14"/><span>插入终端</span></button></div>
    </div>
    <div v-if="pendingFiles.length||uploadedFiles.length||attachmentMessage||uploadBusy" class="terminal-attachments">
      <div v-if="pendingFiles.length&&!uploadBusy" class="attachment-selection">
        <div class="terminal-section-heading"><span><UiIcon name="paperclip" :size="14"/>待上传附件 <small>{{ pendingFiles.length }} 项</small></span><span class="attachment-selection-actions"><button class="terminal-text-button" @click="cancelUpload">取消选择</button><button class="terminal-primary-button" @click="uploadFiles(pendingFiles)"><UiIcon name="upload" :size="14"/><span>确认上传附件</span></button></span></div>
        <ul class="attachment-files"><li v-for="(file,index) in pendingFiles" :key="file.ticket||file.name+'-'+index"><UiIcon name="file" :size="15"/><span class="attachment-name" :title="file.name">{{ file.name }}</span><span class="attachment-size">{{ byteLabel(file.size) }}</span></li></ul>
      </div>
      <div v-if="uploadBusy" class="attachment-progress" role="status">
        <div class="terminal-section-heading"><span><UiIcon name="upload" :size="14"/>上传附件 <small>{{ uploadCurrent }} / {{ uploadTotal }} 已完成</small></span><button class="terminal-text-button" @click="cancelUpload">取消</button></div>
        <div class="attachment-progress-file"><span class="attachment-name" :title="uploadName">{{ uploadName }}</span><span class="attachment-size">已读取 {{ byteLabel(uploadBytes) }} / {{ byteLabel(uploadSize) }}</span></div>
        <progress :value="uploadSize ? Math.min(uploadBytes,uploadSize) : undefined" :max="uploadSize||1" aria-label="当前附件读取进度"></progress>
        <p>仍需服务器确认。</p>
      </div>
      <p v-if="attachmentMessage" class="attachment-message" role="status">{{ attachmentMessage }}</p>
      <div v-if="uploadedFiles.length" class="attachment-completed">
        <div class="terminal-section-heading"><span><UiIcon class="attachment-success" name="check" :size="14"/>已上传 <small>{{ uploadedFiles.length }} 项</small></span></div>
        <ul class="attachment-results"><li v-for="file in uploadedFiles" :key="file.path"><UiIcon name="file" :size="16"/><span class="attachment-result-info"><span class="attachment-name" :title="file.orig||file.name">{{ file.orig||file.name }}</span><code :title="file.path">{{ file.path }}</code></span><button class="terminal-text-button insert-path" :disabled="!connected" @click="insertAttachment(file.path)"><UiIcon name="terminal" :size="14"/><span>插入路径</span></button></li></ul>
      </div>
    </div>
  </section>
</template>

<style scoped>
.terminal-pane{display:flex;flex-direction:column;flex:1;min-width:0;min-height:0;position:relative;background:var(--term-bg,#0d1016)}
.terminal-tools{display:flex;align-items:center;flex-wrap:nowrap;gap:12px;min-height:41px;padding:5px 12px;border-bottom:1px solid var(--line);background:var(--panel);font-size:12px}
.connection-status{display:flex;align-items:center;gap:8px;min-width:0;flex:1;margin:0;color:var(--muted)}
.connection-status>span:last-child{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.connection-status.connected{color:var(--text)}
.connection-dot{width:6px;height:6px;flex:none;border-radius:50%;background:var(--muted)}.connected .connection-dot{background:var(--green)}
.terminal-actions{display:flex;align-items:center;gap:3px;flex:none}.terminal-tool-divider{height:16px;width:1px;margin:0 5px;background:var(--line)}
.terminal-icon-button{display:inline-flex;align-items:center;justify-content:center;width:30px;height:30px;flex:none;padding:0;border:1px solid transparent;border-radius:5px;background:transparent;color:var(--muted);cursor:pointer;transition:background .14s,color .14s}
.terminal-icon-button:hover:not(:disabled),.terminal-icon-button.active{background:var(--panel-2);color:var(--amber)}
.terminal-icon-button:disabled{opacity:.35;cursor:default}.terminal-icon-button:focus-visible,.terminal-text-button:focus-visible,.terminal-primary-button:focus-visible{outline:2px solid var(--amber);outline-offset:1px}
.reconnect-button{color:var(--amber)}
.terminal-search{display:flex;align-items:center;gap:4px;margin:0;padding:6px 12px;border-bottom:1px solid var(--line);background:var(--panel);color:var(--muted);animation:terminal-reveal .14s ease-out}
.terminal-search>svg{margin-right:5px;flex:none}.terminal-search input{flex:1;min-width:0;padding:5px 0;border:0;border-radius:0;outline:0;background:transparent;color:var(--text);font:inherit;font-size:12px;box-shadow:none}.terminal-search:focus-within{box-shadow:inset 2px 0 var(--amber)}
.terminal-host{position:relative;flex:1;min-width:0;min-height:0;padding:0;overflow:hidden;background:var(--term-bg,#0d1016);color:var(--term-fg,#d6dae3);color-scheme:dark}
.terminal-host :deep(.xterm){height:100%;padding:10px 12px 10px 16px;box-sizing:border-box}
.terminal-host :deep(.xterm),.terminal-host :deep(.xterm-viewport),.terminal-host :deep(.xterm-scrollable-element){background-color:var(--term-bg,#0d1016)}
.terminal-host :deep(.xterm-viewport){scrollbar-color:var(--term-sel,#2e3646) var(--term-bg,#0d1016)}
.terminal-host :deep(.composition-view){background:var(--term-bg,#0d1016);color:var(--term-fg,#d6dae3)}
.terminal-menu{position:fixed;z-index:1000;display:grid;width:252px;max-width:calc(100vw - 8px);padding:5px;border:1px solid var(--line);border-radius:7px;background:var(--panel);box-shadow:0 10px 28px #0003;animation:terminal-reveal .12s ease-out}
.terminal-menu button{display:grid;grid-template-columns:15px minmax(0,1fr) auto;align-items:center;gap:9px;min-height:34px;margin:0;padding:7px 9px;border:0;border-radius:4px;text-align:left;font-size:12px;color:var(--text);background:transparent;white-space:nowrap}
.terminal-menu button>svg{color:var(--muted)}.terminal-menu button:hover:not(:disabled),.terminal-menu button:focus-visible{outline:0;background:var(--panel-2);color:var(--amber)}.terminal-menu button:focus-visible{box-shadow:inset 2px 0 var(--amber)}.terminal-menu button:disabled{opacity:.35;cursor:default}.terminal-menu button>.menu-checked{color:var(--amber)}
.terminal-menu kbd{font:inherit;font-size:10px;color:var(--muted);white-space:nowrap}.terminal-menu-divider{height:1px;margin:4px 5px;background:var(--line)}
.terminal-draft{display:grid;gap:7px;padding:8px 12px;border-top:1px solid var(--line);background:var(--panel);font-size:12px;animation:terminal-reveal .14s ease-out}
.terminal-section-heading{display:flex;align-items:center;justify-content:space-between;gap:10px;min-height:28px;font-size:12px;color:var(--text)}
.terminal-section-heading>span:first-child{display:flex;align-items:center;gap:7px;min-width:0}.terminal-section-heading small{margin-left:2px;font-size:11px;font-weight:400;color:var(--muted);white-space:nowrap}
.terminal-draft textarea{display:block;width:100%;min-height:68px;max-height:140px;resize:vertical;padding:8px 10px;border:1px solid var(--line);border-radius:5px;background:var(--bg);color:var(--text);font-family:Menlo,Consolas,"Microsoft YaHei",monospace;font-size:12px;line-height:1.5;outline:0}
.terminal-draft textarea:focus{border-color:var(--amber)}.terminal-draft-actions{display:flex;align-items:center;gap:8px}.terminal-draft-actions>span{margin-right:auto;color:var(--muted);font-size:11px}
.terminal-text-button,.terminal-primary-button{display:inline-flex;align-items:center;justify-content:center;gap:6px;min-height:28px;padding:4px 8px;border:1px solid transparent;border-radius:5px;font-size:11px;line-height:1.35;white-space:nowrap;cursor:pointer;transition:background .14s,color .14s}
.terminal-text-button{background:transparent;color:var(--muted)}.terminal-text-button:hover:not(:disabled){background:var(--panel-2);color:var(--text)}
.terminal-primary-button{background:var(--accent);color:var(--term-bg,#0d1016)}.terminal-primary-button:hover:not(:disabled){filter:brightness(1.06)}.terminal-text-button:disabled,.terminal-primary-button:disabled{opacity:.4;cursor:default}
.terminal-attachments{flex:none;max-height:min(30vh,240px);overflow:auto;border-top:1px solid var(--line);background:var(--panel);font-size:12px}
.attachment-selection,.attachment-progress,.attachment-completed{display:block;margin:0;padding:8px 12px}.attachment-selection-actions{display:flex;align-items:center;gap:5px;flex:none}
.attachment-files,.attachment-results{list-style:none;margin:2px 0 0;padding:0}.attachment-files li{display:flex;align-items:center;gap:8px;min-height:30px;padding:4px 0}.attachment-files li>svg,.attachment-results li>svg{flex:none;color:var(--muted)}
.attachment-name{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.attachment-files .attachment-name{flex:1}.attachment-size{flex:none;color:var(--muted);font-size:11px;font-variant-numeric:tabular-nums;white-space:nowrap}
.attachment-message{margin:0;padding:6px 12px;color:var(--muted);font-size:11px;line-height:1.55;overflow-wrap:anywhere}.attachment-selection+.attachment-message{padding-top:0}
.attachment-completed{border-top:1px solid var(--line)}.attachment-completed:first-child{border:0}.attachment-success{color:var(--green)}
.attachment-results{max-height:132px;overflow:auto}.attachment-results li{display:flex;align-items:center;gap:9px;padding:7px 0}.attachment-results li+li{border-top:1px solid var(--line)}
.attachment-result-info{display:grid;gap:3px;flex:1;min-width:0}.attachment-result-info>.attachment-name{font-size:12px;color:var(--text)}.attachment-result-info code{display:block;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;background:transparent;padding:0;font-size:10px;color:var(--muted)}
.insert-path{color:var(--amber)}.attachment-progress-file{display:flex;align-items:center;justify-content:space-between;gap:12px;margin:6px 0 8px;color:var(--text)}
.attachment-progress progress{display:block;width:100%;height:4px;overflow:hidden;border:0;border-radius:2px;background:var(--line);accent-color:var(--accent)}
.attachment-progress progress::-webkit-progress-bar{background:var(--line);border-radius:2px}.attachment-progress progress::-webkit-progress-value{background:var(--accent);border-radius:2px}.attachment-progress progress::-moz-progress-bar{background:var(--accent);border-radius:2px}
.attachment-progress p{margin:6px 0 0;font-size:11px;color:var(--muted)}
.terminal-sr-only{position:absolute;width:1px;height:1px;margin:-1px;padding:0;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}
@keyframes terminal-reveal{from{opacity:0}to{opacity:1}}
@media(max-width:900px){.terminal-tools{padding-inline:9px;gap:6px}.terminal-host :deep(.xterm){padding-left:10px}.terminal-menu{width:244px}.terminal-section-heading{flex-wrap:wrap;gap:5px}.attachment-selection-actions{margin-left:auto}.terminal-draft textarea{max-height:100px}}
@media(prefers-reduced-motion:reduce){.terminal-menu,.terminal-search,.terminal-draft{animation:none}.terminal-icon-button,.terminal-text-button,.terminal-primary-button{transition:none}}
</style>
