package httpapi

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/legrin-tech/hermes/internal/keystore"
)

type ctxKey int

const ctxAPIKey ctxKey = iota

// KeyFromContext returns the authenticated API key, or nil if not set.
func KeyFromContext(ctx context.Context) *keystore.Key {
	k, _ := ctx.Value(ctxAPIKey).(*keystore.Key)
	return k
}

// authMiddleware validates X-Hermes-Key on every request.
// Healthz is exempt. Admin endpoints require role=admin.
func authMiddleware(keys *keystore.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public paths exempt from auth.
		if r.URL.Path == "/healthz" || isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// If no keystore configured, pass through (backward compat / tests).
		if keys == nil {
			next.ServeHTTP(w, r)
			return
		}

		raw := r.Header.Get("X-Hermes-Key")
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "auth_required", "X-Hermes-Key header is required")
			return
		}

		k, err := keys.Get(r.Context(), raw)
		if errors.Is(err, keystore.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid_key", "invalid API key")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "auth_error", msgInternalError)
			return
		}

		ctx := context.WithValue(r.Context(), ctxAPIKey, k)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isPublicPath returns true for paths that don't require auth.
func isPublicPath(p string) bool {
	p = path.Clean(p)
	return p == "/dashboard" || strings.HasPrefix(p, "/dashboard/") ||
		p == "/icons" || strings.HasPrefix(p, "/icons/")
}
