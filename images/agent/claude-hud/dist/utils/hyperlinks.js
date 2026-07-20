import * as path from 'node:path';
import { pathToFileURL } from 'node:url';
import { sanitizeDisplayText } from './sanitize.js';
/**
 * Wrap `text` in an OSC 8 hyperlink pointing to `uri`.
 */
export function hyperlink(uri, text) {
    const esc = '\x1b';
    const st = '\\';
    return `${esc}]8;;${uri}${esc}${st}${text}${esc}]8;;${esc}${st}`;
}
/**
 * Convert a filesystem path to a `file://` URL string.
 * Returns `null` on failure.
 */
export function getFileHref(filePath) {
    try {
        return pathToFileURL(path.resolve(filePath)).toString();
    }
    catch {
        return null;
    }
}
/**
 * Wrap `text` in an OSC 8 hyperlink after validating the URI.
 *
 * Only `https:` and `file:` protocols are allowed. Returns plain `text`
 * when the URI is missing, invalid, or uses a disallowed protocol.
 *
 * @param uri       - The URI to link to (may be undefined/null).
 * @param text      - The visible text to display.
 * @param protocols - Allowed URL protocols (default: `['https:', 'file:']`).
 */
export function safeHyperlink(uri, text, protocols = ['https:', 'file:']) {
    if (!uri) {
        return text;
    }
    const sanitizedUri = sanitizeDisplayText(uri);
    try {
        const parsed = new URL(sanitizedUri);
        if (!protocols.includes(parsed.protocol)) {
            return text;
        }
        return hyperlink(parsed.toString(), text);
    }
    catch {
        return text;
    }
}
//# sourceMappingURL=hyperlinks.js.map