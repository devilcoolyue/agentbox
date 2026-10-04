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
import UiIcon from './UiIcon.vue';
import BrandMark from './BrandMark.vue';
import UiDialog from './UiDialog.vue';
import { listenForAttachmentDrops } from './attachment-drop';
import { version as desktopVersion } from '../package.json';
import { AppZoom, savedZoom, zoomPreference, zoomShortcut } from './app-zoom';

// Only non-secret preferences live in the renderer. Password is cleared after every attempt.
const server = ref(localStorage.getItem('agentbox.server') || '');
const username = ref(localStorage.getItem('agentbox.user') || '');
const password = ref('');
const loginMode=ref<'password'|'pair'>('password');
const pairCode=ref('');
const issuedPair=ref('');
const pairing=ref(false);
let pairTimer:ReturnType<typeof setTimeout>|undefined;
onBeforeUnmount(()=>clearTimeout(pairTimer));
async function issuePair(){
  if(pairing.value||!connection.value)return;
  const identity=connection.value;
  pairing.value=true;
  error.value='';
  try{const pair=await bridge.invoke<{code:string;expires_in:number}>('issue_pair');if(disposed||identity!==connection.value)return;issuedPair.value=pair.code;clearTimeout(pairTimer);pairTimer=setTimeout(()=>issuedPair.value='',pair.expires_in*1000);}
  catch(err){if(!disposed&&identity===connection.value)error.value=errorMessage(err);}
  finally{if(!disposed)pairing.value=false;}
}
const allowHttp = ref(false);
const remember = ref(true);
const busy = ref(false);
const error = ref('');
const connection = ref<Connection | null>(null);
const sessions = ref<Session[]>([]);
const refreshing = ref(false);
const search = ref('');
const visibleSessions = computed(() => sessions.value.filter(session => `${session.name} ${session.agent}`.toLowerCase().includes(search.value.trim().toLowerCase())));
type WorkspaceView = 'terminal'|'files'|'sync'|'recovery';
const view = ref<WorkspaceView>('terminal');
const settingsOpen = ref(false);
const settingsTab = ref<'appearance'|'connection'|'about'>('appearance');
const updateBusy = ref(false);
const showPassword = ref(false);
const copiedServer = ref(false);
let copyTimer: ReturnType<typeof setTimeout>|undefined;
let listGeneration = 0;
let disposed = false;
let stopDrops: (() => void)|undefined;
onMounted(async () => {
  try {
    const stop = await listenForAttachmentDrops(message => { if (!disposed) error.value = message; });
    if (disposed) stop(); else stopDrops = stop;
  } catch { if (!disposed) error.value = '文件拖放暂时不可用，请使用上传按钮。'; }
});
onBeforeUnmount(() => stopDrops?.());
const serverHost = computed(() => { try { return new URL(connection.value?.server || server.value).host; } catch { return connection.value?.server || '服务器'; } });
const platformName = /Mac/.test(navigator.platform) ? 'macOS' : /Win/.test(navigator.platform) ? 'Windows' : 'Desktop';
const tabs = computed(() => [
  {id:'terminal' as const, label:'终端', icon:'terminal'},
  {id:'files' as const, label:'文件', icon:'folder'},
  ...(connection.value?.capabilities?.features.sync === 1 ? [{id:'sync' as const, label:'同步', icon:'refresh'}] : []),
  ...(connection.value?.capabilities?.features.sync_recovery_inspect === 1 ? [{id:'recovery' as const, label:'恢复记录', icon:'history'}] : []),
]);
function openSettings(tab: 'appearance'|'connection'|'about' = 'appearance') { settingsTab.value = tab; settingsOpen.value = true; }
function agentLabel(agent: string) { return agent === 'claude' ? 'Claude Code' : agent === 'codex' ? 'Codex' : agent; }
function statusLabel(status: string) { return ({running:'运行中',stopped:'已停止',starting:'启动中',creating:'创建中',error:'异常',idle:'空闲',paused:'已暂停'} as Record<string,string>)[status] || status; }
async function copyServer() { try { await navigator.clipboard.writeText(connection.value?.server || server.value); copiedServer.value = true; clearTimeout(copyTimer); copyTimer = setTimeout(() => copiedServer.value = false, 1800); } catch { error.value = '复制失败，请选择地址后手动复制。'; } }
onBeforeUnmount(() => { disposed = true; listGeneration++; clearTimeout(copyTimer); });
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
type ThemeMode = 'system'|'light'|'dark';
const savedTheme = localStorage.getItem('agentbox.theme');
const themeMode = ref<ThemeMode>(savedTheme === 'dark' || savedTheme === 'light' ? savedTheme : 'system');
const colorScheme = window.matchMedia('(prefers-color-scheme: light)');
const systemLight = ref(colorScheme.matches);
const light = computed(() => themeMode.value === 'system' ? systemLight.value : themeMode.value === 'light');
const themeOptions: {value: ThemeMode; label: string; icon: string}[] = [{value:'system',label:'跟随系统',icon:'desktop'}, {value:'light',label:'浅色',icon:'sun'}, {value:'dark',label:'深色',icon:'moon'}];
function systemThemeChanged(event: MediaQueryListEvent) { systemLight.value = event.matches; }
colorScheme.addEventListener('change', systemThemeChanged);
onBeforeUnmount(() => colorScheme.removeEventListener('change', systemThemeChanged));
watch(fontSize, value => localStorage.setItem('agentbox.fontSize', String(value)));
watch(themeMode, value => localStorage.setItem('agentbox.theme', value));
watch(light, value => { document.documentElement.dataset.theme = value ? 'light' : 'dark'; }, { immediate: true });
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
    view.value = 'terminal'; search.value = '';
    await refresh();
  } catch (err) { error.value = errorMessage(err); }
  finally { password.value = ''; pairCode.value=''; busy.value = false; }
}
async function refresh() {
  if (refreshing.value || !connection.value) return;
  refreshing.value = true;
  error.value = '';
  const identity = connection.value;
  const generation = ++listGeneration;
  try {
    const result = await bridge.invoke<Session[]>('list_sessions');
    if (disposed || identity !== connection.value || generation !== listGeneration) return;
    sessions.value = result;
    if (selected.value) selected.value = result.find(session => session.id === selected.value?.id) || null;
    projectSpaces.value = projectSpaces.value.filter(space => result.some(session => session.id === space.id));
    syncSpaces.value = syncSpaces.value.filter(space => result.some(session => session.id === space.id));
  } catch (err) { if (!disposed && identity === connection.value && generation === listGeneration) error.value = errorMessage(err); }
  finally { if (!disposed && generation === listGeneration) refreshing.value = false; }
}
async function logout() {
  if(busy.value||updateBusy.value||pairing.value)return;
  listGeneration++; refreshing.value = false; busy.value = true; active.value = null; projectSpaces.value=[]; syncSpaces.value=[]; syncStatuses.value={}; issuedPair.value=''; clearTimeout(pairTimer); error.value = '';
  try { await bridge.invoke('disconnect'); connection.value = null; sessions.value = []; selected.value = null; settingsOpen.value = false; }
  catch (err) { error.value = errorMessage(err); }
  finally { busy.value = false; }
}
</script>

