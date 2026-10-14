// Stored buckets are disjoint for both agents; output is not prompt input.
export function cacheHitText(row: { input_tokens: number; cache_read_tokens: number; cache_write_tokens: number }): string {
  const input = row.input_tokens + row.cache_read_tokens + row.cache_write_tokens;
  return input > 0 ? (100 * row.cache_read_tokens / input).toFixed(1) + "%" : "—";
}

// Compact token counts like sub2api's formatCacheTokens, extended with B:
// one decimal from 1K up, exact digits below. A value that rounds to 1000 of
// one unit moves to the next (999,950 is 1.0M, not 1000.0K). Full values stay
// in tooltips and CSV.
export function fmtTokens(n: number): string {
  if (Math.abs(n) < 1e3) return n.toLocaleString("en-US");
  const units = ["K", "M", "B"];
  let v = n / 1e3, i = 0;
  while (i < units.length - 1 && Math.abs(Number(v.toFixed(1))) >= 1000) {
    v /= 1000;
    i++;
  }
  return v.toFixed(1) + units[i];
}
