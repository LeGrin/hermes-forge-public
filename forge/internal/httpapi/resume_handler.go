package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/legrin-tech/forge/internal/sessionstore"
)

const (
	msgEnvelopeRequired  = "envelope_id is required"
	msgMessageRequired   = "message is required"
	msgSessionNotFound   = "no session for this envelope"
	msgInvalidRequestFmt = "invalid_request"
	msgNotFoundFmt       = "not_found"
	msgInvalidJSON       = "invalid_json"
	msgStoreError        = "store_error"
)

const sessionDiscoveryFailedMsg = "session_discovery_failed"

const (
	openCodeSessionDiscoveryInterval = 500 * time.Millisecond
	openCodeSessionDiscoveryTailSize = 64 * 1024
)

// resumeHandler handles POST /sessions/{envelope_id}/resume — respawns a dead
// OpenCode session or re-injects into a live one.
type resumeHandler struct {
	logger      *slog.Logger
	store       *sessionstore.Store
	registry    *ProcessRegistry
	launcher    Launcher
	openCodeURL string
}

func newResumeHandler(logger *slog.Logger, store *sessionstore.Store, registry *ProcessRegistry, launcher Launcher, openCodeURL string) *resumeHandler {
	if openCodeURL == "" {
		openCodeURL = defaultOpenCodeURL
	}
	return &resumeHandler{
		logger:      logger,
		store:       store,
		registry:    registry,
		launcher:    launcher,
		openCodeURL: openCodeURL,
	}
}

func (h *resumeHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /sessions/{envelope_id}/resume", h.resume)
}

type resumeRequest struct {
	Message string `json:"message"`
}

type resumeResponse struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
}

// resume handles POST /sessions/{envelope_id}/resume.
func (h *resumeHandler) resume(w http.ResponseWriter, r *http.Request) {
	envelopeID := r.PathValue("envelope_id")
	if envelopeID == "" {
		writeError(w, http.StatusBadRequest, msgInvalidRequestFmt, msgEnvelopeRequired)
		return
	}

	if r.Body == nil || r.ContentLength == 0 {
		writeError(w, http.StatusUnprocessableEntity, msgInvalidRequestFmt, msgMessageRequired)
		return
	}

	var req resumeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if err.Error() == "EOF" || err.Error() == "unexpected end of JSON input" {
			writeError(w, http.StatusUnprocessableEntity, msgInvalidRequestFmt, msgMessageRequired)
			return
		}
		writeError(w, http.StatusBadRequest, msgInvalidJSON, err.Error())
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusUnprocessableEntity, msgInvalidRequestFmt, msgMessageRequired)
		return
	}

	sess, err := h.store.GetByEnvelope(r.Context(), envelopeID)
	if err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, msgNotFoundFmt, msgSessionNotFound)
		} else {
			writeError(w, http.StatusInternalServerError, msgStoreError, "failed to load session")
		}
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, msgNotFoundFmt, msgSessionNotFound)
		return
	}

	// Acquire per-envelope spawn gate before checking liveness.
	// This serializes with deliver.go:ensureSession so concurrent
	// /deliver + /resume for the same dead envelope cannot both spawn.
	h.registry.SpawnGate.Lock(envelopeID)
	defer h.registry.SpawnGate.Unlock(envelopeID)

	// Re-fetch session after acquiring the gate — a concurrent /deliver may
	// have already spawned and inserted a new session row since we loaded sess above.
	freshSess, err := h.store.GetByEnvelope(r.Context(), envelopeID)
	if err == nil && freshSess != nil {
		sess = freshSess
	}

	// If session is alive, inject directly
	if h.registry.Running(sess.SessionID) {
		h.injectAndRespond(w, r, sess, req.Message)
		return
	}

	// Session is dead — respawn (gate already held, no double-lock)
	h.respawnAndInjectLocked(w, r, sess, req.Message)
}

