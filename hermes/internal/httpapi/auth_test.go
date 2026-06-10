package httpapi

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/keystore"

	_ "modernc.org/sqlite"
)

func newTestServerWithKeys(t *testing.T) (http.Handler, *keystore.Store, *keystore.Key) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ks, err := keystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}

	adminKey, err := ks.Create(context.Background(), "admin", "admin")
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}

	srv := NewServer(discardLogger(), store, nil, nil, nil, ServerOpts{Keys: ks})
	return srv, ks, adminKey
}

func TestAuth_NoKey_Returns401(t *testing.T) {
	srv, _, _ := newTestServerWithKeys(t)

	rec := do(t, srv, http.MethodGet, "/envelopes/test", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_InvalidKey_Returns401(t *testing.T) {
	srv, _, _ := newTestServerWithKeys(t)

	req := newReq(t, http.MethodGet, "/envelopes/test", "")
	req.Header.Set("X-Hermes-Key", "dev-key-bogus")
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_ValidKey_PassesThrough(t *testing.T) {
	srv, _, adminKey := newTestServerWithKeys(t)

	// GET /envelopes/nonexistent — should get 404, not 401.
	req := newReq(t, http.MethodGet, "/envelopes/nonexistent", "")
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("expected pass-through, got 401")
	}
}

func TestAuth_Healthz_NoKeyRequired(t *testing.T) {
	srv, _, _ := newTestServerWithKeys(t)

	rec := do(t, srv, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAdmin_Create_And_List(t *testing.T) {
	srv, _, adminKey := newTestServerWithKeys(t)

	// Create a new key.
	req := newReq(t, http.MethodPost, "/admin/keys", `{"label":"User B","role":"user"}`)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "User B") {
		t.Fatalf("expected User B in response: %s", rec.Body.String())
	}

	// List keys — should have 2 (admin + User B).
	req = newReq(t, http.MethodGet, "/admin/keys", "")
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec = doReq(t, srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "User B") {
		t.Fatalf("expected User B in list: %s", rec.Body.String())
	}
}

func TestKeyAdmin_UserRole_Forbidden(t *testing.T) {
	srv, ks, _ := newTestServerWithKeys(t)

	userKey, _ := ks.Create(context.Background(), "regular", "user")

	req := newReq(t, http.MethodGet, "/admin/keys", "")
	req.Header.Set("X-Hermes-Key", userKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAdmin_Delete(t *testing.T) {
	srv, ks, adminKey := newTestServerWithKeys(t)

	toDelete, _ := ks.Create(context.Background(), "temp", "user")

	req := newReq(t, http.MethodDelete, "/admin/keys/"+toDelete.Key, "")
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAdmin_DeleteNotFound(t *testing.T) {
	srv, _, adminKey := newTestServerWithKeys(t)

	req := newReq(t, http.MethodDelete, "/admin/keys/dev-key-nope", "")
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAdmin_CreateInvalidRole(t *testing.T) {
	srv, _, adminKey := newTestServerWithKeys(t)

	req := newReq(t, http.MethodPost, "/admin/keys", `{"label":"test","role":"superadmin"}`)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAdmin_CreateDuplicateLabel(t *testing.T) {
	srv, _, adminKey := newTestServerWithKeys(t)

	// First create succeeds.
	req := newReq(t, http.MethodPost, "/admin/keys", `{"label":"dup-test","role":"user"}`)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	// Second create with same label → 409.
	req = newReq(t, http.MethodPost, "/admin/keys", `{"label":"dup-test","role":"user"}`)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec = doReq(t, srv, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAdmin_CreateBadJSON(t *testing.T) {
	srv, _, adminKey := newTestServerWithKeys(t)

	req := newReq(t, http.MethodPost, "/admin/keys", `{invalid`)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAdmin_CreateMissingLabel(t *testing.T) {
	srv, _, adminKey := newTestServerWithKeys(t)

	req := newReq(t, http.MethodPost, "/admin/keys", `{"role":"user"}`)
	req.Header.Set("X-Hermes-Key", adminKey.Key)
	rec := doReq(t, srv, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func newReq(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	if body == "" {
		return httptest.NewRequest(method, path, nil)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func doReq(t *testing.T, srv http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

func TestIsPublicPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/dashboard", true},
		{"/dashboard/", true},
		{"/dashboard/index.html", true},
		{"/healthz", false}, // healthz is handled separately
		{"/envelopes", false},
		{"/activity", false},
		{"/dashboard/../admin/keys", false},
	}
	for _, tt := range tests {
		if got := isPublicPath(tt.path); got != tt.want {
			t.Errorf("isPublicPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// newTestDB opens a standalone test database.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
