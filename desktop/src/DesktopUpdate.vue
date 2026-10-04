<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue';
import { version as bundledVersion } from '../package.json';
import { bridge, errorMessage } from './bridge';
import UiIcon from './UiIcon.vue';

interface UpdateInfo { configured: boolean; current: string; version: string|null; notes: string|null }
const emit = defineEmits<{(event: 'busy', value: boolean): void}>();
const info = ref<UpdateInfo|null>(null);
const busy = ref(false);
const installing = ref(false);
const message = ref('');
const failed = ref(false);
const confirm = ref(false);
const confirmationPanel = ref<HTMLElement>();
const installButton = ref<HTMLButtonElement>();
const automatic = ref(localStorage.getItem('agentbox.checkDesktopUpdates') === 'true');
const currentVersion = computed(() => info.value?.current || bundledVersion);
let alive = true;

async function showConfirmation() {
  if (busy.value || !info.value?.version) return;
  confirm.value = true;
  await nextTick();
  if (alive && confirm.value) confirmationPanel.value?.querySelector<HTMLButtonElement>('button')?.focus();
}
async function dismissConfirmation() {
  if (busy.value) return;
  confirm.value = false;
  await nextTick();
  if (alive) installButton.value?.focus();
}

async function check() {
  if (busy.value || !alive) return;
  busy.value = true; message.value = ''; failed.value = false; confirm.value = false;
  // A failed new check must not leave an older install candidate actionable.
  info.value = null;
  try {
    const result = await bridge.invoke<UpdateInfo>('desktop_update_check');
    if (!alive) return;
    info.value = result;
    if (!result.configured) message.value = '开发预览版请使用新的安装包更新。';
    else if (!result.version) message.value = '当前已是最新版本。';
  } catch (error) {
    if (alive) { message.value = errorMessage(error); failed.value = true; }
  } finally { if (alive) busy.value = false; }
}
async function install() {
  const version = info.value?.version;
  if (!version || !info.value?.configured || !confirm.value || busy.value || !alive) return;
  busy.value = true; installing.value = true; failed.value = false;
  message.value = '正在下载更新，完成后会安装并重启…';
  emit('busy', true);
  try {
    await bridge.invoke('desktop_update_install', {version});
    if (alive) {
      message.value = '更新已安装，正在重新启动…';
      confirm.value = false;
      if (info.value) info.value.version = null;
    }
  } catch (error) {
    if (alive) {
      message.value = errorMessage(error); failed.value = true; confirm.value = false;
      // The native candidate is consumed for an installation attempt; the next
      // action must check the feed again rather than reuse a failed candidate.
      if (info.value) info.value.version = null;
    }
  } finally {
    emit('busy', false);
    if (alive) { busy.value = false; installing.value = false; }
  }
}
function preference() { localStorage.setItem('agentbox.checkDesktopUpdates', String(automatic.value)); }
onMounted(() => { if (automatic.value) void check(); });
onBeforeUnmount(() => { alive = false; });
</script>

