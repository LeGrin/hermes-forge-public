package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/legrin-tech/forge/internal/runner"
	"github.com/legrin-tech/forge/internal/sessionstore"
)

func TestNotify_Handler(t *testing.T) {
	// Create a mock OpenCode server that records injection calls
	var openCodeCalls []struct {
		sessionID string
		message   string
	}
	openCodeMux := http.NewServeMux()
	openCodeMux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"id": "oc-session-abc", "title": "env-hermes"}})
	})
	openCodeMux.HandleFunc("POST /session/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
			Role    string `json:"role"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		openCodeCalls = append(openCodeCalls, struct {
			sessionID string
			message   string
		}{sessionID: r.URL.Path, message: body.Content})
		w.WriteHeader(http.StatusOK)
	})
	openCodeServer := httptest.NewServer(openCodeMux)
	defer openCodeServer.Close()

	// Create a store for the notify handler
	dsn := filepath.Join(t.TempDir(), "test_notify_handler.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Create notify handler with the mock OpenCode URL
	registry := newProcessRegistry(discardLogger())
	nh := newNotifyHandler(discardLogger(), registry, store, openCodeServer.URL)

	// Test 1: POST /notify with valid body, session alive → 200
	t.Run("alive session returns 200", func(t *testing.T) {
		// Create a real cat process and register it so it appears alive
		proc := runner.New("cat")
		if err := proc.Start(); err != nil {
			t.Fatalf("failed to start cat process: %v", err)
		}
		internalSessionID := "session-env-alive-pid-1234"
		registry.Register(internalSessionID, proc)

		// Insert session into store so it can be looked up by envelope_id
		now := time.Now()
		sess := &sessionstore.Session{
			SessionID:  internalSessionID,
			EnvelopeID: "env-alive",
			Executor:   "opencode",
			State:      "live",
			StartedAt:  now,
			LastSeenAt: now,
		}
		if err := store.Insert(context.Background(), sess); err != nil {
			t.Fatalf("insert session: %v", err)
		}

		body := `{"envelope_id":"env-alive","executor_session_id":"oc-sess-xyz","message":"stop, update deps"}`
		req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		nh.notify(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]bool
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if !resp["ok"] {
			t.Fatalf("expected ok=true, got %v", resp)
		}
		// Verify OpenCode received the injection call
		if len(openCodeCalls) == 0 {
			t.Fatalf("expected OpenCode to be called")
		}
		if !strings.Contains(openCodeCalls[len(openCodeCalls)-1].message, "stop, update deps") {
			t.Fatalf("expected message to contain 'stop, update deps', got %q", openCodeCalls[len(openCodeCalls)-1].message)
		}

		proc.Stop()
	})

	// Reset calls for next test
	openCodeCalls = nil

	// Test 2: POST /notify with valid body, session not alive (not in registry) → 409
	t.Run("dead session returns 409", func(t *testing.T) {
		// Insert session into store but don't register in registry (process not alive)
		internalSessionID := "session-env-dead-pid-5678"
		now := time.Now()
		sess := &sessionstore.Session{
			SessionID:  internalSessionID,
			EnvelopeID: "env-dead",
			Executor:   "opencode",
			State:      "live",
			StartedAt:  now,
			LastSeenAt: now,
		}
		if err := store.Insert(context.Background(), sess); err != nil {
			t.Fatalf("insert session: %v", err)
		}

		body := `{"envelope_id":"env-dead","executor_session_id":"oc-sess-xyz","message":"stop"}`
		req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		nh.notify(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]string
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["error"] != "session_not_alive" {
			t.Fatalf("expected error=session_not_alive, got %v", resp)
		}
		if !strings.Contains(resp["detail"], "resume") {
			t.Fatalf("expected detail to mention 'resume', got %q", resp["detail"])
		}
	})

	// Test 3: POST /notify with unknown envelope_id → 409
	t.Run("unknown envelope returns 409", func(t *testing.T) {
		body := `{"envelope_id":"env-never","executor_session_id":"oc-sess-xyz","message":"stop"}`
		req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		nh.notify(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]string
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["error"] != "session_not_found" {
			t.Fatalf("expected error=session_not_found, got %v", resp)
		}
	})

	// Test 4: POST /notify with missing envelope_id → 422
	t.Run("missing envelope_id returns 422", func(t *testing.T) {
		body := `{"executor_session_id":"oc-sess-xyz","message":"stop"}`
		req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		nh.notify(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// Test 5: POST /notify with missing message → 422
	t.Run("missing message returns 422", func(t *testing.T) {
		body := `{"envelope_id":"env-alive","executor_session_id":"oc-sess-xyz"}`
		req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		nh.notify(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// Test 6: POST /notify with bad JSON → 400
	t.Run("bad JSON returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBufferString(`{not json`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		nh.notify(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestNotify_Integration(t *testing.T) {
	// Integration test: full flow with real test server
	dsn := filepath.Join(t.TempDir(), "test_integration.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Create a mock OpenCode server
	var openCodeCalls []struct {
		sessionID string
		message   string
	}
	openCodeMux := http.NewServeMux()
	openCodeMux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"id": "oc-session-abc"}})
	})
	openCodeMux.HandleFunc("POST /session/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
			Role    string `json:"role"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		openCodeCalls = append(openCodeCalls, struct {
			sessionID string
			message   string
		}{sessionID: r.URL.Path, message: body.Content})
		w.WriteHeader(http.StatusOK)
	})
	openCodeServer := httptest.NewServer(openCodeMux)
	defer openCodeServer.Close()

	// Create server with notify handler wired
	registry := newProcessRegistry(discardLogger())
	deliverHandler := newDeliverHandler(discardLogger(), store, StubLauncher(), registry)
	mux := http.NewServeMux()
	deliverHandler.register(mux)
	nh := newNotifyHandler(discardLogger(), registry, store, openCodeServer.URL)
	nh.register(mux)

	srv := mux

	// Deliver an envelope to create a session
	body := `{"delivery_id":"d-notify","envelope":{"id":"env-notify","task_title":"t","target_executor":"opencode"}}`
	req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver failed: %d: %s", rec.Code, rec.Body.String())
	}
	var ack deliverResponse
	json.Unmarshal(rec.Body.Bytes(), &ack)

	// Now notify the live session
	notifyBody := `{"envelope_id":"` + ack.EnvelopeID + `","executor_session_id":"` + ack.SessionID + `","message":"update deps"}`
	req = httptest.NewRequest(http.MethodPost, "/notify", bytes.NewBufferString(notifyBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("notify failed: %d: %s", rec.Code, rec.Body.String())
	}

	// Verify OpenCode was called
	if len(openCodeCalls) == 0 {
		t.Fatalf("expected OpenCode to be called")
	}
	if openCodeCalls[0].message != "update deps" {
		t.Fatalf("expected message 'update deps', got %q", openCodeCalls[0].message)
	}
}

