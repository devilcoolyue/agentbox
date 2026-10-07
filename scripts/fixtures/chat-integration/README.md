# Chat reliability fixture

`test-chat-integration.mjs` builds the **real `cmd/agentbox` binary**, then connects
Chrome to its real login, session, history, attachment, receipt and WebSocket APIs.
`chat-integration-harness.mjs` owns startup, signals, evidence and cleanup.

This helper seeds an empty, explicitly marked temporary SQLite database and acts
as a **synthetic Docker Engine endpoint**. It never executes submitted commands.
Codex app-server handshakes terminate before a turn; the production runner takes
its existing exec fallback. The helper holds the exec output until the test
releases synthetic completion/interruption events. It counts runner invocations
independently of database receipts. Its control API requires a random per-run
token and is never part of the production binary.

Control queries read the real SQLite database with `mode=ro`. The one deliberate
fault mutation adds/removes a trigger that rejects usage insertion, exercising
the existing usage/ledger rollback. Every read/mutation requires the fixture
marker; seeding refuses an existing database. Users, passwords, prompts and
attachments are synthetic. No credentials or data from an existing instance are
read, no real provider is called, and no Docker socket is mounted into a test
container.

Run after the normal frontend artifact check:

```sh
python3 scripts/verify.py run --step integration.chat \
  --playwright-module /path/to/playwright/index.mjs --browser-channel chrome

# Root server inside a disposable Linux container preserves production chown.
# Supply a locally prepared image with a working /bin runtime (e.g. alpine:3.22).
python3 scripts/verify.py run --step docker.chat-reliability --image alpine:3.22 \
  --playwright-module /path/to/playwright/index.mjs --browser-channel chrome
```

The native command requires root on Linux; prefer the container command there.
On unprivileged macOS the shared-directory chown is denied: the native matrix
asserts that upload failure and explicitly excludes successful upload/recovery.
The full Linux matrix covers the successful upload and expired-reference path.
The Linux command uses a fresh Docker volume and dedicated bridge, publishes only
random loopback ports, and keeps the server's port stable across restart so the
browser's local copies retain the same origin. A minimal image uses UTC, without
requiring host zoneinfo. Binaries are cross-built for the daemon architecture.
It kills/restarts only its own server container. Cleanup removes only names
created by this run; failures are reported rather than silently accepted.

Evidence under `output/playwright/chat-integration-{native,linux}-<run>/` includes
synthetic server logs, screenshots, the scenario list, actual receipt/usage/ledger
rows, runner counts and cleanup status. The unified runner additionally records
source identity and command outcome. This proves the joined API/browser boundary
and actual process restart, **not real Docker CLI execution, physical network or
power loss, a real model, or device/IME acceptance**. Those evidence boundaries
remain explicit in `docs/milestones/m3.md`.
