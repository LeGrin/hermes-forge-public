// Package agentstore holds a live view of every AI/executor process
// reported by peer nodes (Forge on Mac, the VPS node itself, etc.).
//
// It is intentionally in-memory only: snapshots come in every minute
// from reporters, old records that stop being reported fall out after
// a TTL. This is telemetry, not authoritative state — the dashboard
// Constellation tab renders from it.
package agentstore

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when an agent is absent from the recent view.
var ErrNotFound = errors.New("agentstore: not found")

// Agent describes one executor process at the moment of the last
// snapshot that contained it.
type Agent struct {
	// ID is stable across snapshots for the same process: usually
	// <host>:<executor>:<pid>, but reporters may choose their own
	// scheme as long as it stays stable.
	ID string `json:"id"`

	// Host identifies the node the process is running on — "mac-forge",
	// "vps-kitt", etc. Used to group agents in the UI.
	Host string `json:"host"`

	// Executor is the binary name: "claude", "opencode", "kitt", "forge".
	Executor string `json:"executor"`

	// PID on the reporting host. Diagnostic only — not meaningful across hosts.
	PID int `json:"pid"`

	// CWD is the working directory of the process at sample time.
	CWD string `json:"cwd,omitempty"`

	// Project is the short name matched from CWD against the Hermes
	// project registry ("hermes", "kingdom", "tenderium" …). Empty if
	// the reporter couldn't match.
	Project string `json:"project,omitempty"`

	// SessionID is the executor's native session id (Claude's
	// --session-id UUID, OpenCode's `ses_…`). Empty if unknown.
	SessionID string `json:"session_id,omitempty"`

	// Title is a short human label: the last user message head, the
	// command tail, or the Hermes envelope id when spawned by Forge.
	Title string `json:"title,omitempty"`

	// State is "active" | "idle" | "exited". Reporters decide based
	// on CPU + file activity — the store treats it opaquely.
	State string `json:"state"`

	// ParentKind classifies where the process came from:
	// "forge" — spawned by Forge for a Hermes envelope
	// "osm-daemon" — spawned by the OpenCode Session Manager daemon
	// "user-tty" — an operator ran it in a terminal
	// "cmux" — under the cmux terminal multiplexer
	// "init" — orphan adopted by PID 1
	// "other"
	ParentKind string `json:"parent_kind,omitempty"`

	// ParentID is the agent.ID of the parent process that spawned this
	// one. Set by Forge's POST /agent/link endpoint when OpenCode's
	// session.created event carries a parentID — survives snapshot
	// replace so the parent edge is not lost between reporter ticks.
	ParentID string `json:"parent_id,omitempty"`

	// CPUPercent / MemPercent — instantaneous sample.
	CPUPercent float64 `json:"cpu_percent,omitempty"`
	MemPercent float64 `json:"mem_percent,omitempty"`

	// StartedAt is the process start time.
	StartedAt time.Time `json:"started_at"`

	// LastSeenAt is the timestamp of the snapshot that carried this
	// agent. Dashboards drop entries whose LastSeenAt is older than
	// the TTL.
	LastSeenAt time.Time `json:"last_seen_at"`

	// StaleAt is set when Apply() receives a snapshot that omits an
	// agent previously reported by the same host. The agent becomes
	// invisible in Recent() immediately and is deleted by Prune().
	// Zero value means the agent is not stale.
	StaleAt time.Time `json:"stale_at,omitempty"`

	// ReportedBy identifies the reporter node (usually same as Host,
	// but may differ for relays).
	ReportedBy string `json:"reported_by,omitempty"`
}

// Snapshot is what a reporter POSTs for one tick. Agents not present
// in a new snapshot from the same host may be dropped after TTL.
type Snapshot struct {
	Host    string    `json:"host"`
	TakenAt time.Time `json:"taken_at"`
	Agents  []Agent   `json:"agents"`
}

// Store holds the latest known agent state keyed by Agent.ID.
type Store struct {
	ttl time.Duration
	mu  sync.RWMutex
	// agents is keyed by ID; value is the most recent record seen.
	agents map[string]Agent
}

// New creates an empty store. ttl controls how long an agent remains
// visible after the last snapshot that carried it — 5 minutes by default.
func New(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Store{
		ttl:    ttl,
		agents: make(map[string]Agent),
	}
}

