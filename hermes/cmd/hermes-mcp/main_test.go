package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

func TestToolList(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "") // URL unused for this test
	registerTools(s, c)

	ctx := context.Background()
	resp := s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"tools/list","id":1}`))

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var result struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	expected := map[string]bool{
		"hermes_create_envelope": false,
		"hermes_get_envelope":    false,
		"hermes_list_envelopes":  false,
		"hermes_update_status":   false,
	}
	for _, tool := range result.Result.Tools {
		if _, ok := expected[tool.Name]; ok {
			expected[tool.Name] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("tool %q not found in list", name)
		}
	}
}

// TestToolList_AllSchemasHaveProperties locks in that every registered
// tool carries a `properties` key in its input schema — even no-arg
// tools. OpenAI's function-calling API rejects schemas without
// `properties` with 400 `object schema missing properties`, which
// cascades into the whole KITT agent turn failing and the stables
// router marking the upstream provider degraded. Regression lock for
// the `hermes_list_notifications` / `hermes_list_projects` bug.
func TestToolList_AllSchemasHaveProperties(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	registerTools(s, c)

	ctx := context.Background()
	resp := s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"tools/list","id":1}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var result struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(result.Result.Tools) == 0 {
		t.Fatal("expected at least one tool")
	}

	for _, tool := range result.Result.Tools {
		if _, ok := tool.InputSchema["properties"]; !ok {
			t.Errorf("tool %q inputSchema missing `properties` key (OpenAI will 400): %v",
				tool.Name, tool.InputSchema)
		}
	}
}

func TestGetEnvelope_CallsThroughToHTTP(t *testing.T) {
	envelope := `{"id":"env-1","status":"created","task_title":"test"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/envelopes/env-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(envelope))
	}))
	defer srv.Close()

	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient(srv.URL, "")
	registerTools(s, c)

	// Must initialize first
	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))

	resp := s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"name":"hermes_get_envelope","arguments":{"id":"env-1"}}}`))
	raw, _ := json.Marshal(resp)

	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Result.Content) == 0 {
		t.Fatal("no content in response")
	}
	if result.Result.Content[0].Text != envelope {
		t.Errorf("expected %s, got %s", envelope, result.Result.Content[0].Text)
	}
}

func TestUpdateStatus_BuildsCorrectBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"env-1","status":"done"}`))
	}))
	defer srv.Close()

	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient(srv.URL, "")
	registerTools(s, c)

	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))

	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"tools/call","id":3,"params":{"name":"hermes_update_status","arguments":{"id":"env-1","status":"done","note":"task complete","proof":"{\"commit\":\"abc123\"}"}}}`))

	if gotBody["status"] != "done" {
		t.Errorf("expected status=done, got %v", gotBody["status"])
	}
	if gotBody["note"] != "task complete" {
		t.Errorf("expected note, got %v", gotBody["note"])
	}
	proof, ok := gotBody["proof"].(map[string]any)
	if !ok || proof["commit"] != "abc123" {
		t.Errorf("expected proof.commit=abc123, got %v", gotBody["proof"])
	}
}

func initServer(t *testing.T, url string) (*server.MCPServer, context.Context) {
	t.Helper()
	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient(url, "")
	registerTools(s, c)
	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))
	return s, ctx
}

func callTool(t *testing.T, s *server.MCPServer, ctx context.Context, tool string, args map[string]string) (string, bool) {
	t.Helper()
	argsJSON, _ := json.Marshal(args)
	req := json.RawMessage(`{"jsonrpc":"2.0","method":"tools/call","id":99,"params":{"name":"` + tool + `","arguments":` + string(argsJSON) + `}}`)
	resp := s.HandleMessage(ctx, req)
	raw, _ := json.Marshal(resp)
	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	json.Unmarshal(raw, &result)
	text := ""
	if len(result.Result.Content) > 0 {
		text = result.Result.Content[0].Text
	}
	return text, result.Result.IsError
}

func TestGetEnvelope_MissingID(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_get_envelope", map[string]string{})
	if !isErr {
		t.Error("expected error for missing id")
	}
}

func TestUpdateStatus_MissingID(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_update_status", map[string]string{"status": "done"})
	if !isErr {
		t.Error("expected error for missing id")
	}
}

func TestUpdateStatus_MissingStatus(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_update_status", map[string]string{"id": "env-1"})
	if !isErr {
		t.Error("expected error for missing status")
	}
}

func TestUpdateStatus_InvalidProof(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_update_status", map[string]string{"id": "env-1", "status": "done", "proof": "not-json"})
	if !isErr {
		t.Error("expected error for invalid proof JSON")
	}
}

func TestLogDecision_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"env-1","status":"in_progress"}`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	text, isErr := callTool(t, s, ctx, "hermes_log_decision", map[string]string{
		"id":        "env-1",
		"decision":  "use SQLite",
		"reasoning": "simplest option",
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
}

func TestLogDecision_MissingID(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_log_decision", map[string]string{"decision": "test"})
	if !isErr {
		t.Error("expected error for missing id")
	}
}

func TestLogDecision_MissingDecision(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_log_decision", map[string]string{"id": "env-1"})
	if !isErr {
		t.Error("expected error for missing decision")
	}
}

func TestListNotifications_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	_, isErr := callTool(t, s, ctx, "hermes_list_notifications", map[string]string{})
	if isErr {
		t.Error("unexpected error")
	}
}

