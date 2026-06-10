package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
	"github.com/legrin-tech/hermes/internal/sessionstore"
)

const (
	msgMissingID        = "session id is required"
	msgInvalidMessage   = "invalid_message"
	msgInternalErrorStr = "internal error"
	msgNotFound         = "not_found"
	msgStoreError       = "store_error"
	msgMissingIDCode    = "missing_id"
	msgInvalidJSON      = "invalid_json"
	msgInternalErrCode  = "internal_error"
)

// sessionHandler owns session-scoped routes for Hermes session lanes.
type sessionHandler struct {
	store  *sessionstore.Store
	notify *notifystore.Store // nil-safe: notifications skipped if nil
	keys   *keystore.Store    // nil when no keystore configured (dev/test mode)
	logger *slog.Logger
	firer  *webhookFirer // nil-safe: no sinks configured
}

func (h *sessionHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /sessions", h.create)
	mux.HandleFunc("GET /sessions", h.list)
	mux.HandleFunc("GET /sessions/{id}", h.get)
	mux.HandleFunc("GET /sessions/{id}/raw-tail", h.rawTail)
	mux.HandleFunc("POST /sessions/{id}/messages", h.addMessage)
	mux.HandleFunc("GET /sessions/{id}/messages", h.getMessages)
}

type createSessionRequest struct {
	Title   string `json:"title"`
	Project string `json:"project"`
}

func (h *sessionHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSON, err.Error())
		return
	}

	key := KeyFromContext(r.Context())
	apiKey := ""
	if key != nil {
		apiKey = key.Key
	}

	id, err := newUUID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgInternalErrCode, "failed to generate id")
		return
	}

	sess := &sessionstore.Session{
		ID:        id,
		Title:     req.Title,
		Project:   req.Project,
		APIKey:    apiKey,
		Status:    "active",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := h.store.Insert(r.Context(), sess); err != nil {
		h.logger.Error("session insert failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, msgStoreError, msgInternalError)
		return
	}

	w.Header().Set("Location", "/sessions/"+id)
	writeJSON(w, http.StatusCreated, sess)
}

func (h *sessionHandler) list(w http.ResponseWriter, r *http.Request) {
	key := KeyFromContext(r.Context())
	var apiKey string
	if key != nil {
		apiKey = key.Key
	}

	sessions, err := h.store.List(r.Context(), apiKey)
	if err != nil {
		h.logger.Error("session list failed", "err", err)
		writeError(w, http.StatusInternalServerError, msgStoreError, msgInternalError)
		return
	}
	if sessions == nil {
		sessions = []sessionstore.Session{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (h *sessionHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, msgMissingIDCode, msgMissingID)
		return
	}

	sess := h.checkSessionOwnership(w, r, id)
	if sess == nil {
		return // response already written
	}
	writeJSON(w, http.StatusOK, sess)
}

const (
	defaultRawTailBytes = 8192
	maxRawTailBytes     = 65536
	defaultRawTailLines = 80
	maxRawTailLines     = 200
)

func (h *sessionHandler) rawTail(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, msgMissingIDCode, msgMissingID)
		return
	}
	if h.checkSessionOwnership(w, r, sessionID) == nil {
		return
	}
	maxBytes := boundedQueryInt(r, "max_bytes", defaultRawTailBytes, maxRawTailBytes)
	maxLines := boundedQueryInt(r, "max_lines", defaultRawTailLines, maxRawTailLines)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "not_available",
		"detail":    "raw session tail is not available from Hermes; no raw source is registered",
		"max_bytes": maxBytes,
		"max_lines": maxLines,
	})
}

func boundedQueryInt(r *http.Request, name string, fallback, limit int) int {
	value := fallback
	if raw := r.URL.Query().Get(name); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			value = parsed
		}
	}
	if value > limit {
		return limit
	}
	return value
}

type addMessageRequest struct {
	From    string `json:"from"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to,omitempty"`
}

var validMessageKinds = map[string]bool{"decision": true, "steer": true, "reply": true}

func (h *sessionHandler) addMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, msgMissingIDCode, msgMissingID)
		return
	}

	// Ownership check before we even decode the body.
	sess := h.checkSessionOwnership(w, r, sessionID)
	if sess == nil {
		return // response already written
	}

	var req addMessageRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msgInvalidJSON, err.Error())
		return
	}

	if req.From == "" {
		writeError(w, http.StatusUnprocessableEntity, msgInvalidMessage, "from is required")
		return
	}
	if req.Kind == "" {
		writeError(w, http.StatusUnprocessableEntity, msgInvalidMessage, "kind is required")
		return
	}
	if !validMessageKinds[req.Kind] {
		writeError(w, http.StatusUnprocessableEntity, "invalid_kind", "kind must be one of: decision, steer, reply")
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusUnprocessableEntity, msgInvalidMessage, "text is required")
		return
	}

	msgID, err := newUUID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgInternalErrCode, "failed to generate id")
		return
	}

	msg := &sessionstore.Message{
		ID:        msgID,
		SessionID: sessionID,
		From:      req.From,
		Kind:      req.Kind,
		Text:      req.Text,
		ReplyTo:   req.ReplyTo,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := h.store.InsertMessage(r.Context(), msg); err != nil {
		if err == sessionstore.ErrNotFound {
			writeError(w, http.StatusNotFound, msgNotFound, sessionID)
			return
		}
		h.logger.Error("insert message failed", "session_id", sessionID, "err", err)
		writeError(w, http.StatusInternalServerError, msgStoreError, msgInternalError)
		return
	}

	// Notify KITT of new message (for visibility on session-originated messages).
	h.maybeNotifyMessage(r.Context(), sessionID, sess.APIKey, req.From, req.Kind)

	// Push "decision" messages from executors through the shared webhook/
	// Telegram firer so the operator sees interesting in-session findings
	// without waiting for an envelope status change. Kitt-originated
	// messages are skipped — the user already typed them.
	h.maybeFireDecision(r.Context(), sess, req.From, req.Kind, req.Text)

	writeJSON(w, http.StatusCreated, msg)
}

