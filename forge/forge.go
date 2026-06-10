// Package forge is the public facade of the Forge module.
//
// Production code lives in internal/* packages; this file re-exports
// the minimum surface external callers (examples, e2e tests) need to
// boot a Forge instance without piercing the internal barrier.
//
// Keep this file small: each new export is an API commitment.
package forge

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/legrin-tech/forge/internal/httpapi"
	"github.com/legrin-tech/forge/internal/runner"
	"github.com/legrin-tech/forge/internal/sessionstore"
)

// SessionStore is the public alias for sessionstore.Store.
type SessionStore = sessionstore.Store

// Process is the public alias for runner.Process.
type Process = runner.Process

// Launcher spawns an executor process. See httpapi.Launcher.
type Launcher = httpapi.Launcher

// OpenSessionStore opens and migrates a SQLite-backed session store.
func OpenSessionStore(ctx context.Context, dsn string) (*SessionStore, error) {
	return sessionstore.Open(ctx, dsn)
}

// NewHTTPHandler returns the Forge HTTP API (healthz + /deliver + /sessions).
// launcher spawns executor processes on delivery; pass nil for a stub
// launcher that spawns "cat" (useful for tests). hermesURL and hermesKey
// are optional; if set the handler will report executor_session_id back.
func NewHTTPHandler(logger *slog.Logger, store *SessionStore, launcher Launcher, hermesURL, hermesKey string) http.Handler {
	if launcher == nil {
		launcher = httpapi.StubLauncher()
	}
	return httpapi.NewServer(logger, store, launcher, hermesURL, hermesKey)
}
