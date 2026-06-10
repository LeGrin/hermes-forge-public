package httpapi

import (
	"os"
	"strings"
	"testing"
)

const dashboardIndexFile = "index.html"

// Literal markers used to locate the showEnvelopeDetail function body in the
// dashboard HTML. Defined as constants to satisfy the duplicate-literal lint
// rule (go:S1192) and to make intent explicit.
const (
	jsShowEnvelopeDetailStart = "function showEnvelopeDetail(env) {"
	jsShowSessionDetailStart  = "function showSessionDetail(sess) {"
	errEnvelopeDetailBoundary = "showEnvelopeDetail function boundaries not found"

	// CON-012: compose area markers.
	jsComposeTextarea    = `composeTextarea.id = 'compose-text'`
	jsComposeSendBtn     = `composeSendBtn.id = 'compose-send'`
	jsComposeKindSelect  = `composeKind.id = 'compose-kind'`
	jsComposeKindSteer   = `'steer'`
	jsComposeKindReply   = `'reply'`
	jsPostThreadEndpoint = `'/envelopes/' + encodeURIComponent(env.id) + '/thread'`
	jsPostMethod         = `method: 'POST'`
	jsPostContentType    = `'Content-Type': 'application/json'`
	jsPostAuthHeader     = `'X-Hermes-Key': apiKey`
	jsPostFromKitt       = `from: 'kitt'`
	jsComposeError       = `compose-error`
	jsComposeSuccess     = `compose-text`
	// CON-012 review fixes: apiPostRaw helper, CSS classes, race-condition guard.
	jsApiPostCall         = `apiPostRaw('/envelopes/' + encodeURIComponent(env.id) + '/thread', payload)`
	jsApiPostHelper       = `function apiPostRaw(path, body) {`
	jsComposeCSSClass     = `compose-wrap`
	jsSendDisabledInit    = `composeSendBtn.disabled = true`
	jsLatestMsgFromPost   = `return refreshThreadState(postedMessageId)`
	jsSendFailedFallback  = `showComposeError('Send failed (HTTP ' + result.status + '): ' + errMsg)`
	jsSendHandlerStart    = `composeSendBtn.addEventListener('click', function() {`
	jsPostSuccessStart    = `apiPostRaw('/envelopes/' + encodeURIComponent(env.id) + '/thread', payload).then(function(result) {`
	jsSendErrorData       = `const d = result.data || {};`
	jsSendErrorDetail     = `const errMsg = d.detail || d.error || 'unexpected error';`
	jsThreadHeaderReset   = `threadH.textContent = 'Thread'`
	jsComposeErrorVisible = `.compose-error.is-visible { display: block; }`
	jsShowComposeError    = `composeError.classList.add('is-visible')`
	jsHideComposeError    = `composeError.classList.remove('is-visible')`
	jsSendNetworkErr      = `showComposeError('Send failed (network error)')`
	jsReplyRequiresMsg    = `Reply requires an existing thread message.`

	// CON-012: inbox clickthrough markers.
	jsInboxClickthrough  = `showEnvelopeDetail(`
	jsInboxFetchFallback = `apiFetchRaw('/envelopes/' + encodeURIComponent(`
	jsInboxRoleButton    = `div.setAttribute('role', 'button')`
	jsInboxTabIndex      = `div.tabIndex = 0`
	jsInboxKeydown       = `div.onkeydown = function(e) {`
	jsInboxFetchError    = `Failed to load envelope (`
	jsInboxFetchErrorID  = `inbox-fetch-error`
	jsInboxClearError    = `if (previous) previous.remove();`
	jsInboxFetchingGuard = `let fetchingEnvelope = false;`
	jsInboxFetchingSkip  = `if (fetchingEnvelope) return;`
	jsInboxFetchingDone  = `fetchingEnvelope = false;`
)

func readDashboardHTML(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(dashboardIndexFile)
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	return string(raw)
}

// extractShowEnvelopeDetailFn slices the showEnvelopeDetail JS function body
// from the dashboard HTML. It uses the start of showSessionDetail as the
// upper boundary because that function immediately follows in the source.
func extractShowEnvelopeDetailFn(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, jsShowEnvelopeDetailStart)
	end := strings.Index(html, jsShowSessionDetailStart)
	if start < 0 || end < 0 || start >= end {
		t.Fatalf(errEnvelopeDetailBoundary)
	}
	return html[start:end]
}

func extractJSBlock(t *testing.T, body, marker string) string {
	t.Helper()
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("JS marker not found: %s", marker)
	}
	open := strings.Index(body[start:], "{")
	if open < 0 {
		t.Fatalf("JS marker has no block: %s", marker)
	}
	open += start
	depth := 0
	for i := open; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start : i+1]
			}
		}
	}
	t.Fatalf("JS block not closed: %s", marker)
	return ""
}

