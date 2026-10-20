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

`codex-terminal-0.163.0.jsonl` follows the event order of codex-cli 0.163.0
codex-tui rollouts (only line types, timestamps and usage key names were
inspected): `token_usage_record` is written when a response ends and may precede
the tool output, and `item_completed` carries `started_at_ms` for Reasoning and
AgentMessage items. It drives the terminal latency / first-token estimates; a
repeated token_count is not a request and must not move the next start.

`codex-terminal-0.163.0-compaction.jsonl` copies the shape of a codex-cli 0.163.0
codex-tui rollout where a ChatGPT login ran `/compact` (remote compaction): the
`compacted` record's `latest_token_usage_record.usage` is the compaction
request, which never enters `total_token_usage` (the next token_count repeats
the total with a zero `last_token_usage`). An API key / relay account compacts
locally with an ordinary request that does enter the totals right before its
`compacted` record (codex-cli 0.163.0 against a synthetic provider); the tests
build that case inline. Only key names, line order and token counts were
inspected; all values here are synthetic.

Claude terminal parser tests use synthetic assistant records with the top-level
`effort` and `perTurnEffort` keys observed in Claude Code 2.1.278–2.1.296 CLI
transcripts (only metadata shapes were inspected; no conversation was copied).
Only `effort` supplies the effective setting; `perTurnEffort` is not a fallback.
Terminal timing tests add the metadata observed in Claude Code 2.1.296–2.1.303:
`api-request` lines (send time, `id`) referenced by assistant `requestRef` from
2.1.302 on, `parentUuid` chains through `attachment` lines for older versions,
and `apiBlockIndex` / `thinkingDurationMs` on assistant content blocks.
The parser preserves usage when effort is absent or has an unknown shape, and
leaves mixed settings unknown while keeping existing turn/model dedup keys.

The event tests retain synthetic Claude result and Codex exec/app-server samples
for cumulative-vs-incremental semantics. Adapter classification samples live in
`internal/agent/testdata`. These are offline contracts, not a live model-call test.
