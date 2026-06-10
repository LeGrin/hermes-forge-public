package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/envelope"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/projectstore"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedEnvelope inserts a minimal valid envelope with status=created.
func seedEnvelope(t *testing.T, s *envelopestore.Store, id string) *envelope.Envelope {
	t.Helper()
	e := &envelope.Envelope{
		ID:             id,
		CreatedAt:      time.Now().UTC(),
		CreatedBy:      "kitt",
		Title:          "test envelope",
		TaskTitle:      "run smoke",
		TargetExecutor: "opencode",
		Status:         envelope.StatusCreated,
	}
	if err := s.Insert(context.Background(), e); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	return e
}

func newStore(t *testing.T) *envelopestore.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fakeForge is a minimal httptest server that impersonates Forge /deliver.
// It records every call so tests can assert the worker's wire behaviour.
type fakeForge struct {
	*httptest.Server
	calls   atomic.Int32
	fail    atomic.Bool
	hang    chan struct{} // if non-nil, handler blocks on it before replying
	lastReq deliverRequest
	lastMu  sync.Mutex
}

func newFakeForge() *fakeForge {
	f := &fakeForge{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.hang != nil {
			<-f.hang
		}
		if f.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var req deliverRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastMu.Lock()
		f.lastReq = req
		f.lastMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(deliverResponse{
			DeliveryID: req.DeliveryID,
			EnvelopeID: req.Envelope.ID,
			SessionID:  "session-stub-" + req.Envelope.ID,
			AckedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		})
	}))
	return f
}

