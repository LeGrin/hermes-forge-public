package envelopestore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/envelope"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleEnvelope() *envelope.Envelope {
	session := "session-42"
	return &envelope.Envelope{
		ID:                 "env-roundtrip",
		CreatedAt:          time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC),
		CreatedBy:          "kitt",
		Title:              "roundtrip test",
		Domain:             "ops",
		Project:            "hermes",
		TargetNode:         "mac-forge",
		TargetExecutor:     "opencode",
		TaskTitle:          "run smoke",
		TaskGoal:           "prove transport",
		TaskSteps:          []string{"s1", "s2"},
		SuccessCriteria:    []string{"green tests"},
		EscalationCriteria: []string{"exec crash"},
		ProofRequired:      []string{"commit"},
		Status:             envelope.StatusCreated,
		Delivery:           envelope.Delivery{Delivered: false},
		CapabilityHints:    []string{"new_session", "requires_tdd"},
		SessionBinding:     &session,
		ExecutorSessionID:  "forge-session-abc",
		Thread: []envelope.Message{
			{ID: "msg-1", From: "kitt", Kind: "steer", Text: "start working", At: time.Date(2026, 4, 12, 3, 1, 0, 0, time.UTC)},
			{ID: "msg-2", From: "opencode", Kind: "reply", Text: "working on it", ReplyTo: "msg-1", At: time.Date(2026, 4, 12, 3, 2, 0, 0, time.UTC)},
		},
		Metrics: envelope.Metrics{RetryCount: 0},
		History: []string{"created by kitt"},
		Proof:   map[string]string{"note": "pending"},
	}
}

func TestOpen_InvalidDSN(t *testing.T) {
	_, err := Open(context.Background(), "/dev/null/impossible.db")
	if err == nil {
		t.Fatal("expected error for invalid DSN")
	}
}

func TestStore_InsertGet_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := sampleEnvelope()

	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.ID != e.ID || got.TaskTitle != e.TaskTitle || got.Status != e.Status {
		t.Fatalf("scalar mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(e.CreatedAt) {
		t.Fatalf("created_at mismatch: got %v want %v", got.CreatedAt, e.CreatedAt)
	}
	if len(got.TaskSteps) != 2 || got.TaskSteps[1] != "s2" {
		t.Fatalf("task_steps not preserved: %v", got.TaskSteps)
	}
	if len(got.CapabilityHints) != 2 {
		t.Fatalf("hints not preserved: %v", got.CapabilityHints)
	}
	if got.SessionBinding == nil || *got.SessionBinding != "session-42" {
		t.Fatalf("session_binding not preserved: %v", got.SessionBinding)
	}
	if got.Proof["note"] != "pending" {
		t.Fatalf("proof map not preserved: %v", got.Proof)
	}
}

// v2-001: Title, ExecutorSessionID, and Thread round-trip.
func TestStore_InsertGet_RoundTrip_V2Fields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := sampleEnvelope()

	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Title != e.Title {
		t.Fatalf("title mismatch: got %q want %q", got.Title, e.Title)
	}
	if got.ExecutorSessionID != e.ExecutorSessionID {
		t.Fatalf("executor_session_id mismatch: got %q want %q", got.ExecutorSessionID, e.ExecutorSessionID)
	}
	if len(got.Thread) != 2 {
		t.Fatalf("thread length mismatch: got %d want 2", len(got.Thread))
	}
	if got.Thread[0].ID != "msg-1" || got.Thread[0].From != "kitt" {
		t.Fatalf("thread[0] mismatch: %+v", got.Thread[0])
	}
	if got.Thread[1].ReplyTo != "msg-1" {
		t.Fatalf("thread[1].reply_to mismatch: got %q want msg-1", got.Thread[1].ReplyTo)
	}
}

// v2-001: SetExecutorSessionID updates the field.
func TestStore_SetExecutorSessionID_OK(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := sampleEnvelope()
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.SetExecutorSessionID(ctx, e.ID, "new-session-xyz"); err != nil {
		t.Fatalf("SetExecutorSessionID: %v", err)
	}

	got, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExecutorSessionID != "new-session-xyz" {
		t.Fatalf("expected new-session-xyz, got %q", got.ExecutorSessionID)
	}
}

