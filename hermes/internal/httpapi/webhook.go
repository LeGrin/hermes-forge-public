package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
)

// webhookFirer dispatches envelope/session notifications to the configured
// downstream sinks. It is shared between envelopeHandler and sessionHandler
// so there is exactly one path from "interesting state change in Hermes"
// to the operator's channels.
//
// Sinks (all optional, all fire-and-forget):
//   - webhookURL + webhookSecret set → POST to OpenClaw /v1/chat/completions
//     with the shared `hermes-notifications` session so KITT accumulates
//     context across events.
//   - webhookURL set without secret → POST simple JSON (legacy hermes-notify
//     router payload shape).
//   - tgToken + tgChat set → POST directly to Telegram Bot API. This path
//     is for Mac-local dev where there is no OpenClaw.
//
// If none of the fields is set, Fire is a no-op.
type webhookFirer struct {
	webhookURL    string
	webhookSecret string
	tgToken       string
	tgChat        string
	tgThread      string // optional: message_thread_id for Telegram topic routing
	logger        *slog.Logger
}

// Fire dispatches the given notification to all configured sinks.
// It never blocks the caller: every network call is wrapped in a goroutine.
//
// Parameters:
//   - subjectID: envelope id for envelope events, session id for session events.
//   - title:     human-readable title (envelope.TaskTitle or session.Title).
//   - status:    short machine code (e.g. "done", "blocked", "session_decision").
//   - note:      free-form note from the caller.
//   - proofSummary: proof string on envelope completion, empty otherwise.
//   - loopDetected: whether this event represents a detected blocked loop.
func (wf *webhookFirer) Fire(ctx context.Context, subjectID, title, status, note, proofSummary string, loopDetected bool) {
	if wf == nil {
		return
	}

	wf.dispatchTelegram(title, status, note, proofSummary, loopDetected)
	wf.dispatchWebhook(ctx, subjectID, title, status, note, proofSummary, loopDetected)
}

func (wf *webhookFirer) dispatchTelegram(title, status, note, proofSummary string, loopDetected bool) {
	if wf.tgToken == "" || wf.tgChat == "" {
		return
	}
	token, chat, thread := wf.tgToken, wf.tgChat, wf.tgThread
	logger := wf.logger
	msg := formatTelegramMessage(status, title, note, proofSummary, loopDetected)
	go func() {
		if err := telegramSend(token, chat, thread, msg); err != nil {
			logger.Error("telegram send failed", "err", err)
		}
	}()
}

func (wf *webhookFirer) dispatchWebhook(_ context.Context, subjectID, title, status, note, proofSummary string, loopDetected bool) {
	if wf.webhookURL == "" {
		return
	}
	url, secret, logger := wf.webhookURL, wf.webhookSecret, wf.logger

	if secret != "" {
		msg := formatWebhookGoal(subjectID, status, title, note, proofSummary, loopDetected)
		go func() {
			if err := openClawNotify(url, secret, msg); err != nil {
				logger.Error("openclaw notify failed", "err", err)
			}
		}()
		return
	}

	payload := map[string]any{
		"envelope_id":   subjectID,
		"status":        status,
		"task_title":    title,
		"note":          note,
		"proof_summary": proofSummary,
		"loop_detected": loopDetected,
	}
	go func() {
		body, err := json.Marshal(payload)
		if err != nil {
			logger.Error("webhook marshal failed", "err", err)
			return
		}
		resp, err := webhookClient.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx
		if err != nil {
			logger.Error("webhook post failed", "err", err)
			return
		}
		_ = resp.Body.Close()
	}()
}

// newWebhookFirer builds a firer from ServerOpts. Returns nil if no sink is
// configured — handlers can safely call Fire on a nil receiver.
func newWebhookFirer(logger *slog.Logger, o ServerOpts) *webhookFirer {
	if o.WebhookURL == "" && o.TGToken == "" {
		return nil
	}
	return &webhookFirer{
		webhookURL:    o.WebhookURL,
		webhookSecret: o.WebhookSecret,
		tgToken:       o.TGToken,
		tgChat:        o.TGChat,
		tgThread:      o.TGThread,
		logger:        logger,
	}
}