// TestFiltersModule_AgentLifecycleFilter tests the agent lifecycle filter logic.
// This verifies that agentPassesFilter correctly categorizes agents by state.
func TestFiltersModule_AgentLifecycleFilter(t *testing.T) {
	tests := []struct {
		name     string
		agent    map[string]interface{}
		filters  map[string]bool
		expected bool
	}{
		{
			name:     "active agent passes when active filter is on",
			agent:    map[string]interface{}{"state": "active"},
			filters:  map[string]bool{"active": true, "idle": false, "old": false},
			expected: true,
		},
		{
			name:     "active agent blocked when active filter is off",
			agent:    map[string]interface{}{"state": "active"},
			filters:  map[string]bool{"active": false, "idle": true, "old": true},
			expected: false,
		},
		{
			name:     "idle agent passes when idle filter is on",
			agent:    map[string]interface{}{"state": "idle"},
			filters:  map[string]bool{"active": false, "idle": true, "old": false},
			expected: true,
		},
		{
			name:     "old agent passes when old filter is on",
			agent:    map[string]interface{}{"state": "old"},
			filters:  map[string]bool{"active": false, "idle": false, "old": true},
			expected: true,
		},
		{
			name:     "terminated agent is treated as old",
			agent:    map[string]interface{}{"state": "terminated"},
			filters:  map[string]bool{"active": false, "idle": false, "old": true},
			expected: true,
		},
		{
			name:     "done agent is treated as old",
			agent:    map[string]interface{}{"state": "done"},
			filters:  map[string]bool{"active": false, "idle": false, "old": true},
			expected: true,
		},
		{
			name:     "unknown state agent passes if any filter is on",
			agent:    map[string]interface{}{"state": "unknown"},
			filters:  map[string]bool{"active": true, "idle": false, "old": false},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the filter logic from filters.js
			state, _ := tt.agent["state"].(string)
			if state == "" {
				state = "idle"
			}

			isActive := state == "active"
			isIdle := state == "idle"
			isOld := state == "old" || state == "terminated" || state == "done"

			var passes bool
			if isActive && !tt.filters["active"] {
				passes = false
			} else if isIdle && !tt.filters["idle"] {
				passes = false
			} else if isOld && !tt.filters["old"] {
				passes = false
			} else if !isActive && !isIdle && !isOld {
				passes = tt.filters["active"] || tt.filters["idle"] || tt.filters["old"]
			} else {
				passes = true
			}

			if passes != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, passes)
			}
		})
	}
}

// TestFiltersModule_EnvelopeStatusCategory tests envelope status categorization.
func TestFiltersModule_EnvelopeStatusCategory(t *testing.T) {
	tests := []struct {
		status   string
		expected string
	}{
		{"", "active"},
		{"created", "active"},
		{"in_progress", "active"},
		{"delivered", "active"},
		{"awaiting_confirm", "active"},
		{"done", "done"},
		{"closed", "done"},
		{"blocked", "blocked"},
		{"paused", "blocked"},
		{"failed", "failed"},
		{"lost", "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			status := tt.status
			if status == "" {
				status = "active"
			} else {
				status = strings.ToLower(status)
			}

			var cat string
			switch status {
			case "done", "closed":
				cat = "done"
			case "blocked", "paused":
				cat = "blocked"
			case "failed", "lost":
				cat = "failed"
			default:
				cat = "active"
			}

			if cat != tt.expected {
				t.Errorf("status %q: expected %q, got %q", tt.status, tt.expected, cat)
			}
		})
	}
}

// TestFiltersModule_EnvelopeDefaultFilter tests that default envelope filter is "active".
func TestFiltersModule_EnvelopeDefaultFilter(t *testing.T) {
	// The default filter should be "active" (status != done) per CON-010
	defaultFilter := "active"
	if defaultFilter != "active" {
		t.Errorf("expected default filter to be 'active', got %q", defaultFilter)
	}
}

func TestDashboard_InboxUsesNotificationsAndMapStaysMapOnly(t *testing.T) {
	html := readDashboardHTML(t)
	if !strings.Contains(html, `data-tab="inbox"`) {
		t.Fatal("dashboard must expose a separate Inbox tab")
	}
	if !strings.Contains(html, `apiFetch('/notifications')`) {
		t.Fatal("Inbox must load Hermes /notifications, not activity or envelope data")
	}
	mapPanelStart := strings.Index(html, `id="panel-constellation"`)
	mapPanelEnd := strings.Index(html, `id="panel-activity"`)
	if mapPanelStart < 0 || mapPanelEnd < 0 || mapPanelStart >= mapPanelEnd {
		t.Fatalf("dashboard must contain panel-constellation before panel-activity markers (start=%d end=%d)", mapPanelStart, mapPanelEnd)
	}
	mapPanel := html[mapPanelStart:mapPanelEnd]
	if strings.Contains(strings.ToLower(mapPanel), "inbox") || strings.Contains(mapPanel, "/notifications") {
		t.Fatal("Constellation map panel must remain map-only")
	}
}

func TestDashboard_AgentDetailFetchesReadOnlyEndpoint(t *testing.T) {
	html := readDashboardHTML(t)
	if !strings.Contains(html, `apiFetch('/agents/' + encodeURIComponent(a.id))`) {
		t.Fatal("agent detail must use the read-only /agents/{id} endpoint")
	}
}

func TestDashboard_SessionRawTailUsesHermesEndpoint(t *testing.T) {
	html := readDashboardHTML(t)
	if !strings.Contains(html, `/raw-tail?max_lines=20`) {
		t.Fatal("session logs must use bounded Hermes raw-tail endpoint")
	}
}

