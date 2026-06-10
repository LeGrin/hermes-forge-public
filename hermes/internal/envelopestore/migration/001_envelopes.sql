-- H-005b-2: initial envelope table.
-- Single table, JSON text columns for nested structures (delivery, metrics,
-- history, proof, and the string-array fields). One index on status because
-- the delivery worker will claim by status (H-005e).
--
-- W-H17: there is no DELETE statement anywhere in this file or in the Go
-- store package. Envelopes are append-only; terminal rows stay.

CREATE TABLE IF NOT EXISTS envelopes (
  id                  TEXT     PRIMARY KEY,
  created_at          DATETIME NOT NULL,
  created_by          TEXT     NOT NULL,
  domain              TEXT     NOT NULL DEFAULT '',
  project             TEXT     NOT NULL DEFAULT '',
  target_node         TEXT     NOT NULL DEFAULT '',
  target_executor     TEXT     NOT NULL,
  task_title          TEXT     NOT NULL,
  task_goal           TEXT     NOT NULL DEFAULT '',
  task_steps          TEXT     NOT NULL DEFAULT '[]',
  success_criteria    TEXT     NOT NULL DEFAULT '[]',
  escalation_criteria TEXT     NOT NULL DEFAULT '[]',
  proof_required      TEXT     NOT NULL DEFAULT '[]',
  status              TEXT     NOT NULL,
  delivery            TEXT     NOT NULL DEFAULT '{}',
  capability_hints    TEXT     NOT NULL DEFAULT '[]',
  session_binding     TEXT,
  metrics             TEXT     NOT NULL DEFAULT '{}',
  history             TEXT     NOT NULL DEFAULT '[]',
  proof               TEXT     NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_envelopes_status ON envelopes(status);
