import { createI18n, interpolate, languages } from './i18n/core.js';
import type { LanguagePreference, Params } from './i18n/core.js';
import enStatic from './locales/en-static.js';
import twStatic from './locales/zh-TW-static.js';
import enDynamic from './locales/en-dynamic.js';
import twDynamic from './locales/zh-TW-dynamic.js';

export const i18n = createI18n({ catalogs: {
  en: { ...enDynamic, ...enStatic }, 'zh-TW': { ...twDynamic, ...twStatic },
} });
export const t = i18n.t;
export { languages };
export const escapeHTML = (value: string): string => value.replace(/[&<>"']/g,
  char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]!);

/** Only for authored HTML templates whose parameter values are ALREADY escaped
 * (or trusted markup). For arbitrary values use escapeHTML(t(source, params)). */
export function htmlText(source: string, params: Params = {}): string {
  return escapeHTML(t(source)).replace(/\{([a-zA-Z][\w]*)\}/g, (match, key: string) =>
    Object.prototype.hasOwnProperty.call(params, key) ? String(params[key]) : match);
}

type Binding = { node: WeakRef<Node>; render: () => string; attribute?: string; last: string; entry: WeakRef<Binding> };
// The weak registry must not own render closures: a closure can refer to its
// original element. The per-node WeakMap keeps active bindings alive instead.
const bindings = new Set<WeakRef<Binding>>();
const bound = new WeakMap<Node, Map<string, Binding>>();
let writes = 0;
const textValue = (source: string, params?: Params) => {
  const exact = t(source, params);
  if (exact !== interpolate(source, params)) return exact;
  const leading = source.match(/^\s*/)?.[0] || '';
  const trailing = source.match(/\s*$/)?.[0] || '';
  return source.trim() ? leading + t(source.trim(), params) + trailing : source;
};
function bindRender(node: Node, render: () => string, attribute?: string) {
  if (++writes % 256 === 0) for (const entry of bindings) {
    if (!entry.deref()?.node.deref()) bindings.delete(entry);
  }
  const key = attribute || '#text';
  let entries = bound.get(node);
  if (!entries) bound.set(node, entries = new Map());
  const previous = entries.get(key);
  if (previous) bindings.delete(previous.entry);
  const last = render();
  const binding = { node: new WeakRef(node), render, attribute, last } as Binding;
  binding.entry = new WeakRef(binding);
  entries.set(key, binding);
  bindings.add(binding.entry);
  if (attribute) (node as Element).setAttribute(attribute, last);
  else node.textContent = last;
}

/** Explicit bindings only: never infer UI ownership from a string's contents. */
export function setText(node: Node, source: string, params?: Params): void {
  setTextRender(node, () => textValue(source, params));
}
export function setTextRender(node: Node, render: () => string): void {
  if (node.nodeType === Node.TEXT_NODE) { bindRender(node, render); return; }
  // Bind the text node, so decorating a button with SVGs or moving its caption
  // never lets a later language change remove the icon or new user content.
  if (node.childNodes.length !== 1 || node.firstChild?.nodeType !== Node.TEXT_NODE) {
    node.textContent = '';
    node.appendChild(document.createTextNode(''));
  }
  bindRender(node.firstChild!, render);
}
export function setAttr(node: Element, attribute: string, source: string, params?: Params): void { bindRender(node, () => textValue(source, params), attribute); }
export function setAttrRender(node: Element, attribute: string, render: () => string): void { bindRender(node, render, attribute); }

const marked = new WeakSet<Element>();
const markerSelector = '[data-i18n], [data-i18n-text], [data-i18n-attrs]';
export function translateMarked(root: ParentNode): void {
  const nodes = [...root.querySelectorAll<HTMLElement>(markerSelector)];
  if (root instanceof Element && root.matches(markerSelector)) nodes.unshift(root as HTMLElement);
  for (const el of nodes) {
    if (marked.has(el)) continue;
    marked.add(el);
    if (el.dataset.i18n !== undefined) {
      // data-i18n is only used on plain, app-owned text leaves.
      setText(el, el.dataset.i18n);
      const action = el.closest('button[data-icon], a[data-icon]');
      if (action) for (const attribute of ['aria-label', 'data-tip']) {
        const value = action.getAttribute(attribute);
        // Icon decoration can run before the observer sees a new dialog.
        // Adopt only attributes derived from this explicitly marked caption.
        if (value === null || value === t(el.dataset.i18n)) setAttr(action, attribute, el.dataset.i18n);
      }
    }
    if (el.dataset.i18nText) {
      const sources = JSON.parse(el.dataset.i18nText) as string[];
      for (const child of el.childNodes) {
        if (child.nodeType === Node.TEXT_NODE && sources.includes(child.textContent || '')) setText(child, child.textContent!);
      }
      if (sources.length === 1 && el.matches('button, a') && el.hasAttribute('data-icon')) {
        if (!el.hasAttribute('aria-label')) setAttr(el, 'aria-label', sources[0].trim());
        if (!el.hasAttribute('data-tip')) setAttr(el, 'data-tip', sources[0].trim());
      }
    }
    if (el.dataset.i18nAttrs) {
      const attributes = JSON.parse(el.dataset.i18nAttrs) as Record<string, string>;
      for (const [attr, source] of Object.entries(attributes)) setAttr(el, attr, source);
    }
  }
}

function refreshBindings() {
  for (const entry of bindings) {
    const binding = entry.deref();
    const node = binding?.node.deref();
    if (!binding || !node) { bindings.delete(entry); continue; }
    const current = binding.attribute ? (node as Element).getAttribute(binding.attribute) : node.textContent;
    // A renderer replacing an app-owned label with user data revokes the binding.
    if (current !== binding.last) {
      bindings.delete(entry);
      bound.get(node)?.delete(binding.attribute || '#text');
      continue;
    }
    binding.last = binding.render();
    if (binding.attribute) (node as Element).setAttribute(binding.attribute, binding.last);
    else node.textContent = binding.last;
  }
}

let initialized = false;
export function initI18n(): void {
  if (initialized) return;
  initialized = true;
  const selects = [...document.querySelectorAll<HTMLSelectElement>('[data-language-select]')];
  for (const select of selects) {
    for (const language of languages) {
      const option = document.createElement('option');
      option.value = language.value;
      if (language.value === 'system') setText(option, language.label);
      else option.textContent = language.label;
      select.append(option);
    }
    select.value = i18n.preference;
    select.addEventListener('change', () => i18n.setLanguage(select.value as LanguagePreference));
  }
  const refresh = () => {
    document.documentElement.lang = i18n.locale;
    refreshBindings();
    for (const select of selects) {
      select.value = i18n.preference;
      // Keep the shared enhanced-select caption synchronized without dispatching change.
      select.dispatchEvent(new Event('input', { bubbles: true }));
    }
    window.dispatchEvent(new CustomEvent('agentbox-language-change', { detail: { locale: i18n.locale } }));
  };
  translateMarked(document);
  refresh();
  i18n.subscribe(refresh);
  new MutationObserver(records => {
    for (const record of records) for (const node of record.addedNodes) {
      if (node instanceof Element) translateMarked(node);
    }
  }).observe(document.body, { childList: true, subtree: true });
}
