CREATE TABLE IF NOT EXISTS api_keys (
    key        TEXT PRIMARY KEY,
    label      TEXT NOT NULL UNIQUE,
    role       TEXT NOT NULL DEFAULT 'user' CHECK(role IN ('user', 'admin')),
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
