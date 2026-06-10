package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/legrin-tech/hermes/envelope"
	"github.com/legrin-tech/hermes/internal/activityhub"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
)

// webhookClient is a shared HTTP client for simple webhook POSTs (5s timeout).
var webhookClient = &http.Client{Timeout: 5 * time.Second}

// openClawClient has a longer timeout because the gateway calls an LLM.
var openClawClient = &http.Client{Timeout: 30 * time.Second}

const msgInternalError = "internal error"
const msgEnvelopeIDRequired = "envelope id is required"
const errCodeInvalidJSON = "invalid_json"
const errCodeInvalidMessage = "invalid_message"

// envelopeHandler owns envelope-scoped routes.
// The handler is a thin layer over envelopestore: it decodes, validates,
// stores, and maps store errors to HTTP status codes. No business logic.
type envelopeHandler struct {
	store  *envelopestore.Store
	notify *notifystore.Store // nil-safe: notifications skipped if nil
	keys   *keystore.Store    // nil when no keystore configured (dev/test mode)
	logger *slog.Logger
	firer  *webhookFirer    // nil-safe: no sinks configured
	hub    *activityhub.Hub // nil-safe: activity publishing skipped if nil
}

func (h *envelopeHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /envelopes", h.create)
	mux.HandleFunc("GET /envelopes", h.list)
	mux.HandleFunc("GET /envelopes/{id}", h.get)
	mux.HandleFunc("PATCH /envelopes/{id}/status", h.updateStatus)
	mux.HandleFunc("PATCH /envelopes/{id}/session", h.setSession)
	mux.HandleFunc("POST /envelopes/{id}/history", h.addHistory)
	mux.HandleFunc("POST /envelopes/{id}/thread", h.appendMessage)
	mux.HandleFunc("GET /envelopes/{id}/thread", h.getThread)
}

// create handles POST /envelopes.
//
// Worldview enforced at this boundary:
//   - W-H10 (normalize to valid task protocol): malformed JSON → 400;
//     missing required fields → 422.
//   - W-H16 (never deliver same id twice without traceable identity):
//     duplicate id → 409. The caller re-posting the same id is treated
//     as an idempotency signal, not an update path.
func (h *envelopeHandler) create(w http.ResponseWriter, r *http.Request) {
	var e envelope.Envelope
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidJSON, err.Error())
		return
	}

	// Server-side defaults: status=created and created_at=now if the caller
	// didn't set them. This keeps KITT's envelope-creation path minimal.
	if e.Status == "" {
		e.Status = envelope.StatusCreated
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}

	if err := e.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_envelope", err.Error())
		return
	}

	if err := h.store.Insert(r.Context(), &e); err != nil {
		switch {
		case errors.Is(err, envelopestore.ErrDuplicate):
			writeError(w, http.StatusConflict, "duplicate_envelope", err.Error())
		default:
			h.logger.Error("envelope insert failed", "id", e.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		}
		return
	}

	w.Header().Set("Location", "/envelopes/"+e.ID)
	writeJSON(w, http.StatusCreated, &e)
}

// list handles GET /envelopes?status=blocked,paused.
//
// Worldview enforced:
//   - W-H6 (report who is doing what): returns envelopes filtered by status.
//     KITT uses this to discover escalated work (paused/blocked).
func (h *envelopeHandler) list(w http.ResponseWriter, r *http.Request) {
	var statuses []envelope.Status
	if q := r.URL.Query().Get("status"); q != "" {
		for _, s := range strings.Split(q, ",") {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				continue
			}
			st := envelope.Status(trimmed)
			if !st.Known() {
				writeError(w, http.StatusBadRequest, "invalid_status", "unknown status: "+string(st))
				return
			}
			statuses = append(statuses, st)
		}
	}

	envelopes, err := h.store.List(r.Context(), statuses)
	if err != nil {
		h.logger.Error("envelope list failed", "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	if envelopes == nil {
		envelopes = []envelope.Envelope{}
	}
	writeJSON(w, http.StatusOK, envelopes)
}

