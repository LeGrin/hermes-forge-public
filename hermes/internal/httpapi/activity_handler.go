package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/legrin-tech/hermes/internal/activityhub"
)

type activityHandler struct {
	hub    *activityhub.Hub
	logger *slog.Logger
}

func (h *activityHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /activity", h.post)
	mux.HandleFunc("GET /activity", h.recent)
	mux.HandleFunc("GET /events", h.sse)
}

func (h *activityHandler) post(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EnvelopeID string `json:"envelope_id"`
		Kind       string `json:"kind"`
		Summary    string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		return
	}
	if req.Kind == "" {
		writeError(w, http.StatusUnprocessableEntity, "missing_kind", "kind is required")
		return
	}

	// API key from auth middleware context.
	var apiKey string
	if k := KeyFromContext(r.Context()); k != nil {
		apiKey = k.Key
	}

	h.hub.Publish(activityhub.Event{
		APIKey:     apiKey,
		EnvelopeID: req.EnvelopeID,
		Kind:       req.Kind,
		Summary:    req.Summary,
		Timestamp:  time.Now().UTC(),
	})

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (h *activityHandler) sse(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "sse_unsupported", "streaming not supported")
		return
	}

	var apiKey string
	if k := KeyFromContext(r.Context()); k != nil {
		if k.Role != "admin" {
			apiKey = k.Key
		}
	}

	sub := h.hub.Subscribe(apiKey)
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-sub.Events():
			if !ok {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				h.logger.Error("sse marshal failed", "err", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (h *activityHandler) recent(w http.ResponseWriter, r *http.Request) {
	var apiKey string
	if k := KeyFromContext(r.Context()); k != nil {
		if k.Role != "admin" {
			apiKey = k.Key // scope to own events
		}
		// admin with key="" sees all
	}

	events := h.hub.Recent(apiKey, 50)
	writeJSON(w, http.StatusOK, events)
}
