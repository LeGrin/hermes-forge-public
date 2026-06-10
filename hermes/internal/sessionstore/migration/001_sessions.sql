-- Session lane for Hermes: lightweight conversation between KITT, OpenCode, and Claude.
-- Sessions are created when OpenCode/Claude need to communicate back to KITT.
-- Messages within a session are append-only.

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
    title       TEXT NOT NULL DEFAULT '',
    project     TEXT NOT NULL DEFAULT '',
    api_key     TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'active'
);

CREATE INDEX IF NOT EXISTS idx_sessions_api_key ON sessions(api_key);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);

CREATE TABLE IF NOT EXISTS session_messages (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL,
    msg_from    TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'reply',
    text        TEXT NOT NULL,
    reply_to    TEXT,
    created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);

CREATE INDEX IF NOT EXISTS idx_session_messages_session ON session_messages(session_id);
CREATE INDEX IF NOT EXISTS idx_session_messages_created ON session_messages(created_at);