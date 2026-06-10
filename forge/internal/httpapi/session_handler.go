package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/legrin-tech/forge/internal/sessionstore"
)

// sessionHandler serves the session register read API (W-F2) and
// session I/O endpoints (W-F4, W-F5).
type sessionHandler struct {
	logger   *slog.Logger
	store    *sessionstore.Store
	registry *ProcessRegistry
}

func (h *sessionHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /sessions", h.list)
	mux.HandleFunc("GET /sessions/{id}", h.get)
	mux.HandleFunc("GET /sessions/{id}/output", h.readOutput)
	mux.HandleFunc("POST /sessions/{id}/input", h.writeInput)
}

func (h *sessionHandler) list(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "internal error")
		return
	}
	if sessions == nil {
		sessions = []sessionstore.Session{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (h *sessionHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := h.store.Get(r.Context(), id)
	if errors.Is(err, sessionstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// readOutput returns buffered stdout+stderr from the running process.
// W-F4: read from sessions.
// CON-003: Supports ?tail=N query param to return last N lines without clearing buffer.
func (h *sessionHandler) readOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	proc := h.registry.Get(id)
	if proc == nil {
		writeError(w, http.StatusNotFound, "not_found", "no running process for session")
		return
	}

	var out []byte
	if tailStr := r.URL.Query().Get("tail"); tailStr != "" {
		var tail int
		if _, err := fmt.Sscanf(tailStr, "%d", &tail); err != nil || tail <= 0 {
			tail = 20 // default tail
		}
		out = proc.ReadOutputTail(tail)
	} else {
		out = proc.ReadOutput()
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"session_id": id,
		"output":     string(out),
	})
}

type writeInputRequest struct {
	Input string `json:"input"`
}

// writeInput sends data to the running process stdin.
// W-F5: push messages into sessions.
func (h *sessionHandler) writeInput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	proc := h.registry.Get(id)
	if proc == nil {
		writeError(w, http.StatusNotFound, "not_found", "no running process for session")
		return
	}

	// Limit request body to 1 MiB to prevent DoS (review feedback).
	var req writeInputRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Input == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "input is required")
		return
	}

	// Write in a goroutine so we can honor request context cancellation
	// if the child's stdin pipe blocks (review feedback).
	type writeResult struct {
		n   int
		err error
	}
	ch := make(chan writeResult, 1)
	go func() {
		n, err := proc.Write([]byte(req.Input))
		ch <- writeResult{n, err}
	}()

	select {
	case <-r.Context().Done():
		writeError(w, http.StatusRequestTimeout, "request_canceled", "request canceled")
		return
	case res := <-ch:
		if res.err != nil {
			h.logger.Error("session write failed", "session_id", id, "err", res.err)
			writeError(w, http.StatusInternalServerError, "write_error", "write to session failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id":    id,
			"bytes_written": res.n,
		})
	}
}
