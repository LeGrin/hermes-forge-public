package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
	"github.com/legrin-tech/hermes/internal/sessionstore"

	_ "modernc.org/sqlite"
)

//go:embed dashboard
var testDashboard embed.FS

const (
	hermesKeyHeader = "X-Hermes-Key"
	testKeyBName    = "test-key-b"
	// pubSessDirectID is the session ID used in public-namespace ownership tests.
	// Extracted to avoid duplicate-literal lint warnings.
	pubSessDirectID = "pub-sess-direct"
)

func newTestServerWithSession(t *testing.T) (http.Handler, *sessionstore.Store) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	envStore, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		db.Close()
		t.Fatalf("open envelope store: %v", err)
	}

	notifyStore, err := notifystore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("open notify store: %v", err)
	}

	sessStore, err := sessionstore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("open session store: %v", err)
	}

	srv := NewServer(discardLogger(), envStore, nil, notifyStore, sessStore)
	return srv, sessStore
}

func TestCreateSession(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	body := `{"title": "Test Session", "project": "test-project"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var sess map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if sess["title"] != "Test Session" {
		t.Errorf("title: got %v, want %v", sess["title"], "Test Session")
	}
	if sess["status"] != "active" {
		t.Errorf("status: got %v, want %v", sess["status"], "active")
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Error("expected Location header")
	}
}

func TestCreateSession_BadJSON(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestListSessions(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	// Create a session first.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d", rec.Code)
	}

	// List sessions.
	req = httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var sessions []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("len: got %d, want 1", len(sessions))
	}
}

func TestGetSession(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	// Create a session.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d", rec.Code)
	}

	// Extract session ID from location.
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("no Location header")
	}

	// Get the session.
	req = httptest.NewRequest(http.MethodGet, loc, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var sess map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sess["title"] != "S1" {
		t.Errorf("title: got %v, want S1", sess["title"])
	}
}

func TestGetSession_NotFound(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	req := httptest.NewRequest(http.MethodGet, "/sessions/nonexistent", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestSessionRawTail_NotAvailableAndBounded(t *testing.T) {
	srv, _ := newTestServerWithSession(t)
	loc := createSessionForTest(t, srv, "S1", "p1", "")

	req := httptest.NewRequest(http.MethodGet, loc+"/raw-tail?max_bytes=999999&max_lines=999999", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "not_available" {
		t.Fatalf("expected not_available status, got %s", rec.Body.String())
	}
	if got["max_bytes"] != float64(65536) || got["max_lines"] != float64(200) {
		t.Fatalf("expected bounded max values, got %s", rec.Body.String())
	}
}

func TestSessionRawTail_CrossKeyReturns404(t *testing.T) {
	srv, _, ks, keyA := newTestServerWithSessionAndKeys(t)
	keyB, err := ks.Create(context.Background(), testKeyBName, "user")
	if err != nil {
		t.Fatalf("create key b: %v", err)
	}
	loc := createSessionForTest(t, srv, "S1", "p1", keyA.Key)

	req := httptest.NewRequest(http.MethodGet, loc+"/raw-tail", nil)
	req.Header.Set(hermesKeyHeader, keyB.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSessionRawTail_NoAuthReturns401(t *testing.T) {
	srv, _, _, _ := newTestServerWithSessionAndKeys(t)
	req := httptest.NewRequest(http.MethodGet, "/sessions/missing/raw-tail", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func createSessionForTest(t *testing.T, srv http.Handler, title, project, key string) string {
	t.Helper()
	body := `{"title":"` + title + `","project":"` + project + `"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(hermesKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session failed: %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Header().Get("Location")
}

func TestAddMessage(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	// Create a session.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d", rec.Code)
	}

	// Add a message.
	loc := rec.Header().Get("Location")
	msgBody := `{"from": "opencode", "kind": "reply", "text": "Hello KITT"}`
	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg["text"] != "Hello KITT" {
		t.Errorf("text: got %v, want Hello KITT", msg["text"])
	}
	if msg["from"] != "opencode" {
		t.Errorf("from: got %v, want opencode", msg["from"])
	}
}

func TestAddMessage_InvalidKind(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	// Create a session.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// Add message with invalid kind.
	loc := rec.Header().Get("Location")
	msgBody := `{"from": "opencode", "kind": "invalid", "text": "Hello"}`
	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestAddMessage_MissingFrom(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	// Create a session.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// Add message without from.
	loc := rec.Header().Get("Location")
	msgBody := `{"kind": "reply", "text": "Hello"}`
	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestGetMessages(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	// Create a session.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	loc := rec.Header().Get("Location")

	// Add messages.
	for i := 0; i < 3; i++ {
		msgBody := `{"from": "claude", "kind": "reply", "text": "Message ` + string(rune('0'+i)) + `"}`
		req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("add message %d failed: %d", i, rec.Code)
		}
	}

	// Get messages.
	req = httptest.NewRequest(http.MethodGet, loc+"/messages", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var messages []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &messages); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(messages) != 3 {
		t.Errorf("len: got %d, want 3", len(messages))
	}
}

func TestGetMessages_SinceID(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	// Create a session.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	loc := rec.Header().Get("Location")

	// Add 3 messages.
	var msgIDs []string
	for i := 0; i < 3; i++ {
		msgBody := `{"from": "opencode", "kind": "reply", "text": "Msg ` + string(rune('0'+i)) + `"}`
		req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		var msg map[string]any
		json.Unmarshal(rec.Body.Bytes(), &msg)
		msgIDs = append(msgIDs, msg["id"].(string))
	}

	// Get all messages first.
	req = httptest.NewRequest(http.MethodGet, loc+"/messages", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var allMessages []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &allMessages)
	if len(allMessages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(allMessages))
	}

	// Get messages after first using the first message's ID.
	firstID := allMessages[0]["id"].(string)
	req = httptest.NewRequest(http.MethodGet, loc+"/messages?since_id="+firstID, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var messages []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &messages)
	if len(messages) != 2 {
		t.Errorf("len: got %d, want 2", len(messages))
	}
}

// newTestServerWithSessionAndKeys creates a server with both session store and key auth.
func newTestServerWithSessionAndKeys(t *testing.T) (http.Handler, *sessionstore.Store, *keystore.Store, *keystore.Key) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	envStore, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		db.Close()
		t.Fatalf("open envelope store: %v", err)
	}

	notifyStore, err := notifystore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("open notify store: %v", err)
	}

	sessStore, err := sessionstore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("open session store: %v", err)
	}

	ks, err := keystore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("open keystore: %v", err)
	}

	key, err := ks.Create(context.Background(), "test-key-a", "user")
	if err != nil {
		db.Close()
		t.Fatalf("create key: %v", err)
	}

	srv := NewServer(discardLogger(), envStore, nil, notifyStore, sessStore, ServerOpts{Keys: ks})
	return srv, sessStore, ks, key
}

