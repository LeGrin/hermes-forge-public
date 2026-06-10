package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	msgIDRequired  = "id is required"
	descEnvelopeID = "Envelope ID"
)

// newNoArgTool builds an MCP tool that takes no arguments but still
// produces a JSON schema with an explicit `"properties": {}` block.
//
// Background: `mcp.NewTool` + `mcp.WithDescription` alone leaves the
// tool's properties map empty, and `ToolInputSchema`'s generated JSON
// marshaler drops the `properties` key because of `omitempty` on a
// zero-length map. OpenAI's function-calling endpoint rejects that
// shape with 400 `invalid_function_parameters: object schema missing
// properties` — which in turn degrades the codex provider for the
// whole stables router and cascades every KITT agent turn. Using
// `NewToolWithRawSchema` lets us emit the canonical empty-properties
// shape OpenAI expects.
func newNoArgTool(name, description string) mcp.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	return mcp.NewToolWithRawSchema(name, description, schema)
}

func main() {
	// SECTION: flags
	transport := flag.String("transport", "stdio", "Transport mode: stdio (default) or http")
	httpAddr := flag.String("addr", ":8095", "Listen address for http transport (e.g. :8095)")
	flag.Parse()

	// SECTION: env
	hermesURL := os.Getenv("HERMES_URL")
	if hermesURL == "" {
		log.Fatal("HERMES_URL is required")
	}
	apiKey := os.Getenv("HERMES_KEY")
	client := NewHermesClient(hermesURL, apiKey)

	forgeURL := os.Getenv("FORGE_URL")
	var forgeClient *ForgeClient
	if forgeURL != "" {
		forgeClient = NewForgeClient(forgeURL, apiKey)
	}

	// SECTION: MCP server + tools (shared by both transports)
	s := server.NewMCPServer("hermes-mcp", "0.1.0",
		server.WithToolCapabilities(true),
	)
	registerTools(s, client, forgeClient)

	// SECTION: transport dispatch
	switch *transport {
	case "stdio":
		if err := server.ServeStdio(s); err != nil {
			log.Fatalf("stdio server error: %v", err)
		}

	case "http":
		boundAddr, shutdown := startHTTPServer(s, *httpAddr)
		log.Printf("hermes-mcp HTTP transport listening on %s (MCP endpoint: POST/GET /mcp)", boundAddr)

		// Block until SIGINT/SIGTERM
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit

		log.Println("hermes-mcp: shutting down HTTP transport")
		shutdown()

	default:
		log.Fatalf("unknown transport %q — valid values: stdio, http", *transport)
	}
}

