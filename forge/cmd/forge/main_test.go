package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legrin-tech/forge/internal/httpapi"
)

func prependFakeExecutorToPath(t *testing.T, name string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake %s executor: %v", name, err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWriteMCPConfig(t *testing.T) {
	path, err := writeMCPConfig("/usr/local/bin/hermes-mcp", "http://localhost:8080", "", "env-test-123")
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer os.Remove(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var cfg struct {
		MCPServers struct {
			Hermes struct {
				Command string            `json:"command"`
				Env     map[string]string `json:"env"`
			} `json:"hermes"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if cfg.MCPServers.Hermes.Command != "/usr/local/bin/hermes-mcp" {
		t.Errorf("command = %q, want hermes-mcp path", cfg.MCPServers.Hermes.Command)
	}
	if cfg.MCPServers.Hermes.Env["HERMES_URL"] != "http://localhost:8080" {
		t.Errorf("HERMES_URL = %q", cfg.MCPServers.Hermes.Env["HERMES_URL"])
	}
	if cfg.MCPServers.Hermes.Env["ENVELOPE_ID"] != "env-test-123" {
		t.Errorf("ENVELOPE_ID = %q", cfg.MCPServers.Hermes.Env["ENVELOPE_ID"])
	}
	if _, ok := cfg.MCPServers.Hermes.Env["HERMES_KEY"]; ok {
		t.Error("HERMES_KEY should not be present when hermesKey is empty")
	}
}

func TestWriteMCPConfig_WithKey(t *testing.T) {
	path, err := writeMCPConfig("/usr/local/bin/hermes-mcp", "http://localhost:8080", "secret-key", "env-test-456")
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer os.Remove(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var cfg struct {
		MCPServers struct {
			Hermes struct {
				Env map[string]string `json:"env"`
			} `json:"hermes"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if cfg.MCPServers.Hermes.Env["HERMES_KEY"] != "secret-key" {
		t.Errorf("HERMES_KEY = %q, want secret-key", cfg.MCPServers.Hermes.Env["HERMES_KEY"])
	}
}

func TestBuildLauncher_StubWhenNoBin(t *testing.T) {
	l := buildLauncher(slog.New(slog.NewTextHandler(io.Discard, nil)), "", "", "")
	if l == nil {
		t.Fatal("expected stub launcher, got nil")
	}
}

// TestBuildLauncher_StubOnlyMcpBinMissing asserts that HERMES_URL alone IS
// now enough to get a real launcher — the old gating on HERMES_MCP_BIN was
// wrong because hermes-agent does not use MCP. This test locks the new
// behaviour: stub only when HERMES_URL is absent.
func TestBuildLauncher_StubOnlyMcpBinMissing(t *testing.T) {
	// With HERMES_URL set but no MCP bin, we should get a real launcher
	// (not stub) so hermes-agent can be dispatched.
	l := buildLauncher(slog.New(slog.NewTextHandler(io.Discard, nil)), "http://hermes", "dev-key-key", "")
	if l == nil {
		t.Fatal("expected real launcher even without mcp bin")
	}
}

// TestBuildLauncher_RealConfigured covers the "real launcher" branch
// when both HERMES_URL and HERMES_MCP_BIN are set. The returned
// closure is not invoked (no executor on CI), but construction is
// what we want to exercise.
func TestBuildLauncher_RealConfigured(t *testing.T) {
	l := buildLauncher(slog.New(slog.NewTextHandler(io.Discard, nil)), "http://hermes", "dev-key-key", "/bin/true")
	if l == nil {
		t.Fatal("real launcher should not be nil")
	}
}

func TestBuildExecutorPrompt(t *testing.T) {
	prompt := buildExecutorPrompt("env-42")
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !strings.Contains(prompt, "env-42") {
		t.Error("prompt should contain envelope ID")
	}
	if !strings.Contains(prompt, "hermes_update_status") {
		t.Error("prompt should mention hermes_update_status")
	}
	if !strings.Contains(prompt, "hermes_get_envelope") {
		t.Error("prompt should mention hermes_get_envelope so executor reads the task first")
	}
}

func TestNewProcess_Claude(t *testing.T) {
	p, cfgPath, err := newProcess("claude", "env-test", "/usr/local/bin/hermes-mcp", "http://localhost:8080", "", "", false)
	if err != nil {
		t.Fatalf("newProcess: %v", err)
	}
	if cfgPath == "" {
		t.Fatal("expected mcp config path for claude")
	}
	defer os.Remove(cfgPath)
	if p == nil {
		t.Fatal("expected process")
	}
}

func TestNewProcess_Opencode(t *testing.T) {
	p, cfgPath, err := newProcess("opencode", "env-test", "", "", "", "", false)
	if err != nil {
		t.Fatalf("newProcess: %v", err)
	}
	if cfgPath != "" {
		t.Error("opencode should not have mcp config")
	}
	if p == nil {
		t.Fatal("expected process")
	}
}

// TestNewProcess_UnknownExecutor asserts executors we cannot run locally
// return ErrUnknownExecutor so Hermes can mark the delivery as permanently
// unrecoverable (422) rather than retry in a 30s backoff loop forever.
func TestNewProcess_UnknownExecutor(t *testing.T) {
	_, _, err := newProcess("kitt", "env-test", "", "", "", "", false)
	if err == nil {
		t.Fatal("expected error for unknown executor")
	}
	if !errors.Is(err, httpapi.ErrUnknownExecutor) {
		t.Errorf("expected ErrUnknownExecutor, got %v", err)
	}
}

// TestNewProcess_ClaudeResume asserts --resume is appended only when a
// native session id is supplied, so multi-turn Telegram loops continue
// the same conversation instead of starting fresh.
func TestNewProcess_ClaudeResume(t *testing.T) {
	p, cfgPath, err := newProcess("claude", "env-resume", "/usr/local/bin/hermes-mcp", "http://localhost:8080", "", "oc-sess-123", true)
	if err != nil {
		t.Fatalf("newProcess: %v", err)
	}
	defer os.Remove(cfgPath)
	args := strings.Join(p.Args(), " ")
	if !strings.Contains(args, "--resume oc-sess-123") {
		t.Errorf("expected --resume flag with session id, args: %s", args)
	}
}

// TestNewProcess_ClaudeFreshSessionID asserts --session-id is appended
// on a fresh spawn so Forge can pin the uuid and push it to Hermes.
// Regression lock for the multi-turn wiring.
func TestNewProcess_ClaudeFreshSessionID(t *testing.T) {
	uuid := "aaaaaaaa-bbbb-4ccc-9ddd-eeeeeeeeeeee"
	p, cfgPath, err := newProcess("claude", "env-fresh", "/usr/local/bin/hermes-mcp", "http://localhost:8080", "", uuid, false)
	if err != nil {
		t.Fatalf("newProcess: %v", err)
	}
	defer os.Remove(cfgPath)
	args := strings.Join(p.Args(), " ")
	if !strings.Contains(args, "--session-id "+uuid) {
		t.Errorf("expected --session-id flag on fresh spawn, args: %s", args)
	}
	if strings.Contains(args, "--resume") {
		t.Errorf("should not pass --resume on fresh spawn, args: %s", args)
	}
}

// TestNewProcess_OpencodeSession asserts --session is appended for
// opencode when continuing a session.
func TestNewProcess_OpencodeSession(t *testing.T) {
	p, _, err := newProcess("opencode", "env-opencode-resume", "", "", "", "oc-12345", true)
	if err != nil {
		t.Fatalf("newProcess: %v", err)
	}
	args := strings.Join(p.Args(), " ")
	if !strings.Contains(args, "--session oc-12345") {
		t.Errorf("expected --session flag for opencode, args: %s", args)
	}
}

// TestNewProcess_OpencodePromptIsPositional locks in that opencode's
// prompt is passed as the LAST positional argument, not via --prompt.
// Bug history: an earlier version passed --prompt which opencode does
// not recognise — it silently printed help and exited in ~1s, leaving
// every envelope stuck in delivered. The prompt must be the tail of
// the argv.
func TestNewProcess_OpencodePromptIsPositional(t *testing.T) {
	p, _, err := newProcess("opencode", "env-pos", "", "", "", "", false)
	if err != nil {
		t.Fatalf("newProcess: %v", err)
	}
	argv := p.Args()
	// Argv[0] is the program name, so the prompt should be the last
	// element and nothing is attached via a --prompt flag.
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--prompt") {
		t.Fatalf("opencode should not carry a --prompt flag; argv: %v", argv)
	}
	if len(argv) < 2 {
		t.Fatalf("expected at least 2 argv elements, got %v", argv)
	}
	last := argv[len(argv)-1]
	if !strings.Contains(last, "env-pos") {
		t.Errorf("expected prompt as final positional arg containing envelope id, got %q", last)
	}
}

func TestNewProcess_OpencodeBindsDiscoverableSession(t *testing.T) {
	p, _, err := newProcess("opencode", "env-opencode-bind", "", "", "", "", false)
	if err != nil {
		t.Fatalf("newProcess: %v", err)
	}
	args := strings.Join(p.Args(), " ")
	if !strings.Contains(args, "--title env-opencode-bind") {
		t.Errorf("expected envelope id as OpenCode title, args: %s", args)
	}
	if !strings.Contains(args, "--port 4096") {
		t.Errorf("expected fixed OpenCode API port for session discovery, args: %s", args)
	}
	if strings.Contains(args, "--session ") {
		t.Errorf("fresh OpenCode spawn should not pass --session, args: %s", args)
	}
}

func TestCleanupMCPConfig_EmptyPath(t *testing.T) {
	// Should not panic with empty path.
	cleanupMCPConfig(nil, "")
}

func TestCleanupDataDir_EmptyPath(t *testing.T) {
	// Should not panic with empty path.
	cleanupDataDir(nil, "")
}

// TestSeedOpencodeAuth_CopiesCredentials asserts the seeder copies the
// operator's auth.json into the isolated XDG tree so the spawned
// opencode can authenticate without sharing the shared data dir.
func TestSeedOpencodeAuth_CopiesCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcDir := home + "/.local/share/opencode"
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatalf("mk src: %v", err)
	}
	payload := []byte(`{"anthropic":{"type":"api","key":"fake-opencode-api-key"}}`)
	if err := os.WriteFile(srcDir+"/auth.json", payload, 0o600); err != nil {
		t.Fatalf("write src auth: %v", err)
	}

	dst := t.TempDir()
	if err := seedOpencodeAuth(dst); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := os.ReadFile(dst + "/opencode/auth.json")
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("auth contents mismatch")
	}
	// Auth file must keep restrictive perms so operator credentials
	// don't leak via readable tmp.
	info, err := os.Stat(dst + "/opencode/auth.json")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("auth.json perm = %v, want 0600", info.Mode().Perm())
	}
}