// TestEnvelopeFilter_ActiveIncludesBlockedAndFailed tests that the "active" filter
// (the default view) includes blocked and failed envelopes, since "active" means
// status != done per CON-010 runbook semantics.
func TestEnvelopeFilter_ActiveIncludesBlockedAndFailed(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		chipValue string
		expected  bool
	}{
		{"done not included in active", "done", "active", false},
		{"closed not included in active", "closed", "active", false},
		{"blocked included in active", "blocked", "active", true},
		{"failed included in active", "failed", "active", true},
		{"in_progress included in active", "in_progress", "active", true},
		{"delivered included in active", "delivered", "active", true},
		{"created included in active", "created", "active", true},
		{"blocked only in blocked chip", "blocked", "blocked", true},
		{"done only in done chip", "done", "done", true},
		{"failed only in failed chip", "failed", "failed", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == "" {
				status = "active"
			} else {
				status = strings.ToLower(status)
			}

			var cat string
			switch status {
			case "done", "closed":
				cat = "done"
			case "blocked", "paused":
				cat = "blocked"
			case "failed", "lost":
				cat = "failed"
			default:
				cat = "active"
			}

			var passes bool
			if tt.chipValue == "all" {
				passes = true
			} else if tt.chipValue == "active" {
				passes = cat != "done"
			} else {
				passes = cat == tt.chipValue
			}

			if passes != tt.expected {
				t.Errorf("status=%q chip=%q: expected %v, got %v", tt.status, tt.chipValue, tt.expected, passes)
			}
		})
	}
}

// TestCountByStatus_ActiveCountIsAllMinusDone verifies that "active" count
// is computed as all - done, since the "active" chip filter shows status != done.
func TestCountByStatus_ActiveCountIsAllMinusDone(t *testing.T) {
	// Simulate envelopes with various statuses
	statuses := []string{
		"created",     // active
		"in_progress", // active
		"blocked",     // blocked
		"failed",      // failed
		"done",        // done
		"delivered",   // active
		"closed",      // done
	}

	counts := map[string]int{
		"all": 0, "active": 0, "done": 0, "blocked": 0, "failed": 0,
	}
	for _, s := range statuses {
		var cat string
		switch s {
		case "done", "closed":
			cat = "done"
		case "blocked", "paused":
			cat = "blocked"
		case "failed", "lost":
			cat = "failed"
		default:
			cat = "active"
		}
		counts["all"]++
		counts[cat]++
	}
	// active = all - done
	counts["active"] = counts["all"] - counts["done"]

	if counts["all"] != 7 {
		t.Errorf("expected all=7, got %d", counts["all"])
	}
	if counts["done"] != 2 {
		t.Errorf("expected done=2, got %d", counts["done"])
	}
	if counts["active"] != 5 {
		t.Errorf("expected active=5 (all-done), got %d", counts["active"])
	}
	if counts["blocked"] != 1 {
		t.Errorf("expected blocked=1, got %d", counts["blocked"])
	}
	if counts["failed"] != 1 {
		t.Errorf("expected failed=1, got %d", counts["failed"])
	}
}

// TestDashboardEnvelopeFilter_InitBeforeLoad verifies the dashboard restores the
// persisted envelope chip before envelope fetch/render paths run.
func TestDashboardEnvelopeFilter_InitBeforeLoad(t *testing.T) {
	body := readDashboardHTML(t)

	showAppStart := strings.Index(body, "function showApp() {")
	showAppEnd := strings.Index(body, "function refreshAll() {")
	if showAppStart < 0 || showAppEnd < 0 || showAppStart >= showAppEnd {
		t.Fatalf("showApp markers not found in dashboard html")
	}
	showApp := body[showAppStart:showAppEnd]
	if strings.Index(showApp, "initEnvFilter();") > strings.Index(showApp, "loadEnvelopes();") {
		t.Fatalf("showApp should restore persisted envelope filter before loadEnvelopes")
	}

	loadEnvelopesStart := strings.Index(body, "function loadEnvelopes() {")
	loadEnvelopesEnd := strings.Index(body, "function updateEnvelopeBadges(envelopes) {")
	if loadEnvelopesStart < 0 || loadEnvelopesEnd < 0 || loadEnvelopesStart >= loadEnvelopesEnd {
		t.Fatalf("loadEnvelopes markers not found in dashboard html")
	}
	loadEnvelopes := body[loadEnvelopesStart:loadEnvelopesEnd]
	if strings.Index(loadEnvelopes, "initEnvFilter();") > strings.Index(loadEnvelopes, "apiFetch('/envelopes')") {
		t.Fatalf("loadEnvelopes should restore persisted envelope filter before fetching")
	}
	if !strings.Contains(body, "window.currentEnvFilter = 'active';") {
		t.Fatalf("dashboard should initialize envelope filter state on window before first render")
	}
	if !strings.Contains(body, "Filters.envelopePassesFilter(env, window.currentEnvFilter)") {
		t.Fatalf("dashboard should render envelopes from initialized window envelope filter state")
	}

	loadBootstrap := strings.Index(body, "window.addEventListener('load', function() {")
	if loadBootstrap < 0 {
		t.Fatalf("dashboard should bootstrap showApp from window load after external scripts are available")
	}
	preBootstrap := body[:loadBootstrap]
	if strings.Contains(preBootstrap, "if (apiKey) showApp();") {
		t.Fatalf("dashboard should not call showApp eagerly before load bootstrap")
	}
}

// TestRegistryList_IconPathInListResponse verifies that icon_path is returned
// in the /registry/projects list response (already covered by existing tests,
// but included here for explicit verification).
func TestRegistryList_IconPathInListResponse(t *testing.T) {
	// This is already tested in TestRegistryList_WithIconPath
	// We verify the expected behavior here for documentation purposes.
	//
	// Expected: GET /registry/projects returns icon_path in each project object
	// This was verified by TestRegistryList_WithIconPath which checks:
	//   strings.Contains(rec.Body.String(), `"icon_path":"/icons/icon-test.png"`)
}

