package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
)

type notificationHandler struct {
	store  *notifystore.Store
	keys   *keystore.Store // nil when no keystore is configured (dev/test mode)
	logger *slog.Logger
}

const notificationAuthRequiredMessage = "X-Hermes-Key header is required"

func (h *notificationHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /notifications", h.list)
	mux.HandleFunc("POST /notifications/ack", h.bulkAck)
	mux.HandleFunc("POST /notifications/{id}/ack", h.ack)
}

func (h *notificationHandler) list(w http.ResponseWriter, r *http.Request) {
	apiKey, ok := h.notificationAPIKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "auth_required", notificationAuthRequiredMessage)
		return
	}
	notifications, err := h.store.ListUnacknowledgedForKey(r.Context(), apiKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "internal error")
		h.logger.Error("notification list failed", "err", err)
		return
	}
	writeJSON(w, http.StatusOK, notifications)
}

func (h *notificationHandler) ack(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must be an integer")
		return
	}
	apiKey, ok := h.notificationAPIKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "auth_required", notificationAuthRequiredMessage)
		return
	}
	if err := h.store.AcknowledgeForKey(r.Context(), id, apiKey); err != nil {
		if errors.Is(err, notifystore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "notification not found or already acknowledged")
			return
		}
		h.logger.Error("notification acknowledge failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
}

type bulkAckNotificationsRequest struct {
	IDs []int64 `json:"ids"`
}

// maxBulkAckBodyBytes caps the request body for bulk ack to limit DoS exposure.
const maxBulkAckBodyBytes = 64 * 1024 // 64 KiB

func (h *notificationHandler) bulkAck(w http.ResponseWriter, r *http.Request) {
	// Auth check before body decode to avoid wasting resources on unauthenticated requests.
	apiKey, ok := h.notificationAPIKey(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "auth_required", notificationAuthRequiredMessage)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBulkAckBodyBytes)
	var req bulkAckNotificationsRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "missing_ids", "ids is required")
		return
	}
	result, err := h.store.BulkAcknowledge(r.Context(), req.IDs, apiKey)
	if err != nil {
		h.logger.Error("notification bulk acknowledge failed", "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// notificationAPIKey returns the validated API key for notification operations.
// It delegates to notificationInsertKey with the keystore-configured flag.
//
// When a keystore is configured (h.keys != nil), only the context-validated
// identity is accepted — the auth middleware has already verified the key.
// Trusting the raw X-Hermes-Key header as identity in that case would bypass
// the middleware's validation.
//
// When no keystore is configured (dev/test mode, h.keys == nil), the raw
// header is accepted as a namespace identifier (not as an auth proof, since
// there is no auth in that mode).
func (h *notificationHandler) notificationAPIKey(r *http.Request) (string, bool) {
	return notificationInsertKey(r, h.keys != nil)
}

// notificationInsertKey returns the API key to use when inserting a notification
// on behalf of an envelope operation.
//
// When a keystore is configured (keysConfigured=true), only a context-validated
// key is accepted. If no context key is present the request was either rejected
// by the middleware or bypassed it — either way insertion is skipped to avoid
// writing unscoped rows.
//
// When no keystore is configured (keysConfigured=false, dev/test mode), the raw
// X-Hermes-Key header is accepted as a namespace identifier. Note that
// Header.Get returns "" for both a missing header and an explicitly empty
// header; in no-keystore mode an empty value is treated as the public namespace
// and insertion proceeds.
func notificationInsertKey(r *http.Request, keysConfigured bool) (string, bool) {
	key := KeyFromContext(r.Context())
	if key != nil {
		return key.Key, true
	}
	// Keystore configured but no context key: middleware should have rejected
	// the request already. Reject here as a defence-in-depth measure.
	if keysConfigured {
		return "", false
	}
	// No keystore: accept raw header as namespace identifier.
	// Empty value (missing or explicitly empty header) addresses the public namespace.
	raw := r.Header.Get("X-Hermes-Key")
	return raw, true
}