// get handles GET /envelopes/{id}.
//
// Worldview enforced:
//   - W-H6 (report who is doing what): returns current envelope state as
//     the authoritative read side. Truth lives in the store.
func (h *envelopeHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", msgEnvelopeIDRequired)
		return
	}
	e, err := h.store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, envelopestore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", id)
			return
		}
		h.logger.Error("envelope get failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

type updateStatusRequest struct {
	Status string            `json:"status"`
	Proof  map[string]string `json:"proof,omitempty"`
	Note   string            `json:"note,omitempty"`
}

// updateStatus handles PATCH /envelopes/{id}/status.
//
// Worldview enforced at this boundary:
//   - W-H15 (say "done" without proof): CanTransition rejects done without
//     proof_required keys → 422.
//   - W-H17 (forget completed/active ownership): terminal states cannot
//     transition → 422.
func (h *envelopeHandler) updateStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", msgEnvelopeIDRequired)
		return
	}

	var req updateStatusRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_status", "status is required")
		return
	}

	updated, err := h.store.UpdateStatus(r.Context(), id, envelope.Status(req.Status), req.Proof, req.Note)
	if err != nil {
		switch {
		case errors.Is(err, envelopestore.ErrNotFound):
			writeError(w, http.StatusNotFound, "not_found", id)
		case errors.Is(err, envelope.ErrTerminalTransition),
			errors.Is(err, envelope.ErrDoneWithoutProof),
			errors.Is(err, envelope.ErrUnknownStatus):
			writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
		default:
			h.logger.Error("envelope update status failed", "id", id, "err", err)
			writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		}
		return
	}

	// Fire notification for interesting statuses (best-effort, never blocks response).
	h.maybeNotify(r, id, req.Status, req.Note, req.Proof)
	// Dashboard Activity tab gets every transition, not just the
	// operator-interesting ones (delivered / in_progress are what make
	// "something is happening" visible in real time).
	h.publishActivity(r, id, req.Status, req.Note)

	writeJSON(w, http.StatusOK, updated)
}

type addHistoryRequest struct {
	Entry string `json:"entry"`
}

