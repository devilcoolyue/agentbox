import { label } from '../colors.js';
import { t } from '../../i18n/index.js';
import { formatTokens } from '../../utils/format.js';
export function formatSessionTokenSummary(tokens, prefix) {
    const total = tokens.inputTokens + tokens.outputTokens + tokens.cacheCreationTokens + tokens.cacheReadTokens;
    if (total === 0) {
        return null;
    }
    const parts = [
        `${t('format.in')}: ${formatTokens(tokens.inputTokens)}`,
        `${t('format.out')}: ${formatTokens(tokens.outputTokens)}`,
    ];
    if (tokens.cacheCreationTokens > 0 || tokens.cacheReadTokens > 0) {
        parts.push(`${t('format.cache')}: ${formatTokens(tokens.cacheCreationTokens + tokens.cacheReadTokens)}`);
    }
    return `${prefix} ${formatTokens(total)} (${parts.join(', ')})`;
}
export function renderSessionTokensLine(ctx) {
    const display = ctx.config?.display;
    if (display?.showSessionTokens === false) {
        return null;
    }
    const tokens = ctx.transcript.sessionTokens;
    if (!tokens) {
        return null;
    }
    const colors = ctx.config?.colors;
    const summary = formatSessionTokenSummary(tokens, t('label.tokens'));
    return summary ? label(summary, colors) : null;
}
//# sourceMappingURL=session-tokens.js.map