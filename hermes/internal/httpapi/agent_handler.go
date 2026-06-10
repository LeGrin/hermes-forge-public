package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/legrin-tech/hermes/internal/activityhub"
	"github.com/legrin-tech/hermes/internal/agentstore"
)

// agentHandler exposes the Constellation view over HTTP. Reporters
// (Forge on Mac, future peer nodes) POST a snapshot every minute;
// the dashboard GETs the current union.
type agentHandler struct {
	store  *agentstore.Store
	hub    *activityhub.Hub // nil-safe; used to fan SSE updates on new snapshots
	logger *slog.Logger
}

func (h *agentHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /agents/snapshot", h.snapshot)
	mux.HandleFunc("GET /agents", h.list)
	mux.HandleFunc("GET /agents/{id}", h.get)
	mux.HandleFunc("POST /agents/prune", h.prune)
}

// snapshot accepts a full agent list from one reporter and replaces
// that reporter's slice of the picture. Reporters MUST send the
// complete current state — missing agents fall out via TTL.
func (h *agentHandler) snapshot(w http.ResponseWriter, r *http.Request) {
	var snap agentstore.Snapshot
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if snap.Host == "" {
		writeError(w, http.StatusUnprocessableEntity, "missing_host", "snapshot.host is required")
		return
	}
	applied := h.store.Apply(snap)

	// Emit one aggregate activity event per snapshot — the dashboard
	// can use this to trigger a refresh without polling. Individual
	// agent events would be too noisy.
	if h.hub != nil {
		h.hub.Publish(activityhub.Event{
			Kind:      "agents_snapshot",
			Summary:   formatSnapshotSummary(snap.Host, applied),
			Timestamp: time.Now().UTC(),
		})
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": len(applied),
		"host":     snap.Host,
	})
}

// list returns every known-live agent sorted for a stable UI.
func (h *agentHandler) list(w http.ResponseWriter, _ *http.Request) {
	agents := h.store.Recent()
	if agents == nil {
		agents = []agentstore.Agent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func (h *agentHandler) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", "agent id is required")
		return
	}
	agent, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, agentstore.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", id)
			return
		}
		h.logger.Error("agent get failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "store_error", msgInternalError)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// prune removes agents that have not been seen within the TTL window.
// Requires admin role (X-Hermes-Key with role=admin).
func (h *agentHandler) prune(w http.ResponseWriter, r *http.Request) {
	k := KeyFromContext(r.Context())
	if k == nil || k.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin_required", "admin role required")
		return
	}

	pruned := h.store.Prune()
	remaining := len(h.store.Recent())

	h.logger.Info("agent prune", "pruned", pruned, "remaining", remaining, "operator", k.Label)
	writeJSON(w, http.StatusOK, map[string]int{
		"pruned":    pruned,
		"remaining": remaining,
	})
}

// formatSnapshotSummary builds the one-line Activity entry: host plus
// a breakdown of agents by state.
func formatSnapshotSummary(host string, agents []agentstore.Agent) string {
	active, idle, exited := 0, 0, 0
	for _, a := range agents {
		switch a.State {
		case "active":
			active++
		case "idle":
			idle++
		case "exited":
			exited++
		}
	}
	return host + " snapshot: " +
		itoa(len(agents)) + " agents (" +
		itoa(active) + " active, " +
		itoa(idle) + " idle, " +
		itoa(exited) + " exited)"
}

// itoa avoids the strconv import when the rest of the file already
// reaches for slog / encoding/json.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
