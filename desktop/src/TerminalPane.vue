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
const draft=ref('');
const showDraft=ref(false);
const hasSelection = ref(false);
const contextMenu = ref<HTMLDivElement>();
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
function theme() { term.options.theme = props.light ? { background: '#f9fafb', foreground: '#18202c', cursor: '#245dad', selectionBackground: '#b8d4fa' } : { background: '#14181f', foreground: '#dae0e8', cursor: '#e0b572', selectionBackground: '#384965' }; }
async function copy() { try { await navigator.clipboard.writeText(term.getSelection()); } catch (error) { message.value = errorMessage(error); } }
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
function menuAction(action: 'copy' | 'paste' | 'select' | 'clear') {
  closeMenu(true);
  if (action === 'copy') void copy();
  else if (action === 'paste') void paste();
  else if (action === 'select') term.selectAll();
  else term.clearSelection();
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
  term.loadAddon(fit); term.loadAddon(search); term.open(host.value!); theme(); fit.fit();
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
      <span class="connection-status" :class="{ connected }" role="status">{{ message }}</span>
      <button @click="reconnect">重新连接</button>
      <button :disabled="!hasSelection" :title="copyKeys" @click="copy">复制</button><button :title="pasteKeys" @click="paste">粘贴</button><button :disabled="uploadBusy" @click="chooseFiles">上传文件</button>
      <button aria-label="打开终端菜单" title="右键或 Shift F10" :aria-expanded="!!menuPosition" aria-haspopup="menu" @click="openMenu()">更多…</button>
      <button @click="showDraft=!showDraft">暂存输入</button>
      <form class="search" @submit.prevent="search.findNext(searchText)"><input v-model="searchText" aria-label="搜索终端输出" placeholder="搜索输出"><button>查找</button></form>
    </div>
    <div ref="host" class="terminal-host" :class="{ light }" @contextmenu.capture="context" @paste.capture="pasteFiles" @compositionstart.capture="composing=true" @compositionend.capture="compositionEnd"></div>
    <div v-if="menuPosition" ref="contextMenu" class="terminal-menu" role="menu" aria-label="终端菜单" :style="{ left: menuPosition.x+'px', top: menuPosition.y+'px' }" @keydown="menuKey">
      <button role="menuitem" :disabled="!hasSelection" @click="menuAction('copy')">复制所选文本 <kbd>{{ copyKeys }}</kbd></button>
      <button role="menuitem" @click="menuAction('paste')">粘贴文字或附件 <kbd>{{ pasteKeys }}</kbd></button>
      <button role="menuitem" @click="menuAction('select')">全选终端输出</button>
      <button role="menuitem" :disabled="!hasSelection" @click="menuAction('clear')">取消选择</button>
    </div>
    <div v-if="showDraft" class="terminal-draft"><textarea v-model="draft" aria-label="终端暂存输入" placeholder="在这里编写多行内容，再插入当前终端" maxlength="122880"></textarea><button :disabled="!connected||!draft" @click="insertDraft">插入终端</button><button @click="draft=''">清空</button></div>
    <div v-if="pendingFiles.length&&!uploadBusy" class="attachment-selection">
      <span>已选 {{ pendingFiles.length }} 个附件：{{ pendingFiles.map(f=>f.name).join('、') }}</span>
      <button @click="uploadFiles(pendingFiles)">确认上传附件</button><button @click="cancelUpload">取消选择</button>
    </div>
    <p v-if="attachmentMessage" class="attachment-message" role="status">{{ attachmentMessage }}</p>
    <ul v-if="uploadedFiles.length" class="attachment-results"><li v-for="file in uploadedFiles" :key="file.path"><code>{{ file.path }}</code><button :disabled="!connected" @click="insertAttachment(file.path)">插入路径</button></li></ul>
    <div v-if="uploadBusy" class="attachment-progress" role="status">
      <span>正在上传附件 {{ uploadCurrent }} / {{ uploadTotal }}：{{ uploadName }}</span>
      <progress :value="uploadCurrent" :max="uploadTotal"></progress>
      <span>当前文件已读取 {{ byteLabel(uploadBytes) }} / {{ byteLabel(uploadSize) }}（仍需服务器确认）</span>
      <button @click="cancelUpload">取消</button>
    </div>
  </section>
</template>

<style scoped>
.attachment-selection,.attachment-message,.attachment-results,.terminal-draft{padding:8px 12px;font-size:12px;margin:0;overflow-wrap:anywhere}.attachment-results{max-height:120px;overflow:auto;list-style:none}.attachment-results li{display:flex;gap:8px;align-items:center}.attachment-results code{flex:1}.terminal-draft{display:flex;gap:8px}.terminal-draft textarea{flex:1;min-height:70px;resize:vertical}.terminal-tools{flex-wrap:wrap}
.terminal-menu{position:fixed;z-index:1000;display:grid;min-width:210px;max-width:calc(100vw - 8px);padding:5px;border:1px solid var(--line);border-radius:6px;background:var(--panel);box-shadow:0 8px 30px #0005}
.terminal-menu button{display:flex;justify-content:space-between;gap:20px;border-color:transparent;text-align:left;font-size:12px;white-space:normal}
.terminal-menu kbd{font:inherit;color:var(--muted);white-space:nowrap}
</style>
