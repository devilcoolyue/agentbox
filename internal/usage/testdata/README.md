# Usage protocol fixtures

All IDs, timestamps, text and numbers are synthetic; no user transcript or credential is included.

`codex-terminal-0.145.0.jsonl` follows the public `rust-v0.145.0` definitions in
https://github.com/openai/codex/blob/rust-v0.145.0/codex-rs/protocol/src/protocol.rs
(`SessionMeta`, `TurnContextItem`, `TokenUsageInfo`, `TokenCountEvent`, `RolloutItem`).
`task_started`/`task_complete` and their v2 aliases identify turns. `append_last_usage`
adds each usage result to `total_token_usage`; token_count can repeat the same info
for rate-limit changes, so the parser uses cumulative deltas rather than summing
replayed last_token_usage. Reasoning is already part of output. Missing originator,
turn ID, cumulative usage or unknown formats are not guessed into billable rows.
`TurnContextItem.effort` is the recorded reasoning setting; fixtures include a
model change with a different effort and a later turn with no effort. Missing
effort must not inherit the previous turn or today's account configuration.

Claude terminal parser tests use synthetic assistant records with the top-level
`effort` and `perTurnEffort` keys observed in Claude Code 2.1.278–2.1.296 CLI
transcripts (only metadata shapes were inspected; no conversation was copied).
Only `effort` supplies the effective setting; `perTurnEffort` is not a fallback.
The parser preserves usage when effort is absent or has an unknown shape, and
leaves mixed settings unknown while keeping existing turn/model dedup keys.

The event tests retain synthetic Claude result and Codex exec/app-server samples
for cumulative-vs-incremental semantics. Adapter classification samples live in
`internal/agent/testdata`. These are offline contracts, not a live model-call test.
