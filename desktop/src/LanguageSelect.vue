<script setup lang="ts">
import { language, languageOptions, t } from './i18n';
import UiIcon from './UiIcon.vue';
</script>

<template>
  <label class="language-select">
    <UiIcon name="globe" :size="15" />
    <span class="sr-only">{{ t('界面语言') }}</span>
    <select v-model="language" :aria-label="t('界面语言')">
      <option v-for="option in languageOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
    </select>
  </label>
</template>

<style scoped>
.language-select { display: inline-flex; align-items: center; gap: 7px; color: var(--muted); max-width: 100%; }
.language-select select { width: auto; min-width: 120px; max-width: 100%; font-size: 12px; }
.language-select svg { flex: none; }
</style>
