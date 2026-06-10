package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SECTION: helpers

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeTG returns a *telegram wired to a test HTTP server.
// The server records the last received body and always returns 200.
type fakeCapture struct {
	sent []string
}

func newFakeTelegram(cap *fakeCapture) (*telegram, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		cap.sent = append(cap.sent, body["text"])
		w.WriteHeader(http.StatusOK)
	}))
	tg := newTelegram("token", "chat123", "")
	tg.baseURL = srv.URL
	tg.client = srv.Client()
	return tg, srv
}

// SECTION: TestFormatMessage — preserve existing coverage

func TestFormatMessage(t *testing.T) {
	cases := []struct {
		p    payload
		want string
	}{
		{payload{Status: "done", TaskTitle: "Fix auth", ProofSummary: "commit=abc"}, "✅ Fix auth\ncommit=abc"},
		{payload{Status: "blocked", TaskTitle: "Fix auth", Note: "waiting"}, "🚫 BLOCKED: Fix auth\nwaiting"},
		{payload{Status: "failed", TaskTitle: "Fix auth", Note: "crash"}, "❌ FAILED: Fix auth\ncrash"},
		{payload{Status: "loop_detected", TaskTitle: "Fix auth", Note: "4x blocked"}, "🔄 LOOP: Fix auth\n4x blocked"},
		{payload{Status: "blocked", TaskTitle: "Fix auth", Note: "loop", LoopDetected: true}, "🔄 LOOP: Fix auth\nloop"},
		{payload{Status: "awaiting_confirm", TaskTitle: "Fix auth", Note: "confirm?"}, "❓ Fix auth\nconfirm?"},
		{payload{Status: "delivered", TaskTitle: "Fix auth"}, "📨 delivered: Fix auth"},
	}
	for _, tc := range cases {
		got := formatMessage(tc.p)
		if got != tc.want {
			t.Errorf("formatMessage(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// SECTION: TestPriority

func TestPriority(t *testing.T) {
	cases := []struct{ status, want string }{
		{"blocked", "urgent"},
		{"failed", "urgent"},
		{"loop_detected", "urgent"},
		{"done", "normal"},
		{"awaiting_confirm", "normal"},
		{"paused", "low"},
		{"unknown_status", "low"},
	}
	for _, tc := range cases {
		got := priority(tc.status)
		if got != tc.want {
			t.Errorf("priority(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// SECTION: TestRouterUrgentAlwaysSends

func TestRouterUrgentAlwaysSends(t *testing.T) {
	cap := &fakeCapture{}
	tg, srv := newFakeTelegram(cap)
	defer srv.Close()

	rt := newRouter(discardLogger(), tg, nil)
	rt.SetBusy("meeting")

	p := payload{Status: "blocked", TaskTitle: "Auth broke", Note: "DB down"}
	body, _ := json.Marshal(p)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	rt.handleWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(cap.sent) != 1 {
		t.Fatalf("expected 1 telegram message, got %d", len(cap.sent))
	}
	if !strings.Contains(cap.sent[0], "BLOCKED") {
		t.Errorf("message should contain BLOCKED, got %q", cap.sent[0])
	}
	if len(rt.buffer) != 0 {
		t.Errorf("urgent should not be buffered, buffer len=%d", len(rt.buffer))
	}
}

// SECTION: TestRouterNormalBufferedWhenBusy

func TestRouterNormalBufferedWhenBusy(t *testing.T) {
	cap := &fakeCapture{}
	tg, srv := newFakeTelegram(cap)
	defer srv.Close()

	rt := newRouter(discardLogger(), tg, nil)
	rt.SetBusy("deep work")

	p := payload{Status: "done", TaskTitle: "Tests pass"}
	body, _ := json.Marshal(p)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	rt.handleWebhook(rec, req)

	if len(cap.sent) != 0 {
		t.Errorf("expected no telegram send while busy, got %d", len(cap.sent))
	}
	rt.mu.Lock()
	bufLen := len(rt.buffer)
	rt.mu.Unlock()
	if bufLen != 1 {
		t.Errorf("expected 1 item in buffer, got %d", bufLen)
	}
}

// SECTION: TestRouterFlushSendsBatch

func TestRouterFlushSendsBatch(t *testing.T) {
	cap := &fakeCapture{}
	tg, srv := newFakeTelegram(cap)
	defer srv.Close()

	rt := newRouter(discardLogger(), tg, nil)
	rt.SetBusy("focus")

	for i := 0; i < 3; i++ {
		rt.addToBuffer(payload{Status: "done", TaskTitle: "Task", ProofSummary: "ok"})
	}

	rt.Flush()

	if len(cap.sent) != 1 {
		t.Fatalf("expected 1 batch message, got %d", len(cap.sent))
	}
	if !strings.Contains(cap.sent[0], "Пропущені нотифікації") {
		t.Errorf("batch message should contain header, got %q", cap.sent[0])
	}
}

// SECTION: TestRouterFlushSingleItem

func TestRouterFlushSingleItem(t *testing.T) {
	cap := &fakeCapture{}
	tg, srv := newFakeTelegram(cap)
	defer srv.Close()

	rt := newRouter(discardLogger(), tg, nil)
	rt.addToBuffer(payload{Status: "done", TaskTitle: "Solo task", ProofSummary: "done"})
	rt.Flush()

	if len(cap.sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(cap.sent))
	}
	if !strings.Contains(cap.sent[0], "Solo task") {
		t.Errorf("expected task title in message, got %q", cap.sent[0])
	}
}

// SECTION: TestLLMSummarize

func TestLLMSummarize(t *testing.T) {
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)

		// Verify request shape.
		if req["model"] == nil {
			t.Error("expected model in request")
		}
		msgs, _ := req["messages"].([]any)
		if len(msgs) != 2 {
			t.Errorf("expected 2 messages, got %d", len(msgs))
		}

		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "✅ 3 задачі виконано"}},
			},
		})
	}))
	defer fakeLLM.Close()

	llm := newLLMClient(fakeLLM.URL, "key", "test-model")
	llm.client = fakeLLM.Client()

	items := []payload{
		{Status: "done", TaskTitle: "Task A"},
		{Status: "done", TaskTitle: "Task B"},
		{Status: "done", TaskTitle: "Task C"},
	}
	summary, err := llm.Summarize(context.Background(), items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "✅ 3 задачі виконано" {
		t.Errorf("unexpected summary: %q", summary)
	}
}