func TestStore_SetExecutorSessionID_NotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.SetExecutorSessionID(context.Background(), "nope", "session")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// v2-001: SetExecutorSessionID must reject terminal envelopes.
func TestStore_SetExecutorSessionID_Terminal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := sampleEnvelope()
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Move to done (sampleEnvelope requires proof "commit", so provide it).
	if _, err := s.UpdateStatus(ctx, e.ID, envelope.StatusDone, map[string]string{"commit": "abc123"}, ""); err != nil {
		t.Fatalf("move to done: %v", err)
	}

	err := s.SetExecutorSessionID(ctx, e.ID, "new-session")
	if err == nil {
		t.Fatal("expected error on terminal envelope, got nil")
	}
	if !strings.Contains(err.Error(), "terminal envelope") {
		t.Fatalf("expected terminal envelope error, got: %v", err)
	}
}

// v2-001: Migration is idempotent — re-opening the store does not re-apply migrations.
func TestStore_Migration_Idempotent(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "idempotent.db")

	// First open.
	s1, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	// Second open — must succeed without re-running migrations.
	s2, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("second open (idempotency check): %v", err)
	}
	_ = s2.Close()
}

func TestStore_Insert_Duplicate_Rejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := sampleEnvelope()

	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := s.Insert(ctx, e)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate on second insert, got %v", err)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, err := s.Get(ctx, "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_Insert_RejectsInvalidEnvelope(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := sampleEnvelope()
	e.TaskTitle = "" // triggers envelope.Validate error

	if err := s.Insert(ctx, e); err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

// minimalCreated inserts a freshly-minted 'created' envelope with the
// given id and created_at. Used by NextCreated / MarkDelivered tests.
func minimalCreated(t *testing.T, s *Store, id string, createdAt time.Time) *envelope.Envelope {
	t.Helper()
	e := &envelope.Envelope{
		ID:             id,
		CreatedAt:      createdAt,
		CreatedBy:      "kitt",
		Title:          "test envelope",
		TaskTitle:      "run smoke",
		TargetExecutor: "opencode",
		Status:         envelope.StatusCreated,
	}
	if err := s.Insert(context.Background(), e); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
	return e
}

func TestStore_NextCreated_PicksOldest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-b", base.Add(1*time.Second))
	minimalCreated(t, s, "env-a", base) // oldest
	minimalCreated(t, s, "env-c", base.Add(2*time.Second))

	got, err := s.NextCreated(ctx)
	if err != nil {
		t.Fatalf("next created: %v", err)
	}
	if got.ID != "env-a" {
		t.Fatalf("expected env-a (oldest), got %s", got.ID)
	}
}

