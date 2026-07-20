/**
 * Truncate a string to `maxLen` characters, appending an ellipsis when
 * the string exceeds the limit.
 *
 * @param text    - The string to truncate.
 * @param maxLen  - Maximum character length (including the suffix).
 * @param suffix  - The truncation indicator (default: `'...'`).
 */
export function truncateString(text, maxLen, suffix = '...') {
    if (!text)
        return '';
    if (text.length <= maxLen)
        return text;
    return text.slice(0, Math.max(0, maxLen - suffix.length)) + suffix;
}
//# sourceMappingURL=truncate.js.map