func registerTools(s *server.MCPServer, c *HermesClient, fc ...*ForgeClient) {
	s.AddTool(mcp.NewTool("hermes_create_envelope",
		mcp.WithDescription("Create a new envelope in Hermes. Pass full envelope JSON as 'envelope' field."),
		mcp.WithString("envelope", mcp.Description("Envelope JSON object as string"), mcp.Required()),
	), makeCreateHandler(c))

	s.AddTool(mcp.NewTool("hermes_get_envelope",
		mcp.WithDescription("Get an envelope by ID from Hermes."),
		mcp.WithString("id", mcp.Description(descEnvelopeID), mcp.Required()),
	), makeGetHandler(c))

	s.AddTool(mcp.NewTool("hermes_list_envelopes",
		mcp.WithDescription("List envelopes, optionally filtered by status (comma-separated)."),
		mcp.WithString("status", mcp.Description("Comma-separated status filter, e.g. 'blocked,paused'")),
	), makeListHandler(c))

	s.AddTool(newNoArgTool("hermes_list_projects",
		"List all registered projects. Returns project name, domain, target_node, target_executor, and working_dir for each."),
		makeListProjectsHandler(c))

	s.AddTool(mcp.NewTool("hermes_update_status",
		mcp.WithDescription("Update envelope status. IMPORTANT: You MUST call this when finishing work (status=done with proof) or when blocked/need help (status=blocked with note). KITT and the user are notified automatically on status changes."),
		mcp.WithString("id", mcp.Description(descEnvelopeID), mcp.Required()),
		mcp.WithString("status", mcp.Description("New status: read, in_progress, paused, blocked, awaiting_confirm, done, failed, lost"), mcp.Required()),
		mcp.WithString("note", mcp.Description("Transition note (reason, context)")),
		mcp.WithString("proof", mcp.Description("JSON object with proof keys, e.g. {\"commit\":\"abc123\"}")),
	), makeUpdateHandler(c))

	s.AddTool(mcp.NewTool("hermes_log_decision",
		mcp.WithDescription("Log an important decision made during task execution. Use this when you choose between approaches, reject an alternative, or make a non-obvious choice. Decisions are visible to KITT and the user in the envelope history."),
		mcp.WithString("id", mcp.Description(descEnvelopeID), mcp.Required()),
		mcp.WithString("decision", mcp.Description("What was decided"), mcp.Required()),
		mcp.WithString("reasoning", mcp.Description("Why this choice was made, what alternatives were considered")),
	), makeLogDecisionHandler(c))

	s.AddTool(newNoArgTool("hermes_list_notifications",
		"List unacknowledged notifications. Used by KITT to check for status updates."),
		makeListNotificationsHandler(c))

	s.AddTool(mcp.NewTool("hermes_ack_notification",
		mcp.WithDescription("Acknowledge a notification after processing it."),
		mcp.WithString("id", mcp.Description("Notification ID"), mcp.Required()),
	), makeAckNotificationHandler(c))

	s.AddTool(mcp.NewTool("hermes_report_activity",
		mcp.WithDescription("Report live activity to the Hermes dashboard. Call this when performing notable actions (tool calls, decisions, status changes) so the dashboard shows real-time progress."),
		mcp.WithString("envelope_id", mcp.Description(descEnvelopeID)),
		mcp.WithString("kind", mcp.Description("Event kind: tool_use, status, decision, heartbeat"), mcp.Required()),
		mcp.WithString("summary", mcp.Description("Short human-readable summary of what is happening")),
	), makeReportActivityHandler(c))

	s.AddTool(mcp.NewTool("hermes_post_message",
		mcp.WithDescription("Post a message to the envelope thread. Use kind=decision when you need Kitt's input before continuing (agent-initiated question). Use kind=reply to respond to a steer from Kitt. kind=steer is for Kitt only."),
		mcp.WithString("envelope_id", mcp.Description(descEnvelopeID), mcp.Required()),
		mcp.WithString("kind", mcp.Description("Message kind: decision (agent asks Kitt), steer (Kitt directs agent — use from=kitt), reply (response to a message)"), mcp.Required()),
		mcp.WithString("text", mcp.Description("Message text"), mcp.Required()),
		mcp.WithString("reply_to", mcp.Description("ID of the message being replied to (for kind=reply)")),
		mcp.WithString("from", mcp.Description("Sender identity: opencode, claude, kitt (default: opencode)")),
	), makePostMessageHandler(c))

	s.AddTool(mcp.NewTool("hermes_get_thread",
		mcp.WithDescription("Get thread messages for an envelope. Use since_id to poll only new messages since last check."),
		mcp.WithString("envelope_id", mcp.Description(descEnvelopeID), mcp.Required()),
		mcp.WithString("since_id", mcp.Description("Only return messages after this message ID (for polling)")),
	), makeGetThreadHandler(c))

	if len(fc) > 0 && fc[0] != nil {
		s.AddTool(mcp.NewTool("hermes_resume_session",
			mcp.WithDescription("Resume an idle OpenCode session for an envelope and inject a message. Use when you posted a steer but the session is no longer running (POST /notify returned 409)."),
			mcp.WithString("envelope_id", mcp.Description(descEnvelopeID), mcp.Required()),
			mcp.WithString("message", mcp.Description("Message to inject into the resumed session"), mcp.Required()),
		), makeResumeSessionHandler(fc[0]))
	}
}

func makeCreateHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envStr := req.GetString("envelope", "")
		if envStr == "" {
			return toolError("envelope is required"), nil
		}
		if !json.Valid([]byte(envStr)) {
			return toolError("envelope must be valid JSON"), nil
		}
		result, err := c.CreateEnvelope(ctx, json.RawMessage(envStr))
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeGetHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return toolError(msgIDRequired), nil
		}
		result, err := c.GetEnvelope(ctx, id)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeListHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		status := req.GetString("status", "")
		result, err := c.ListEnvelopes(ctx, status)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeListProjectsHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := c.ListProjects(ctx)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeUpdateHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return toolError(msgIDRequired), nil
		}
		status := req.GetString("status", "")
		if status == "" {
			return toolError("status is required"), nil
		}

		body := map[string]any{"status": status}
		if note := req.GetString("note", ""); note != "" {
			body["note"] = note
		}
		if proofStr := req.GetString("proof", ""); proofStr != "" {
			var proof map[string]string
			if err := json.Unmarshal([]byte(proofStr), &proof); err != nil {
				return toolError(fmt.Sprintf("proof must be valid JSON object: %v", err)), nil
			}
			body["proof"] = proof
		}

		raw, err := json.Marshal(body)
		if err != nil {
			return toolError(err.Error()), nil
		}
		result, err := c.UpdateStatus(ctx, id, raw)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeLogDecisionHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return toolError(msgIDRequired), nil
		}
		decision := req.GetString("decision", "")
		if decision == "" {
			return toolError("decision is required"), nil
		}
		entry := "[DECISION] " + decision
		if reasoning := req.GetString("reasoning", ""); reasoning != "" {
			entry += " | Reasoning: " + reasoning
		}

		body, err := json.Marshal(map[string]string{"entry": entry})
		if err != nil {
			return toolError(fmt.Sprintf("marshal entry: %v", err)), nil
		}
		result, err := c.AddHistory(ctx, id, body)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeListNotificationsHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result, err := c.ListNotifications(ctx)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeAckNotificationHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id := req.GetString("id", "")
		if id == "" {
			return toolError(msgIDRequired), nil
		}
		result, err := c.AckNotification(ctx, id)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeReportActivityHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		kind := req.GetString("kind", "")
		if kind == "" {
			return toolError("kind is required"), nil
		}
		body := map[string]string{
			"kind":    kind,
			"summary": req.GetString("summary", ""),
		}
		if eid := req.GetString("envelope_id", ""); eid != "" {
			body["envelope_id"] = eid
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return toolError(err.Error()), nil
		}
		result, err := c.ReportActivity(ctx, raw)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makePostMessageHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envelopeID := req.GetString("envelope_id", "")
		if envelopeID == "" {
			return toolError("envelope_id is required"), nil
		}
		kind := req.GetString("kind", "")
		if kind == "" {
			return toolError("kind is required"), nil
		}
		// Validate kind value
		switch kind {
		case "decision", "steer", "reply":
			// valid
		default:
			return toolError("kind must be one of: decision, steer, reply"), nil
		}
		text := req.GetString("text", "")
		if text == "" {
			return toolError("text is required"), nil
		}
		from := req.GetString("from", "opencode")
		if from == "" {
			from = "opencode"
		}
		replyTo := req.GetString("reply_to", "")
		if kind == "reply" && replyTo == "" {
			return toolError("reply_to is required when kind=reply"), nil
		}
		result, err := c.PostMessage(ctx, envelopeID, from, kind, text, replyTo)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeGetThreadHandler(c *HermesClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envelopeID := req.GetString("envelope_id", "")
		if envelopeID == "" {
			return toolError("envelope_id is required"), nil
		}
		sinceID := req.GetString("since_id", "")
		result, err := c.GetThread(ctx, envelopeID, sinceID)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func makeResumeSessionHandler(fc *ForgeClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		envelopeID := req.GetString("envelope_id", "")
		if envelopeID == "" {
			return toolError("envelope_id is required"), nil
		}
		message := req.GetString("message", "")
		if message == "" {
			return toolError("message is required"), nil
		}
		result, err := fc.ResumeSession(ctx, envelopeID, message)
		if err != nil {
			return toolError(err.Error()), nil
		}
		return toolText(string(result)), nil
	}
}

func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: text}},
	}
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: msg}},
		IsError: true,
	}
}
