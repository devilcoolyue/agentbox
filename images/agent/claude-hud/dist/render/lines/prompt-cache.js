import { getContextColor, RESET, label, warning as warningColor } from '../colors.js';
import { t } from '../../i18n/index.js';
function getPromptCacheWarningSeconds(ttlSeconds) {
    return Math.min(ttlSeconds, Math.max(60, Math.floor(ttlSeconds / 5)));
}
function colorPromptCacheValue(value, state, ctx) {
    if (state === 'expired') {
        return label(value, ctx.config?.colors);
    }
    if (state === 'warning') {
        return warningColor(value, ctx.config?.colors);
    }
    return `${getContextColor(0, ctx.config?.colors)}${value}${RESET}`;
}
export function formatPromptCacheCountdown(remainingMs) {
    if (remainingMs <= 0) {
        return t('status.expired');
    }
    const totalSeconds = Math.ceil(remainingMs / 1000);
    const hours = Math.floor(totalSeconds / 3600);
    const minutes = Math.floor((totalSeconds % 3600) / 60);
    const seconds = totalSeconds % 60;
    if (hours > 0) {
        return `${hours}h ${minutes}m ${seconds}s`;
    }
    return `${minutes}m ${seconds}s`;
}
export function renderPromptCacheLine(ctx, now = Date.now()) {
    const display = ctx.config?.display;
    if (!display?.showPromptCache) {
        return null;
    }
    const lastAssistantResponseAt = ctx.transcript.lastAssistantResponseAt;
    if (!lastAssistantResponseAt || Number.isNaN(lastAssistantResponseAt.getTime())) {
        return null;
    }
    const ttlSeconds = (typeof display.promptCacheTtlSeconds === 'number'
        && Number.isFinite(display.promptCacheTtlSeconds)
        && display.promptCacheTtlSeconds > 0)
        ? Math.floor(display.promptCacheTtlSeconds)
        : 300;
    const remainingMs = (lastAssistantResponseAt.getTime() + ttlSeconds * 1000) - now;
    const state = remainingMs <= 0
        ? 'expired'
        : remainingMs <= getPromptCacheWarningSeconds(ttlSeconds) * 1000
            ? 'warning'
            : 'active';
    return `${label(t('label.promptCache'), ctx.config?.colors)} ${colorPromptCacheValue(`⏱ ${formatPromptCacheCountdown(remainingMs)}`, state, ctx)}`;
}
//# sourceMappingURL=prompt-cache.js.map