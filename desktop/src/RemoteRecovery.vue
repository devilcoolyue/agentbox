<script setup lang="ts">
import UiIcon from './UiIcon.vue';
import { computed, onBeforeUnmount, ref } from 'vue';
import { errorMessage } from './bridge';
import { SyncTask, byteLabel } from './sync-task';
import { comparisonLabels, recoveryStatusLabel, type OrphanPage, type OrphanReview, type RemoteRecoveryStatus } from './orphan-recovery';

const props = defineProps<{session: string; serverId: string; canClean: boolean; expanded?: boolean}>();
const page = ref<OrphanPage|null>(null);
const review = ref<OrphanReview|null>(null);
const busy = ref(false);
const message = ref('');
const device = ref('');
const appliedDevice = ref('');
const cursors = ref<string[]>(['']);
const phrase = ref('');
const task = new SyncTask(() => {});
let alive = true;
onBeforeUnmount(() => { alive = false; task.close(); });
const confirmation = '永久清理原内容';
const canRetire = computed(() => props.canClean && review.value?.can_retire && phrase.value === confirmation && !busy.value);
function operation(status: RemoteRecoveryStatus) {
  return {workspace: props.session, server_id: props.serverId, device: status.device, operation_id: status.operation.operation_id};
}
async function load(cursor: string, reset = false) {
  if (busy.value) return;
  busy.value = true; message.value = ''; review.value = null; phrase.value = '';
  const selectedDevice = reset ? device.value.trim() : appliedDevice.value;
  try {
    const result = await task.run<{orphan_page: OrphanPage}>('sync_orphan_list', {
      query: {workspace: props.session, server_id: props.serverId, device: selectedDevice, cursor},
    });
    if (alive) {
      page.value = result.orphan_page; appliedDevice.value = selectedDevice;
      if (reset) cursors.value = [''];
      else if (cursor !== cursors.value[cursors.value.length - 1]) cursors.value.push(cursor);
    }
  } catch (error) { if (alive) { page.value = null; message.value = errorMessage(error); } }
  finally { if (alive) busy.value = false; }
}
async function previous() {
  if (busy.value || cursors.value.length < 2) return;
  cursors.value.pop(); await load(cursors.value[cursors.value.length - 1]!);
}
async function inspect(status: RemoteRecoveryStatus) {
  if (busy.value) return;
  busy.value = true; review.value = null; phrase.value = ''; message.value = '';
  try {
    const result = await task.run<{orphan_review: OrphanReview}>('sync_orphan_review', {operation: operation(status)});
    if (alive) review.value = result.orphan_review;
  } catch (error) { if (alive) message.value = errorMessage(error); }
  finally { if (alive) busy.value = false; }
}
async function exportBefore() {
  const shown = review.value; if (!shown || busy.value) return;
  busy.value = true; message.value = '';
  try {
    const result = await task.run<{filename: string}|null>('sync_orphan_export', {operation: operation(shown.status)});
    if (alive && result) message.value = `原内容已导出为 ${result.filename}`;
  } catch (error) { if (alive) message.value = errorMessage(error); }
  finally { if (alive) busy.value = false; }
}
async function retire() {
  const shown = review.value; if (!shown || !canRetire.value) return;
  busy.value = true; message.value = ''; phrase.value = '';
  // Clear the confirmation even on a lost response; the next action must review
  // a fresh receipt. No automatic retry of a destructive operation.
  review.value = null;
  try {
    const result = await task.run<{orphan_review: OrphanReview}>('sync_orphan_retire', {
      operation: operation(shown.status), confirmation: shown.digest,
    });
    if (alive) {
      review.value = result.orphan_review;
      const item = page.value?.items.find(item => item.id === shown.status.operation.operation_id);
      if (item) item.status = result.orphan_review.status;
      message.value = '服务器原内容已清理，执行收据保留。当前文件和同步基线没有改变。';
    }
  } catch (error) {
    if (alive) message.value = `${errorMessage(error)}。清理可能已部分完成，请重新核对该记录后续做。`;
  } finally { if (alive) busy.value = false; }
}
async function cancel() {
  try { await task.cancel(); } catch (error) { if (alive) message.value = errorMessage(error); }
}
</script>

