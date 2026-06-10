// Package e2e holds the Hermes walking-skeleton smoke tests.
package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/envelope"
)

// TestFullPath_SessionIO proves the complete RUNBOOK demo path:
// POST envelope → worker delivers → Forge spawns process → write task to session stdin → read echoed output → update status to done.
//
// Worldview: composite proof for W-F3 (launch), W-F4 (read), W-F5 (write), W-F6 (real process), W-H4 (reliable delivery), W-H5 (truthful confirm), W-H15 (done requires proof), W-H6 (status reporting).
func TestFullPath_SessionIO(t *testing.T) {
	env := setupTestEnv(t)
	id := "env-fullpath"

	delivered := createEnvelopeAndWaitForDelivery(t, env, id)
	sessionID := getSessionBindingOrFatal(t, delivered)
	writeTaskToSession(t, env.forgeURL, sessionID, "execute task: full path session IO test\n")
	verifyEchoedOutput(t, env.forgeURL, sessionID, "execute task: full path session IO test")
	transitionToDoneWithProof(t, env, id, map[string]string{"output_hash": "sha256:abc123"})
	verifyFinalStateViaHTTP(t, env.hermesSrv.URL, id, map[string]string{"output_hash": "sha256:abc123"})
}

func createEnvelopeAndWaitForDelivery(t *testing.T, env *testEnv, id string) *envelope.Envelope {
	payload, err := json.Marshal(map[string]any{
		"id":              id,
		"created_by":      "kitt",
		"title":           "full path session IO test",
		"task_title":      "full path session IO test",
		"target_executor": "opencode",
		"proof_required":  []string{"output_hash"},
	})
	if err != nil {
		t.Fatalf("marshal envelope payload: %v", err)
	}
	resp := postEnvelope(t, env.hermesSrv.URL, string(payload))
	defer resp.Body.Close()
	assertStatusCreated(t, resp)
	return env.waitForStatus(env.ctx, t, id, envelope.StatusDelivered)
}

func getSessionBindingOrFatal(t *testing.T, env *envelope.Envelope) string {
	if env.SessionBinding == nil || *env.SessionBinding == "" {
		t.Fatal("expected session_binding populated after delivery")
	}
	return *env.SessionBinding
}

func writeTaskToSession(t *testing.T, forgeURL, sessionID, task string) {
	taskMsg, err := json.Marshal(map[string]string{"input": strings.TrimSuffix(task, "\n") + "\n"})
	if err != nil {
		t.Fatalf("marshal task input: %v", err)
	}
	writeResp, err := http.Post(forgeURL+"/sessions/"+sessionID+"/input", "application/json", bytes.NewBuffer(taskMsg))
	if err != nil {
		t.Fatalf("write to session: %v", err)
	}
	defer writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(writeResp.Body)
		t.Fatalf("write: expected 200, got %d: %s", writeResp.StatusCode, string(body))
	}
}

func verifyEchoedOutput(t *testing.T, forgeURL, sessionID, expected string) {
	output := pollForOutput(t, forgeURL, sessionID)
	if !strings.Contains(output, expected) {
		t.Fatalf("expected task echoed back, got %q", output)
	}
}

func pollForOutput(t *testing.T, forgeURL, sessionID string) string {
	t.Helper()
	var output string
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		out := readSessionOutput(t, forgeURL, sessionID)
		output += out
		if strings.Contains(output, "execute task:") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return output
}

func readSessionOutput(t *testing.T, forgeURL, sessionID string) string {
	t.Helper()
	readResp, err := http.Get(forgeURL + "/sessions/" + sessionID + "/output")
	if err != nil {
		t.Fatalf("read from session: %v", err)
	}
	if readResp.StatusCode != http.StatusOK {
		readResp.Body.Close()
		t.Fatalf("read session output: expected 200, got %d", readResp.StatusCode)
	}
	var readBody struct {
		Output string `json:"output"`
	}
	if err := json.NewDecoder(readResp.Body).Decode(&readBody); err != nil {
		readResp.Body.Close()
		t.Fatalf("decode session output: %v", err)
	}
	readResp.Body.Close()
	return readBody.Output
}

func transitionToDoneWithProof(t *testing.T, env *testEnv, id string, proof map[string]string) {
	done, err := env.hermesStore.UpdateStatus(env.ctx, id, envelope.StatusDone, proof, "task completed with output")
	if err != nil {
		t.Fatalf("done transition: %v", err)
	}
	if done.Status != envelope.StatusDone {
		t.Fatalf("expected done, got %q", done.Status)
	}
	if done.Proof["output_hash"] != proof["output_hash"] {
		t.Fatalf("expected proof %q, got %v", proof["output_hash"], done.Proof)
	}
}

func verifyFinalStateViaHTTP(t *testing.T, hermesURL, id string, expectedProof map[string]string) {
	t.Helper()
	finalEnv := fetchEnvelope(t, hermesURL, id)
	if finalEnv.Status != envelope.StatusDone {
		t.Fatalf("HTTP GET: expected done, got %q", finalEnv.Status)
	}
	if finalEnv.Proof["output_hash"] != expectedProof["output_hash"] {
		t.Fatalf("HTTP GET: expected proof %q, got %v", expectedProof["output_hash"], finalEnv.Proof)
	}
	if finalEnv.Metrics.CompletedAt == nil {
		t.Fatal("HTTP GET: expected completed_at set")
	}
}

func fetchEnvelope(t *testing.T, hermesURL, id string) *envelope.Envelope {
	t.Helper()
	getResp, err := http.Get(hermesURL + "/envelopes/" + id)
	if err != nil {
		t.Fatalf("GET envelope: %v", err)
	}
	if getResp.StatusCode != http.StatusOK {
		getResp.Body.Close()
		t.Fatalf("GET envelope: expected 200, got %d", getResp.StatusCode)
	}
	var env envelope.Envelope
	if err := json.NewDecoder(getResp.Body).Decode(&env); err != nil {
		getResp.Body.Close()
		t.Fatalf("decode envelope: %v", err)
	}
	getResp.Body.Close()
	return &env
}
