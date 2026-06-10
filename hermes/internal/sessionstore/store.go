// Package sessionstore persists Hermes session lanes in SQLite.
//
// A session lane is a lightweight conversation channel between KITT, OpenCode,
// and Claude. Sessions are created when OpenCode/Claude need to communicate
// back to KITT outside of the normal envelope flow. Messages are append-only.
package sessionstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
)

//go:embed migration/*.sql
var migrations embed.FS

// ErrNotFound is returned when a session or message is not found.
var ErrNotFound = errors.New("sessionstore: not found")

// Session represents a conversation lane. Created by OpenCode/Claude when
// they need to send messages back to KITT outside the normal envelope flow.
type Session struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Title     string `json:"title"`
	Project   string `json:"project"`
	APIKey    string `json:"-"`      // Never exposed in JSON responses.
	Status    string `json:"status"` // "active", "closed"
}

// Message represents a single message in a session conversation.
type Message struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	From        string `json:"from"` // "opencode", "claude"
	Kind        string `json:"kind"` // "decision", "steer", "reply"
	Text        string `json:"text"`
	ReplyTo     string `json:"reply_to,omitempty"`
	CreatedAt   string `json:"created_at"`
	InsertOrder int64  `json:"insert_order"` // monotonic; used for deterministic ordering
}

// Store persists sessions and messages in SQLite.
type Store struct {
	db *sql.DB
}

// OpenWithDB creates a Store sharing an existing *sql.DB connection.
func OpenWithDB(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	migrationFiles := []string{
		"migration/001_sessions.sql",
		"migration/002_session_messages_order.sql",
		"migration/003_session_messages_order_backfill.sql",
	}
	for _, file := range migrationFiles {
		ddl, err := migrations.ReadFile(file)
		if err != nil {
			return fmt.Errorf("sessionstore: read migration %s: %w", file, err)
		}
		if _, err := s.db.ExecContext(ctx, string(ddl)); err != nil {
			// Ignore "duplicate column" for ADD COLUMN idempotency.
			if !strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("sessionstore: migrate %s: %w", file, err)
			}
		}
	}
	return nil
}

// Insert creates a new session. Returns ErrNotFound if the session already exists.
func (s *Store) Insert(ctx context.Context, sess *Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, title, project, api_key, status) VALUES (?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.CreatedAt, sess.Title, sess.Project, sess.APIKey, sess.Status)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("sessionstore: duplicate id: %s", sess.ID)
		}
		return fmt.Errorf("sessionstore: insert: %w", err)
	}
	return nil
}

// Get returns a session by id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, title, project, api_key, status FROM sessions WHERE id = ?`, id)
	var sess Session
	if err := row.Scan(&sess.ID, &sess.CreatedAt, &sess.Title, &sess.Project, &sess.APIKey, &sess.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sessionstore: scan: %w", err)
	}
	return &sess, nil
}