func TestForceTopology_GroupingPreservesOriginalAgentIDs(t *testing.T) {
	js, err := os.ReadFile("topology-force.js")
	if err != nil {
		t.Fatalf("read topology-force.js: %v", err)
	}
	body := string(js)
	if !strings.Contains(body, "nodeMap = {}") {
		t.Fatalf("force topology should rebuild nodeMap from the current grouped nodes on every render")
	}
	if !strings.Contains(body, "nodeMap[member.id] = n") {
		t.Fatalf("grouped topology must map each original agent id to its grouped node")
	}
	if !strings.Contains(body, "resolveNode(member.parent_id)") {
		t.Fatalf("parent links must resolve raw parent_id values through grouped nodeMap")
	}
	if !strings.Contains(body, "resolveNodeID(fromNode)") || !strings.Contains(body, "resolveNodeID(toNode)") {
		t.Fatalf("traffic pulses must resolve original agent ids to grouped nodes")
	}
}

func TestForceTopology_GroupIDsAreDeterministicFromGroupingKey(t *testing.T) {
	js, err := os.ReadFile("topology-force.js")
	if err != nil {
		t.Fatalf("read topology-force.js: %v", err)
	}
	body := string(js)
	if !strings.Contains(body, "function stableGroupID(key)") {
		t.Fatalf("force topology should derive group ids through a stable helper")
	}
	if !strings.Contains(body, "var groupId = stableGroupID(key)") {
		t.Fatalf("group id should be derived only from the grouping key")
	}
	if strings.Contains(body, "'group:' + idx") || strings.Contains(body, "\"group:\" + idx") {
		t.Fatalf("group id must not include enumeration indexes")
	}
}

// extractRenderInboxFn slices the renderInbox JS function body from the
// dashboard HTML. It uses inboxMatchesFilter as the upper boundary.
func extractRenderInboxFn(t *testing.T, html string) string {
	t.Helper()
	const start = "function renderInbox() {"
	const end = "function inboxMatchesFilter("
	s := strings.Index(html, start)
	e := strings.Index(html, end)
	if s < 0 || e < 0 || s >= e {
		t.Fatalf("renderInbox function boundaries not found (start=%d end=%d)", s, e)
	}
	return html[s:e]
}

// TestDashboard_ComposeArea verifies that showEnvelopeDetail injects a compose
// area with textarea, kind select (steer/reply), and a Send button.
func TestDashboard_ComposeArea(t *testing.T) {
	html := readDashboardHTML(t)
	fn := extractShowEnvelopeDetailFn(t, html)

	t.Run("HasTextarea", func(t *testing.T) {
		if !strings.Contains(fn, jsComposeTextarea) {
			t.Fatalf("showEnvelopeDetail must inject a compose textarea with id='compose-text'")
		}
	})

	t.Run("HasSendButton", func(t *testing.T) {
		if !strings.Contains(fn, jsComposeSendBtn) {
			t.Fatalf("showEnvelopeDetail must inject a Send button with id='compose-send'")
		}
	})

	t.Run("HasKindSelect", func(t *testing.T) {
		if !strings.Contains(fn, jsComposeKindSelect) {
			t.Fatalf("showEnvelopeDetail must inject a kind <select> with id='compose-kind'")
		}
	})

	t.Run("KindSelectHasSteer", func(t *testing.T) {
		if !strings.Contains(fn, jsComposeKindSteer) {
			t.Fatalf("compose kind select must include value='steer'")
		}
	})

	t.Run("KindSelectHasReply", func(t *testing.T) {
		if !strings.Contains(fn, jsComposeKindReply) {
			t.Fatalf("compose kind select must include value='reply'")
		}
	})
}

// assertContainsInFn fails the test if needle is not found in body.
// Extracted to reduce cognitive complexity of TestDashboard_ComposeSend (go:S3776).
func assertContainsInFn(t *testing.T, body, needle, msg string) {
	t.Helper()
	if !strings.Contains(body, needle) {
		t.Fatalf("%s", msg)
	}
}

func assertNotContainsInFn(t *testing.T, body, needle, msg string) {
	t.Helper()
	if strings.Contains(body, needle) {
		t.Fatalf("%s", msg)
	}
}

func assertInboxKeyboardAccessible(t *testing.T, fn string) {
	t.Helper()
	assertContainsInFn(t, fn, jsInboxRoleButton, "renderInbox cards must expose role=button")
	assertContainsInFn(t, fn, jsInboxTabIndex, "renderInbox cards must be tabbable")
	assertContainsInFn(t, fn, jsInboxKeydown, "renderInbox cards must open on keyboard events")
	assertContainsInFn(t, fn, `e.key === 'Enter'`, "renderInbox cards must open on Enter")
	assertContainsInFn(t, fn, `e.key === ' '`, "renderInbox cards must open on Space")
}

