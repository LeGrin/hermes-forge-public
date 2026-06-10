package agentstore

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestStore_ApplyAndRecent(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()
	snap := Snapshot{
		Host:    "mac-forge",
		TakenAt: now,
		Agents: []Agent{
			{ID: "mac:claude:100", Executor: "claude", PID: 100, State: "active", StartedAt: now.Add(-time.Hour)},
			{ID: "mac:opencode:200", Executor: "opencode", PID: 200, State: "idle", StartedAt: now.Add(-2 * time.Hour)},
		},
	}
	applied := s.Apply(snap)
	if len(applied) != 2 {
		t.Fatalf("applied: got %d, want 2", len(applied))
	}

	rec := s.Recent()
	if len(rec) != 2 {
		t.Fatalf("recent: got %d, want 2", len(rec))
	}
	// active should come first (state rank 0)
	if rec[0].State != "active" {
		t.Errorf("expected active first, got %q", rec[0].State)
	}
	if rec[0].Host != "mac-forge" || rec[1].Host != "mac-forge" {
		t.Errorf("host propagation failed: %v", rec)
	}
}

func TestStore_TTLPrune(t *testing.T) {
	s := New(50 * time.Millisecond)
	s.Apply(Snapshot{
		Host:    "mac-forge",
		TakenAt: time.Now().UTC().Add(-time.Hour), // older than TTL
		Agents:  []Agent{{ID: "stale", Executor: "claude", State: "exited", StartedAt: time.Now().UTC()}},
	})
	if n := s.Prune(); n != 1 {
		t.Errorf("prune: expected 1 stale, got %d", n)
	}
	if len(s.Recent()) != 0 {
		t.Errorf("recent should be empty after prune")
	}
}

func TestStore_LatestSnapshotReplaces(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()
	s.Apply(Snapshot{Host: "h", TakenAt: now, Agents: []Agent{
		{ID: "agent-1", Executor: "claude", State: "active", CPUPercent: 5, StartedAt: now},
	}})
	s.Apply(Snapshot{Host: "h", TakenAt: now.Add(time.Second), Agents: []Agent{
		{ID: "agent-1", Executor: "claude", State: "idle", CPUPercent: 0.1, StartedAt: now},
	}})
	r := s.Recent()
	if len(r) != 1 || r[0].State != "idle" || r[0].CPUPercent != 0.1 {
		t.Errorf("expected latest snapshot to win: %+v", r)
	}
}

// TestStore_HostPropagation asserts that agents with empty Host get
// the snapshot's Host applied, so reporters don't have to duplicate
// the value on every record.
func TestStore_HostPropagation(t *testing.T) {
	s := New(time.Minute)
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: time.Now().UTC(), Agents: []Agent{
		{ID: "a", Executor: "claude", State: "active", StartedAt: time.Now().UTC()},
	}})
	r := s.Recent()
	if len(r) != 1 {
		t.Fatal("expected one agent")
	}
	if r[0].Host != "mac-forge" || r[0].ReportedBy != "mac-forge" {
		t.Errorf("expected Host+ReportedBy filled in, got %+v", r[0])
	}
}

// TestStore_RejectEmptyID locks the invariant: records without an ID
// cannot be stored, otherwise `map[ID]Agent` would silently collide.
func TestStore_RejectEmptyID(t *testing.T) {
	s := New(time.Minute)
	applied := s.Apply(Snapshot{Host: "h", Agents: []Agent{
		{ID: "", Executor: "x", State: "active", StartedAt: time.Now().UTC()},
	}})
	if len(applied) != 0 {
		t.Errorf("empty ID should have been rejected; applied=%d", len(applied))
	}
	if len(s.Recent()) != 0 {
		t.Errorf("store should be empty")
	}
}

// TestAgentStore_Apply_KeepsParentID is the regression test for CON-004.
// It asserts that when an agent record already has a ParentID and a new
// snapshot arrives WITHOUT that field, Apply() does NOT overwrite it
// with an empty string — ParentID survives snapshot replace.
func TestAgentStore_Apply_KeepsParentID(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()

	// First snapshot: agent with ParentID set (carried from Forge via
	// POST /agent/link link-time augmentation).
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now, Agents: []Agent{
		{
			ID:         "mac-forge:opencode:9000",
			Executor:   "opencode",
			PID:        9000,
			State:      "active",
			StartedAt:  now.Add(-time.Minute),
			ParentID:   "mac-forge:opencode:8500",
			ParentKind: "opencode",
		},
	}})

	rec := s.Recent()
	if len(rec) != 1 {
		t.Fatalf("expected 1 agent after first snapshot, got %d", len(rec))
	}
	if rec[0].ParentID != "mac-forge:opencode:8500" {
		t.Errorf("ParentID not set: got %q", rec[0].ParentID)
	}

	// Second snapshot from the same host: Forge's ps scan picks up the
	// same agent (still alive) but its own snapshot does NOT include the
	// ParentID field — the reporter does not know about parent links.
	// Apply must NOT blank the field.
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now.Add(time.Second), Agents: []Agent{
		{
			ID:        "mac-forge:opencode:9000",
			Executor:  "opencode",
			PID:       9000,
			State:     "idle",
			StartedAt: now.Add(-time.Minute),
			// ParentID intentionally absent — reporter snapshot
		},
	}})

	rec = s.Recent()
	if len(rec) != 1 {
		t.Fatalf("expected 1 agent after second snapshot, got %d", len(rec))
	}
	if rec[0].ParentID != "mac-forge:opencode:8500" {
		t.Errorf("ParentID was dropped on snapshot replace: got %q, want %q",
			rec[0].ParentID, "mac-forge:opencode:8500")
	}
}

