-- Retire the staged collaboration feature (013-016). Its participant workspaces
-- were hidden managed sessions; remove those rows before their marker table so
-- they can never surface as ordinary workspaces. Usage and credit history stay.
DELETE FROM sessions WHERE id IN (SELECT session_id FROM collaboration_participants);
DROP TRIGGER IF EXISTS clear_chat_collaboration_context;
DROP TRIGGER IF EXISTS revoke_chat_collaboration_context;
DROP TRIGGER IF EXISTS revoke_collaboration_decision;
DROP TABLE IF EXISTS collaboration_review_evidence;
DROP TABLE IF EXISTS collaboration_decision_defaults;
DROP TABLE IF EXISTS decision_attempts;
DROP TABLE IF EXISTS collaboration_decisions;
DROP TABLE IF EXISTS collaboration_applications;
DROP TABLE IF EXISTS chat_collaboration_context;
DROP TABLE IF EXISTS collaboration_supplements;
DROP TABLE IF EXISTS collaboration_threads;
DROP TABLE IF EXISTS collaboration_verifications;
DROP TABLE IF EXISTS collaboration_events;
DROP TABLE IF EXISTS collaboration_artifacts;
DROP TABLE IF EXISTS collaboration_questions;
DROP TABLE IF EXISTS collaboration_actions;
DROP TABLE IF EXISTS collaboration_messages;
DROP TABLE IF EXISTS collaboration_steps;
DROP TABLE IF EXISTS collaboration_participants;
DROP TABLE IF EXISTS collaboration_configs;
DROP TABLE IF EXISTS collaborations;

-- A worker workspace runs another account or CLI against its owner's files.
-- It is a hidden session: listed nowhere, reachable only by the delegation
-- runtime, and removed together with its owner.
CREATE TABLE delegation_workers (
    session_id TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL,
    account_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(owner_session_id, account_id)
);

-- One row per delegate tool call made by a main agent turn. Text columns are
-- bounded by the server and cleared when the owning workspace is deleted; the
-- metadata stays for the per-model statistics shown next to the tags.
CREATE TABLE delegations (
    id TEXT PRIMARY KEY,
    user TEXT NOT NULL,
    session_id TEXT NOT NULL,
    thread_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK(seq >= 1),
    state TEXT NOT NULL CHECK(state IN ('routing','running','completed','failed','declined','cancelled','uncertain')),
    task_type TEXT NOT NULL,
    risks_json TEXT NOT NULL DEFAULT '[]',
    access TEXT NOT NULL CHECK(access IN ('read','write')),
    task TEXT NOT NULL,
    route_json TEXT NOT NULL DEFAULT 'null',
    worker_session_id TEXT NOT NULL DEFAULT '',
    agent TEXT NOT NULL DEFAULT '',
    account_id TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    provider_session TEXT NOT NULL DEFAULT '',
    continue_of TEXT NOT NULL DEFAULT '',
    report TEXT NOT NULL DEFAULT '',
    changes_json TEXT NOT NULL DEFAULT 'null',
    error_code TEXT NOT NULL DEFAULT '',
    feedback TEXT NOT NULL DEFAULT '' CHECK(feedback IN ('','ok','minor_fix','redo','abandoned')),
    feedback_note TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    started_at TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT '',
    UNIQUE(turn_id, seq)
);
CREATE INDEX idx_delegations_session ON delegations(session_id, created_at);
CREATE INDEX idx_delegations_user ON delegations(user, created_at);
CREATE INDEX idx_delegations_model ON delegations(account_id, model, task_type);

ALTER TABLE usage_events ADD COLUMN delegation_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_usage_delegation ON usage_events(delegation_id) WHERE delegation_id != '';
