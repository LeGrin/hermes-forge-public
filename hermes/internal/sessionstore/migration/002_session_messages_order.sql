-- Add monotonically increasing insert_order for deterministic message ordering.
-- SQLite does not allow adding a PRIMARY KEY column via ALTER TABLE, so we
-- add a regular INTEGER column with a trigger to auto-increment per session.
-- The backfill is handled by migration 003 so this migration is idempotent
-- for both fresh installs and already-migrated DBs.

ALTER TABLE session_messages ADD COLUMN insert_order INTEGER NOT NULL DEFAULT 0;

-- Trigger to assign monotonically increasing insert_order per session for new inserts.
CREATE TRIGGER IF NOT EXISTS set_insert_order
AFTER INSERT ON session_messages
FOR EACH ROW
WHEN NEW.insert_order = 0
BEGIN
    UPDATE session_messages
    SET insert_order = (
        SELECT COALESCE(MAX(insert_order), 0) + 1
        FROM session_messages
        WHERE session_id = NEW.session_id
    )
    WHERE id = NEW.id;
END;