<template>
  <details class="remote-recovery" :open="expanded">
    <summary class="recovery-heading">
      <span class="recovery-title"><UiIcon name="history" :size="20" /><strong>服务器恢复记录</strong></span>
      <UiIcon name="chevron-right" class="recovery-chevron" />
    </summary>
    <div class="recovery-body">
      <p class="recovery-intro">查找变更前的文件，核对后导出。本机同步历史丢失时，也可以从这里查找。</p>
      <form class="recovery-toolbar" @submit.prevent="load('', true)">
        <label><span>设备 ID <small>可选</small></span><input v-model="device" :disabled="busy" maxlength="128" autocomplete="off" placeholder="留空查看全部设备"></label>
        <button :disabled="busy"><UiIcon name="refresh" />读取记录</button>
        <button v-if="busy" type="button" class="quiet" @click="cancel"><UiIcon name="close" />取消操作</button>
      </form>
      <p v-if="message" class="recovery-feedback" role="status"><UiIcon name="info" />{{ message }}</p>
      <template v-if="page">
        <div v-if="!page.items.length" class="recovery-empty"><UiIcon name="history" :size="24" /><p>这一页没有恢复记录。</p></div>
        <ul class="recovery-list">
          <li v-for="item in page.items" :key="item.id">
            <template v-if="item.status">
              <UiIcon :name="item.status.kind==='mkdir'||item.status.kind==='rmdir'?'folder':'file'" class="recovery-file-icon" :size="18" />
              <div class="recovery-file">
                <strong>{{ item.status.path }}</strong>
                <span class="recovery-file-status">{{ recoveryStatusLabel(item.status) }} · {{ item.status.kind==='replace'?'写入文件':item.status.kind==='delete'?'删除文件':item.status.kind==='mkdir'?'新建目录':item.status.kind==='rmdir'?'移除空目录':'文件操作' }}</span>
                <details class="recovery-identifiers"><summary>记录信息</summary><small>设备：{{ item.status.device }}</small><small>操作：{{ item.id }}</small></details>
              </div>
              <button :disabled="busy" @click="inspect(item.status)"><UiIcon name="shield" />核对记录</button>
            </template>
            <template v-else>
              <UiIcon name="alert" class="recovery-file-icon" :size="18" />
              <div class="recovery-file"><strong>记录无法核验</strong><span class="recovery-file-status">记录缺失或损坏，不能据此清理恢复内容。</span><small>{{ item.id }}</small></div>
            </template>
          </li>
        </ul>
        <nav class="recovery-actions" aria-label="服务器恢复记录分页">
          <button :disabled="busy || cursors.length < 2" @click="previous"><UiIcon name="chevron-left" />上一页</button>
          <span>第 {{ cursors.length }} 页</span>
          <button :disabled="busy || !page.next_cursor" @click="load(page.next_cursor)">下一页<UiIcon name="chevron-right" /></button>
        </nav>
      </template>
      <section v-if="review" class="recovery-review" aria-label="服务器恢复记录核对">
        <div class="recovery-review-heading"><UiIcon name="shield" :size="19" /><div><span>核对结果</span><h3>{{ review.status.path }}</h3></div></div>
        <p class="recovery-comparison">{{ recoveryStatusLabel(review.status) }}。{{ comparisonLabels[review.comparison] }}。</p>
        <p class="recovery-note">核对这一条记录不会确认整批同步完成，也不会更新同步基线。</p>
        <div v-if="review.recovery_state === 'available'" class="recovery-export">
          <div><strong>变更前的文件</strong><span>{{ byteLabel(review.status.before?.size || 0) }}</span></div>
          <button :disabled="busy" @click="exportBefore"><UiIcon name="download" />导出原内容…</button>
        </div>
        <p v-else-if="review.recovery_state === 'missing' || review.recovery_state === 'corrupt'" class="error"><UiIcon name="alert" />原内容缺失或校验失败，不能导出或清理。</p>
        <div v-if="review.can_retire && canClean" class="recovery-danger">
          <h4><UiIcon name="trash" />清理这份恢复内容</h4>
          <p>清理会永久删除这份服务器原内容，之后无法从这里导出。请先导出需要保留的内容，并确认其他设备不再需要。当前项目文件与用于防止重复执行的记录会保留。</p>
          <label>输入“{{ confirmation }}”确认<input v-model="phrase" :disabled="busy" autocomplete="off" :placeholder="confirmation"></label>
          <button class="danger" :disabled="!canRetire" @click="retire"><UiIcon name="trash" />永久清理此条原内容</button>
        </div>
        <p v-else-if="review.status.operation.status === 'uncertain'" class="recovery-note">执行结果不确定，保留记录和恢复内容；请先核对实际文件。</p>
        <p v-else-if="!canClean" class="recovery-note">此服务器仅支持核对与导出恢复内容。</p>
        <p v-else-if="review.local_pending" class="recovery-note">本机还有未完成的同步引用这条记录，请先在同步面板核对。</p>
        <p v-else-if="review.lease_active" class="recovery-note">服务器上有正在进行的同步，请停止后重新核对。</p>
        <p v-else-if="review.status.retirement !== 'retired'" class="recovery-note">当前不能清理，请先处理同步占用或本机未完成的同步，再重新核对。</p>
      </section>
    </div>
  </details>