func TestStore_NextCreated_Empty(t *testing.T) {
	s := newTestStore(t)
	_, err := s.NextCreated(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_NextCreated_IgnoresNonCreated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-done", base)
	// Flip the only row out of created; NextCreated should see nothing.
	if err := s.MarkDelivered(ctx, "env-done", "session-x", base.Add(time.Second)); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if _, err := s.NextCreated(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_MarkDelivered_FlipsAndPersists(t *testing.T) {
	// W-H5: flip writes status + delivery + session_binding atomically.
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-flip", base)

	deliveredAt := base.Add(10 * time.Second)
	if err := s.MarkDelivered(ctx, "env-flip", "session-flip", deliveredAt); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	got, err := s.Get(ctx, "env-flip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != envelope.StatusDelivered {
		t.Fatalf("expected status=delivered, got %q", got.Status)
	}
	if got.SessionBinding == nil || *got.SessionBinding != "session-flip" {
		t.Fatalf("session_binding not persisted: %v", got.SessionBinding)
	}
	if !got.Delivery.Delivered {
		t.Fatalf("expected delivery.delivered=true")
	}
	if got.Delivery.DeliveredAt == nil || !got.Delivery.DeliveredAt.Equal(deliveredAt) {
		t.Fatalf("delivered_at mismatch: %v", got.Delivery.DeliveredAt)
	}
}

func TestStore_MarkDelivered_Idempotent(t *testing.T) {
	// Second MarkDelivered on the same row is a no-op returning ErrNotFound
	// — the WHERE clause guards against double-flips.
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-once", base)

	if err := s.MarkDelivered(ctx, "env-once", "s1", base.Add(time.Second)); err != nil {
		t.Fatalf("first mark: %v", err)
	}
	err := s.MarkDelivered(ctx, "env-once", "s2", base.Add(2*time.Second))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second flip, got %v", err)
	}
	// The first flip wins; session binding must not have been overwritten.
	got, _ := s.Get(ctx, "env-once")
	if *got.SessionBinding != "s1" {
		t.Fatalf("session_binding overwritten: %q", *got.SessionBinding)
	}
}

func TestStore_MarkDelivered_UnknownID(t *testing.T) {
	s := newTestStore(t)
	err := s.MarkDelivered(context.Background(), "nope", "s", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- UpdateStatus tests (H-006b) ---

func TestStore_UpdateStatus_OK(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-up", base)

	got, err := s.UpdateStatus(ctx, "env-up", envelope.StatusDelivered, nil, "")
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if got.Status != envelope.StatusDelivered {
		t.Fatalf("expected delivered, got %q", got.Status)
	}
	if got.Metrics.UpdatedAt == nil {
		t.Fatal("expected metrics.updated_at to be set")
	}
}

func TestStore_UpdateStatus_TerminalReject(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-t", base)

	// Move to done (no proof_required).
	if _, err := s.UpdateStatus(ctx, "env-t", envelope.StatusDone, nil, ""); err != nil {
		t.Fatalf("move to done: %v", err)
	}
	// Try to leave done.
	_, err := s.UpdateStatus(ctx, "env-t", envelope.StatusInProgress, nil, "")
	if !errors.Is(err, envelope.ErrTerminalTransition) {
		t.Fatalf("expected ErrTerminalTransition, got %v", err)
	}
}

func TestStore_UpdateStatus_DoneRequiresProof(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := &envelope.Envelope{
		ID:             "env-pr",
		CreatedAt:      time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC),
		CreatedBy:      "kitt",
		Title:          "proof test",
		TaskTitle:      "test",
		TargetExecutor: "x",
		Status:         envelope.StatusCreated,
		ProofRequired:  []string{"log_url"},
	}
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Without proof → rejected.
	_, err := s.UpdateStatus(ctx, "env-pr", envelope.StatusDone, nil, "")
	if !errors.Is(err, envelope.ErrDoneWithoutProof) {
		t.Fatalf("expected ErrDoneWithoutProof, got %v", err)
	}

	// With proof → accepted.
	got, err := s.UpdateStatus(ctx, "env-pr", envelope.StatusDone, map[string]string{"log_url": "https://example.com"}, "")
	if err != nil {
		t.Fatalf("update with proof: %v", err)
	}
	if got.Proof["log_url"] != "https://example.com" {
		t.Fatalf("proof not persisted: %v", got.Proof)
	}
}

func TestStore_UpdateStatus_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UpdateStatus(context.Background(), "nope", envelope.StatusDelivered, nil, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_UpdateStatus_ProofAccumulates(t *testing.T) {
	// Proof keys merge across multiple UpdateStatus calls.
	ctx := context.Background()
	s := newTestStore(t)
	e := &envelope.Envelope{
		ID:             "env-acc",
		CreatedAt:      time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC),
		CreatedBy:      "kitt",
		Title:          "accumulate proof test",
		TaskTitle:      "test",
		TargetExecutor: "x",
		Status:         envelope.StatusCreated,
		ProofRequired:  []string{"log_url", "screenshot"},
	}
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// First call: provide log_url, move to in_progress.
	if _, err := s.UpdateStatus(ctx, "env-acc", envelope.StatusInProgress,
		map[string]string{"log_url": "https://example.com/log"}, ""); err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Second call: provide screenshot, move to done — should succeed
	// because log_url was persisted from the first call.
	got, err := s.UpdateStatus(ctx, "env-acc", envelope.StatusDone,
		map[string]string{"screenshot": "https://example.com/ss"}, "")
	if err != nil {
		t.Fatalf("done with accumulated proof: %v", err)
	}
	if got.Proof["log_url"] != "https://example.com/log" {
		t.Fatalf("log_url not accumulated: %v", got.Proof)
	}
	if got.Proof["screenshot"] != "https://example.com/ss" {
		t.Fatalf("screenshot not in proof: %v", got.Proof)
	}
}

func TestStore_UpdateStatus_TerminalSetsCompletedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-comp", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	got, err := s.UpdateStatus(ctx, "env-comp", envelope.StatusDone, nil, "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Metrics.CompletedAt == nil {
		t.Fatal("expected completed_at to be set for terminal state")
	}
}

func TestStore_UpdateStatus_HistoryWithNote(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-hist", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	got, err := s.UpdateStatus(ctx, "env-hist", envelope.StatusBlocked, nil, "waiting for API key from ops")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(got.History) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(got.History))
	}
	entry := got.History[0]
	if !strings.Contains(entry, "created → blocked") {
		t.Fatalf("history entry missing transition: %q", entry)
	}
	if !strings.Contains(entry, "waiting for API key from ops") {
		t.Fatalf("history entry missing note: %q", entry)
	}

	// Second transition should accumulate.
	got, err = s.UpdateStatus(ctx, "env-hist", envelope.StatusInProgress, nil, "API key received")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(got.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(got.History))
	}
	if !strings.Contains(got.History[1], "blocked → in_progress") {
		t.Fatalf("second entry missing transition: %q", got.History[1])
	}
	if !strings.Contains(got.History[1], "API key received") {
		t.Fatalf("second entry missing note: %q", got.History[1])
	}
}

