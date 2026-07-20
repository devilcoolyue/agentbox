import { label } from '../colors.js';
import { t } from '../../i18n/index.js';
import { sanitize as sanitizeDisplayText } from './added-dirs.js';
const ADVISOR_ID_PATTERN = /^(?:claude-)?(opus|sonnet|haiku)-(\d+)-(\d+)/i;
// Hard cap on the visible advisor segment so a malformed transcript or
// override cannot grow into an oversized recurring statusline part.
// 64 leaves headroom for "Advisor: " prefix on narrow terminals while still
// fitting any realistic model name (e.g. "claude-haiku-4-5-20251001" = 25).
const MAX_ADVISOR_DISPLAY_LEN = 64;
/**
 * Prettifies a raw advisor model ID (as captured from the transcript) into a
 * human-friendly label. Falls back to the input string when the pattern is
 * not recognised.
 *
 *   claude-opus-4-7              → Opus 4.7
 *   claude-sonnet-4-6            → Sonnet 4.6
 *   claude-haiku-4-5-20251001    → Haiku 4.5
 *   opus                         → Opus
 */
export function prettifyAdvisorId(rawId) {
    const id = rawId.trim();
    if (!id) {
        return '';
    }
    const match = id.match(ADVISOR_ID_PATTERN);
    if (match) {
        const family = match[1].charAt(0).toUpperCase() + match[1].slice(1).toLowerCase();
        return `${family} ${match[2]}.${match[3]}`;
    }
    // Short alias fallbacks (e.g. settings.json stores `"opus"`, `"sonnet"`).
    const lower = id.toLowerCase();
    if (lower === 'opus' || lower === 'sonnet' || lower === 'haiku') {
        return lower.charAt(0).toUpperCase() + lower.slice(1);
    }
    return id.replace(/^claude-/i, '');
}
function safeAdvisorText(input) {
    if (typeof input !== 'string') {
        return '';
    }
    // Strip control + bidi + ANSI ESC bytes (sanitizeDisplayText handles all of
    // U+0000-U+001F including ESC, so OSC/CSI/SS3 sequences lose their lead
    // byte and the remainder renders as inert plain text), then cap length.
    return sanitizeDisplayText(input.trim()).slice(0, MAX_ADVISOR_DISPLAY_LEN);
}
export function renderAdvisorLine(ctx) {
    const display = ctx.config?.display;
    if (display?.showAdvisor !== true) {
        return null;
    }
    const override = safeAdvisorText(display.advisorOverride);
    const transcriptValue = safeAdvisorText(ctx.transcript?.advisorModel);
    const rawValue = override.length > 0 ? override : transcriptValue;
    if (!rawValue) {
        return null;
    }
    const pretty = override.length > 0 ? rawValue : prettifyAdvisorId(rawValue);
    // Defence-in-depth: re-sanitize and re-cap after prettifying in case any
    // future change to the prettifier reflects raw input bytes through.
    const safePretty = safeAdvisorText(pretty);
    if (!safePretty) {
        return null;
    }
    const colors = ctx.config?.colors;
    return `${label(`${t('label.advisor')}:`, colors)} ${safePretty}`;
}
//# sourceMappingURL=advisor.js.map