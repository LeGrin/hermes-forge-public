-- Repair migration: backfill insert_order for rows that have insert_order=0.
-- Handles both fresh installs and already-migrated DBs with mixed state
-- (some rows with insert_order=0, others with correct positive values).
--
-- The UPDATE WHERE clause ensures idempotency: only rows with insert_order=0 are updated.
-- If all rows already have correct insert_order values, this is a no-op.
--
-- Fresh installs: all rows have insert_order=0, so they get sequential values 1..N.
--
-- Already-migrated DBs with mixed state: 
--   - Rows already with positive insert_order keep their values
--   - Rows with insert_order=0 are numbered sequentially starting after
--     the maximum existing positive value, preserving relative rowid order
--
-- The UPDATE runs FIRST to fix duplicate zero values before creating the unique index.
-- If any uniqueness issues remain after the backfill, the index creation will fail
-- (which indicates a real data problem, not a migration issue).
--
-- Add unique index to catch any remaining race conditions on concurrent inserts.
UPDATE session_messages
SET insert_order = (
    -- Count of insert_order=0 rows that come before this row in rowid order
    -- (these are the pre-migration rows that need numbering)
    SELECT COUNT(*) FROM session_messages b
    WHERE b.session_id = session_messages.session_id
      AND b.insert_order = 0
      AND b.rowid <= session_messages.rowid
) + (
    -- Max existing positive insert_order in this session (0 if none)
    SELECT COALESCE(MAX(insert_order), 0) FROM session_messages c
    WHERE c.session_id = session_messages.session_id
      AND c.insert_order > 0
)
WHERE insert_order = 0;

CREATE UNIQUE INDEX IF NOT EXISTS idx_session_messages_session_order
ON session_messages(session_id, insert_order);
