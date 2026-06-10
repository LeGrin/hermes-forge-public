package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
)

const ownerEnvelopeID = "env-owned"
const notificationTestKey = "dev-key-notify-test"
const notificationHeader = "X-Hermes-Key"
const notificationsPath = "/notifications"
const notificationAckOnePath = "/notifications/1/ack"
const notificationBulkAckPath = "/notifications/ack"
const expected401Format = "expected 401, got %d: %s"
const testHeaderContentType = "Content-Type"
const testMIMEJSON = "application/json"
const errListNotifications = "list notifications: %v"
const notificationInsertKeyPath = "/envelopes/env-x/status"

func notificationReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(notificationHeader, notificationTestKey)
	return req
}

func newTestServerWithNotify(t *testing.T) (http.Handler, *envelopestore.Store, *notifystore.Store) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open envelope store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notify, err := notifystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open notify store: %v", err)
	}
	return NewServer(discardLogger(), store, nil, notify, nil), store, notify
}

func newTestServerWithNotifyAndKeys(t *testing.T) (http.Handler, *notifystore.Store, *keystore.Store, *keystore.Key) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	store, err := envelopestore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open envelope store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	notify, err := notifystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open notify store: %v", err)
	}
	keys, err := keystore.OpenWithDB(context.Background(), store.DB())
	if err != nil {
		t.Fatalf("open keystore: %v", err)
	}
	key, err := keys.Create(context.Background(), "owner", "user")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return NewServer(discardLogger(), store, nil, notify, nil, ServerOpts{Keys: keys}), notify, keys, key
}

// TestListNotifications_Empty verifies GET /notifications returns empty array.
func TestListNotifications_Empty(t *testing.T) {
	srv, _, _ := newTestServerWithNotify(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodGet, notificationsPath, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Fatalf("expected empty array, got %s", body)
	}
}

// TestListNotifications_HasPending verifies pending notifications are returned.
func TestListNotifications_HasPending(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	n := &notifystore.Notification{EnvelopeID: "env-1", Status: "done", APIKey: notificationTestKey}
	if err := notify.Insert(context.Background(), n); err != nil {
		t.Fatalf("insert notification: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodGet, notificationsPath, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"env-1"`) {
		t.Fatalf("expected env-1 in response, got %s", rec.Body.String())
	}
}

// TestListNotifications_NoHeaderNoKeystoreUsesPublicNamespace verifies that in
// no-keystore mode, a GET /notifications request without X-Hermes-Key returns
// 200 and operates on the public namespace (empty api_key rows).
func TestListNotifications_NoHeaderNoKeystoreUsesPublicNamespace(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)

	// Insert one public-namespace notification and one keyed notification.
	pubN := &notifystore.Notification{EnvelopeID: "env-pub", Status: "done", APIKey: ""}
	keyedN := &notifystore.Notification{EnvelopeID: "env-keyed", Status: "done", APIKey: "dev-key-other"}
	for _, n := range []*notifystore.Notification{pubN, keyedN} {
		if err := notify.Insert(context.Background(), n); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// Request without any header — should reach public namespace.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, notificationsPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for public namespace in no-keystore mode, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"env-pub"`) {
		t.Fatalf("expected public namespace notification in response, got %s", body)
	}
	if strings.Contains(body, `"env-keyed"`) {
		t.Fatalf("expected keyed notification to be excluded from public namespace, got %s", body)
	}
}

// TestListNotifications_NoAuthKeystoreReturns401 verifies that when a keystore
// IS configured, a request without a validated context key returns 401.
func TestListNotifications_NoAuthKeystoreReturns401(t *testing.T) {
	srv, _, _, _ := newTestServerWithNotifyAndKeys(t)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, notificationsPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf(expected401Format, rec.Code, rec.Body.String())
	}
}