func (h *sessionHandler) getMessages(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, msgMissingIDCode, msgMissingID)
		return
	}

	// Ownership check.
	if h.checkSessionOwnership(w, r, sessionID) == nil {
		return // response already written
	}

	sinceID := r.URL.Query().Get("since_id")

	messages, err := h.store.GetMessages(r.Context(), sessionID, sinceID)
	if err != nil {
		if err == sessionstore.ErrNotFound {
			writeError(w, http.StatusNotFound, msgNotFound, sessionID)
			return
		}
		h.logger.Error("get messages failed", "session_id", sessionID, "err", err)
		writeError(w, http.StatusInternalServerError, msgStoreError, msgInternalError)
		return
	}

	if messages == nil {
		messages = []sessionstore.Message{}
	}
	writeJSON(w, http.StatusOK, messages)
}

// checkSessionOwnership retrieves the session and verifies the caller owns it.
// Returns the session if ownership check passes.
// Returns nil if ownership check fails (response already written to w).
//
// Security model:
//   - Keystore configured (h.keys != nil): only a context-validated key is
//     accepted. An unauthenticated/no-key request may only access sessions
//     whose api_key is empty (public namespace). Any session with a non-empty
//     api_key is treated as 404 to avoid leaking its existence.
//   - No keystore (dev/test mode): empty key is the public namespace; any
//     caller may access any session (no auth enforcement).
func (h *sessionHandler) checkSessionOwnership(w http.ResponseWriter, r *http.Request, sessionID string) *sessionstore.Session {
	key := KeyFromContext(r.Context())
	apiKey := ""
	if key != nil {
		apiKey = key.Key
	}

	sess, err := h.store.Get(r.Context(), sessionID)
	if err != nil {
		if err == sessionstore.ErrNotFound {
			writeError(w, http.StatusNotFound, msgNotFound, sessionID)
			return nil
		}
		h.logger.Error("session get failed", "session_id", sessionID, "err", err)
		writeError(w, http.StatusInternalServerError, msgStoreError, msgInternalError)
		return nil
	}

	if apiKey == "" {
		// No validated key in context.
		if h.keys != nil && sess.APIKey != "" {
			// Keystore is configured and the session belongs to a specific key.
			// Unauthenticated callers must not access it — return 404 to avoid
			// leaking the session's existence.
			writeError(w, http.StatusNotFound, msgNotFound, sessionID)
			return nil
		}
		// Either no keystore (dev mode) or session is in the public namespace.
		return sess
	}

	// Validated key present → enforce ownership.
	if sess.APIKey != apiKey {
		writeError(w, http.StatusNotFound, msgNotFound, sessionID)
		return nil
	}
	return sess
}

// maybeNotifyMessage inserts a notification for session-originated messages.
// This gives KITT visibility into messages from OpenCode/Claude that don't
// arrive via the normal envelope flow.
func (h *sessionHandler) maybeNotifyMessage(ctx context.Context, sessionID, apiKey, from, kind string) {
	if h.notify == nil {
		return
	}
	if apiKey == "" && h.keys != nil {
		// Keystore is configured but session has no API key — skip insertion to
		// avoid unreachable empty-key rows. In dev/test mode (no keystore),
		// empty key is accepted as a valid namespace.
		return
	}

	n := &notifystore.Notification{
		EnvelopeID: sessionID,
		Status:     "session_message",
		Note:       fmt.Sprintf("message from %s (%s)", from, kind),
		APIKey:     apiKey,
	}
	if err := h.notify.Insert(ctx, n); err != nil {
		h.logger.Error("notification insert failed", "session_id", sessionID, "err", err)
	}
}

// maybeFireDecision forwards a session "decision" message through the
// shared firer so the OpenClaw webhook / Telegram sinks see it. Only
// executor-authored decisions are forwarded — kitt-authored ("user typed
// it") and non-decision kinds ("steer", "reply") are filtered out.
//
// Status on the wire is "session_decision" so KITT's skill can route it
// differently from envelope status changes if it wants to.
func (h *sessionHandler) maybeFireDecision(ctx context.Context, sess *sessionstore.Session, from, kind, text string) {
	if h.firer == nil {
		return
	}
	if kind != "decision" {
		return
	}
	if from == "" || from == "kitt" {
		return
	}
	title := sess.Title
	if title == "" {
		title = sess.ID
	}
	note := fmt.Sprintf("%s: %s", from, text)
	h.firer.Fire(ctx, sess.ID, title, "session_decision", note, "", false)
}
