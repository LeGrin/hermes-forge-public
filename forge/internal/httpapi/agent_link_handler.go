// Package httpapi wires Forge HTTP routes.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
)

// agentLinkStore holds parent→child links established by the OpenCode
// plugin via POST /agent/link. The map is keyed by child agent ID and
// is consulted by the reporter on each Scan() so ParentID is embedded
// in the next snapshot sent to Hermes.
//
// Concurrent-safe: reads and writes go through a RWMutex.
type agentLinkStore struct {
	mu    sync.RWMutex
	links map[string]string // childID → parentID
}

// Link records a parent→child relationship. Subsequent calls for the
// same childID overwrite the previous parent.
func (s *agentLinkStore) Link(childID, parentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links == nil {
		s.links = make(map[string]string)
	}
	s.links[childID] = parentID
}

// ParentOf returns the parent ID for the given child, or "" if unknown.
func (s *agentLinkStore) ParentOf(childID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.links[childID]
}

// linkRequest is the POST /agent/link body.
type linkRequest struct {
	AgentID  string `json:"agent_id"`
	ParentID string `json:"parent_id"`
}

// agentLinkHandler exposes POST /agent/link so the OpenCode plugin can
// register parent-child edges as soon as OpenCode spawns a sub-agent.
type agentLinkHandler struct {
	logger *slog.Logger
	store  *agentLinkStore
}

// register mounts the /agent/link route.
func (h *agentLinkHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /agent/link", h.link)
}

// link accepts {agent_id, parent_id} and stores the edge for the
// next reporter snapshot to pick up.
func (h *agentLinkHandler) link(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", r.Method+" not allowed here")
		return
	}
	var req linkRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.AgentID == "" || req.ParentID == "" {
		writeError(w, http.StatusUnprocessableEntity, "missing_fields", "agent_id and parent_id are required")
		return
	}

	h.store.Link(req.AgentID, req.ParentID)
	h.logger.Info("agent link registered", "child", req.AgentID, "parent", req.ParentID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
