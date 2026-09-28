-- Keep message identities independently of aggregate usage rows. Resuming a
-- thread or restarting the server must not bill an already settled message.
CREATE TABLE IF NOT EXISTS usage_messages (
    session_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    PRIMARY KEY (session_id, message_id)
);