// TestAddMessage_MissingSession_Returns404 verifies POST /sessions/{id}/messages
// fails with 404 for a nonexistent session (store-level enforcement).
func TestAddMessage_MissingSession_Returns404(t *testing.T) {
	srv, _, _, key := newTestServerWithSessionAndKeys(t)

	msgBody := `{"from": "opencode", "kind": "reply", "text": "hello"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/nonexistent/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, key.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSessionOwnership_CrossKeyGetSession tests that GET /sessions/{id} returns 404
// when a different key is used than the one that created the session.
func TestSessionOwnership_CrossKeyGetSession(t *testing.T) {
	srv, sessStore, ks, keyA := newTestServerWithSessionAndKeys(t)

	// Create session with key A.
	keyB, _ := ks.Create(context.Background(), testKeyBName, "user")

	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d", rec.Code)
	}
	loc := rec.Header().Get("Location")

	// Extract session ID.
	sessID := loc[strings.LastIndex(loc, "/"):]
	_, _ = sessStore.Get(context.Background(), sessID[1:]) // confirm it exists

	// Try to read with key B → should get 404.
	req = httptest.NewRequest(http.MethodGet, loc, nil)
	req.Header.Set(hermesKeyHeader, keyB.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-key read, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify key A can still read it.
	req = httptest.NewRequest(http.MethodGet, loc, nil)
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for owner read, got %d", rec.Code)
	}
}

// TestSessionOwnership_CrossKeyAddMessage tests that POST /sessions/{id}/messages
// returns 404 when a different key is used than the one that created the session.
func TestSessionOwnership_CrossKeyAddMessage(t *testing.T) {
	srv, _, ks, keyA := newTestServerWithSessionAndKeys(t)

	keyB, _ := ks.Create(context.Background(), testKeyBName, "user")

	// Create session with key A.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d", rec.Code)
	}
	loc := rec.Header().Get("Location")

	// Try to add message with key B → should get 404.
	msgBody := `{"from": "opencode", "kind": "reply", "text": "hello"}`
	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, keyB.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-key write, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify key A can still write.
	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for owner write, got %d", rec.Code)
	}
}

// TestSessionOwnership_CrossKeyGetMessages tests that GET /sessions/{id}/messages
// returns 404 when a different key is used than the one that created the session.
func TestSessionOwnership_CrossKeyGetMessages(t *testing.T) {
	srv, _, ks, keyA := newTestServerWithSessionAndKeys(t)

	keyB, _ := ks.Create(context.Background(), testKeyBName, "user")

	// Create session with key A.
	body := `{"title": "S1", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failed: %d", rec.Code)
	}
	loc := rec.Header().Get("Location")

	// Try to read messages with key B → should get 404.
	req = httptest.NewRequest(http.MethodGet, loc+"/messages", nil)
	req.Header.Set(hermesKeyHeader, keyB.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-key read messages, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify key A can still read.
	req = httptest.NewRequest(http.MethodGet, loc+"/messages", nil)
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for owner read, got %d", rec.Code)
	}
}

