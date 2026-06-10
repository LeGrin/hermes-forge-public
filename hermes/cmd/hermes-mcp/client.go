// Package main implements the Hermes MCP server — a thin HTTP-to-MCP
// bridge that wraps the Hermes HTTP API as MCP tools. Deployable
// anywhere that can reach Hermes via HERMES_URL.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HermesClient talks to the Hermes HTTP API.
type HermesClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewHermesClient(baseURL, apiKey string) *HermesClient {
	return &HermesClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

const envelopesPath = "/envelopes/"

func (c *HermesClient) CreateEnvelope(ctx context.Context, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, envelopesPath[:len(envelopesPath)-1], body)
}

func (c *HermesClient) GetEnvelope(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, envelopesPath+url.PathEscape(id), nil)
}

func (c *HermesClient) ListEnvelopes(ctx context.Context, statuses string) (json.RawMessage, error) {
	path := envelopesPath[:len(envelopesPath)-1]
	if statuses != "" {
		path += "?status=" + url.QueryEscape(statuses)
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *HermesClient) UpdateStatus(ctx context.Context, id string, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPatch, envelopesPath+url.PathEscape(id)+"/status", body)
}

func (c *HermesClient) ListProjects(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/registry/projects", nil)
}

func (c *HermesClient) AddHistory(ctx context.Context, id string, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, envelopesPath+url.PathEscape(id)+"/history", body)
}

func (c *HermesClient) ListNotifications(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/notifications", nil)
}

func (c *HermesClient) AckNotification(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/notifications/"+url.PathEscape(id)+"/ack", nil)
}

func (c *HermesClient) ReportActivity(ctx context.Context, body json.RawMessage) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/activity", body)
}

func (c *HermesClient) PostMessage(ctx context.Context, envelopeID, from, kind, text, replyTo string) (json.RawMessage, error) {
	body := map[string]string{
		"from": from,
		"kind": kind,
		"text": text,
	}
	if replyTo != "" {
		body["reply_to"] = replyTo
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	return c.do(ctx, http.MethodPost, envelopesPath+url.PathEscape(envelopeID)+"/thread", raw)
}

func (c *HermesClient) GetThread(ctx context.Context, envelopeID, sinceID string) (json.RawMessage, error) {
	path := envelopesPath + url.PathEscape(envelopeID) + "/thread"
	if sinceID != "" {
		path += "?since_id=" + url.QueryEscape(sinceID)
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *HermesClient) do(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("X-Hermes-Key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("hermes %s %s returned %d: %s", method, path, resp.StatusCode, string(raw))
	}
	return raw, nil
}

// ForgeClient talks to the Forge HTTP API.
type ForgeClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// NewForgeClient creates a new Forge client.
func NewForgeClient(baseURL, apiKey string) *ForgeClient {
	return &ForgeClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// ResumeSession calls Forge's POST /sessions/{envelope_id}/resume to respawn
// an idle session and inject a message.
func (c *ForgeClient) ResumeSession(ctx context.Context, envelopeID, message string) (json.RawMessage, error) {
	body := map[string]string{"message": message}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	return c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(envelopeID)+"/resume", raw)
}

func (c *ForgeClient) do(ctx context.Context, method, path string, body json.RawMessage) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("X-Hermes-Key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("forge %s %s returned %d: %s", method, path, resp.StatusCode, string(raw))
	}
	return raw, nil
}
