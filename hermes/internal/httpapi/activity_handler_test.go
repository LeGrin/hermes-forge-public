package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legrin-tech/hermes/internal/activityhub"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/keystore"
)

func newTestServerWithActivity(t *testing.T) (http.Handler, *activityhub.Hub, *keystore.Key) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ks, err := keystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	adminKey, err := ks.Create(context.Background(), "admin", "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	hub := activityhub.New()
	srv := NewServer(discardLogger(), store, nil, nil, nil, ServerOpts{Keys: ks, Activity: hub})
	return srv, hub, adminKey
}

func TestPostActivity_Accepted(t *testing.T) {
	srv, hub, key := newTestServerWithActivity(t)

	req := newReq(t, http.MethodPost, "/activity",
		`{"envelope_id":"env-1","kind":"tool_use","summary":"Edit foo.go"}`)
	req.Header.Set("X-Hermes-Key", key.Key)
	rec := doReq(t, srv, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	events := hub.Recent("", 10)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Summary != "Edit foo.go" {
		t.Fatalf("unexpected summary: %q", events[0].Summary)
	}
	if events[0].APIKey != key.Key {
		t.Fatalf("expected api_key %q, got %q", key.Key, events[0].APIKey)
	}
}

func TestPostActivity_MissingKind(t *testing.T) {
	srv, _, key := newTestServerWithActivity(t)

	req := newReq(t, http.MethodPost, "/activity", `{"summary":"no kind"}`)
	req.Header.Set("X-Hermes-Key", key.Key)
	rec := doReq(t, srv, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetActivity_AdminSeesAll(t *testing.T) {
	srv, hub, adminKey := newTestServerWithActivity(t)

	hub.Publish(activityhub.Event{APIKey: "dev-key-other", Kind: "tick", Summary: "other user"})
	hub.Publish(activityhub.Event{APIKey: adminKey.Key, Kind: "tick", Summary: "admin"})

	req := newReq(t, http.MethodGet, "/activity", "")
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Admin sees both events.
	if !strings.Contains(rec.Body.String(), "other user") {
		t.Fatalf("expected admin to see all events: %s", rec.Body.String())
	}
}

func TestGetActivity_UserSeesOwnOnly(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ks, err := keystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	userKey, err := ks.Create(context.Background(), "userX", "user")
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}

	hub := activityhub.New()
	hub.Publish(activityhub.Event{APIKey: userKey.Key, Kind: "tick", Summary: "mine"})
	hub.Publish(activityhub.Event{APIKey: "dev-key-other", Kind: "tick", Summary: "not mine"})

	srv := NewServer(discardLogger(), store, nil, nil, nil, ServerOpts{Keys: ks, Activity: hub})

	req := newReq(t, http.MethodGet, "/activity", "")
	req.Header.Set("X-Hermes-Key", userKey.Key)
	rec := doReq(t, srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "mine") {
		t.Fatalf("expected own event: %s", body)
	}
	if strings.Contains(body, "not mine") {
		t.Fatalf("user should not see other's events: %s", body)
	}
}

func TestSSE_ReceivesLiveEvents(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ks, err := keystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	key, err := ks.Create(context.Background(), "sse-user", "user")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	hub := activityhub.New()
	handler := NewServer(discardLogger(), store, nil, nil, nil, ServerOpts{Keys: ks, Activity: hub})

	// Use a real httptest.Server so SSE streaming works.
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	req.Header.Set("X-Hermes-Key", key.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(buf[:n]))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	// Publish an event after SSE connection is established.
	hub.Publish(activityhub.Event{APIKey: key.Key, Kind: "test", Summary: "sse-event"})

	buf := make([]byte, 1024)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "sse-event") {
		t.Fatalf("expected sse-event in stream, got %q", got)
	}
}

func TestPostActivity_NoAuth(t *testing.T) {
	srv, _, _ := newTestServerWithActivity(t)

	req := newReq(t, http.MethodPost, "/activity", `{"kind":"tick"}`)
	rec := doReq(t, srv, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
