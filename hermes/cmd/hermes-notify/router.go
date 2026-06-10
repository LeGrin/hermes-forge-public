// Package main — priority routing, in-memory buffer, and flush logic.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SECTION: priority matrix

var priorityMap = map[string]string{
	"blocked":          "urgent",
	"failed":           "urgent",
	"loop_detected":    "urgent",
	"done":             "normal",
	"awaiting_confirm": "normal",
	"paused":           "low",
}

// priority returns "urgent", "normal", or "low" for a given status.
func priority(status string) string {
	if p, ok := priorityMap[status]; ok {
		return p
	}
	return "low"
}

// SECTION: router

const bufferCap = 100

type router struct {
	mu        sync.Mutex
	busy      bool
	busySince time.Time
	buffer    []payload
	logger    *slog.Logger
	tg        *telegram
	llm       *llmClient
}

func newRouter(logger *slog.Logger, tg *telegram, llm *llmClient) *router {
	return &router{logger: logger, tg: tg, llm: llm}
}

// handleWebhook decodes the POST body and routes by priority.
func (rt *router) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var p payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		rt.logger.Error("decode webhook payload failed", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	// Normalise loop_detected flag into status.
	if p.LoopDetected && p.Status != "loop_detected" {
		p.Status = "loop_detected"
	}
	rt.logger.Info("webhook received", "envelope_id", p.EnvelopeID, "status", p.Status)
	rt.route(p)
	w.WriteHeader(http.StatusOK)
}

func (rt *router) route(p payload) {
	pri := priority(p.Status)
	rt.mu.Lock()
	busy := rt.busy
	rt.mu.Unlock()

	switch {
	case pri == "urgent":
		rt.send(formatMessage(p))
	case pri == "normal" && !busy:
		rt.send(formatMessage(p))
	default:
		rt.addToBuffer(p)
	}
}

func (rt *router) send(text string) {
	if rt.tg == nil {
		return
	}
	if err := rt.tg.Send(text); err != nil {
		rt.logger.Error("telegram send failed", "err", err)
	}
}

func (rt *router) addToBuffer(p payload) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.buffer) >= bufferCap {
		rt.buffer = rt.buffer[1:] // drop oldest
	}
	rt.buffer = append(rt.buffer, p)
}

// SetBusy marks the router busy (buffers normal/low notifications).
func (rt *router) SetBusy(reason string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.busy = true
	rt.busySince = time.Now()
	rt.logger.Info("router set busy", "reason", reason)
}

// SetFree marks free and flushes buffered notifications.
func (rt *router) SetFree() {
	rt.mu.Lock()
	rt.busy = false
	rt.mu.Unlock()
	rt.logger.Info("router set free")
	rt.Flush()
}

// Mode returns "busy" or "free".
func (rt *router) Mode() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.busy {
		return "busy"
	}
	return "free"
}

// Flush drains the buffer and sends a summary (or single message) to Telegram.
func (rt *router) Flush() {
	rt.mu.Lock()
	items := rt.buffer
	rt.buffer = nil
	rt.mu.Unlock()

	if len(items) == 0 {
		return
	}
	if len(items) == 1 {
		rt.send(formatMessage(items[0]))
		return
	}
	rt.send(rt.formatBatch(items))
}

// formatBatch produces a summary via LLM or falls back to a bullet list.
func (rt *router) formatBatch(items []payload) string {
	if rt.llm != nil {
		if summary, err := rt.llm.Summarize(context.Background(), items); err == nil {
			return summary
		}
	}
	return bulletList(items)
}

// bulletList is the LLM-unavailable fallback formatter.
func bulletList(items []payload) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Пропущені нотифікації (%d):\n", len(items))
	for _, p := range items {
		sb.WriteString("• ")
		sb.WriteString(formatMessage(p))
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// autoFlush runs on a ticker: safety auto-free after 2h busy, partial flush after 30 min.
func (rt *router) autoFlush(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.mu.Lock()
			busy := rt.busy
			since := rt.busySince
			hasItems := len(rt.buffer) > 0
			rt.mu.Unlock()

			if !busy {
				continue
			}
			elapsed := time.Since(since)
			if elapsed > 2*time.Hour {
				rt.logger.Info("auto-free after 2h busy")
				rt.SetFree()
				continue
			}
			if hasItems && elapsed > 30*time.Minute {
				rt.logger.Info("partial flush after 30min busy")
				rt.Flush()
			}
		}
	}
}

// pollCommands wires Telegram bot commands to router state changes.
func (rt *router) pollCommands(ctx context.Context) {
	if rt.tg == nil {
		return
	}
	rt.tg.PollUpdates(ctx, func(cmd, text string) {
		switch cmd {
		case "busy":
			rt.SetBusy(text)
			rt.send("🔕 Режим «зайнятий» увімкнено. Нотифікації буферизуються.")
		case "free":
			rt.send("🔔 Режим «вільний». Відправляю пропущені нотифікації...")
			rt.SetFree()
		case "status":
			rt.mu.Lock()
			mode := "free"
			if rt.busy {
				mode = "busy"
			}
			count := len(rt.buffer)
			rt.mu.Unlock()
			rt.send(fmt.Sprintf("ℹ️ Статус: %s | Буфер: %d", mode, count))
		}
	})
}

// formatMessage formats a single payload into a Telegram message string.
// Kept here alongside routing logic as it is used by both route() and Flush().
func formatMessage(p payload) string {
	switch {
	case p.LoopDetected || p.Status == "loop_detected":
		return fmt.Sprintf("🔄 LOOP: %s\n%s", p.TaskTitle, p.Note)
	case p.Status == "done":
		return fmt.Sprintf("✅ %s\n%s", p.TaskTitle, p.ProofSummary)
	case p.Status == "blocked":
		return fmt.Sprintf("🚫 BLOCKED: %s\n%s", p.TaskTitle, p.Note)
	case p.Status == "failed":
		return fmt.Sprintf("❌ FAILED: %s\n%s", p.TaskTitle, p.Note)
	case p.Status == "awaiting_confirm":
		return fmt.Sprintf("❓ %s\n%s", p.TaskTitle, p.Note)
	default:
		return fmt.Sprintf("📨 %s: %s", p.Status, p.TaskTitle)
	}
}
