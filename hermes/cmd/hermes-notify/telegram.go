// Package main — Telegram send + bot command polling.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type telegram struct {
	token    string
	chatID   string
	threadID string // optional: message_thread_id for supergroup topic routing
	baseURL  string // overridable in tests
	client   *http.Client
}

func newTelegram(token, chatID, threadID string) *telegram {
	return &telegram{
		token:    token,
		chatID:   chatID,
		threadID: threadID,
		baseURL:  "https://api.telegram.org",
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Send posts a Telegram message with HTML parse mode and optional topic routing.
func (tg *telegram) Send(text string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", tg.baseURL, tg.token)
	payload := map[string]any{
		"chat_id":    tg.chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if tg.threadID != "" {
		threadNum, err := strconv.ParseInt(tg.threadID, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid telegram thread id %q: %w", tg.threadID, err)
		}
		payload["message_thread_id"] = threadNum
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telegram request: %w", err)
	}
	resp, err := tg.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// PollUpdates long-polls Telegram getUpdates and dispatches /busy, /free, /status commands.
func (tg *telegram) PollUpdates(ctx context.Context, onCommand func(cmd, text string)) {
	var offset int64
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, nextOffset, err := tg.fetchUpdates(ctx, offset)
		if err != nil {
			// transient network error — back off briefly
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		offset = nextOffset

		for _, u := range updates {
			text := u.Message.Text
			if !strings.HasPrefix(text, "/") {
				continue
			}
			parts := strings.SplitN(text, " ", 2)
			cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
			arg := ""
			if len(parts) == 2 {
				arg = parts[1]
			}
			switch cmd {
			case "busy", "free", "status":
				onCommand(cmd, arg)
			}
		}
	}
}

// SECTION: internal Telegram API helpers

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Text string `json:"text"`
	} `json:"message"`
}

// fetchUpdates calls getUpdates with long-poll timeout=30 and returns parsed updates + next offset.
func (tg *telegram) fetchUpdates(ctx context.Context, offset int64) ([]tgUpdate, int64, error) {
	url := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=30", tg.baseURL, tg.token, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, offset, err
	}

	// Use a separate client with longer timeout for long-polling.
	pollClient := &http.Client{Timeout: 40 * time.Second}
	resp, err := pollClient.Do(req)
	if err != nil {
		return nil, offset, err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, offset, err
	}

	nextOffset := offset
	for _, u := range result.Result {
		if u.UpdateID >= nextOffset {
			nextOffset = u.UpdateID + 1
		}
	}
	return result.Result, nextOffset, nil
}
