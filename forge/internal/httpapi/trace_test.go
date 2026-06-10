package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/legrin-tech/forge/internal/runner"
	"github.com/legrin-tech/forge/internal/sessionstore"
)

func TestTraceSpawnGate(t *testing.T) {
	var spawnCount int
	var spawnMu sync.Mutex

	openCodeMux := http.NewServeMux()
	openCodeMux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"id": "oc-shared-session", "title": "env-trace"}})
	})
	openCodeMux.HandleFunc("POST /session/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	openCodeServer := httptest.NewServer(openCodeMux)
	defer openCodeServer.Close()

	dsn := filepath.Join(t.TempDir(), "trace_spawn.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	registry := newProcessRegistry(discardLogger())
	launcher := Launcher(func(executor, envelopeID, workingDir, _ string, _ bool) (*runner.Process, error) {
		spawnMu.Lock()
		spawnCount++
		spawnMu.Unlock()
		p := runner.New("cat")
		if err := p.Start(); err != nil {
			return nil, err
		}
		return p, nil
	})

	now := time.Now()
	sess := &sessionstore.Session{
		SessionID:  "session-env-trace-pid-9999",
		EnvelopeID: "env-trace",
		Executor:   "opencode",
		State:      "lost",
		StartedAt:  now,
		LastSeenAt: now,
	}
	if err := store.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	d := newDeliverHandlerWithHermes(discardLogger(), store, launcher, registry, nil, openCodeServer.URL)
	rh := newResumeHandler(discardLogger(), store, registry, launcher, openCodeServer.URL)

	testMux := http.NewServeMux()
	d.register(testMux)
	rh.register(testMux)
	httpSrv := httptest.NewServer(testMux)
	defer httpSrv.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		body := validDelivery("d-trace-1", "env-trace")
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/deliver", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()

	go func() {
		defer wg.Done()
		body := `{"message":"resume now"}`
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/sessions/env-trace/resume", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()

	wg.Wait()

	spawnMu.Lock()
	count := spawnCount
	spawnMu.Unlock()

	if count > 1 {
		t.Fatalf("expected at most 1 spawn, got %d", count)
	}
}
