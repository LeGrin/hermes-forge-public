package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// hermesClient calls Hermes API using plain HTTP (W-H12: no direct Hermes imports).
type hermesClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// newHermesClient creates a Hermes API client.
func newHermesClient(baseURL, apiKey string) *hermesClient {
	if baseURL == "" {
		return nil
	}
	return &hermesClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// SetExecutorSessionID calls PATCH /envelopes/{id}/session
// to associate an executor_session_id with an envelope.
func (c *hermesClient) SetExecutorSessionID(ctx context.Context, envelopeID, sessionID string) error {
	if c == nil {
		return nil // no-op if client not configured
	}

	escapedID := url.PathEscape(envelopeID)
	url := fmt.Sprintf("%s/envelopes/%s/session", c.baseURL, escapedID)
	body := map[string]string{"executor_session_id": sessionID}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		// Hermes's auth middleware reads X-Hermes-Key; using Authorization:
		// Bearer would be rejected with 401 even when the key itself is valid.
		req.Header.Set("X-Hermes-Key", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hermes PATCH /envelopes/%s/session returned %d", envelopeID, resp.StatusCode)
	}
	return nil
}
