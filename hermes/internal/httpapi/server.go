// Package httpapi wires Hermes HTTP routes.
package httpapi

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/legrin-tech/hermes/internal/activityhub"
	"github.com/legrin-tech/hermes/internal/agentstore"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
	"github.com/legrin-tech/hermes/internal/projectstore"
	"github.com/legrin-tech/hermes/internal/sessionstore"
)

// ServerOpts configures the Hermes HTTP server. All fields are optional.
type ServerOpts struct {
	Keys          *keystore.Store
	Activity      *activityhub.Hub
	Agents        *agentstore.Store // nil-safe: /agents endpoints disabled
	WebhookURL    string            // optional: fired on interesting envelope status changes
	WebhookSecret string            // optional: Authorization Bearer token for the webhook
	TGToken       string            // optional: Telegram bot token for direct notification delivery
	TGChat        string            // optional: Telegram chat ID for direct notification delivery
	TGThread      string            // optional: Telegram message_thread_id for topic routing (supergroups)
	IconsDir      string            // optional: path to deploy/icons directory for /icons/ serving
}

// NewServer returns an http.Handler with Hermes routes mounted.
// The /healthz route is always registered and exempt from auth.
// Pass nil for any store that is not needed (tests, partial setups).
func NewServer(logger *slog.Logger, envelopes *envelopestore.Store, projects *projectstore.Store, notifications *notifystore.Store, sessions *sessionstore.Store, opts ...ServerOpts) http.Handler {
	var o ServerOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", makeHealthz(envelopes))

	firer := newWebhookFirer(logger, o)

	if envelopes != nil {
		h := &envelopeHandler{store: envelopes, notify: notifications, keys: o.Keys, logger: logger, firer: firer, hub: o.Activity}
		h.register(mux)
	}
	if projects != nil {
		r := &registryHandler{store: projects, envelopes: envelopes, logger: logger}
		r.register(mux)
	}
	if notifications != nil {
		n := &notificationHandler{store: notifications, keys: o.Keys, logger: logger}
		n.register(mux)
	}
	if sessions != nil {
		s := &sessionHandler{
			store:  sessions,
			notify: notifications,
			keys:   o.Keys,
			logger: logger,
			firer:  firer,
		}
		s.register(mux)
	}
	if o.Keys != nil {
		k := &keyHandler{store: o.Keys, logger: logger}
		k.register(mux)
	}
	if o.Activity != nil {
		a := &activityHandler{hub: o.Activity, logger: logger}
		a.register(mux)
	}
	if o.Agents != nil {
		ah := &agentHandler{store: o.Agents, hub: o.Activity, logger: logger}
		ah.register(mux)
	}
	// KITT container logs endpoint (CON-003).
	k := newKittLogsHandler()
	k.register(mux)
	// Dashboard is public (auth handled client-side via API key in JS).
	dashFS, err := fs.Sub(dashboardFiles, "dashboard")
	if err != nil {
		panic("dashboard files missing: " + err.Error())
	}
	mux.Handle("GET /dashboard", http.RedirectHandler("/dashboard/", http.StatusFound))
	mux.Handle("GET /dashboard/", http.StripPrefix("/dashboard/", http.FileServerFS(dashFS)))

	// Icons are served publicly (no auth required).
	if o.IconsDir != "" {
		IconsDir = o.IconsDir
	}
	mux.Handle("GET /icons/", iconsHandler())

	return withLogging(logger, authMiddleware(o.Keys, mux))
}

//go:embed dashboard
var dashboardFiles embed.FS

// makeHealthz returns a healthz handler that pings the DB if available.
func makeHealthz(store *envelopestore.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store != nil {
			if err := store.DB().PingContext(r.Context()); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "error",
					"detail": "database unreachable",
				})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// statusWriter wraps ResponseWriter to capture the HTTP status code.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.code = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter so SSE streaming works
// through the logging middleware.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.code,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// writeJSON writes v as JSON with the given status. Best-effort — encoder
// errors are logged by the caller's middleware, not surfaced to the client.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a structured error response. kind is a short machine
// code (e.g. "invalid_envelope"), detail is the human-readable message.
func writeError(w http.ResponseWriter, status int, kind, detail string) {
	writeJSON(w, status, map[string]string{
		"error":  kind,
		"detail": detail,
	})
}
