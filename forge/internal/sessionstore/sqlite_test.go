package sessionstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

func sampleSession(id, envID string) *Session {
	now := time.Date(2026, 4, 12, 5, 0, 0, 0, time.UTC)
	return &Session{
		SessionID:  id,
		EnvelopeID: envID,
		Executor:   "opencode",
		State:      "starting",
		StartedAt:  now,
		LastSeenAt: now,
	}
}

func TestStore_InsertGet_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess := sampleSession("s-1", "env-1")

	if err := s.Insert(ctx, sess); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Get(ctx, "s-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionID != "s-1" || got.EnvelopeID != "env-1" || got.Executor != "opencode" {
		t.Fatalf("mismatch: %+v", got)
	}
	if got.State != "starting" {
		t.Fatalf("expected state=starting, got %q", got.State)
	}
}

func TestStore_Insert_Duplicate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess := sampleSession("s-dup", "env-dup")

	if err := s.Insert(ctx, sess); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.Insert(ctx, sess)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_GetByEnvelope(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess := sampleSession("s-env", "env-lookup")
	if err := s.Insert(ctx, sess); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetByEnvelope(ctx, "env-lookup")
	if err != nil {
		t.Fatalf("get by envelope: %v", err)
	}
	if got.SessionID != "s-env" {
		t.Fatalf("expected s-env, got %s", got.SessionID)
	}

	// unknown envelope → not found
	_, err = s.GetByEnvelope(ctx, "env-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStore_List(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Date(2026, 4, 12, 5, 0, 0, 0, time.UTC)

	s1 := sampleSession("s-a", "env-a")
	s1.StartedAt = base
	s2 := sampleSession("s-b", "env-b")
	s2.StartedAt = base.Add(1 * time.Second)

	_ = s.Insert(ctx, s1)
	_ = s.Insert(ctx, s2)

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(list))
	}
	// most recent first
	if list[0].SessionID != "s-b" {
		t.Fatalf("expected most-recent first (s-b), got %s", list[0].SessionID)
	}
}

// W-F1: sessions survive close/reopen.
func TestStore_PersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "persist.db")

	s1, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	_ = s1.Insert(ctx, sampleSession("s-persist", "env-persist"))
	_ = s1.Close()

	s2, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.Get(ctx, "s-persist")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.EnvelopeID != "env-persist" {
		t.Fatalf("expected env-persist, got %s", got.EnvelopeID)
	}
}

func TestStore_OpenInvalidDSN(t *testing.T) {
	_, err := Open(context.Background(), "/dev/null/impossible.db")
	if err == nil {
		t.Fatal("expected error for invalid DSN")
	}
}

func TestStore_ListEmpty(t *testing.T) {
	s := newTestStore(t)
	sessions, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0, got %d", len(sessions))
	}
}