func TestStore_UpdateStatus_HistoryWithoutNote(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-hn", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	got, err := s.UpdateStatus(ctx, "env-hn", envelope.StatusDelivered, nil, "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(got.History) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(got.History))
	}
	// Must contain the transition but NOT a trailing ": " (no note).
	entry := got.History[0]
	if !strings.Contains(entry, "created → delivered") {
		t.Fatalf("history entry missing transition: %q", entry)
	}
	// The entry should end with "delivered", not "delivered: " or "delivered: something".
	if strings.Contains(entry, "delivered: ") {
		t.Fatalf("history entry has note content when none was provided: %q", entry)
	}
}

// --- List tests (H-007b) ---

func TestStore_List_All(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-l1", base)
	minimalCreated(t, s, "env-l2", base.Add(time.Second))

	got, err := s.List(ctx, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	// Most recent first.
	if got[0].ID != "env-l2" {
		t.Fatalf("expected env-l2 first, got %s", got[0].ID)
	}
}

func TestStore_List_ByStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC)
	minimalCreated(t, s, "env-s1", base)
	minimalCreated(t, s, "env-s2", base.Add(time.Second))

	// Move env-s1 to blocked.
	if _, err := s.UpdateStatus(ctx, "env-s1", envelope.StatusBlocked, nil, "stuck"); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.List(ctx, []envelope.Status{envelope.StatusBlocked})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "env-s1" {
		t.Fatalf("expected [env-s1], got %v", got)
	}
}

// --- AddHistory tests ---

func TestStore_AddHistory_OK(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-ah", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	got, err := s.AddHistory(ctx, "env-ah", "[DECISION] chose SQLite")
	if err != nil {
		t.Fatalf("add history: %v", err)
	}
	if len(got.History) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got.History))
	}
	if !strings.Contains(got.History[0], "[DECISION] chose SQLite") {
		t.Fatalf("entry not preserved: %q", got.History[0])
	}
}

func TestStore_AddHistory_Accumulates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-ah2", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	if _, err := s.AddHistory(ctx, "env-ah2", "first"); err != nil {
		t.Fatalf("first: %v", err)
	}
	got, err := s.AddHistory(ctx, "env-ah2", "second")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(got.History) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got.History))
	}
}

func TestStore_AddHistory_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.AddHistory(context.Background(), "nope", "entry")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- Open DSN variants ---

func TestOpen_WithExistingQueryString(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=WAL"
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()
}

func TestOpen_WithBusyTimeoutAlready(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_busy_timeout=3000"
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()
}

// --- UpdateStatus read delivery ---

func TestStore_UpdateStatus_ReadSetsDelivery(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-rd", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	got, err := s.UpdateStatus(ctx, "env-rd", envelope.StatusRead, nil, "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !got.Delivery.Read {
		t.Fatal("expected delivery.read=true after status=read")
	}
	if got.Delivery.ReadAt == nil {
		t.Fatal("expected delivery.read_at set after status=read")
	}
}

// --- Insert with nil session binding ---

func TestStore_Insert_NilSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := sampleEnvelope()
	e.ID = "env-nil-session"
	e.SessionBinding = nil
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Get(ctx, "env-nil-session")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionBinding != nil {
		t.Fatalf("expected nil session, got %v", got.SessionBinding)
	}
}

func TestStore_List_Empty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// --- AppendMessage tests (v2-002) ---

func TestStore_AppendMessage_OK(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-thread", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	msg := envelope.Message{
		ID:   "msg-1",
		From: "kitt",
		Kind: "steer",
		Text: "start working",
		At:   time.Date(2026, 4, 12, 3, 5, 0, 0, time.UTC),
	}
	if err := s.AppendMessage(ctx, "env-thread", msg); err != nil {
		t.Fatalf("append message: %v", err)
	}

	got, err := s.Get(ctx, "env-thread")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Thread) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got.Thread))
	}
	if got.Thread[0].ID != "msg-1" {
		t.Fatalf("thread[0].id = %q, want msg-1", got.Thread[0].ID)
	}
	if got.Thread[0].From != "kitt" {
		t.Fatalf("thread[0].from = %q, want kitt", got.Thread[0].From)
	}
	if got.Thread[0].Kind != "steer" {
		t.Fatalf("thread[0].kind = %q, want steer", got.Thread[0].Kind)
	}
	if got.Thread[0].Text != "start working" {
		t.Fatalf("thread[0].text = %q, want start working", got.Thread[0].Text)
	}
}

