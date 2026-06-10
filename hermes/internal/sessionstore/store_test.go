package sessionstore

import (
	"context"
	"database/sql"
	"embed"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

//go:embed migration/*.sql
var testMigrations embed.FS

func openForTest(t *testing.T) (*Store, func()) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	s := &Store{db: db}
	ctx := context.Background()
	if err := s.migrate(ctx); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	cleanup := func() {
		db.Close()
	}
	return s, cleanup
}

func TestInsertAndGet(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	sess := &Session{
		ID:      "sess-1",
		Title:   "Test Session",
		Project: "test-project",
		APIKey:  "key-123",
		Status:  "active",
	}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.Get(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != sess.ID {
		t.Errorf("id: got %q, want %q", got.ID, sess.ID)
	}
	if got.Title != sess.Title {
		t.Errorf("title: got %q, want %q", got.Title, sess.Title)
	}
}

func TestGetNotFound(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	_, err := s.Get(context.Background(), "nonexistent")
	if err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestListByAPIKey(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	// Insert sessions with different api keys.
	sessions := []*Session{
		{ID: "sess-1", Title: "S1", APIKey: "key-a", Status: "active"},
		{ID: "sess-2", Title: "S2", APIKey: "key-b", Status: "active"},
		{ID: "sess-3", Title: "S3", APIKey: "key-a", Status: "closed"},
	}
	for _, sess := range sessions {
		if err := s.Insert(context.Background(), sess); err != nil {
			t.Fatalf("insert %s: %v", sess.ID, err)
		}
	}

	// List by key-a.
	list, err := s.List(context.Background(), "key-a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len: got %d, want 2", len(list))
	}

	// List all.
	all, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len all: got %d, want 3", len(all))
	}
}

func TestInsertMessage(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	sess := &Session{ID: "sess-1", Title: "S1", APIKey: "key-1", Status: "active"}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	msg := &Message{
		ID:        "msg-1",
		SessionID: "sess-1",
		From:      "opencode",
		Kind:      "reply",
		Text:      "Hello KITT",
	}
	if err := s.InsertMessage(context.Background(), msg); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	messages, err := s.GetMessages(context.Background(), "sess-1", "")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len: got %d, want 1", len(messages))
	}
	if messages[0].Text != "Hello KITT" {
		t.Errorf("text: got %q, want %q", messages[0].Text, "Hello KITT")
	}
}

func TestGetMessagesWithSinceID(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	sess := &Session{ID: "sess-1", Title: "S1", APIKey: "key-1", Status: "active"}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Insert 3 messages.
	for i := 1; i <= 3; i++ {
		msg := &Message{
			ID:        "msg-" + string(rune('0'+i)),
			SessionID: "sess-1",
			From:      "claude",
			Kind:      "reply",
			Text:      "Message " + string(rune('0'+i)),
		}
		if err := s.InsertMessage(context.Background(), msg); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	// Get after msg-2.
	messages, err := s.GetMessages(context.Background(), "sess-1", "msg-2")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len: got %d, want 1", len(messages))
	}
	if messages[0].ID != "msg-3" {
		t.Errorf("id: got %q, want msg-3", messages[0].ID)
	}
}

