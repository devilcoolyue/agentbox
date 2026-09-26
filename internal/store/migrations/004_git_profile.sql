CREATE TABLE IF NOT EXISTS git_profiles (
	user       TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	email      TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
