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

	"github.com/legrin-tech/forge/internal/runner"
	"github.com/legrin-tech/forge/internal/sessionstore"
)

// PART 1: Tests for resume handler with alive session (inject directly, no respawn)

// TestResume_AliveSession_InjectDirectly tests that POST /sessions/{id}/resume
// with an alive session injects the message directly without respawning.
func TestResume_AliveSession_InjectDirectly(t *testing.T) {
	var openCodeCalls []struct {
		sessionID string
		message   string
	}
	openCodeMux := http.NewServeMux()
	openCodeMux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"id": "oc-session-abc", "title": "env-alive"}})
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

	dsn := filepath.Join(t.TempDir(), "test_resume_alive.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	registry := newProcessRegistry(discardLogger())
	rh := newResumeHandler(discardLogger(), store, registry, StubLauncher(), openCodeServer.URL)

	// Register on mux and use test server so PathValue works
	mux := http.NewServeMux()
	rh.register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Create a running process and register it
	proc := runner.New("cat")
	if err := proc.Start(); err != nil {
		t.Fatalf("failed to start cat process: %v", err)
	}
	internalSessionID := "session-env-alive-pid-1234"
	registry.Register(internalSessionID, proc)

	// Insert session into store
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

	body := `{"message":"continue with the task"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sessions/env-alive/resume", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Fatalf("expected ok=true, got %v", resp)
	}
	if resp["session_id"] != "oc-session-abc" {
		t.Fatalf("expected session_id=oc-session-abc, got %v", resp["session_id"])
	}

	// Should have injected into OpenCode
	if len(openCodeCalls) == 0 {
		t.Fatal("expected OpenCode to be called for injection")
	}
}

// TestResume_UnknownEnvelopeID tests that POST /sessions/{id}/resume with unknown envelope returns 404.
func TestResume_UnknownEnvelopeID(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test_resume_unknown.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	registry := newProcessRegistry(discardLogger())
	rh := newResumeHandler(discardLogger(), store, registry, StubLauncher(), "http://localhost:4096")

	mux := http.NewServeMux()
	rh.register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := `{"message":"hello"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sessions/unknown-env/resume", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResume_MissingMessage tests that POST /sessions/{id}/resume without message returns 422.
func TestResume_MissingMessage(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test_resume_missing_msg.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	registry := newProcessRegistry(discardLogger())
	rh := newResumeHandler(discardLogger(), store, registry, StubLauncher(), "http://localhost:4096")

	mux := http.NewServeMux()
	rh.register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No body at all
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sessions/env-1/resume", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}

	// Empty message
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/sessions/env-1/resume", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for empty message, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResume_DeadSession_RespawnsAndInjects tests that POST /sessions/{id}/resume
// with a dead session respawns the process and injects the message.
func TestResume_DeadSession_RespawnsAndInjects(t *testing.T) {
	var openCodeCalls []struct {
		sessionID string
		message   string
	}
	var spawnedProcess *struct {
		executor   string
		envelopeID string
		workingDir string
	}
	openCodeMux := http.NewServeMux()
	openCodeMux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		// Return a new session on each call to simulate new process
		json.NewEncoder(w).Encode([]map[string]string{{"id": "oc-session-respawned", "title": "env-dead", "directory": "/tmp/test-workdir"}})
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

	dsn := filepath.Join(t.TempDir(), "test_resume_dead.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	registry := newProcessRegistry(discardLogger())

	// Create a custom launcher that tracks what was called
	customLauncher := func(executor, envelopeID, workingDir, _ string, _ bool) (*runner.Process, error) {
		spawnedProcess = &struct {
			executor   string
			envelopeID string
			workingDir string
		}{executor: executor, envelopeID: envelopeID, workingDir: workingDir}
		proc := runner.New("cat")
		if err := proc.Start(); err != nil {
			return nil, err
		}
		return proc, nil
	}

	rh := newResumeHandler(discardLogger(), store, registry, customLauncher, openCodeServer.URL)

	mux := http.NewServeMux()
	rh.register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Insert a DEAD session (no process running) with working_dir
	now := time.Now()
	sess := &sessionstore.Session{
		SessionID:  "session-env-dead-pid-9999", // Process is dead
		EnvelopeID: "env-dead",
		Executor:   "opencode",
		WorkingDir: "/tmp/test-workdir",
		State:      "lost",
		StartedAt:  now,
		LastSeenAt: now,
	}
	if err := store.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	body := `{"message":"resume and continue"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sessions/env-dead/resume", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Fatalf("expected ok=true, got %v", resp)
	}
	if resp["session_id"] != "oc-session-respawned" {
		t.Fatalf("expected session_id=oc-session-respawned, got %v", resp["session_id"])
	}

	// Verify the launcher was called with correct parameters
	if spawnedProcess == nil {
		t.Fatal("expected launcher to be called")
	}
	if spawnedProcess.executor != "opencode" {
		t.Fatalf("expected executor=opencode, got %q", spawnedProcess.executor)
	}
	if spawnedProcess.envelopeID != "env-dead" {
		t.Fatalf("expected envelopeID=env-dead, got %q", spawnedProcess.envelopeID)
	}
	if spawnedProcess.workingDir != "/tmp/test-workdir" {
		t.Fatalf("expected workingDir=/tmp/test-workdir, got %q", spawnedProcess.workingDir)
	}

	// Should have injected into OpenCode
	if len(openCodeCalls) == 0 {
		t.Fatal("expected OpenCode to be called for injection")
	}
	if !strings.Contains(openCodeCalls[len(openCodeCalls)-1].message, "resume and continue") {
		t.Fatalf("expected message to contain 'resume and continue', got %q", openCodeCalls[len(openCodeCalls)-1].message)
	}
}

type stubOpenCodeOutputReader struct {
	output  []byte
	done    chan struct{}
	maxTail int
}

func (s *stubOpenCodeOutputReader) ReadOutputTail(n int) []byte {
	if n > s.maxTail {
		s.maxTail = n
	}
	if n >= len(s.output) {
		return s.output
	}
	return s.output[len(s.output)-n:]
}

func (s *stubOpenCodeOutputReader) Done() <-chan struct{} { return s.done }

func TestDiscoverOpenCodeSessionIDFromProcess_UsesBoundedTail(t *testing.T) {
	proc := &stubOpenCodeOutputReader{
		output: []byte("not json\n{\"sessionID\":\"oc-bounded\"}\n"),
		done:   make(chan struct{}),
	}

	got, err := discoverOpenCodeSessionIDFromProcess(context.Background(), proc)
	if err != nil {
		t.Fatalf("discover session: %v", err)
	}
	if got != "oc-bounded" {
		t.Fatalf("session id = %q, want oc-bounded", got)
	}
	if proc.maxTail > openCodeSessionDiscoveryTailSize {
		t.Fatalf("ReadOutputTail requested %d bytes, want <= %d", proc.maxTail, openCodeSessionDiscoveryTailSize)
	}
}
