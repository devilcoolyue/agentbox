/**
 * Autocompact buffer percentage.
 *
 * NOTE: This value is applied as a percentage of Claude Code's reported
 * context window size. The `33k/200k` example is just the 200k-window case.
 * It is empirically derived from current Claude Code `/context` output, is
 * not officially documented by Anthropic, and may need adjustment if users
 * report mismatches in future Claude Code versions.
 */
export const AUTOCOMPACT_BUFFER_PERCENT = 0.165;
//# sourceMappingURL=constants.js.map