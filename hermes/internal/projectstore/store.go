// Package projectstore persists the project registry in SQLite.
// KITT queries this to know where to address envelopes; the delivery
// worker uses it to enrich payloads with working_dir for Forge.
package projectstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migration/*.sql
var migrations embed.FS

var (
	ErrNotFound  = errors.New("projectstore: not found")
	ErrDuplicate = errors.New("projectstore: duplicate project")
)

// Project is a registered project entry.
type Project struct {
	Project        string `json:"project"`
	Domain         string `json:"domain"`
	TargetNode     string `json:"target_node"`
	TargetExecutor string `json:"target_executor"`
	WorkingDir     string `json:"working_dir"`
	CreatedAt      string `json:"created_at"`
	IconPath       string `json:"icon_path"`
}

// Store is a thin SQLite-backed project registry.
type Store struct {
	db     *sql.DB
	ownsDB bool // true if Store opened the connection (and should close it)
}

// Open opens (or creates) the project registry in the given DSN.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("projectstore: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("projectstore: ping: %w", err)
	}
	s := &Store{db: db, ownsDB: true}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenWithDB creates a Store sharing an existing *sql.DB connection.
// The caller retains ownership of db — Close is a no-op.
func OpenWithDB(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db, ownsDB: false}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.ownsDB {
		return s.db.Close()
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	// Create the migrations tracking table if it doesn't exist.
	// Using IF NOT EXISTS makes this idempotent.
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("projectstore: create migrations table: %w", err)
	}

	migrationFiles := []string{
		"migration/001_projects.sql",
		"migration/002_icon_path.sql",
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
			return fmt.Errorf("projectstore: check migration %q: %w", file, err)
		}

		ddl, err := migrations.ReadFile(file)
		if err != nil {
			return fmt.Errorf("projectstore: read migration %s: %w", file, err)
		}
		if _, err := s.db.ExecContext(ctx, string(ddl)); err != nil {
			// SQLite does not support "ADD COLUMN IF NOT EXISTS".
			// If a column already exists (e.g. DB was created before
			// schema_migrations existed), SQLite returns
			// "duplicate column name: <col>".  Treat that as a no-op so
			// that upgrading a pre-existing DB does not crash.
			if strings.Contains(err.Error(), "duplicate column name") {
				// Column already present — migration is effectively applied.
			} else {
				return fmt.Errorf("projectstore: migrate %s: %w", file, err)
			}
		}

		// Record the migration as applied.
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO schema_migrations (name) VALUES (?)`, file); err != nil {
			return fmt.Errorf("projectstore: record migration %q: %w", file, err)
		}
	}
	return nil
}

const selectColumns = `project, domain, target_node, target_executor, working_dir, created_at, coalesce(icon_path, '') as icon_path`

func scanFromRows(rows *sql.Rows) (*Project, error) {
	var p Project
	if err := rows.Scan(&p.Project, &p.Domain, &p.TargetNode, &p.TargetExecutor, &p.WorkingDir, &p.CreatedAt, &p.IconPath); err != nil {
		return nil, err
	}
	return &p, nil
}

// Insert adds a project to the registry. Empty TargetNode/TargetExecutor
// fall back to DB column defaults ("mac-forge" / "claude").
func (s *Store) Insert(ctx context.Context, p *Project) error {
	node := p.TargetNode
	if node == "" {
		node = "mac-forge"
	}
	executor := p.TargetExecutor
	if executor == "" {
		executor = "claude"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (project, domain, target_node, target_executor, working_dir) VALUES (?, ?, ?, ?, ?)`,
		p.Project, p.Domain, node, executor, p.WorkingDir,
	)
	if err != nil && isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrDuplicate, p.Project)
	}
	return err
}

// List returns all registered projects. Returns an empty slice (not nil)
// when no projects exist.
func (s *Store) List(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectColumns+` FROM projects ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("projectstore: list: %w", err)
	}
	defer rows.Close()

	result := []*Project{}
	for rows.Next() {
		p, err := scanFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("projectstore: scan: %w", err)
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projectstore: list: %w", err)
	}
	return result, nil
}

// Get returns a single project by name.
func (s *Store) Get(ctx context.Context, project string) (*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectColumns+` FROM projects WHERE project = ? LIMIT 1`, project)
	if err != nil {
		return nil, fmt.Errorf("projectstore: get: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scanFromRows(rows)
}

func isUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "PRIMARY KEY")
}

// SetIconPath updates the icon_path for a project. Pass empty string to clear.
func (s *Store) SetIconPath(ctx context.Context, project, iconPath string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE projects SET icon_path = ? WHERE project = ?`,
		iconPath, project,
	)
	if err != nil {
		return fmt.Errorf("projectstore: set icon_path: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
