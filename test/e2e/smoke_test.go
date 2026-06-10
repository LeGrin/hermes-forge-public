// Package e2e holds the Hermes walking-skeleton smoke test.
package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/legrin-tech/hermes/envelope"
)

// TestWalkingSkeleton_EnvelopeReachesDelivered proves the core Hermes walking skeleton:
// POST envelope → Hermes store → delivery worker → Forge delivers → Forge ack → Hermes confirms delivered.
//
// Worldview: proof for W-H1 (envelope created), W-H4 (reliable delivery), W-H5 (truthful confirm),
// W-H6 (status reporting), and W-H16 (duplicate rejection via conflict).

func TestWalkingSkeleton_EnvelopeReachesDelivered(t *testing.T) {
	env := setupTestEnv(t)

	payload := `{"id":"env-smoke","created_by":"kitt","title":"walking skeleton smoke","task_title":"walking skeleton smoke","target_executor":"opencode"}`
	resp := postEnvelope(t, env.hermesSrv.URL, payload)
	defer resp.Body.Close()
	assertStatusCreated(t, resp)

	got := env.waitForStatus(env.ctx, t, "env-smoke", envelope.StatusDelivered)
	if got.SessionBinding == nil || *got.SessionBinding == "" {
		t.Fatalf("expected session_binding populated, got %v", got.SessionBinding)
	}
	if !got.Delivery.Delivered || got.Delivery.DeliveredAt == nil {
		t.Fatalf("expected delivery.delivered=true with delivered_at set, got %+v", got.Delivery)
	}

	// W-H16: re-POST with same id should be rejected as duplicate.
	rePost := postEnvelope(t, env.hermesSrv.URL, payload)
	defer rePost.Body.Close()
	if rePost.StatusCode != http.StatusConflict {
		body, err := io.ReadAll(rePost.Body)
		if err != nil {
			t.Fatalf("read conflict response body: %v", err)
		}
		t.Fatalf("expected 409 on duplicate id, got %d: %s", rePost.StatusCode, string(body))
	}
	var errBody map[string]string
	bodyBytes, _ := io.ReadAll(rePost.Body)
	if err := json.Unmarshal(bodyBytes, &errBody); err != nil {
		t.Fatalf("decode error response: %v, body: %s", err, string(bodyBytes))
	}
	if errBody["error"] != "duplicate_envelope" {
		t.Fatalf("expected duplicate_envelope kind, got %+v", errBody)
	}
}
