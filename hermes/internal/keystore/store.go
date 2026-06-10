// Package keystore manages API keys for Hermes multitenancy.
//
// Each key identifies one user ecosystem. Keys are created by the admin
// and passed to Forge + AI assistants for authentication.
package keystore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

//go:embed migration/*.sql
var migrations embed.FS

// Key represents an API key entry.
type Key struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
}

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("keystore: not found")

// ErrDuplicate is returned when the label already exists.
var ErrDuplicate = errors.New("keystore: duplicate")

// Store is a thin SQLite-backed API key registry.
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
	ddl, err := migrations.ReadFile("migration/001_api_keys.sql")
	if err != nil {
		return fmt.Errorf("keystore: read migration: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(ddl)); err != nil {
		return fmt.Errorf("keystore: migrate: %w", err)
	}
	return nil
}

// generateKey returns a random key in the format hk_<24 hex chars>.
func generateKey() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("keystore: rand: %w", err)
	}
	return "hk_" + hex.EncodeToString(b), nil
}

// Create generates a new API key with the given label and role.
func (s *Store) Create(ctx context.Context, label, role string) (*Key, error) {
	if role == "" {
		role = "user"
	}
	key, err := generateKey()
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO api_keys (key, label, role) VALUES (?, ?, ?)`,
		key, label, role,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: label %q", ErrDuplicate, label)
		}
		return nil, fmt.Errorf("keystore: insert: %w", err)
	}
	return s.Get(ctx, key)
}

// Get returns a key by its value.
func (s *Store) Get(ctx context.Context, key string) (*Key, error) {
	var k Key
	err := s.db.QueryRowContext(ctx,
		`SELECT key, label, role, created_at FROM api_keys WHERE key = ?`, key,
	).Scan(&k.Key, &k.Label, &k.Role, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("keystore: get: %w", err)
	}
	return &k, nil
}

// List returns all API keys.
func (s *Store) List(ctx context.Context) ([]*Key, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, label, role, created_at FROM api_keys ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("keystore: list: %w", err)
	}
	defer rows.Close()
	result := []*Key{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.Key, &k.Label, &k.Role, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("keystore: scan: %w", err)
		}
		result = append(result, &k)
	}
	return result, rows.Err()
}

// isUniqueViolation checks if an error is a SQLite UNIQUE constraint violation.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Delete removes a key by its value.
func (s *Store) Delete(ctx context.Context, key string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("keystore: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