// TestAgentStore_Apply_ParentIDUpdated confirms ParentID CAN be updated
// when a new snapshot explicitly carries a different value.
func TestAgentStore_Apply_ParentIDUpdated(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()

	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now, Agents: []Agent{
		{ID: "a", Executor: "opencode", State: "active", StartedAt: now, ParentID: "old-parent"},
	}})

	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now.Add(time.Second), Agents: []Agent{
		{ID: "a", Executor: "opencode", State: "active", StartedAt: now, ParentID: "new-parent"},
	}})

	rec := s.Recent()
	if rec[0].ParentID != "new-parent" {
		t.Errorf("ParentID update failed: got %q", rec[0].ParentID)
	}
}

// TestStore_StaleAgents_InvisibleImmediately asserts that when a snapshot
// shrinks, the absent agents become invisible in Recent() on the next call
// without waiting for TTL.
func TestStore_StaleAgents_InvisibleImmediately(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()

	// First snapshot: two agents
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now, Agents: []Agent{
		{ID: "agent-1", Executor: "claude", State: "active", StartedAt: now},
		{ID: "agent-2", Executor: "opencode", State: "idle", StartedAt: now},
	}})

	rec := s.Recent()
	if len(rec) != 2 {
		t.Fatalf("expected 2 agents after first snapshot, got %d", len(rec))
	}

	// Second snapshot: only one agent — agent-2 disappeared
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now.Add(time.Second), Agents: []Agent{
		{ID: "agent-1", Executor: "claude", State: "active", StartedAt: now},
	}})

	rec = s.Recent()
	if len(rec) != 1 {
		t.Fatalf("expected 1 agent after shrink, got %d", len(rec))
	}
	if rec[0].ID != "agent-1" {
		t.Errorf("expected agent-1, got %s", rec[0].ID)
	}
}

// TestStore_StaleAgents_Reappear clears staleness when the agent
// comes back in a later snapshot.
func TestStore_StaleAgents_Reappear(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()

	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now, Agents: []Agent{
		{ID: "agent-1", Executor: "claude", State: "active", StartedAt: now},
	}})
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now.Add(time.Second), Agents: []Agent{}}) // agent-1 goes stale

	rec := s.Recent()
	if len(rec) != 0 {
		t.Fatalf("expected 0 agents while stale, got %d", len(rec))
	}

	// agent-1 comes back
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now.Add(2 * time.Second), Agents: []Agent{
		{ID: "agent-1", Executor: "claude", State: "active", StartedAt: now},
	}})

	rec = s.Recent()
	if len(rec) != 1 {
		t.Fatalf("expected 1 agent after reappear, got %d", len(rec))
	}
}

// TestStore_StaleAgents_Prune removes stale agents.
func TestStore_StaleAgents_Prune(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()

	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now, Agents: []Agent{
		{ID: "agent-1", Executor: "claude", State: "active", StartedAt: now},
	}})
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now.Add(time.Second), Agents: []Agent{}}) // agent-1 goes stale

	if n := s.Prune(); n != 1 {
		t.Errorf("expected prune to remove 1 stale agent, got %d", n)
	}
	if len(s.Recent()) != 0 {
		t.Errorf("expected empty store after prune")
	}
}

// TestStore_StaleAgents_CrossHostIsolation ensures agents from a
// different host are NOT marked stale when the other host's snapshot shrinks.
func TestStore_StaleAgents_CrossHostIsolation(t *testing.T) {
	s := New(time.Minute)
	now := time.Now().UTC()

	// Both hosts have one agent
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now, Agents: []Agent{
		{ID: "forge:agent-1", Executor: "claude", State: "active", StartedAt: now},
	}})
	s.Apply(Snapshot{Host: "vps-kitt", TakenAt: now, Agents: []Agent{
		{ID: "kitt:agent-1", Executor: "kitt", State: "active", StartedAt: now},
	}})

	// mac-forge shrinks — only its agent should go stale
	s.Apply(Snapshot{Host: "mac-forge", TakenAt: now.Add(time.Second), Agents: []Agent{}})

	rec := s.Recent()
	if len(rec) != 1 {
		t.Fatalf("expected 1 agent (kitt's) after forge shrunk, got %d", len(rec))
	}
	if rec[0].ID != "kitt:agent-1" {
		t.Errorf("expected kitt:agent-1, got %s", rec[0].ID)
	}
}

// TestStore_Prune_UpsertConcurrency verifies that concurrent Upsert and Prune
// operations do not race under the store's mutex protection. Uses a small
// shared key set so goroutines contend on the same map entries.
func TestStore_Prune_UpsertConcurrency(t *testing.T) {
	s := New(50 * time.Millisecond)
	now := time.Now().UTC()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Small shared key set — agents 0-4 repeatedly written by all goroutines
	// so they contend on the same map entries.
	const keyCount = 5

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					key := idx % keyCount
					s.Apply(Snapshot{
						Host:    "host-0",
						TakenAt: now,
						Agents: []Agent{
							{ID: fmt.Sprintf("agent-%d", key), Executor: "claude", State: "active", StartedAt: now},
						},
					})
				}
			}
		}(i)
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.Prune()
				}
			}
		}()
	}

	// Let them run for a bit
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	// No panic or race detector error means success
}
