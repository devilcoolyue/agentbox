-- A task belongs to one durable conversation anchor; older tasks stay unbound.
CREATE TABLE collaboration_threads (
    run_id TEXT PRIMARY KEY REFERENCES collaborations(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    anchor INTEGER NOT NULL CHECK(anchor >= 0)
);
CREATE INDEX idx_collaboration_threads ON collaboration_threads(session_id,thread_id,anchor,run_id);
CREATE TABLE collaboration_supplements (
    run_id TEXT NOT NULL REFERENCES collaborations(id),
    action_id TEXT NOT NULL,
    applied_version INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(run_id,action_id)
);
CREATE TABLE chat_collaboration_context (
    turn_id TEXT NOT NULL,
    run_id TEXT NOT NULL REFERENCES collaborations(id),
    context_json TEXT NOT NULL,
    PRIMARY KEY(turn_id,run_id)
);
CREATE TRIGGER clear_chat_collaboration_context AFTER UPDATE OF request_json ON chat_requests WHEN NEW.request_json='null'
BEGIN DELETE FROM chat_collaboration_context WHERE turn_id=NEW.turn_id; END;
CREATE TRIGGER revoke_chat_collaboration_context AFTER UPDATE OF state ON collaborations WHEN NEW.state='deleted'
BEGIN DELETE FROM chat_collaboration_context WHERE run_id=NEW.id; END;
CREATE TABLE collaboration_applications (
    run_id TEXT NOT NULL REFERENCES collaborations(id),
    action_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('staging','switching','completed','failed','uncertain')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(run_id,action_id)
);
CREATE UNIQUE INDEX idx_collaboration_application_active ON collaboration_applications(session_id) WHERE state IN ('staging','switching','uncertain');