// addHistory handles POST /envelopes/{id}/history.
// Appends an entry to the envelope's history without changing status.
// Used by Claude to log decisions: "[DECISION] chose X because Y".
func (h *envelopeHandler) addHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", msgEnvelopeIDRequired)
		return
	}

	var req addHistoryRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Entry == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_entry", "entry is required")
		return
	}

	stamped := fmt.Sprintf("[%s] %s", time.Now().UTC().Format(time.RFC3339), req.Entry)
	updated, err := h.store.AddHistory(r.Context(), id, stamped)
	if err != nil {
		if errors.Is(err, envelopestore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", id)
			return
		}
		h.logger.Error("envelope add history failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type setSessionRequest struct {
	ExecutorSessionID string `json:"executor_session_id"`
}

// setSession handles PATCH /envelopes/{id}/session.
// Sets the executor_session_id field on an envelope.
func (h *envelopeHandler) setSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", msgEnvelopeIDRequired)
		return
	}

	var req setSessionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ExecutorSessionID == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_session", "executor_session_id is required")
		return
	}

	if err := h.store.SetExecutorSessionID(r.Context(), id, req.ExecutorSessionID); err != nil {
		if errors.Is(err, envelopestore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", id)
			return
		}
		h.logger.Error("envelope set session failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// maybeNotify inserts a notification if the status is interesting.
// Detects loops: >3 blocked notifications for the same envelope in 30 min.
func (h *envelopeHandler) maybeNotify(r *http.Request, envelopeID, status, note string, proof map[string]string) {
	if h.notify == nil {
		return
	}
	if !notifystore.InterestingStatuses[status] {
		return
	}
	apiKey, ok := notificationInsertKey(r, h.keys != nil)
	if !ok {
		return
	}

	n := &notifystore.Notification{
		EnvelopeID:   envelopeID,
		Status:       status,
		Note:         note,
		ProofSummary: notifystore.ProofSummaryFromMap(proof),
		APIKey:       apiKey,
	}
	if err := h.notify.Insert(r.Context(), n); err != nil {
		h.logger.Error("notification insert failed", "envelope_id", envelopeID, "err", err)
		return
	}
	h.maybeFireWebhook(r.Context(), envelopeID, status, note, n.ProofSummary, false)

	// Loop detection: if >3 blocked notifications in 30 min, flag it.
	if status == "blocked" {
		count, err := h.notify.CountRecentByStatus(r.Context(), envelopeID, "blocked", 30)
		if err != nil {
			h.logger.Error("loop detection query failed", "err", err)
			return
		}
		if count > 3 {
			loop := &notifystore.Notification{
				EnvelopeID: envelopeID,
				Status:     "loop_detected",
				Note:       fmt.Sprintf("LOOP DETECTED: envelope %s has been blocked %d times in the last 30 minutes", envelopeID, count),
				APIKey:     apiKey,
			}
			if err := h.notify.Insert(r.Context(), loop); err != nil {
				h.logger.Error("loop notification insert failed", "err", err)
				return
			}
			h.maybeFireWebhook(r.Context(), envelopeID, "loop_detected", loop.Note, "", true)
		}
	}
}

// openClawSessionKey is the persistent session key used for all Hermes
// notifications delivered to the OpenClaw worker agent.
const openClawSessionKey = "hermes-notifications"

// openClawChatRequest is the OpenAI-compatible chat completions request body.
type openClawChatRequest struct {
	Model    string            `json:"model"`
	Messages []openClawChatMsg `json:"messages"`
}

type openClawChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// publishActivity mirrors an envelope status change into the activityhub
// SSE stream so the dashboard Activity tab gets a live entry without an
// executor having to call hermes_report_activity explicitly.
//
// The event's APIKey is intentionally empty ("public") so every
// authenticated subscriber sees every envelope transition. Envelope
// status changes are operational telemetry shared across the whole
// operator view, not per-key private data. The caller that wrote the
// PATCH may be Forge (forge key), an executor (its own key), or KITT
// (kitt key); restricting visibility to the writer would silently
// hide most activity from the operator's dashboard. Multi-tenant
// scoping comes back when we actually have multiple tenants.
//
// Kept nil-safe so tests without a hub still pass.
func (h *envelopeHandler) publishActivity(_ *http.Request, envelopeID, status, note string) {
	if h.hub == nil {
		return
	}
	summary := fmt.Sprintf("%s → %s", envelopeID, status)
	if note != "" {
		summary += ": " + note
	}
	h.hub.Publish(activityhub.Event{
		EnvelopeID: envelopeID,
		Kind:       "status",
		Summary:    summary,
		Timestamp:  time.Now().UTC(),
	})
}

// maybeFireWebhook resolves the envelope's task title and delegates to the
// shared firer. Kept as a method so the surrounding loop-detection logic
// doesn't need to pipe the title through every call site.
func (h *envelopeHandler) maybeFireWebhook(ctx context.Context, envelopeID, status, note, proofSummary string, loopDetected bool) {
	taskTitle := ""
	if e, err := h.store.Get(ctx, envelopeID); err == nil {
		taskTitle = e.TaskTitle
	}
	h.firer.Fire(ctx, envelopeID, taskTitle, status, note, proofSummary, loopDetected)
}

// openClawNotify sends a notification to the OpenClaw worker agent via
// the OpenAI-compatible HTTP API with a persistent session key.
func openClawNotify(baseURL, token, message string) error {
	reqBody := openClawChatRequest{
		Model: "openclaw/worker",
		Messages: []openClawChatMsg{
			{Role: "user", Content: message},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal openclaw request: %w", err)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-openclaw-session-key", openClawSessionKey)

	resp, err := openClawClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("openclaw returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// formatWebhookGoal builds a human-readable notification string.
func formatWebhookGoal(envelopeID, status, taskTitle, note, proofSummary string, loopDetected bool) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Hermes notification: envelope %s, status=%s", envelopeID, status))
	if taskTitle != "" {
		sb.WriteString(fmt.Sprintf(", task=%q", taskTitle))
	}
	if loopDetected {
		sb.WriteString(", LOOP DETECTED")
	}
	if note != "" {
		sb.WriteString(fmt.Sprintf(". Note: %s", note))
	}
	if proofSummary != "" {
		sb.WriteString(fmt.Sprintf(". Proof: %s", proofSummary))
	}
	return sb.String()
}

// formatTelegramMessage formats a status change as a concise Telegram message.
func formatTelegramMessage(status, taskTitle, note, proofSummary string, loopDetected bool) string {
	if loopDetected || status == "loop_detected" {
		return fmt.Sprintf("🔄 LOOP: %s\n%s", taskTitle, note)
	}
	switch status {
	case "done":
		return fmt.Sprintf("✅ %s\n%s", taskTitle, proofSummary)
	case "blocked":
		return fmt.Sprintf("🚫 BLOCKED: %s\n%s", taskTitle, note)
	case "failed":
		return fmt.Sprintf("❌ FAILED: %s\n%s", taskTitle, note)
	case "awaiting_confirm":
		return fmt.Sprintf("❓ %s\n%s", taskTitle, note)
	case "session_decision":
		return fmt.Sprintf("💡 %s\n%s", taskTitle, note)
	default:
		if note != "" {
			return fmt.Sprintf("📨 %s: %s\n%s", status, taskTitle, note)
		}
		return fmt.Sprintf("📨 %s: %s", status, taskTitle)
	}
}

// telegramAPIBaseURL is the Telegram Bot API base URL. Overridable in tests.
var telegramAPIBaseURL = "https://api.telegram.org"

// telegramSend sends a message via Telegram Bot API (fire-and-forget).
// threadID is the message_thread_id for topic routing in supergroups; pass "" to send to main thread.
func telegramSend(token, chatID, threadID, text string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBaseURL, token)
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if threadID != "" {
		threadNum, err := strconv.ParseInt(threadID, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid telegram thread id %q: %w", threadID, err)
		}
		payload["message_thread_id"] = threadNum
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telegram request: %w", err)
	}
	resp, err := webhookClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// newUUID generates a UUID v4 using crypto/rand (no external dependencies).
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("uuid: rand.Read: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// --- Thread endpoints (v2-002) ---

type appendThreadRequest struct {
	From    string `json:"from"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to,omitempty"`
}

var validKinds = map[string]bool{"decision": true, "steer": true, "reply": true}

// appendMessage handles POST /envelopes/{id}/thread.
func (h *envelopeHandler) appendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", msgEnvelopeIDRequired)
		return
	}

	var req appendThreadRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidJSON, err.Error())
		return
	}

	// Validate required fields.
	if req.From == "" {
		writeError(w, http.StatusUnprocessableEntity, errCodeInvalidMessage, "from is required")
		return
	}
	if req.Kind == "" {
		writeError(w, http.StatusUnprocessableEntity, errCodeInvalidMessage, "kind is required")
		return
	}
	if !validKinds[req.Kind] {
		writeError(w, http.StatusUnprocessableEntity, "invalid_kind", "kind must be one of: decision, steer, reply")
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusUnprocessableEntity, errCodeInvalidMessage, "text is required")
		return
	}

	msgID, err := newUUID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to generate id")
		return
	}
	msg := envelope.Message{
		ID:      msgID,
		From:    req.From,
		Kind:    req.Kind,
		Text:    req.Text,
		ReplyTo: req.ReplyTo,
		At:      time.Now().UTC(),
	}

	if err := h.store.AppendMessage(r.Context(), id, msg); err != nil {
		if errors.Is(err, envelopestore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", id)
			return
		}
		h.logger.Error("append message failed", "envelope_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}

	writeJSON(w, http.StatusCreated, msg)
}

// getThread handles GET /envelopes/{id}/thread?since_id=<msg_id>.
func (h *envelopeHandler) getThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", msgEnvelopeIDRequired)
		return
	}

	sinceID := r.URL.Query().Get("since_id")

	messages, err := h.store.GetThread(r.Context(), id, sinceID)
	if err != nil {
		if errors.Is(err, envelopestore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", id)
			return
		}
		h.logger.Error("get thread failed", "envelope_id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}

	if messages == nil {
		messages = []envelope.Message{}
	}
	writeJSON(w, http.StatusOK, messages)
}
