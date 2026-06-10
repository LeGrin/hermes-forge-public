// Package httpapi wires Forge HTTP routes.
//
// Structural invariant (W-H12): this package MUST NOT import any package
// under github.com/legrin-tech/hermes. Forge is an independent module —
// the only contract between it and Hermes is the wire format of /deliver.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/legrin-tech/forge/internal/runner"
	"github.com/legrin-tech/forge/internal/sessionstore"
)

// agentLinkStore is package-level so the reporter can query it without
// threading it through the constructor.
var globalAgentLinkStore = &agentLinkStore{}

// NewServer returns an http.Handler with Forge routes mounted.
// store may be nil only in tests that exercise routes not touching
// persistence (e.g. /healthz). launcher spawns executor processes
// on delivery; if nil, a stub launcher is used (for backward compat
// in tests that don't care about process spawning). hermesURL and
// hermesKey are optional; if set, the hermesClient will be created
// and used to report executor_session_id back to Hermes after delivery.
func NewServer(logger *slog.Logger, store *sessionstore.Store, launcher Launcher, hermesURL, hermesKey string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)

	// Always register /agent/link — it is needed even when store is nil
	// (OpenCode plugin calls it before any session is created).
	lh := &agentLinkHandler{logger: logger, store: globalAgentLinkStore}
	lh.register(mux)

	if store != nil {
		registry := newProcessRegistry(logger)

		var hc *hermesClient
		if hermesURL != "" {
			hc = newHermesClient(hermesURL, hermesKey)
		}

		d := newDeliverHandlerWithHermes(logger, store, launcher, registry, hc, defaultOpenCodeURL)
		d.register(mux)

		sh := &sessionHandler{logger: logger, store: store, registry: registry}
		sh.register(mux)

		nh := newNotifyHandler(logger, registry, store, defaultOpenCodeURL)
		nh.register(mux)

		rh := newResumeHandler(logger, store, registry, launcher, defaultOpenCodeURL)
		rh.register(mux)
	}

	return withLogging(logger, mux)
}

// AgentLinkStore returns the package-level agent link store so the
// reporter can query it during Scan().
func AgentLinkStore() *agentLinkStore {
	return globalAgentLinkStore
}

// StubLauncher returns a Launcher that spawns "cat" — stays alive,
// echoes stdin to stdout. Useful for tests and as a fallback.
func StubLauncher() Launcher {
	return func(_, _, _, _ string, _ bool) (*runner.Process, error) {
		p := runner.New("cat")
		if err := p.Start(); err != nil {
			return nil, err
		}
		return p, nil
	}
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, kind, detail string) {
	writeJSON(w, status, map[string]string{
		"error":  kind,
		"detail": detail,
	})
}
