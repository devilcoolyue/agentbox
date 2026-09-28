// Stored buckets are disjoint for both agents; output is not prompt input.
export function cacheHitText(row) {
    const input = row.input_tokens + row.cache_read_tokens + row.cache_write_tokens;
    return input > 0 ? (100 * row.cache_read_tokens / input).toFixed(1) + "%" : "—";
}