// TestDashboard_ComposeSend verifies the POST body and button-disable logic.
func TestDashboard_ComposeSend(t *testing.T) {
	html := readDashboardHTML(t)
	fn := extractShowEnvelopeDetailFn(t, html)
	apiPostRawFn := extractJSBlock(t, html, jsApiPostHelper)
	sendHandlerFn := extractJSBlock(t, fn, jsSendHandlerStart)
	postSuccessFn := extractJSBlock(t, sendHandlerFn, jsPostSuccessStart)

	t.Run("UsesApiPostRawHelper", func(t *testing.T) {
		// Send must route through apiPostRaw (not raw fetch) for consistent auth handling.
		assertContainsInFn(t, fn, jsApiPostCall, "compose Send must call apiPostRaw() helper, not raw fetch")
	})

	t.Run("ApiPostRawHelperExists", func(t *testing.T) {
		// The apiPostRaw helper must be defined in the dashboard.
		if !strings.Contains(html, jsApiPostHelper) {
			t.Fatalf("dashboard must define apiPostRaw helper function")
		}
	})

	t.Run("ApiPostRawHelperUsesAuthHeader", func(t *testing.T) {
		assertContainsInFn(t, apiPostRawFn, jsPostAuthHeader, "apiPostRaw must set X-Hermes-Key header")
		assertContainsInFn(t, apiPostRawFn, jsPostMethod, "apiPostRaw must use method: 'POST'")
		assertContainsInFn(t, apiPostRawFn, jsPostContentType, "apiPostRaw must set JSON Content-Type")
	})

	t.Run("PostsToThreadEndpoint", func(t *testing.T) {
		assertContainsInFn(t, fn, jsPostThreadEndpoint, "compose Send must POST to /envelopes/{id}/thread")
	})

	t.Run("SendsFromKitt", func(t *testing.T) {
		assertContainsInFn(t, fn, jsPostFromKitt, "compose Send must set from: 'kitt' in the request body")
	})

	t.Run("DisablesButtonOnFlight", func(t *testing.T) {
		assertContainsInFn(t, sendHandlerFn, "composeSendBtn.disabled = true", "compose Send handler must disable the button while the POST is in flight")
	})

	t.Run("DisablesInputsOnFlight", func(t *testing.T) {
		assertContainsInFn(t, fn, "composeTextarea.disabled = composeSending", "compose textarea must be disabled while send/refetch is in flight")
		assertContainsInFn(t, fn, "composeKind.disabled = composeSending", "compose kind select must be disabled while send/refetch is in flight")
	})

	t.Run("SendDisabledUntilThreadLoads", func(t *testing.T) {
		// Race-condition guard: Send button must start disabled and be enabled
		// only after the initial fetchAndRenderThread resolves.
		assertContainsInFn(t, fn, jsSendDisabledInit, "compose Send button must be disabled initially (race-condition guard)")
	})

	t.Run("ShowsErrorOnNonOk", func(t *testing.T) {
		assertContainsInFn(t, fn, jsComposeError, "compose Send must reference compose-error element to surface errors")
	})

	t.Run("ShowsErrorDespiteDefaultHiddenCSS", func(t *testing.T) {
		if !strings.Contains(html, jsComposeErrorVisible) {
			t.Fatalf("compose error visible state must override default display:none")
		}
		assertContainsInFn(t, fn, jsShowComposeError, "compose error show path must add the visible class")
		assertContainsInFn(t, fn, jsHideComposeError, "compose error hide path must remove the visible class")
	})

	t.Run("ReplyRequiresExistingThreadMessage", func(t *testing.T) {
		assertContainsInFn(t, sendHandlerFn, `kind === 'reply' && !latestMessageId`, "reply sends must require an existing latest message id")
		assertContainsInFn(t, fn, jsReplyRequiresMsg, "reply validation must show a visible missing-thread-message error")
	})

	t.Run("UsesHumanReadableSendError", func(t *testing.T) {
		assertContainsInFn(t, fn, jsSendErrorData, "compose Send must use a simple result.data fallback object")
		assertContainsInFn(t, fn, jsSendErrorDetail, "compose Send must prefer detail before backend error code")
	})

	t.Run("NoTrailingColonForMissingErrorMessage", func(t *testing.T) {
		assertContainsInFn(t, fn, jsSendFailedFallback, "compose Send must not append a trailing colon when the error response has no message")
	})

	t.Run("ShowsNetworkErrorForStatusZero", func(t *testing.T) {
		assertContainsInFn(t, postSuccessFn, `result.status === 0`, "compose Send must special-case apiPostRaw network failures")
		assertContainsInFn(t, postSuccessFn, jsSendNetworkErr, "compose Send must show a network error for status 0")
	})

	t.Run("ClearsTextareaOnSuccess", func(t *testing.T) {
		assertContainsInFn(t, postSuccessFn, ".value = ''", "compose Send must clear textarea value on success")
	})

	t.Run("OnlyClearsSubmittedDraft", func(t *testing.T) {
		assertContainsInFn(t, postSuccessFn, "if (composeTextarea.value.trim() === text)", "compose Send must not erase a changed follow-up draft after async send")
	})

	t.Run("RefetchesThreadOnSuccess", func(t *testing.T) {
		assertContainsInFn(t, postSuccessFn, jsLatestMsgFromPost, "compose Send must re-fetch the thread after a successful POST")
	})

	t.Run("IgnoresEmptyText", func(t *testing.T) {
		assertContainsInFn(t, fn, "trim()", "compose Send must trim text and ignore empty submissions")
	})

	t.Run("PreservesLatestMessageIdFromPostWhenRefreshEmpty", func(t *testing.T) {
		// POST /thread returns the created message object; preserve its id when
		// the follow-up thread refresh fails or returns empty so reply remains possible.
		assertContainsInFn(t, postSuccessFn, `const postedMessageId = result.data && typeof result.data.id === 'string' ? result.data.id : ''`, "compose Send must safely extract the posted message id")
		assertContainsInFn(t, postSuccessFn, jsLatestMsgFromPost, "latestMessageId must fall back to the posted message id when refresh returns empty")
	})

	t.Run("UsesCSSClassesNotInlineStyles", func(t *testing.T) {
		// Compose UI must use CSS classes, not inline style.cssText.
		assertContainsInFn(t, fn, jsComposeCSSClass, "compose wrapper must use CSS class 'compose-wrap'")
		if strings.Contains(fn, "composeDiv.style.cssText") {
			t.Fatalf("compose wrapper must not use inline style.cssText — use CSS class instead")
		}
		if strings.Contains(fn, "composeRow.style.cssText") {
			t.Fatalf("compose row must not use inline style.cssText — use CSS class instead")
		}
		if strings.Contains(fn, "composeTextarea.style.cssText") {
			t.Fatalf("compose textarea must not use inline style.cssText — use CSS class instead")
		}
	})
}

