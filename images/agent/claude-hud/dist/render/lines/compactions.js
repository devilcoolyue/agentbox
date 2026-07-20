import { label } from '../colors.js';
import { t } from '../../i18n/index.js';
export function renderCompactionsLine(ctx) {
    const display = ctx.config?.display;
    if (display?.showCompactions !== true) {
        return null;
    }
    const compactions = ctx.transcript.compactionCount ?? 0;
    if (compactions === 0) {
        return null;
    }
    const colors = ctx.config?.colors;
    return label(`${t('label.compactions')}: ${compactions}`, colors);
}
//# sourceMappingURL=compactions.js.map