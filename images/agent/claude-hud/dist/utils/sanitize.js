/**
 * Regex matching display-unsafe control characters and bidi overrides.
 *
 * Covers C0 (U+0000–U+001F), DEL + C1 (U+007F–U+009F), Arabic bidi
 * mark (U+061C), LRM/RLM (U+200E–U+200F), bidi embedding/override
 * (U+202A–U+202E), bidi isolate (U+2066–U+2069), and deprecated
 * formatting chars (U+206A–U+206F).
 */
const CONTROL_AND_BIDI_PATTERN = new RegExp('[' +
    '\\u0000-\\u001F\\u007F-\\u009F' +
    '\\u061C\\u200E\\u200F' +
    '\\u202A-\\u202E\\u2066-\\u2069\\u206A-\\u206F' +
    ']', 'g');
/**
 * Strip ANSI escape sequences (CSI, OSC, 7-bit C1/Fe), control characters,
 * and bidi overrides from an untrusted string.
 *
 * Use this whenever displaying user-supplied or external text in the
 * terminal to prevent escape injection and layout corruption.
 */
export function sanitizeDisplayText(input) {
    return input
        .replace(/\x1B\[[0-?]*[ -/]*[@-~]/g, '') // CSI sequences
        .replace(/\x1B\][^\x07\x1B]*(?:\x07|\x1B\\)/g, '') // OSC sequences
        .replace(/\x1B[@-Z\\-_]/g, '') // 7-bit C1 / ESC Fe
        .replace(CONTROL_AND_BIDI_PATTERN, ''); // control + bidi chars
}
//# sourceMappingURL=sanitize.js.map