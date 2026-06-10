// Package notifystore persists notifications for status changes that
// external consumers (KITT) should react to. Notifications are created
// automatically when an envelope reaches an "interesting" status and
// are acknowledged after the consumer processes them.
package notifystore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

//go:embed migration/*.sql
var migrations embed.FS

// InterestingStatuses are envelope statuses that trigger a notification.
var InterestingStatuses = map[string]bool{
	"done":             true,
	"blocked":          true,
	"failed":           true,
	"paused":           true,
	"awaiting_confirm": true,
}

// Notification is a queued status change for KITT to process.
type Notification struct {
	ID           int64  `json:"id"`
	EnvelopeID   string `json:"envelope_id"`
	Status       string `json:"status"`
	Note         string `json:"note,omitempty"`
	ProofSummary string `json:"proof_summary,omitempty"`
	CreatedAt    string `json:"created_at"`
	Acknowledged bool   `json:"acknowledged"`
	APIKey       string `json:"-"`
}

// Store is a thin SQLite-backed notification queue.
type Store struct {
	db *sql.DB
}

// ErrNotFound is returned by Acknowledge when the notification does not
// exist or has already been acknowledged.
var ErrNotFound = errors.New("notifystore: not found")

// OpenWithDB creates a Store sharing an existing *sql.DB connection.
func OpenWithDB(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	ddl, err := migrations.ReadFile("migration/001_notifications.sql")
	if err != nil {
		return fmt.Errorf("notifystore: read migration 001: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(ddl)); err != nil {
		return fmt.Errorf("notifystore: migrate 001: %w", err)
	}
	return s.migrateAPIKeyScope(ctx)
}

func (s *Store) migrateAPIKeyScope(ctx context.Context) error {
	// Use PRAGMA table_info to check column existence instead of relying on
	// brittle error-string matching from ALTER TABLE.
	hasCol, err := s.notificationColumnExists(ctx, "api_key")
	if err != nil {
		return fmt.Errorf("notifystore: check api_key column: %w", err)
	}
	if !hasCol {
		if err := s.addAPIKeyColumn(ctx); err != nil {
			return err
		}
	}
	// Recreate the index only when it is absent or does not include created_at.
	// Inspecting PRAGMA index_info avoids an unconditional DROP on every startup.
	needIndex, err := s.idxNotificationsAPIKeyUnackNeedsCreatedAt(ctx)
	if err != nil {
		return fmt.Errorf("notifystore: inspect api_key index: %w", err)
	}
	if needIndex {
		return s.rebuildAPIKeyIndex(ctx)
	}
	return nil
}

// addAPIKeyColumn adds the api_key column to a legacy notifications table and
// acknowledges all pre-existing rows in a single transaction. Pre-existing rows
// have api_key='' (the scoped public namespace used only in no-keystore/public
// mode) and are unreachable in keystore mode, so acknowledging them prevents
// phantom unacknowledged notifications.
func (s *Store) addAPIKeyColumn(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notifystore: begin legacy ack tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notifications ADD COLUMN api_key TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("notifystore: add api_key: %w", err)
	}
	// Acknowledge all pre-existing unacknowledged rows (they have api_key='').
	if _, err := tx.ExecContext(ctx, `UPDATE notifications SET acknowledged = 1 WHERE acknowledged = 0`); err != nil {
		return fmt.Errorf("notifystore: ack legacy rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notifystore: commit legacy ack tx: %w", err)
	}
	return nil
}

// rebuildAPIKeyIndex drops and recreates idx_notifications_api_key_unack to
// include the created_at column. The DROP + CREATE are wrapped in a transaction
// so a CREATE failure rolls back the DROP, preserving the original index.
func (s *Store) rebuildAPIKeyIndex(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notifystore: begin index migration tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err = tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_notifications_api_key_unack`); err != nil {
		return fmt.Errorf("notifystore: drop api_key index: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_notifications_api_key_unack
	ON notifications(api_key, created_at) WHERE acknowledged = 0`); err != nil {
		return fmt.Errorf("notifystore: index api_key: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("notifystore: commit index migration: %w", err)
	}
	return nil
}