// TestCreateSession_CreatedAtRoundTrip verifies that created_at returned from POST /sessions
// is the same RFC3339 value returned from GET /sessions and GET /sessions/{id}.
func TestCreateSession_CreatedAtRoundTrip(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	body := `{"title": "Test Session", "project": "test-project"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	createdAt, ok := created["created_at"].(string)
	if !ok {
		t.Fatal("created_at missing or not a string")
	}
	if createdAt == "" {
		t.Fatal("created_at should not be empty")
	}

	// Verify created_at is valid RFC3339.
	parsed, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("created_at is not RFC3339: %v, value: %s", err, createdAt)
	}
	if parsed.IsZero() {
		t.Fatal("parsed created_at is zero")
	}

	// Fetch via GET /sessions/{id} and verify same created_at.
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("Location header missing")
	}

	req = httptest.NewRequest(http.MethodGet, loc, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET session failed: %d: %s", rec.Code, rec.Body.String())
	}

	var fetched map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode fetched session: %v", err)
	}

	fetchedAt, ok := fetched["created_at"].(string)
	if !ok {
		t.Fatal("created_at missing or not a string in fetched session")
	}
	if fetchedAt != createdAt {
		t.Errorf("created_at mismatch: POST returned %s, GET returned %s", createdAt, fetchedAt)
	}

	// Fetch via GET /sessions (list) and verify same created_at.
	req = httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions failed: %d: %s", rec.Code, rec.Body.String())
	}

	var sessions []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}

	listAt, ok := sessions[0]["created_at"].(string)
	if !ok {
		t.Fatal("created_at missing or not a string in session list")
	}
	if listAt != createdAt {
		t.Errorf("created_at mismatch: POST returned %s, GET /sessions returned %s", createdAt, listAt)
	}
}

// TestListSessions_CrossKeyOwnership verifies GET /sessions only returns sessions
// owned by the caller's API key when auth is present.
func TestListSessions_CrossKeyOwnership(t *testing.T) {
	srv, _, ks, keyA := newTestServerWithSessionAndKeys(t)

	keyB, _ := ks.Create(context.Background(), testKeyBName, "user")

	// Create two sessions with key A.
	for i := 0; i < 2; i++ {
		body := `{"title": "A` + string(rune('0'+i)) + `", "project": "p"}`
		req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(hermesKeyHeader, keyA.Key)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create A%d failed: %d", i, rec.Code)
		}
	}

	// Create one session with key B.
	body := `{"title": "B0", "project": "p"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, keyB.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create B failed: %d", rec.Code)
	}

	// List with key A → should see only A's sessions (2).
	req = httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list with keyA failed: %d: %s", rec.Code, rec.Body.String())
	}

	var sessionsA []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionsA); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sessionsA) != 2 {
		t.Errorf("keyA: expected 2 sessions, got %d", len(sessionsA))
	}

	// List with key B → should see only B's session (1).
	req = httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set(hermesKeyHeader, keyB.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("list with keyB failed: %d: %s", rec.Code, rec.Body.String())
	}

	var sessionsB []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionsB); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sessionsB) != 1 {
		t.Errorf("keyB: expected 1 session, got %d", len(sessionsB))
	}

	// Verify keyB's session has the correct title.
	if sessionsB[0]["title"] != "B0" {
		t.Errorf("keyB session title: got %v, want B0", sessionsB[0]["title"])
	}
}

