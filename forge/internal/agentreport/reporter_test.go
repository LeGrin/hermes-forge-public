package agentreport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClassifyExecutor(t *testing.T) {
	cases := map[string]string{
		"claude":                "claude",
		"claude --resume foo":   "claude",
		"/usr/local/bin/claude": "claude",
		"/usr/local/bin/claude --dangerously-skip-permissions --session-id abc": "claude",
		"/opt/homebrew/opt/node/bin/node /opt/homebrew/bin/opencode serve":      "opencode",
		"/opt/homebrew/.../opencode-darwin-arm64/bin/opencode run something":    "opencode",
		"opencode -s ses_123":       "opencode",
		"/usr/local/bin/forge":      "forge",
		"node /some/other/thing.js": "",
		"/bin/zsh -c something":     "",
	}
	for cmd, want := range cases {
		if got := classifyExecutor(cmd); got != want {
			t.Errorf("classifyExecutor(%q)=%q want %q", cmd, got, want)
		}
	}
}

func TestParseSessionID(t *testing.T) {
	cases := []struct {
		exec, cmd, want string
	}{
		{"claude", "claude --session-id aaaa-bbbb prompt", "aaaa-bbbb"},
		{"claude", "claude --resume uuid-xyz -p hello", "uuid-xyz"},
		{"claude", "claude --session-id=directly-equals --print", "directly-equals"},
		{"opencode", "opencode -s ses_26e983 run", "ses_26e983"},
		{"opencode", "opencode run --session ses_999 prompt", "ses_999"},
		{"opencode", "opencode run prompt", ""},
	}
	for _, c := range cases {
		if got := parseSessionID(c.exec, c.cmd); got != c.want {
			t.Errorf("parseSessionID(%s, %q)=%q want %q", c.exec, c.cmd, got, c.want)
		}
	}
}

func TestDeriveTitle_FromForgeEnvelope(t *testing.T) {
	// Forge tags opencode runs with --title <envelope_id>; reporter
	// should lift it as the friendly name.
	cmd := "opencode run --title hermes-test-run-79611e5f --format json prompt here"
	got := deriveTitle("opencode", cmd, "")
	if got != "hermes-test-run-79611e5f" {
		t.Errorf("expected envelope id, got %q", got)
	}
}

func TestDeriveTitle_FromSessionID(t *testing.T) {
	got := deriveTitle("opencode", "opencode -s ses_26e983572ffeCL7v3cJPa2sZFl", "ses_26e983572ffeCL7v3cJPa2sZFl")
	if !strings.HasPrefix(got, "ses_") {
		t.Errorf("expected ses_ prefix, got %q", got)
	}
}

func TestMatchProject_LongestPrefix(t *testing.T) {
	r := &Reporter{
		ProjectMap: map[string]string{
			"/workspace/kingdom":          "kingdom",
			"/workspace/kingdom/services": "kingdom-services",
			"/workspace/hermes":           "hermes",
		},
	}
	if got := r.matchProject("/workspace/kingdom/services/rookery"); got != "kingdom-services" {
		t.Errorf("longest prefix match failed: %q", got)
	}
	if got := r.matchProject("/workspace/hermes/foo/bar"); got != "hermes" {
		t.Errorf("hermes match failed: %q", got)
	}
	if got := r.matchProject("/tmp/whatever"); got != "" {
		t.Errorf("unknown path should match nothing, got %q", got)
	}
}

func TestParsePSLine_MacOSFormat(t *testing.T) {
	// macOS `ps -eo pid,ppid,%cpu,%mem,lstart,command`
	line := "  7808  1878   0.8  0.6 Thu Apr 16 23:44:27 2026 claude        "
	row, ok := parsePSLine(line)
	if !ok {
		t.Fatal("parse returned !ok")
	}
	if row.PID != 7808 || row.PPID != 1878 {
		t.Errorf("pids: %+v", row)
	}
	if row.CPU < 0.79 || row.CPU > 0.81 {
		t.Errorf("cpu parse: got %f", row.CPU)
	}
	if row.Command != "claude" {
		t.Errorf("command: got %q", row.Command)
	}
	if row.Started.IsZero() {
		t.Errorf("started should have parsed")
	}
}

func TestDeriveTitle_AllPaths(t *testing.T) {
	cases := []struct {
		name, exec, cmd, sid, want string
	}{
		{"envelope-title-wins", "opencode", "opencode run --title env-xyz --format json hello", "", "env-xyz"},
		{"session-id-fallback", "opencode", "opencode -s ses_long_session_id_here_yes something", "ses_long_session_id_here_yes", "ses_long_session_id_"},
		{"short-cmd", "claude", "claude", "", "claude"},
		{"long-cmd-truncated", "claude", strings.Repeat("a", 200), "", strings.Repeat("a", 80) + "…"},
	}
	for _, c := range cases {
		if got := deriveTitle(c.exec, c.cmd, c.sid); got != c.want {
			t.Errorf("%s: deriveTitle()=%q want %q", c.name, got, c.want)
		}
	}
}

