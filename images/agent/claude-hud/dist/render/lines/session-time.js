import { label } from '../colors.js';
import { interpolate, t } from '../../i18n/index.js';
function pad(n) {
    return n < 10 ? `0${n}` : `${n}`;
}
function formatStartDate(date) {
    const y = date.getFullYear();
    const m = pad(date.getMonth() + 1);
    const d = pad(date.getDate());
    const h = pad(date.getHours());
    const min = pad(date.getMinutes());
    return `${y}-${m}-${d} ${h}:${min}`;
}
function formatRelativeTime(ms) {
    if (ms < 0) {
        return t('format.justNow');
    }
    const withAgo = (value) => interpolate(t('format.relativeTime'), { value });
    const seconds = Math.floor(ms / 1000);
    if (seconds < 60) {
        return withAgo(`${seconds}s`);
    }
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) {
        return withAgo(`${minutes}m`);
    }
    const hours = Math.floor(minutes / 60);
    const remainingMinutes = minutes % 60;
    if (hours < 24) {
        return withAgo(remainingMinutes > 0 ? `${hours}h ${remainingMinutes}m` : `${hours}h`);
    }
    const days = Math.floor(hours / 24);
    const remainingHours = hours % 24;
    return withAgo(remainingHours > 0 ? `${days}d ${remainingHours}h` : `${days}d`);
}
export function renderSessionTimeLine(ctx, nowFn) {
    const display = ctx.config?.display;
    const showStart = display?.showSessionStartDate === true;
    const showLastReply = display?.showLastResponseAt === true;
    const colors = ctx.config?.colors;
    if (!showStart && !showLastReply) {
        return null;
    }
    const parts = [];
    if (showStart && ctx.transcript.sessionStart) {
        const startStr = formatStartDate(ctx.transcript.sessionStart);
        parts.push(`${label(`${t('label.sessionStarted')}:`, colors)} ${startStr}`);
    }
    if (showLastReply && ctx.transcript.lastAssistantResponseAt) {
        const now = nowFn ? nowFn() : Date.now();
        const elapsed = now - ctx.transcript.lastAssistantResponseAt.getTime();
        parts.push(`${label(`${t('label.lastReply')}:`, colors)} ${formatRelativeTime(elapsed)}`);
    }
    if (parts.length === 0) {
        return null;
    }
    return parts.join(` ${label('│', colors)} `);
}
//# sourceMappingURL=session-time.js.map