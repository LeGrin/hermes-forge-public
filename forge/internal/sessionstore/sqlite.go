// Package sessionstore persists Forge's session register in SQLite.
//
// W-F1: the register survives process restarts because it is backed by
// a file, not an in-memory map.
//
// There is no Delete method — sessions transition to 'closed' or 'lost'
// but are never removed. This mirrors the append-only design of
// hermes/internal/envelopestore (W-H17).
package sessionstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migration/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("sessionstore: not found")
var ErrDuplicate = errors.New("sessionstore: duplicate session_id")

// Session is a row in the session register.
type Session struct {
	SessionID  string    `json:"session_id"`
	EnvelopeID string    `json:"envelope_id"`
	Executor   string    `json:"executor"`
	WorkingDir string    `json:"working_dir"`
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// Store persists sessions in SQLite.
type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sessionstore: open: %w", err)
	}
	// SQLite does not support concurrent writers; limit to one connection
	// to avoid "database is locked" errors under concurrent requests.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sessionstore: ping: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	// Read all migration files and sort them by name to ensure consistent ordering.
	entries, err := migrations.ReadDir("migration")
	if err != nil {
		return fmt.Errorf("sessionstore: read migration dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	for _, name := range names {
		ddl, err := migrations.ReadFile("migration/" + name)
		if err != nil {
			return fmt.Errorf("sessionstore: read migration %s: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx, string(ddl)); err != nil {
			// Ignore "duplicate column name" errors — column already exists.
			// This makes migrations idempotent for ALTER TABLE ADD COLUMN.
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("sessionstore: migrate %s: %w", name, err)
			}
		}
	}
	return nil
}

// Insert creates a new session row. Returns ErrDuplicate if the
// session_id already exists.
func (s *Store) Insert(ctx context.Context, sess *Session) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions (session_id, envelope_id, executor, working_dir, state, started_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.SessionID, sess.EnvelopeID, sess.Executor, sess.WorkingDir, sess.State,
		sess.StartedAt, sess.LastSeenAt,
	)
	if err != nil && isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrDuplicate, sess.SessionID)
	}
	return err
}

// Get returns a single session by id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, sessionID string) (*Session, error) {
	return s.scanRow(s.db.QueryRowContext(ctx,
		`SELECT session_id, envelope_id, executor, working_dir, state, started_at, last_seen_at
		 FROM sessions WHERE session_id = ?`, sessionID))
}

// GetByEnvelope returns the session bound to an envelope, or ErrNotFound.
// Used by /deliver to reuse an existing session on retry.
func (s *Store) GetByEnvelope(ctx context.Context, envelopeID string) (*Session, error) {
	return s.scanRow(s.db.QueryRowContext(ctx,
		`SELECT session_id, envelope_id, executor, working_dir, state, started_at, last_seen_at
		 FROM sessions WHERE envelope_id = ?
		 ORDER BY started_at DESC LIMIT 1`, envelopeID))
}

// List returns all sessions, most recent first.
func (s *Store) List(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, envelope_id, executor, working_dir, state, started_at, last_seen_at
		 FROM sessions ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sessionstore: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.SessionID, &sess.EnvelopeID, &sess.Executor,
			&sess.WorkingDir, &sess.State, &sess.StartedAt, &sess.LastSeenAt); err != nil {
			return nil, fmt.Errorf("sessionstore: scan: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) scanRow(row *sql.Row) (*Session, error) {
	var sess Session
	err := row.Scan(&sess.SessionID, &sess.EnvelopeID, &sess.Executor,
		&sess.WorkingDir, &sess.State, &sess.StartedAt, &sess.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sessionstore: scan: %w", err)
	}
	return &sess, nil
}

func isUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "PRIMARY KEY")
}

// UpdateSessionID updates the session_id and last_seen_at for the session
// bound to the given envelope. Used by resumeHandler after respawn to persist
// the new session ID so future resume calls use the live session.
func (s *Store) UpdateSessionID(ctx context.Context, envelopeID, newSessionID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET session_id = ?, last_seen_at = ? WHERE envelope_id = ?`,
		newSessionID, time.Now(), envelopeID)
	if err != nil {
		return fmt.Errorf("sessionstore: update session id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sessionstore: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
