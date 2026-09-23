CREATE TABLE IF NOT EXISTS sessions (
	id           TEXT PRIMARY KEY,
	user         TEXT NOT NULL,
	name         TEXT NOT NULL,
	agent        TEXT NOT NULL,
	account_id   TEXT NOT NULL,
	container_id TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL,
	chat_session TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user);
CREATE TABLE IF NOT EXISTS users (
	name       TEXT PRIMARY KEY,
	role       TEXT NOT NULL,
	pass_hash  TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	token      TEXT PRIMARY KEY,
	user       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tokens_user ON tokens(user);
CREATE TABLE IF NOT EXISTS usage_events (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	ts                 TEXT    NOT NULL,
	user               TEXT    NOT NULL,
	session_id         TEXT    NOT NULL,
	thread_id          TEXT    NOT NULL DEFAULT '',
	turn_id            TEXT    NOT NULL DEFAULT '',
	agent              TEXT    NOT NULL DEFAULT '',
	account_id         TEXT    NOT NULL DEFAULT '',
	model              TEXT    NOT NULL DEFAULT '',
	kind               TEXT    NOT NULL DEFAULT 'chat',
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micro_usd     INTEGER NOT NULL DEFAULT 0,
	duration_ms        INTEGER NOT NULL DEFAULT 0,
	wall_ms            INTEGER NOT NULL DEFAULT 0,
	ttft_ms            INTEGER NOT NULL DEFAULT 0,
	provider           TEXT    NOT NULL DEFAULT '',
	req_id             TEXT    NOT NULL DEFAULT '',
	raw                TEXT    NOT NULL DEFAULT ''
);
-- 注意：req_id 上的唯一索引只能建在 migrate() 里，不能放这儿。已有的库对
-- CREATE TABLE IF NOT EXISTS 是空操作，此刻 req_id 这一列还没被 ALTER 加上，
-- 在这里建索引会让整段 schema 执行失败、服务起不来。
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_events(ts);
CREATE INDEX IF NOT EXISTS idx_usage_user_ts ON usage_events(user, ts);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage_events(session_id);
CREATE INDEX IF NOT EXISTS idx_usage_turn ON usage_events(turn_id);
CREATE TABLE IF NOT EXISTS quotas (
	user              TEXT PRIMARY KEY,
	enforced          INTEGER NOT NULL DEFAULT 1,
	balance_micro_usd INTEGER NOT NULL DEFAULT 0,
	granted_micro_usd INTEGER NOT NULL DEFAULT 0,
	spent_micro_usd   INTEGER NOT NULL DEFAULT 0,
	created_at        TEXT NOT NULL,
	updated_at        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS credit_ledger (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	ts              TEXT    NOT NULL,
	user            TEXT    NOT NULL,
	ref             TEXT    NOT NULL,
	reason          TEXT    NOT NULL,
	delta_micro_usd INTEGER NOT NULL,
	balance_after   INTEGER NOT NULL,
	note            TEXT    NOT NULL DEFAULT '',
	actor           TEXT    NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_ref ON credit_ledger(ref);
CREATE INDEX IF NOT EXISTS idx_ledger_user_ts ON credit_ledger(user, ts);