// newTestServerWithSessionTG creates a server with Telegram delivery plumbed
// through ServerOpts. It intercepts Telegram API calls via a local httptest
// server by overriding telegramAPIBaseURL for the duration of the test.
func newTestServerWithSessionTG(t *testing.T) (http.Handler, *sessionstore.Store, <-chan string) {
	t.Helper()

	calls := make(chan string, 8)
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls <- string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(tgSrv.Close)

	prev := telegramAPIBaseURL
	telegramAPIBaseURL = tgSrv.URL
	t.Cleanup(func() { telegramAPIBaseURL = prev })

	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	envStore, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		db.Close()
		t.Fatalf("open envelope store: %v", err)
	}
	notifyStore, err := notifystore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("open notify store: %v", err)
	}
	sessStore, err := sessionstore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("open session store: %v", err)
	}

	srv := NewServer(discardLogger(), envStore, nil, notifyStore, sessStore, ServerOpts{
		TGToken: "test-token",
		TGChat:  "123",
	})
	return srv, sessStore, calls
}

// TestAddMessage_TelegramOnDecision asserts that a session "decision" message
// from an executor (opencode/claude) triggers a fire-and-forget Telegram send.
func TestAddMessage_TelegramOnDecision(t *testing.T) {
	srv, _, calls := newTestServerWithSessionTG(t)

	body := `{"title": "Build session", "project": "hermes"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d", rec.Code)
	}
	loc := rec.Header().Get("Location")

	msgBody := `{"from": "opencode", "kind": "decision", "text": "picked Go 1.26 for runtime"}`
	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add message: %d %s", rec.Code, rec.Body.String())
	}

	select {
	case payload := <-calls:
		if !strings.Contains(payload, "picked Go 1.26") {
			t.Errorf("payload missing message text: %s", payload)
		}
		if !strings.Contains(payload, "Build session") {
			t.Errorf("payload missing session title: %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("telegram send not observed within 2s")
	}
}

// TestAddMessage_NoTelegramForKitt asserts that KITT-authored messages and
// non-decision kinds do not trigger Telegram — prevents feedback spam.
func TestAddMessage_NoTelegramForKitt(t *testing.T) {
	srv, _, calls := newTestServerWithSessionTG(t)

	body := `{"title": "Quiet session", "project": "hermes"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d", rec.Code)
	}
	loc := rec.Header().Get("Location")

	cases := []string{
		`{"from": "kitt", "kind": "decision", "text": "user typed this"}`,
		`{"from": "opencode", "kind": "reply", "text": "ack"}`,
		`{"from": "claude", "kind": "steer", "text": "please continue"}`,
	}
	for _, body := range cases {
		req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("add message (%s): %d %s", body, rec.Code, rec.Body.String())
		}
	}

	select {
	case payload := <-calls:
		t.Fatalf("unexpected telegram send: %s", payload)
	case <-time.After(250 * time.Millisecond):
		// no send — expected
	}
}