// Apply merges a snapshot: every agent in snap replaces its previous
// record. Agents from the same host that are absent from the new
// snapshot are marked StaleAt so they disappear from Recent() immediately.
// ParentID is preserved across snapshots: if the incoming record has an
// empty ParentID but the stored record already has one, the stored
// value is retained. Returns the merged agents (for SSE fan-out).
func (s *Store) Apply(snap Snapshot) []Agent {
	if snap.TakenAt.IsZero() {
		snap.TakenAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	snapIDs := snapshotAgentIDs(snap.Agents)

	applied := make([]Agent, 0, len(snap.Agents))
	for _, a := range snap.Agents {
		agent, ok := s.prepareSnapshotAgent(a, snap)
		if !ok {
			continue
		}
		s.agents[agent.ID] = agent
		applied = append(applied, agent)
	}

	s.markAbsentAgentsStale(snap.Host, snap.TakenAt, snapIDs)

	return applied
}

func snapshotAgentIDs(agents []Agent) map[string]struct{} {
	ids := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		if a.ID != "" {
			ids[a.ID] = struct{}{}
		}
	}
	return ids
}

func (s *Store) prepareSnapshotAgent(a Agent, snap Snapshot) (Agent, bool) {
	if a.ID == "" {
		return Agent{}, false
	}
	a.LastSeenAt = snap.TakenAt
	a.StaleAt = time.Time{}
	if a.Host == "" {
		a.Host = snap.Host
	}
	if a.ReportedBy == "" {
		a.ReportedBy = snap.Host
	}
	if a.ParentID == "" {
		a.ParentID = s.existingParentID(a.ID)
	}
	return a, true
}

func (s *Store) existingParentID(id string) string {
	existing, ok := s.agents[id]
	if !ok {
		return ""
	}
	return existing.ParentID
}

func (s *Store) markAbsentAgentsStale(host string, takenAt time.Time, snapIDs map[string]struct{}) {
	for id, a := range s.agents {
		if !shouldMarkStale(id, a, host, takenAt, snapIDs) {
			continue
		}
		a.StaleAt = takenAt
		s.agents[id] = a
	}
}

func shouldMarkStale(id string, a Agent, host string, takenAt time.Time, snapIDs map[string]struct{}) bool {
	if a.Host != host || !a.StaleAt.IsZero() {
		return false
	}
	if _, ok := snapIDs[id]; ok {
		return false
	}
	// An out-of-order snapshot must not evict agents reported later.
	return !takenAt.Before(a.LastSeenAt)
}

// Recent returns every agent whose LastSeenAt is within the TTL
// and is not marked stale, sorted for a stable UI render: host asc,
// then state (active > idle > exited), then started_at desc.
func (s *Store) Recent() []Agent {
	cutoff := time.Now().UTC().Add(-s.ttl)
	s.mu.RLock()
	out := make([]Agent, 0, len(s.agents))
	for _, a := range s.agents {
		if !a.StaleAt.IsZero() {
			continue // stale — absent from latest snapshot for this host
		}
		if a.LastSeenAt.Before(cutoff) {
			continue
		}
		out = append(out, a)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		if stateRank(out[i].State) != stateRank(out[j].State) {
			return stateRank(out[i].State) < stateRank(out[j].State)
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// Get returns one recent agent by id. Stale or TTL-expired agents are hidden.
func (s *Store) Get(id string) (Agent, error) {
	cutoff := time.Now().UTC().Add(-s.ttl)
	s.mu.RLock()
	defer s.mu.RUnlock()
	agent, ok := s.agents[id]
	if !ok || !agent.StaleAt.IsZero() || agent.LastSeenAt.Before(cutoff) {
		return Agent{}, ErrNotFound
	}
	return agent, nil
}

// Prune drops agents that are stale or past the TTL. Callers may run
// this on a timer or piggy-back on Recent() which already filters.
// Exposed so tests and dashboards can assert size deterministically.
func (s *Store) Prune() int {
	cutoff := time.Now().UTC().Add(-s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, a := range s.agents {
		if !a.StaleAt.IsZero() || a.LastSeenAt.Before(cutoff) {
			delete(s.agents, id)
			n++
		}
	}
	return n
}

// stateRank orders active > idle > exited for sort stability.
func stateRank(s string) int {
	switch s {
	case "active":
		return 0
	case "idle":
		return 1
	case "exited":
		return 2
	default:
		return 3
	}
}
