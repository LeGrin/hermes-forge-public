package notifystore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"database/sql"
	_ "modernc.org/sqlite"
)

const (
	fmtList        = "list: %v"
	fmtCreateTable = "create table: %v"

	// errDBClosed is the expected fatal message when an operation is attempted
	// on a closed DB. Extracted to avoid duplicate-literal lint warnings.
	errDBClosed             = "expected error when DB is closed, got nil"
	errDBClosedIndexInspect = "expected error when DB is closed during index inspection, got nil"
)

// requireCreateLegacyIdx fails the test with a consistent message when creating
// a legacy index (without created_at) fails. Centralises the repeated pattern.
func requireCreateLegacyIdx(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db := openTestDB(t)
	s, err := OpenWithDB(context.Background(), db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestInsertAndListUnacknowledged(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.Insert(ctx, &Notification{
		EnvelopeID: "env-1",
		Status:     "done",
		Note:       "all tests pass",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	list, err := s.ListUnacknowledged(ctx)
	if err != nil {
		t.Fatalf(fmtList, err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}
	if list[0].EnvelopeID != "env-1" {
		t.Errorf("envelope_id = %q", list[0].EnvelopeID)
	}
	if list[0].Status != "done" {
		t.Errorf("status = %q", list[0].Status)
	}
}

func TestAcknowledge(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	n := &Notification{EnvelopeID: "env-1", Status: "blocked"}
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.Acknowledge(ctx, n.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	list, err := s.ListUnacknowledged(ctx)
	if err != nil {
		t.Fatalf(fmtList, err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 unacked, got %d", len(list))
	}
}

func TestAcknowledgeIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	n := &Notification{EnvelopeID: "env-1", Status: "done"}
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Acknowledge(ctx, n.ID); err != nil {
		t.Fatalf("first ack: %v", err)
	}

	// Second ack should fail (already acknowledged).
	if err := s.Acknowledge(ctx, n.ID); err == nil {
		t.Fatal("expected error on double ack")
	}
}

func TestCountRecentByStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if err := s.Insert(ctx, &Notification{EnvelopeID: "env-loop", Status: "blocked"}); err != nil {
			t.Fatalf("insert blocked %d: %v", i, err)
		}
	}
	if err := s.Insert(ctx, &Notification{EnvelopeID: "env-loop", Status: "done"}); err != nil {
		t.Fatalf("insert done: %v", err)
	}
	if err := s.Insert(ctx, &Notification{EnvelopeID: "env-other", Status: "blocked"}); err != nil {
		t.Fatalf("insert other: %v", err)
	}

	count, err := s.CountRecentByStatus(ctx, "env-loop", "blocked", 30)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 4 {
		t.Errorf("expected 4, got %d", count)
	}
}

func TestListEmpty(t *testing.T) {
	s := openTestStore(t)
	list, err := s.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0, got %d", len(list))
	}
}

func TestProofSummaryFromMap(t *testing.T) {
	m := map[string]string{"commit": "abc123", "pr": "#42"}
	s := ProofSummaryFromMap(m)
	if s == "" {
		t.Fatal("expected non-empty summary")
	}
	if ProofSummaryFromMap(nil) != "" {
		t.Fatal("expected empty for nil map")
	}
}

func TestProofSummaryFromMap_Truncation(t *testing.T) {
	m := map[string]string{}
	// Create a map that produces a summary > 1000 chars.
	for i := 0; i < 50; i++ {
		m[fmt.Sprintf("key_%03d", i)] = "value_that_is_somewhat_long_to_make_total_exceed_limit"
	}
	s := ProofSummaryFromMap(m)
	if len(s) > 1010 {
		t.Fatalf("expected truncation to ~1000 chars, got %d", len(s))
	}
	if !strings.HasSuffix(s, "…") {
		t.Fatal("expected truncated string to end with ellipsis")
	}
}

func TestCountRecentByStatus_Zero(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	count, err := s.CountRecentByStatus(ctx, "nonexistent", "blocked", 30)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestPurgeOld(t *testing.T) {
	db := openTestDB(t)
	s, err := OpenWithDB(context.Background(), db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()

	n := &Notification{EnvelopeID: "env-old", Status: "done"}
	if err := s.Insert(ctx, n); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Acknowledge(ctx, n.ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// Backdate the notification to make it eligible for purge.
	if _, err := db.ExecContext(ctx,
		`UPDATE notifications SET created_at = datetime('now', '-10 days') WHERE id = ?`, n.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	deleted, err := s.PurgeOld(ctx, 7)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}
}

func TestPurgeOld_KeepsUnacked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Insert(ctx, &Notification{EnvelopeID: "env-unacked", Status: "done"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	deleted, err := s.PurgeOld(ctx, 0)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 0 {
		t.Errorf("expected 0 deleted (unacked), got %d", deleted)
	}
}

func TestPurgeOld_NegativeDays(t *testing.T) {
	s := openTestStore(t)
	_, err := s.PurgeOld(context.Background(), -1)
	if err == nil {
		t.Fatal("expected error for negative days")
	}
}

func TestAcknowledgeUnknown(t *testing.T) {
	s := openTestStore(t)
	err := s.Acknowledge(context.Background(), 9999)
	if err == nil {
		t.Fatal("expected error for unknown id")
	}
}

func TestBulkAcknowledgeForKey_ReportsOwnedAlreadyAckedAndMissing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	owned := &Notification{EnvelopeID: "env-owned", Status: "done", APIKey: "dev-key-owner"}
	acked := &Notification{EnvelopeID: "env-acked", Status: "done", APIKey: "dev-key-owner"}
	other := &Notification{EnvelopeID: "env-other", Status: "done", APIKey: "dev-key-other"}
	for _, n := range []*Notification{owned, acked, other} {
		if err := s.Insert(ctx, n); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := s.Acknowledge(ctx, acked.ID); err != nil {
		t.Fatalf("pre-ack: %v", err)
	}

	result, err := s.BulkAcknowledge(ctx, []int64{owned.ID, acked.ID, other.ID, 9999}, "dev-key-owner")
	if err != nil {
		t.Fatalf("bulk ack: %v", err)
	}
	if result.Requested != 4 || result.Acknowledged != 1 || result.AlreadyAcknowledged != 1 || result.Missing != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}

	list, err := s.ListUnacknowledgedForKey(ctx, "dev-key-owner")
	if err != nil {
		t.Fatalf("list owned: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no remaining owned notifications, got %d", len(list))
	}
}

func TestBulkAcknowledgeForKey_ChunksLargeInput(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ids := make([]int64, 0, maxBulkAckIDsPerQuery+5)
	for i := 0; i < maxBulkAckIDsPerQuery+5; i++ {
		n := &Notification{EnvelopeID: fmt.Sprintf("env-%d", i), Status: "done", APIKey: "dev-key-owner"}
		if err := s.Insert(ctx, n); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		ids = append(ids, n.ID)
	}
	ids = append(ids, ids[0], 999999)

	result, err := s.BulkAcknowledge(ctx, ids, "dev-key-owner")
	if err != nil {
		t.Fatalf("bulk ack: %v", err)
	}
	if result.Requested != len(ids) || result.Acknowledged != maxBulkAckIDsPerQuery+5 || result.AlreadyAcknowledged != 1 || result.Missing != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	list, err := s.ListUnacknowledgedForKey(ctx, "dev-key-owner")
	if err != nil {
		t.Fatalf(fmtList, err)
	}
	if len(list) != 0 {
		t.Fatalf("expected all notifications acked, got %d", len(list))
	}
}

// TestKeyScopedAPIsAllowEmptyKeyAsPublicNamespace verifies that an empty apiKey
// is accepted as the scoped public namespace: it filters by api_key = '' rather
// than returning ErrAPIKeyRequired or granting unscoped data access. This supports
// no-keystore/public mode where notifications are stored with an empty key.
func TestKeyScopedAPIsAllowEmptyKeyAsPublicNamespace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Insert a notification in the public namespace (empty key).
	pub := &Notification{EnvelopeID: "env-pub", Status: "done", APIKey: ""}
	if err := s.Insert(ctx, pub); err != nil {
		t.Fatalf("insert public notification: %v", err)
	}
	// Insert a notification in a private namespace.
	priv := &Notification{EnvelopeID: "env-priv", Status: "done", APIKey: "dev-key-private"}
	if err := s.Insert(ctx, priv); err != nil {
		t.Fatalf("insert private notification: %v", err)
	}

	// ListUnacknowledgedForKey("") should return only the public-namespace row.
	list, err := s.ListUnacknowledgedForKey(ctx, "")
	if err != nil {
		t.Fatalf("ListUnacknowledgedForKey empty key: %v", err)
	}
	if len(list) != 1 || list[0].EnvelopeID != "env-pub" {
		t.Fatalf("expected only public namespace notification, got %v", list)
	}

	// AcknowledgeForKey("") should ack the public-namespace row.
	if err := s.AcknowledgeForKey(ctx, pub.ID, ""); err != nil {
		t.Fatalf("AcknowledgeForKey empty key: %v", err)
	}

	// BulkAcknowledge("") on the private row should count it as missing.
	result, err := s.BulkAcknowledge(ctx, []int64{priv.ID}, "")
	if err != nil {
		t.Fatalf("BulkAcknowledge empty key: %v", err)
	}
	if result.Missing != 1 {
		t.Fatalf("expected private row to be missing from public namespace, got %+v", result)
	}
}

func TestAcknowledgeForKey_IsScoped(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owned := &Notification{EnvelopeID: "env-owned", Status: "done", APIKey: "dev-key-owner"}
	foreign := &Notification{EnvelopeID: "env-foreign", Status: "done", APIKey: "dev-key-other"}
	for _, n := range []*Notification{owned, foreign} {
		if err := s.Insert(ctx, n); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := s.AcknowledgeForKey(ctx, foreign.ID, "dev-key-owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected scoped miss, got %v", err)
	}
	if err := s.AcknowledgeForKey(ctx, owned.ID, "dev-key-owner"); err != nil {
		t.Fatalf("ack owned: %v", err)
	}
	remaining, err := s.ListUnacknowledgedForKey(ctx, "dev-key-owner")
	if err != nil {
		t.Fatalf("list owned: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected no owner notifications, got %d", len(remaining))
	}
}

func TestMigrate_CreatesAPIKeyIndexWhenColumnExists(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL,
		status TEXT NOT NULL,
		note TEXT NOT NULL DEFAULT '',
		proof_summary TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		acknowledged INTEGER NOT NULL DEFAULT 0,
		api_key TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := OpenWithDB(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var name string
	err = db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_notifications_api_key_unack'`).Scan(&name)
	if err != nil || name != "idx_notifications_api_key_unack" {
		t.Fatalf("expected api key index, got %q: %v", name, err)
	}
}

func TestMigrate_APIKeyIndexIncludesCreatedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := OpenWithDB(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var sql string
	err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_notifications_api_key_unack'`,
	).Scan(&sql)
	if err != nil {
		t.Fatalf("query index sql: %v", err)
	}
	if !strings.Contains(sql, "created_at") {
		t.Fatalf("expected index to include created_at, got: %s", sql)
	}
}

// TestBulkAcknowledge_ConcurrentAckAttributedToAlreadyAcknowledged verifies that
// when markBulkAcknowledged returns fewer RowsAffected than toAck (simulating a
// concurrent ack), the difference is attributed to AlreadyAcknowledged rather
// than silently lost.
//
// We simulate this by pre-acknowledging one notification between classifyBulkAck
// and markBulkAcknowledged — which is not directly injectable in a unit test, so
// we instead verify the accounting via two sequential BulkAcknowledge calls:
// the second call on the same IDs should report AlreadyAcknowledged, not Acknowledged.
func TestBulkAcknowledge_ConcurrentAckAttributedToAlreadyAcknowledged(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const key = "dev-key-concurrent-test"

	n1 := &Notification{EnvelopeID: "env-c1", Status: "done", APIKey: key}
	n2 := &Notification{EnvelopeID: "env-c2", Status: "blocked", APIKey: key}
	for _, n := range []*Notification{n1, n2} {
		if err := s.Insert(ctx, n); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	ids := []int64{n1.ID, n2.ID}

	// First bulk ack — both should be acknowledged.
	r1, err := s.BulkAcknowledge(ctx, ids, key)
	if err != nil {
		t.Fatalf("first bulk ack: %v", err)
	}
	if r1.Acknowledged != 2 || r1.AlreadyAcknowledged != 0 {
		t.Fatalf("first ack: want acknowledged=2 already=0, got %+v", r1)
	}

	// Second bulk ack on same IDs — both already acknowledged.
	r2, err := s.BulkAcknowledge(ctx, ids, key)
	if err != nil {
		t.Fatalf("second bulk ack: %v", err)
	}
	if r2.Acknowledged != 0 || r2.AlreadyAcknowledged != 2 {
		t.Fatalf("second ack: want acknowledged=0 already=2, got %+v", r2)
	}
	if r2.Requested != 2 {
		t.Fatalf("second ack: want requested=2, got %d", r2.Requested)
	}
}

// TestMigrateAPIKeyScope_IdempotentOnFreshDB verifies that running migrate
// twice on a fresh database does not error (index already present with
// created_at — no DROP/CREATE should be attempted on the second call).
func TestMigrateAPIKeyScope_IdempotentOnFreshDB(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// First open — creates table + index.
	if _, err := OpenWithDB(ctx, db); err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Second open — must be a no-op (no DROP, no error).
	if _, err := OpenWithDB(ctx, db); err != nil {
		t.Fatalf("second open (idempotent): %v", err)
	}
}

// TestMigrateAPIKeyScope_RebuildsLegacyIndexWithoutCreatedAt verifies that
// when the index exists but lacks created_at (legacy schema), migration drops
// and recreates it with created_at included.
func TestMigrateAPIKeyScope_RebuildsLegacyIndexWithoutCreatedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Manually create the table and a legacy index without created_at.
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL,
		status TEXT NOT NULL,
		note TEXT NOT NULL DEFAULT '',
		proof_summary TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		acknowledged INTEGER NOT NULL DEFAULT 0,
		api_key TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatalf(fmtCreateTable, err)
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX idx_notifications_api_key_unack ON notifications(api_key) WHERE acknowledged = 0`)
	requireCreateLegacyIdx(t, err)

	// Migration should detect missing created_at and rebuild the index.
	if _, err := OpenWithDB(ctx, db); err != nil {
		t.Fatalf("migrate with legacy index: %v", err)
	}

	var sql string
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_notifications_api_key_unack'`,
	).Scan(&sql); err != nil {
		t.Fatalf("query rebuilt index: %v", err)
	}
	if !strings.Contains(sql, "created_at") {
		t.Fatalf("expected rebuilt index to include created_at, got: %s", sql)
	}
}

// TestMapKeys_DeterministicOrder verifies that mapKeys returns IDs in
// ascending order regardless of map iteration order.
func TestMapKeys_DeterministicOrder(t *testing.T) {
	ids := map[int64]struct{}{5: {}, 1: {}, 3: {}, 2: {}, 4: {}}
	got := mapKeys(ids)
	want := []int64{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("len: want %d, got %d", len(want), len(got))
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("index %d: want %d, got %d (full: %v)", i, v, got[i], got)
		}
	}
}

// TestMapKeys_Empty verifies mapKeys handles an empty map without panic.
func TestMapKeys_Empty(t *testing.T) {
	got := mapKeys(map[int64]struct{}{})
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

// TestIdxNotificationsAPIKeyUnackNeedsCreatedAt_SurfacesRealErrors verifies that
// idxNotificationsAPIKeyUnackNeedsCreatedAt
// returns an error (not (true, nil)) when the DB query fails for a reason other
// than sql.ErrNoRows (e.g. the DB is closed).
func TestIdxNotificationsAPIKeyUnackNeedsCreatedAt_SurfacesRealErrors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &Store{db: db}
	// Close the DB to force a real query error.
	_ = db.Close()
	_, err := s.idxNotificationsAPIKeyUnackNeedsCreatedAt(ctx)
	if err == nil {
		t.Fatal(errDBClosed)
	}
}

// TestNotificationColumnExists_ReturnsCorrectly verifies the renamed helper
// correctly detects presence/absence of a column in the notifications table.
func TestNotificationColumnExists_ReturnsCorrectly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Create a minimal notifications table.
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf(fmtCreateTable, err)
	}
	s := &Store{db: db}

	// "envelope_id" exists.
	ok, err := s.notificationColumnExists(ctx, "envelope_id")
	if err != nil {
		t.Fatalf("notificationColumnExists envelope_id: %v", err)
	}
	if !ok {
		t.Fatal("expected envelope_id to exist")
	}

	// "api_key" does not exist yet.
	ok, err = s.notificationColumnExists(ctx, "api_key")
	if err != nil {
		t.Fatalf("notificationColumnExists api_key: %v", err)
	}
	if ok {
		t.Fatal("expected api_key to be absent")
	}
}

// TestIdxNotificationsAPIKeyUnackNeedsCreatedAt_UsesIndexInfo verifies that
// idxNotificationsAPIKeyUnackNeedsCreatedAt
// correctly detects a legacy index that lacks created_at by inspecting
// PRAGMA index_info rather than the stored SQL string.
func TestIdxNotificationsAPIKeyUnackNeedsCreatedAt_UsesIndexInfo(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		acknowledged INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf(fmtCreateTable, err)
	}
	// Create a legacy index without created_at.
	_, err = db.ExecContext(ctx, `CREATE INDEX idx_notifications_api_key_unack ON notifications(api_key) WHERE acknowledged = 0`)
	requireCreateLegacyIdx(t, err)
	s := &Store{db: db}

	// Should detect that created_at is missing from the index.
	needs, err := s.idxNotificationsAPIKeyUnackNeedsCreatedAt(ctx)
	if err != nil {
		t.Fatalf("idxNotificationsAPIKeyUnackNeedsCreatedAt: %v", err)
	}
	if !needs {
		t.Fatal("expected legacy index (no created_at) to need rebuild")
	}

	// Now create the correct index with created_at.
	_, err = db.ExecContext(ctx, `DROP INDEX idx_notifications_api_key_unack`)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX idx_notifications_api_key_unack ON notifications(api_key, created_at) WHERE acknowledged = 0`)
	if err != nil {
		t.Fatalf("create new index: %v", err)
	}

	needs, err = s.idxNotificationsAPIKeyUnackNeedsCreatedAt(ctx)
	if err != nil {
		t.Fatalf("idxNotificationsAPIKeyUnackNeedsCreatedAt after rebuild: %v", err)
	}
	if needs {
		t.Fatal("expected updated index (with created_at) to NOT need rebuild")
	}
}

// TestMigrateAPIKeyScope_TransactionRollbackOnCreateFailure verifies that the
// DROP + CREATE INDEX migration is wrapped in a transaction: if the CREATE
// fails, the DROP is rolled back and the original index is preserved.
// We simulate failure by using a table that lacks the created_at column,
// which causes the new index definition to fail.
func TestMigrateAPIKeyScope_TransactionRollbackOnCreateFailure(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create table without created_at to force CREATE INDEX failure.
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		acknowledged INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf(fmtCreateTable, err)
	}
	// Create a legacy index (no created_at in table or index).
	_, err = db.ExecContext(ctx, `CREATE INDEX idx_notifications_api_key_unack ON notifications(api_key) WHERE acknowledged = 0`)
	requireCreateLegacyIdx(t, err)

	s := &Store{db: db}
	// migrateAPIKeyScope should fail because created_at column doesn't exist.
	err = s.migrateAPIKeyScope(ctx)
	if err == nil {
		t.Fatal("expected error when created_at column missing from table")
	}

	// The original index must still exist (transaction rolled back DROP).
	var name string
	qErr := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_notifications_api_key_unack'`,
	).Scan(&name)
	if qErr != nil || name != "idx_notifications_api_key_unack" {
		t.Fatalf("expected original index to survive rollback, got name=%q err=%v", name, qErr)
	}
}