func TestStore_AppendMessage_Accumulates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-acc", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	msg1 := envelope.Message{ID: "m1", From: "kitt", Kind: "steer", Text: "first", At: time.Now().UTC()}
	msg2 := envelope.Message{ID: "m2", From: "opencode", Kind: "reply", Text: "second", ReplyTo: "m1", At: time.Now().UTC()}
	if err := s.AppendMessage(ctx, "env-acc", msg1); err != nil {
		t.Fatalf("append msg1: %v", err)
	}
	if err := s.AppendMessage(ctx, "env-acc", msg2); err != nil {
		t.Fatalf("append msg2: %v", err)
	}

	got, err := s.Get(ctx, "env-acc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Thread) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got.Thread))
	}
	if got.Thread[1].ReplyTo != "m1" {
		t.Fatalf("thread[1].reply_to = %q, want m1", got.Thread[1].ReplyTo)
	}
}

func TestStore_AppendMessage_NotFound(t *testing.T) {
	s := newTestStore(t)
	msg := envelope.Message{ID: "m1", From: "kitt", Kind: "steer", Text: "orphan"}
	err := s.AppendMessage(context.Background(), "nope", msg)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- GetThread tests (v2-002) ---

func TestStore_GetThread_OK(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := &envelope.Envelope{
		ID:             "env-gt",
		CreatedAt:      time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC),
		CreatedBy:      "kitt",
		Title:          "get thread test",
		TaskTitle:      "test",
		TargetExecutor: "opencode",
		Status:         envelope.StatusCreated,
		Thread: []envelope.Message{
			{ID: "g1", From: "kitt", Kind: "steer", Text: "first", At: time.Date(2026, 4, 12, 3, 1, 0, 0, time.UTC)},
			{ID: "g2", From: "opencode", Kind: "reply", Text: "second", ReplyTo: "g1", At: time.Date(2026, 4, 12, 3, 2, 0, 0, time.UTC)},
			{ID: "g3", From: "kitt", Kind: "steer", Text: "third", At: time.Date(2026, 4, 12, 3, 3, 0, 0, time.UTC)},
		},
	}
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetThread(ctx, "env-gt", "")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
}

func TestStore_GetThread_WithSinceID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	e := &envelope.Envelope{
		ID:             "env-gt2",
		CreatedAt:      time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC),
		CreatedBy:      "kitt",
		Title:          "get thread since test",
		TaskTitle:      "test",
		TargetExecutor: "opencode",
		Status:         envelope.StatusCreated,
		Thread: []envelope.Message{
			{ID: "s1", From: "kitt", Kind: "steer", Text: "first", At: time.Date(2026, 4, 12, 3, 1, 0, 0, time.UTC)},
			{ID: "s2", From: "opencode", Kind: "reply", Text: "second", ReplyTo: "s1", At: time.Date(2026, 4, 12, 3, 2, 0, 0, time.UTC)},
			{ID: "s3", From: "kitt", Kind: "steer", Text: "third", At: time.Date(2026, 4, 12, 3, 3, 0, 0, time.UTC)},
		},
	}
	if err := s.Insert(ctx, e); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetThread(ctx, "env-gt2", "s1")
	if err != nil {
		t.Fatalf("get thread since: %v", err)
	}
	// Should return messages AFTER s1 (exclusive), which are s2 and s3
	if len(got) != 2 {
		t.Fatalf("expected 2 messages after s1, got %d", len(got))
	}
	if got[0].ID != "s2" {
		t.Fatalf("expected s2 first, got %s", got[0].ID)
	}
	if got[1].ID != "s3" {
		t.Fatalf("expected s3 second, got %s", got[1].ID)
	}
}

func TestStore_GetThread_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetThread(context.Background(), "nope", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_GetThread_Empty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	minimalCreated(t, s, "env-empty", time.Date(2026, 4, 12, 3, 0, 0, 0, time.UTC))

	got, err := s.GetThread(ctx, "env-empty", "")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	// Empty thread stored as JSON [] unmarshals to empty slice, not nil.
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}
