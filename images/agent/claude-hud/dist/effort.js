const KNOWN_SYMBOLS = {
    low: '○',
    medium: '◔',
    high: '◑',
    xhigh: '◕',
    max: '●',
};
/**
 * Resolve the current session's effort level.
 *
 * Resolution order (matches `extractEffortString` below):
 * 1. stdin.effort as non-empty string — original PR #471 future-proofed path.
 * 2. stdin.effort as object with string `level` — Claude Code 2.1.115+ schema
 *    (e.g., `{ "level": "max" }`).
 * 3. null.
 *
 * When the transcript marks ultracode active (`options.ultracodeActive`), the
 * reported level is wrapped as `ultracode(<level>)` — the level is taken as-is,
 * not assumed to be `xhigh`.
 *
 * Non-matching inputs (numbers, booleans, arrays, objects without a string
 * `level`) return null rather than crashing.
 */
export function resolveEffortLevel(stdinEffort, options = {}) {
    const fromStdin = extractEffortString(stdinEffort);
    if (!fromStdin) {
        return null;
    }
    const info = formatEffort(fromStdin);
    // Ultracode reaches stdin as a normal level, so the transcript marker is the
    // only signal; wrap the reported level rather than assuming xhigh.
    return options.ultracodeActive === true
        ? { level: `ultracode(${info.level})`, symbol: info.symbol }
        : info;
}
function extractEffortString(value) {
    if (typeof value === 'string') {
        return value.length > 0 ? value : null;
    }
    if (value && typeof value === 'object' && !Array.isArray(value)) {
        const level = value.level;
        if (typeof level === 'string' && level.length > 0) {
            return level;
        }
    }
    return null;
}
function formatEffort(level) {
    const normalized = normalizeEffort(level);
    return { level: normalized, symbol: KNOWN_SYMBOLS[normalized] ?? '' };
}
function normalizeEffort(level) {
    return level?.toLowerCase().trim() ?? '';
}
//# sourceMappingURL=effort.js.map