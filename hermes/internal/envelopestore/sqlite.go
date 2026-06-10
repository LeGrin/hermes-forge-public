// Package envelopestore persists envelopes in SQLite.
//
// The package is deliberately small: Open, Insert, Get. There is no Delete
// method and no UPDATE path that can remove a row. This is the store-side
// enforcement of W-H17 (Hermes never forgets completed/active ownership).
//
// Nested envelope fields are persisted as JSON text columns. This trades
// query ergonomics for schema simplicity — v0 needs point lookups and
// status-indexed scans, nothing more.
package envelopestore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; no cgo

	"github.com/legrin-tech/hermes/envelope"
)

//go:embed migration/*.sql
var migrations embed.FS

// ErrNotFound is returned by Get when no envelope matches the given id.
var ErrNotFound = errors.New("envelopestore: not found")

// ErrDuplicate is returned by Insert when a row with the same id already
// exists. This is the canonical idempotency signal for callers: re-POSTing
// an envelope with the same id is a 409 at the HTTP boundary.
var ErrDuplicate = errors.New("envelopestore: duplicate id")

// Store persists envelopes in a SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) a SQLite-backed Store at dsn.
// For tests use a temp-file DSN: filepath.Join(t.TempDir(), "test.db").
func Open(ctx context.Context, dsn string) (*Store, error) {
	// Append busy_timeout pragma to avoid "database is locked" under
	// concurrent access from HTTP server + worker.
	if !strings.Contains(dsn, "?") {
		dsn += "?_busy_timeout=5000"
	} else if !strings.Contains(dsn, "_busy_timeout") {
		dsn += "&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("envelopestore: open: %w", err)
	}
	// SQLite does not support concurrent writers; limit to one connection
	// to avoid "database is locked" errors under concurrent requests.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("envelopestore: ping: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB for sharing with other stores that
// need the same single-connection pool (avoids "database is locked").
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	// Create the migrations tracking table if it doesn't exist.
	// Using IF NOT EXISTS makes this idempotent.
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("envelopestore: create migrations table: %w", err)
	}

	migrationFiles := []string{
		"migration/001_envelopes.sql",
		"migration/003_thread.sql",
	}
	for _, file := range migrationFiles {
		// Check if already applied.
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE name = ?`, file).Scan(&exists)
		if err == nil {
			// Migration already applied.
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("envelopestore: check migration %q: %w", file, err)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			// Unexpected error when checking migration status.
			return fmt.Errorf("envelopestore: check migration %q: %w", file, err)
		}

		ddl, err := migrations.ReadFile(file)
		if err != nil {
			return fmt.Errorf("envelopestore: migration file %q not found: %w", file, err)
		}
		if _, err := s.db.ExecContext(ctx, string(ddl)); err != nil {
			return fmt.Errorf("envelopestore: migrate %s: %w", file, err)
		}

		// Record the migration as applied.
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations (name) VALUES (?)`, file); err != nil {
			return fmt.Errorf("envelopestore: record migration %q: %w", file, err)
		}
	}
	return nil
}

// Insert persists e. Returns the envelope's own Validate error if the
// payload is malformed, or the underlying driver error on conflict.
// Re-inserting the same id is a conflict, not an update — the caller must
// treat duplicates as an idempotency signal, not a write path.
func (s *Store) Insert(ctx context.Context, e *envelope.Envelope) error {
	if err := e.Validate(); err != nil {
		return err
	}
	cols, err := marshalCols(e)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO envelopes (
  id, created_at, created_by, title, domain, project, target_node, target_executor,
  task_title, task_goal, task_steps, success_criteria, escalation_criteria,
  proof_required, status, delivery, capability_hints, session_binding,
  executor_session_id, thread, metrics, history, proof
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.CreatedAt, e.CreatedBy, e.Title, e.Domain, e.Project, e.TargetNode, e.TargetExecutor,
		e.TaskTitle, e.TaskGoal, cols.steps, cols.success, cols.escalation,
		cols.proofReq, string(e.Status), cols.delivery, cols.hints, nullable(e.SessionBinding),
		e.ExecutorSessionID, cols.thread, cols.metrics, cols.history, cols.proof,
	)
	if err != nil && isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrDuplicate, e.ID)
	}
	return err
}

// isUniqueViolation detects SQLite primary-key/unique constraint failures.
// String match is intentional: modernc.org/sqlite's typed error surface is
// driver-specific and we will swap drivers when moving to pgx in a later
// slice. Tighten to typed errors at that point.
func isUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "PRIMARY KEY")
}

