package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/internal/activityhub"
	"github.com/legrin-tech/hermes/internal/agentstore"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/keystore"
)

const agentTestHost = "mac-forge"

func newAgentTestServer(t *testing.T) (http.Handler, *agentstore.Store) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	es, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open envelopestore: %v", err)
	}
	t.Cleanup(func() { _ = es.Close() })
	agents := agentstore.New(time.Minute)
	return NewServer(discardLogger(), es, nil, nil, nil, ServerOpts{Agents: agents}), agents
}

// newAgentTestServerWithKeys creates a server with keystore for testing auth.
func newAgentTestServerWithKeys(t *testing.T) (http.Handler, *keystore.Store, *keystore.Key, *agentstore.Store) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	es, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open envelopestore: %v", err)
	}
	t.Cleanup(func() { _ = es.Close() })

	ks, err := keystore.OpenWithDB(context.Background(), es.DB())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}

	adminKey, err := ks.Create(context.Background(), "test-admin", "admin")
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}

	agents := agentstore.New(time.Minute)
	srv := NewServer(discardLogger(), es, nil, nil, nil, ServerOpts{Keys: ks, Agents: agents})
	return srv, ks, adminKey, agents
}

func postSnap(t *testing.T, srv http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/agents/snapshot", bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

func TestAgents_SnapshotThenList(t *testing.T) {
	srv, _ := newAgentTestServer(t)

	now := time.Now().UTC()
	body := `{
	  "host": "` + agentTestHost + `",
	  "taken_at": "` + now.Format(time.RFC3339) + `",
	  "agents": [
	    {"id":"mac-forge:claude:7808","host":"` + agentTestHost + `","executor":"claude","pid":7808,"state":"active","started_at":"` + now.Format(time.RFC3339) + `"},
	    {"id":"mac-forge:opencode:93022","host":"` + agentTestHost + `","executor":"opencode","pid":93022,"state":"idle","parent_kind":"init","started_at":"` + now.Format(time.RFC3339) + `"}
	  ]
	}`
	if rec := postSnap(t, srv, body); rec.Code != http.StatusAccepted {
		t.Fatalf("snapshot: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/agents", nil)
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(got))
	}
	// active first (stable sort contract)
	if got[0]["state"] != "active" {
		t.Errorf("expected active first, got %v", got[0]["state"])
	}
}

func TestAgents_GetDetail(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	postSnap(t, srv, `{"host":"`+agentTestHost+`","taken_at":"`+now+`","agents":[{"id":"agent-1","executor":"opencode","pid":123,"state":"active","started_at":"`+now+`"}]}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != "agent-1" || got["executor"] != "opencode" {
		t.Fatalf("unexpected agent detail: %s", rec.Body.String())
	}
}

func TestAgents_GetDetail_NotFound(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/missing", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgents_GetDetail_NoAuthReturns401(t *testing.T) {
	srv, _, _, store := newAgentTestServerWithKeys(t)
	now := time.Now().UTC()
	store.Apply(agentstore.Snapshot{Host: agentTestHost, TakenAt: now, Agents: []agentstore.Agent{{ID: "agent-1", Executor: "claude", State: "active", StartedAt: now}}})

	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgents_Snapshot_MissingHost(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	if rec := postSnap(t, srv, `{"agents":[]}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for missing host, got %d", rec.Code)
	}
}