// TestNotificationColumnExists_SurfacesQueryError verifies that
// notificationColumnExists returns an error when the DB is closed (scan error path).
func TestNotificationColumnExists_SurfacesQueryError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &Store{db: db}
	_ = db.Close()
	_, err := s.notificationColumnExists(ctx, "api_key")
	if err == nil {
		t.Fatal(errDBClosed)
	}
}

// TestMigrateAPIKeyScope_AddColumnError verifies that migrateAPIKeyScope
// surfaces an error when the api_key column is absent but ALTER TABLE fails
// (e.g. because the table does not exist).
func TestMigrateAPIKeyScope_AddColumnError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// No notifications table at all — PRAGMA table_info returns no rows (column
	// absent), then ALTER TABLE fails because the table doesn't exist.
	s := &Store{db: db}
	err := s.migrateAPIKeyScope(ctx)
	if err == nil {
		t.Fatal("expected error when notifications table is absent, got nil")
	}
}

// TestMigrateAPIKeyScope_AlreadyUpToDate verifies that migrateAPIKeyScope is a
// no-op (returns nil) when the table already has api_key and the index already
// includes created_at.
func TestMigrateAPIKeyScope_AlreadyUpToDate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		acknowledged INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf(fmtCreateTable, err)
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX idx_notifications_api_key_unack
		ON notifications(api_key, created_at) WHERE acknowledged = 0`)
	if err != nil {
		t.Fatalf("create up-to-date index: %v", err)
	}
	s := &Store{db: db}
	if err := s.migrateAPIKeyScope(ctx); err != nil {
		t.Fatalf("expected no-op migration to succeed, got: %v", err)
	}
}

// TestMigrateAPIKeyScope_InspectIndexError verifies that migrateAPIKeyScope
// surfaces an error from idxNotificationsAPIKeyUnackNeedsCreatedAt when the DB is closed after the
// api_key column check passes (column present) but before the index check.
func TestMigrateAPIKeyScope_InspectIndexError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Create table with api_key so the column check passes.
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		acknowledged INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf(fmtCreateTable, err)
	}
	s := &Store{db: db}
	// Close DB so the index inspection query fails.
	_ = db.Close()
	err = s.migrateAPIKeyScope(ctx)
	if err == nil {
		t.Fatal(errDBClosedIndexInspect)
	}
}

// TestMigrateAPIKeyScope_LegacyRowsAcknowledgedOnColumnAdd verifies that when
// the api_key column is absent (legacy schema), any pre-existing unacknowledged
// rows are acknowledged during migration so they do not accumulate as phantom
// notifications unreachable in keystore mode.
func TestMigrateAPIKeyScope_LegacyRowsAcknowledgedOnColumnAdd(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create a legacy table without api_key column.
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '',
		proof_summary TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		acknowledged INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	// Insert two unacknowledged legacy rows.
	for i := 0; i < 2; i++ {
		_, err = db.ExecContext(ctx, `INSERT INTO notifications (envelope_id, status) VALUES ('env-legacy', 'done')`)
		if err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}
	}

	s := &Store{db: db}
	if err := s.migrateAPIKeyScope(ctx); err != nil {
		t.Fatalf("migrateAPIKeyScope: %v", err)
	}

	// All pre-existing rows must be acknowledged after migration.
	var unackCount int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE acknowledged = 0`).Scan(&unackCount)
	if err != nil {
		t.Fatalf("count unacknowledged: %v", err)
	}
	if unackCount != 0 {
		t.Errorf("expected 0 unacknowledged legacy rows after migration, got %d", unackCount)
	}

	// api_key column must now exist.
	var colName string
	err = db.QueryRowContext(ctx, `SELECT name FROM pragma_table_info('notifications') WHERE name = 'api_key'`).Scan(&colName)
	if err != nil || colName != "api_key" {
		t.Errorf("expected api_key column to exist after migration")
	}
}

