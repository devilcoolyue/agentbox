-- Durable two-role collaboration tasks. Provider credentials never live here;
-- bindings contain only account/model selectors and are re-authorized before use.
CREATE TABLE collaborations (
    id TEXT PRIMARY KEY,
    user TEXT NOT NULL,
    user_created_at TEXT NOT NULL,
    source_session_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    request_json TEXT NOT NULL,
    current_scope TEXT NOT NULL DEFAULT '',
    cleanup_pending INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL CHECK (state IN ('accepted','preparing','planning','awaiting_plan','executing','waiting_answer','waiting_user','reviewing','paused','cancelling','cancelled','completed','failed','uncertain','deleted')),
    phase TEXT NOT NULL DEFAULT 'preparing',
    wait_reason TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    config_version INTEGER NOT NULL DEFAULT 1 CHECK (config_version > 0),
    plan_version INTEGER NOT NULL DEFAULT 0 CHECK (plan_version >= 0),
    max_repairs INTEGER NOT NULL DEFAULT 2 CHECK (max_repairs >= 0),
    max_steps INTEGER NOT NULL DEFAULT 20 CHECK (max_steps > 0),
    steps_used INTEGER NOT NULL DEFAULT 0 CHECK (steps_used >= 0),
    pause_requested INTEGER NOT NULL DEFAULT 0,
    repairs_used INTEGER NOT NULL DEFAULT 0,
    snapshot_id TEXT NOT NULL DEFAULT '',
    pending_question TEXT NOT NULL DEFAULT '',
    resume_state TEXT NOT NULL DEFAULT '',
    next_kind TEXT NOT NULL DEFAULT '',
    image_id TEXT NOT NULL DEFAULT '',
    budget_micro_usd INTEGER NOT NULL DEFAULT 0 CHECK (budget_micro_usd >= 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(user, user_created_at, request_id)
);
CREATE INDEX idx_collaborations_user ON collaborations(user, user_created_at, updated_at);

CREATE TABLE collaboration_configs (
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    config_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(run_id, version)
);

CREATE TABLE collaboration_participants (
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('coordinator','executor')),
    generation INTEGER NOT NULL CHECK (generation > 0),
    config_version INTEGER NOT NULL CHECK (config_version > 0),
    session_id TEXT NOT NULL,
    provider_session TEXT NOT NULL DEFAULT '',
    process_epoch INTEGER NOT NULL DEFAULT 0 CHECK (process_epoch >= 0),
    stopped_epoch INTEGER NOT NULL DEFAULT 0 CHECK (stopped_epoch >= 0 AND stopped_epoch <= process_epoch),
    state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','stopped','revoked')),
    created_at TEXT NOT NULL,
    PRIMARY KEY(run_id, role, generation),
    UNIQUE(session_id)
);

CREATE TABLE collaboration_steps (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('plan','execute','clarify','review','repair','verify','handoff_repair')),
    role TEXT NOT NULL CHECK (role IN ('coordinator','executor')),
    participant_generation INTEGER NOT NULL CHECK (participant_generation > 0),
    config_version INTEGER NOT NULL CHECK (config_version > 0),
    plan_version INTEGER NOT NULL CHECK (plan_version >= 0),
    attempt INTEGER NOT NULL DEFAULT 1 CHECK (attempt > 0),
    turn_id TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL DEFAULT '',
    ended_at TEXT NOT NULL DEFAULT '',
    input_fingerprint TEXT NOT NULL,
    input_json TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('accepted','starting','running','finalizing','completed','failed','interrupted','uncertain','reviewed','cancelled')),
    error_code TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    snapshot_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(run_id, kind, attempt)
);
CREATE UNIQUE INDEX idx_collaboration_turn ON collaboration_steps(turn_id) WHERE turn_id != '';
CREATE UNIQUE INDEX idx_collaboration_active_step ON collaboration_steps(run_id) WHERE state IN ('accepted','starting','running','finalizing','uncertain');

CREATE TABLE collaboration_messages (
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL CHECK (seq > 0),
    id TEXT NOT NULL,
    step_id TEXT NOT NULL,
    sender_role TEXT NOT NULL CHECK (sender_role IN ('coordinator','executor','user','system')),
    recipient_role TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL CHECK (type IN ('plan','question','answer','result','review','system')),
    config_version INTEGER NOT NULL CHECK (config_version > 0),
    plan_version INTEGER NOT NULL CHECK (plan_version >= 0),
    reply_to TEXT NOT NULL DEFAULT '',
    body_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(run_id, seq),
    UNIQUE(run_id, id)
);

CREATE TABLE collaboration_actions (
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    action_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    action_json TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'accepted',
    result_json TEXT NOT NULL DEFAULT '{}',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(run_id, action_id)
);

CREATE TABLE collaboration_questions (
    run_id TEXT NOT NULL REFERENCES collaborations(id),
    message_id TEXT NOT NULL,
    origin_kind TEXT NOT NULL,
    parent_question TEXT NOT NULL DEFAULT '',
    config_version INTEGER NOT NULL,
    plan_version INTEGER NOT NULL,
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','answered','retired')),
    PRIMARY KEY(run_id,message_id)
);

CREATE TABLE collaboration_artifacts (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    step_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    storage_key TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    state TEXT NOT NULL CHECK (state IN ('staging','ready','missing','revoked')),
    created_at TEXT NOT NULL
);

CREATE TABLE collaboration_events (
    run_id TEXT NOT NULL REFERENCES collaborations(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL CHECK (seq > 0),
    type TEXT NOT NULL,
    body_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(run_id, seq)
);

CREATE TABLE collaboration_verifications (
    run_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL,
    config_version INTEGER NOT NULL,
    command TEXT NOT NULL,
    helper_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('running','completed','failed','uncertain','not_requested')),
    output TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    exit_code INTEGER,
    output_truncated INTEGER NOT NULL DEFAULT 0,
    argv_json TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(run_id,snapshot_id,config_version)
);
