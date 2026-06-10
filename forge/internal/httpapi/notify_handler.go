package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/legrin-tech/forge/internal/sessionstore"
)

// defaultOpenCodeURL is the base URL of the local OpenCode server.
const defaultOpenCodeURL = "http://localhost:4096"

// maxNotifyBodySize is the maximum size of the POST /notify request body.
const maxNotifyBodySize = 64 * 1024 // 64 KiB

// errOpenCodeInjectFailed is the error message when OpenCode injection fails.
const errOpenCodeInjectFailed = "opencode inject returned %d"

// notifyRequest is the body of POST /notify from Hermes.
type notifyRequest struct {
	EnvelopeID        string `json:"envelope_id"`
	ExecutorSessionID string `json:"executor_session_id"`
	Message           string `json:"message"`
}

// notifyHandler receives steer notifications from Hermes and injects
// them into live OpenCode sessions.
type notifyHandler struct {
	logger      *slog.Logger
	registry    *ProcessRegistry
	store       *sessionstore.Store // session store for envelope→session lookup
	openCodeURL string              // base URL of OpenCode server
	httpClient  *http.Client        // reusable HTTP client
}

// newNotifyHandler creates a notifyHandler.
func newNotifyHandler(logger *slog.Logger, registry *ProcessRegistry, store *sessionstore.Store, openCodeURL string) *notifyHandler {
	if openCodeURL == "" {
		openCodeURL = defaultOpenCodeURL
	}
	return &notifyHandler{
		logger:      logger,
		registry:    registry,
		store:       store,
		openCodeURL: openCodeURL,
		httpClient:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (h *notifyHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /notify", h.notify)
}

// notify handles POST /notify requests from Hermes.
//
// NOTE: This endpoint allows message injection into OpenCode sessions.
// Protected by network isolation (Tailscale + internal only).
func (h *notifyHandler) notify(w http.ResponseWriter, r *http.Request) {
	// Cap request body size to avoid unbounded memory allocation.
	if r.ContentLength > maxNotifyBodySize {
		writeError(w, http.StatusUnprocessableEntity, "body_too_large", "request body exceeds 64 KiB limit")
		return
	}

	var req notifyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	// Validate required fields
	if req.EnvelopeID == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "envelope_id is required")
		return
	}
	if req.ExecutorSessionID == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "executor_session_id is required")
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "message is required")
		return
	}

	// Look up Forge session by envelope_id to get the internal session_id
	sess, err := h.store.GetByEnvelope(r.Context(), req.EnvelopeID)
	if err != nil || sess == nil {
		writeError(w, http.StatusConflict, "session_not_found", "no session for this envelope")
		return
	}

	// Verify this is an OpenCode session before injection
	if sess.Executor != "opencode" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_executor", "notify only supported for opencode executor")
		return
	}

	// Check if Forge process is alive using the internal session_id
	if !h.registry.Running(sess.SessionID) {
		writeError(w, http.StatusConflict, "session_not_alive",
			"session is not running; use resume endpoint")
		return
	}

	// Inject message into OpenCode using executor_session_id (the OC session ID)
	if err := h.injectIntoOpenCode(r.Context(), req.ExecutorSessionID, req.Message); err != nil {
		h.logger.Error("inject into opencode failed",
			"session_id", req.ExecutorSessionID,
			"err", err)
		writeError(w, http.StatusInternalServerError, "inject_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// injectIntoOpenCode calls POST /session/{id}/message on the OpenCode server.
func (h *notifyHandler) injectIntoOpenCode(ctx context.Context, sessionID, message string) error {
	escapedID := url.PathEscape(sessionID)
	url := h.openCodeURL + "/session/" + escapedID + "/message"
	body := map[string]string{
		"content": message,
		"role":    "user",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(errOpenCodeInjectFailed, resp.StatusCode)
	}
	return nil
}
