export const languages = [
    { value: 'system', label: '跟随系统' },
    { value: 'zh-CN', label: '简体中文' },
    { value: 'zh-TW', label: '繁體中文' },
    { value: 'en', label: 'English' },
];
export function normalizeLanguage(value) {
    if (typeof value !== 'string')
        return;
    const parts = value.trim().toLowerCase().replaceAll('_', '-').split('-');
    if (parts[0] === 'en')
        return 'en';
    if (parts[0] !== 'zh')
        return;
    // An explicit script wins over a region (for example zh-Hans-HK).
    if (parts.includes('hant'))
        return 'zh-TW';
    if (parts.includes('hans'))
        return 'zh-CN';
    return parts.some(p => ['tw', 'hk', 'mo'].includes(p)) ? 'zh-TW' : 'zh-CN';
}
export function detectLanguage(candidates) {
    for (const candidate of candidates) {
        const locale = normalizeLanguage(candidate);
        if (locale)
            return locale;
    }
    return 'en';
}
export function interpolate(message, params = {}) {
    // Single pass: placeholder-looking user data is never interpreted recursively.
    return message.replace(/\{([a-zA-Z][\w]*)\}/g, (match, key) => Object.prototype.hasOwnProperty.call(params, key) ? String(params[key]) : match);
}
export function createI18n(options) {
    const storageKey = options.storageKey || 'agentbox.language';
    const systemLanguages = options.systemLanguages || (() => typeof navigator === 'undefined' ? [] : navigator.languages?.length ? navigator.languages : [navigator.language]);
    let storage = options.storage;
    if (storage === undefined) {
        try {
            storage = typeof localStorage === 'undefined' ? null : localStorage;
        }
        catch {
            storage = null;
        }
    }
    const parse = (value) => value === 'system' ? value : normalizeLanguage(value) || 'system';
    let preference = 'system';
    try {
        preference = parse(storage?.getItem(storageKey));
    }
    catch { /* private browsing */ }
    let locale = preference === 'system' ? detectLanguage(systemLanguages()) : preference;
    const listeners = new Set();
    const update = (next) => {
        const resolved = next === 'system' ? detectLanguage(systemLanguages()) : next;
        if (preference === next && locale === resolved)
            return;
        preference = next;
        locale = resolved;
        for (const listener of listeners)
            listener();
    };
    const storageChanged = (event) => {
        if (event.key === storageKey || event.key === null)
            update(parse(event.newValue));
    };
    const systemChanged = () => { if (preference === 'system')
        update('system'); };
    if (typeof window !== 'undefined') {
        window.addEventListener('storage', storageChanged);
        window.addEventListener('languagechange', systemChanged);
    }
    return {
        get locale() { return locale; },
        get preference() { return preference; },
        t(source, params) {
            const catalog = options.catalogs[locale];
            const translated = catalog && Object.prototype.hasOwnProperty.call(catalog, source) ? catalog[source] : source;
            return interpolate(translated, params);
        },
        setLanguage(value) {
            const next = parse(value);
            try {
                storage?.setItem(storageKey, next);
            }
            catch { /* Keep the in-memory choice usable. */ }
            update(next);
        },
        subscribe(listener) {
            listeners.add(listener);
            return () => listeners.delete(listener);
        },
        dispose() {
            listeners.clear();
            if (typeof window !== 'undefined') {
                window.removeEventListener('storage', storageChanged);
                window.removeEventListener('languagechange', systemChanged);
            }
        },
    };
}
