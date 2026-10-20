-- Per-request timing the CLI measured itself (Claude Code's OpenTelemetry
-- api_request event): from sending the request to the first stream event
-- (api_ttft_ms) and to the end of the stream (api_duration_ms). Rows find
-- their timing by the provider request id; telemetry that arrives before its
-- row waits in request_timings, keyed per user so another user's process
-- cannot touch it, and is dropped once applied or after a few days.
ALTER TABLE usage_events ADD COLUMN api_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN api_ttft_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_events ADD COLUMN api_duration_ms INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_usage_api_request ON usage_events(api_request_id) WHERE api_request_id != '';
CREATE TABLE request_timings (
    user TEXT NOT NULL,
    request_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    ttft_ms INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    received_at TEXT NOT NULL,
    PRIMARY KEY (user, request_id)
);
CREATE INDEX idx_request_timings_received ON request_timings(received_at);
