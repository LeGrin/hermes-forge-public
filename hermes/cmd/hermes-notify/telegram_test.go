package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramSend_UsesNumericThreadID(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tg := newTelegram("token", "chat", "6789")
	tg.baseURL = srv.URL
	tg.client = srv.Client()

	if err := tg.Send("hello"); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if got, ok := payload["message_thread_id"].(float64); !ok || got != 6789 {
		t.Fatalf("expected numeric message_thread_id=6789, got %#v", payload["message_thread_id"])
	}
}

func TestTelegramSend_InvalidThreadID(t *testing.T) {
	tg := newTelegram("token", "chat", "bad")
	if err := tg.Send("hello"); err == nil {
		t.Fatal("expected error for invalid thread id")
	}
}
