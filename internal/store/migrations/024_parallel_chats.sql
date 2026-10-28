-- Chat threads of one workspace may run at the same time (chat_parallel.go).
-- An active or unreviewed request now blocks only its own thread, and each
-- thread keeps its own queue of follow-ups.
DROP INDEX idx_chat_request_active;
CREATE UNIQUE INDEX idx_chat_request_active ON chat_requests(session_id, thread_id)
    WHERE state IN ('accepted', 'starting', 'running', 'uncertain');
DROP INDEX idx_chat_request_queue;
CREATE INDEX idx_chat_request_queue ON chat_requests(session_id, thread_id, created_at) WHERE state = 'queued';