// TestDeliver_SessionDiscovery tests that after delivery, the hermesClient
// is called to set the executor_session_id on Hermes.
func TestDeliver_SessionDiscovery(t *testing.T) {
	// Mock OpenCode server that returns a session ID
	openCodeMux := http.NewServeMux()
	openCodeMux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"id": "oc-session-abc", "title": "env-hermes"}})
	})
	openCodeServer := httptest.NewServer(openCodeMux)
	defer openCodeServer.Close()

	// This test requires a mock Hermes server to record the PATCH call
	var (
		hermesCalls []struct {
			method string
			path   string
			body   map[string]string
		}
		hermesMu sync.Mutex // protects hermesCalls
	)
	hermesMux := http.NewServeMux()
	hermesMux.HandleFunc("PATCH /envelopes/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		hermesMu.Lock()
		hermesCalls = append(hermesCalls, struct {
			method string
			path   string
			body   map[string]string
		}{method: r.Method, path: r.URL.Path, body: body})
		hermesMu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	hermesServer := httptest.NewServer(hermesMux)
	defer hermesServer.Close()

	// Create hermes client pointing to mock server
	hc := newHermesClient(hermesServer.URL, "test-key")

	// Create store and registry
	dsn := filepath.Join(t.TempDir(), "test_sess_discovery.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	registry := newProcessRegistry(discardLogger())

	// Create deliver handler with hermes client and mock OpenCode URL
	dh := newDeliverHandlerWithHermes(discardLogger(), store, StubLauncher(), registry, hc, openCodeServer.URL)

	mux := http.NewServeMux()
	dh.register(mux)

	// Deliver an envelope
	body := `{"delivery_id":"d-hermes","envelope":{"id":"env-hermes","task_title":"t","target_executor":"opencode"}}`
	req := httptest.NewRequest(http.MethodPost, "/deliver", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver failed: %d: %s", rec.Code, rec.Body.String())
	}

	// Wait for session discovery goroutine to fire (poll up to 2 seconds)
	var found bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hermesMu.Lock()
		hasCalls := len(hermesCalls) > 0
		hermesMu.Unlock()
		if hasCalls {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !found {
		t.Fatalf("expected hermesClient.SetExecutorSessionID to be called within 2 seconds")
	}

	hermesMu.Lock()
	defer hermesMu.Unlock()
	if hermesCalls[0].method != "PATCH" {
		t.Fatalf("expected PATCH, got %s", hermesCalls[0].method)
	}
	if !strings.Contains(hermesCalls[0].path, "/envelopes/env-hermes/session") {
		t.Fatalf("expected path to contain /envelopes/env-hermes/session, got %s", hermesCalls[0].path)
	}
	if hermesCalls[0].body["executor_session_id"] == "" {
		t.Fatalf("expected executor_session_id to be set")
	}
}
