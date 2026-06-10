package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/envelope"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/projectstore"
)

func newTestServerWithRegistry(t *testing.T) http.Handler {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	ps, err := projectstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open projectstore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	return NewServer(discardLogger(), nil, ps, nil, nil)
}

// newTestServerWithBothStores creates a server with both project and envelope stores.
func newTestServerWithBothStores(t *testing.T) (http.Handler, *projectstore.Store, *envelopestore.Store) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	es, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open envelopestore: %v", err)
	}
	ps, err := projectstore.OpenWithDB(context.Background(), es.DB())
	if err != nil {
		t.Fatalf("open projectstore: %v", err)
	}
	t.Cleanup(func() {
		_ = es.Close()
		_ = ps.Close()
	})
	return NewServer(discardLogger(), es, ps, nil, nil), ps, es
}

func TestRegistryList_Empty(t *testing.T) {
	srv := newTestServerWithRegistry(t)
	req := httptest.NewRequest(http.MethodGet, "/registry/projects", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Fatalf("expected [], got %s", body)
	}
}

func TestRegistryCreate_And_List(t *testing.T) {
	srv := newTestServerWithRegistry(t)

	// Create
	body := `{"project":"hermes","domain":"ops","working_dir":"/tmp/hermes"}`
	req := httptest.NewRequest(http.MethodPost, "/registry/projects", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"working_dir":"/tmp/hermes"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	// List
	req = httptest.NewRequest(http.MethodGet, "/registry/projects", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"hermes"`) {
		t.Fatalf("missing project in list: %s", rec.Body.String())
	}
}

func TestRegistryCreate_Duplicate(t *testing.T) {
	srv := newTestServerWithRegistry(t)
	body := `{"project":"x","domain":"d","working_dir":"/tmp"}`

	req := httptest.NewRequest(http.MethodPost, "/registry/projects", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first insert: expected 201, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/registry/projects", strings.NewReader(body))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: expected 409, got %d", rec.Code)
	}
}

func TestRegistryCreate_RelativePath(t *testing.T) {
	srv := newTestServerWithRegistry(t)
	body := `{"project":"x","working_dir":"relative/path"}`
	req := httptest.NewRequest(http.MethodPost, "/registry/projects", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for relative path, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegistryCreate_InvalidJSON(t *testing.T) {
	srv := newTestServerWithRegistry(t)
	req := httptest.NewRequest(http.MethodPost, "/registry/projects", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRegistryCreate_Defaults(t *testing.T) {
	srv := newTestServerWithRegistry(t)
	body := `{"project":"y","working_dir":"/tmp/y"}`
	req := httptest.NewRequest(http.MethodPost, "/registry/projects", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	// Should have defaults filled in
	if !strings.Contains(rec.Body.String(), `"domain":"default"`) {
		t.Errorf("expected default domain: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"target_node":"mac-forge"`) {
		t.Errorf("expected default target_node: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"target_executor":"claude"`) {
		t.Errorf("expected default target_executor: %s", rec.Body.String())
	}
}

func TestRegistryCreate_MissingFields(t *testing.T) {
	srv := newTestServerWithRegistry(t)

	tests := []struct {
		name string
		body string
	}{
		{"no project", `{"working_dir":"/tmp"}`},
		{"no working_dir", `{"project":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/registry/projects", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- active_sessions enrichment tests (v2-002) ---

func TestRegistryList_WithActiveSessions(t *testing.T) {
	srv, ps, es := newTestServerWithBothStores(t)

	// Create a project.
	createReq := httptest.NewRequest(http.MethodPost, "/registry/projects",
		strings.NewReader(`{"project":"hermes","domain":"engineering","working_dir":"/tmp/hermes"}`))
	createReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, createReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Create envelopes in delivered and in_progress status.
	now := time.Now().UTC()
	env1 := &envelope.Envelope{
		ID:             "env-active-1",
		CreatedAt:      now.Add(-1 * time.Hour),
		CreatedBy:      "kitt",
		Title:          "Active Task 1",
		TaskTitle:      "Do thing",
		TargetExecutor: "opencode",
		Status:         envelope.StatusDelivered,
		Project:        "hermes",
		Metrics: envelope.Metrics{
			StartedAt: &now,
		},
	}
	if err := es.Insert(context.Background(), env1); err != nil {
		t.Fatalf("insert env1: %v", err)
	}
	// Update to delivered status to set started_at.
	if _, err := es.UpdateStatus(context.Background(), "env-active-1", envelope.StatusDelivered, nil, ""); err != nil {
		t.Fatalf("update env1 status: %v", err)
	}

	env2 := &envelope.Envelope{
		ID:             "env-active-2",
		CreatedAt:      now.Add(-30 * time.Minute),
		CreatedBy:      "kitt",
		Title:          "Active Task 2",
		TaskTitle:      "Do other thing",
		TargetExecutor: "opencode",
		Status:         envelope.StatusInProgress,
		Project:        "hermes",
	}
	if err := es.Insert(context.Background(), env2); err != nil {
		t.Fatalf("insert env2: %v", err)
	}
	if _, err := es.UpdateStatus(context.Background(), "env-active-2", envelope.StatusInProgress, nil, ""); err != nil {
		t.Fatalf("update env2 status: %v", err)
	}

	// Create envelope in a different project (should not appear).
	env3 := &envelope.Envelope{
		ID:             "env-other",
		CreatedAt:      now,
		CreatedBy:      "kitt",
		Title:          "Other Project Task",
		TaskTitle:      "Do thing",
		TargetExecutor: "opencode",
		Status:         envelope.StatusDelivered,
		Project:        "other-project",
	}
	if err := es.Insert(context.Background(), env3); err != nil {
		t.Fatalf("insert env3: %v", err)
	}
	if _, err := es.UpdateStatus(context.Background(), "env-other", envelope.StatusDelivered, nil, ""); err != nil {
		t.Fatalf("update env3 status: %v", err)
	}

	// List projects and check active_sessions.
	listReq := httptest.NewRequest(http.MethodGet, "/registry/projects", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var projects []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}

	proj := projects[0]
	sessions, ok := proj["active_sessions"].([]any)
	if !ok {
		t.Fatalf("expected active_sessions array, got %T", proj["active_sessions"])
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 active sessions for hermes, got %d: %v", len(sessions), sessions)
	}

	// Verify session structure.
	for _, s := range sessions {
		session, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("expected session to be map[string]any, got %T", s)
		}
		if session["envelope_id"] == nil {
			t.Fatal("expected envelope_id in session")
		}
		if session["title"] == nil {
			t.Fatal("expected title in session")
		}
		if session["status"] == nil {
			t.Fatal("expected status in session")
		}
		if session["executor"] == nil {
			t.Fatal("expected executor in session")
		}
		if session["started_at"] == nil {
			t.Fatal("expected started_at in session")
		}
	}

	// Verify we can find both active envelopes.
	ids := make(map[string]bool)
	for _, s := range sessions {
		session, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("expected session to be map[string]any, got %T", s)
		}
		envelopeID, ok := session["envelope_id"].(string)
		if !ok {
			t.Fatalf("expected envelope_id to be string, got %T", session["envelope_id"])
		}
		ids[envelopeID] = true
	}
	if !ids["env-active-1"] {
		t.Fatal("expected env-active-1 in active sessions")
	}
	if !ids["env-active-2"] {
		t.Fatal("expected env-active-2 in active sessions")
	}

	// Suppress unused variable warning.
	_ = ps
}

func TestRegistryList_ActiveSessions_EmptyArray(t *testing.T) {
	srv, _, _ := newTestServerWithBothStores(t)

	// Create a project with no envelopes.
	createReq := httptest.NewRequest(http.MethodPost, "/registry/projects",
		strings.NewReader(`{"project":"empty-project","domain":"test","working_dir":"/tmp/empty"}`))
	createReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, createReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// List projects - active_sessions should be empty array, not null.
	listReq := httptest.NewRequest(http.MethodGet, "/registry/projects", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var projects []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &projects); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}

	proj := projects[0]
	sessions, ok := proj["active_sessions"].([]any)
	if !ok {
		t.Fatalf("expected active_sessions array, got %T", proj["active_sessions"])
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 active sessions, got %d", len(sessions))
	}
}

func TestRegistryList_NoEnvelopeStore_StillWorks(t *testing.T) {
	// When envelope store is nil, registry should still return projects without active_sessions.
	srv := newTestServerWithRegistry(t)

	createReq := httptest.NewRequest(http.MethodPost, "/registry/projects",
		strings.NewReader(`{"project":"basic","domain":"test","working_dir":"/tmp/basic"}`))
	createReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, createReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", rec.Code)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/registry/projects", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Should return the project in original format (no active_sessions).
	if !strings.Contains(rec.Body.String(), `"project":"basic"`) {
		t.Fatalf("expected project in response: %s", rec.Body.String())
	}
}

func TestRegistryList_WithIconPath(t *testing.T) {
	srv, ps, _ := newTestServerWithBothStores(t)
	ctx := context.Background()

	// Create a project.
	createReq := httptest.NewRequest(http.MethodPost, "/registry/projects",
		strings.NewReader(`{"project":"icon-test","domain":"ops","working_dir":"/tmp/icon-test"}`))
	createReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, createReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Set icon_path via store.
	if err := ps.SetIconPath(ctx, "icon-test", "/icons/icon-test.png"); err != nil {
		t.Fatalf("set icon_path: %v", err)
	}

	// List and verify icon_path is returned.
	listReq := httptest.NewRequest(http.MethodGet, "/registry/projects", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), `"icon_path":"/icons/icon-test.png"`) {
		t.Fatalf("expected icon_path in response: %s", rec.Body.String())
	}
}

func TestRegistryList_EmptyIconPath(t *testing.T) {
	srv, _, _ := newTestServerWithBothStores(t)

	// Create a project without setting icon_path.
	createReq := httptest.NewRequest(http.MethodPost, "/registry/projects",
		strings.NewReader(`{"project":"no-icon","domain":"ops","working_dir":"/tmp/no-icon"}`))
	createReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, createReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// List and verify icon_path is empty string, not null.
	listReq := httptest.NewRequest(http.MethodGet, "/registry/projects", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, listReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// icon_path should be present and empty, not null.
	if !strings.Contains(rec.Body.String(), `"icon_path":""`) {
		t.Fatalf("expected empty icon_path in response: %s", rec.Body.String())
	}
}

func TestRegistryPatchIcon_Success(t *testing.T) {
	srv, _, _ := newTestServerWithBothStores(t)

	// Create a project.
	createReq := httptest.NewRequest(http.MethodPost, "/registry/projects",
		strings.NewReader(`{"project":"patch-icon-test","domain":"ops","working_dir":"/tmp/patch-test"}`))
	createReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, createReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// PATCH icon_path.
	patchReq := httptest.NewRequest(http.MethodPatch, "/registry/projects/patch-icon-test/icon",
		strings.NewReader(`{"icon_path":"/icons/patch-icon-test.svg"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, patchReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch icon: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), `"icon_path":"/icons/patch-icon-test.svg"`) {
		t.Fatalf("expected icon_path in response: %s", rec.Body.String())
	}
}

func TestRegistryPatchIcon_NotFound(t *testing.T) {
	srv, _, _ := newTestServerWithBothStores(t)

	// PATCH icon_path for nonexistent project.
	patchReq := httptest.NewRequest(http.MethodPatch, "/registry/projects/nonexistent/icon",
		strings.NewReader(`{"icon_path":"/icons/nonexistent.png"}`))
	patchReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, patchReq)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch icon for nonexistent: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegistryPatchIcon_InvalidJSON(t *testing.T) {
	srv, _, _ := newTestServerWithBothStores(t)

	// Create a project first.
	createReq := httptest.NewRequest(http.MethodPost, "/registry/projects",
		strings.NewReader(`{"project":"inv-icon","domain":"ops","working_dir":"/tmp/inv-icon"}`))
	createReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, createReq)

	// PATCH with invalid JSON.
	patchReq := httptest.NewRequest(http.MethodPatch, "/registry/projects/inv-icon/icon",
		strings.NewReader(`not json`))
	patchReq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, patchReq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch with invalid json: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
