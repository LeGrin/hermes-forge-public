package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/legrin-tech/forge/internal/sessionstore"
)

// newExecUUID returns an RFC-4122 v4 UUID string. Used to pre-pin a
// native executor session id (currently Claude --session-id) so Forge
// can push it back to Hermes synchronously with the spawn and resume
// the same conversation on later deliveries.
func newExecUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is catastrophic; fall back to a
		// deterministic-but-unique string so the spawn still succeeds.
		return fmt.Sprintf("00000000-0000-0000-0000-%012x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ErrUnknownExecutor is returned by a Launcher when the envelope targets
// an executor that this Forge cannot run locally (e.g. the VPS-side
// "kitt" agent). The deliver handler converts it into 422 Unprocessable
// Entity so Hermes marks the delivery as permanently unrecoverable and
// stops retrying — otherwise stuck envelopes burn CPU forever.
var ErrUnknownExecutor = errors.New("forge: unknown executor")

type deliverRequest struct {
	DeliveryID string         `json:"delivery_id"`
	Envelope   map[string]any `json:"envelope"`
	WorkingDir string         `json:"working_dir,omitempty"`
}

type deliverResponse struct {
	DeliveryID string `json:"delivery_id"`
	EnvelopeID string `json:"envelope_id"`
	SessionID  string `json:"session_id"`
	AckedAt    string `json:"acked_at"`
}

// deliverHandler accepts deliveries from Hermes, spawns executor
// processes via the Launcher, and persists sessions in the session store.
//
// Worldview:
//   - W-F6: ack only after a real process handle exists (not a stub).
//   - W-F1 + W-F2: session persisted in SQLite before ack is returned.
//   - W-H16: same delivery_id → 200 no-op with prior ack body.
const maxCachedDeliveries = 1000

type deliverHandler struct {
	logger   *slog.Logger
	store    *sessionstore.Store
	launcher Launcher
	registry *ProcessRegistry
	hermes   *hermesClient // may be nil

	openCodeURL string // URL of OpenCode server for session discovery

	mu         sync.Mutex
	deliveries map[string]deliverResponse // delivery_id → prior ack (cache)
}

func newDeliverHandler(logger *slog.Logger, store *sessionstore.Store, launcher Launcher, registry *ProcessRegistry) *deliverHandler {
	return newDeliverHandlerWithHermes(logger, store, launcher, registry, nil, "")
}

func newDeliverHandlerWithHermes(logger *slog.Logger, store *sessionstore.Store, launcher Launcher, registry *ProcessRegistry, hermes *hermesClient, openCodeURL string) *deliverHandler {
	if launcher == nil {
		launcher = StubLauncher()
	}
	if openCodeURL == "" {
		openCodeURL = defaultOpenCodeURL
	}
	return &deliverHandler{
		logger:      logger,
		store:       store,
		launcher:    launcher,
		registry:    registry,
		hermes:      hermes,
		openCodeURL: openCodeURL,
		deliveries:  make(map[string]deliverResponse),
	}
}

func (h *deliverHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /deliver", h.deliver)
}

// deliver accepts a delivery from Hermes, spawns (or reuses) an executor
// session, persists it in SQLite, and returns an idempotent ack.
func (h *deliverHandler) deliver(w http.ResponseWriter, r *http.Request) {
	var req deliverRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.DeliveryID == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_delivery", "delivery_id is required")
		return
	}
	envelopeID, _ := req.Envelope["id"].(string)
	if envelopeID == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_delivery", "envelope.id is required")
		return
	}
	executor, _ := req.Envelope["target_executor"].(string)
	if executor == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_delivery", "envelope.target_executor is required")
		return
	}

	// Validate working_dir if provided (review feedback: reject relative paths).
	if req.WorkingDir != "" && !filepath.IsAbs(req.WorkingDir) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_working_dir", "working_dir must be an absolute path")
		return
	}
	if req.WorkingDir != "" {
		req.WorkingDir = filepath.Clean(req.WorkingDir)
	}

	// W-H16: replay of same delivery_id returns the prior ack untouched.
	// Hold the lock through the check to prevent TOCTOU: two concurrent
	// requests with the same delivery_id must not both proceed past here.
	h.mu.Lock()
	if prior, ok := h.deliveries[req.DeliveryID]; ok {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, prior)
		return
	}
	// Release early — ensureSession acquires the spawn gate internally.
	h.mu.Unlock()

	// executor_session_id is the executor's own session handle (Claude's
	// --resume target or OpenCode's --session value). Empty means fresh.
	execSessionID, _ := req.Envelope["executor_session_id"].(string)

	// For Claude we pin the session id up-front so the first spawn uses
	// --session-id <uuid> and every later delivery for the same envelope
	// uses --resume <uuid>. The generated uuid is pushed back to Hermes
	// asynchronously after a successful spawn (see spawnAndRegisterSession).
	resume := execSessionID != ""
	if !resume && executor == "claude" {
		execSessionID = newExecUUID()
	}

	// W-F1 + W-F2 + W-F6: look up or create a session BEFORE acking.
	now := time.Now().UTC()
	sess, errKind, err := h.ensureSession(r, envelopeID, executor, req.WorkingDir, execSessionID, resume, now)
	if err != nil {
		status := http.StatusInternalServerError
		if errKind == "unknown_executor" {
			// Permanent error — Hermes should not retry.
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, errKind, err.Error())
		return
	}

	ack := deliverResponse{
		DeliveryID: req.DeliveryID,
		EnvelopeID: envelopeID,
		SessionID:  sess.SessionID,
		AckedAt:    now.Format(time.RFC3339Nano),
	}

	h.mu.Lock()
	if len(h.deliveries) >= maxCachedDeliveries {
		h.deliveries = make(map[string]deliverResponse)
	}
	h.deliveries[req.DeliveryID] = ack
	h.mu.Unlock()

	h.logger.Info("delivery acked",
		"delivery_id", req.DeliveryID,
		"envelope_id", envelopeID,
		"session_id", sess.SessionID,
	)
	writeJSON(w, http.StatusCreated, ack)
}