// notificationColumnExists reports whether the named column exists in the
// notifications table by querying PRAGMA table_info, which is stable across
// SQLite versions. The table name is hardcoded to avoid SQL injection via
// dynamic PRAGMA concatenation.
func (s *Store) notificationColumnExists(ctx context.Context, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(notifications)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// idxNotificationsAPIKeyUnackNeedsCreatedAt returns true when idx_notifications_api_key_unack
// does not exist or its columns (per PRAGMA index_info) do not include created_at,
// meaning it predates the schema change and must be rebuilt.
// Real query errors are surfaced; only a missing index (no rows) returns (true, nil).
func (s *Store) idxNotificationsAPIKeyUnackNeedsCreatedAt(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`PRAGMA index_info(idx_notifications_api_key_unack)`,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	hasCreatedAt := false
	hasRows := false
	for rows.Next() {
		hasRows = true
		var seqno, cid int
		var name string
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return false, err
		}
		if name == "created_at" {
			hasCreatedAt = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	// No rows → index does not exist → needs creation.
	if !hasRows {
		return true, nil
	}
	// Index exists but lacks created_at → needs rebuild.
	return !hasCreatedAt, nil
}

// Insert adds a notification to the queue.
func (s *Store) Insert(ctx context.Context, n *Notification) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notifications (envelope_id, status, note, proof_summary, api_key) VALUES (?, ?, ?, ?, ?)`,
		n.EnvelopeID, n.Status, n.Note, n.ProofSummary, n.APIKey,
	)
	if err != nil {
		return fmt.Errorf("notifystore: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	n.ID = id
	return nil
}

const selectCols = `id, envelope_id, status, note, proof_summary, created_at, acknowledged, api_key`

// ListUnacknowledged returns all notifications not yet acknowledged.
func (s *Store) ListUnacknowledged(ctx context.Context) ([]*Notification, error) {
	query := `SELECT ` + selectCols + ` FROM notifications WHERE acknowledged = 0 ORDER BY created_at ASC`
	return s.listUnacknowledged(ctx, query)
}

// ListUnacknowledgedForKey returns unacknowledged notifications scoped to apiKey.
// An empty apiKey is valid and addresses only the scoped public namespace
// (no-keystore/public mode), not unscoped data access.
func (s *Store) ListUnacknowledgedForKey(ctx context.Context, apiKey string) ([]*Notification, error) {
	query := `SELECT ` + selectCols + ` FROM notifications WHERE acknowledged = 0 AND api_key = ? ORDER BY created_at ASC`
	return s.listUnacknowledged(ctx, query, apiKey)
}

func (s *Store) listUnacknowledged(ctx context.Context, query string, args ...any) ([]*Notification, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("notifystore: list: %w", err)
	}
	defer rows.Close()

	result := []*Notification{}
	for rows.Next() {
		var n Notification
		var ack int
		if err := rows.Scan(&n.ID, &n.EnvelopeID, &n.Status, &n.Note, &n.ProofSummary, &n.CreatedAt, &ack, &n.APIKey); err != nil {
			return nil, fmt.Errorf("notifystore: scan: %w", err)
		}
		n.Acknowledged = ack != 0
		result = append(result, &n)
	}
	return result, rows.Err()
}

// BulkAckResult reports the idempotent outcome of a bulk ack request.
type BulkAckResult struct {
	Requested           int `json:"requested"`
	Acknowledged        int `json:"acknowledged"`
	AlreadyAcknowledged int `json:"already_acknowledged"`
	Missing             int `json:"missing"`
}

const maxBulkAckIDsPerQuery = 500

// BulkAcknowledge marks owned notifications acknowledged. Unknown IDs and IDs
// outside apiKey scope are counted as missing so the endpoint does not leak them.
// An empty apiKey is valid and addresses only the scoped public namespace
// (no-keystore/public mode), not unscoped data access.
func (s *Store) BulkAcknowledge(ctx context.Context, ids []int64, apiKey string) (BulkAckResult, error) {
	result := BulkAckResult{Requested: len(ids)}
	if len(ids) == 0 {
		return result, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("notifystore: bulk ack begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	seenToAck := map[int64]struct{}{}
	for _, chunk := range chunkInt64s(ids, maxBulkAckIDsPerQuery) {
		if err := processAckChunk(ctx, tx, chunk, apiKey, seenToAck, &result); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("notifystore: bulk ack commit: %w", err)
	}
	return result, nil
}

// processAckChunk handles one chunk of IDs within the BulkAcknowledge transaction.
// It loads current ack states, classifies each ID, updates the DB, and reconciles
// the result counters against actual RowsAffected to handle concurrent acks.
func processAckChunk(ctx context.Context, tx *sql.Tx, chunk []int64, apiKey string, seenToAck map[int64]struct{}, result *BulkAckResult) error {
	states, err := loadAckStates(ctx, tx, chunk, apiKey)
	if err != nil {
		return err
	}
	toAck := classifyBulkAck(chunk, states, seenToAck, result)
	n, err := markBulkAcknowledged(ctx, tx, toAck, apiKey)
	if err != nil {
		return err
	}
	// Derive acknowledged count from UPDATE RowsAffected to avoid
	// over-reporting under concurrent acks (classifyBulkAck pre-counts
	// before the UPDATE guard runs). Any difference between toAck and
	// RowsAffected means a concurrent ack raced us — attribute to AlreadyAcknowledged.
	result.Acknowledged += int(n)
	result.AlreadyAcknowledged += len(toAck) - int(n)
	return nil
}

func loadAckStates(ctx context.Context, tx *sql.Tx, ids []int64, apiKey string) (map[int64]bool, error) {
	query := `SELECT id, acknowledged FROM notifications WHERE api_key = ? AND id IN (` + placeholders(len(ids)) + `)`
	args := append([]any{apiKey}, int64Args(ids)...)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("notifystore: bulk ack lookup: %w", err)
	}
	defer rows.Close()
	return scanAckStates(rows)
}

func scanAckStates(rows *sql.Rows) (map[int64]bool, error) {
	states := map[int64]bool{}
	for rows.Next() {
		var id int64
		var ack int
		if err := rows.Scan(&id, &ack); err != nil {
			return nil, fmt.Errorf("notifystore: bulk ack scan: %w", err)
		}
		states[id] = ack != 0
	}
	return states, rows.Err()
}

func classifyBulkAck(ids []int64, states map[int64]bool, seenToAck map[int64]struct{}, result *BulkAckResult) []int64 {
	toAck := map[int64]struct{}{}
	for _, id := range ids {
		ack, ok := states[id]
		if !ok {
			result.Missing++
		} else if ack || hasID(seenToAck, id) {
			result.AlreadyAcknowledged++
		} else {
			toAck[id] = struct{}{}
			seenToAck[id] = struct{}{}
			// Do NOT increment result.Acknowledged here; it is derived from
			// markBulkAcknowledged RowsAffected to avoid over-reporting under
			// concurrent acks.
		}
	}
	return mapKeys(toAck)
}

func chunkInt64s(ids []int64, size int) [][]int64 {
	chunks := make([][]int64, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

func markBulkAcknowledged(ctx context.Context, tx *sql.Tx, ids []int64, apiKey string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	query := `UPDATE notifications SET acknowledged = 1 WHERE acknowledged = 0 AND api_key = ? AND id IN (` + placeholders(len(ids)) + `)`
	args := append([]any{apiKey}, int64Args(ids)...)
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("notifystore: bulk ack update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("notifystore: bulk ack rows affected: %w", err)
	}
	return n, nil
}

func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func int64Args(ids []int64) []any {
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

func hasID(ids map[int64]struct{}, id int64) bool {
	_, ok := ids[id]
	return ok
}

func mapKeys(ids map[int64]struct{}) []int64 {
	keys := make([]int64, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// Acknowledge marks a notification as processed.
func (s *Store) Acknowledge(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET acknowledged = 1 WHERE id = ? AND acknowledged = 0`, id)
	if err != nil {
		return fmt.Errorf("notifystore: ack: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notifystore: ack rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: notification %d not found or already acknowledged", ErrNotFound, id)
	}
	return nil
}

