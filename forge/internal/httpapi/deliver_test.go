package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/legrin-tech/forge/internal/runner"
	"github.com/legrin-tech/forge/internal/sessionstore"
)

// validDelivery is the smallest JSON body accepted by /deliver.
func validDelivery(deliveryID, envelopeID string) string {
	return `{
		"delivery_id": "` + deliveryID + `",
		"envelope": {
			"id": "` + envelopeID + `",
			"task_title": "run smoke",
			"target_executor": "opencode"
		}
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

func TestDeliver_Created(t *testing.T) {
	// W-F6: ack must carry a session_id (handle existed before we acked).
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-1", "env-1"))

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got deliverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeliveryID != "d-1" || got.EnvelopeID != "env-1" {
		t.Fatalf("unexpected ack: %+v", got)
	}
	if got.SessionID == "" {
		t.Fatalf("expected session_id set, got empty")
	}
	if got.AckedAt == "" {
		t.Fatalf("expected acked_at set")
	}
}

func TestDeliver_Idempotent_SameDeliveryID(t *testing.T) {
	// W-H16: re-POST of same delivery_id returns prior ack untouched.
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-dup", "env-dup"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first: expected 201, got %d", rec.Code)
	}
	var first deliverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)

	rec = do(t, srv, http.MethodPost, "/deliver", validDelivery("d-dup", "env-dup"))
	if rec.Code != http.StatusOK {
		t.Fatalf("replay: expected 200, got %d", rec.Code)
	}
	var second deliverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)

	if first != second {
		t.Fatalf("replay ack diverged:\n first=%+v\nsecond=%+v", first, second)
	}
}

func TestDeliver_RetryWithNewDeliveryID_ReusesSession(t *testing.T) {
	// W-F2: session register is keyed by envelope_id, so a retry with a
	// fresh delivery_id for the same envelope should reuse the session.
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-a", "env-reuse"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first: expected 201, got %d", rec.Code)
	}
	var first deliverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)

	rec = do(t, srv, http.MethodPost, "/deliver", validDelivery("d-b", "env-reuse"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry: expected 201, got %d", rec.Code)
	}
	var second deliverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)

	if first.SessionID != second.SessionID {
		t.Fatalf("expected same session_id across retries, got %q vs %q",
			first.SessionID, second.SessionID)
	}
	if first.DeliveryID == second.DeliveryID {
		t.Fatalf("expected distinct delivery_ids")
	}
}

func TestDeliver_BadJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", `{not json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"invalid_json"`) {
		t.Fatalf("expected invalid_json kind, got %s", rec.Body.String())
	}
}

// W-F2: GET /sessions returns sessions created by /deliver.
func TestSessions_ListAfterDeliver(t *testing.T) {
	srv, _ := newTestServer(t)

	// Deliver creates a session
	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-sess", "env-sess"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d", rec.Code)
	}

	rec = do(t, srv, http.MethodGet, "/sessions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 session, got %d", len(list))
	}
	if list[0]["envelope_id"] != "env-sess" {
		t.Fatalf("unexpected session: %+v", list[0])
	}
}

func TestSessions_GetByID(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-get", "env-get-sess"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d", rec.Code)
	}
	var ack deliverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)

	rec = do(t, srv, http.MethodGet, "/sessions/"+ack.SessionID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var sess map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if sess["session_id"] != ack.SessionID {
		t.Fatalf("expected %s, got %v", ack.SessionID, sess["session_id"])
	}
}

func TestSessions_GetNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodGet, "/sessions/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestDeliver_SpawnsRealProcess verifies W-F6 upgrade: session_id
// contains a PID, proving a real process was spawned before ack.
func TestDeliver_SpawnsRealProcess(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-proc", "env-proc"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got deliverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Session ID format: session-<envelope_id>-pid-<N>
	if !strings.Contains(got.SessionID, "env-proc") {
		t.Fatalf("expected session_id to contain envelope_id, got %q", got.SessionID)
	}
	// Parse and validate PID is a positive integer (review feedback).
	const marker = "-pid-"
	idx := strings.LastIndex(got.SessionID, marker)
	if idx < 0 {
		t.Fatalf("expected session_id with %q, got %q", marker, got.SessionID)
	}
	pidStr := got.SessionID[idx+len(marker):]
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		t.Fatalf("expected positive PID in session_id, got %q (parsed: %d, err: %v)",
			got.SessionID, pid, err)
	}
}

// TestSessionIO_WriteAndRead proves W-F4 + W-F5 via HTTP endpoints.
// Writes input to a cat session, then reads the echoed output.
func TestSessionIO_WriteAndRead(t *testing.T) {
	srv, _ := newTestServer(t)

	// Deliver to create a session with a running cat process.
	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-io", "env-io"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var ack deliverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}

	// W-F5: write to session stdin.
	inputPayload := `{"input":"hello session\n"}`
	rec = do(t, srv, http.MethodPost, "/sessions/"+ack.SessionID+"/input", inputPayload)
	if rec.Code != http.StatusOK {
		t.Fatalf("write: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var writeResp struct {
		SessionID    string `json:"session_id"`
		BytesWritten int    `json:"bytes_written"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &writeResp); err != nil {
		t.Fatalf("unmarshal write resp: %v", err)
	}
	if writeResp.BytesWritten != 14 {
		t.Fatalf("expected 14 bytes written, got %d", writeResp.BytesWritten)
	}

	// W-F4: poll for echoed output instead of sleeping (review feedback).
	expected := "hello session\n"
	deadline := time.Now().Add(1 * time.Second)
	var collected strings.Builder
	for time.Now().Before(deadline) {
		rec = do(t, srv, http.MethodGet, "/sessions/"+ack.SessionID+"/output", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("read: expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var readResp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &readResp); err != nil {
			t.Fatalf("unmarshal read resp: %v", err)
		}
		collected.WriteString(readResp["output"])
		if collected.String() == expected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if collected.String() != expected {
		t.Fatalf("expected echoed output %q, got %q", expected, collected.String())
	}

	// Subsequent read should be empty (buffer cleared by polling above).
	rec = do(t, srv, http.MethodGet, "/sessions/"+ack.SessionID+"/output", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read2: expected 200, got %d", rec.Code)
	}
	var readResp2 map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &readResp2); err != nil {
		t.Fatalf("unmarshal read2: %v", err)
	}
	if readResp2["output"] != "" {
		t.Fatalf("expected empty output after drain, got %q", readResp2["output"])
	}
}

// TestSessionIO_ReadNotFound returns 404 for unknown session.
func TestSessionIO_ReadNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(t, srv, http.MethodGet, "/sessions/nope/output", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestSessionIO_WriteNotFound returns 404 for unknown session.
func TestSessionIO_WriteNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(t, srv, http.MethodPost, "/sessions/nope/input", `{"input":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestSessionIO_WriteEmptyInput returns 422 for empty input.
func TestSessionIO_WriteEmptyInput(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-empty", "env-empty"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d", rec.Code)
	}
	var ack deliverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)

	rec = do(t, srv, http.MethodPost, "/sessions/"+ack.SessionID+"/input", `{"input":""}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSessionIO_WriteBadJSON returns 400 for malformed JSON.
func TestSessionIO_WriteBadJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-bad", "env-bad"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d", rec.Code)
	}
	var ack deliverResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)

	rec = do(t, srv, http.MethodPost, "/sessions/"+ack.SessionID+"/input", `{not json}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeliver_WorkingDir_PassedToLauncher(t *testing.T) {
	// Review feedback: verify working_dir flows from request to launcher.
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var capturedDir string
	launcher := func(executor, envelopeID, workingDir, _ string, _ bool) (*runner.Process, error) {
		capturedDir = workingDir
		p := runner.New("cat")
		if err := p.Start(); err != nil {
			return nil, err
		}
		return p, nil
	}

	srv := NewServer(discardLogger(), store, launcher, "", "")
	body := `{"delivery_id":"d-wd","envelope":{"id":"e-wd","task_title":"t","target_executor":"claude"},"working_dir":"/tmp/my-project"}`
	rec := do(t, srv, http.MethodPost, "/deliver", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if capturedDir != "/tmp/my-project" {
		t.Fatalf("expected working_dir=/tmp/my-project, got %q", capturedDir)
	}
}

func TestDeliver_WorkingDir_RelativeRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	body := `{"delivery_id":"d-rel","envelope":{"id":"e-rel","task_title":"t","target_executor":"claude"},"working_dir":"relative/path"}`
	rec := do(t, srv, http.MethodPost, "/deliver", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeliver_UnknownExecutor_Returns422 verifies that a launcher rejecting
// an executor permanently (e.g. target=kitt on Mac) produces a 422 so Hermes
// stops retrying in a backoff loop. Regression test for stuck-envelope leak.
func TestDeliver_UnknownExecutor_Returns422(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	launcher := Launcher(func(executor, envelopeID, workingDir, _ string, _ bool) (*runner.Process, error) {
		return nil, ErrUnknownExecutor
	})
	srv := NewServer(discardLogger(), store, launcher, "", "")

	rec := do(t, srv, http.MethodPost, "/deliver", `{
		"delivery_id":"d-unk-1",
		"envelope":{"id":"env-unk-1","task_title":"x","target_executor":"kitt"}
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown_executor") {
		t.Errorf("body should include unknown_executor kind: %s", rec.Body.String())
	}
}

