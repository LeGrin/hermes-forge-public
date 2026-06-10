package keystore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, err := OpenWithDB(context.Background(), db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestCreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	k, err := s.Create(ctx, "LeGrin", "admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(k.Key, "hk_") {
		t.Fatalf("expected hk_ prefix, got %q", k.Key)
	}
	if k.Label != "LeGrin" {
		t.Fatalf("label = %q", k.Label)
	}
	if k.Role != "admin" {
		t.Fatalf("role = %q", k.Role)
	}

	got, err := s.Get(ctx, k.Key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Key != k.Key {
		t.Fatalf("keys mismatch")
	}
}

func TestCreateDefaultRole(t *testing.T) {
	s := openTestStore(t)
	k, err := s.Create(context.Background(), "User B", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.Role != "user" {
		t.Fatalf("expected default role 'user', got %q", k.Role)
	}
}

func TestGetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Get(context.Background(), "dev-key-nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, "A", "user"); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := s.Create(ctx, "B", "admin"); err != nil {
		t.Fatalf("create B: %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
}

func TestDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	k, err := s.Create(ctx, "ToDelete", "user")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete(ctx, k.Key); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err = s.Get(ctx, k.Key)
	if err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestDeleteNotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.Delete(context.Background(), "dev-key-nope")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateDuplicateLabel(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, "same", "user"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.Create(ctx, "same", "admin")
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got: %v", err)
	}
}

func TestListEmpty(t *testing.T) {
	s := openTestStore(t)
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0, got %d", len(list))
	}
}
