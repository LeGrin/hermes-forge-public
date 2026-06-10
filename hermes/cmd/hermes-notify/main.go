// Command hermes-notify is a priority-aware notification router.
// It receives webhook POSTs from Hermes and forwards to Telegram,
// with buffering when the user is busy and LLM-powered batch summaries on flush.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
)

// payload mirrors the webhook body sent by hermes.
type payload struct {
	EnvelopeID   string `json:"envelope_id"`
	Status       string `json:"status"`
	TaskTitle    string `json:"task_title"`
	Note         string `json:"note"`
	ProofSummary string `json:"proof_summary"`
	LoopDetected bool   `json:"loop_detected"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := envOr("NOTIFY_ADDR", ":8095")
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	chatID := os.Getenv("TELEGRAM_CHAT_ID")
	threadID := os.Getenv("TELEGRAM_THREAD_ID")
	llmURL := os.Getenv("LLM_URL")
	llmKey := os.Getenv("LLM_API_KEY")
	llmModel := envOr("LLM_MODEL", "anthropic/claude-haiku-4-5")

	tg := newTelegram(token, chatID, threadID)
	llm := newLLMClient(llmURL, llmKey, llmModel)
	rt := newRouter(logger, tg, llm)

	ctx := context.Background()
	go rt.pollCommands(ctx)
	go rt.autoFlush(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", rt.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","mode":"` + rt.Mode() + `"}`))
	})

	logger.Info("hermes-notify starting", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