// TestNewExecUUID_ShapeAndUniqueness asserts the helper produces a
// canonical RFC-4122 v4 string and two calls don't collide. Used to
// pin Claude sessions pre-spawn, so a silent regression here would
// break multi-turn resume.
func TestNewExecUUID_ShapeAndUniqueness(t *testing.T) {
	a := newExecUUID()
	b := newExecUUID()
	if a == b {
		t.Fatalf("two successive UUIDs collided: %s", a)
	}
	if len(a) != 36 || a[8] != '-' || a[13] != '-' || a[14] != '4' || a[18] != '-' || a[23] != '-' {
		t.Errorf("not a canonical v4 UUID: %q", a)
	}
}

// TestDeliver_ClaudePinsSessionID verifies that on a fresh delivery to
// target_executor=claude, the launcher receives a non-empty session
// id with resume=false — the contract Forge relies on to push the
// id back to Hermes so subsequent turns can --resume it.
func TestDeliver_ClaudePinsSessionID(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var gotSID string
	var gotResume bool
	launcher := Launcher(func(_, _, _, sid string, resume bool) (*runner.Process, error) {
		gotSID = sid
		gotResume = resume
		p := runner.New("cat")
		if startErr := p.Start(); startErr != nil {
			return nil, startErr
		}
		return p, nil
	})
	srv := NewServer(discardLogger(), store, launcher, "", "")

	rec := do(t, srv, http.MethodPost, "/deliver", `{
		"delivery_id":"d-claude-fresh",
		"envelope":{"id":"env-claude-fresh","task_title":"x","target_executor":"claude"}
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotSID == "" {
		t.Fatal("expected launcher to receive a pinned session_id for claude")
	}
	if gotResume {
		t.Error("expected resume=false for fresh claude spawn")
	}
	if len(gotSID) != 36 { // RFC 4122 length
		t.Errorf("expected 36-char UUID, got %q (len %d)", gotSID, len(gotSID))
	}
}

// TestDeliver_ClaudePushesSessionIDToHermes wires a fake Hermes server
// and asserts Forge pushes the pinned UUID via PATCH /envelopes/.../session
// after a fresh claude spawn — that is what makes multi-turn resume
// possible.
func TestDeliver_ClaudePushesSessionIDToHermes(t *testing.T) {
	patches := make(chan map[string]string, 2)
	hermesFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/session") {
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			b["__path"] = r.URL.Path
			b["__auth"] = r.Header.Get("X-Hermes-Key")
			patches <- b
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hermesFake.Close)

	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	launcher := Launcher(func(_, _, _, _ string, _ bool) (*runner.Process, error) {
		p := runner.New("cat")
		_ = p.Start()
		return p, nil
	})
	srv := NewServer(discardLogger(), store, launcher, hermesFake.URL, "dev-key-forge-test")

	rec := do(t, srv, http.MethodPost, "/deliver", `{
		"delivery_id":"d-push",
		"envelope":{"id":"env-push","task_title":"x","target_executor":"claude"}
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case p := <-patches:
		if p["__auth"] != "dev-key-forge-test" {
			t.Errorf("auth header wrong: %q", p["__auth"])
		}
		if !strings.HasSuffix(p["__path"], "/envelopes/env-push/session") {
			t.Errorf("path: %q", p["__path"])
		}
		if p["executor_session_id"] == "" || len(p["executor_session_id"]) != 36 {
			t.Errorf("session id not a UUID: %q", p["executor_session_id"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hermes PATCH not observed within 3s")
	}
}

// TestDeliver_ClaudeResumePassthrough verifies that a delivery carrying
// envelope.executor_session_id threads it through as resume=true so
// Claude runs --resume instead of pinning a new uuid.
func TestDeliver_ClaudeResumePassthrough(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var gotSID string
	var gotResume bool
	launcher := Launcher(func(_, _, _, sid string, resume bool) (*runner.Process, error) {
		gotSID = sid
		gotResume = resume
		p := runner.New("cat")
		_ = p.Start()
		return p, nil
	})
	srv := NewServer(discardLogger(), store, launcher, "", "")

	rec := do(t, srv, http.MethodPost, "/deliver", `{
		"delivery_id":"d-claude-resume",
		"envelope":{"id":"env-claude-resume","task_title":"x","target_executor":"claude","executor_session_id":"prev-uuid-xyz"}
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotSID != "prev-uuid-xyz" {
		t.Errorf("expected session_id passthrough, got %q", gotSID)
	}
	if !gotResume {
		t.Error("expected resume=true when envelope carries executor_session_id")
	}
}

func TestDeliver_MissingFields(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"no delivery_id", `{"envelope":{"id":"e","target_executor":"x"}}`},
		{"no envelope id", `{"delivery_id":"d","envelope":{"target_executor":"x"}}`},
		{"no executor", `{"delivery_id":"d","envelope":{"id":"e"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			rec := do(t, srv, http.MethodPost, "/deliver", tc.payload)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"error":"invalid_delivery"`) {
				t.Fatalf("expected invalid_delivery kind, got %s", rec.Body.String())
			}
		})
	}
}

// TestSessionIO_ReadOutput_Tail verifies GET /sessions/{id}/output?tail=N
// returns 200 for a live session and 404 for an unknown one (CON-003).
// The ReadOutputTail unit tests in runner_test.go cover the actual tail
// content logic; this test covers HTTP routing and status codes.
func TestSessionIO_ReadOutput_Tail(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-tail", "env-tail"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var ack deliverResponse
	json.Unmarshal(rec.Body.Bytes(), &ack)

	// Write one line and poll until cat echoes it back (confirms process alive).
Deadline:
	for i := 0; i < 100; i++ {
		do(t, srv, http.MethodPost, "/sessions/"+ack.SessionID+"/input", `{"input":"x\n"}`)
		for j := 0; j < 20; j++ {
			rec = do(t, srv, http.MethodGet, "/sessions/"+ack.SessionID+"/output?tail=5", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("tail=5: expected 200, got %d", rec.Code)
			}
			var resp map[string]string
			json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp["output"] != "" {
				break Deadline
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["output"] == "" {
		t.Fatal("cat never echoed input after multiple attempts")
	}

	// Unknown session with tail param returns 404.
	rec = do(t, srv, http.MethodGet, "/sessions/nope/output?tail=5", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: expected 404, got %d", rec.Code)
	}
}

// TestSessionIO_ReadOutput_TailFallsBack tests that invalid/empty tail
// query values fall back to 20 (CON-003).
func TestSessionIO_ReadOutput_TailFallsBack(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/deliver", validDelivery("d-tailfb", "env-tailfb"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d", rec.Code)
	}
	var ack deliverResponse
	json.Unmarshal(rec.Body.Bytes(), &ack)

	do(t, srv, http.MethodPost, "/sessions/"+ack.SessionID+"/input", `{"input":"line1\nline2\nline3\n"}`)
	time.Sleep(50 * time.Millisecond)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"empty", "/sessions/" + ack.SessionID + "/output?tail="},
		{"invalid", "/sessions/" + ack.SessionID + "/output?tail=abc"},
		{"zero", "/sessions/" + ack.SessionID + "/output?tail=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each should return 200 (handler never errors on bad param).
			r := do(t, srv, http.MethodGet, tc.query, "")
			if r.Code != http.StatusOK {
				t.Errorf("expected 200, got %d: %s", r.Code, r.Body.String())
			}
		})
	}
}

// TestSessionIO_ReadOutput_NotFound stays 404 for unknown sessions.
func TestSessionIO_ReadOutput_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(t, srv, http.MethodGet, "/sessions/nope/output?tail=5", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestDeliver_ConcurrentDeliverResume_NoDuplicateSpawn verifies that concurrent
// /deliver and /sessions/{id}/resume for the same dead envelope do not both spawn.
// This is the regression test for the deliver/resume spawn-gate race.
func TestDeliver_ConcurrentDeliverResume_NoDuplicateSpawn(t *testing.T) {
	// This test requires a shared spawn gate between deliver and resume handlers.
	// Without the fix, both handlers can spawn concurrently for the same envelope.
	var spawnCount int
	var spawnMu sync.Mutex

	openCodeMux := http.NewServeMux()
	openCodeMux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"id": "oc-shared-session", "title": "env-concurrent"}})
	})
	openCodeMux.HandleFunc("POST /session/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	openCodeServer := httptest.NewServer(openCodeMux)
	defer openCodeServer.Close()

	dsn := filepath.Join(t.TempDir(), "concurrent_spawn.db")
	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	registry := newProcessRegistry(discardLogger())

	launcher := Launcher(func(executor, envelopeID, workingDir, _ string, _ bool) (*runner.Process, error) {
		spawnMu.Lock()
		spawnCount++
		spawnMu.Unlock()
		p := runner.New("cat")
		if err := p.Start(); err != nil {
			return nil, err
		}
		return p, nil
	})

	// Insert a dead session (no process in registry)
	now := time.Now()
	sess := &sessionstore.Session{
		SessionID:  "session-env-concurrent-pid-9999",
		EnvelopeID: "env-concurrent",
		Executor:   "opencode",
		WorkingDir: "",
		State:      "lost",
		StartedAt:  now,
		LastSeenAt: now,
	}
	if err := store.Insert(context.Background(), sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Build server with shared registry and openCodeURL pointing to fake server
	testMux := http.NewServeMux()
	d := newDeliverHandlerWithHermes(discardLogger(), store, launcher, registry, nil, openCodeServer.URL)
	d.register(testMux)
	rh := newResumeHandler(discardLogger(), store, registry, launcher, openCodeServer.URL)
	rh.register(testMux)
	httpSrv := httptest.NewServer(testMux)
	defer httpSrv.Close()

	// Fire concurrent deliver + resume for the same envelope
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		body := validDelivery("d-concurrent-1", "env-concurrent")
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/deliver", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	go func() {
		defer wg.Done()
		body := `{"message":"resume now"}`
		req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/sessions/env-concurrent/resume", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	wg.Wait()

	spawnMu.Lock()
	count := spawnCount
	spawnMu.Unlock()

	if count > 1 {
		t.Fatalf("expected at most 1 spawn, got %d — duplicate spawn race detected", count)
	}
}

// TestDiscoverOpenCodeSessionID_MatchesEnvelopeTitle verifies that session discovery
// binds by the explicit OpenCode --title value, not by global session count.
func TestDiscoverOpenCodeSessionID_MatchesEnvelopeTitle(t *testing.T) {
	// Server returns multiple sessions; only the one titled with the envelope id should bind.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{
			{"id": "session-a", "title": "other-envelope"},
			{"id": "session-b", "title": "env-bind"},
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := discoverOpenCodeSessionIDForEnvelope(ctx, srv.URL, discardLogger(), "env-bind", "")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got != "session-b" {
		t.Fatalf("expected session-b, got %q", got)
	}
}

func TestDiscoverOpenCodeSessionIDFromOutput_ParsesJSONEventSessionID(t *testing.T) {
	output := []byte(`{"type":"step_start","timestamp":1778258544804,"sessionID":"ses_1f7880a3dffejU1LaTN296I2BL"}
{"type":"text","sessionID":"ses_1f7880a3dffejU1LaTN296I2BL","part":{"type":"text","text":"OK"}}
`)

	got, ok := discoverOpenCodeSessionIDFromOutput(output)
	if !ok {
		t.Fatal("expected session id in opencode json output")
	}
	if got != "ses_1f7880a3dffejU1LaTN296I2BL" {
		t.Fatalf("session id mismatch: got %q", got)
	}
}

func TestDiscoverOpenCodeSessionIDFromOutput_IgnoresNonJSONAndMissingID(t *testing.T) {
	output := []byte("noise\n{\"type\":\"text\"}\n")

	if got, ok := discoverOpenCodeSessionIDFromOutput(output); ok {
		t.Fatalf("expected no session id, got %q", got)
	}
}

func TestDiscoverOpenCodeSessionIDFromOutput_RejectsEmbeddedRawMarker(t *testing.T) {
	output := []byte("prefix before json {\"sessionID\":\"ses_noisy\",\"type\":\"text\"}")

	if got, ok := discoverOpenCodeSessionIDFromOutput(output); ok {
		t.Fatalf("expected embedded raw marker to be ignored, got %q", got)
	}
}

func TestAggregateDiscoveryErrors_PreservesBothCauses(t *testing.T) {
	err := aggregateDiscoveryErrors(map[string]error{
		"process": fmt.Errorf("process output missing session id"),
		"api":     fmt.Errorf("API timeout"),
	})
	if err == nil {
		t.Fatal("expected aggregate error")
	}
	got := err.Error()
	if !strings.Contains(got, "process output missing session id") || !strings.Contains(got, "API timeout") {
		t.Fatalf("expected both discovery causes, got %q", got)
	}
}

func TestDiscoverOpenCodeSessionID_RejectsDuplicateEnvelopeTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{
			{"id": "session-a", "title": "env-dup"},
			{"id": "session-b", "title": "env-dup"},
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := discoverOpenCodeSessionIDForEnvelope(ctx, srv.URL, discardLogger(), "env-dup", "")
	if err == nil {
		t.Fatal("expected error when duplicate titled sessions exist, got nil")
	}
}

func TestMatchingOpenCodeSessions_FiltersDirectory(t *testing.T) {
	sessions := []openCodeSession{
		{ID: "session-a", Title: "env-dir", Directory: "/tmp/other"},
		{ID: "session-b", Title: "env-dir", Directory: "/tmp/project"},
	}

	matches := matchingOpenCodeSessions(sessions, "env-dir", "/tmp/project")
	if len(matches) != 1 || matches[0].ID != "session-b" {
		t.Fatalf("expected session-b match, got %+v", matches)
	}
}

type alreadyDoneProcess struct{}

func (alreadyDoneProcess) ReadOutputTail(int) []byte { return nil }
func (alreadyDoneProcess) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestDiscoverAndStoreSessionID_NoFallbackPatchOnDiscoveryFailure(t *testing.T) {
	patches := 0
	hermes := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hermes.Close()

	h := &deliverHandler{
		logger:      discardLogger(),
		hermes:      newHermesClient(hermes.URL, ""),
		openCodeURL: "http://127.0.0.1:1",
	}

	h.discoverAndStoreSessionID("env-fallback", "", alreadyDoneProcess{})

	if patches != 0 {
		t.Fatalf("expected no fallback executor session patch on discovery failure, got %d", patches)
	}
}
