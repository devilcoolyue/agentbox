-- Receipts outlive deleted sessions/threads; never cascade away the ID fence.
CREATE TABLE chat_requests (
    user TEXT NOT NULL,
    user_created_at TEXT NOT NULL,
    session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    request_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'accepted', 'starting', 'running', 'completed', 'failed',
        'interrupted', 'uncertain', 'reviewed', 'abandoned', 'deleted'
    )),
    error_code TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user, user_created_at, session_id, request_id)
);
CREATE UNIQUE INDEX idx_chat_request_turn ON chat_requests(turn_id) WHERE turn_id != '';
CREATE UNIQUE INDEX idx_chat_request_active ON chat_requests(session_id)
    WHERE state IN ('accepted', 'starting', 'running', 'uncertain');
CREATE INDEX idx_chat_request_thread ON chat_requests(session_id, thread_id);