// TestAckNotification_OK verifies successful acknowledgement.
func TestAckNotification_OK(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	n := &notifystore.Notification{EnvelopeID: "env-1", Status: "done", APIKey: notificationTestKey}
	if err := notify.Insert(context.Background(), n); err != nil {
		t.Fatalf("insert notification: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodPost, notificationAckOnePath, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"acknowledged"`) {
		t.Fatalf("expected acknowledged in response, got %s", rec.Body.String())
	}
}

// TestAckNotification_NoHeaderNoKeystoreUsesPublicNamespace verifies that in
// no-keystore mode, ack without X-Hermes-Key operates on the public namespace.
// A notification with empty api_key can be acked; unknown ID returns 404.
func TestAckNotification_NoHeaderNoKeystoreUsesPublicNamespace(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	n := &notifystore.Notification{EnvelopeID: "env-pub-ack", Status: "done", APIKey: ""}
	if err := notify.Insert(context.Background(), n); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	idPath := "/notifications/" + strconv64(n.ID) + "/ack"

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, idPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for public namespace ack in no-keystore mode, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAckNotification_NoAuthKeystoreReturns401 verifies that when a keystore
// IS configured, ack without a validated context key returns 401.
func TestAckNotification_NoAuthKeystoreReturns401(t *testing.T) {
	srv, notify, _, _ := newTestServerWithNotifyAndKeys(t)
	n := &notifystore.Notification{EnvelopeID: "env-1", Status: "done", APIKey: ""}
	if err := notify.Insert(context.Background(), n); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, notificationAckOnePath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf(expected401Format, rec.Code, rec.Body.String())
	}
}

// TestAckNotification_InvalidID verifies bad id returns 400.
func TestAckNotification_InvalidID(t *testing.T) {
	srv, _, _ := newTestServerWithNotify(t)

	rec := do(t, srv, http.MethodPost, "/notifications/notanint/ack", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAckNotification_NotFound verifies unknown id returns 404.
func TestAckNotification_NotFound(t *testing.T) {
	srv, _, _ := newTestServerWithNotify(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodPost, "/notifications/999/ack", ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAckNotification_AlreadyAcked verifies double-ack returns 404.
func TestAckNotification_AlreadyAcked(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	n := &notifystore.Notification{EnvelopeID: "env-1", Status: "done", APIKey: notificationTestKey}
	if err := notify.Insert(context.Background(), n); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	// First ack.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodPost, notificationAckOnePath, ""))
	// Second ack — should 404.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodPost, notificationAckOnePath, ""))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on double-ack, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestListNotifications_StoreError verifies 500 when DB is closed.
func TestListNotifications_StoreError(t *testing.T) {
	srv, store, _ := newTestServerWithNotify(t)
	_ = store.Close() // close DB to force store error

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodGet, notificationsPath, ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAckNotification_StoreError verifies 500 when DB is closed.
func TestAckNotification_StoreError(t *testing.T) {
	srv, store, notify := newTestServerWithNotify(t)
	// Insert a notification before closing.
	n := &notifystore.Notification{EnvelopeID: "env-err", Status: "done", APIKey: notificationTestKey}
	if err := notify.Insert(context.Background(), n); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = store.Close() // close DB to force store error

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, notificationReq(http.MethodPost, notificationAckOnePath, ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBulkAckNotifications_IdempotentAndKeyScoped(t *testing.T) {
	srv, notify, keys, owner := newTestServerWithNotifyAndKeys(t)
	other, err := keys.Create(context.Background(), "other", "user")
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}

	owned := &notifystore.Notification{EnvelopeID: ownerEnvelopeID, Status: "done", APIKey: owner.Key}
	acked := &notifystore.Notification{EnvelopeID: "env-acked", Status: "done", APIKey: owner.Key}
	foreign := &notifystore.Notification{EnvelopeID: "env-foreign", Status: "done", APIKey: other.Key}
	for _, n := range []*notifystore.Notification{owned, acked, foreign} {
		if err := notify.Insert(context.Background(), n); err != nil {
			t.Fatalf("insert notification: %v", err)
		}
	}
	if err := notify.Acknowledge(context.Background(), acked.ID); err != nil {
		t.Fatalf("pre-ack: %v", err)
	}

	body := `{"ids":[` + strconv64(owned.ID) + `,` + strconv64(acked.ID) + `,` + strconv64(foreign.ID) + `,9999]}`
	req := httptest.NewRequest(http.MethodPost, notificationBulkAckPath, strings.NewReader(body))
	req.Header.Set(testHeaderContentType, testMIMEJSON)
	req.Header.Set(notificationHeader, owner.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]float64
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["requested"] != 4 || got["acknowledged"] != 1 || got["already_acknowledged"] != 1 || got["missing"] != 2 {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

// TestBulkAckNotifications_NoHeaderNoKeystoreUsesPublicNamespace verifies that
// in no-keystore mode, bulk ack without X-Hermes-Key operates on the public
// namespace (empty api_key). Unknown IDs are counted as missing, not rejected.
func TestBulkAckNotifications_NoHeaderNoKeystoreUsesPublicNamespace(t *testing.T) {
	srv, _, _ := newTestServerWithNotify(t)
	req := httptest.NewRequest(http.MethodPost, notificationBulkAckPath, strings.NewReader(`{"ids":[1]}`))
	req.Header.Set(testHeaderContentType, testMIMEJSON)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	// ID 1 doesn't exist → missing:1, but request is accepted (200).
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for public namespace bulk ack in no-keystore mode, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"missing"`) {
		t.Fatalf("expected missing field in response, got %s", rec.Body.String())
	}
}

func TestListNotifications_KeyScoped(t *testing.T) {
	srv, notify, keys, owner := newTestServerWithNotifyAndKeys(t)
	other, err := keys.Create(context.Background(), "other", "user")
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}
	for _, n := range []*notifystore.Notification{
		{EnvelopeID: ownerEnvelopeID, Status: "done", APIKey: owner.Key},
		{EnvelopeID: "env-other", Status: "done", APIKey: other.Key},
	} {
		if err := notify.Insert(context.Background(), n); err != nil {
			t.Fatalf("insert notification: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, notificationsPath, nil)
	req.Header.Set(notificationHeader, owner.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ownerEnvelopeID) || strings.Contains(rec.Body.String(), "env-other") {
		t.Fatalf("expected only owner notifications, got %s", rec.Body.String())
	}
}

func TestBulkAckNotifications_NoAuthReturns401(t *testing.T) {
	srv, _, _, _ := newTestServerWithNotifyAndKeys(t)
	req := httptest.NewRequest(http.MethodPost, notificationBulkAckPath, strings.NewReader(`{"ids":[1]}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf(expected401Format, rec.Code, rec.Body.String())
	}
}

func strconv64(n int64) string {
	return strconv.FormatInt(n, 10)
}

// TestNotificationRawHeaderRejectedWhenKeystoreConfigured verifies that when a
// keystore is configured, a request with an unregistered X-Hermes-Key header
// value (not present in the keystore) is rejected with 401. The keystore
// middleware validates the header against the store; an unregistered value
// never produces a validated context key and is therefore rejected.
func TestNotificationRawHeaderRejectedWhenKeystoreConfigured(t *testing.T) {
	srv, _, _, _ := newTestServerWithNotifyAndKeys(t)

	// Send a header with a value that is not registered in the keystore.
	// The keystore middleware rejects this before the handler runs.
	req := httptest.NewRequest(http.MethodGet, notificationsPath, nil)
	req.Header.Set(notificationHeader, "dev-key-unregistered-value")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when keystore configured and unregistered key used, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestBulkAckRawHeaderRejectedWhenKeystoreConfigured verifies bulk ack also
// rejects requests with an unregistered key when keystore is configured.
func TestBulkAckRawHeaderRejectedWhenKeystoreConfigured(t *testing.T) {
	srv, _, _, _ := newTestServerWithNotifyAndKeys(t)

	req := httptest.NewRequest(http.MethodPost, notificationBulkAckPath, strings.NewReader(`{"ids":[1]}`))
	req.Header.Set(testHeaderContentType, testMIMEJSON)
	req.Header.Set(notificationHeader, "dev-key-unregistered-value")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when keystore configured and unregistered key used, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// TestAddHistory_OK verifies history entry appended successfully.
func TestAddHistory_OK(t *testing.T) {
	srv, _ := newTestServer(t)
	// Create envelope first.
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-hist-1"))

	rec := do(t, srv, http.MethodPost, "/envelopes/env-hist-1/history", `{"entry":"[DECISION] chose X"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[DECISION] chose X") {
		t.Fatalf("expected entry in response history, got %s", rec.Body.String())
	}
}

// TestAddHistory_EmptyEntry verifies empty entry rejected.
func TestAddHistory_EmptyEntry(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-hist-2"))

	rec := do(t, srv, http.MethodPost, "/envelopes/env-hist-2/history", `{"entry":""}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAddHistory_NotFound verifies 404 for unknown envelope.
func TestAddHistory_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := do(t, srv, http.MethodPost, "/envelopes/no-such-env/history", `{"entry":"note"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNotificationEmittedOnInterestingStatus verifies that updating an envelope
// to an interesting status (done/blocked/failed) creates a notification.
func TestNotificationEmittedOnInterestingStatus(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	// Create envelope.
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-notify-1"))
	// Transition to "done".
	do(t, srv, http.MethodPatch, "/envelopes/env-notify-1/status",
		`{"status":"in_progress"}`)
	do(t, srv, http.MethodPatch, "/envelopes/env-notify-1/status",
		`{"status":"done","proof":{"summary":"finished"}}`)

	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf(errListNotifications, err)
	}
	found := false
	for _, n := range notifications {
		if n.EnvelopeID == "env-notify-1" && n.Status == "done" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected done notification for env-notify-1, got %v", notifications)
	}
}

// TestAddHistory_BadJSON verifies bad JSON returns 400.
func TestAddHistory_BadJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-hist-bad"))

	rec := do(t, srv, http.MethodPost, "/envelopes/env-hist-bad/history", `{bad}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNotificationLoopDetection verifies >3 blocked triggers loop_detected.
func TestNotificationLoopDetection(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-loop"))

	// Cycle through blocked multiple times.
	for i := 0; i < 4; i++ {
		do(t, srv, http.MethodPatch, "/envelopes/env-loop/status", `{"status":"blocked","note":"stuck"}`)
		do(t, srv, http.MethodPatch, "/envelopes/env-loop/status", `{"status":"in_progress"}`)
	}
	// One more blocked to trigger detection (>3 blocked in 30 min).
	do(t, srv, http.MethodPatch, "/envelopes/env-loop/status", `{"status":"blocked","note":"stuck again"}`)

	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, n := range notifications {
		if n.Status == "loop_detected" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected loop_detected notification")
	}
}

// TestNotificationWithProofSummary verifies proof summary in notification.
func TestNotificationWithProofSummary(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-proof-n"))
	do(t, srv, http.MethodPatch, "/envelopes/env-proof-n/status",
		`{"status":"done","proof":{"commit":"abc123","pr":"#42"}}`)

	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, n := range notifications {
		if n.EnvelopeID == "env-proof-n" && n.Status == "done" {
			if n.ProofSummary == "" {
				t.Error("expected non-empty proof_summary")
			}
			return
		}
	}
	t.Fatal("notification not found")
}

// TestNotificationEmittedOnPaused verifies paused triggers notification.
func TestNotificationEmittedOnPaused(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-paused"))
	do(t, srv, http.MethodPatch, "/envelopes/env-paused/status", `{"status":"paused","note":"waiting"}`)

	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, n := range notifications {
		if n.EnvelopeID == "env-paused" && n.Status == "paused" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected paused notification")
	}
}

// TestNotificationEmittedOnFailed verifies failed triggers notification.
func TestNotificationEmittedOnFailed(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-fail"))
	do(t, srv, http.MethodPatch, "/envelopes/env-fail/status", `{"status":"failed","note":"crash"}`)

	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, n := range notifications {
		if n.EnvelopeID == "env-fail" && n.Status == "failed" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected failed notification")
	}
}

// TestNotificationNotEmittedForUninterestingStatus verifies that "read" status
// does not create a notification.
func TestNotificationNotEmittedForUninterestingStatus(t *testing.T) {
	srv, _, notify := newTestServerWithNotify(t)
	do(t, srv, http.MethodPost, "/envelopes", validPayload("env-notify-2"))
	do(t, srv, http.MethodPatch, "/envelopes/env-notify-2/status", `{"status":"read"}`)

	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf(errListNotifications, err)
	}
	for _, n := range notifications {
		if n.EnvelopeID == "env-notify-2" {
			t.Fatalf("unexpected notification for uninteresting status: %+v", n)
		}
	}
}

// TestMaybeNotify_SkipsInsertionWhenKeystoreConfiguredAndNoKey verifies that
// maybeNotify does not insert empty-key notification rows when a keystore is
// configured but the request carries no API key.
//
// When a keystore is configured, the auth middleware rejects unauthenticated
// requests with 401 before the handler runs. This test confirms that the
// middleware gate holds: a status update without a key returns 401 and no
// notification is inserted.
func TestMaybeNotify_SkipsInsertionWhenKeystoreConfiguredAndNoKey(t *testing.T) {
	srv, notify, _, registeredKey := newTestServerWithNotifyAndKeys(t)

	// Attempt to create an envelope using the registered key — should succeed.
	createReq := httptest.NewRequest(http.MethodPost, "/envelopes", strings.NewReader(validPayload("env-nokey-skip")))
	createReq.Header.Set(testHeaderContentType, testMIMEJSON)
	createReq.Header.Set(notificationHeader, registeredKey.Key)
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated && createRec.Code != http.StatusOK {
		t.Fatalf("expected envelope creation to succeed with registered key, got %d: %s", createRec.Code, createRec.Body.String())
	}

	// Now attempt a status update WITHOUT any key — middleware must reject with 401.
	noKeyReq := httptest.NewRequest(http.MethodPatch, "/envelopes/env-nokey-skip/status",
		strings.NewReader(`{"status":"done"}`))
	noKeyReq.Header.Set(testHeaderContentType, testMIMEJSON)
	noKeyRec := httptest.NewRecorder()
	srv.ServeHTTP(noKeyRec, noKeyReq)
	if noKeyRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated status update when keystore configured, got %d: %s",
			noKeyRec.Code, noKeyRec.Body.String())
	}

	// No notification should have been inserted.
	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf(errListNotifications, err)
	}
	for _, n := range notifications {
		if n.EnvelopeID == "env-nokey-skip" {
			t.Fatalf("expected no notification when keystore configured and no key present, got %+v", n)
		}
	}
}

// TestBulkAckNotifications_OversizedBodyRejected verifies that bulk ack rejects
// request bodies exceeding maxBulkAckBodyBytes to limit DoS exposure.
func TestBulkAckNotifications_OversizedBodyRejected(t *testing.T) {
	srv, _, _, key := newTestServerWithNotifyAndKeys(t)

	// Build a body larger than maxBulkAckBodyBytes (64 KiB).
	ids := make([]string, 0, 5000)
	for i := 0; i < 5000; i++ {
		ids = append(ids, strconv64(int64(i+1)))
	}
	oversized := `{"ids":[` + strings.Join(ids, ",") + `]}`
	if len(oversized) <= 64*1024 {
		t.Skipf("generated body %d bytes is not oversized enough — adjust test", len(oversized))
	}

	req := httptest.NewRequest(http.MethodPost, notificationBulkAckPath, strings.NewReader(oversized))
	req.Header.Set(testHeaderContentType, testMIMEJSON)
	req.Header.Set(notificationHeader, key.Key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized body, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestNotificationInsertKey_KeystoreConfiguredNoContextKey verifies that
// notificationInsertKey returns ok=false when a keystore is configured but no
// validated key is present in the request context. This is the defence-in-depth
// guard: even if a raw X-Hermes-Key header is present it must not be trusted.
func TestNotificationInsertKey_KeystoreConfiguredNoContextKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, notificationInsertKeyPath, nil)
	req.Header.Set(notificationHeader, "dev-key-raw-unvalidated")

	key, ok := notificationInsertKey(req, true /* keysConfigured */)
	if ok {
		t.Fatalf("expected ok=false when keystore configured and no context key, got key=%q", key)
	}
}

// TestNotificationInsertKey_KeystoreConfiguredWithContextKey verifies that
// notificationInsertKey returns the validated context key when one is present.
func TestNotificationInsertKey_KeystoreConfiguredWithContextKey(t *testing.T) {
	const wantKey = "dev-key-validated"
	req := httptest.NewRequest(http.MethodPatch, notificationInsertKeyPath, nil)
	ctx := context.WithValue(req.Context(), ctxAPIKey, &keystore.Key{Key: wantKey})
	req = req.WithContext(ctx)

	key, ok := notificationInsertKey(req, true /* keysConfigured */)
	if !ok {
		t.Fatal("expected ok=true when validated context key present")
	}
	if key != wantKey {
		t.Fatalf("expected key=%q, got %q", wantKey, key)
	}
}

// TestNotificationInsertKey_NoKeystoreNoHeader verifies that in no-keystore
// mode a missing header returns the public namespace (empty key, ok=true).
func TestNotificationInsertKey_NoKeystoreNoHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, notificationInsertKeyPath, nil)

	key, ok := notificationInsertKey(req, false /* keysConfigured */)
	if !ok {
		t.Fatal("expected ok=true in no-keystore mode with missing header")
	}
	if key != "" {
		t.Fatalf("expected empty key for public namespace, got %q", key)
	}
}

// TestNotificationInsertKey_NoKeystoreWithHeader verifies that in no-keystore
// mode a raw header value is accepted as the namespace identifier.
func TestNotificationInsertKey_NoKeystoreWithHeader(t *testing.T) {
	const wantKey = "dev-key-namespace"
	req := httptest.NewRequest(http.MethodPatch, notificationInsertKeyPath, nil)
	req.Header.Set(notificationHeader, wantKey)

	key, ok := notificationInsertKey(req, false /* keysConfigured */)
	if !ok {
		t.Fatal("expected ok=true in no-keystore mode with header")
	}
	if key != wantKey {
		t.Fatalf("expected key=%q, got %q", wantKey, key)
	}
}

// TestMaybeNotify_InsertsWithValidatedContextKey verifies that when a keystore
// is configured and the request carries a validated context key, a status
// update to an interesting status creates a notification scoped to that key.
func TestMaybeNotify_InsertsWithValidatedContextKey(t *testing.T) {
	srv, notify, _, registeredKey := newTestServerWithNotifyAndKeys(t)

	// Create envelope with the registered key.
	createReq := httptest.NewRequest(http.MethodPost, "/envelopes", strings.NewReader(validPayload("env-ctx-key")))
	createReq.Header.Set(testHeaderContentType, testMIMEJSON)
	createReq.Header.Set(notificationHeader, registeredKey.Key)
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated && createRec.Code != http.StatusOK {
		t.Fatalf("expected envelope creation to succeed, got %d: %s", createRec.Code, createRec.Body.String())
	}

	// Update status to "done" with the registered key — middleware validates it.
	updateReq := httptest.NewRequest(http.MethodPatch, "/envelopes/env-ctx-key/status",
		strings.NewReader(`{"status":"done"}`))
	updateReq.Header.Set(testHeaderContentType, testMIMEJSON)
	updateReq.Header.Set(notificationHeader, registeredKey.Key)
	updateRec := httptest.NewRecorder()
	srv.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for status update with valid key, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	// Notification must be inserted and scoped to the registered key.
	notifications, err := notify.ListUnacknowledged(context.Background())
	if err != nil {
		t.Fatalf(errListNotifications, err)
	}
	found := false
	for _, n := range notifications {
		if n.EnvelopeID == "env-ctx-key" && n.Status == "done" {
			if n.APIKey != registeredKey.Key {
				t.Fatalf("expected notification scoped to key %q, got %q", registeredKey.Key, n.APIKey)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected done notification for env-ctx-key with validated key")
	}
}
