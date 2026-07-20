import { dim, label } from '../colors.js';
import { getFileHref, safeHyperlink } from '../../utils/hyperlinks.js';
import { sanitizeDisplayText } from '../../utils/sanitize.js';
export function sanitize(value) {
    return sanitizeDisplayText(value);
}
export function basenameOf(dir) {
    const segments = dir.split(/[/\\]/).filter(Boolean);
    return segments[segments.length - 1] ?? dir;
}
export const MAX_RENDERED_ADDED_DIRS = 5;
export const MAX_ADDED_DIR_NAME_LEN = 24;
// Length is measured in UTF-16 code units, not grapheme clusters; a name
// of mostly 4-byte codepoints (emoji, rare CJK) may render slightly wider
// than MAX_ADDED_DIR_NAME_LEN. Acceptable simplification for a statusline.
export function truncateBasename(name) {
    if (name.length <= MAX_ADDED_DIR_NAME_LEN)
        return name;
    return name.slice(0, MAX_ADDED_DIR_NAME_LEN - 1) + '…';
}
export function normalizeAddedDirs(value) {
    if (!Array.isArray(value))
        return [];
    return value.filter((v) => typeof v === 'string' &&
        v.length > 0 &&
        sanitize(basenameOf(v)).length > 0);
}
export function renderAddedDirsLine(ctx) {
    const display = ctx.config?.display;
    if (display?.showAddedDirs === false)
        return null;
    if ((display?.addedDirsLayout ?? 'inline') !== 'line')
        return null;
    const dirs = normalizeAddedDirs(ctx.stdin.workspace?.added_dirs);
    if (dirs.length === 0)
        return null;
    const colors = ctx.config?.colors;
    const visible = dirs.slice(0, MAX_RENDERED_ADDED_DIRS);
    const overflow = dirs.length - visible.length;
    const rendered = visible.map((dir) => {
        const name = truncateBasename(sanitize(basenameOf(dir)));
        return safeHyperlink(getFileHref(dir), dim(name));
    });
    if (overflow > 0) {
        rendered.push(dim(`+${overflow} more`));
    }
    return `${label('Added dirs:', colors)} ${rendered.join(dim(', '))}`;
}
//# sourceMappingURL=added-dirs.js.map