// ensureSession returns a session with a live process handle.
// Serialized under the per-envelope SpawnGate to prevent duplicate spawns
// from concurrent /deliver and /sessions/{id}/resume calls.
func (h *deliverHandler) ensureSession(
	r *http.Request,
	envelopeID, executor, workingDir, execSessionID string,
	resume bool,
	now time.Time,
) (*sessionstore.Session, string, error) {
	h.registry.SpawnGate.Lock(envelopeID)
	defer h.registry.SpawnGate.Unlock(envelopeID)

	sess, err := h.store.GetByEnvelope(r.Context(), envelopeID)
	if err != nil && !errors.Is(err, sessionstore.ErrNotFound) {
		h.logger.Error("session lookup failed", "err", err)
		return nil, "store_error", fmt.Errorf("internal error")
	}

	// Existing session: check if process is still alive (W-F6).
	if sess != nil && h.registry.Running(sess.SessionID) {
		return sess, "", nil
	}

	if sess != nil {
		h.logger.Info("respawning process for existing session",
			"envelope_id", envelopeID, "old_session_id", sess.SessionID)
	}

	return h.spawnAndRegisterSession(r, sess, envelopeID, executor, workingDir, execSessionID, resume, now)
}

func (h *deliverHandler) spawnAndRegisterSession(
	r *http.Request,
	existingSess *sessionstore.Session,
	envelopeID, executor, workingDir, execSessionID string,
	resume bool,
	now time.Time,
) (*sessionstore.Session, string, error) {
	proc, launchErr := h.launcher(executor, envelopeID, workingDir, execSessionID, resume)
	if launchErr != nil {
		h.logger.Error("launcher failed", "executor", executor, "err", launchErr)
		if errors.Is(launchErr, ErrUnknownExecutor) {
			return nil, "unknown_executor", launchErr
		}
		return nil, "launch_error", fmt.Errorf("failed to spawn executor session")
	}

	if proc.PID() <= 0 {
		h.logger.Error("launcher returned invalid PID", "executor", executor, "pid", proc.PID())
		_ = proc.Stop()
		return nil, "launch_error", fmt.Errorf("failed to spawn executor session")
	}

	sessionID := fmt.Sprintf("session-%s-pid-%d", envelopeID, proc.PID())
	newSess := &sessionstore.Session{
		SessionID:  sessionID,
		EnvelopeID: envelopeID,
		Executor:   executor,
		WorkingDir: workingDir,
		State:      "starting",
		StartedAt:  now,
		LastSeenAt: now,
	}
	// If a prior session row exists for this envelope, update it in-place so
	// GetByEnvelope always returns exactly one row and ordering is deterministic.
	// This prevents the concurrent-spawn race where resume re-fetches and gets
	// the old dead row (same started_at timestamp, non-deterministic ORDER BY).
	if existingSess != nil {
		if err := h.store.UpdateSessionID(r.Context(), envelopeID, sessionID); err != nil {
			h.logger.Error("session update failed", "err", err)
			_ = proc.Stop()
			return nil, "store_error", fmt.Errorf("internal error")
		}
	} else {
		if err := h.store.Insert(r.Context(), newSess); err != nil {
			h.logger.Error("session insert failed", "err", err)
			_ = proc.Stop()
			return nil, "store_error", fmt.Errorf("internal error")
		}
	}

	h.registry.Register(sessionID, proc)

	// If hermes client available and executor is OpenCode, discover the
	// OpenCode session by the explicit --title tag set to envelopeID.
	if h.hermes != nil && executor == "opencode" {
		go h.discoverAndStoreSessionID(envelopeID, workingDir, proc)
	}

	// For Claude we already pinned the UUID before spawning (via
	// --session-id <uuid>); push it back to Hermes on a fresh session
	// so the next delivery for this envelope carries it into --resume.
	if h.hermes != nil && executor == "claude" && !resume && execSessionID != "" {
		go func(envID, sid string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.hermes.SetExecutorSessionID(ctx, envID, sid); err != nil {
				h.logger.Error("claude session id push failed", "envelope_id", envID, "err", err)
			}
		}(envelopeID, execSessionID)
	}

	return newSess, "", nil
}

