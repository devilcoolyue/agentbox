<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { bridge, errorMessage, type Connection, type Session } from './bridge';
import TerminalPane from './TerminalPane.vue';
import ProjectWorkspace from './ProjectWorkspace.vue';
import LocalInspection from './LocalInspection.vue';
import SyncWorkspace from './SyncWorkspace.vue';
import FileBrowser from './FileBrowser.vue';
import RemoteRecovery from './RemoteRecovery.vue';
import DesktopUpdate from './DesktopUpdate.vue';
import { AppZoom, savedZoom, zoomPreference, zoomShortcut } from './app-zoom';

// Only non-secret preferences live in the renderer. Password is cleared after every attempt.
const server = ref(localStorage.getItem('agentbox.server') || '');
const username = ref(localStorage.getItem('agentbox.user') || '');
const password = ref('');
const loginMode=ref<'password'|'pair'>('password');
const pairCode=ref('');
const issuedPair=ref('');
let pairTimer:ReturnType<typeof setTimeout>|undefined;
onBeforeUnmount(()=>clearTimeout(pairTimer));
async function issuePair(){
  error.value='';
  try{const pair=await bridge.invoke<{code:string;expires_in:number}>('issue_pair');issuedPair.value=pair.code;clearTimeout(pairTimer);pairTimer=setTimeout(()=>issuedPair.value='',pair.expires_in*1000);}
  catch(err){error.value=errorMessage(err);}
}
const allowHttp = ref(false);
const remember = ref(true);
const busy = ref(false);
const error = ref('');
const connection = ref<Connection | null>(null);
const sessions = ref<Session[]>([]);
const selected = ref<Session | null>(null);
const active = ref<Session | null>(null);
const projectSpaces=ref<Session[]>([]);
const syncSpaces=ref<Session[]>([]);
const syncStatuses=ref<Record<string,string>>({});
watch(selected,session=>{if(connection.value?.capabilities?.features.sync===1&&session&&!syncSpaces.value.some(s=>s.id===session.id))syncSpaces.value.push(session);});
const projectMode=computed(()=>connection.value?.capabilities?.features.project_terminals===1);
watch(selected,session=>{if(projectMode.value&&session&&!projectSpaces.value.some(s=>s.id===session.id))projectSpaces.value.push(session);});
const backend = ref('正在检查后台…');
async function checkBackend() {
  try { backend.value = await bridge.invoke<string>('backend_status'); }
  catch (err) { backend.value = errorMessage(err); }
}
onMounted(checkBackend);
const fontSize = ref(Math.max(10, Math.min(24, Number(localStorage.getItem('agentbox.fontSize')) || 14)));
const light = ref(localStorage.getItem('agentbox.theme') === 'light');
watch(fontSize, value => localStorage.setItem('agentbox.fontSize', String(value)));
watch(light, value => localStorage.setItem('agentbox.theme', value ? 'light' : 'dark'));
const zoomPercent = ref(100);
const zoomError = ref('');
const mac = /Mac/.test(navigator.platform);
const zoomKeys = mac ? '⌘ + / − / 0' : 'Ctrl + / − / 0';
let composing = false;
const zoom = new AppZoom(
  percent => bridge.invoke('desktop_set_zoom', { percent }),
  percent => {
    zoomPercent.value = percent; zoomError.value = '';
    localStorage.setItem(zoomPreference, String(percent));
    // Native page zoom resizes layout; notify terminals even on WebViews whose
    // ResizeObserver delivery is delayed until the next frame.
    window.dispatchEvent(new Event('agentbox-zoom'));
  },
  error => { zoomError.value = errorMessage(error); },
);
function zoomKey(event: KeyboardEvent) {
  const action = zoomShortcut(event, mac, composing);
  if (!action) return;
  event.preventDefault(); event.stopImmediatePropagation();
  if (event.type === 'keydown') void zoom.change(action);
}
function compositionStart() { composing = true; }
function compositionEnd() { composing = false; }
onMounted(() => {
  window.addEventListener('keydown', zoomKey, true);
  window.addEventListener('keyup', zoomKey, true);
  window.addEventListener('compositionstart', compositionStart, true);
  window.addEventListener('compositionend', compositionEnd, true);
  window.addEventListener('blur', compositionEnd);
  void zoom.set(savedZoom(localStorage.getItem(zoomPreference)));
});
onBeforeUnmount(() => {
  window.removeEventListener('keydown', zoomKey, true);
  window.removeEventListener('keyup', zoomKey, true);
  window.removeEventListener('compositionstart', compositionStart, true);
  window.removeEventListener('compositionend', compositionEnd, true);
  window.removeEventListener('blur', compositionEnd);
});