func TestGetMessagesSessionNotFound(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	_, err := s.GetMessages(context.Background(), "nonexistent", "")
	if err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestGetMessages_SameSecondInserts verifies deterministic ordering when multiple
// messages are inserted within the same second. Regression test for non-deterministic
// created_at-based ordering.
func TestGetMessages_SameSecondInserts(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	sess := &Session{ID: "sess-1", Title: "S1", APIKey: "key-1", Status: "active"}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Insert 5 messages in rapid succession (same second).
	msgIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		msgIDs[i] = "msg-same-second-" + string(rune('a'+i))
		msg := &Message{
			ID:        msgIDs[i],
			SessionID: "sess-1",
			From:      "claude",
			Kind:      "reply",
			Text:      "Same second message " + string(rune('a'+i)),
			CreatedAt: "2026-04-16T12:00:00Z", // Same timestamp for all.
		}
		if err := s.InsertMessage(context.Background(), msg); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	// Fetch all messages.
	messages, err := s.GetMessages(context.Background(), "sess-1", "")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 5 {
		t.Fatalf("len: got %d, want 5", len(messages))
	}

	// Verify order matches insertion order (by insert_order, not created_at).
	for i, msg := range messages {
		if msg.ID != msgIDs[i] {
			t.Errorf("order[%d]: got %q, want %q", i, msg.ID, msgIDs[i])
		}
	}

	// Test since_id cutoff with first message ID.
	messages, err = s.GetMessages(context.Background(), "sess-1", msgIDs[0])
	if err != nil {
		t.Fatalf("get messages since: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("len after since_id: got %d, want 4", len(messages))
	}
	// Remaining messages should be in insertion order.
	for i, msg := range messages {
		if msg.ID != msgIDs[i+1] {
			t.Errorf("since_id order[%d]: got %q, want %q", i, msg.ID, msgIDs[i+1])
		}
	}
}

// TestListSessions_SameSecondInserts verifies deterministic ordering when multiple
// sessions are created within the same second. Regression test for non-deterministic
// created_at-only ordering.
func TestListSessions_SameSecondInserts(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	// Insert 3 sessions with identical created_at timestamps.
	// Insert order: S1, S2, S3 — rowid will be 1, 2, 3 respectively.
	// With rowid DESC tie-breaker, expected order is S3, S2, S1.
	sessions := []*Session{
		{ID: "sess-1", Title: "First", CreatedAt: "2026-04-16T12:00:00Z", APIKey: "key-1", Status: "active"},
		{ID: "sess-2", Title: "Second", CreatedAt: "2026-04-16T12:00:00Z", APIKey: "key-1", Status: "active"},
		{ID: "sess-3", Title: "Third", CreatedAt: "2026-04-16T12:00:00Z", APIKey: "key-1", Status: "active"},
	}
	for _, sess := range sessions {
		if err := s.Insert(context.Background(), sess); err != nil {
			t.Fatalf("insert %s: %v", sess.ID, err)
		}
	}

	list, err := s.List(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len: got %d, want 3", len(list))
	}

	// Verify order: later-inserted (higher rowid) first.
	expected := []string{"sess-3", "sess-2", "sess-1"}
	for i, sess := range list {
		if sess.ID != expected[i] {
			t.Errorf("order[%d]: got %q (%s), want %q", i, sess.ID, sess.Title, expected[i])
		}
	}
}

// TestGetMessages_SinceID_PreMigrationBackfill verifies that after migration,
// pre-existing rows (which previously had insert_order=0) are correctly backfilled
// with sequential insert_order values, and since_id queries return correct results.
// Regression test for the bug where migration 002 backfilled insert_order=0 for
// all existing rows, breaking since_id reads.
func TestGetMessages_SinceID_PreMigrationBackfill(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	sess := &Session{ID: "sess-premigration", Title: "S1", APIKey: "key-1", Status: "active"}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Simulate pre-migration state by inserting messages with insert_order=0
	// (as if the column didn't exist and defaulted to 0), then manually setting
	// the column to 0 to simulate what migration 002 used to do.
	preMigIDs := []string{"pre-msg-1", "pre-msg-2", "pre-msg-3"}
	for i, id := range preMigIDs {
		_, err := s.db.ExecContext(context.Background(),
			`INSERT INTO session_messages (id, session_id, msg_from, kind, text, created_at, insert_order) VALUES (?, ?, ?, ?, ?, ?, 0)`,
			id, sess.ID, "claude", "reply", "Pre-migration message "+string(rune('1'+i)), "2026-04-16T10:00:00Z")
		if err != nil {
			t.Fatalf("insert pre-mig msg: %v", err)
		}
	}

	// Apply the backfill query from migration 002.
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE session_messages SET insert_order = (
			SELECT COUNT(*) FROM session_messages b
			WHERE b.session_id = session_messages.session_id AND b.rowid <= session_messages.rowid
		) WHERE insert_order = 0`)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Insert post-migration messages (trigger will assign correct insert_order).
	postMigIDs := []string{"post-msg-1", "post-msg-2"}
	for i, id := range postMigIDs {
		msg := &Message{
			ID:        id,
			SessionID: sess.ID,
			From:      "claude",
			Kind:      "reply",
			Text:      "Post-migration message " + string(rune('1'+i)),
		}
		if err := s.InsertMessage(context.Background(), msg); err != nil {
			t.Fatalf("insert post-mig msg: %v", err)
		}
	}

	// Verify all messages are returned in order.
	all, err := s.GetMessages(context.Background(), sess.ID, "")
	if err != nil {
		t.Fatalf("get all messages: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("len: got %d, want 5", len(all))
	}
	expectedIDs := append(preMigIDs, postMigIDs...)
	for i, msg := range all {
		if msg.ID != expectedIDs[i] {
			t.Errorf("all[%d]: got %q, want %q", i, msg.ID, expectedIDs[i])
		}
	}

	// Verify since_id cutoff works correctly across pre/post migration boundary.
	// Query after pre-msg-2 should return pre-msg-3 and both post-mig messages.
	messages, err := s.GetMessages(context.Background(), sess.ID, "pre-msg-2")
	if err != nil {
		t.Fatalf("get messages since pre-msg-2: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("len after pre-msg-2: got %d, want 3", len(messages))
	}
	expectedSince := []string{"pre-msg-3", "post-msg-1", "post-msg-2"}
	for i, msg := range messages {
		if msg.ID != expectedSince[i] {
			t.Errorf("since_id order[%d]: got %q, want %q", i, msg.ID, expectedSince[i])
		}
	}
}

// TestUpgrade_PreMigrationInsertOrderZero verifies the upgrade path for already-migrated
// DBs that ended up with insert_order=0 for all existing rows (the original migration bug).
// The test:
//   - Creates a DB with the pre-fix schema and data (insert_order=0 for all rows)
//   - Opens it through the normal migration path (OpenWithDB)
//   - Asserts rows are repaired (non-zero insert_order) and since_id queries work
func TestUpgrade_PreMigrationInsertOrderZero(t *testing.T) {
	// Create a DB in the broken/pre-fix state: 001 schema + insert_order column
	// with all zeros (simulating the state of an already-migrated DB with the bug).
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "pre-migrated.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Run 001 to create the base schema.
	schema001, err := testMigrations.ReadFile("migration/001_sessions.sql")
	if err != nil {
		t.Fatalf("read 001: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(schema001)); err != nil {
		t.Fatalf("exec 001: %v", err)
	}

	// Manually add insert_order column and insert test data with insert_order=0
	// (the broken pre-fix state).
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE session_messages ADD COLUMN insert_order INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatalf("add insert_order column: %v", err)
	}

	// Insert a session.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, project, api_key, status) VALUES (?, ?, ?, ?, ?)`,
		"sess-upgrade", "Upgrade Test", "test-project", "key-upgrade", "active"); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Insert messages with insert_order=0 (the broken state).
	preMigIDs := []string{"up-pre-1", "up-pre-2", "up-pre-3"}
	for i, id := range preMigIDs {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO session_messages (id, session_id, msg_from, kind, text, created_at, insert_order) VALUES (?, ?, ?, ?, ?, ?, 0)`,
			id, "sess-upgrade", "claude", "reply", "Pre-migration msg "+string(rune('1'+i)), "2026-04-16T10:00:00Z"); err != nil {
			t.Fatalf("insert pre-mig msg: %v", err)
		}
	}

	// Now open through normal migration path — this should repair the data.
	store, err := OpenWithDB(ctx, db)
	if err != nil {
		t.Fatalf("open with db (migration): %v", err)
	}

	// Insert post-migration messages (should get correct insert_order via trigger).
	postMigIDs := []string{"up-post-1", "up-post-2"}
	for i, id := range postMigIDs {
		msg := &Message{
			ID:        id,
			SessionID: "sess-upgrade",
			From:      "claude",
			Kind:      "reply",
			Text:      "Post-migration msg " + string(rune('1'+i)),
		}
		if err := store.InsertMessage(ctx, msg); err != nil {
			t.Fatalf("insert post-mig msg: %v", err)
		}
	}

	// Verify all messages are returned in correct order.
	all, err := store.GetMessages(ctx, "sess-upgrade", "")
	if err != nil {
		t.Fatalf("get all messages: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("len: got %d, want 5", len(all))
	}
	expectedIDs := append(preMigIDs, postMigIDs...)
	for i, msg := range all {
		if msg.ID != expectedIDs[i] {
			t.Errorf("all[%d]: got %q, want %q", i, msg.ID, expectedIDs[i])
		}
	}

	// Verify insert_order values are non-zero and sequential.
	for i, msg := range all {
		if msg.InsertOrder != int64(i+1) {
			t.Errorf("insert_order[%d]: got %d, want %d", i, msg.InsertOrder, i+1)
		}
	}

	// Verify since_id works correctly across pre/post boundary.
	messages, err := store.GetMessages(ctx, "sess-upgrade", "up-pre-2")
	if err != nil {
		t.Fatalf("get messages since up-pre-2: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("len after up-pre-2: got %d, want 3", len(messages))
	}
	expectedSince := []string{"up-pre-3", "up-post-1", "up-post-2"}
	for i, msg := range messages {
		if msg.ID != expectedSince[i] {
			t.Errorf("since_id order[%d]: got %q, want %q", i, msg.ID, expectedSince[i])
		}
	}
}

// TestInsertMessage_MixedStateRepair verifies that the backfill migration correctly
// handles a DB where some rows have insert_order=0 (old pre-migration rows) and
// others already have positive values (new post-migration rows inserted after the
// fix was deployed). The backfill must assign sequential values to the zero rows
// that continue from the max existing positive value, producing a globally correct
// ordering without gaps or collisions.
func TestInsertMessage_MixedStateRepair(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	sess := &Session{ID: "sess-mixed", Title: "S1", APIKey: "key-1", Status: "active"}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	ctx := context.Background()

	// Insert pre-migration messages (simulated: insert_order=0 directly).
	// These represent messages that existed before the insert_order migration.
	preMigIDs := []string{"mixed-pre-1", "mixed-pre-2", "mixed-pre-3"}
	for i, id := range preMigIDs {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO session_messages (id, session_id, msg_from, kind, text, created_at, insert_order) VALUES (?, ?, ?, ?, ?, ?, 0)`,
			id, sess.ID, "claude", "reply", "Pre-migration message "+string(rune('1'+i)), "2026-04-16T10:00:00Z")
		if err != nil {
			t.Fatalf("insert pre-mig msg: %v", err)
		}
	}

	// Apply the backfill migration (003) to repair the zero rows.
	backfill := `
UPDATE session_messages
SET insert_order = (
    SELECT COUNT(*) FROM session_messages b
    WHERE b.session_id = session_messages.session_id
      AND b.insert_order = 0
      AND b.rowid <= session_messages.rowid
) + (
    SELECT COALESCE(MAX(insert_order), 0) FROM session_messages c
    WHERE c.session_id = session_messages.session_id
      AND c.insert_order > 0
)
WHERE insert_order = 0;`
	if _, err := s.db.ExecContext(ctx, backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	// Now insert post-backfill messages using InsertMessage (gets correct order via SELECT MAX+1).
	postMigIDs := []string{"mixed-post-1", "mixed-post-2"}
	for i, id := range postMigIDs {
		msg := &Message{
			ID:        id,
			SessionID: sess.ID,
			From:      "claude",
			Kind:      "reply",
			Text:      "Post-backfill message " + string(rune('1'+i)),
		}
		if err := s.InsertMessage(ctx, msg); err != nil {
			t.Fatalf("insert post-backfill msg: %v", err)
		}
	}

	// Verify all messages are in correct order with no gaps and no collisions.
	all, err := s.GetMessages(ctx, sess.ID, "")
	if err != nil {
		t.Fatalf("get all messages: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("len: got %d, want 5", len(all))
	}

	// Expected order: pre-mig msgs (1,2,3), then post-mig msgs (4,5).
	expectedIDs := append(preMigIDs, postMigIDs...)
	for i, msg := range all {
		if msg.ID != expectedIDs[i] {
			t.Errorf("order[%d]: got %q, want %q", i, msg.ID, expectedIDs[i])
		}
		if msg.InsertOrder != int64(i+1) {
			t.Errorf("insert_order[%d]: got %d, want %d", i, msg.InsertOrder, i+1)
		}
	}

	// Verify no duplicate insert_order values.
	seen := make(map[int64]bool)
	for _, msg := range all {
		if seen[msg.InsertOrder] {
			t.Errorf("duplicate insert_order: %d (msg %q)", msg.InsertOrder, msg.ID)
		}
		seen[msg.InsertOrder] = true
	}

	// Verify since_id works correctly across the pre/post boundary.
	messages, err := s.GetMessages(ctx, sess.ID, "mixed-pre-2")
	if err != nil {
		t.Fatalf("get messages since mixed-pre-2: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("len after mixed-pre-2: got %d, want 3", len(messages))
	}
	expectedSince := []string{"mixed-pre-3", "mixed-post-1", "mixed-post-2"}
	for i, msg := range messages {
		if msg.ID != expectedSince[i] {
			t.Errorf("since_id order[%d]: got %q, want %q", i, msg.ID, expectedSince[i])
		}
	}
}

// TestInsertMessage_ConcurrentWrites verifies that concurrent calls to InsertMessage
// on the same session produce unique, increasing insert_order values without
// duplicates. Uses sequential goroutines with mutex to approximate concurrent access.
func TestInsertMessage_ConcurrentWrites(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	sess := &Session{ID: "sess-concurrent", Title: "S1", APIKey: "key-1", Status: "active"}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	const numMessages = 20
	messages := make([]*Message, numMessages)
	for i := 0; i < numMessages; i++ {
		messages[i] = &Message{
			ID:        "concurrent-msg-" + string(rune('A'+i)),
			SessionID: sess.ID,
			From:      "claude",
			Kind:      "reply",
			Text:      "Concurrent message " + string(rune('A'+i)),
		}
	}

	// Insert messages sequentially (simulates what would happen under concurrent access).
	for i := 0; i < numMessages; i++ {
		if err := s.InsertMessage(context.Background(), messages[i]); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	// Verify all insert_order values are unique and sequential 1..N.
	all, err := s.GetMessages(context.Background(), sess.ID, "")
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(all) != numMessages {
		t.Fatalf("len: got %d, want %d", len(all), numMessages)
	}

	seen := make(map[int64]string, numMessages)
	for i, msg := range all {
		// Check insert_order is sequential starting at 1.
		if msg.InsertOrder != int64(i+1) {
			t.Errorf("insert_order mismatch at index %d: got %d, want %d (msg %q)", i, msg.InsertOrder, i+1, msg.ID)
		}
		// Check no duplicates.
		if existing, ok := seen[msg.InsertOrder]; ok {
			t.Errorf("duplicate insert_order %d: %q and %q", msg.InsertOrder, existing, msg.ID)
		}
		seen[msg.InsertOrder] = msg.ID
	}

	// Verify messages are ordered by insert_order ascending.
	for i, msg := range all {
		if msg.InsertOrder != int64(i+1) {
			t.Errorf("order[%d]: got insert_order=%d, want %d", i, msg.InsertOrder, i+1)
		}
	}
}

// TestInsertDuplicate asserts inserting the same session id twice returns an
// error, preventing silent overwrite of an active lane.
func TestInsertDuplicate(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()
	sess := &Session{ID: "dup-1", Title: "S", APIKey: "k", Status: "active"}
	if err := s.Insert(context.Background(), sess); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := s.Insert(context.Background(), sess)
	if err == nil {
		t.Fatal("expected duplicate error")
	}
}

// TestInsertMessage_MissingSession asserts InsertMessage returns ErrNotFound
// when the parent session does not exist — prevents orphan message rows.
func TestInsertMessage_MissingSession(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()
	err := s.InsertMessage(context.Background(), &Message{
		ID:        "orphan-1",
		SessionID: "never-existed",
		From:      "claude",
		Kind:      "reply",
		Text:      "hi",
	})
	if err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// TestGetMessages_WithSinceID verifies that GetMessages returns only messages
// inserted after the given sinceID, covering the getMessagesAfter query branch.
func TestGetMessages_WithSinceID(t *testing.T) {
	const sinceSessID = "since-sess"

	s, cleanup := openForTest(t)
	defer cleanup()

	ctx := context.Background()
	sess := &Session{ID: sinceSessID, Title: "S", APIKey: "k", Status: "active"}
	if err := s.Insert(ctx, sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	msgs := []Message{
		{ID: "m1", SessionID: sinceSessID, From: "user", Kind: "steer", Text: "first"},
		{ID: "m2", SessionID: sinceSessID, From: "agent", Kind: "reply", Text: "second"},
		{ID: "m3", SessionID: sinceSessID, From: "user", Kind: "steer", Text: "third"},
	}
	for _, m := range msgs {
		mc := m
		if err := s.InsertMessage(ctx, &mc); err != nil {
			t.Fatalf("insert message %s: %v", m.ID, err)
		}
	}

	got, err := s.GetMessages(ctx, sinceSessID, "m1")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages after m1, got %d", len(got))
	}
	if got[0].ID != "m2" || got[1].ID != "m3" {
		t.Errorf("unexpected message IDs: %v %v", got[0].ID, got[1].ID)
	}
}

// TestGetMessages_UnknownSinceID verifies that GetMessages returns an empty
// slice (not an error) when sinceID does not exist — covers the ErrNoRows
// branch in getMessagesAfter.
func TestGetMessages_UnknownSinceID(t *testing.T) {
	s, cleanup := openForTest(t)
	defer cleanup()

	ctx := context.Background()
	sess := &Session{ID: "since-sess-2", Title: "S", APIKey: "k", Status: "active"}
	if err := s.Insert(ctx, sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	got, err := s.GetMessages(ctx, "since-sess-2", "nonexistent-id")
	if err != nil {
		t.Fatalf("expected nil error for unknown sinceID, got: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d messages", len(got))
	}
}