// TestAddMessage_BadJSON covers the decoder-error branch in addMessage —
// malformed body must become 400 without touching the store.
func TestAddMessage_BadJSON(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	body := `{"title": "S", "project": "p"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	loc := rec.Header().Get("Location")

	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(`{not-json`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestAddMessage_MissingText covers the empty-text validation branch.
func TestAddMessage_MissingText(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	body := `{"title": "S", "project": "p"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	loc := rec.Header().Get("Location")

	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(`{"from":"claude","kind":"reply"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

// TestGetMessages_SinceUnknownID returns an empty list when since_id is not
// found (rather than 404) so clients can tolerate crash/resume races.
func TestGetMessages_SinceUnknownID(t *testing.T) {
	srv, _ := newTestServerWithSession(t)

	body := `{"title": "S", "project": "p"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	loc := rec.Header().Get("Location")

	req = httptest.NewRequest(http.MethodGet, loc+"/messages?since_id=nope", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var out []any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out) != 0 {
		t.Errorf("expected empty list, got %d", len(out))
	}
}

// TestAddMessage_WebhookOnDecision verifies that the prod-path (no TG token,
// only a webhook URL pointing at OpenClaw) also receives session decisions.
// Prevents regression where a deployment without direct Telegram credentials
// would silently drop executor decisions.
func TestAddMessage_WebhookOnDecision(t *testing.T) {
	calls := make(chan map[string]any, 4)
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		calls <- p
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hookSrv.Close)

	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	envStore, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		db.Close()
		t.Fatalf("envstore: %v", err)
	}
	notifyStore, err := notifystore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("notifystore: %v", err)
	}
	sessStore, err := sessionstore.OpenWithDB(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatalf("sessionstore: %v", err)
	}
	srv := NewServer(discardLogger(), envStore, nil, notifyStore, sessStore, ServerOpts{WebhookURL: hookSrv.URL})

	body := `{"title": "Deploy check", "project": "hermes"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d", rec.Code)
	}
	loc := rec.Header().Get("Location")

	msgBody := `{"from": "claude", "kind": "decision", "text": "chose SQLite trigger"}`
	req = httptest.NewRequest(http.MethodPost, loc+"/messages", bytes.NewBufferString(msgBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add message: %d %s", rec.Code, rec.Body.String())
	}

	select {
	case p := <-calls:
		if p["status"] != "session_decision" {
			t.Errorf("status: got %v, want session_decision", p["status"])
		}
		if title, _ := p["task_title"].(string); title != "Deploy check" {
			t.Errorf("task_title: got %v, want Deploy check", p["task_title"])
		}
		if note, _ := p["note"].(string); !strings.Contains(note, "chose SQLite trigger") {
			t.Errorf("note missing text: %v", p["note"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook not invoked within 2s")
	}
}

// TestSessionOwnership_UnauthenticatedCannotAccessKeyedSession verifies that
// when a keystore is configured, an unauthenticated request (no X-Hermes-Key)
// cannot access a session that belongs to a specific API key.
// The middleware returns 401; checkSessionOwnership provides defence-in-depth
// (returns 404) if the middleware is ever bypassed.
func TestSessionOwnership_UnauthenticatedCannotAccessKeyedSession(t *testing.T) {
	srv, _, _, keyA := newTestServerWithSessionAndKeys(t)

	// Create a session owned by keyA.
	body := `{"title": "Owned Session", "project": "p1"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")

	// Unauthenticated GET /sessions/{id} → middleware returns 401.
	req = httptest.NewRequest(http.MethodGet, loc, nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated access to keyed session, got %d: %s", rec.Code, rec.Body.String())
	}

	// Unauthenticated GET /sessions/{id}/raw-tail → middleware returns 401.
	req = httptest.NewRequest(http.MethodGet, loc+"/raw-tail", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated raw-tail on keyed session, got %d: %s", rec.Code, rec.Body.String())
	}

	// Unauthenticated GET /sessions/{id}/messages → middleware returns 401.
	req = httptest.NewRequest(http.MethodGet, loc+"/messages", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated get messages on keyed session, got %d: %s", rec.Code, rec.Body.String())
	}

	// Authenticated owner can still access.
	req = httptest.NewRequest(http.MethodGet, loc, nil)
	req.Header.Set(hermesKeyHeader, keyA.Key)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for owner access, got %d", rec.Code)
	}
}

// TestSessionOwnership_CheckOwnership_KeystoreMode_NoContextKey verifies that
// checkSessionOwnership itself (defence-in-depth) returns 404 for a keyed
// session when called with no context key and keystore configured.
// This covers the case where the middleware is bypassed (e.g. future public path).
func TestSessionOwnership_CheckOwnership_KeystoreMode_NoContextKey(t *testing.T) {
	_, sessStore, ks, _ := newTestServerWithSessionAndKeys(t)

	// Build a handler that calls checkSessionOwnership directly without auth middleware.
	h := &sessionHandler{
		store:  sessStore,
		keys:   ks,
		logger: discardLogger(),
	}

	// Insert a keyed session directly.
	keyedSess := &sessionstore.Session{
		ID:      "keyed-sess-direct",
		Title:   "Keyed",
		Project: "p1",
		APIKey:  "some-key",
		Status:  "active",
	}
	if err := sessStore.Insert(context.Background(), keyedSess); err != nil {
		t.Fatalf("insert keyed session: %v", err)
	}

	// Call checkSessionOwnership with no context key (simulates middleware bypass).
	req := httptest.NewRequest(http.MethodGet, "/sessions/keyed-sess-direct", nil)
	rec := httptest.NewRecorder()
	result := h.checkSessionOwnership(rec, req, "keyed-sess-direct")
	if result != nil {
		t.Fatalf("expected nil (blocked), got session %+v", result)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from checkSessionOwnership, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSessionOwnership_UnauthenticatedCanAccessPublicSession verifies that
// in keystore mode, unauthenticated requests can still access sessions
// with an empty api_key (public namespace sessions) IF they bypass the middleware.
// In practice the middleware blocks them at 401; this tests the handler logic directly.
func TestSessionOwnership_UnauthenticatedCanAccessPublicSession(t *testing.T) {
	_, sessStore, ks, _ := newTestServerWithSessionAndKeys(t)

	h := &sessionHandler{
		store:  sessStore,
		keys:   ks,
		logger: discardLogger(),
	}

	// Insert a session with empty api_key (public namespace).
	pubSess := &sessionstore.Session{
		ID:      pubSessDirectID,
		Title:   "Public Session",
		Project: "p1",
		APIKey:  "",
		Status:  "active",
	}
	if err := sessStore.Insert(context.Background(), pubSess); err != nil {
		t.Fatalf("insert public session: %v", err)
	}

	// No context key → public session must be accessible.
	req := httptest.NewRequest(http.MethodGet, "/sessions/" + pubSessDirectID, nil)
	rec := httptest.NewRecorder()
	result := h.checkSessionOwnership(rec, req, pubSessDirectID)
	if result == nil {
		t.Fatalf("expected session returned for public namespace, got nil (status %d: %s)", rec.Code, rec.Body.String())
	}
	if result.ID != pubSessDirectID {
		t.Errorf("wrong session returned: %v", result.ID)
	}
}