async function login(restore = false) {
  if (busy.value) return;
  busy.value = true; error.value = '';
  try {
    connection.value = loginMode.value==='pair' ? await bridge.invoke<Connection>('connect_pair',{server:server.value,code:pairCode.value,allowHttp:allowHttp.value,remember:remember.value}) : await bridge.invoke<Connection>('connect', { server: server.value, username: username.value, password: restore ? null : password.value, allowHttp: allowHttp.value, remember: remember.value });
    localStorage.setItem('agentbox.server', connection.value.server);
    localStorage.setItem('agentbox.user', connection.value.user);
    username.value=connection.value.user;server.value=connection.value.server;
    await refresh();
  } catch (err) { error.value = errorMessage(err); }
  finally { password.value = ''; pairCode.value=''; busy.value = false; }
}
async function refresh() {
  error.value = '';
  try { sessions.value = await bridge.invoke<Session[]>('list_sessions'); }
  catch (err) { error.value = errorMessage(err); }
}
async function logout() {
  busy.value = true; active.value = null; projectSpaces.value=[]; syncSpaces.value=[]; syncStatuses.value={}; issuedPair.value=''; clearTimeout(pairTimer); error.value = '';
  try { await bridge.invoke('disconnect'); connection.value = null; sessions.value = []; selected.value = null; }
  catch (err) { error.value = errorMessage(err); }
  finally { busy.value = false; }
}
</script>

