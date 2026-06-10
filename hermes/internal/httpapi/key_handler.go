package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/legrin-tech/hermes/internal/keystore"
)

type keyHandler struct {
	store  *keystore.Store
	logger *slog.Logger
}

func (h *keyHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/keys", h.list)
	mux.HandleFunc("POST /admin/keys", h.create)
	mux.HandleFunc("DELETE /admin/keys/{key}", h.delete)
}

func (h *keyHandler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	k := KeyFromContext(r.Context())
	if k == nil || k.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin_required", "admin role required")
		return false
	}
	return true
}

func (h *keyHandler) list(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	keys, err := h.store.List(r.Context())
	if err != nil {
		h.logger.Error("key list failed", "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (h *keyHandler) create(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var req struct {
		Label string `json:"label"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return
	}
	if req.Label == "" {
		writeError(w, http.StatusUnprocessableEntity, "missing_label", "label is required")
		return
	}
	if req.Role != "" && req.Role != "user" && req.Role != "admin" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_role", "role must be 'user' or 'admin'")
		return
	}
	k, err := h.store.Create(r.Context(), req.Label, req.Role)
	if err != nil {
		if errors.Is(err, keystore.ErrDuplicate) {
			writeError(w, http.StatusConflict, "duplicate_label", "label already exists")
			return
		}
		h.logger.Error("key create failed", "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

func (h *keyHandler) delete(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	key := r.PathValue("key")
	if err := h.store.Delete(r.Context(), key); err != nil {
		if errors.Is(err, keystore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "key not found")
			return
		}
		h.logger.Error("key delete failed", "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
