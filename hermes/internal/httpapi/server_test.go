package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legrin-tech/hermes/internal/envelopestore"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T) (http.Handler, *envelopestore.Store) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewServer(discardLogger(), store, nil, nil, nil), store
}

func TestHealthz_OK(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected json content-type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestHealthz_WithDB(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestHealthz_DBDown(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	srv := NewServer(discardLogger(), store, nil, nil, nil)
	// Close the underlying DB to simulate unreachable database.
	_ = store.Close()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("expected error in body, got %s", rec.Body.String())
	}
}

func TestDashboard_ServesHTML(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "Hermes Dashboard") {
		t.Fatalf("expected dashboard content, got %s", rec.Body.String()[:100])
	}
}

func TestDashboard_NoAuthRequired(t *testing.T) {
	srv, _, _ := newTestServerWithActivity(t)

	// Dashboard should work without X-Hermes-Key header even with auth enabled.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (no auth needed), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDashboard_Redirect(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", rec.Code)
	}
}

func TestHealthz_MethodNotAllowed(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestIcons_ServesRealImage(t *testing.T) {
	// Create a temporary icons directory with a real PNG file.
	iconsDir := t.TempDir()
	// Write a minimal valid PNG (1x1 transparent pixel).
	pngData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41, // IDAT chunk
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, // IEND chunk
		0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(filepath.Join(iconsDir, "kitt.png"), pngData, 0644); err != nil {
		t.Fatalf("write test icon: %v", err)
	}

	srv, _ := newTestServerWithIcons(t, iconsDir)

	req := httptest.NewRequest(http.MethodGet, "/icons/kitt.png", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "image/png") {
		t.Fatalf("expected image/png content-type, got %q", ct)
	}
	// Verify PNG magic bytes in response.
	body := rec.Body.Bytes()
	if len(body) < 8 || body[0] != 0x89 || body[1] != 0x50 || body[2] != 0x4E || body[3] != 0x47 {
		t.Fatalf("expected PNG magic bytes in body, got %x", body[:8])
	}
}

func TestIcons_404ForUnknown(t *testing.T) {
	iconsDir := t.TempDir()
	srv, _ := newTestServerWithIcons(t, iconsDir)

	req := httptest.NewRequest(http.MethodGet, "/icons/nonexistent.png", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestIcons_NoAuthRequired(t *testing.T) {
	iconsDir := t.TempDir()
	pngData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(filepath.Join(iconsDir, "test.png"), pngData, 0644); err != nil {
		t.Fatalf("write test icon: %v", err)
	}
	srv, _ := newTestServerWithIcons(t, iconsDir)

	// Access without X-Hermes-Key should work (icons are public).
	req := httptest.NewRequest(http.MethodGet, "/icons/test.png", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (no auth), got %d: %s", rec.Code, rec.Body.String())
	}
}

func newTestServerWithIcons(t *testing.T, iconsDir string) (http.Handler, *envelopestore.Store) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewServer(discardLogger(), store, nil, nil, nil, ServerOpts{IconsDir: iconsDir}), store
}
