package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHermesClient_SetExecutorSessionID_AuthHeader locks in that Forge
// calls Hermes with X-Hermes-Key (not Authorization: Bearer). Hermes's
// auth middleware only reads X-Hermes-Key; using Bearer returns 401 —
// a real production regression caught during T-006 multi-turn smoke.
func TestHermesClient_SetExecutorSessionID_AuthHeader(t *testing.T) {
	var gotHeader string
	var gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Hermes-Key")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newHermesClient(srv.URL, "dev-key-test-forge")
	if err := c.SetExecutorSessionID(context.Background(), "env-123", "sess-abc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if gotHeader != "dev-key-test-forge" {
		t.Errorf("X-Hermes-Key header: got %q, want dev-key-test-forge", gotHeader)
	}
	if !strings.HasSuffix(gotPath, "/envelopes/env-123/session") {
		t.Errorf("path: got %q", gotPath)
	}
	if gotBody["executor_session_id"] != "sess-abc" {
		t.Errorf("body: got %#v", gotBody)
	}
}

// TestHermesClient_SetExecutorSessionID_Non200 ensures non-200
// responses surface as errors — so the "claude session id push failed"
// log actually fires when Hermes rejects the PATCH.
func TestHermesClient_SetExecutorSessionID_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := newHermesClient(srv.URL, "dev-key-test")
	err := c.SetExecutorSessionID(context.Background(), "env-401", "sess-xyz")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should include 401: %v", err)
	}
}