// TestMigrateAPIKeyScope_NewRowsNotAcknowledgedAfterColumnExists verifies that
// rows inserted AFTER migration (when api_key column already exists) are NOT
// auto-acknowledged — only legacy rows from before the column existed are.
func TestMigrateAPIKeyScope_NewRowsNotAcknowledgedAfterColumnExists(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Insert a row after migration (store already migrated on open).
	if err := s.Insert(ctx, &Notification{EnvelopeID: "env-new", Status: "done", APIKey: ""}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Row must remain unacknowledged.
	rows, err := s.ListUnacknowledgedForKey(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 unacknowledged row, got %d", len(rows))
	}
}

// TestAddAPIKeyColumn_ErrorWhenDBClosed verifies that addAPIKeyColumn surfaces
// an error when the DB is closed (BeginTx fails).
func TestAddAPIKeyColumn_ErrorWhenDBClosed(t *testing.T) {
	db := openTestDB(t)
	s := &Store{db: db}
	_ = db.Close()
	err := s.addAPIKeyColumn(context.Background())
	if err == nil {
		t.Fatal(errDBClosed)
	}
}

// TestRebuildAPIKeyIndex_ErrorWhenDBClosed verifies that rebuildAPIKeyIndex
// surfaces an error when the DB is closed (BeginTx fails).
func TestRebuildAPIKeyIndex_ErrorWhenDBClosed(t *testing.T) {
	db := openTestDB(t)
	s := &Store{db: db}
	_ = db.Close()
	err := s.rebuildAPIKeyIndex(context.Background())
	if err == nil {
		t.Fatal(errDBClosed)
	}
}

// TestRebuildAPIKeyIndex_Success verifies that rebuildAPIKeyIndex creates the
// correct index (including created_at) on a table that already has the column.
func TestRebuildAPIKeyIndex_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `CREATE TABLE notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		envelope_id TEXT NOT NULL,
		api_key TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		acknowledged INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatalf(fmtCreateTable, err)
	}
	s := &Store{db: db}
	if err := s.rebuildAPIKeyIndex(ctx); err != nil {
		t.Fatalf("rebuildAPIKeyIndex: %v", err)
	}
	var sql string
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_notifications_api_key_unack'`,
	).Scan(&sql); err != nil {
		t.Fatalf("query index: %v", err)
	}
	if !strings.Contains(sql, "created_at") {
		t.Fatalf("expected index to include created_at, got: %s", sql)
	}
}
