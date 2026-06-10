// Package hermes is the public facade of the Hermes module.
//
// Production code lives in internal/* packages; this file re-exports
// the minimum surface external callers (examples, e2e tests, future
// embedders) need to boot a Hermes instance without piercing the
// internal barrier.
//
// Keep this file small: each new export is an API commitment.
package hermes

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/legrin-tech/hermes/internal/agentstore"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/httpapi"
	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
	"github.com/legrin-tech/hermes/internal/projectstore"
	"github.com/legrin-tech/hermes/internal/sessionstore"
	"github.com/legrin-tech/hermes/internal/worker"
)

// AgentStore is the live constellation store handle.
type AgentStore = agentstore.Store

// NewAgentStore returns an in-memory agent store with the given TTL
// (0 = 5-minute default).
func NewAgentStore(ttl time.Duration) *AgentStore { return agentstore.New(ttl) }

// Store is the envelope persistence handle.
type Store = envelopestore.Store

// ProjectStore is the project registry handle.
type ProjectStore = projectstore.Store

// OpenStore opens and migrates a SQLite-backed envelope store at dsn.
func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	return envelopestore.Open(ctx, dsn)
}

// OpenProjectStore opens and migrates the project registry at dsn.
func OpenProjectStore(ctx context.Context, dsn string) (*ProjectStore, error) {
	return projectstore.Open(ctx, dsn)
}

// OpenProjectStoreWithDB opens project registry sharing an existing *sql.DB.
func OpenProjectStoreWithDB(ctx context.Context, db *sql.DB) (*ProjectStore, error) {
	return projectstore.OpenWithDB(ctx, db)
}

// NotifyStore is the notification queue handle.
type NotifyStore = notifystore.Store

// OpenNotifyStoreWithDB opens notification store sharing an existing *sql.DB.
func OpenNotifyStoreWithDB(ctx context.Context, db *sql.DB) (*NotifyStore, error) {
	return notifystore.OpenWithDB(ctx, db)
}

// SessionStore is the session lane handle.
type SessionStore = sessionstore.Store

// OpenSessionStoreWithDB opens session store sharing an existing *sql.DB.
func OpenSessionStoreWithDB(ctx context.Context, db *sql.DB) (*SessionStore, error) {
	return sessionstore.OpenWithDB(ctx, db)
}

// KeyStore is the API key registry handle.
type KeyStore = keystore.Store

// OpenKeyStoreWithDB opens key store sharing an existing *sql.DB.
func OpenKeyStoreWithDB(ctx context.Context, db *sql.DB) (*KeyStore, error) {
	return keystore.OpenWithDB(ctx, db)
}

// ServerOpts configures optional Hermes HTTP server components.
type ServerOpts = httpapi.ServerOpts

// NewHTTPHandler returns the Hermes HTTP API (healthz + /envelopes + /registry + /notifications + /sessions).
// Optional stores may be nil (feature disabled).
func NewHTTPHandler(logger *slog.Logger, store *Store, projects *ProjectStore, notifications *NotifyStore, sessions *SessionStore, opts ...ServerOpts) http.Handler {
	if len(opts) > 0 {
		return httpapi.NewServer(logger, store, projects, notifications, sessions, opts[0])
	}
	return httpapi.NewServer(logger, store, projects, notifications, sessions)
}

// Worker is the delivery worker. Alias to the internal type so external
// callers can set fields via a struct literal.
type Worker = worker.Worker

// NewHTTPForgeClient builds a Forge client that speaks the /deliver
// wire format. Returned as the concrete internal type, but typically
// assigned to a Worker.Client field (which is an interface).
func NewHTTPForgeClient(baseURL string) *worker.HTTPForgeClient {
	return worker.NewHTTPForgeClient(baseURL)
}