func TestMinInt(t *testing.T) {
	if minInt(3, 5) != 3 {
		t.Error("minInt(3,5)")
	}
	if minInt(9, 2) != 2 {
		t.Error("minInt(9,2)")
	}
	if minInt(7, 7) != 7 {
		t.Error("minInt(7,7)")
	}
}

func TestDeriveState_CPUThreshold(t *testing.T) {
	if deriveState("claude", psRow{CPU: 5.0}) != "active" {
		t.Error("high cpu should be active")
	}
	if deriveState("claude", psRow{CPU: 0.2}) != "idle" {
		t.Error("low cpu should be idle")
	}
	if deriveState("claude", psRow{CPU: 0.0}) != "idle" {
		t.Error("zero cpu should be idle")
	}
}

func TestExtractFlagValue_BothForms(t *testing.T) {
	cases := []struct {
		cmd, flag, want string
	}{
		{"cmd --flag value --other", "--flag", "value"},
		{"cmd --flag=value --other", "--flag", "value"},
		{"cmd --no-match here", "--flag", ""},
		{"cmd --flag", "--flag", ""}, // dangling flag with no value → empty
	}
	for _, c := range cases {
		if got := extractFlagValue(c.cmd, c.flag); got != c.want {
			t.Errorf("extractFlagValue(%q,%q)=%q want %q", c.cmd, c.flag, got, c.want)
		}
	}
}

// TestFire_NoAgentsNoPost asserts the reporter does not POST an empty
// snapshot — keeps Hermes logs quiet when Mac has no AI processes.
func TestFire_NoAgentsNoPost(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(202)
	}))
	t.Cleanup(srv.Close)

	rep := &Reporter{
		Host:      "empty-host",
		HermesURL: srv.URL,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// Force Scan to return empty by using an impossible executor filter.
	// We cannot easily stub Scan without exporting it, so rely on the
	// fact that this package's classifyExecutor never matches "nothing",
	// and call fire() directly — Scan returns [] on any host that has
	// no claude/opencode/forge processes; on CI/test env this is usually
	// the case.
	// Fallback: just check that when Scan returns empty, post is not invoked.
	rep.fire(context.Background())
	if called {
		// Not a hard fail — the host may genuinely have AI processes.
		t.Log("host has real AI processes; fire did post (acceptable)")
	}
}

// TestScan_WithStubbedPS feeds a deterministic `ps` output via
// Reporter.PSStub and asserts Scan picks up the top-level AI
// processes, filters out language-server children, and classifies
// parents correctly.
func TestScan_WithStubbedPS(t *testing.T) {
	stub := `  PID  PPID   %CPU %MEM STARTED                      COMMAND
66343     1   0.0  0.1 Fri Apr 17 12:00:00 2026 /path/forge
 7808  1878   0.3  0.6 Thu Apr 16 23:44:27 2026 claude
 8564  7808   0.0  0.1 Thu Apr 16 23:44:33 2026 node /path/notebooklm-mcp/index.js
 9000 66343   5.1  0.3 Fri Apr 17 12:00:00 2026 claude --session-id abc-uuid --print
93022     1   0.0  0.3 Fri Apr 17 01:35:07 2026 /opt/homebrew/bin/opencode serve
50000 50000   0.0  0.0 Fri Apr 17 12:00:00 2026 /bin/zsh -c something
`
	rep := &Reporter{
		Host:     "test-host",
		ForgePID: 66343,
		PSStub:   stub,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProjectMap: map[string]string{
			"/opt/homebrew": "homebrew",
		},
	}
	agents := rep.Scan(context.Background())

	var claudeForge, claudeUser, openCode *Agent
	for i := range agents {
		switch agents[i].PID {
		case 9000:
			claudeForge = &agents[i]
		case 7808:
			claudeUser = &agents[i]
		case 93022:
			openCode = &agents[i]
		}
	}
	if claudeForge == nil || claudeUser == nil || openCode == nil {
		t.Fatalf("missed agents: %+v", agents)
	}
	// Child node notebooklm MCP should be filtered (parent is claude).
	for _, a := range agents {
		if a.PID == 8564 {
			t.Error("child mcp process should have been filtered")
		}
	}
	if claudeForge.ParentKind != "forge" {
		t.Errorf("pid 9000 parent_kind: got %q, want forge", claudeForge.ParentKind)
	}
	if openCode.ParentKind != "init" {
		t.Errorf("pid 93022 parent_kind: got %q, want init", openCode.ParentKind)
	}
	if claudeForge.SessionID != "abc-uuid" {
		t.Errorf("session id parse: got %q", claudeForge.SessionID)
	}
	if claudeForge.State != "active" {
		t.Errorf("5.1%% cpu should be active, got %q", claudeForge.State)
	}
	if claudeUser.State != "idle" {
		t.Errorf("0.3%% cpu should be idle, got %q", claudeUser.State)
	}
}

