<script setup lang="ts">
import { t, messageRef } from './i18n';
import { onBeforeUnmount, ref } from 'vue';
import { bridge, errorMessage } from './bridge';
import { byteLabel } from './sync-task';
import UiIcon from './UiIcon.vue';

interface Inspection {
  files: number; directories: number; bytes: number; windows_issues: number;
  capabilities: {executable: boolean; name_policy: {windows: boolean; case_sensitive: boolean; normalization_sensitive: boolean}};
}
const busy = ref(false);
const canceling = ref(false);
const result = ref<Inspection|null>(null);
const error = messageRef('');
let alive = true;
async function inspect() {
  if (busy.value || !alive) return;
  busy.value = true; canceling.value = false; error.value = ''; result.value = null;
  try {
    const value = await bridge.invoke<Inspection|null>('inspect_local');
    if (alive) result.value = value;
  } catch (err) { if (alive) error.value = errorMessage(err); }
  finally { if (alive) { busy.value = false; canceling.value = false; } }
}
async function cancel() {
  if (!busy.value || canceling.value) return;
  canceling.value = true;
  try { await bridge.invoke('cancel_inspection'); }
  catch (err) { if (alive) { error.value = errorMessage(err); canceling.value = false; } }
}
onBeforeUnmount(() => { alive = false; if (busy.value) void cancel(); });
</script>

<template>
  <details class="local-inspection">
    <summary><UiIcon name="folder" :size="16" /><span>{{ t("检查本地目录") }}</span><UiIcon class="inspection-chevron" name="chevron" :size="13" /></summary>
    <div class="inspection-body">
      <p class="inspection-description">{{ t("查看文件数量、大小与 Windows 文件名兼容性。") }}</p>
      <div class="inspection-actions"><button :disabled="busy" @click="inspect"><UiIcon :name="busy ? 'refresh' : 'folder'" :size="15" :class="{'is-inspecting': busy}" />{{ busy ? t("正在检查…") : t("选择目录并检查") }}</button><button v-if="busy" class="ghost" :disabled="canceling" @click="cancel">{{ canceling ? t("正在取消…") : t("取消检查") }}</button></div>
      <p class="inspection-rule">{{ t("遵循 .agentboxignore，忽略依赖与构建目录。") }}</p>
      <p v-if="error" class="error inspection-message" role="alert"><UiIcon name="info" :size="15" /><span>{{ error }}</span></p>
      <div v-if="result" class="inspection-result" role="status">
        <p class="inspection-counts"><strong>{{ result.files }}</strong> {{ t("个文件") }}<span>·</span>{{ t("{p1} 个目录", { p1: (result.directories) }) }}<span>·</span>{{ byteLabel(result.bytes) }}</p>
        <p class="inspection-message" :class="{error: result.windows_issues, compatible: !result.windows_issues}"><UiIcon :name="result.windows_issues ? 'info' : 'check'" :size="15" /><span>{{ result.windows_issues ? t("发现 {p1} 项 Windows 文件名兼容问题，请在同步前处理。", { p1: (result.windows_issues) }) : t("本次检查未发现 Windows 文件名兼容问题。") }}</span></p>
        <p class="inspection-rules">{{ result.capabilities.name_policy.case_sensitive ? t("文件名区分大小写") : t("文件名不区分大小写") }} · {{ result.capabilities.name_policy.normalization_sensitive ? t("区分 Unicode 等价名称") : t("合并 Unicode 等价名称") }}</p>
      </div>
    </div>
  </details>
</template>

<style scoped>
.local-inspection{margin:18px 0 0;padding:16px 0 0;border-top:1px solid var(--line-soft);font-size:12px;color:var(--text)}
.local-inspection>summary{display:flex;align-items:center;gap:8px;list-style:none;cursor:pointer;font-size:13px;font-weight:500}.local-inspection>summary::-webkit-details-marker{display:none}.local-inspection>summary>svg{color:var(--muted)}.inspection-chevron{margin-left:auto;transform:rotate(-90deg);transition:transform 140ms ease}.local-inspection[open] .inspection-chevron{transform:none}
.inspection-body{padding:11px 0 0 24px}.inspection-description{margin:0 0 12px;color:var(--muted);font-size:12px;line-height:1.7}.inspection-actions{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.inspection-actions button{display:inline-flex;align-items:center;gap:6px;min-height:32px;margin:0;padding:5px 10px;font-size:12px}.inspection-rule{margin:9px 0 0;color:var(--muted);font-size:11px;line-height:1.7}
.inspection-result{margin-top:14px;padding-top:13px;border-top:1px solid var(--line-soft)}.inspection-counts{display:flex;align-items:baseline;flex-wrap:wrap;gap:5px;margin:0 0 8px;font-size:12px}.inspection-counts strong{font:13px var(--mono);color:var(--text)}.inspection-counts>span{margin:0 3px;color:var(--muted)}
.inspection-message{display:flex;align-items:flex-start;gap:6px;margin:10px 0 0;font-size:12px;line-height:1.7}.inspection-message>svg{flex:none;margin-top:3px}.inspection-message.error{color:var(--red)}.inspection-message.compatible>svg{color:var(--green)}.inspection-message.compatible{color:var(--muted)}.inspection-rules{margin:7px 0 0;color:var(--muted);font-size:11px;line-height:1.7}
.is-inspecting{animation:directory-inspecting .9s linear infinite}@keyframes directory-inspecting{to{transform:rotate(360deg)}}
@media(max-width:480px){.inspection-body{padding-left:0}}
@media(prefers-reduced-motion:reduce){.is-inspecting{animation:none}.inspection-chevron{transition:none}}
</style>