<template>
  <main :class="{ light }">
    <div v-if="!connection" class="login-layout">
      <section class="login-intro"><div class="wordmark">agentbox<span>desktop</span></div><h1>你的远程工作空间，<br>就在桌面。</h1><p>连接已有 Agentbox 服务器，继续你的 Claude Code 或 Codex 工作。</p><p class="muted">Windows · macOS / 开发预览</p></section>
      <form class="login-form" @submit.prevent="login()">
        <h2>连接服务器</h2>
        <div class="login-method"><button type="button" :disabled="busy" :class="{primary:loginMode==='password'}" @click="loginMode='password'">账号登录</button><button type="button" :disabled="busy" :class="{primary:loginMode==='pair'}" @click="loginMode='pair'">配对码登录</button></div>
        <label>服务器地址<input v-model="server" type="url" required placeholder="https://agentbox.example.com" :disabled="busy"></label>
        <label v-if="loginMode==='password'">用户名<input v-model="username" autocomplete="username" required :disabled="busy"></label>
        <label v-if="loginMode==='password'">密码<input v-model="password" type="password" autocomplete="current-password" :disabled="busy"></label>
        <label v-else>配对码<input v-model="pairCode" autocomplete="off" required maxlength="22" :disabled="busy" placeholder="从已登录的客户端生成"></label>
        <label class="checkbox"><input v-model="remember" type="checkbox">在系统凭证库中记住登录</label>
        <label v-if="server.trim().startsWith('http:')" class="checkbox"><input v-model="allowHttp" type="checkbox">允许明文 HTTP（仅用于可信网络）</label>
        <p v-if="error" class="error" role="alert">{{ error }}</p>
        <button class="primary" :disabled="busy || (loginMode==='password'?!password:!pairCode)">{{ busy ? '正在连接…' : '登录' }}</button>
        <button v-if="loginMode==='password'" type="button" :disabled="busy || !server || !username" @click="login(true)">使用已保存的登录</button>
      </form>
    </div>
    <template v-else>
      <header><div class="wordmark">agentbox<span>desktop</span></div><span class="server-name">{{ connection.server }} · {{ connection.user }}</span><label class="font-size">字号 <input v-model.number="fontSize" type="range" min="10" max="24"></label><button @click="light = !light">{{ light ? '深色' : '浅色' }}</button><button :disabled="busy" @click="logout">退出登录</button></header>
      <p v-if="error" class="error top-error" role="alert">{{ error }}</p>
      <div v-if="connection.capabilities?.features.pairing===1" class="pair-bar"><button @click="issuePair">配对另一设备</button><template v-if="issuedPair"><span>10 分钟内有效、仅可使用一次：</span><input :value="issuedPair" readonly aria-label="配对码" @focus="($event.target as HTMLInputElement).select()"><button @click="issuedPair=''">收起</button></template></div>
      <div class="workspace-layout">
        <aside><div class="sidebar-title"><h2>工作空间</h2><button @click="refresh">刷新</button></div><p v-if="!sessions.length" class="muted">暂无空间，请在网页创建。</p><button v-for="session in sessions" :key="session.id" class="workspace-item" :class="{ selected: selected?.id === session.id }" @click="selected = session"><strong>{{ session.name }}</strong><span>{{ session.agent }} · {{ session.status }}<span v-if="syncStatuses[session.id]"> · {{ syncStatuses[session.id] }}</span></span></button><p class="sidebar-note">{{ projectMode?'项目连接模式':'基础连接模式' }}<br>{{ projectMode?'可使用独立 AI / Shell 终端。':'与网页共享 main 终端。' }}{{ connection.capabilities?.features.sync===1?'':'文件同步尚未启用。' }}</p><details class="backend-details"><summary>后台状态</summary><p>{{ backend }}</p><button @click="checkBackend">重新检查</button></details><LocalInspection /><FileBrowser v-if="selected" :key="'files-'+selected.id" :session="selected.id" /><RemoteRecovery v-if="selected && connection.capabilities?.features.sync_recovery_inspect===1 && connection.capabilities.server_id" :key="'recovery-'+selected.id" :session="selected.id" :server-id="connection.capabilities.server_id" :can-clean="connection.capabilities.features.sync_recovery_gc===1" /><SyncWorkspace v-for="space in syncSpaces" v-show="selected?.id===space.id" :key="space.id" :session="space" @sync-status="syncStatuses[space.id]=$event" /></aside>
        <div class="workspace-content">
          <ProjectWorkspace v-for="space in projectSpaces" v-show="selected?.id===space.id" :key="space.id" :session="space" :font-size="fontSize" :light="light" />
          <div v-if="!projectMode && selected && selected.id !== active?.id" class="open-workspace"><h2>{{ selected.name }}</h2><p>此终端与网页共享同一个会话。连接后可能接管已打开的网页终端；断开连接不会停止远端任务。</p><button class="primary" @click="active = selected">连接共享终端</button></div>
          <div v-else-if="!active && !selected" class="empty"><h2>选择一个工作空间</h2><p>你的文件和运行环境仍保存在服务器上。</p></div>
          <TerminalPane v-if="active" v-show="selected?.id === active.id" :key="active.id" :session="active.id" :font-size="fontSize" :light="light" />
        </div>
      </div>
    </template>
    <footer class="app-preferences">
      <div class="app-zoom" role="group" aria-label="界面缩放" :title="zoomKeys">
        <span>界面缩放</span>
        <button :disabled="zoomPercent <= 80" aria-label="缩小界面" @click="zoom.change('out')">−</button>
        <button class="zoom-reset" aria-label="恢复界面缩放至 100%" @click="zoom.change('reset')">{{ zoomPercent }}%</button>
        <button :disabled="zoomPercent >= 200" aria-label="放大界面" @click="zoom.change('in')">+</button>
      </div>
      <p v-if="zoomError" class="error" role="alert">{{ zoomError }}</p>
      <DesktopUpdate />
    </footer>
  </main>
</template>