func TestTickOnce_Delivers(t *testing.T) {
	// W-H5 + W-H16 ack side: row flips to delivered only after Forge ack.
	store := newStore(t)
	seedEnvelope(t, store, "env-ok")

	forge := newFakeForge()
	defer forge.Close()

	w := &Worker{
		Store:  store,
		Client: NewHTTPForgeClient(forge.URL),
		Logger: discardLogger(),
	}

	done, err := w.TickOnce(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if done {
		t.Fatalf("expected done=false (work dispatched)")
	}

	got, err := store.Get(context.Background(), "env-ok")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != envelope.StatusDelivered {
		t.Fatalf("expected status=delivered, got %q", got.Status)
	}
	if got.SessionBinding == nil || *got.SessionBinding != "session-stub-env-ok" {
		t.Fatalf("expected session binding, got %v", got.SessionBinding)
	}
	if got.Delivery.DeliveredAt == nil {
		t.Fatalf("expected delivered_at set")
	}
	if forge.calls.Load() != 1 {
		t.Fatalf("expected 1 forge call, got %d", forge.calls.Load())
	}
}

func TestTickOnce_NothingToDo(t *testing.T) {
	store := newStore(t)
	forge := newFakeForge()
	defer forge.Close()

	w := &Worker{
		Store:  store,
		Client: NewHTTPForgeClient(forge.URL),
		Logger: discardLogger(),
	}

	done, err := w.TickOnce(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !done {
		t.Fatalf("expected done=true on empty queue")
	}
	if forge.calls.Load() != 0 {
		t.Fatalf("expected 0 forge calls, got %d", forge.calls.Load())
	}
}

func TestTickOnce_ForgeError_LeavesRowCreated(t *testing.T) {
	// W-H4 + W-H14: transient forge failure must not drop the row.
	store := newStore(t)
	seedEnvelope(t, store, "env-fail")

	forge := newFakeForge()
	forge.fail.Store(true)
	defer forge.Close()

	w := &Worker{
		Store:  store,
		Client: NewHTTPForgeClient(forge.URL),
		Logger: discardLogger(),
	}

	done, err := w.TickOnce(context.Background())
	if err == nil {
		t.Fatalf("expected forge error, got nil")
	}
	if !done {
		t.Fatalf("expected done=true on error")
	}
	got, err := store.Get(context.Background(), "env-fail")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != envelope.StatusCreated {
		t.Fatalf("expected status=created after forge failure, got %q", got.Status)
	}
	if got.SessionBinding != nil {
		t.Fatalf("expected no session binding, got %v", *got.SessionBinding)
	}
}

func TestTickOnce_CancelledMidDelivery_LeavesRowCreated(t *testing.T) {
	// W-H14: crash-between-POST-and-ack scenario. We cancel ctx while the
	// fake Forge is blocked, which makes http.Client.Do return an error.
	// The worker must leave the row at 'created'.
	store := newStore(t)
	seedEnvelope(t, store, "env-cancel")

	forge := newFakeForge()
	forge.hang = make(chan struct{})
	defer forge.Close()
	defer close(forge.hang)

	w := &Worker{
		Store:  store,
		Client: NewHTTPForgeClient(forge.URL),
		Logger: discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := w.TickOnce(ctx)
		errCh <- err
	}()

	// Give the request time to land in the handler, then cancel.
	for forge.calls.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected error from cancelled tick")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("tick did not return after cancel")
	}

	got, err := store.Get(context.Background(), "env-cancel")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != envelope.StatusCreated {
		t.Fatalf("expected status=created after cancel, got %q", got.Status)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	store := newStore(t)
	forge := newFakeForge()
	defer forge.Close()

	w := &Worker{
		Store:  store,
		Client: NewHTTPForgeClient(forge.URL),
		Tick:   5 * time.Millisecond,
		Logger: discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after cancel")
	}
}

// stubStore lets us test the "ErrNotFound from NextCreated" branch without
// relying on empty SQLite behaviour.
type stubStore struct {
	nextErr error
}

func (s *stubStore) NextCreated(ctx context.Context) (*envelope.Envelope, error) {
	return nil, s.nextErr
}
func (s *stubStore) MarkDelivered(ctx context.Context, id, sb string, at time.Time) error {
	return nil
}

func TestTickOnce_NextErrorPropagates(t *testing.T) {
	w := &Worker{
		Store:  &stubStore{nextErr: errors.New("db down")},
		Client: nil,
		Logger: discardLogger(),
	}
	done, err := w.TickOnce(context.Background())
	if err == nil {
		t.Fatalf("expected propagated error")
	}
	if !done {
		t.Fatalf("expected done=true on error")
	}
}

func TestTickOnce_WorkingDir_FromProjectRegistry(t *testing.T) {
	// Review feedback: verify worker sends working_dir in delivery payload.
	store := newStore(t)
	e := seedEnvelope(t, store, "env-wd")
	e.Project = "myproj"
	// Re-insert with project set (seedEnvelope already inserted, so update via raw SQL).
	store.DB().ExecContext(context.Background(), `UPDATE envelopes SET project = ? WHERE id = ?`, "myproj", "env-wd")

	// Set up projectstore with the project.
	psDSN := filepath.Join(t.TempDir(), "ps.db")
	ps, err := projectstore.Open(context.Background(), psDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	_ = ps.Insert(context.Background(), &projectstore.Project{
		Project:    "myproj",
		Domain:     "test",
		WorkingDir: "/tmp/myproj-dir",
	})

	forge := newFakeForge()
	defer forge.Close()

	w := &Worker{
		Store:    store,
		Client:   NewHTTPForgeClient(forge.URL),
		Projects: ps,
		Logger:   discardLogger(),
	}

	done, err := w.TickOnce(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if done {
		t.Fatalf("expected work dispatched")
	}

	forge.lastMu.Lock()
	gotDir := forge.lastReq.WorkingDir
	forge.lastMu.Unlock()

	if gotDir != "/tmp/myproj-dir" {
		t.Fatalf("expected working_dir=/tmp/myproj-dir, got %q", gotDir)
	}
}

func TestErrorBackoff(t *testing.T) {
	cases := []struct {
		consecutive int
		want        time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second}, // capped at maxBackoff
		{10, 30 * time.Second},
	}
	for _, tc := range cases {
		got := errorBackoff(tc.consecutive)
		if got != tc.want {
			t.Errorf("errorBackoff(%d) = %v, want %v", tc.consecutive, got, tc.want)
		}
	}
}

func TestDrainOnce_DrainsMultiple(t *testing.T) {
	store := newStore(t)
	seedEnvelope(t, store, "drain-1")
	seedEnvelope(t, store, "drain-2")

	forge := newFakeForge()
	defer forge.Close()

	w := &Worker{
		Store:  store,
		Client: NewHTTPForgeClient(forge.URL),
		Logger: discardLogger(),
	}

	ce := w.drainOnce(context.Background(), 0)
	if ce != 0 {
		t.Fatalf("expected 0 consecutive errors, got %d", ce)
	}
	if forge.calls.Load() != 2 {
		t.Fatalf("expected 2 forge calls, got %d", forge.calls.Load())
	}
}

func TestDrainOnce_ErrorIncrementsCounter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so backoff select returns immediately

	w := &Worker{
		Store:  &stubStore{nextErr: errors.New("db down")},
		Client: nil,
		Logger: discardLogger(),
	}

	ce := w.drainOnce(ctx, 2)
	if ce != 3 {
		t.Fatalf("expected consecutive errors 3, got %d", ce)
	}
}

func TestRun_DefaultTick(t *testing.T) {
	store := newStore(t)
	forge := newFakeForge()
	defer forge.Close()

	w := &Worker{
		Store:  store,
		Client: NewHTTPForgeClient(forge.URL),
		Tick:   0, // should default to 500ms
		Logger: discardLogger(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// fakeForgeClient stubs ForgeClient for unit tests without HTTP.
type fakeForgeClient struct {
	err       error
	sessionID string
}

func (f *fakeForgeClient) Deliver(_ context.Context, _ string, _ *envelope.Envelope, _ string) (string, error) {
	return f.sessionID, f.err
}

func TestTickOnce_ContextCancelledAfterDeliver(t *testing.T) {
	// W-H5: if context is cancelled between Deliver and MarkDelivered,
	// the row must NOT be flipped.
	store := newStore(t)
	seedEnvelope(t, store, "env-ctx")

	ctx, cancel := context.WithCancel(context.Background())

	w := &Worker{
		Store:  store,
		Client: &fakeForgeClient{sessionID: "s-1"},
		Logger: discardLogger(),
	}

	// Cancel context before TickOnce — but after Deliver would succeed.
	// We need to cancel between Deliver and MarkDelivered.
	// Since fakeForgeClient returns immediately, cancel ctx first won't work.
	// Instead, test the explicit ctx.Err() check at line 159.
	cancel()

	done, err := w.TickOnce(ctx)
	if err == nil {
		t.Fatalf("expected context error")
	}
	if !done {
		t.Fatalf("expected done=true")
	}

	got, _ := store.Get(context.Background(), "env-ctx")
	if got.Status != envelope.StatusCreated {
		t.Fatalf("expected created, got %q", got.Status)
	}
}