// selectColumns is the canonical projection used by Get and NextCreated.
const selectColumns = `id, created_at, created_by, title, domain, project, target_node, target_executor,
       task_title, task_goal, task_steps, success_criteria, escalation_criteria,
       proof_required, status, delivery, capability_hints, session_binding,
       executor_session_id, thread, metrics, history, proof`

// scanRow fills an envelope from a *sql.Row matching selectColumns.
func scanRow(row *sql.Row) (*envelope.Envelope, error) {
	var (
		e       envelope.Envelope
		status  string
		session sql.NullString
		cols    jsonCols
	)
	err := row.Scan(
		&e.ID, &e.CreatedAt, &e.CreatedBy, &e.Title, &e.Domain, &e.Project, &e.TargetNode, &e.TargetExecutor,
		&e.TaskTitle, &e.TaskGoal, &cols.steps, &cols.success, &cols.escalation,
		&cols.proofReq, &status, &cols.delivery, &cols.hints, &session,
		&e.ExecutorSessionID, &cols.thread, &cols.metrics, &cols.history, &cols.proof,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("envelopestore: scan: %w", err)
	}
	e.Status = envelope.Status(status)
	if session.Valid {
		v := session.String
		e.SessionBinding = &v
	}
	if err := unmarshalInto(&e, cols); err != nil {
		return nil, err
	}
	return &e, nil
}

// scanFromRows scans a single row from *sql.Rows into an envelope.
func scanFromRows(rows *sql.Rows) (*envelope.Envelope, error) {
	var (
		e       envelope.Envelope
		status  string
		session sql.NullString
		cols    jsonCols
	)
	if err := rows.Scan(
		&e.ID, &e.CreatedAt, &e.CreatedBy, &e.Title, &e.Domain, &e.Project, &e.TargetNode, &e.TargetExecutor,
		&e.TaskTitle, &e.TaskGoal, &cols.steps, &cols.success, &cols.escalation,
		&cols.proofReq, &status, &cols.delivery, &cols.hints, &session,
		&e.ExecutorSessionID, &cols.thread, &cols.metrics, &cols.history, &cols.proof,
	); err != nil {
		return nil, fmt.Errorf("envelopestore: scan: %w", err)
	}
	e.Status = envelope.Status(status)
	if session.Valid {
		v := session.String
		e.SessionBinding = &v
	}
	if err := unmarshalInto(&e, cols); err != nil {
		return nil, err
	}
	return &e, nil
}

// Get returns the envelope with the given id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*envelope.Envelope, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM envelopes WHERE id = ?`, id)
	return scanRow(row)
}

// NextCreated returns the oldest envelope currently in status=created,
// or ErrNotFound if none. The worker uses this to pick its next target.
// v0 assumes a single worker, so this is a plain SELECT; multi-worker
// claim semantics (SKIP LOCKED / conditional UPDATE RETURNING) land when
// we move to pgx.
func (s *Store) NextCreated(ctx context.Context) (*envelope.Envelope, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+selectColumns+` FROM envelopes
		 WHERE status = 'created'
		 ORDER BY created_at ASC, id ASC
		 LIMIT 1`)
	return scanRow(row)
}