<template>
  <main class="desktop-app" :class="{ light, 'is-connected': connection }">
    <div v-if="!connection" class="login-layout">
      <form class="login-form" @submit.prevent="login()">
        <div class="login-brand"><BrandMark /><span class="edition-label">桌面客户端</span></div>
        <div class="login-heading"><h1>连接你的工作空间</h1><p>使用与网页版相同的账号登录。</p></div>
        <div class="login-method segmented" aria-label="登录方式">
          <button type="button" :disabled="busy" :class="{active:loginMode==='password'}" :aria-pressed="loginMode==='password'" @click="loginMode='password'">账号登录</button>
          <button type="button" :disabled="busy" :class="{active:loginMode==='pair'}" :aria-pressed="loginMode==='pair'" @click="loginMode='pair'">配对码登录</button>
        </div>
        <label class="field">服务器地址<span class="input-with-icon"><UiIcon name="globe" /><input v-model="server" type="url" required placeholder="https://agentbox.example.com" :disabled="busy" autocapitalize="none" spellcheck="false"></span></label>
        <template v-if="loginMode==='password'">
          <label class="field">账号<span class="input-with-icon"><UiIcon name="user" /><input v-model="username" autocomplete="username" required :disabled="busy" placeholder="输入账号" autocapitalize="none" spellcheck="false"></span></label>
          <label class="field">密码<span class="input-with-icon"><UiIcon name="lock" /><input v-model="password" aria-label="密码" :type="showPassword?'text':'password'" autocomplete="current-password" :disabled="busy" placeholder="输入密码"><button type="button" class="input-action icon-button" :aria-label="showPassword?'隐藏密码':'显示密码'" :aria-pressed="showPassword" @click="showPassword=!showPassword"><UiIcon :name="showPassword?'eye-off':'eye'" /></button></span></label>
        </template>
        <label v-else class="field">配对码<span class="input-with-icon"><UiIcon name="link" /><input v-model="pairCode" autocomplete="off" required maxlength="22" :disabled="busy" placeholder="输入另一台客户端生成的配对码"></span></label>
        <label class="checkbox"><input v-model="remember" type="checkbox">记住登录</label>
        <label v-if="server.trim().startsWith('http:')" class="checkbox http-notice"><input v-model="allowHttp" type="checkbox">允许明文 HTTP（仅用于可信网络）</label>
        <p v-if="error" class="error" role="alert"><UiIcon name="alert" />{{ error }}</p>
        <button class="primary login-submit" :disabled="busy || (loginMode==='password'?!password:!pairCode)"><span v-if="busy" class="spin" /><UiIcon v-else name="login" />{{ busy ? '正在连接…' : '登录' }}</button>
        <button v-if="loginMode==='password'" class="ghost saved-login" type="button" :disabled="busy || !server || !username" @click="login(true)">使用已保存的登录</button>
        <p class="login-footnote"><UiIcon name="shield" :size="13" />登录凭证安全保存在系统凭证库中</p>
      </form>
      <button class="login-preferences ghost" type="button" @click="openSettings()"><UiIcon name="sliders" />外观与设置</button>
    </div>
    <div v-else class="desktop-shell">
      <aside class="sidebar">
        <div class="side-head"><button class="side-brand" aria-label="返回工作空间首页" @click="selected=null"><BrandMark compact /></button><span class="edition-label">桌面</span></div>
        <button class="server-switch" title="连接设置" @click="openSettings('connection')"><span class="server-symbol"><UiIcon name="globe" :size="17" /></span><span class="server-identity"><strong>{{ serverHost }}</strong><small>当前服务器</small></span><UiIcon name="chevron-right" :size="14" /></button>
        <div class="side-section-head"><span>工作空间 <span class="side-count">{{ sessions.length }}</span></span><button class="icon-button" :disabled="refreshing" aria-label="刷新工作空间" title="刷新工作空间" @click="refresh"><UiIcon name="refresh" :class="{rotating:refreshing}" :size="14" /></button></div>
        <label class="workspace-search"><UiIcon name="search" :size="14" /><input v-model="search" aria-label="搜索工作空间" placeholder="搜索工作空间" type="search"></label>
        <nav class="workspace-list" aria-label="工作空间">
          <p v-if="!visibleSessions.length" class="side-empty">{{ sessions.length?'没有匹配的工作空间':'还没有工作空间' }}</p>
          <button v-for="session in visibleSessions" :key="session.id" :title="session.name" class="workspace-item" :class="{selected:selected?.id===session.id}" :aria-current="selected?.id===session.id?'page':undefined" @click="selected=session">
            <UiIcon :name="session.agent==='claude'?'claude':session.agent==='codex'?'codex':'workspace'" :size="19" />
            <span class="workspace-identity"><strong>{{ session.name }}</strong><small>{{ agentLabel(session.agent) }}<span v-if="syncStatuses[session.id]"> · {{ syncStatuses[session.id] }}</span></small></span>
            <span class="status-dot" :class="session.status" :title="statusLabel(session.status)" :aria-label="statusLabel(session.status)" />
          </button>
        </nav>
        <div class="side-foot">
          <div class="theme-switch" role="group" aria-label="外观主题"><button v-for="option in themeOptions" :key="option.value" class="icon-button" :class="{active:themeMode===option.value}" :title="option.label" :aria-label="option.label" :aria-pressed="themeMode===option.value" @click="themeMode=option.value"><UiIcon :name="option.icon" :size="16" /></button></div>
          <button class="side-item" @click="openSettings()"><UiIcon name="sliders" /><span>客户端设置</span><UiIcon name="chevron-right" :size="14" /></button>
          <div class="side-account"><span class="avatar">{{ connection.user.slice(0,1).toUpperCase() }}</span><span><strong>{{ connection.user }}</strong><small>{{ connection.role==='admin'?'管理员':'已登录' }}</small></span><button class="icon-button" aria-label="账号与连接设置" title="账号与连接设置" @click="openSettings('connection')"><UiIcon name="more" /></button></div>
        </div>
      </aside>
      <section class="workspace-content">
        <header class="workspace-header"><div><span class="eyebrow">{{ selected?agentLabel(selected.agent):'AGENTBOX DESKTOP' }}</span><h1>{{ selected?.name || '工作空间' }}</h1></div><span v-if="selected" class="workspace-status" :class="selected.status"><span class="status-dot" :class="selected.status" />{{ statusLabel(selected.status) }}</span><span v-if="selected&&!projectMode" class="mode-label" title="此服务器使用与网页版共享的终端">兼容模式</span></header>
        <p v-if="error&&!settingsOpen" class="error top-error" role="alert"><UiIcon name="alert" />{{ error }}</p>
        <nav v-if="selected" class="workspace-tabs" aria-label="工作区功能"><button v-for="tab in tabs" :key="tab.id" :class="{active:view===tab.id}" :aria-current="view===tab.id?'page':undefined" @click="view=tab.id"><UiIcon :name="tab.icon" :size="16" />{{ tab.label }}<span v-if="tab.id==='sync'&&syncStatuses[selected.id]" class="tab-status-dot" /></button></nav>
        <div v-if="!selected" class="workspace-welcome"><div class="welcome-mark"><BrandMark icon-only /></div><h2>{{ sessions.length?'选择一个工作空间':'还没有工作空间' }}</h2><p>{{ sessions.length?'从左侧选择空间，继续终端和文件操作。':'在网页版创建工作空间后，刷新列表即可连接。' }}</p><button class="ghost" @click="refresh"><UiIcon name="refresh" />刷新列表</button></div>
        <div v-show="selected&&view==='terminal'" class="terminal-workspace">
          <ProjectWorkspace v-for="space in projectSpaces" v-show="selected?.id===space.id" :key="space.id" :session="space" :font-size="fontSize" :light="light" />
          <div v-if="!projectMode&&selected&&selected.id!==active?.id" class="open-workspace"><span class="empty-symbol"><UiIcon name="terminal" :size="30" /></span><h2>连接此工作空间</h2><p>与网页版共享同一终端。连接可能接管网页中的终端；断开不会停止远端任务。</p><button class="primary" @click="active=selected"><UiIcon name="terminal" />连接共享终端</button></div>
          <TerminalPane v-if="active" v-show="selected?.id===active.id" :key="active.id" :session="active.id" :font-size="fontSize" :light="light" />
        </div>
        <FileBrowser v-if="selected" v-show="view==='files'" :key="'files-'+selected.id" :session="selected.id" :visible="view==='files'&&!settingsOpen" />
        <div v-show="selected&&view==='sync'" class="workspace-panel sync-panel"><div class="panel-heading"><h2>项目同步</h2><p>绑定本地目录，预览变更后同步到服务器。</p></div><SyncWorkspace v-for="space in syncSpaces" v-show="selected?.id===space.id" :key="space.id" :session="space" @sync-status="syncStatuses[space.id]=$event" /></div>
        <div v-show="selected&&view==='recovery'" class="workspace-panel recovery-panel"><RemoteRecovery v-if="selected&&connection.capabilities?.features.sync_recovery_inspect===1&&connection.capabilities.server_id" :key="'recovery-'+selected.id" :session="selected.id" :server-id="connection.capabilities.server_id" :can-clean="connection.capabilities.features.sync_recovery_gc===1" expanded /></div>
        <p class="sidebar-note sr-only">{{ projectMode?'项目连接模式':'基础连接模式' }}。{{ connection.capabilities?.features.sync===1?'同步可用。':'文件同步尚未启用。' }}</p>
      </section>
    </div>
    <footer class="app-statusbar"><span class="platform-label"><UiIcon name="desktop" :size="13" />{{ platformName }}<span class="statusbar-divider" />{{ connection?'已登录':'未连接' }}</span><span v-if="connection&&selected" class="statusbar-workspace">{{ selected.name }}</span><div class="app-zoom" role="group" aria-label="界面缩放" :title="zoomKeys"><button class="icon-button" :disabled="zoomPercent<=80" aria-label="缩小界面" @click="zoom.change('out')"><UiIcon name="zoom-out" :size="13" /></button><button class="zoom-reset ghost" aria-label="恢复界面缩放至 100%" @click="zoom.change('reset')">{{ zoomPercent }}%</button><button class="icon-button" :disabled="zoomPercent>=200" aria-label="放大界面" @click="zoom.change('in')"><UiIcon name="zoom-in" :size="13" /></button></div><button class="version-button ghost" title="版本与更新" @click="openSettings('about')">v{{ desktopVersion }}</button><p v-if="zoomError" class="error" role="alert">{{ zoomError }}</p></footer>
    <UiDialog :open="settingsOpen" title="客户端设置" :busy="busy||updateBusy||pairing" @close="settingsOpen=false">
      <nav class="settings-tabs" aria-label="设置分类"><button :class="{active:settingsTab==='appearance'}" @click="settingsTab='appearance'">外观</button><button v-if="connection" :class="{active:settingsTab==='connection'}" @click="settingsTab='connection'">连接</button><button :class="{active:settingsTab==='about'}" @click="settingsTab='about'">关于与更新</button></nav>
      <section v-show="settingsTab==='appearance'" class="settings-section"><div class="setting-row"><div><h3>主题</h3><p>与网页版一致的深浅配色。</p></div><div class="theme-options"><button v-for="option in themeOptions" :key="option.value" :class="{active:themeMode===option.value}" :aria-pressed="themeMode===option.value" @click="themeMode=option.value"><UiIcon :name="option.icon" />{{ option.label }}</button></div></div><div class="setting-row"><div><h3>终端字号</h3><p>终端保持深色，确保命令输出清晰。</p></div><label class="font-control"><input v-model.number="fontSize" aria-label="终端字号" type="range" min="10" max="24"><span>{{ fontSize }} px</span></label></div><div class="setting-row"><div><h3>界面缩放</h3><p>{{ zoomKeys }} · 恢复默认比例使用 0</p></div><div class="zoom-controls"><button class="icon-button" :disabled="zoomPercent<=80" aria-label="设置中缩小界面" @click="zoom.change('out')"><UiIcon name="zoom-out" /></button><span>{{ zoomPercent }}%</span><button class="icon-button" :disabled="zoomPercent>=200" aria-label="设置中放大界面" @click="zoom.change('in')"><UiIcon name="zoom-in" /></button></div></div></section>
      <section v-show="settingsTab==='connection'" class="settings-section"><template v-if="connection"><div class="connection-summary"><span class="server-symbol"><UiIcon name="globe" :size="22" /></span><div><h3>{{ serverHost }}</h3><p>{{ connection.user }} · {{ connection.role==='admin'?'管理员':'用户' }}</p></div></div><label class="field">服务器地址<span class="copy-field"><input :value="connection.server" readonly aria-label="当前服务器地址"><button class="icon-button" :aria-label="copiedServer?'地址已复制':'复制服务器地址'" @click="copyServer"><UiIcon :name="copiedServer?'check':'copy'" /></button></span></label><div v-if="connection.capabilities?.features.pairing===1" class="setting-row"><div><h3>配对另一台设备</h3><p>生成一个 10 分钟内有效的单次配对码。</p></div><button :disabled="busy||pairing" @click="issuePair"><UiIcon name="link" />生成配对码</button></div><div v-if="issuedPair" class="pair-bar"><input :value="issuedPair" readonly aria-label="配对码" @focus="($event.target as HTMLInputElement).select()"><button class="ghost" @click="issuedPair=''">收起</button></div><LocalInspection /><details class="backend-details"><summary><UiIcon name="info" />连接诊断</summary><p>{{ backend }}</p><p>{{ projectMode?'支持独立项目终端':'使用网页版共享终端' }} · {{ connection.capabilities?.features.sync===1?'同步已启用':'同步未启用' }}</p><button :disabled="busy" @click="checkBackend"><UiIcon name="refresh" />重新检查</button></details><p v-if="error" class="error" role="alert">{{ error }}</p><div class="settings-signout"><button class="ghost danger" :disabled="busy||updateBusy||pairing" @click="logout"><UiIcon name="logout" />退出登录</button></div></template></section>
      <section v-show="settingsTab==='about'" class="settings-section"><div class="about-brand"><BrandMark compact /><span>桌面客户端 · {{ platformName }}</span></div><DesktopUpdate @busy="updateBusy=$event" /></section>
    </UiDialog>
  </main>
</template>
