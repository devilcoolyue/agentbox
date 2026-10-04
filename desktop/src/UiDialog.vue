<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue';
import UiIcon from './UiIcon.vue';
import { newTaskId } from './task-id';

const props = defineProps<{ open: boolean; title: string; busy?: boolean }>();
const emit = defineEmits<{(event: 'close'): void}>();
const panel = ref<HTMLElement>();
const headingId = `dialog-${newTaskId()}`;
let previous: HTMLElement | null = null;
let generation = 0;
const focusable = () => Array.from(panel.value?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), summary, [href], [tabindex="0"]') || []).filter(element => element.getClientRects().length);
function close() { if (!props.busy) emit('close'); }
function key(event: KeyboardEvent) {
  if (!props.open || event.defaultPrevented) return;
  if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close(); }
  if (event.key !== 'Tab') return;
  const items = focusable();
  const first = items[0]; const last = items[items.length - 1];
  if (!first) { event.preventDefault(); panel.value?.focus(); return; }
  if (!panel.value?.contains(document.activeElement) || (event.shiftKey && document.activeElement === first) || (!event.shiftKey && document.activeElement === last)) {
    event.preventDefault(); (event.shiftKey ? last : first)?.focus();
  }
}
watch(() => props.open, async open => {
  const current = ++generation;
  if (open) {
    previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    document.addEventListener('keydown', key);
    await nextTick();
    if (current === generation && props.open) (panel.value?.querySelector<HTMLElement>('[autofocus]') || focusable()[0] || panel.value)?.focus();
  } else {
    document.removeEventListener('keydown', key);
    if (previous?.isConnected) previous.focus();
  }
}, { immediate: true });
onBeforeUnmount(() => { generation++; document.removeEventListener('keydown', key); if (props.open && previous?.isConnected) previous.focus(); });
</script>

<template>
  <Teleport to="body">
    <Transition name="dialog">
      <div v-show="open" class="dialog-scrim" @pointerdown.self="close">
        <section ref="panel" class="ui-dialog" role="dialog" aria-modal="true" :aria-labelledby="headingId" tabindex="-1">
          <div class="dialog-header"><h2 :id="headingId">{{ title }}</h2><button type="button" class="icon-button" :disabled="busy" :aria-label="`关闭${title}`" @click="close"><UiIcon name="close" /></button></div>
          <div class="dialog-body"><slot /></div>
          <div v-if="$slots.footer" class="dialog-footer"><slot name="footer" /></div>
        </section>
      </div>
    </Transition>
  </Teleport>
</template>