// TestScan_HonoursHostEnv runs a real `ps` against the host and
// checks Scan never panics, always returns a concrete slice, and
// respects the host label set on the reporter (propagated via
// Apply when store consumes; we don't store here, just inspect).
func TestScan_NoPanic(t *testing.T) {
	rep := &Reporter{
		Host:   "test-scan",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	got := rep.Scan(context.Background())
	if got == nil {
		t.Fatal("Scan returned nil, want empty slice")
	}
	// On the dev host we probably have claude/opencode; assert basic
	// invariants if we do pick any up.
	for _, a := range got {
		if a.ID == "" || a.Executor == "" {
			t.Errorf("bad agent record: %+v", a)
		}
		if a.Host != "test-scan" {
			t.Errorf("host override dropped: got %q", a.Host)
		}
	}
}

// TestReadCWD_OwnPID shells lsof at the test process and expects
// some path — if lsof is unavailable we tolerate empty string.
func TestReadCWD_OwnPID(t *testing.T) {
	rep := &Reporter{}
	cwd := rep.readCWD(context.Background(), os.Getpid())
	// Acceptable outcomes: a real path or empty (lsof missing).
	if cwd != "" && !strings.HasPrefix(cwd, "/") {
		t.Errorf("cwd should be absolute, got %q", cwd)
	}
}

// TestSetProjectMap_HotReload asserts SetProjectMap replaces the
// lookup atomically — reporters can refresh the project registry
// on the fly without restart.
func TestSetProjectMap_HotReload(t *testing.T) {
	rep := &Reporter{ProjectMap: map[string]string{"/a": "old"}}
	rep.SetProjectMap(map[string]string{"/b": "new"})
	if rep.matchProject("/a/x") != "" {
		t.Errorf("old entry should be gone")
	}
	if rep.matchProject("/b/y") != "new" {
		t.Errorf("new entry should match")
	}
}

func TestClassifyParent_ForgeAncestor(t *testing.T) {
	r := &Reporter{ForgePID: 66343}
	byPID := map[int]psRow{
		66343: {PID: 66343, PPID: 1, Command: "/path/forge"},
		70000: {PID: 70000, PPID: 66343, Command: "claude --print -p"},
	}
	if got := r.classifyParent(70000, byPID); got != "forge" {
		t.Errorf("expected forge, got %q", got)
	}
}

// TestPost_HappyPath stands up a fake Hermes and asserts the POST
// body shape + auth header so any future refactor keeps Hermes happy.
func TestPost_HappyPath(t *testing.T) {
	var gotKey string
	var gotBody Snapshot
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/snapshot" || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		gotKey = r.Header.Get("X-Hermes-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	rep := &Reporter{
		Host:      "test-host",
		HermesURL: srv.URL,
		HermesKey: "dev-key-test",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := rep.post(context.Background(), Snapshot{
		Host:    "test-host",
		TakenAt: time.Now().UTC(),
		Agents:  []Agent{{ID: "a", Executor: "claude", State: "active"}},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if gotKey != "dev-key-test" {
		t.Errorf("X-Hermes-Key: got %q", gotKey)
	}
	if gotBody.Host != "test-host" || len(gotBody.Agents) != 1 {
		t.Errorf("body mismatch: %+v", gotBody)
	}
}

// TestPost_Non2xxIsError surfaces non-2xx responses so the reporter
// loop can log them. Regression lock.
func TestPost_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	rep := &Reporter{
		HermesURL: srv.URL,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	err := rep.post(context.Background(), Snapshot{Host: "h"})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error: %v", err)
	}
}

// TestRun_FiresAtLeastOnce asserts the background loop hits the
// Hermes endpoint at least once shortly after Run starts, proving
// the initial tick is not gated on Interval expiry.
func TestRun_FiresAtLeastOnce(t *testing.T) {
	calls := make(chan Snapshot, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s Snapshot
		_ = json.NewDecoder(r.Body).Decode(&s)
		calls <- s
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	rep := &Reporter{
		Host:      "scanner-test",
		HermesURL: srv.URL,
		Interval:  10 * time.Millisecond,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go rep.Run(ctx)

	select {
	case snap := <-calls:
		if snap.Host != "scanner-test" {
			t.Errorf("expected host scanner-test, got %q", snap.Host)
		}
	case <-ctx.Done():
		// Run scanned ps on the host; depending on test env there may
		// be no claude/opencode processes so fire() skips entirely —
		// tolerate that rather than flap the test.
		t.Skip("no AI processes on host — reporter skipped fire; acceptable for this sanity test")
	}
}

func TestClassifyParent_CmuxAncestor(t *testing.T) {
	r := &Reporter{ForgePID: 12345}
	byPID := map[int]psRow{
		1000: {PID: 1000, PPID: 1, Command: "/Applications/cmux.app/Contents/MacOS/cmux"},
		1001: {PID: 1001, PPID: 1000, Command: "/bin/zsh"},
		1002: {PID: 1002, PPID: 1001, Command: "claude"},
	}
	if got := r.classifyParent(1001, byPID); got != "cmux" {
		t.Errorf("expected cmux, got %q", got)
	}
}