func TestAckNotification_MissingID(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_ack_notification", map[string]string{})
	if !isErr {
		t.Error("expected error for missing id")
	}
}

func TestAckNotification_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	_, isErr := callTool(t, s, ctx, "hermes_ack_notification", map[string]string{"id": "42"})
	if isErr {
		t.Error("unexpected error")
	}
}

func TestListEnvelopes_WithStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("status"); got != "blocked" {
			t.Errorf("expected status=blocked, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	_, isErr := callTool(t, s, ctx, "hermes_list_envelopes", map[string]string{"status": "blocked"})
	if isErr {
		t.Error("unexpected error")
	}
}

func TestListProjects_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	_, isErr := callTool(t, s, ctx, "hermes_list_projects", map[string]string{})
	if isErr {
		t.Error("unexpected error")
	}
}

func TestCreateEnvelope_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"env-new"}`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	_, isErr := callTool(t, s, ctx, "hermes_create_envelope", map[string]string{"envelope": `{"id":"env-new"}`})
	if isErr {
		t.Error("unexpected error")
	}
}

func TestCreateEnvelope_Empty(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_create_envelope", map[string]string{})
	if !isErr {
		t.Error("expected error for empty envelope")
	}
}

func TestClient_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := NewHermesClient(srv.URL, "")
	_, err := c.GetEnvelope(context.Background(), "env-1")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code: %v", err)
	}
}

func TestClient_SendsAPIKeyHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Hermes-Key"); got != "dev-key-test123" {
			t.Errorf("expected X-Hermes-Key=dev-key-test123, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewHermesClient(srv.URL, "dev-key-test123")
	_, err := c.ListEnvelopes(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_NoAPIKey_NoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Hermes-Key"); got != "" {
			t.Errorf("expected no X-Hermes-Key header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewHermesClient(srv.URL, "")
	_, err := c.ListEnvelopes(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClient_NetworkError(t *testing.T) {
	c := NewHermesClient("http://127.0.0.1:1", "") // nothing listening
	_, err := c.GetEnvelope(context.Background(), "env-1")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestPostMessage_MissingEnvelopeID(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"kind": "decision",
		"text": "hello",
	})
	if !isErr {
		t.Error("expected error for missing envelope_id")
	}
}

func TestPostMessage_MissingKind(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"text":        "hello",
	})
	if !isErr {
		t.Error("expected error for missing kind")
	}
}

func TestPostMessage_MissingText(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"kind":        "decision",
	})
	if !isErr {
		t.Error("expected error for missing text")
	}
}

func TestPostMessage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/envelopes/env-1/thread" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["from"] != "opencode" {
			t.Errorf("expected from=opencode, got %q", body["from"])
		}
		if body["kind"] != "decision" {
			t.Errorf("expected kind=decision, got %q", body["kind"])
		}
		if body["text"] != "hello world" {
			t.Errorf("expected text=hello world, got %q", body["text"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg-1"}`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	text, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"kind":        "decision",
		"text":        "hello world",
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
}

func TestPostMessage_WithReplyTo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["reply_to"] != "msg-0" {
			t.Errorf("expected reply_to=msg-0, got %q", body["reply_to"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg-2"}`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	text, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"kind":        "reply",
		"text":        "here is my response",
		"reply_to":    "msg-0",
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
}

func TestPostMessage_CustomFrom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["from"] != "kitt" {
			t.Errorf("expected from=kitt, got %q", body["from"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg-1"}`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	text, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"kind":        "steer",
		"text":        "guiding the agent",
		"from":        "kitt",
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
}

func TestPostMessage_ReplyWithoutReplyTo(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"kind":        "reply",
		"text":        "responding without reply_to",
	})
	if !isErr {
		t.Error("expected error for kind=reply without reply_to")
	}
}

func TestPostMessage_InvalidKind(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"kind":        "unknown",
		"text":        "hello",
	})
	if !isErr {
		t.Error("expected error for invalid kind")
	}
}

