// Package e2e holds the Hermes walking-skeleton smoke tests.
package e2e_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/legrin-tech/hermes/envelope"
)

// TestEscalation_BlockedAndResume proves the full escalation round-trip:
// created → delivered (worker) → blocked (note) → list discovers it → in_progress (clarified) → done (proof).
//
// Worldview: composite proof for W-H6 (list-by-status), W-H15 (done requires proof), W-H17 (terminal stays terminal).
func TestEscalation_BlockedAndResume(t *testing.T) {
	env := setupTestEnv(t)
	id := "env-esc"

	delivered := createAndWaitForDelivered(t, env, id)
	if delivered.Status != envelope.StatusDelivered {
		t.Fatalf("envelope never reached delivered; last status=%q", delivered.Status)
	}

	transitionToBlocked(t, env, id, "missing API credentials from ops team")
	verifyBlockedInList(t, env, id, "missing API credentials")
	transitionToInProgress(t, env, id, "credentials received, resuming")
	transitionToDone(t, env, id, map[string]string{"commit_hash": "abc123"})
	verifyTerminalCannotTransition(t, env, id)
	verifyHistoryCount(t, env, id, 3)
}

func createAndWaitForDelivered(t *testing.T, env *testEnv, id string) *envelope.Envelope {
	payload, err := json.Marshal(map[string]any{
		"id":              id,
		"created_by":      "kitt",
		"title":           "escalation test",
		"task_title":      "escalation test",
		"target_executor": "opencode",
		"proof_required":  []string{"commit_hash"},
	})
	if err != nil {
		t.Fatalf("marshal envelope payload: %v", err)
	}
	resp := postEnvelope(t, env.hermesSrv.URL, string(payload))
	defer resp.Body.Close()
	assertStatusCreated(t, resp)
	return env.waitForStatus(env.ctx, t, id, envelope.StatusDelivered)
}

func transitionToBlocked(t *testing.T, env *testEnv, id, note string) {
	blocked, err := env.hermesStore.UpdateStatus(env.ctx, id, envelope.StatusBlocked, nil, note)
	if err != nil {
		t.Fatalf("transition to blocked: %v", err)
	}
	if blocked.Status != envelope.StatusBlocked {
		t.Fatalf("expected blocked, got %q", blocked.Status)
	}
}

func verifyBlockedInList(t *testing.T, env *testEnv, id, note string) {
	escalated, err := env.hermesStore.List(env.ctx, []envelope.Status{envelope.StatusBlocked})
	if err != nil {
		t.Fatalf("list blocked: %v", err)
	}
	if len(escalated) != 1 || escalated[0].ID != id {
		t.Fatalf("expected [%s] in blocked list, got %v", id, escalated)
	}
	found := false
	for _, entry := range escalated[0].History {
		if strings.Contains(entry, note) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("blocker note not in history: %v", escalated[0].History)
	}
}

func transitionToInProgress(t *testing.T, env *testEnv, id, note string) {
	resumed, err := env.hermesStore.UpdateStatus(env.ctx, id, envelope.StatusInProgress, nil, note)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Status != envelope.StatusInProgress {
		t.Fatalf("expected in_progress, got %q", resumed.Status)
	}
}

func transitionToDone(t *testing.T, env *testEnv, id string, proof map[string]string) {
	done, err := env.hermesStore.UpdateStatus(env.ctx, id, envelope.StatusDone, proof, "task completed successfully")
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if done.Status != envelope.StatusDone {
		t.Fatalf("expected done, got %q", done.Status)
	}
	if done.Proof["commit_hash"] != proof["commit_hash"] {
		t.Fatalf("expected proof %q, got %v", proof["commit_hash"], done.Proof)
	}
	if done.Metrics.CompletedAt == nil {
		t.Fatal("expected completed_at set for terminal state")
	}
}

func verifyTerminalCannotTransition(t *testing.T, env *testEnv, id string) {
	_, err := env.hermesStore.UpdateStatus(env.ctx, id, envelope.StatusInProgress, nil, "should fail")
	if err == nil {
		t.Fatal("expected error transitioning from terminal state")
	}
}

func verifyHistoryCount(t *testing.T, env *testEnv, id string, expected int) {
	final, err := env.hermesStore.Get(env.ctx, id)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if len(final.History) != expected {
		t.Fatalf("expected %d history entries, got %d: %v", expected, len(final.History), final.History)
	}
}
