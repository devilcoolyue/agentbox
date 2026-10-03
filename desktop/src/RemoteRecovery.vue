<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue';
import { errorMessage } from './bridge';
import { SyncTask, byteLabel } from './sync-task';
import { comparisonLabels, recoveryStatusLabel, type OrphanPage, type OrphanReview, type RemoteRecoveryStatus } from './orphan-recovery';

const props = defineProps<{session: string; serverId: string; canClean: boolean}>();
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
  <details class="remote-recovery">
    <summary>服务器恢复记录</summary>
    <p>本机同步历史丢失时，可从服务器核对和导出原内容。这里也会列出仍有本地历史的操作。</p>
    <form @submit.prevent="load('', true)">
      <label>设备 ID（留空查看全部）<input v-model="device" :disabled="busy" maxlength="128" autocomplete="off"></label>
      <button :disabled="busy">读取记录</button>
    </form>
    <button v-if="busy" @click="cancel">取消操作</button>
    <p v-if="message" role="status">{{ message }}</p>
    <template v-if="page">
      <p v-if="!page.items.length" class="muted">这一页没有恢复记录。</p>
      <ul class="recovery-list">
        <li v-for="item in page.items" :key="item.id">
          <template v-if="item.status">
            <strong>{{ item.status.path }}</strong>
            <span>{{ recoveryStatusLabel(item.status) }} · {{ item.status.kind }}</span>
            <small>设备：{{ item.status.device }}</small>
            <small>操作：{{ item.id }}</small>
            <button :disabled="busy" @click="inspect(item.status)">核对记录</button>
          </template>
          <template v-else><strong>记录无法核验</strong><small>{{ item.id }}</small><span>记录缺失或损坏，不能据此清理恢复内容。</span></template>
        </li>
      </ul>
      <div class="recovery-actions"><button :disabled="busy || cursors.length < 2" @click="previous">上一页</button><span>第 {{ cursors.length }} 页</span><button :disabled="busy || !page.next_cursor" @click="load(page.next_cursor)">下一页</button></div>
    </template>
    <section v-if="review" class="recovery-review" aria-label="服务器恢复记录核对">
      <h3>{{ review.status.path }}</h3>
      <p>{{ recoveryStatusLabel(review.status) }}。{{ comparisonLabels[review.comparison] }}。</p>
      <p>单条执行收据不表示整批同步已完成，也不会提交同步基线。</p>
      <p v-if="review.recovery_state === 'available'">可导出原内容：{{ byteLabel(review.status.before?.size || 0) }}</p>
      <button v-if="review.recovery_state === 'available'" :disabled="busy" @click="exportBefore">导出原内容…</button>
      <p v-else-if="review.recovery_state === 'missing' || review.recovery_state === 'corrupt'" class="error">原内容缺失或校验失败，不能导出或清理。</p>
      <template v-if="review.can_retire && canClean">
        <p>清理会永久删除这条操作的服务器恢复内容，之后不能再从这里导出。执行收据保留，当前项目文件不变。请先导出需要保留的内容，并确认其他设备不再需要此副本。</p>
        <label>输入“{{ confirmation }}”确认<input v-model="phrase" :disabled="busy" autocomplete="off"></label>
        <button class="danger" :disabled="!canRetire" @click="retire">永久清理此条原内容</button>
      </template>
      <p v-else-if="review.status.operation.status === 'uncertain'" class="muted">执行结果不确定，保留记录和恢复内容；请先核对实际文件。</p>
      <p v-else-if="!canClean" class="muted">此服务器仅支持核对与导出恢复内容。</p>
      <p v-else-if="review.local_pending" class="muted">本机待定批次仍引用这条操作，请先在同步面板核对该批次。</p>
      <p v-else-if="review.lease_active" class="muted">服务器上有正在进行的同步，请停止后重新核对。</p>
      <p v-else-if="review.status.retirement !== 'retired'" class="muted">当前不能清理，请先处理同步占用或本机待定批次，再重新核对。</p>
    </section>
  </details>
</template>

<style scoped>
.remote-recovery{border-top:1px solid var(--border,#39404c);padding:12px 0;font-size:12px;overflow-wrap:anywhere}
summary{cursor:pointer;font-weight:600}p{line-height:1.6}form,label{display:grid;gap:8px}form{margin-bottom:8px}
.recovery-list{padding:0;list-style:none;max-height:360px;overflow:auto}.recovery-list li{display:grid;gap:6px;padding:12px 0;border-bottom:1px solid var(--border,#39404c)}
.recovery-actions{display:flex;align-items:center;justify-content:space-between;gap:8px}.recovery-review{margin-top:12px;padding:10px;border:1px solid var(--border,#39404c);border-radius:8px}.recovery-review button{margin-top:8px}small{opacity:.75}
</style>