func TestPostMessage_EmptyFromDefaultsToOpencode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["from"] != "opencode" {
			t.Errorf("expected from=opencode, got %q", body["from"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg-1"}`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	text, isErr := callTool(t, s, ctx, "hermes_post_message", map[string]string{
		"envelope_id": "env-1",
		"kind":        "decision",
		"text":        "hello",
		"from":        "", // empty string should default to opencode
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
}

func TestGetThread_MissingEnvelopeID(t *testing.T) {
	s, ctx := initServer(t, "http://localhost:0")
	_, isErr := callTool(t, s, ctx, "hermes_get_thread", map[string]string{})
	if !isErr {
		t.Error("expected error for missing envelope_id")
	}
}

func TestGetThread_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/envelopes/env-1/thread" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"msg-1"},{"id":"msg-2"}]`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	text, isErr := callTool(t, s, ctx, "hermes_get_thread", map[string]string{
		"envelope_id": "env-1",
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
}

func TestGetThread_WithSinceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since_id"); got != "msg-1" {
			t.Errorf("expected since_id=msg-1, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"msg-2"}]`))
	}))
	defer srv.Close()

	s, ctx := initServer(t, srv.URL)
	text, isErr := callTool(t, s, ctx, "hermes_get_thread", map[string]string{
		"envelope_id": "env-1",
		"since_id":    "msg-1",
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
}

func TestCreateEnvelope_InvalidJSON(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	registerTools(s, c)

	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))

	resp := s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"tools/call","id":4,"params":{"name":"hermes_create_envelope","arguments":{"envelope":"not json"}}}`))
	raw, _ := json.Marshal(resp)

	var result struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	json.Unmarshal(raw, &result)
	if !result.Result.IsError {
		t.Error("expected isError=true for invalid JSON")
	}
}

// Tests for hermes_resume_session

func TestResumeSession_MissingEnvelopeID(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	fc := NewForgeClient("http://localhost:0", "")
	registerTools(s, c, fc)

	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))

	_, isErr := callTool(t, s, ctx, "hermes_resume_session", map[string]string{
		"message": "continue",
	})
	if !isErr {
		t.Error("expected error for missing envelope_id")
	}
}

func TestResumeSession_MissingMessage(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	fc := NewForgeClient("http://localhost:0", "")
	registerTools(s, c, fc)

	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))

	_, isErr := callTool(t, s, ctx, "hermes_resume_session", map[string]string{
		"envelope_id": "env-1",
	})
	if !isErr {
		t.Error("expected error for missing message")
	}
}

func TestResumeSession_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions/env-1/resume" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["message"] != "resume and continue" {
			t.Errorf("expected message='resume and continue', got %q", body["message"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"session_id":"oc-session-123"}`))
	}))
	defer srv.Close()

	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	fc := NewForgeClient(srv.URL, "")
	registerTools(s, c, fc)

	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))

	text, isErr := callTool(t, s, ctx, "hermes_resume_session", map[string]string{
		"envelope_id": "env-1",
		"message":     "resume and continue",
	})
	if isErr {
		t.Errorf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "oc-session-123") {
		t.Errorf("expected response to contain session_id, got %s", text)
	}
}

func TestResumeSession_ForgeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not_found"}`))
	}))
	defer srv.Close()

	s := server.NewMCPServer("test", "0.0.1", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	fc := NewForgeClient(srv.URL, "")
	registerTools(s, c, fc)

	ctx := context.Background()
	s.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"initialize","id":0,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`))

	text, isErr := callTool(t, s, ctx, "hermes_resume_session", map[string]string{
		"envelope_id": "env-unknown",
		"message":     "hello",
	})
	if !isErr {
		t.Error("expected error for Forge returning 404")
	}
	if !strings.Contains(text, "not_found") {
		t.Errorf("expected error to contain 'not_found', got %s", text)
	}
}

// ---------------------------------------------------------------------------
// HTTP transport mode tests
// ---------------------------------------------------------------------------

// TestHTTPTransport_HealthEndpoint verifies that when hermes-mcp is started
// in HTTP mode it exposes the MCP endpoint and responds to a JSON-RPC
// initialize request over HTTP POST.
func TestHTTPTransport_HealthEndpoint(t *testing.T) {
	s := server.NewMCPServer("hermes-mcp", "0.1.0", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	registerTools(s, c)

	addr, shutdown := startHTTPTransport(t, s)
	defer shutdown()

	initBody := `{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`
	resp, err := http.Post("http://"+addr+"/mcp", "application/json", strings.NewReader(initBody))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 200 or 202, got %d", resp.StatusCode)
	}
}

// TestHTTPTransport_ToolsListOverHTTP verifies tools/list works via HTTP transport.
func TestHTTPTransport_ToolsListOverHTTP(t *testing.T) {
	s := server.NewMCPServer("hermes-mcp", "0.1.0", server.WithToolCapabilities(true))
	c := NewHermesClient("http://localhost:0", "")
	registerTools(s, c)

	addr, shutdown := startHTTPTransport(t, s)
	defer shutdown()

	// Initialize session first
	initBody := `{"jsonrpc":"2.0","method":"initialize","id":1,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`
	initResp, err := http.Post("http://"+addr+"/mcp", "application/json", strings.NewReader(initBody))
	if err != nil {
		t.Fatalf("initialize POST: %v", err)
	}
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	initResp.Body.Close()

	// tools/list
	listBody := `{"jsonrpc":"2.0","method":"tools/list","id":2}`
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(listBody))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	listResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tools/list POST: %v", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", listResp.StatusCode)
	}
}

// startHTTPTransport is a test helper that starts a StreamableHTTP MCP server
// on a random port and returns the address and a shutdown function.
// This function must exist in the main package (transport_http.go).
func startHTTPTransport(t *testing.T, s *server.MCPServer) (addr string, shutdown func()) {
	t.Helper()
	return startHTTPServer(s, "127.0.0.1:0")
}
