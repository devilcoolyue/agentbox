// Stored buckets are disjoint for both agents; output is not prompt input.
export function cacheHitText(row) {
    const input = row.input_tokens + row.cache_read_tokens + row.cache_write_tokens;
    return input > 0 ? (100 * row.cache_read_tokens / input).toFixed(1) + "%" : "—";
}
// Compact token counts like sub2api's formatCacheTokens, extended with B:
// one decimal from 1K up, exact digits below. A value that rounds to 1000 of
// one unit moves to the next (999,950 is 1.0M, not 1000.0K). Full values stay
// in tooltips and CSV.
export function fmtTokens(n) {
    if (Math.abs(n) < 1e3)
        return n.toLocaleString("en-US");
    const units = ["K", "M", "B"];
    let v = n / 1e3, i = 0;
    while (i < units.length - 1 && Math.abs(Number(v.toFixed(1))) >= 1000) {
        v /= 1000;
        i++;
    }
    return v.toFixed(1) + units[i];
}
// Output speed of one request: output tokens ÷ the time from the first token
// to the end of the stream. The output count includes thinking, so this is
// how fast the model generated, not how fast visible text appeared.
//
//   measured       the CLI's own timing for this request (api_ttft_ms /
//                  api_duration_ms from Claude Code's telemetry), any row kind;
//   log            a terminal row timed from CLI log timestamps that also
//                  pinned the first token (a reply that opens with thinking);
//   log-with-wait  a terminal row whose log gave only the whole duration, so
//                  the first-token wait is in the divisor and the number reads
//                  low (cc-switch's session-log rule).
//
// Short windows say nothing: a few dozen tokens often arrive in one burst
// right after the first event. Below SPEED_MIN_OUTPUT_TOKENS output or
// SPEED_MIN_WINDOW_MS of generation there is no speed; log-with-wait keeps
// cc-switch's stricter 200 tokens / 1 s, since the wait would dominate.
// Chat rows without the CLI's timing carry turn-level numbers that span tool
// runs, so they never get one.
const SPEED_MIN_OUTPUT_TOKENS = 100;
const SPEED_MIN_WINDOW_MS = 500;
const WAIT_MIN_OUTPUT_TOKENS = 200;
const WAIT_MIN_DURATION_MS = 1000;
export function outputSpeed(row) {
    const out = row.output_tokens;
    const dur = row.api_duration_ms || 0;
    if (dur > 0) {
        const ttft = row.api_ttft_ms || 0;
        if (!ttft)
            return { tps: null, why: "untimed" };
        return rate(out, dur - ttft, SPEED_MIN_OUTPUT_TOKENS, SPEED_MIN_WINDOW_MS, "measured");
    }
    if (row.kind !== "terminal")
        return { tps: null, why: "turn" };
    if (!row.wall_ms)
        return { tps: null, why: "untimed" };
    if (row.ttft_ms)
        return rate(out, row.wall_ms - row.ttft_ms, SPEED_MIN_OUTPUT_TOKENS, SPEED_MIN_WINDOW_MS, "log");
    return rate(out, row.wall_ms, WAIT_MIN_OUTPUT_TOKENS, WAIT_MIN_DURATION_MS, "log-with-wait");
}
function rate(out, ms, minOut, minMs, basis) {
    if (out < minOut || ms < minMs)
        return { tps: null, why: "short" };
    return { tps: out / (ms / 1000), basis };
}
// tok/s: whole numbers from 1 up, one decimal below.
export function fmtSpeed(tps) {
    return tps >= 1 ? String(Math.round(tps)) : tps.toFixed(1);
}
