// Package main — LLM client for batch notification summarization.
// Uses OpenAI-compatible chat completions API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type llmClient struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

// newLLMClient returns nil if url is empty (LLM disabled path).
func newLLMClient(url, apiKey, model string) *llmClient {
	if url == "" {
		return nil
	}
	return &llmClient{
		url:    url,
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Summarize calls the LLM to produce a concise Ukrainian Telegram summary.
func (c *llmClient) Summarize(ctx context.Context, items []payload) (string, error) {
	userContent, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("marshal items: %w", err)
	}

	reqBody, err := json.Marshal(map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{
				"role":    "system",
				"content": "You are a concise notification summarizer. Summarize these Hermes task notifications into a brief Ukrainian message for Telegram. Group by outcome. Use emoji. Max 500 chars.",
			},
			{
				"role":    "user",
				"content": string(userContent),
			},
		},
		"max_tokens": 300,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no choices in LLM response")
	}
	return result.Choices[0].Message.Content, nil
}