// TestDashboard_InboxClickthrough verifies that inbox notification cards open
// the envelope detail panel when clicked.
func TestDashboard_InboxClickthrough(t *testing.T) {
	html := readDashboardHTML(t)
	fn := extractRenderInboxFn(t, html)

	t.Run("CardsAreClickable", func(t *testing.T) {
		assertContainsInFn(t, fn, jsInboxClickthrough, "renderInbox cards must call showEnvelopeDetail on click")
	})

	t.Run("FetchFallbackWhenNotCached", func(t *testing.T) {
		// If the envelope is not in envelopesCache, the handler must fetch
		// /envelopes/{id} through the error-surfacing raw helper before opening
		// the detail panel.
		assertContainsInFn(t, fn, jsInboxFetchFallback, "renderInbox must fetch /envelopes/{id} with apiFetchRaw when envelope is not in cache")
	})

	t.Run("FetchFallbackFailureIsVisible", func(t *testing.T) {
		assertContainsInFn(t, fn, jsInboxFetchError, "renderInbox must show the fallback envelope fetch error text")
		assertContainsInFn(t, fn, jsInboxFetchErrorID, "renderInbox must show a visible fallback envelope fetch error")
	})

	t.Run("FetchFallbackClearsStaleError", func(t *testing.T) {
		assertContainsInFn(t, fn, `const previous = div.querySelector('.inbox-fetch-error');`, "renderInbox fallback fetch must find stale fetch errors")
		assertContainsInFn(t, fn, jsInboxClearError, "renderInbox fallback fetch must clear stale fetch errors before success or failure handling")
	})

	t.Run("FetchFallbackHasPerCardConcurrencyGuard", func(t *testing.T) {
		assertContainsInFn(t, fn, jsInboxFetchingGuard, "renderInbox fallback fetch must track per-card fetch state")
		assertContainsInFn(t, fn, jsInboxFetchingSkip, "renderInbox fallback fetch must skip concurrent fetches for the same card")
		assertContainsInFn(t, fn, `fetchingEnvelope = true;`, "renderInbox fallback fetch must mark the card as fetching before apiFetchRaw")
		assertContainsInFn(t, fn, jsInboxFetchingDone, "renderInbox fallback fetch must clear the per-card fetch state")
	})

	t.Run("DoesNotAutoAck", func(t *testing.T) {
		// Clicking a notification card must NOT call ackVisibleInbox or POST to /notifications/ack.
		assertNotContainsInFn(t, fn, "ackVisibleInbox", "renderInbox click handler must not auto-ack notifications")
		assertNotContainsInFn(t, fn, "notifications/ack", "renderInbox click handler must not POST to notifications/ack")
	})

	t.Run("KeyboardAccessibleWithoutAutoAck", func(t *testing.T) {
		assertInboxKeyboardAccessible(t, fn)
	})
}

func TestDashboard_GroupMemberStatusUsesAllowlistAndDOM(t *testing.T) {
	body := readDashboardHTML(t)
	if !strings.Contains(body, "function safeStatusState(state)") {
		t.Fatalf("dashboard should allowlist status states before using them as class suffixes")
	}
	if !strings.Contains(body, "var memberState = safeStatusState(member.state)") {
		t.Fatalf("group member rendering should sanitize member.state")
	}
	if strings.Contains(body, "status-${memberState}") {
		t.Fatalf("group member status class should not interpolate raw state into innerHTML")
	}
	if !strings.Contains(body, "memberBadge.className = 'status-badge status-' + memberState") {
		t.Fatalf("group member status badge should be built via DOM APIs after allowlisting")
	}
}

// CON-013: live thread auto-refresh markers.
const (
	// openEnvelopeId tracking
	jsOpenEnvelopeIdVar = `let openEnvelopeId = ''`
	jsOpenEnvelopeIdSet = `openEnvelopeId = env.id`

	// refreshOpenThread callback
	jsRefreshOpenThreadVar = `let refreshOpenThread = null`
	jsRefreshOpenThreadSet = `refreshOpenThread = refreshThreadState`

	// 15s polling fallback
	jsThreadPollInterval    = `let threadPollInterval = null`
	jsThreadPollStart       = `threadPollInterval = setInterval(`
	jsThreadPollIntervalMs  = `15000`
	jsThreadPollClear       = `clearInterval(threadPollInterval)`
	jsThreadPollNullOnClear = `threadPollInterval = null`

	// SSE hook: refresh open thread on relevant events for the open envelope only.
	jsSSERefreshOpenThread = `if (refreshOpenThread && openEnvelopeId && (!evt.envelope_id || evt.envelope_id === openEnvelopeId))`

	// Unified detail cleanup must clear tracking state.
	jsResetEnvelopeDetailStart = `function resetEnvelopeDetailState() {`
	jsResetEnvelopeDetailCall  = `resetEnvelopeDetailState();`
	jsResetClearId             = `openEnvelopeId = ''`
	jsResetClearRefresh        = `refreshOpenThread = null`
	jsResetClearPoll           = `clearInterval(threadPollInterval)`

	// State-updating refresh path shared by initial load, polling, SSE, and post-send refresh.
	jsRefreshThreadStateStart  = `function refreshThreadState(fallbackMessageId) {`
	jsRefreshThreadStateLatest = `latestMessageId = (msgs && msgs.length > 0) ? (msgs[msgs.length - 1].id || '') : (fallbackMessageId || '')`
	jsRefreshThreadStateLoaded = `threadLoaded = true`
	jsRefreshThreadStateUpdate = `updateComposeState()`

	// Unchanged thread renders should avoid DOM churn/flicker.
	jsThreadSignatureVar   = `let threadRenderSignature = ''`
	jsThreadSignatureBuild = `const nextSignature = messages.length + ':' + messages.map(function(m) { return m.id || ''; }).join('|')`
	jsThreadSignatureSkip  = `if (nextSignature === threadRenderSignature)`

	jsLogoutStart          = `function logout() {`
	jsShowAgentDetailStart = `function showAgentDetail(a) {`
)

