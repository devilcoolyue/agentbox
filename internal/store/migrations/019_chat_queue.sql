-- Follow-up messages sent while a turn runs. A queued request waits for the
-- session's current turn and holds no active slot; a steered one was handed to
-- the running turn (merged_into names its request) and is settled on delivery.
-- SQLite cannot widen a CHECK in place, so the table is rebuilt in rowid order.
CREATE TABLE chat_requests_v19 (
    user TEXT NOT NULL,
    user_created_at TEXT NOT NULL,
    session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    request_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN (
        'queued', 'accepted', 'starting', 'running', 'completed', 'failed',
        'interrupted', 'uncertain', 'reviewed', 'abandoned', 'deleted'
    )),
    error_code TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    merged_into TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user, user_created_at, session_id, request_id)
);
INSERT INTO chat_requests_v19(user,user_created_at,session_id,request_id,thread_id,turn_id,fingerprint,request_json,state,error_code,revision,created_at,updated_at)
    SELECT user,user_created_at,session_id,request_id,thread_id,turn_id,fingerprint,request_json,state,error_code,revision,created_at,updated_at
    FROM chat_requests ORDER BY rowid;
DROP TABLE chat_requests;
ALTER TABLE chat_requests_v19 RENAME TO chat_requests;
CREATE UNIQUE INDEX idx_chat_request_turn ON chat_requests(turn_id) WHERE turn_id != '';
CREATE UNIQUE INDEX idx_chat_request_active ON chat_requests(session_id)
    WHERE state IN ('accepted', 'starting', 'running', 'uncertain');
CREATE INDEX idx_chat_request_thread ON chat_requests(session_id, thread_id);
CREATE INDEX idx_chat_request_queue ON chat_requests(session_id, created_at) WHERE state = 'queued';