// injectAndRespond injects a message into OpenCode and returns 200.
func (h *resumeHandler) injectAndRespond(w http.ResponseWriter, r *http.Request, sess *sessionstore.Session, message string) {
	ocSessionID, err := h.discoverOpenCodeSessionID(r.Context(), sess.EnvelopeID, sess.WorkingDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, sessionDiscoveryFailedMsg, err.Error())
		return
	}

	if err := h.injectIntoOpenCode(r.Context(), ocSessionID, message); err != nil {
		writeError(w, http.StatusInternalServerError, "inject_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resumeResponse{OK: true, SessionID: ocSessionID})
}

// respawnAndInjectLocked spawns a new executor process, discovers the new OC session, and injects.
// Precondition: caller holds the per-envelope SpawnGate lock.
func (h *resumeHandler) respawnAndInjectLocked(w http.ResponseWriter, r *http.Request, sess *sessionstore.Session, message string) {
	ctx := r.Context()

	// Spawn new process using stored executor and working_dir
	proc, err := h.launcher(sess.Executor, sess.EnvelopeID, sess.WorkingDir, "", false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "launch_error", err.Error())
		return
	}

	if proc.PID() <= 0 {
		_ = proc.Stop()
		writeError(w, http.StatusInternalServerError, "launch_error", "launcher returned invalid PID")
		return
	}

	// Update session ID to reflect new process
	newSessionID := sessionIDFromPID(sess.EnvelopeID, proc.PID())
	sess.SessionID = newSessionID
	h.registry.Register(newSessionID, proc)

	// Persist new session ID so future resume calls use the live session
	if err := h.store.UpdateSessionID(ctx, sess.EnvelopeID, newSessionID); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "failed to persist new session")
		return
	}

	// Discover new OpenCode session
	ocSessionID, err := h.discoverOpenCodeSessionID(ctx, sess.EnvelopeID, sess.WorkingDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, sessionDiscoveryFailedMsg, err.Error())
		return
	}

	// Inject message
	if err := h.injectIntoOpenCode(ctx, ocSessionID, message); err != nil {
		writeError(w, http.StatusInternalServerError, "inject_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resumeResponse{OK: true, SessionID: ocSessionID})
}

// discoverOpenCodeSessionID polls OpenCode's /session endpoint until the
// session titled with envelopeID appears.
func (h *resumeHandler) discoverOpenCodeSessionID(ctx context.Context, envelopeID, workingDir string) (string, error) {
	return discoverOpenCodeSessionIDForEnvelope(ctx, h.openCodeURL, h.logger, envelopeID, workingDir)
}

// injectIntoOpenCode calls POST /session/{id}/message on the OpenCode server.
func (h *resumeHandler) injectIntoOpenCode(ctx context.Context, sessionID, message string) error {
	return injectIntoOpenCode(ctx, h.openCodeURL, sessionID, message)
}

// NOTE: The session store does NOT persist executor_session_id (the OpenCode
// session ID). After respawn, we re-discover it by its explicit session title.
// This is the same approach used in deliver.go discoverAndStoreSessionID.

func sessionIDFromPID(envelopeID string, pid int) string {
	return "session-" + envelopeID + "-pid-" + strconv.Itoa(pid)
}

type openCodeSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
}

type opencodeOutputReader interface {
	ReadOutputTail(int) []byte
	Done() <-chan struct{}
}

func discoverOpenCodeSessionIDFromProcess(ctx context.Context, proc opencodeOutputReader) (string, error) {
	if proc == nil {
		return "", fmt.Errorf("opencode process output unavailable")
	}

	ticker := time.NewTicker(openCodeSessionDiscoveryInterval)
	defer ticker.Stop()

	for {
		if sessionID, ok := discoverOpenCodeSessionIDFromOutput(proc.ReadOutputTail(openCodeSessionDiscoveryTailSize)); ok {
			return sessionID, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-proc.Done():
			if sessionID, ok := discoverOpenCodeSessionIDFromOutput(proc.ReadOutputTail(openCodeSessionDiscoveryTailSize)); ok {
				return sessionID, nil
			}
			return "", fmt.Errorf("opencode session id not found in process output")
		case <-ticker.C:
		}
	}
}

func discoverOpenCodeSessionIDFromOutput(output []byte) (string, bool) {
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(line, &event); err != nil || event.SessionID == "" {
			continue
		}
		return event.SessionID, true
	}
	return "", false
}

// Shared helper for discovering OpenCode session ID by explicit title — used
// by both deliver and resume. Forge launches OpenCode with --title envelopeID,
// so matching on title avoids choosing among unrelated active sessions.
func discoverOpenCodeSessionIDForEnvelope(ctx context.Context, openCodeURL string, logger *slog.Logger, envelopeID, workingDir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 10; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, openCodeURL+"/session", nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		var sessions []openCodeSession
		if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		matches := matchingOpenCodeSessions(sessions, envelopeID, workingDir)
		if len(matches) == 1 {
			return matches[0].ID, nil
		}
		// Zero or multiple matching sessions: keep polling. Multiple exact
		// matches should be rare but are still ambiguous.
	}
	return "", fmt.Errorf("could not discover opencode session id for envelope %q", envelopeID)
}

func matchingOpenCodeSessions(sessions []openCodeSession, envelopeID, workingDir string) []openCodeSession {
	matches := make([]openCodeSession, 0, 1)
	for _, session := range sessions {
		if session.ID == "" || session.Title != envelopeID {
			continue
		}
		if workingDir != "" && session.Directory != "" && session.Directory != workingDir {
			continue
		}
		matches = append(matches, session)
	}
	return matches
}

// Shared helper for injecting into OpenCode — used by both notify and resume.
func injectIntoOpenCode(ctx context.Context, openCodeURL, sessionID, message string) error {
	escapedID := url.PathEscape(sessionID)
	apiURL := openCodeURL + "/session/" + escapedID + "/message"
	body := map[string]string{
		"content": message,
		"role":    "user",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(errOpenCodeInjectFailed, resp.StatusCode)
	}
	return nil
}
