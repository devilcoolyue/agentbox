import { computed, ref } from 'vue';
import { formatProblem } from '../../web/src/problems';
import type { APIProblem } from '../../web/src/contracts/v1';
import { createI18n, languages, type LanguagePreference, type Params } from '../../web/src/i18n/core';
import en from './locales/en';
import traditional from './locales/zh-TW';

// The preference contains no account data and is shared by all server profiles.
const runtime = createI18n({ catalogs: { en, 'zh-TW': traditional } });
const revision = ref(0);
function updateDocument() {
  if (typeof document !== 'undefined') document.documentElement.lang = runtime.locale;
}
runtime.subscribe(() => { revision.value++; updateDocument(); });
updateDocument();
export const locale = computed(() => { revision.value; return runtime.locale; });
export const language = computed({
  get: () => { revision.value; return runtime.preference; },
  set: (value: LanguagePreference) => runtime.setLanguage(value),
});
export const languageOptions = computed(() => languages.map(option => ({
  value: option.value,
  label: option.value === 'system' ? t('跟随系统') : option.label,
})));
export function t(source: string, params?: Params): string {
  // Reading the revision lets Vue update the current render/computed value in
  // place. Switching language never changes terminal keys or mounted views.
  revision.value;
  return runtime.t(source, params);
}

export type MessageParams = Record<string, number | DisplayMessage>;
export interface LocalizedMessage { readonly source: string; readonly params?: MessageParams }
export interface ProblemMessage { readonly problem: Pick<APIProblem, 'code' | 'operation_id'>; readonly fallback: string | LocalizedMessage }
export type DisplayMessage = string | LocalizedMessage | ProblemMessage;
export function msg(source: string, params?: MessageParams): LocalizedMessage {
  // Capture placeholder values at the time of the event. A later language
  // switch must not reread changing transfer counters or user selections.
  return { source, params: params ? { ...params } : undefined };
}
export function renderMessage(message: DisplayMessage): string {
  if (typeof message === 'string') return message;
  if ('problem' in message) return formatProblem(message.problem, t, renderMessage(message.fallback) || t('操作失败，请重试'));
  const params: Record<string, string | number> = {};
  for (const [key, value] of Object.entries(message.params || {})) params[key] = typeof value === 'number' ? value : renderMessage(value);
  return t(message.source, params);
}
export function messageRef(initial: DisplayMessage = '') {
  const value = ref<DisplayMessage>(initial);
  return computed<string, DisplayMessage>({
    get: () => renderMessage(value.value),
    set: next => { value.value = next; },
  });
}
