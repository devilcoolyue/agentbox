// Shared debug logging utility
// Enable via: DEBUG=claude-hud or DEBUG=*
const DEBUG = process.env.DEBUG?.includes('claude-hud') || process.env.DEBUG === '*';
/**
 * Create a namespaced debug logger
 * @param namespace - Tag for log messages (e.g., 'config', 'usage')
 */
export function createDebug(namespace) {
    return function debug(msg, ...args) {
        if (DEBUG) {
            console.error(`[claude-hud:${namespace}] ${msg}`, ...args);
        }
    };
}
//# sourceMappingURL=debug.js.map