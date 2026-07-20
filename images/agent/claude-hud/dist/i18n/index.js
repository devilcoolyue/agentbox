import { en } from "./en.js";
import { zhHans } from "./zh-Hans.js";
import { zhHant } from "./zh-Hant.js";
const locales = {
    en,
    zh: zhHans,
    "zh-Hans": zhHans,
    "zh-Hant": zhHant,
    "zh-TW": zhHant,
};
// Resolve short language tags to canonical BCP 47 forms.
// Based on CLDR likely subtags: zh → zh-Hans-CN
// https://www.unicode.org/cldr/charts/latest/supplemental/likely_subtags.html
const CANONICAL = {
    "en": "en",
    "zh": "zh-Hans",
    "zh-Hans": "zh-Hans",
    "zh-Hant": "zh-Hant",
    "zh-TW": "zh-Hant",
};
let currentLanguage = "en";
export function setLanguage(lang) {
    currentLanguage = lang;
}
export function getLanguage() {
    return currentLanguage;
}
// https://www.rfc-editor.org/info/bcp47
export function getCanonicalLanguage() {
    return CANONICAL[currentLanguage] ?? "en";
}
// https://www.unicode.org/reports/tr11/
export function isCjkLanguage() {
    const canon = getCanonicalLanguage();
    return canon === "zh-Hans" || canon === "zh-Hant";
}
export function t(key) {
    const canon = getCanonicalLanguage();
    return locales[canon]?.[key] ?? locales.en[key] ?? key;
}
// Minimal named-placeholder interpolation. Layout that varies by language
// (spacing, affix position) lives in each locale's pattern string rather than in
// render code. Unknown placeholders render as empty string (kept lenient).
export function interpolate(pattern, params) {
    return pattern.replace(/\{(\w+)\}/g, (_, k) => String(params[k] ?? ""));
}
//# sourceMappingURL=index.js.map