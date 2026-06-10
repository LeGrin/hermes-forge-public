-- v2-001: Add title, executor_session_id, and thread columns for two-way communication.
-- title: user-facing envelope title (required, non-empty)
-- executor_session_id: the session ID of the Forge executor handling this envelope
-- thread: JSON array of Message objects for conversation history
--
-- Idempotency is guaranteed by the schema_migrations tracking table in sqlite.go.
-- Each migration is recorded after successful execution and skipped on re-open.

ALTER TABLE envelopes ADD COLUMN title TEXT NOT NULL DEFAULT '';
ALTER TABLE envelopes ADD COLUMN executor_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE envelopes ADD COLUMN thread TEXT NOT NULL DEFAULT '[]';
