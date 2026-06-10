-- Session register for Forge.
-- W-F1: Forge persists sessions across restart via this SQLite table.
-- There is no DELETE statement in this file or in the sessionstore
-- package — sessions are append-only (may transition to 'closed' or
-- 'lost' but are never removed).

CREATE TABLE IF NOT EXISTS sessions (
    session_id   TEXT PRIMARY KEY,
    envelope_id  TEXT NOT NULL,
    executor     TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'starting'
                 CHECK (state IN ('starting', 'live', 'closed', 'lost')),
    started_at   DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_envelope
    ON sessions(envelope_id);
