<script setup lang="ts">
// Reuse the tracked Agentbox mark itself, also used by the web app's tpl-mark.
// The only HTML inserted here is this static repository SVG, never server data.
import logo from '../../internal/web/static/img/logo.svg?raw';

withDefaults(defineProps<{ compact?: boolean; iconOnly?: boolean }>(), {
  compact: false,
  iconOnly: false,
});
</script>

<template>
  <span class="brand-mark" :class="{ compact, 'icon-only': iconOnly }" role="img" aria-label="Agentbox">
    <span class="brand-symbol" aria-hidden="true" v-html="logo"></span>
    <span v-if="!iconOnly" class="brand-label" aria-hidden="true">AGENTBOX</span>
  </span>
</template>

<style scoped>
/* Sizes, monospaced lettering and tracking follow internal/web/static/css/base.css's
   .wordmark / .wordmark-sm. The original logo palette remains the default. */
.brand-mark {
  display: inline-flex;
  align-items: center;
  gap: 9px;
  color: var(--text, currentColor);
  font-family: var(--mono, ui-monospace, SFMono-Regular, Menlo, Consolas, monospace);
  font-size: var(--brand-font-size, 22px);
  font-weight: 500;
  line-height: 1;
  letter-spacing: .18em;
  white-space: nowrap;
}
.brand-mark.compact { gap: 7px; font-size: var(--brand-font-size, 15px); }
.brand-symbol { display: block; flex: none; width: var(--brand-mark-size, 23px); height: var(--brand-mark-size, 23px); margin: 0; }
.compact .brand-symbol { width: var(--brand-mark-size, 17px); height: var(--brand-mark-size, 17px); }
.brand-symbol :deep(svg) { display: block; width: 100%; height: 100%; }
.brand-symbol :deep([fill="#e8a33d"]) { fill: var(--brand-mark, #e8a33d); }
.brand-symbol :deep([fill="#241803"]) { fill: var(--brand-socket, #241803); }
.brand-symbol :deep([fill="#ffc76b"]) { fill: var(--brand-eye, #ffc76b); }
.brand-symbol :deep([fill="#8a5f1f"]) { fill: var(--brand-ink, #8a5f1f); }
.brand-symbol :deep([stroke="#8a5f1f"]) { stroke: var(--brand-ink, #8a5f1f); }
.brand-label { margin: 0; font-size: inherit; letter-spacing: inherit; }
</style>
