package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentLink_LinkAndRetrieve(t *testing.T) {
	store := &agentLinkStore{}
	h := &agentLinkHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store}

	mux := http.NewServeMux()
	h.register(mux)

	// Link child1 → parent1
	req, _ := http.NewRequest(http.MethodPost, "/agent/link", bytes.NewBufferString(`{"agent_id":"child1","parent_id":"parent1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first link: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Link child2 → parent2
	req, _ = http.NewRequest(http.MethodPost, "/agent/link", bytes.NewBufferString(`{"agent_id":"child2","parent_id":"parent2"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second link: got %d, want 200", rec.Code)
	}

	// Verify lookups
	if got := store.ParentOf("child1"); got != "parent1" {
		t.Errorf("child1 parent: got %q, want parent1", got)
	}
	if got := store.ParentOf("child2"); got != "parent2" {
		t.Errorf("child2 parent: got %q, want parent2", got)
	}
	if got := store.ParentOf("nonexistent"); got != "" {
		t.Errorf("nonexistent: got %q, want empty", got)
	}
}

func TestAgentLink_OverwriteParent(t *testing.T) {
	store := &agentLinkStore{}
	h := &agentLinkHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store}
	mux := http.NewServeMux()
	h.register(mux)

	// Link child1 → parent1
	req, _ := http.NewRequest(http.MethodPost, "/agent/link", bytes.NewBufferString(`{"agent_id":"child1","parent_id":"parent1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Overwrite with parent2
	req, _ = http.NewRequest(http.MethodPost, "/agent/link", bytes.NewBufferString(`{"agent_id":"child1","parent_id":"parent2"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if got := store.ParentOf("child1"); got != "parent2" {
		t.Errorf("after overwrite: got %q, want parent2", got)
	}
}

func TestAgentLink_MissingFields(t *testing.T) {
	store := &agentLinkStore{}
	h := &agentLinkHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store}
	mux := http.NewServeMux()
	h.register(mux)

	cases := []struct {
		name string
		body string
	}{
		{"empty agent_id", `{"agent_id":"","parent_id":"p"}`},
		{"empty parent_id", `{"agent_id":"c","parent_id":""}`},
		{"missing agent_id", `{"parent_id":"p"}`},
		{"missing parent_id", `{"agent_id":"c"}`},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, "/agent/link", bytes.NewBufferString(c.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d, want 422", c.name, rec.Code)
		}
	}
}

func TestAgentLink_BadJSON(t *testing.T) {
	store := &agentLinkStore{}
	h := &agentLinkHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store}
	mux := http.NewServeMux()
	h.register(mux)

	req, _ := http.NewRequest(http.MethodPost, "/agent/link", bytes.NewBufferString(`{not-json}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: got %d, want 400", rec.Code)
	}
}

func TestAgentLink_MethodNotAllowed(t *testing.T) {
	store := &agentLinkStore{}
	h := &agentLinkHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store}
	mux := http.NewServeMux()
	h.register(mux)

	req, _ := http.NewRequest(http.MethodGet, "/agent/link", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("get: got %d, want 405", rec.Code)
	}
}