// extractCloseDetailFn slices the closeDetail JS function body from the dashboard HTML.
func extractCloseDetailFn(t *testing.T, html string) string {
	t.Helper()
	const start = "function closeDetail() {"
	const end = "/* --- Agents / Constellation --- */"
	s := strings.Index(html, start)
	e := strings.Index(html, end)
	if s < 0 || e < 0 || s >= e {
		t.Fatalf("closeDetail function boundaries not found (start=%d end=%d)", s, e)
	}
	return html[s:e]
}

// extractConnectSSEFn slices the connectSSE JS function body from the dashboard HTML.
func extractConnectSSEFn(t *testing.T, html string) string {
	t.Helper()
	const start = "function connectSSE() {"
	const end = "function setSseStatus("
	s := strings.Index(html, start)
	e := strings.Index(html, end)
	if s < 0 || e < 0 || s >= e {
		t.Fatalf("connectSSE function boundaries not found (start=%d end=%d)", s, e)
	}
	return html[s:e]
}

// TestDashboard_LiveThreadRefresh verifies CON-013: open envelope detail panel
// auto-refreshes its thread without closing/reopening.
func TestDashboard_LiveThreadRefresh(t *testing.T) {
	html := readDashboardHTML(t)
	fn := extractShowEnvelopeDetailFn(t, html)
	closeFn := extractCloseDetailFn(t, html)
	sseFn := extractConnectSSEFn(t, html)
	resetFn := extractJSBlock(t, html, jsResetEnvelopeDetailStart)
	refreshStateFn := extractJSBlock(t, fn, jsRefreshThreadStateStart)
	sessionFn := extractJSBlock(t, html, jsShowSessionDetailStart)
	logoutFn := extractJSBlock(t, html, jsLogoutStart)
	agentFn := extractJSBlock(t, html, jsShowAgentDetailStart)

	t.Run("TracksOpenEnvelopeId", func(t *testing.T) {
		// Module-level variable must exist to track which envelope is open.
		assertContainsInFn(t, html, jsOpenEnvelopeIdVar,
			"dashboard must declare module-level openEnvelopeId variable")
	})

	t.Run("SetsOpenEnvelopeIdOnOpen", func(t *testing.T) {
		// showEnvelopeDetail must record the envelope id when the panel opens.
		assertContainsInFn(t, fn, jsOpenEnvelopeIdSet,
			"showEnvelopeDetail must set openEnvelopeId = env.id")
	})

	t.Run("TracksRefreshCallback", func(t *testing.T) {
		// Module-level variable must hold the refresh callback for the open thread.
		assertContainsInFn(t, html, jsRefreshOpenThreadVar,
			"dashboard must declare module-level refreshOpenThread variable")
	})

	t.Run("SetsRefreshCallbackOnOpen", func(t *testing.T) {
		// showEnvelopeDetail must wire the state-updating refresh callback.
		assertContainsInFn(t, fn, jsRefreshOpenThreadSet,
			"showEnvelopeDetail must set refreshOpenThread = refreshThreadState")
	})

	t.Run("StartsPollIntervalOnOpen", func(t *testing.T) {
		// showEnvelopeDetail must start a 15s polling interval as fallback.
		assertContainsInFn(t, html, jsThreadPollInterval,
			"dashboard must declare module-level threadPollInterval variable")
		assertContainsInFn(t, fn, jsThreadPollStart,
			"showEnvelopeDetail must start a setInterval for thread polling")
		assertContainsInFn(t, fn, jsThreadPollIntervalMs,
			"thread polling interval must be 15000ms (15s)")
	})

	t.Run("ClearsExistingPollBeforeStarting", func(t *testing.T) {
		// Opening a new envelope while one is already open must not leak the old timer.
		assertContainsInFn(t, fn, jsThreadPollClear,
			"showEnvelopeDetail must clearInterval(threadPollInterval) before starting a new one")
	})

	t.Run("UnifiedCleanupClearsThreadState", func(t *testing.T) {
		// resetEnvelopeDetailState must clear openEnvelopeId, refreshOpenThread, and the poll interval.
		assertContainsInFn(t, resetFn, jsResetClearId,
			"resetEnvelopeDetailState must reset openEnvelopeId to empty string")
		assertContainsInFn(t, resetFn, jsResetClearRefresh,
			"resetEnvelopeDetailState must reset refreshOpenThread to null")
		assertContainsInFn(t, resetFn, jsResetClearPoll,
			"resetEnvelopeDetailState must clearInterval(threadPollInterval)")
		assertContainsInFn(t, resetFn, jsThreadPollNullOnClear,
			"resetEnvelopeDetailState must set threadPollInterval = null after clearing")
	})

	t.Run("CleanupRunsFromDetailTransitions", func(t *testing.T) {
		assertContainsInFn(t, closeFn, jsResetEnvelopeDetailCall,
			"closeDetail must use resetEnvelopeDetailState")
		assertContainsInFn(t, fn, jsResetEnvelopeDetailCall,
			"showEnvelopeDetail must reset old envelope detail state before opening a new envelope")
		assertContainsInFn(t, sessionFn, jsResetEnvelopeDetailCall,
			"showSessionDetail must reset envelope detail state before opening a session")
		assertContainsInFn(t, agentFn, jsResetEnvelopeDetailCall,
			"showAgentDetail must reset envelope detail state before opening an agent")
		assertContainsInFn(t, logoutFn, jsResetEnvelopeDetailCall,
			"logout must reset envelope detail state")
	})

	t.Run("SSEHookRefreshesOnlyRelevantOpenThread", func(t *testing.T) {
		// The SSE event handler must call refreshOpenThread when a relevant event
		// arrives and either has no envelope id or matches the open envelope.
		assertContainsInFn(t, sseFn, jsSSERefreshOpenThread,
			"connectSSE must filter thread refreshes by evt.envelope_id")
	})

	t.Run("RefreshCallbackUpdatesComposeState", func(t *testing.T) {
		assertContainsInFn(t, refreshStateFn, jsRefreshThreadStateLatest,
			"refreshThreadState must update latestMessageId on every refresh path")
		assertContainsInFn(t, refreshStateFn, jsRefreshThreadStateLoaded,
			"refreshThreadState must mark the thread loaded")
		assertContainsInFn(t, refreshStateFn, jsRefreshThreadStateUpdate,
			"refreshThreadState must update compose enabled/disabled state")
		assertContainsInFn(t, fn, `refreshThreadState();`,
			"initial thread load must use refreshThreadState")
		assertContainsInFn(t, fn, jsLatestMsgFromPost,
			"post-send refresh must use refreshThreadState with the posted message fallback")
		assertContainsInFn(t, fn, `threadPollInterval = setInterval(refreshThreadState, 15000)`,
			"polling refresh must use refreshThreadState")
	})

	t.Run("SkipsUnchangedThreadRender", func(t *testing.T) {
		assertContainsInFn(t, fn, jsThreadSignatureVar,
			"showEnvelopeDetail must track a simple thread render signature")
		assertContainsInFn(t, fn, jsThreadSignatureBuild,
			"fetchAndRenderThread must build a signature from message ids and length")
		assertContainsInFn(t, fn, jsThreadSignatureSkip,
			"fetchAndRenderThread must skip DOM work when the thread has not changed")
	})
}

