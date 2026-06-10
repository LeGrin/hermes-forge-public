// Package worker ships envelopes from Hermes to Forge.
//
// The worker is a single-goroutine loop: on each tick it asks the store
// for the oldest status=created envelope, POSTs it to Forge's /deliver
// endpoint, and — only after a successful ack — flips the row to
// delivered and writes back the session_binding Forge returned.
//
// Worldview invariants enforced here:
//
//   - W-H4  (deliver reliably): Forge errors leave the row at 'created'
//     so the next tick retries. No data is dropped on transient failure.
//   - W-H5  (no optimistic confirmation): MarkDelivered is only called
//     after the Forge ack has been parsed successfully.
//   - W-H14 (never lose messages): a crash or context-cancel between the
//     HTTP POST and MarkDelivered leaves status='created' untouched, so
//     the row is reclaimable by the next worker run.
//   - W-H16 (traceable delivery identity): the worker derives a stable
//     delivery_id per envelope so Forge can dedupe retries.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/legrin-tech/hermes/envelope"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/projectstore"
)

// Store is the subset of *envelopestore.Store the worker needs. Declared
// as an interface so tests can stub it without standing up SQLite.
type Store interface {
	NextCreated(ctx context.Context) (*envelope.Envelope, error)
	MarkDelivered(ctx context.Context, id, sessionBinding string, deliveredAt time.Time) error
}

// ForgeClient delivers one envelope to Forge and returns the session
// handle Forge minted (or reused) for it.
type ForgeClient interface {
	Deliver(ctx context.Context, deliveryID string, e *envelope.Envelope, workingDir string) (sessionID string, err error)
}

// ProjectLookup resolves project name → registry entry. Optional.
type ProjectLookup interface {
	Get(ctx context.Context, project string) (*projectstore.Project, error)
}

// Worker polls the store on Tick and delivers one envelope per tick.
// Single-worker v0: no concurrent claim semantics.
type Worker struct {
	Store    Store
	Client   ForgeClient
	Projects ProjectLookup // optional — if nil, working_dir is empty
	Tick     time.Duration
	Logger   *slog.Logger
}

// Run blocks until ctx is cancelled, draining one envelope per tick.
// It returns nil on ctx cancellation (graceful shutdown) — transient
// per-tick errors are logged, not returned.
//
// Backoff: on consecutive errors the worker sleeps exponentially
// (1s, 2s, 4s, … up to 30s) to avoid hammering a down Forge.
func (w *Worker) Run(ctx context.Context) error {
	if w.Tick <= 0 {
		w.Tick = 500 * time.Millisecond
	}
	t := time.NewTicker(w.Tick)
	defer t.Stop()

	var consecutiveErrors int

	for {
		consecutiveErrors = w.drainOnce(ctx, consecutiveErrors)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

const maxBackoff = 30 * time.Second

// drainOnce delivers envelopes greedily until the queue is empty or an
// error occurs. Returns updated consecutiveErrors count.
func (w *Worker) drainOnce(ctx context.Context, consecutiveErrors int) int {
	for {
		done, err := w.TickOnce(ctx)
		if err != nil {
			consecutiveErrors++
			backoff := errorBackoff(consecutiveErrors)
			w.Logger.Error("worker tick failed", "err", err, "backoff_s", backoff.Seconds(), "consecutive", consecutiveErrors)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			return consecutiveErrors
		}
		consecutiveErrors = 0
		if done || ctx.Err() != nil {
			return 0
		}
	}
}

// errorBackoff returns exponential backoff: 1s, 2s, 4s, … capped at maxBackoff.
func errorBackoff(consecutive int) time.Duration {
	shift := min(consecutive-1, 5)
	d := time.Duration(1<<shift) * time.Second
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// TickOnce attempts to deliver exactly one envelope. It returns done=true
// if there was nothing to do (or the attempt hit an error the caller
// should back off on), done=false if work was dispatched successfully and
// the caller should immediately try again.
func (w *Worker) TickOnce(ctx context.Context) (done bool, err error) {
	e, err := w.Store.NextCreated(ctx)
	if errors.Is(err, envelopestore.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("next created: %w", err)
	}

	// W-H16: stable delivery_id per envelope.
	deliveryID := "d-" + e.ID

	// Resolve working_dir from project registry (best-effort).
	var workingDir string
	if w.Projects != nil && e.Project != "" {
		if proj, err := w.Projects.Get(ctx, e.Project); err == nil {
			workingDir = proj.WorkingDir
		} else {
			w.Logger.Warn("project lookup failed", "project", e.Project, "err", err)
		}
	}

	sessionID, err := w.Client.Deliver(ctx, deliveryID, e, workingDir)
	if err != nil {
		// W-H4 / W-H14: leave the row as 'created'. It will be retried.
		return true, fmt.Errorf("forge deliver %s: %w", e.ID, err)
	}

	// W-H5: flip to delivered ONLY after the ack was parsed. If ctx was
	// cancelled between the POST and here, the flip is skipped and the
	// row remains reclaimable on next boot.
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err := w.Store.MarkDelivered(ctx, e.ID, sessionID, time.Now().UTC()); err != nil {
		return true, fmt.Errorf("mark delivered %s: %w", e.ID, err)
	}
	w.Logger.Info("envelope delivered", "id", e.ID, "session_id", sessionID)
	return false, nil
}

// --- HTTP Forge client ---

// HTTPForgeClient is a thin client for Forge's POST /deliver.
type HTTPForgeClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewHTTPForgeClient constructs a client with a sane default timeout.
func NewHTTPForgeClient(baseURL string) *HTTPForgeClient {
	return &HTTPForgeClient{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

type deliverRequest struct {
	DeliveryID string             `json:"delivery_id"`
	Envelope   *envelope.Envelope `json:"envelope"`
	WorkingDir string             `json:"working_dir,omitempty"`
}

type deliverResponse struct {
	DeliveryID string `json:"delivery_id"`
	EnvelopeID string `json:"envelope_id"`
	SessionID  string `json:"session_id"`
	AckedAt    string `json:"acked_at"`
}

// Deliver POSTs the envelope to Forge and returns Forge's session_id.
// Non-2xx responses are returned as errors so the worker retries.
func (c *HTTPForgeClient) Deliver(ctx context.Context, deliveryID string, e *envelope.Envelope, workingDir string) (string, error) {
	body, err := json.Marshal(deliverRequest{DeliveryID: deliveryID, Envelope: e, WorkingDir: workingDir})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/deliver", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("forge returned %d: %s", resp.StatusCode, string(raw))
	}
	var ack deliverResponse
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return "", fmt.Errorf("decode ack: %w", err)
	}
	if ack.SessionID == "" {
		return "", errors.New("forge ack missing session_id")
	}
	return ack.SessionID, nil
}
