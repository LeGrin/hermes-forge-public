-- Notification queue for status changes that KITT should know about.
-- Hermes inserts a row when an envelope reaches an "interesting" status
-- (done, blocked, failed, paused, awaiting_confirm). KITT polls
-- GET /notifications and ACKs after processing.

CREATE TABLE IF NOT EXISTS notifications (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    envelope_id TEXT    NOT NULL,
    status      TEXT    NOT NULL,
    note        TEXT    NOT NULL DEFAULT '',
    proof_summary TEXT  NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    acknowledged INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_notifications_unack
    ON notifications(acknowledged) WHERE acknowledged = 0;
