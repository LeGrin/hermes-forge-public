package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFormatTelegramMessage covers every branch of the formatter so we
// catch accidental regressions when new statuses are introduced.
func TestFormatTelegramMessage(t *testing.T) {
	cases := []struct {
		name         string
		status       string
		title        string
		note         string
		proof        string
		loopDetected bool
		wantContains []string
	}{
		{"loop", "blocked", "Task X", "stuck", "", true, []string{"🔄 LOOP", "Task X", "stuck"}},
		{"loop_status", "loop_detected", "Task Y", "repeat", "", false, []string{"🔄 LOOP", "Task Y"}},
		{"done", "done", "Task Z", "", "proof-hash", false, []string{"✅", "Task Z", "proof-hash"}},
		{"blocked", "blocked", "Task B", "needs key", "", false, []string{"🚫 BLOCKED", "needs key"}},
		{"failed", "failed", "Task F", "panic", "", false, []string{"❌ FAILED", "panic"}},
		{"awaiting", "awaiting_confirm", "Task Q", "which env?", "", false, []string{"❓", "which env?"}},
		{"session_decision", "session_decision", "Sess T", "claude: go-1.26", "", false, []string{"💡", "Sess T", "go-1.26"}},
		{"default_with_note", "in_progress", "Task I", "halfway", "", false, []string{"📨", "in_progress", "halfway"}},
		{"default_no_note", "created", "Task N", "", "", false, []string{"📨", "created", "Task N"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTelegramMessage(tc.status, tc.title, tc.note, tc.proof, tc.loopDetected)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
		})
	}
}

// TestWebhookFirer_NilSafe exercises the nil-receiver branch so callers can
// use `firer.Fire(...)` without defensive nil checks at every site.
func TestWebhookFirer_NilSafe(t *testing.T) {
	var wf *webhookFirer
	wf.Fire(context.Background(), "id", "title", "done", "note", "proof", false)
}

// TestNewWebhookFirer_NoSinks documents that the builder returns nil when
// there is nothing to configure — the callers rely on that contract.
func TestNewWebhookFirer_NoSinks(t *testing.T) {
	got := newWebhookFirer(discardLogger(), ServerOpts{})
	if got != nil {
		t.Fatalf("expected nil firer, got %#v", got)
	}
}

// TestWebhookFirer_OpenClawSecretMode verifies that when webhookSecret is set
// the firer targets the OpenClaw /v1/chat/completions API shape rather than
// the legacy simple-POST shape.
func TestWebhookFirer_OpenClawSecretMode(t *testing.T) {
	calls := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls <- r
		bodies <- b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	wf := &webhookFirer{
		webhookURL:    srv.URL,
		webhookSecret: "secret-token",
		logger:        discardLogger(),
	}
	wf.Fire(context.Background(), "sub-1", "Title", "done", "done ok", "proof-abc", false)

	select {
	case req := <-calls:
		if got := req.URL.Path; got != "/v1/chat/completions" {
			t.Errorf("path: got %q, want /v1/chat/completions", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("auth header: got %q", got)
		}
		body := <-bodies
		var msg openClawChatRequest
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if msg.Model != "openclaw/worker" {
			t.Errorf("model: got %q", msg.Model)
		}
		if len(msg.Messages) != 1 || !strings.Contains(msg.Messages[0].Content, "sub-1") {
			t.Errorf("unexpected body: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openclaw endpoint not called")
	}
}

// TestWebhookFirer_SimplePayloadShape captures the JSON shape expected by
// hermes-notify. If this test changes, hermes-notify's router.go will also
// need updating.
func TestWebhookFirer_SimplePayloadShape(t *testing.T) {
	payloads := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		payloads <- p
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	wf := &webhookFirer{webhookURL: srv.URL, logger: discardLogger()}
	wf.Fire(context.Background(), "env-9", "Go!", "blocked", "needs help", "", false)

	select {
	case p := <-payloads:
		for _, key := range []string{"envelope_id", "status", "task_title", "note", "proof_summary", "loop_detected"} {
			if _, ok := p[key]; !ok {
				t.Errorf("missing key %q in %#v", key, p)
			}
		}
		if p["status"] != "blocked" || p["task_title"] != "Go!" {
			t.Errorf("fields wrong: %#v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("simple webhook not called")
	}
}

// TestWebhookFirer_UnreachableWebhook ensures that a broken downstream does
// not crash the firer — error is logged, caller continues.
func TestWebhookFirer_UnreachableWebhook(t *testing.T) {
	// URL that will refuse connections: localhost on an unbound port.
	wf := &webhookFirer{webhookURL: "http://127.0.0.1:1", logger: discardLogger()}
	wf.Fire(context.Background(), "e", "t", "done", "", "", false)
	// Give the goroutine a moment to run and fail; we just need it not to panic.
	time.Sleep(50 * time.Millisecond)
}

// TestWebhookFirer_OpenClawError verifies the secret-mode error branch runs
// when the upstream returns a non-200 — the helper logs and returns without
// leaking a goroutine or panicking.
func TestWebhookFirer_OpenClawError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(srv.Close)

	wf := &webhookFirer{webhookURL: srv.URL, webhookSecret: "x", logger: discardLogger()}
	wf.Fire(context.Background(), "e", "t", "done", "", "", false)
	time.Sleep(100 * time.Millisecond)
}

// TestTelegramSend_HTTPError verifies telegramSend surfaces a non-200 as an
// error so the firer's log path can observe it.
func TestTelegramSend_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"no"}`))
	}))
	t.Cleanup(srv.Close)

	prev := telegramAPIBaseURL
	telegramAPIBaseURL = srv.URL
	t.Cleanup(func() { telegramAPIBaseURL = prev })

	err := telegramSend("token", "chat", "", "hi")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should include status: %v", err)
	}
}

func TestTelegramSend_UsesNumericThreadID(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	prev := telegramAPIBaseURL
	telegramAPIBaseURL = srv.URL
	t.Cleanup(func() { telegramAPIBaseURL = prev })

	if err := telegramSend("token", "chat", "12345", "hi"); err != nil {
		t.Fatalf("telegramSend error: %v", err)
	}
	if got, ok := payload["message_thread_id"].(float64); !ok || got != 12345 {
		t.Fatalf("expected numeric message_thread_id=12345, got %#v", payload["message_thread_id"])
	}
}

func TestTelegramSend_InvalidThreadID(t *testing.T) {
	err := telegramSend("token", "chat", "bad-thread", "hi")
	if err == nil {
		t.Fatal("expected error for invalid thread id")
	}
	if !strings.Contains(err.Error(), "invalid telegram thread id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ensure bytes import is kept even if one of the helpers is trimmed later.
var _ = bytes.NewReader
