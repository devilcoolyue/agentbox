-- Routing is accepted with the original intent; it grants no CLI execution.
CREATE TABLE collaboration_decisions (
    run_id TEXT PRIMARY KEY REFERENCES collaborations(id),
    state TEXT NOT NULL CHECK(state IN ('pending','claimed','frozen','interrupted','deleted')),
    revision INTEGER NOT NULL DEFAULT 1,
    snapshot_json TEXT NOT NULL,
    result_json TEXT NOT NULL DEFAULT 'null',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE decision_attempts (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL UNIQUE REFERENCES collaborations(id),
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    actual_model TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK(state IN ('dispatched','unknown','settled','not_dispatched')),
    reserved_micro_usd INTEGER NOT NULL CHECK(reserved_micro_usd >= 0),
    cost_micro_usd INTEGER NOT NULL DEFAULT 0 CHECK(cost_micro_usd >= 0),
    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK(input_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK(output_tokens >= 0),
    evidence_json TEXT NOT NULL DEFAULT 'null',
    error_code TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TRIGGER revoke_collaboration_decision AFTER UPDATE OF state ON collaborations WHEN NEW.state='deleted'
BEGIN
    UPDATE collaboration_decisions SET state='deleted',snapshot_json='null',result_json='null',revision=revision+1 WHERE run_id=NEW.id;
END;
CREATE TABLE collaboration_decision_defaults (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK(provider IN ('inherit','builtin','jev'))
);