// List returns envelopes filtered by statuses, most recent first.
// If statuses is empty, all envelopes are returned. This is the read
// side for W-H6 (report who is doing what) — KITT uses
// GET /envelopes?status=blocked,paused to discover escalated work.
func (s *Store) List(ctx context.Context, statuses []envelope.Status) ([]envelope.Envelope, error) {
	var query string
	var args []any

	if len(statuses) == 0 {
		query = `SELECT ` + selectColumns + ` FROM envelopes ORDER BY created_at DESC, id DESC`
	} else {
		placeholders := make([]string, len(statuses))
		args = make([]any, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args[i] = string(st)
		}
		query = `SELECT ` + selectColumns + ` FROM envelopes WHERE status IN (` +
			strings.Join(placeholders, ",") + `) ORDER BY created_at DESC, id DESC`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("envelopestore: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []envelope.Envelope
	for rows.Next() {
		e, err := scanFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ActiveSession is a lightweight projection for registry enrichment.
type ActiveSession struct {
	EnvelopeID string
	Title      string
	Status     envelope.Status
	Executor   string
	StartedAt  *time.Time // from metrics.started_at, may be nil
	CreatedAt  time.Time
}

// ListActiveSessions returns lightweight active session entries for a project.
func (s *Store) ListActiveSessions(ctx context.Context, project string) ([]ActiveSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, status, target_executor, metrics, created_at
		FROM envelopes
		WHERE project = ? AND status IN ('delivered', 'in_progress')
		ORDER BY created_at DESC, id DESC
	`, project)
	if err != nil {
		return nil, fmt.Errorf("envelopestore: list active sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []ActiveSession
	for rows.Next() {
		var (
			id, title, status, executor string
			metricsJSON                 string
			createdAt                   time.Time
		)
		if err := rows.Scan(&id, &title, &status, &executor, &metricsJSON, &createdAt); err != nil {
			return nil, err
		}
		// Parse metrics JSON to get started_at.
		var metrics struct {
			StartedAt *time.Time `json:"started_at"`
		}
		if err := json.Unmarshal([]byte(metricsJSON), &metrics); err != nil {
			return nil, fmt.Errorf("envelopestore: unmarshal metrics: %w", err)
		}

		sessions = append(sessions, ActiveSession{
			EnvelopeID: id,
			Title:      title,
			Status:     envelope.Status(status),
			Executor:   executor,
			StartedAt:  metrics.StartedAt,
			CreatedAt:  createdAt,
		})
	}
	return sessions, rows.Err()
}

// ListActiveSessionsForProjects returns active sessions grouped by project for all given projects.
// This avoids N+1 queries when enriching multiple projects with active sessions.
func (s *Store) ListActiveSessionsForProjects(ctx context.Context, projects []string) (map[string][]ActiveSession, error) {
	if len(projects) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(projects))
	args := make([]any, len(projects))
	for i, p := range projects {
		placeholders[i] = "?"
		args[i] = p
	}

	query := `
		SELECT id, title, status, target_executor, metrics, created_at, project
		FROM envelopes
		WHERE project IN (` + strings.Join(placeholders, ",") + `) AND status IN ('delivered', 'in_progress')
		ORDER BY project, created_at DESC, id DESC
	`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("envelopestore: list active sessions for projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string][]ActiveSession)
	for _, p := range projects {
		result[p] = nil // Initialize to nil (not empty slice) so we can distinguish from no project
	}

	for rows.Next() {
		var (
			id, title, status, executor, project string
			metricsJSON                          string
			createdAt                            time.Time
		)
		if err := rows.Scan(&id, &title, &status, &executor, &metricsJSON, &createdAt, &project); err != nil {
			return nil, err
		}
		var metrics struct {
			StartedAt *time.Time `json:"started_at"`
		}
		if err := json.Unmarshal([]byte(metricsJSON), &metrics); err != nil {
			return nil, fmt.Errorf("envelopestore: unmarshal metrics: %w", err)
		}

		result[project] = append(result[project], ActiveSession{
			EnvelopeID: id,
			Title:      title,
			Status:     envelope.Status(status),
			Executor:   executor,
			StartedAt:  metrics.StartedAt,
			CreatedAt:  createdAt,
		})
	}
	return result, rows.Err()
}

// MarkDelivered atomically flips status from created → delivered and
// persists delivered_at + session_binding. Returns ErrNotFound if no row
// matched (either the id is unknown or another worker/retry already moved
// it). This is the transactional boundary that upholds W-H5 (no
// optimistic flip) and W-H14 (a crash before MarkDelivered leaves the row
// reclaimable because status stays 'created').
func (s *Store) MarkDelivered(ctx context.Context, id string, sessionBinding string, deliveredAt time.Time) error {
	deliveryJSON, err := json.Marshal(envelope.Delivery{
		Delivered:   true,
		DeliveredAt: &deliveredAt,
	})
	if err != nil {
		return fmt.Errorf("envelopestore: marshal delivery: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE envelopes
SET status = ?, delivery = ?, session_binding = ?
WHERE id = ? AND status = 'created'`,
		string(envelope.StatusDelivered), deliveryJSON, sessionBinding, id)
	if err != nil {
		return fmt.Errorf("envelopestore: mark delivered: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("envelopestore: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetExecutorSessionID updates the executor_session_id for the envelope with
// the given id. Returns ErrNotFound if no envelope matches. Returns an error
// if the envelope is in a terminal state (done, failed, lost).
//
// The terminal-state check is enforced atomically in the UPDATE to avoid a
// race between reading the envelope and updating it.
func (s *Store) SetExecutorSessionID(ctx context.Context, id string, sessionID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE envelopes SET executor_session_id = ? WHERE id = ? AND status NOT IN ('done', 'failed', 'lost')`,
		sessionID, id)
	if err != nil {
		return fmt.Errorf("envelopestore: set executor session id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("envelopestore: rows affected: %w", err)
	}
	if n == 0 {
		// Check if envelope exists at all to distinguish not_found from terminal.
		_, err := s.Get(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		if err == nil {
			// Envelope exists but was in terminal state.
			return fmt.Errorf("envelopestore: cannot set session on terminal envelope")
		}
		return err
	}
	return nil
}

// UpdateStatus loads the envelope inside a transaction, validates the
// transition via CanTransition, and atomically updates status + proof +
// metrics + delivery metadata + history. The transaction prevents race
// conditions where concurrent updates could violate W-H17 (terminal
// state escape) or lose proof entries.
//
// Side-effects applied automatically:
//   - A history entry "[timestamp] from → to" is appended (plus ": note"
//     when note is non-empty). Skipped when from == to.
//   - Terminal states set Metrics.CompletedAt if not already set.
//   - StatusRead sets Delivery.Read + Delivery.ReadAt if not already set.
func (s *Store) UpdateStatus(ctx context.Context, id string, next envelope.Status, proof map[string]string, note string) (*envelope.Envelope, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("envelopestore: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM envelopes WHERE id = ?`, id)
	e, err := scanRow(row)
	if err != nil {
		return nil, err
	}

	// Merge incoming proof into existing before validation.
	if len(proof) > 0 {
		if e.Proof == nil {
			e.Proof = make(map[string]string)
		}
		for k, v := range proof {
			e.Proof[k] = v
		}
	}

	if err := e.CanTransition(next); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	applyTransitionSideEffects(e, next, now, note)

	proofJSON, metricsJSON, deliveryJSON, historyJSON, err := marshalStatusFields(e)
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `
UPDATE envelopes SET status = ?, proof = ?, metrics = ?, delivery = ?, history = ? WHERE id = ?`,
		string(next), proofJSON, metricsJSON, deliveryJSON, historyJSON, id)
	if err != nil {
		return nil, fmt.Errorf("envelopestore: update status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("envelopestore: commit: %w", err)
	}
	return e, nil
}

// AddHistory appends an entry to the envelope's history without
// changing status. Used for decision logging and notes.
func (s *Store) AddHistory(ctx context.Context, id string, entry string) (*envelope.Envelope, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("envelopestore: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `SELECT `+selectColumns+` FROM envelopes WHERE id = ?`, id)
	e, err := scanRow(row)
	if err != nil {
		return nil, err
	}

	e.History = append(e.History, entry)
	historyJSON, err := json.Marshal(stringsOrEmpty(e.History))
	if err != nil {
		return nil, fmt.Errorf("envelopestore: marshal history: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE envelopes SET history = ? WHERE id = ?`, historyJSON, id); err != nil {
		return nil, fmt.Errorf("envelopestore: update history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("envelopestore: commit: %w", err)
	}
	return e, nil
}

// applyTransitionSideEffects mutates e in-place: sets status, updates
// metrics timestamps, appends history, and marks read delivery metadata.
func applyTransitionSideEffects(e *envelope.Envelope, next envelope.Status, now time.Time, note string) {
	from := e.Status
	e.Status = next
	e.Metrics.UpdatedAt = &now

	if from != next {
		entry := fmt.Sprintf("[%s] %s → %s", now.Format(time.RFC3339), from, next)
		if note != "" {
			entry += ": " + note
		}
		e.History = append(e.History, entry)
	}

	if next.IsTerminal() && e.Metrics.CompletedAt == nil {
		e.Metrics.CompletedAt = &now
	}
	if next == envelope.StatusRead && !e.Delivery.Read {
		e.Delivery.Read = true
		e.Delivery.ReadAt = &now
	}
}

// marshalStatusFields serializes the four JSON columns that UpdateStatus writes.
func marshalStatusFields(e *envelope.Envelope) (proof, metrics, delivery, history []byte, err error) {
	if proof, err = json.Marshal(mapOrEmpty(e.Proof)); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("envelopestore: marshal proof: %w", err)
	}
	if metrics, err = json.Marshal(e.Metrics); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("envelopestore: marshal metrics: %w", err)
	}
	if delivery, err = json.Marshal(e.Delivery); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("envelopestore: marshal delivery: %w", err)
	}
	if history, err = json.Marshal(stringsOrEmpty(e.History)); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("envelopestore: marshal history: %w", err)
	}
	return proof, metrics, delivery, history, nil
}

// --- internal helpers ---

// jsonCols holds the JSON-text representations of envelope's array/object
// fields. Used bidirectionally: marshalCols writes it out for Insert,
// and Get scans rows straight into it.
type jsonCols struct {
	steps, success, escalation, proofReq     []byte
	delivery, hints, metrics, history, proof []byte
	thread                                   []byte // v2-001: conversation thread
}

func marshalCols(e *envelope.Envelope) (jsonCols, error) {
	var c jsonCols
	type target struct {
		dst *[]byte
		src any
	}
	targets := []target{
		{&c.steps, stringsOrEmpty(e.TaskSteps)},
		{&c.success, stringsOrEmpty(e.SuccessCriteria)},
		{&c.escalation, stringsOrEmpty(e.EscalationCriteria)},
		{&c.proofReq, stringsOrEmpty(e.ProofRequired)},
		{&c.delivery, e.Delivery},
		{&c.hints, stringsOrEmpty(e.CapabilityHints)},
		{&c.metrics, e.Metrics},
		{&c.history, stringsOrEmpty(e.History)},
		{&c.proof, mapOrEmpty(e.Proof)},
		{&c.thread, messagesOrEmpty(e.Thread)}, // v2-001
	}
	for _, t := range targets {
		raw, err := json.Marshal(t.src)
		if err != nil {
			return c, fmt.Errorf("envelopestore: marshal: %w", err)
		}
		*t.dst = raw
	}
	return c, nil
}

func unmarshalInto(e *envelope.Envelope, c jsonCols) error {
	pairs := []struct {
		raw []byte
		dst any
	}{
		{c.steps, &e.TaskSteps},
		{c.success, &e.SuccessCriteria},
		{c.escalation, &e.EscalationCriteria},
		{c.proofReq, &e.ProofRequired},
		{c.delivery, &e.Delivery},
		{c.hints, &e.CapabilityHints},
		{c.metrics, &e.Metrics},
		{c.history, &e.History},
		{c.proof, &e.Proof},
		{c.thread, &e.Thread}, // v2-001
	}
	for _, p := range pairs {
		if len(p.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(p.raw, p.dst); err != nil {
			return fmt.Errorf("envelopestore: unmarshal: %w", err)
		}
	}
	return nil
}

func stringsOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// messagesOrEmpty returns e.Thread or an empty slice (never nil).
// Used for JSON marshalling so we always get '[]' not 'null'.
func messagesOrEmpty(m []envelope.Message) []envelope.Message {
	if m == nil {
		return []envelope.Message{}
	}
	return m
}

func mapOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nullable(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// AppendMessage appends a Message to the envelope thread (JSON blob in SQLite).
// Returns ErrNotFound if the envelope does not exist.
// Uses a transaction to prevent race conditions between read and write.
func (s *Store) AppendMessage(ctx context.Context, envelopeID string, msg envelope.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("envelopestore: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read current thread within transaction.
	var threadJSON string
	err = tx.QueryRowContext(ctx, `SELECT thread FROM envelopes WHERE id = ?`, envelopeID).Scan(&threadJSON)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("envelopestore: read thread: %w", err)
	}

	var messages []envelope.Message
	if err := json.Unmarshal([]byte(threadJSON), &messages); err != nil {
		return fmt.Errorf("envelopestore: unmarshal thread: %w", err)
	}
	messages = append(messages, msg)

	newJSON, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("envelopestore: marshal thread: %w", err)
	}

	_, err = tx.ExecContext(ctx, `UPDATE envelopes SET thread = ? WHERE id = ?`, string(newJSON), envelopeID)
	if err != nil {
		return fmt.Errorf("envelopestore: update thread: %w", err)
	}

	return tx.Commit()
}

// GetThread returns messages from the envelope thread.
// If sinceID is non-empty, returns only messages AFTER that ID (exclusive).
// Returns ErrNotFound if the envelope does not exist.
func (s *Store) GetThread(ctx context.Context, envelopeID string, sinceID string) ([]envelope.Message, error) {
	e, err := s.Get(ctx, envelopeID)
	if err != nil {
		return nil, err // ErrNotFound propagates from Get
	}

	if sinceID == "" {
		return e.Thread, nil
	}

	// Scan until sinceID found, return everything after.
	for i, msg := range e.Thread {
		if msg.ID == sinceID {
			return e.Thread[i+1:], nil
		}
	}
	// sinceID not found in thread — return empty (caller asked for messages after a non-existent ID).
	return []envelope.Message{}, nil
}