// SECTION: TestLLMFallback

func TestLLMFallback(t *testing.T) {
	rt := newRouter(discardLogger(), nil, nil) // nil LLM

	items := []payload{
		{Status: "done", TaskTitle: "Fix auth", ProofSummary: "commit=abc"},
		{Status: "paused", TaskTitle: "Update tests"},
	}
	result := rt.formatBatch(items)

	if !strings.Contains(result, "Пропущені нотифікації (2)") {
		t.Errorf("expected bullet list header, got %q", result)
	}
	if !strings.Contains(result, "Fix auth") {
		t.Errorf("expected task title in fallback, got %q", result)
	}
}

// SECTION: TestRouterAutoFreeAfterTimeout

func TestRouterAutoFreeAfterTimeout(t *testing.T) {
	cap := &fakeCapture{}
	tg, srv := newFakeTelegram(cap)
	defer srv.Close()

	rt := newRouter(discardLogger(), tg, nil)
	rt.mu.Lock()
	rt.busy = true
	rt.busySince = time.Now().Add(-3 * time.Hour) // simulate 3h ago
	rt.mu.Unlock()

	// Add an item so flush sends a message.
	rt.addToBuffer(payload{Status: "done", TaskTitle: "Old task"})

	// Simulate one autoFlush tick manually.
	rt.mu.Lock()
	busy := rt.busy
	since := rt.busySince
	rt.mu.Unlock()

	if busy && time.Since(since) > 2*time.Hour {
		rt.SetFree()
	}

	if rt.Mode() != "free" {
		t.Errorf("expected free after 2h timeout, got %s", rt.Mode())
	}
	if len(cap.sent) != 1 {
		t.Errorf("expected 1 message after auto-free flush, got %d", len(cap.sent))
	}
}

// SECTION: TestHandlerBadJSON — preserve fire-and-forget behavior

func TestHandlerBadJSON(t *testing.T) {
	rt := newRouter(discardLogger(), nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{bad json"))
	rec := httptest.NewRecorder()
	rt.handleWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (fire-and-forget), got %d", rec.Code)
	}
}

// SECTION: TestTelegramCommands

func TestTelegramCommands(t *testing.T) {
	updateID := int64(100)
	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": []map[string]any{
					{
						"update_id": updateID,
						"message":   map[string]any{"text": "/busy meeting"},
					},
				},
			})
			updateID++ // next poll returns nothing to stop looping
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeAPI.Close()

	tg := newTelegram("token", "chat", "")
	tg.baseURL = fakeAPI.URL
	tg.client = fakeAPI.Client()

	var mu sync.Mutex
	var received []string
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go tg.PollUpdates(ctx, func(cmd, text string) {
		mu.Lock()
		received = append(received, cmd+":"+text)
		mu.Unlock()
	})

	<-ctx.Done()

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Error("expected at least one command from poll")
	}
	if received[0] != "busy:meeting" {
		t.Errorf("expected busy:meeting, got %q", received[0])
	}
}
