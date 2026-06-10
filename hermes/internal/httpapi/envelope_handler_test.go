package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/internal/activityhub"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/notifystore"
)

// validPayload is the smallest JSON body that passes Envelope.Validate.
func validPayload(id string) string {
	return `{
		"id": "` + id + `",
		"created_by": "kitt",
		"title": "test envelope",
		"task_title": "run smoke",
		"target_executor": "opencode"
	}`
}

func do(t *testing.T, srv http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

func TestCreateEnvelope_Created(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/envelopes", validPayload("env-1"))

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/envelopes/env-1" {
		t.Fatalf("unexpected Location header: %q", loc)
	}
	// Server-side defaults filled in.
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["status"] != "created" {
		t.Fatalf("expected default status=created, got %v", got["status"])
	}
	if got["created_at"] == "" || got["created_at"] == nil {
		t.Fatalf("expected server-set created_at, got %v", got["created_at"])
	}
}

func TestCreateEnvelope_BadJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/envelopes", `{not json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"invalid_json"`) {
		t.Fatalf("expected invalid_json kind, got %s", rec.Body.String())
	}
}

func TestCreateEnvelope_MissingFields(t *testing.T) {
	// W-H10: schema rejection at HTTP boundary.
	cases := []struct {
		name    string
		payload string
	}{
		{"no id", `{"title":"t","task_title":"smoke","target_executor":"x"}`},
		{"no title", `{"id":"e","task_title":"smoke","target_executor":"x"}`},
		{"no task_title", `{"id":"e","title":"t","target_executor":"x"}`},
		{"no executor", `{"id":"e","title":"t","task_title":"smoke"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			rec := do(t, srv, http.MethodPost, "/envelopes", tc.payload)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"error":"invalid_envelope"`) {
				t.Fatalf("expected invalid_envelope kind, got %s", rec.Body.String())
			}
		})
	}
}

func TestCreateEnvelope_Duplicate(t *testing.T) {
	// W-H16: re-POSTing the same id is a 409, not an update.
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/envelopes", validPayload("env-dup"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first insert: expected 201, got %d", rec.Code)
	}

	rec = do(t, srv, http.MethodPost, "/envelopes", validPayload("env-dup"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second insert: expected 409, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"duplicate_envelope"`) {
		t.Fatalf("expected duplicate_envelope kind, got %s", rec.Body.String())
	}
}

func TestGetEnvelope_OK(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/envelopes", validPayload("env-get"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup POST failed: %d", rec.Code)
	}

	rec = do(t, srv, http.MethodGet, "/envelopes/env-get", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != "env-get" {
		t.Fatalf("expected id=env-get, got %v", got["id"])
	}
}

func TestGetEnvelope_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodGet, "/envelopes/nope", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"not_found"`) {
		t.Fatalf("expected not_found kind, got %s", rec.Body.String())
	}
}

// --- PATCH /envelopes/{id}/status tests (H-006b, W-H15) ---