// List returns sessions for an api_key, most recent first. If api_key is empty,
// returns all sessions. Sessions with the same created_at are ordered by rowid
// descending (later-inserted sessions appear first), ensuring deterministic ordering.
func (s *Store) List(ctx context.Context, apiKey string) ([]Session, error) {
	var rows *sql.Rows
	var err error
	if apiKey == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, created_at, title, project, api_key, status FROM sessions ORDER BY created_at DESC, rowid DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, created_at, title, project, api_key, status FROM sessions WHERE api_key = ? ORDER BY created_at DESC, rowid DESC`,
			apiKey)
	}
	if err != nil {
		return nil, fmt.Errorf("sessionstore: list: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.CreatedAt, &sess.Title, &sess.Project, &sess.APIKey, &sess.Status); err != nil {
			return nil, fmt.Errorf("sessionstore: scan: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// InsertMessage adds a message to a session.
// Returns ErrNotFound if the session does not exist.
// Uses a transaction with conflict retry to atomically allocate insert_order.
func (s *Store) InsertMessage(ctx context.Context, msg *Message) error {
	// First verify the session exists.
	_, err := s.Get(ctx, msg.SessionID)
	if err != nil {
		return err
	}

	return s.insertMessageWithRetry(ctx, msg)
}

func (s *Store) insertMessageWithRetry(ctx context.Context, msg *Message) error {
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sessionstore: begin tx: %w", err)
		}

		order, err := s.getNextInsertOrder(ctx, tx, msg.SessionID)
		if err != nil {
			tx.Rollback()
			return err
		}

		msg.InsertOrder = order
		err = s.insertMessageTx(ctx, tx, msg)
		if err != nil {
			tx.Rollback()
			if isUniqueViolation(err) {
				continue
			}
			return err
		}

		if err := tx.Commit(); err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return fmt.Errorf("sessionstore: commit: %w", err)
		}
		return nil
	}

	return fmt.Errorf("sessionstore: insert message: max retries exceeded")
}

func (s *Store) getNextInsertOrder(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var order int64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(insert_order), 0) + 1 FROM session_messages WHERE session_id = ?`,
		sessionID).Scan(&order)
	if err != nil {
		return 0, fmt.Errorf("sessionstore: get next insert_order: %w", err)
	}
	return order, nil
}

func (s *Store) insertMessageTx(ctx context.Context, tx *sql.Tx, msg *Message) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO session_messages (id, session_id, msg_from, kind, text, reply_to, created_at, insert_order) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.SessionID, msg.From, msg.Kind, msg.Text, nullable(msg.ReplyTo), msg.CreatedAt, msg.InsertOrder)
	if err != nil {
		return fmt.Errorf("sessionstore: insert message: %w", err)
	}
	return nil
}

// GetMessages returns messages for a session, oldest first (by insert_order).
// If sinceID is non-empty, returns only messages after that ID using
// deterministic insert_order semantics (not string comparison).
func (s *Store) GetMessages(ctx context.Context, sessionID, sinceID string) ([]Message, error) {
	// First verify session exists.
	_, err := s.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// If sinceID provided, look up its insert_order first for deterministic cutoff.
	if sinceID != "" {
		return s.getMessagesAfter(ctx, sessionID, sinceID)
	}

	// No sinceID: return all messages ordered by insert_order.
	return s.getAllMessages(ctx, sessionID)
}

func (s *Store) getMessagesAfter(ctx context.Context, sessionID, sinceID string) ([]Message, error) {
	var sinceOrder int64
	err := s.db.QueryRowContext(ctx,
		`SELECT insert_order FROM session_messages WHERE id = ? AND session_id = ?`,
		sinceID, sessionID).Scan(&sinceOrder)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []Message{}, nil
		}
		return nil, fmt.Errorf("sessionstore: lookup since_id: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, msg_from, kind, text, reply_to, created_at, insert_order
		 FROM session_messages WHERE session_id = ? AND insert_order > ? ORDER BY insert_order ASC`,
		sessionID, sinceOrder)
	if err != nil {
		return nil, fmt.Errorf("sessionstore: get messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

func (s *Store) getAllMessages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, msg_from, kind, text, reply_to, created_at, insert_order
		 FROM session_messages WHERE session_id = ? ORDER BY insert_order ASC`,
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("sessionstore: get messages: %w", err)
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var all []Message
	for rows.Next() {
		var msg Message
		var replyTo sql.NullString
		if err := rows.Scan(&msg.ID, &msg.SessionID, &msg.From, &msg.Kind, &msg.Text, &replyTo, &msg.CreatedAt, &msg.InsertOrder); err != nil {
			return nil, fmt.Errorf("sessionstore: scan message: %w", err)
		}
		if replyTo.Valid {
			msg.ReplyTo = replyTo.String
		}
		all = append(all, msg)
	}
	return all, rows.Err()
}

// isUniqueViolation detects SQLite primary-key/unique constraint failures.
func isUniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "PRIMARY KEY")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