<template>
  <section class="desktop-update" aria-label="版本与更新">
    <div class="update-row">
      <div class="update-version"><h3>当前版本</h3><span>v{{ currentVersion }}</span><small v-if="info && !info.configured">开发预览版</small></div>
      <button :disabled="busy" @click="check"><UiIcon name="refresh" :size="15" :class="{'is-checking': busy && !installing}" />{{ busy && !installing ? '正在检查…' : '检查桌面更新' }}</button>
    </div>
    <label class="update-row update-auto"><span><strong>启动时检查更新</strong><small>打开客户端时检查可用版本。</small></span><input v-model="automatic" type="checkbox" :disabled="busy" @change="preference"></label>
    <div v-if="info?.configured && info.version" class="update-available"><div><UiIcon name="download" :size="16" /><strong>发现新版本 v{{ info.version }}</strong></div><button v-if="!confirm" ref="installButton" class="primary" :disabled="busy" @click="showConfirmation">安装此桌面更新…</button></div>
    <details v-if="info?.notes" class="update-notes"><summary>更新说明<UiIcon name="chevron" :size="13" /></summary><pre>{{ info.notes }}</pre></details>
    <p v-if="message" class="update-message" :class="{error: failed}" :role="failed ? 'alert' : 'status'"><UiIcon :name="failed ? 'info' : installing ? 'download' : info?.configured && !info.version ? 'check' : 'info'" :size="15" /><span>{{ message }}</span></p>
    <section v-if="confirm" ref="confirmationPanel" class="update-confirm" role="alertdialog" aria-label="确认安装桌面更新">
      <h3>安装 v{{ info?.version }} 并重启？</h3>
      <p>客户端连接会暂时断开，远端任务继续运行。登录和设置会保留。</p>
      <p>开始前请停止同步与文件传输。</p>
      <div><button :disabled="busy" @click="dismissConfirmation">返回</button><button class="primary" :disabled="busy" @click="install"><UiIcon name="download" :size="15" />{{ installing ? '正在安装…' : '确认安装并重启' }}</button></div>
    </section>
  </section>
</template>

<style scoped>
.desktop-update{padding:0;font-size:12px;color:var(--text)}
.update-row{display:flex;align-items:center;justify-content:space-between;gap:18px;padding:16px 0;border-bottom:1px solid var(--line-soft)}
.update-version{display:flex;align-items:center;gap:9px;flex-wrap:wrap;min-width:0}.update-version h3{margin:0;font-size:13px;font-weight:500}.update-version>span{font:12px var(--mono);color:var(--muted)}.update-version small{font-size:11px;color:var(--muted)}
.desktop-update button{display:inline-flex;align-items:center;justify-content:center;gap:6px;min-height:33px;padding:5px 10px;font-size:12px;flex:none}
.update-auto{cursor:pointer}.update-auto>span{display:flex;flex-direction:column;gap:5px}.update-auto strong{font-size:13px;font-weight:500}.update-auto small{font-size:11px;color:var(--muted)}.update-auto input{width:15px;height:15px;flex:none;margin:0;accent-color:var(--accent)}
.update-available{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:16px 0 5px}.update-available>div{display:flex;align-items:center;gap:7px;color:var(--amber)}.update-available strong{font-size:12px;font-weight:500}
.update-notes{margin-top:10px}.update-notes summary{display:flex;align-items:center;gap:6px;list-style:none;cursor:pointer;color:var(--muted);font-size:12px}.update-notes summary::-webkit-details-marker{display:none}.update-notes summary>svg{transform:rotate(-90deg)}.update-notes[open] summary>svg{transform:none}.update-notes pre{margin:10px 0 0;max-height:160px;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;color:var(--muted);font:inherit;line-height:1.7}
.update-message{display:flex;align-items:flex-start;gap:7px;margin:14px 0 0;color:var(--muted);font-size:12px;line-height:1.7}.update-message>svg{flex:none;margin-top:3px}.update-message.error{color:var(--red)}
.update-confirm{margin-top:16px;padding:16px;border:1px solid var(--line);border-radius:8px;background:var(--bg)}.update-confirm h3{margin:0 0 9px;font-size:13px;font-weight:600}.update-confirm p{margin:6px 0;color:var(--muted);font-size:12px;line-height:1.7}.update-confirm>div{display:flex;align-items:center;justify-content:flex-end;gap:8px;margin-top:15px}
.is-checking{animation:update-checking .9s linear infinite}@keyframes update-checking{to{transform:rotate(360deg)}}
@media(max-width:480px){.update-row,.update-available{gap:10px}.update-version{gap:5px}.update-available{align-items:flex-start;flex-direction:column}.update-confirm{padding:13px}}
@media(prefers-reduced-motion:reduce){.is-checking{animation:none}}
</style>
