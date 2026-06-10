package forge

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestFacade_RoundTrip(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db")

	store, err := OpenSessionStore(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// With nil launcher (uses stub).
	handler := NewHTTPHandler(logger, store, nil, "", "")
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}