</template>

<style scoped>
.remote-recovery { font-size: 13px; color: var(--text); overflow-wrap: anywhere; }
.recovery-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 0 0 18px; list-style: none; cursor: pointer; }
.recovery-heading::-webkit-details-marker { display: none; }
.recovery-title { display: flex; align-items: center; gap: 10px; font-size: 18px; }
.recovery-title > .ui-icon { color: var(--amber, var(--accent)); }
.recovery-chevron { color: var(--muted); transition: transform 140ms ease; }
.remote-recovery[open] > .recovery-heading > .recovery-chevron { transform: rotate(90deg); }
.recovery-body { display: grid; gap: 20px; }
p { margin: 0; line-height: 1.65; }
.recovery-intro, .recovery-note { color: var(--muted); }
.recovery-toolbar { display: flex; align-items: flex-end; flex-wrap: wrap; gap: 10px; }
label { display: grid; gap: 7px; color: var(--text); }
.recovery-toolbar label { width: min(360px, 100%); }
label small { margin-left: 6px; font-size: 11px; color: var(--muted); }
input { width: 100%; box-sizing: border-box; font-family: var(--mono, monospace); font-size: 12px; }
button { display: inline-flex; align-items: center; justify-content: center; gap: 7px; min-height: 34px; transition: border-color 140ms ease, background-color 140ms ease; }
button.quiet { background: transparent; }
.recovery-feedback { display: flex; align-items: start; gap: 9px; padding: 12px 14px; border-radius: 6px; background: var(--panel-2, var(--panel)); }
.recovery-feedback > .ui-icon { margin-top: 3px; color: var(--muted); }
.recovery-empty { display: flex; align-items: center; gap: 12px; padding: 24px 0; color: var(--muted); border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); }
.recovery-list { margin: 0; padding: 0; list-style: none; max-height: min(48vh, 520px); overflow-y: auto; }
.recovery-list li { display: flex; align-items: start; gap: 12px; padding: 17px 0; border-bottom: 1px solid var(--line); }
.recovery-list li:first-child { border-top: 1px solid var(--line); }
.recovery-file-icon { margin-top: 2px; color: var(--muted); }
.recovery-file { display: grid; flex: 1; min-width: 0; gap: 5px; }
.recovery-file > strong { font-family: var(--mono, monospace); font-size: 12px; font-weight: 500; }
.recovery-file-status, .recovery-file small { color: var(--muted); font-size: 12px; }
.recovery-identifiers { color: var(--muted); font-size: 11px; }
.recovery-identifiers summary { width: fit-content; cursor: pointer; }
.recovery-identifiers small { display: block; margin-top: 5px; font-family: var(--mono, monospace); }
.recovery-actions { display: flex; align-items: center; justify-content: flex-end; gap: 12px; }
.recovery-actions span { color: var(--muted); font-size: 12px; }
.recovery-review { display: grid; gap: 14px; padding-top: 22px; border-top: 1px solid var(--line); }
.recovery-review-heading { display: flex; align-items: center; gap: 11px; }
.recovery-review-heading > .ui-icon { color: var(--amber, var(--accent)); }
.recovery-review-heading span { color: var(--muted); font-size: 11px; }
h3 { margin: 4px 0 0; font-size: 14px; font-weight: 600; font-family: var(--mono, monospace); }
.recovery-export { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; padding: 14px 0; border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); }
.recovery-export > div { display: flex; align-items: center; gap: 12px; }
.recovery-export span { color: var(--muted); font-family: var(--mono, monospace); font-size: 12px; }
.recovery-danger { display: grid; justify-items: start; gap: 12px; margin-top: 4px; padding: 18px 20px; border: 1px solid var(--red-line, var(--line)); border-left: 3px solid var(--red, #c96859); border-radius: 6px; background: var(--panel-2, var(--panel)); }
.recovery-danger h4 { display: flex; align-items: center; gap: 8px; margin: 0; font-size: 13px; }
.recovery-danger p { color: var(--muted); max-width: 740px; }
.recovery-danger label { width: min(340px, 100%); font-size: 12px; }
button.danger { color: var(--red, #c96859); border-color: var(--red-line, var(--line)); background: transparent; }
.error { display: flex; align-items: center; gap: 8px; color: var(--red, #c96859); }
@media (max-width: 620px) { .recovery-list li { flex-wrap: wrap; }.recovery-list li > button { margin-left: 30px; }.recovery-danger { padding: 14px; }.recovery-actions { justify-content: space-between; }.recovery-title { font-size: 16px; } }
@media (prefers-reduced-motion: reduce) { button, .recovery-chevron { transition: none; } }
</style>