func TestAgents_Snapshot_BadJSON(t *testing.T) {
	srv, _ := newAgentTestServer(t)
	if rec := postSnap(t, srv, `{not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad json, got %d", rec.Code)
	}
}

// TestAgents_Snapshot_RepeatReplaces asserts a second snapshot from
// the same host replaces agent records in-place — agents fall out via
// TTL, never accumulate.
// TestAgents_Snapshot_PublishesActivity asserts the handler emits an
// "agents_snapshot" event on the activityhub so the dashboard SSE
// stream can trigger a constellation refresh without polling.
func TestAgents_Snapshot_PublishesActivity(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	es, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open envelopestore: %v", err)
	}
	t.Cleanup(func() { _ = es.Close() })

	hub := activityhub.New()
	agents := agentstore.New(time.Minute)
	srv := NewServer(discardLogger(), es, nil, nil, nil, ServerOpts{Activity: hub, Agents: agents})

	sub := hub.Subscribe("") // admin view
	defer sub.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	postSnap(t, srv, `{"host":"`+agentTestHost+`","taken_at":"`+now+`","agents":[{"id":"mac-forge:claude:1","executor":"claude","state":"active","started_at":"`+now+`"}]}`)

	select {
	case evt := <-sub.Events():
		if evt.Kind != "agents_snapshot" {
			t.Errorf("kind: got %q, want agents_snapshot", evt.Kind)
		}
		if !strings.Contains(evt.Summary, agentTestHost) {
			t.Errorf("summary should include host: %q", evt.Summary)
		}
	case <-time.After(time.Second):
		t.Fatal("expected agents_snapshot activity event")
	}
}

func TestAgents_Snapshot_RepeatReplaces(t *testing.T) {
	srv, store := newAgentTestServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	postSnap(t, srv, `{"host":"`+agentTestHost+`","taken_at":"`+now+`","agents":[{"id":"mac-forge:claude:7808","executor":"claude","state":"active","started_at":"`+now+`","cpu_percent":12.3}]}`)
	postSnap(t, srv, `{"host":"`+agentTestHost+`","taken_at":"`+now+`","agents":[{"id":"mac-forge:claude:7808","executor":"claude","state":"idle","started_at":"`+now+`","cpu_percent":0.1}]}`)

	list := store.Recent()
	if len(list) != 1 {
		t.Fatalf("expected 1 agent (second snapshot should replace first), got %d", len(list))
	}
	if list[0].State != "idle" {
		t.Errorf("expected latest snapshot's state, got %q", list[0].State)
	}
	if list[0].CPUPercent != 0.1 {
		t.Errorf("expected latest cpu, got %v", list[0].CPUPercent)
	}
}

// CON-013: POST /agents/prune requires admin auth.
func TestAgents_Prune_NoAuth_Returns401(t *testing.T) {
	srv, _, _, _ := newAgentTestServerWithKeys(t)

	req := httptest.NewRequest(http.MethodPost, "/agents/prune", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated prune, got %d: %s", rec.Code, rec.Body.String())
	}
}

// CON-013: POST /agents/prune requires admin role, not just any valid key.
func TestAgents_Prune_UserRole_Returns403(t *testing.T) {
	srv, ks, _, _ := newAgentTestServerWithKeys(t)

	userKey, err := ks.Create(context.Background(), "test-user", "user")
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agents/prune", nil)
	req.Header.Set("X-Hermes-Key", userKey.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for user-role prune, got %d: %s", rec.Code, rec.Body.String())
	}
}

// CON-013: POST /agents/prune removes stale agents and returns counts.
func TestAgents_Prune_RemovesStaleAgents(t *testing.T) {
	srv, _, adminKey, store := newAgentTestServerWithKeys(t)
	now := time.Now().UTC()

	// Add a fresh agent - visible in Recent()
	store.Apply(agentstore.Snapshot{
		Host:    agentTestHost,
		TakenAt: now,
		Agents: []agentstore.Agent{
			{ID: "fresh-agent", Executor: "claude", State: "active", StartedAt: now},
		},
	})

	// Add a stale agent - NOT in Recent() (TTL is 1 min, agent is 2 min old),
	// but still in the store's internal map, waiting for Prune() to remove it.
	store.Apply(agentstore.Snapshot{
		Host:    agentTestHost,
		TakenAt: now.Add(-2 * time.Minute),
		Agents: []agentstore.Agent{
			{ID: "stale-agent", Executor: "opencode", State: "exited", StartedAt: now.Add(-time.Hour)},
		},
	})

	// Recent() filters by TTL, so only the fresh agent shows
	if n := len(store.Recent()); n != 1 {
		t.Fatalf("expected 1 fresh agent in Recent(), got %d", n)
	}

	req := httptest.NewRequest(http.MethodPost, "/agents/prune", nil)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var result map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["pruned"] != 1 {
		t.Errorf("expected pruned=1, got %d", result["pruned"])
	}
	if result["remaining"] != 1 {
		t.Errorf("expected remaining=1, got %d", result["remaining"])
	}
}

// CON-013: POST /agents/prune is idempotent.
func TestAgents_Prune_Idempotent(t *testing.T) {
	srv, _, adminKey, store := newAgentTestServerWithKeys(t)
	now := time.Now().UTC()

	// Add a stale agent (2 min old, TTL is 1 min)
	store.Apply(agentstore.Snapshot{
		Host:    agentTestHost,
		TakenAt: now.Add(-2 * time.Minute),
		Agents: []agentstore.Agent{
			{ID: "stale-agent", Executor: "opencode", State: "exited", StartedAt: now.Add(-time.Hour)},
		},
	})

	// First prune - should remove the stale agent
	req := httptest.NewRequest(http.MethodPost, "/agents/prune", nil)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first prune: expected 200, got %d", rec.Code)
	}

	var first map[string]int
	json.Unmarshal(rec.Body.Bytes(), &first)

	// Second prune should return 0 pruned (idempotent)
	req = httptest.NewRequest(http.MethodPost, "/agents/prune", nil)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second prune: expected 200, got %d", rec.Code)
	}

	var second map[string]int
	json.Unmarshal(rec.Body.Bytes(), &second)

	if second["pruned"] != 0 {
		t.Errorf("second prune: expected pruned=0, got %d", second["pruned"])
	}
	if first["remaining"] != second["remaining"] {
		t.Errorf("remaining count changed between calls: first=%d, second=%d", first["remaining"], second["remaining"])
	}
}