// discoverAndStoreSessionID polls OpenCode's /session endpoint for the
// session explicitly titled with envelopeID, then reports the executor
// session ID back to Hermes via the hermesClient.
func (h *deliverHandler) discoverAndStoreSessionID(envelopeID, workingDir string, proc opencodeOutputReader) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	type discoveryResult struct {
		source    string
		sessionID string
		err       error
	}
	results := make(chan discoveryResult, 2)
	go func() {
		sessionID, err := discoverOpenCodeSessionIDFromProcess(ctx, proc)
		results <- discoveryResult{source: "process", sessionID: sessionID, err: err}
	}()
	go func() {
		apiCtx, apiCancel := context.WithTimeout(ctx, 10*time.Second)
		defer apiCancel()
		sessionID, err := discoverOpenCodeSessionIDForEnvelope(apiCtx, h.openCodeURL, h.logger, envelopeID, workingDir)
		results <- discoveryResult{source: "api", sessionID: sessionID, err: err}
	}()

	discoveryErrs := make(map[string]error, 2)
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil && result.sessionID != "" {
			if err := h.hermes.SetExecutorSessionID(ctx, envelopeID, result.sessionID); err != nil {
				h.logger.Error("failed to set executor session id", "envelope_id", envelopeID, "err", err)
			}
			return
		}
		if result.err != nil {
			discoveryErrs[result.source] = result.err
		} else {
			discoveryErrs[result.source] = fmt.Errorf("discovery returned empty session id")
		}
	}
	err := aggregateDiscoveryErrors(discoveryErrs)
	if err != nil {
		h.logger.Warn("could not discover opencode session id", "envelope_id", envelopeID, "err", err)
		return
	}
}

func aggregateDiscoveryErrors(errs map[string]error) error {
	if len(errs) == 0 {
		return nil
	}
	processErr, hasProcessErr := errs["process"]
	apiErr, hasAPIErr := errs["api"]
	if hasProcessErr && hasAPIErr {
		return fmt.Errorf("process discovery failed: %v; API discovery failed: %v", processErr, apiErr)
	}
	if hasProcessErr {
		return fmt.Errorf("process discovery failed: %v", processErr)
	}
	if hasAPIErr {
		return fmt.Errorf("API discovery failed: %v", apiErr)
	}
	return fmt.Errorf("discovery failed: %v", errs)
}
