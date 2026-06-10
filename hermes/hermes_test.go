package hermes

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

	store, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	projects, err := OpenProjectStoreWithDB(ctx, store.DB())
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}

	notifications, err := OpenNotifyStoreWithDB(ctx, store.DB())
	if err != nil {
		t.Fatalf("open notify store: %v", err)
	}

	sessions, err := OpenSessionStoreWithDB(ctx, store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHTTPHandler(logger, store, projects, notifications, sessions)
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}

	client := NewHTTPForgeClient("http://localhost:0")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}