func TestUpdateStatus_OK(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-patch"))

	rec := do(t, srv, http.MethodPatch, "/envelopes/env-patch/status",
		`{"status":"delivered"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "delivered" {
		t.Fatalf("expected status=delivered, got %v", got["status"])
	}
}

func TestUpdateStatus_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPatch, "/envelopes/nope/status",
		`{"status":"delivered"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateStatus_PublishesActivity locks that an interesting status
// transition (e.g. delivered → in_progress) lands on the activity hub
// so the dashboard Activity tab stays live without an executor having
// to call hermes_report_activity explicitly.
func TestUpdateStatus_PublishesActivity(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notify, err := notifystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open notify: %v", err)
	}
	hub := activityhub.New()
	srv := NewServer(discardLogger(), store, nil, notify, nil, ServerOpts{Activity: hub})

	sub := hub.Subscribe("") // admin view — sees all
	defer sub.Close()

	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-activity"))
	do(t, srv, http.MethodPatch, "/envelopes/env-activity/status", `{"status":"delivered"}`)
	do(t, srv, http.MethodPatch, "/envelopes/env-activity/status", `{"status":"in_progress"}`)

	// in_progress is an "interesting" status; hub should have an entry.
	select {
	case evt := <-sub.Events():
		if evt.EnvelopeID != "env-activity" {
			t.Errorf("expected envelope_id=env-activity, got %q", evt.EnvelopeID)
		}
		if evt.Kind != "status" {
			t.Errorf("expected kind=status, got %q", evt.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("expected an activity event after status transition")
	}
}

func TestUpdateStatus_TerminalReject(t *testing.T) {
	// W-H17: terminal state cannot transition.
	srv, _ := newTestServer(t)

	// Create envelope with proof_required so we can reach done.
	do(t, srv, http.MethodPost, "/envelopes",
		`{"id":"env-term","title":"t","task_title":"t","target_executor":"x","proof_required":[]}`)

	// Move to done (empty proof_required → allowed).
	rec := do(t, srv, http.MethodPatch, "/envelopes/env-term/status",
		`{"status":"done"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("transition to done: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Try to transition out of done.
	rec = do(t, srv, http.MethodPatch, "/envelopes/env-term/status",
		`{"status":"in_progress"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"invalid_transition"`) {
		t.Fatalf("expected invalid_transition, got %s", rec.Body.String())
	}
}

func TestUpdateStatus_DoneWithoutProof(t *testing.T) {
	// W-H15: done requires proof for each proof_required key.
	srv, _ := newTestServer(t)

	do(t, srv, http.MethodPost, "/envelopes",
		`{"id":"env-proof","title":"t","task_title":"t","target_executor":"x","proof_required":["log_url","screenshot"]}`)

	// Try done without proof.
	rec := do(t, srv, http.MethodPatch, "/envelopes/env-proof/status",
		`{"status":"done"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing") {
		t.Fatalf("expected missing proof detail, got %s", rec.Body.String())
	}

	// Provide partial proof — still rejected.
	rec = do(t, srv, http.MethodPatch, "/envelopes/env-proof/status",
		`{"status":"done","proof":{"log_url":"https://example.com/log"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("partial proof: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}

	// Provide full proof — accepted.
	rec = do(t, srv, http.MethodPatch, "/envelopes/env-proof/status",
		`{"status":"done","proof":{"log_url":"https://example.com/log","screenshot":"https://example.com/ss"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("full proof: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateStatus_BadJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPatch, "/envelopes/any/status", `{bad}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestUpdateStatus_MissingStatus(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPatch, "/envelopes/any/status", `{}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestUpdateStatus_UnknownStatus(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-unk"))

	rec := do(t, srv, http.MethodPatch, "/envelopes/env-unk/status",
		`{"status":"banana"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateStatus_ProofAccumulatesAcrossCalls(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes",
		`{"id":"env-pacc","title":"t","task_title":"t","target_executor":"x","proof_required":["a","b"]}`)

	// First PATCH: add proof key "a", stay at in_progress.
	rec := do(t, srv, http.MethodPatch, "/envelopes/env-pacc/status",
		`{"status":"in_progress","proof":{"a":"val-a"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first patch: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Second PATCH: add proof key "b", transition to done.
	// Key "a" was persisted from first call — should succeed.
	rec = do(t, srv, http.MethodPatch, "/envelopes/env-pacc/status",
		`{"status":"done","proof":{"b":"val-b"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("done with accumulated proof: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	proof := got["proof"].(map[string]any)
	if proof["a"] != "val-a" || proof["b"] != "val-b" {
		t.Fatalf("proof not accumulated: %v", proof)
	}
}

func TestUpdateStatus_NoteAppearsInHistory(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-note"))

	rec := do(t, srv, http.MethodPatch, "/envelopes/env-note/status",
		`{"status":"blocked","note":"waiting for credentials"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	history := got["history"].([]any)
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	entry := history[0].(string)
	if !strings.Contains(entry, "created → blocked") {
		t.Fatalf("history missing transition: %q", entry)
	}
	if !strings.Contains(entry, "waiting for credentials") {
		t.Fatalf("history missing note: %q", entry)
	}
}

// --- PATCH /envelopes/{id}/session tests (v2-001) ---

func TestSetSession_OK(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-sess"))

	rec := do(t, srv, http.MethodPatch, "/envelopes/env-sess/session",
		`{"executor_session_id":"forge-session-123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["ok"] != true {
		t.Fatalf("expected ok=true, got %v", got)
	}

	// Verify the field was actually set.
	rec = do(t, srv, http.MethodGet, "/envelopes/env-sess", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got["executor_session_id"] != "forge-session-123" {
		t.Fatalf("expected executor_session_id=forge-session-123, got %v", got["executor_session_id"])
	}
}

func TestSetSession_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPatch, "/envelopes/nope/session",
		`{"executor_session_id":"sess"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSetSession_MissingSessionID(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-sess-missing"))

	rec := do(t, srv, http.MethodPatch, "/envelopes/env-sess-missing/session", `{}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "executor_session_id is required") {
		t.Fatalf("expected executor_session_id is required, got %s", rec.Body.String())
	}
}

func TestSetSession_EmptySessionID(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-sess-empty"))

	rec := do(t, srv, http.MethodPatch, "/envelopes/env-sess-empty/session",
		`{"executor_session_id":""}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- GET /envelopes tests (H-007b, W-H6) ---

func TestListEnvelopes_Empty(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodGet, "/envelopes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[]") {
		t.Fatalf("expected empty array, got %s", rec.Body.String())
	}
}

func TestListEnvelopes_All(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-a"))
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-b"))

	rec := do(t, srv, http.MethodGet, "/envelopes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(got))
	}
}

func TestListEnvelopes_FilterByStatus(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-c1"))
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-c2"))

	// Move env-c1 to blocked.
	do(t, srv, http.MethodPatch, "/envelopes/env-c1/status",
		`{"status":"blocked","note":"stuck"}`)

	// Filter for blocked only.
	rec := do(t, srv, http.MethodGet, "/envelopes?status=blocked", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 blocked envelope, got %d", len(got))
	}
	if got[0]["id"] != "env-c1" {
		t.Fatalf("expected env-c1, got %v", got[0]["id"])
	}
}

func TestListEnvelopes_MultipleStatuses(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-m1"))
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-m2"))
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-m3"))

	do(t, srv, http.MethodPatch, "/envelopes/env-m1/status", `{"status":"blocked"}`)
	do(t, srv, http.MethodPatch, "/envelopes/env-m2/status", `{"status":"paused"}`)
	// env-m3 stays created.

	rec := do(t, srv, http.MethodGet, "/envelopes?status=blocked,paused", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 envelopes (blocked+paused), got %d", len(got))
	}
}

func TestListEnvelopes_InvalidStatus(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodGet, "/envelopes?status=banana", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestWebhookSimpleMode verifies simple webhook format when no secret is set.
func TestWebhookSimpleMode(t *testing.T) {
	received := make(chan []byte, 1)
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notify, err := notifystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open notify store: %v", err)
	}
	srv := NewServer(discardLogger(), store, nil, notify, nil, ServerOpts{WebhookURL: webhookSrv.URL})

	do(t, srv, http.MethodPost, "/envelopes",
		`{"id":"wh-1","title":"Fix auth bug","task_title":"Fix auth bug","target_executor":"opencode","proof_required":[]}`)
	rec := do(t, srv, http.MethodPatch, "/envelopes/wh-1/status",
		`{"status":"done","note":"task complete"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case body := <-received:
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode webhook: %v", err)
		}
		if payload["envelope_id"] != "wh-1" {
			t.Fatalf("expected envelope_id=wh-1, got %v", payload["envelope_id"])
		}
		if payload["status"] != "done" {
			t.Fatalf("expected status=done, got %v", payload["status"])
		}
		if payload["task_title"] != "Fix auth bug" {
			t.Fatalf("expected task_title, got %v", payload["task_title"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook not received within 2s")
	}
}

// TestWebhookOpenClawMode verifies OpenClaw HTTP API mode (POST /v1/chat/completions)
// with persistent session key when secret is set.
func TestWebhookOpenClawMode(t *testing.T) {
	type capturedReq struct {
		Auth       string
		SessionKey string
		Body       openClawChatRequest
	}
	received := make(chan capturedReq, 1)

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req openClawChatRequest
		_ = json.Unmarshal(body, &req)
		received <- capturedReq{
			Auth:       r.Header.Get("Authorization"),
			SessionKey: r.Header.Get("x-openclaw-session-key"),
			Body:       req,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer webhookSrv.Close()

	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notify, err := notifystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open notify store: %v", err)
	}
	srv := NewServer(discardLogger(), store, nil, notify, nil, ServerOpts{
		WebhookURL:    webhookSrv.URL,
		WebhookSecret: "gw-token-123",
	})

	do(t, srv, http.MethodPost, "/envelopes",
		`{"id":"wh-oc-1","title":"Fix auth bug","task_title":"Fix auth bug","target_executor":"opencode","proof_required":[]}`)
	rec := do(t, srv, http.MethodPatch, "/envelopes/wh-oc-1/status",
		`{"status":"done","note":"task complete"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case got := <-received:
		if got.Auth != "Bearer gw-token-123" {
			t.Fatalf("expected Bearer auth, got %q", got.Auth)
		}
		if got.SessionKey != openClawSessionKey {
			t.Fatalf("expected session key %q, got %q", openClawSessionKey, got.SessionKey)
		}
		if got.Body.Model != "openclaw/worker" {
			t.Fatalf("expected model openclaw/worker, got %q", got.Body.Model)
		}
		if len(got.Body.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got.Body.Messages))
		}
		msg := got.Body.Messages[0].Content
		if !strings.Contains(msg, "wh-oc-1") || !strings.Contains(msg, "status=done") {
			t.Fatalf("unexpected message content: %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openclaw webhook not received within 2s")
	}
}

// --- POST /envelopes/{id}/thread tests (v2-002) ---

func TestAppendMessage_OK(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-thread"))

	rec := do(t, srv, http.MethodPost, "/envelopes/env-thread/thread",
		`{"from":"kitt","kind":"steer","text":"start working"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg["id"] == "" {
		t.Fatal("expected server-assigned id")
	}
	if msg["from"] != "kitt" {
		t.Fatalf("expected from=kitt, got %v", msg["from"])
	}
	if msg["kind"] != "steer" {
		t.Fatalf("expected kind=steer, got %v", msg["kind"])
	}
	if msg["text"] != "start working" {
		t.Fatalf("expected text, got %v", msg["text"])
	}
	if msg["at"] == nil {
		t.Fatal("expected server-assigned at")
	}
}

func TestAppendMessage_WithReplyTo(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-reply"))

	// First message.
	rec1 := do(t, srv, http.MethodPost, "/envelopes/env-reply/thread",
		`{"from":"kitt","kind":"steer","text":"first"}`)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first: expected 201, got %d", rec1.Code)
	}
	var msg1 map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &msg1); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	id, ok := msg1["id"].(string)
	if !ok || id == "" {
		t.Fatalf("expected non-empty id in response, got %v", msg1["id"])
	}
	msg1ID := id

	// Reply to first message.
	rec2 := do(t, srv, http.MethodPost, "/envelopes/env-reply/thread",
		fmt.Sprintf(`{"from":"opencode","kind":"reply","text":"responding","reply_to":%q}`, msg1ID))
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second: expected 201, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var msg2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &msg2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg2["reply_to"] != msg1ID {
		t.Fatalf("expected reply_to=%s, got %v", msg1ID, msg2["reply_to"])
	}
}

func TestAppendMessage_InvalidKind(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-bad-kind"))

	rec := do(t, srv, http.MethodPost, "/envelopes/env-bad-kind/thread",
		`{"from":"kitt","kind":"banana","text":"invalid"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kind must be one of") {
		t.Fatalf("expected kind error, got %s", rec.Body.String())
	}
}

func TestAppendMessage_MissingText(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-no-text"))

	rec := do(t, srv, http.MethodPost, "/envelopes/env-no-text/thread",
		`{"from":"kitt","kind":"steer"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "text is required") {
		t.Fatalf("expected text error, got %s", rec.Body.String())
	}
}

func TestAppendMessage_MissingFrom(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-no-from"))

	rec := do(t, srv, http.MethodPost, "/envelopes/env-no-from/thread",
		`{"kind":"steer","text":"oops"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAppendMessage_EnvelopeNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/envelopes/nope/thread",
		`{"from":"kitt","kind":"steer","text":"orphan"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- GET /envelopes/{id}/thread tests (v2-002) ---

func TestGetThread_OK(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-gt"))

	// Add a message first.
	do(t, srv, http.MethodPost, "/envelopes/env-gt/thread",
		`{"from":"kitt","kind":"steer","text":"hello"}`)

	rec := do(t, srv, http.MethodGet, "/envelopes/env-gt/thread", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var msgs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["text"] != "hello" {
		t.Fatalf("expected text=hello, got %v", msgs[0]["text"])
	}
}

func TestGetThread_WithSinceID(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-gt2"))

	// Add two messages.
	rec1 := do(t, srv, http.MethodPost, "/envelopes/env-gt2/thread",
		`{"from":"kitt","kind":"steer","text":"first"}`)
	var msg1 map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &msg1); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	id, ok := msg1["id"].(string)
	if !ok || id == "" {
		t.Fatalf("expected non-empty id in response, got %v", msg1["id"])
	}

	do(t, srv, http.MethodPost, "/envelopes/env-gt2/thread",
		`{"from":"opencode","kind":"reply","text":"second"}`)

	// Get thread since first message.
	rec := do(t, srv, http.MethodGet, "/envelopes/env-gt2/thread?since_id="+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var msgs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message after since_id, got %d", len(msgs))
	}
	if msgs[0]["text"] != "second" {
		t.Fatalf("expected text=second, got %v", msgs[0]["text"])
	}
}

func TestGetThread_EnvelopeNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodGet, "/envelopes/nope/thread", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetThread_Empty(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-empty-thread"))

	rec := do(t, srv, http.MethodGet, "/envelopes/env-empty-thread/thread", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var msgs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected empty array, got %d", len(msgs))
	}
}