// AcknowledgeForKey marks a notification as processed only within apiKey scope.
// An empty apiKey is valid and addresses only the scoped public namespace
// (no-keystore/public mode), not unscoped data access.
func (s *Store) AcknowledgeForKey(ctx context.Context, id int64, apiKey string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET acknowledged = 1 WHERE id = ? AND api_key = ? AND acknowledged = 0`, id, apiKey)
	if err != nil {
		return fmt.Errorf("notifystore: ack for key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("notifystore: ack for key rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: notification %d not found or already acknowledged", ErrNotFound, id)
	}
	return nil
}

// CountRecentByStatus counts notifications for an envelope with a given
// status created in the last `minutes` minutes. Used for loop detection.
func (s *Store) CountRecentByStatus(ctx context.Context, envelopeID, status string, minutes int) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications
		 WHERE envelope_id = ? AND status = ?
		 AND created_at > datetime('now', ?)`,
		envelopeID, status, fmt.Sprintf("-%d minutes", minutes),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("notifystore: count recent: %w", err)
	}
	return count, nil
}

// PurgeOld deletes acknowledged notifications older than the given number of days.
// Returns the count of deleted rows. Should be called periodically to prevent
// unbounded table growth.
func (s *Store) PurgeOld(ctx context.Context, olderThanDays int) (int64, error) {
	if olderThanDays < 0 {
		return 0, fmt.Errorf("notifystore: olderThanDays must be >= 0, got %d", olderThanDays)
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM notifications WHERE acknowledged = 1 AND created_at < datetime('now', ?)`,
		fmt.Sprintf("-%d days", olderThanDays),
	)
	if err != nil {
		return 0, fmt.Errorf("notifystore: purge: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ProofSummaryFromMap creates a one-line deterministic summary from a proof map.
func ProofSummaryFromMap(proof map[string]string) string {
	if len(proof) == 0 {
		return ""
	}
	keys := make([]string, 0, len(proof))
	for k := range proof {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(proof))
	for _, k := range keys {
		parts = append(parts, k+": "+proof[k])
	}
	summary := strings.Join(parts, "; ")
	if utf8.RuneCountInString(summary) > 1000 {
		runes := []rune(summary)
		return string(runes[:1000]) + "…"
	}
	return summary
}
