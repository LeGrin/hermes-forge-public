// Package e2e holds the Hermes walking-skeleton smoke test.
//
// This test boots Hermes (store + HTTP + delivery worker) and Forge
// (HTTP acceptor) in-process via their public facades and drives one
// envelope through the full path: KITT-style POST → Hermes store →
// delivery worker → Forge /deliver → ack → delivered.
package e2e_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/legrin-tech/forge"
	"github.com/legrin-tech/hermes"
	"github.com/legrin-tech/hermes/envelope"
)

// workerTimeout is the max time we wait for the worker to stop.
// It must exceed the worker's HTTP client timeout (5s) so that in-flight
// requests are not killed by a racing teardown.
const workerTimeout = 6 * time.Second

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testEnv holds shared e2e test infrastructure.
type testEnv struct {
	logger      *slog.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	forgeStore  *forge.SessionStore
	forgeSrv    *httptest.Server
	forgeURL    string
	hermesStore *hermes.Store
	hermesSrv   *httptest.Server
	worker      *hermes.Worker
	workerDone  chan struct{}
}

// cleanup stops the worker and fails the test if it doesn't stop in time.
func (e *testEnv) cleanup(t *testing.T) {
	e.cancel()
	select {
	case <-e.workerDone:
		// worker stopped cleanly
	case <-time.After(workerTimeout):
		t.Fatalf("worker did not stop within %v", workerTimeout)
	}
}

// teardown closes all server and store resources.
func (e *testEnv) teardown(t *testing.T) {
	e.forgeSrv.Close()
	e.hermesSrv.Close()
	if err := e.forgeStore.Close(); err != nil {
		t.Errorf("forge store close failed: %v", err)
	}
	if err := e.hermesStore.Close(); err != nil {
		t.Errorf("hermes store close failed: %v", err)
	}
}

func (e *testEnv) waitForStatus(ctx context.Context, t *testing.T, id string, target envelope.Status) *envelope.Envelope {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.hermesStore.Get(ctx, id)
		if err != nil {
			t.Fatalf("store.Get: %v", err)
		}
		if got.Status == target {
			return got
		}
		time.Sleep(15 * time.Millisecond)
	}
	got, err := e.hermesStore.Get(ctx, id)
	if err != nil {
		t.Fatalf("envelope never reached %s; Get failed: %v", target, err)
	}
	t.Fatalf("envelope never reached %s; last status=%q", target, got.Status)
	return nil
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	logger := discardLogger()
	ctx, cancel := context.WithCancel(context.Background())

	forgeStore, forgeSrv, forgeURL := setupForge(ctx, t, logger)
	hermesStore, hermesSrv := setupHermes(ctx, t, logger)
	worker, workerDone := startWorker(hermesStore, forgeURL, logger, ctx)

	env := newTestEnv(logger, ctx, cancel, forgeStore, forgeSrv, forgeURL, hermesStore, hermesSrv, worker, workerDone)
	// Register teardown before cleanup so worker stops before server/store teardown (LIFO).
	t.Cleanup(func() { env.teardown(t) })
	t.Cleanup(func() { env.cleanup(t) })
	return env
}

func newTestEnv(logger *slog.Logger, ctx context.Context, cancel context.CancelFunc, forgeStore *forge.SessionStore, forgeSrv *httptest.Server, forgeURL string, hermesStore *hermes.Store, hermesSrv *httptest.Server, worker *hermes.Worker, workerDone chan struct{}) *testEnv {
	return &testEnv{
		logger:      logger,
		ctx:         ctx,
		cancel:      cancel,
		forgeStore:  forgeStore,
		forgeSrv:    forgeSrv,
		forgeURL:    forgeURL,
		hermesStore: hermesStore,
		hermesSrv:   hermesSrv,
		worker:      worker,
		workerDone:  workerDone,
	}
}

func setupForge(ctx context.Context, t *testing.T, logger *slog.Logger) (*forge.SessionStore, *httptest.Server, string) {
	t.Helper()
	forgeStore, err := forge.OpenSessionStore(ctx, filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatalf("open forge store: %v", err)
	}
	forgeSrv := httptest.NewServer(forge.NewHTTPHandler(logger, forgeStore, nil, "", ""))
	return forgeStore, forgeSrv, forgeSrv.URL
}

func setupHermes(ctx context.Context, t *testing.T, logger *slog.Logger) (*hermes.Store, *httptest.Server) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "hermes.db")
	store, err := hermes.OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	db := store.DB()
	projects, err := hermes.OpenProjectStoreWithDB(ctx, db)
	if err != nil {
		t.Fatalf("open project store: %v", err)
	}
	notifications, err := hermes.OpenNotifyStoreWithDB(ctx, db)
	if err != nil {
		t.Fatalf("open notify store: %v", err)
	}
	sessions, err := hermes.OpenSessionStoreWithDB(ctx, db)
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	hermesSrv := httptest.NewServer(hermes.NewHTTPHandler(logger, store, projects, notifications, sessions))
	return store, hermesSrv
}

func startWorker(store *hermes.Store, forgeURL string, logger *slog.Logger, ctx context.Context) (*hermes.Worker, chan struct{}) {
	worker := &hermes.Worker{
		Store:  store,
		Client: hermes.NewHTTPForgeClient(forgeURL),
		Tick:   20 * time.Millisecond,
		Logger: logger,
	}
	workerDone := make(chan struct{})
	go func() {
		_ = worker.Run(ctx)
		close(workerDone)
	}()
	return worker, workerDone
}

func postEnvelope(t *testing.T, url string, payload string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/envelopes", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("POST /envelopes: %v", err)
	}
	return resp
}

func assertStatusCreated(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, string(body))
	}
}