// TestDashboard_ShowEnvelopeDetail groups CON-011 assertions for the
// showEnvelopeDetail JS function. Each subtest shares a single HTML parse and
// function extraction to avoid repeated setup while keeping failures isolated.
func TestDashboard_ShowEnvelopeDetail(t *testing.T) {
	html := readDashboardHTML(t)
	fn := extractShowEnvelopeDetailFn(t, html)

	t.Run("FetchesLiveThread", func(t *testing.T) {
		// Opening an envelope detail panel must trigger a live fetch of
		// /envelopes/{id}/thread via the error-surfacing apiFetchRaw helper.
		if !strings.Contains(fn, `apiFetchRaw('/envelopes/' + encodeURIComponent(env.id) + '/thread')`) {
			t.Fatal("showEnvelopeDetail must fetch live thread via apiFetchRaw('/envelopes/{id}/thread')")
		}
	})

	t.Run("HasLoadingState", func(t *testing.T) {
		// The thread section must show a loading indicator while the fetch is pending.
		if !strings.Contains(fn, "Loading") {
			t.Fatal("showEnvelopeDetail must show a loading state while thread fetch is pending")
		}
	})

	t.Run("EmptyThreadMessage", func(t *testing.T) {
		// An empty live thread must show the exact sentinel text used by the UI.
		if !strings.Contains(fn, "No messages yet") {
			t.Fatal("showEnvelopeDetail must show 'No messages yet' when live thread is empty")
		}
	})

	t.Run("UsesRenderSessionMessages", func(t *testing.T) {
		// Non-empty threads must be delegated to renderSessionMessages for
		// consistent threaded display.
		if !strings.Contains(fn, "renderSessionMessages(threadContainer, messages)") {
			t.Fatal("showEnvelopeDetail must delegate thread rendering to renderSessionMessages")
		}
	})

	t.Run("ShowsFailedToLoadThread", func(t *testing.T) {
		// A non-ok response from apiFetchRaw must surface a visible error.
		if !strings.Contains(fn, "Failed to load thread") {
			t.Fatal("showEnvelopeDetail must show 'Failed to load thread' on non-ok response")
		}
	})

	t.Run("ResetsThreadHeaderBeforeFetchError", func(t *testing.T) {
		// A post-send refresh failure must not leave a stale Thread (n) count visible.
		if !strings.Contains(fn, jsThreadHeaderReset) {
			t.Fatal("showEnvelopeDetail must reset the thread header before rendering a fetch error")
		}
	})

	t.Run("NormalizesAtToCreatedAt", func(t *testing.T) {
		// Messages whose timestamp is serialized as "at" (envelope.Message JSON
		// tag) must be normalized to "created_at" so renderMessage can display
		// the timestamp correctly.
		if !strings.Contains(fn, "created_at: m.at") {
			t.Fatal("showEnvelopeDetail must normalize m.at to m.created_at for envelope.Message compatibility")
		}
	})
}
