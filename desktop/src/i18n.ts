import { computed, ref } from 'vue';
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

export interface LocalizedMessage { readonly source: string; readonly params?: Params }
export type DisplayMessage = string | LocalizedMessage;
export function msg(source: string, params?: Params): LocalizedMessage {
  // Capture placeholder values at the time of the event. A later language
  // switch must not reread changing transfer counters or user selections.
  return { source, params: params ? { ...params } : undefined };
}
export function messageRef(initial: DisplayMessage = '') {
  const value = ref<DisplayMessage>(initial);
  return computed<string, DisplayMessage>({
    get: () => typeof value.value === 'string' ? value.value : t(value.value.source, value.value.params),
    set: next => { value.value = next; },
  });
}
