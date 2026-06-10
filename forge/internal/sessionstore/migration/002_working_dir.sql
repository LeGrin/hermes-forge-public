-- Add working_dir column to sessions table.
-- This allows Forge to resume sessions with the correct working directory.
-- W-F1: working_dir is persisted so respawns use the same directory.

ALTER TABLE sessions ADD COLUMN working_dir TEXT NOT NULL DEFAULT '';
