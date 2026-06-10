package projectstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openTestStoreWithDB(t *testing.T) *Store {
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
		t.Fatalf("open with db: %v", err)
	}
	return s
}

func TestOpenWithDB_InsertAndGet(t *testing.T) {
	s := openTestStoreWithDB(t)
	ctx := context.Background()
	if err := s.Insert(ctx, &Project{Project: "test-p", Domain: "d", WorkingDir: "/tmp"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	p, err := s.Get(ctx, "test-p")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.Project != "test-p" {
		t.Errorf("project = %q", p.Project)
	}
}

func TestClose_OwnsDB(t *testing.T) {
	s := openTestStore(t)
	// Close should not panic and should close the db.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestClose_SharedDB(t *testing.T) {
	s := openTestStoreWithDB(t)
	// Close on shared db should be a no-op.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestOpen_InvalidDSN(t *testing.T) {
	// A DSN pointing to a directory should fail at open/ping.
	_, err := Open(context.Background(), "/dev/null/impossible.db")
	if err == nil {
		t.Fatal("expected error for invalid DSN")
	}
}

func TestInsertAndList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.Insert(ctx, &Project{
		Project:        "hermes",
		Domain:         "ops",
		TargetNode:     "mac-forge",
		TargetExecutor: "claude",
		WorkingDir:     "/tmp/test-hermes",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	projects, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1, got %d", len(projects))
	}
	if projects[0].Project != "hermes" {
		t.Errorf("project = %q, want hermes", projects[0].Project)
	}
	if projects[0].WorkingDir != "/tmp/test-hermes" {
		t.Errorf("working_dir = %q", projects[0].WorkingDir)
	}
}

func TestInsertDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	p := &Project{Project: "x", Domain: "d", WorkingDir: "/tmp"}
	if err := s.Insert(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, p); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	_ = s.Insert(ctx, &Project{Project: "hermes", Domain: "ops", WorkingDir: "/tmp/h"})
	p, err := s.Get(ctx, "hermes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.WorkingDir != "/tmp/h" {
		t.Errorf("working_dir = %q", p.WorkingDir)
	}
}

func TestListEmpty(t *testing.T) {
	s := openTestStore(t)
	projects, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("expected 0, got %d", len(projects))
	}
}

func TestSetIconPath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Insert a project first.
	if err := s.Insert(ctx, &Project{Project: "test-icon", Domain: "d", WorkingDir: "/tmp"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Set icon_path.
	if err := s.SetIconPath(ctx, "test-icon", "/icons/test-icon.png"); err != nil {
		t.Fatalf("set icon_path: %v", err)
	}

	// Verify icon_path is returned in List.
	projects, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}
	if projects[0].IconPath != "/icons/test-icon.png" {
		t.Errorf("icon_path = %q, want /icons/test-icon.png", projects[0].IconPath)
	}

	// Verify icon_path is returned in Get.
	p, err := s.Get(ctx, "test-icon")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.IconPath != "/icons/test-icon.png" {
		t.Errorf("icon_path = %q, want /icons/test-icon.png", p.IconPath)
	}
}

func TestSetIconPath_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.SetIconPath(ctx, "nonexistent", "/icons/nonexistent.png")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestIconPath_DefaultEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Insert a project WITHOUT setting icon_path.
	if err := s.Insert(ctx, &Project{Project: "no-icon-path", Domain: "d", WorkingDir: "/tmp"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	projects, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Newly inserted projects without icon_path should have empty string (coalesce null to '').
	if projects[0].IconPath != "" {
		t.Errorf("new project icon_path = %q, want empty string", projects[0].IconPath)
	}
}

func TestMigration_002_IconPath(t *testing.T) {
	// Verify that the 002_icon_path migration adds icon_path column.
	s := openTestStore(t)
	ctx := context.Background()

	// The icon_path column should exist and be queryable.
	// We verify this by inserting a project and setting its icon_path.
	if err := s.Insert(ctx, &Project{Project: "mig-test", Domain: "d", WorkingDir: "/tmp"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.SetIconPath(ctx, "mig-test", "/icons/mig-test.png"); err != nil {
		t.Fatalf("set icon_path after migration: %v", err)
	}

	p, err := s.Get(ctx, "mig-test")
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if p.IconPath != "/icons/mig-test.png" {
		t.Errorf("icon_path = %q, want /icons/mig-test.png", p.IconPath)
	}
}

// TestMigration_002_PreExistingColumn simulates the upgrade scenario where
// a DB already has the icon_path column but schema_migrations is empty.
// migrate() must not crash with "duplicate column name".
func TestMigration_002_PreExistingColumn(t *testing.T) {
	ctx := context.Background()
	dsn := t.TempDir() + "/pre-existing.db"

	// Bootstrap a raw DB that already has icon_path but no schema_migrations.
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	db.SetMaxOpenConns(1)

	// Manually create the projects table with icon_path already present.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE projects (
			project         TEXT PRIMARY KEY,
			domain          TEXT NOT NULL DEFAULT '',
			target_node     TEXT NOT NULL DEFAULT 'mac-forge',
			target_executor TEXT NOT NULL DEFAULT 'claude',
			working_dir     TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL DEFAULT (datetime('now')),
			icon_path       TEXT
		)`)
	if err != nil {
		t.Fatalf("create projects table: %v", err)
	}
	// Insert a row to confirm the table is usable.
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (project, domain, working_dir) VALUES ('legacy', 'ops', '/tmp/legacy')`)
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	_ = db.Close()

	// Now open via the Store — migrate() must succeed even though
	// icon_path already exists and schema_migrations is empty.
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open on pre-existing-column DB: %v", err)
	}
	defer s.Close()

	// The legacy row must still be readable.
	p, err := s.Get(ctx, "legacy")
	if err != nil {
		t.Fatalf("get legacy project: %v", err)
	}
	if p.WorkingDir != "/tmp/legacy" {
		t.Errorf("working_dir = %q, want /tmp/legacy", p.WorkingDir)
	}
}