// TestSeedOpencodeAuth_SilentOnMissingSource locks the contract that
// a fresh operator box without auth.json doesn't error — opencode can
// still be spawned; it'll surface its own "no creds" error.
func TestSeedOpencodeAuth_SilentOnMissingSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dst := t.TempDir()
	if err := seedOpencodeAuth(dst); err != nil {
		t.Errorf("expected nil error on missing source, got %v", err)
	}
}

// TestBuildLauncher_OpencodeIsolatedDataDir asserts the real launcher
// hands opencode a unique XDG_DATA_HOME so its SQLite store is not
// shared with the operator's interactive TUI (which otherwise dead-
// locks new `opencode run` spawns on WAL contention).
func TestBuildLauncher_OpencodeIsolatedDataDir(t *testing.T) {
	prependFakeExecutorToPath(t, "opencode")

	l := buildLauncher(slog.New(slog.NewTextHandler(io.Discard, nil)), "http://hermes", "dev-key-key", "/bin/true")
	if l == nil {
		t.Fatal("expected real launcher")
	}
	p, err := l("opencode", "env-xdg-iso", "", "", false)
	if err != nil {
		t.Fatalf("launch opencode: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	env := p.Env()
	if env == nil {
		t.Fatal("opencode spawn should have explicit env override")
	}
	var xdg string
	for _, kv := range env {
		if strings.HasPrefix(kv, "XDG_DATA_HOME=") {
			xdg = strings.TrimPrefix(kv, "XDG_DATA_HOME=")
			break
		}
	}
	if xdg == "" {
		t.Fatal("XDG_DATA_HOME must be set for opencode")
	}
	if !strings.Contains(xdg, "forge-oc-env-xdg-iso") {
		t.Errorf("XDG_DATA_HOME %q should include envelope id", xdg)
	}
	if _, err := os.Stat(xdg); err != nil {
		t.Errorf("XDG_DATA_HOME dir should exist: %v", err)
	}
}

// TestBuildLauncher_ClaudeNoDataDirOverride asserts the claude branch
// does NOT set XDG_DATA_HOME — only opencode needs the isolation.
func TestBuildLauncher_ClaudeNoDataDirOverride(t *testing.T) {
	prependFakeExecutorToPath(t, "claude")

	l := buildLauncher(slog.New(slog.NewTextHandler(io.Discard, nil)), "http://hermes", "dev-key-key", "/bin/true")
	if l == nil {
		t.Fatal("expected real launcher")
	}
	p, err := l("claude", "env-claude-env", "", "", false)
	if err != nil {
		t.Fatalf("launch claude: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if p.Env() != nil {
		t.Error("claude should inherit parent env (nil override)")
	}
}

// TestAgentHost_Default asserts the host label falls back to
// "mac-forge" when AGENT_HOST is unset so the dashboard can group
// agents by host without any operator config.
func TestAgentHost_Default(t *testing.T) {
	t.Setenv("AGENT_HOST", "")
	if got := agentHost(); got != "mac-forge" {
		t.Errorf("agentHost default: got %q, want mac-forge", got)
	}
}

func TestAgentHost_Override(t *testing.T) {
	t.Setenv("AGENT_HOST", "custom-lab")
	if got := agentHost(); got != "custom-lab" {
		t.Errorf("agentHost override: got %q", got)
	}
}

// TestLoadProjectMap_ReturnsEmptyOnMissingURL locks the contract that
// a disabled reporter (no HERMES_URL) still returns a usable empty map
// rather than panicking.
func TestLoadProjectMap_ReturnsEmptyOnMissingURL(t *testing.T) {
	got := loadProjectMap(context.Background(), "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// TestLoadProjectMap_ParsesRegistry stands up a fake Hermes that
// returns the registry JSON shape and asserts the loader extracts
// working_dir → project-name pairs.
func TestLoadProjectMap_ParsesRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/registry/projects" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"project":"hermes","working_dir":"/workspace/hermes"},
			{"project":"kingdom","working_dir":"/workspace/kingdom"},
			{"project":"empty","working_dir":""}
		]`))
	}))
	t.Cleanup(srv.Close)

	got := loadProjectMap(context.Background(), srv.URL, "dev-key-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got["/workspace/hermes"] != "hermes" {
		t.Errorf("hermes mapping: %v", got)
	}
	if got["/workspace/kingdom"] != "kingdom" {
		t.Errorf("kingdom mapping: %v", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("empty working_dir should be skipped: %v", got)
	}
}

// TestBuildHermesAgentPrompt_NoHardcodedSecrets asserts the prompt does NOT
// contain any hardcoded IP addresses, keys, or credential literals.
// Regression lock for the control-plane hardening blocker.
func TestBuildHermesAgentPrompt_NoHardcodedSecrets(t *testing.T) {
	prompt := buildHermesAgentPrompt("env-sec-test")
	if strings.Contains(prompt, "127.0.0.1") {
		t.Error("prompt must not contain hardcoded IP address")
	}
	if strings.Contains(prompt, "hk_") {
		t.Error("prompt must not contain hardcoded Hermes key literal")
	}
	if !strings.Contains(prompt, "env-sec-test") {
		t.Error("prompt must contain envelope ID")
	}
	if !strings.Contains(prompt, "$HERMES_URL") {
		t.Error("prompt must reference $HERMES_URL env var")
	}
	if !strings.Contains(prompt, "$HERMES_KEY") {
		t.Error("prompt must reference $HERMES_KEY env var")
	}
}

// TestNewProcess_HermesAgent_KeyNotInArgv asserts that the Hermes key is
// NOT embedded in the process argv — it must be passed via environment only.
// Regression lock: previously hermesKey was interpolated directly into the
// prompt string which became an argv element, exposing it in `ps aux`.
func TestNewProcess_HermesAgent_KeyNotInArgv(t *testing.T) {
	t.Setenv("HERMES_AGENT_BIN", "hermes")
	p, cfgPath, err := newProcess("hermes-agent", "env-argv-test", "", "http://hermes:8081", "dev-key-secret-key", "", false)
	if err != nil {
		t.Fatalf("newProcess hermes-agent: %v", err)
	}
	if cfgPath != "" {
		t.Error("hermes-agent should not produce an mcp config path")
	}
	argv := strings.Join(p.Args(), " ")
	if strings.Contains(argv, "dev-key-secret-key") {
		t.Errorf("Hermes key must NOT appear in argv; got: %s", argv)
	}
	if strings.Contains(argv, "http://hermes:8081") {
		t.Errorf("Hermes URL must NOT appear in argv; got: %s", argv)
	}
	// Key and URL must be in the process env instead.
	var foundURL, foundKey bool
	for _, kv := range p.Env() {
		if kv == "HERMES_URL=http://hermes:8081" {
			foundURL = true
		}
		if kv == "HERMES_KEY=dev-key-secret-key" {
			foundKey = true
		}
	}
	if !foundURL {
		t.Error("HERMES_URL must be set in process env")
	}
	if !foundKey {
		t.Error("HERMES_KEY must be set in process env")
	}
}

// TestNewProcess_HermesAgent_NoMCPBinRequired asserts hermes-agent can be
// launched even when HERMES_MCP_BIN is empty. Previously buildLauncher
// gated on mcpBin != "" which silently fell back to stub for hermes-agent.
func TestNewProcess_HermesAgent_NoMCPBinRequired(t *testing.T) {
	t.Setenv("HERMES_AGENT_BIN", "hermes")
	p, _, err := newProcess("hermes-agent", "env-nomcp", "", "http://hermes:8081", "dev-key-key", "", false)
	if err != nil {
		t.Fatalf("hermes-agent must not require mcpBin, got: %v", err)
	}
	if p == nil {
		t.Fatal("expected process")
	}
}
