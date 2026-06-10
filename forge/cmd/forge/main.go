// Command forge is the Mac execution gateway.
//
// Spawns real executor processes on delivery via configurable launcher.
// Default: stub launcher (cat) for development/testing.
// Set HERMES_URL to enable real executors. HERMES_MCP_BIN is additionally
// required for MCP-based executors (claude). hermes-agent does not use MCP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/legrin-tech/forge/internal/agentreport"
	"github.com/legrin-tech/forge/internal/httpapi"
	"github.com/legrin-tech/forge/internal/runner"
	"github.com/legrin-tech/forge/internal/sessionstore"
)

// launchConfig holds the parameters for launching an executor process.
type launchConfig struct {
	logger            *slog.Logger
	executor          string
	envelopeID        string
	workingDir        string
	executorSessionID string
	resume            bool
	mcpBin            string
	hermesURL         string
	hermesKey         string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := ":8090"
	if v := os.Getenv("FORGE_ADDR"); v != "" {
		addr = v
	}
	dsn := "forge.db"
	if v := os.Getenv("FORGE_DB"); v != "" {
		dsn = v
	}

	store, err := sessionstore.Open(context.Background(), dsn)
	if err != nil {
		logger.Error("session store open failed", "err", err, "dsn", dsn)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	// Build launcher from env or use stub.
	hermesURL := os.Getenv("HERMES_URL")
	hermesKey := os.Getenv("HERMES_KEY")
	mcpBin := os.Getenv("HERMES_MCP_BIN")
	launcher := buildLauncher(logger, hermesURL, hermesKey, mcpBin)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: httpapi.NewServer(logger, store, launcher, hermesURL, hermesKey),
	}
	logger.Info("forge starting", "addr", addr, "dsn", dsn)

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server exited", "err", err)
			stop()
		}
	}()

	// Agent reporter: scans local AI processes every minute and POSTs
	// a snapshot to Hermes so the dashboard Constellation tab stays
	// live. Disabled when HERMES_URL is not set (stub/dev mode).
	if hermesURL != "" {
		rep := &agentreport.Reporter{
			Host:       agentHost(),
			HermesURL:  hermesURL,
			HermesKey:  hermesKey,
			ForgePID:   os.Getpid(),
			Interval:   60 * time.Second,
			Logger:     logger,
			ProjectMap: loadProjectMap(ctx, hermesURL, hermesKey, logger),
		}
		go rep.Run(ctx)
		logger.Info("agent reporter started", "host", rep.Host, "interval", rep.Interval)
	}

	<-ctx.Done()
	logger.Info("forge shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
	logger.Info("forge stopped")
}

// buildLauncher returns a Launcher configured from environment.
// If HERMES_URL is set, spawns real executors. HERMES_MCP_BIN is only
// required for MCP-based executors (claude). hermes-agent does not use
// MCP and must not be blocked by a missing HERMES_MCP_BIN.
func buildLauncher(logger *slog.Logger, hermesURL, hermesKey, mcpBin string) httpapi.Launcher {
	if hermesURL == "" {
		logger.Info("using stub launcher (set HERMES_URL for real executors)")
		return httpapi.StubLauncher()
	}
	logger.Info("real launcher configured", "hermes_url", hermesURL, "mcp_bin", mcpBin)

	return func(executor, envelopeID, workingDir, executorSessionID string, resume bool) (*runner.Process, error) {
		return launchExecutor(launchConfig{
			logger:            logger,
			executor:          executor,
			envelopeID:        envelopeID,
			workingDir:        workingDir,
			executorSessionID: executorSessionID,
			resume:            resume,
			mcpBin:            mcpBin,
			hermesURL:         hermesURL,
			hermesKey:         hermesKey,
		})
	}
}

// launchExecutor handles the actual process launch logic, extracted
// from buildLauncher to keep cognitive complexity within limits.
func launchExecutor(cfg launchConfig) (*runner.Process, error) {
	p, cfgPath, err := newProcess(cfg.executor, cfg.envelopeID, cfg.mcpBin, cfg.hermesURL, cfg.hermesKey, cfg.executorSessionID, cfg.resume)
	if err != nil {
		return nil, err
	}

	if cfg.workingDir != "" {
		p.SetDir(cfg.workingDir)
		cfg.logger.Info("executor working dir", "dir", cfg.workingDir, "executor", cfg.executor)
	}

	dataDir, err := prepareOpencodeDataDir(cfg.executor, cfg.envelopeID)
	if err != nil {
		return nil, err
	}

	if dataDir != "" {
		// Deduplicate: remove any existing XDG_DATA_HOME so ours is first
		env := []string{"XDG_DATA_HOME=" + dataDir}
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "XDG_DATA_HOME=") {
				env = append(env, v)
			}
		}
		p.SetEnv(env)
		cfg.logger.Info("opencode xdg data dir", "dir", dataDir, "envelope_id", cfg.envelopeID)
	}

	if err := p.Start(); err != nil {
		cleanupOnError(p, cfgPath, dataDir)
		return nil, fmt.Errorf("launch %s: %w", cfg.executor, err)
	}

	if cfg.executor == "opencode" {
		if err := p.CloseStdin(); err != nil {
			cfg.logger.Warn("close opencode stdin", "err", err, "envelope_id", cfg.envelopeID)
		}
	}

	cleanupMCPConfig(p, cfgPath)
	cleanupDataDir(p, dataDir)
	return p, nil
}

// prepareOpencodeDataDir creates an isolated XDG data directory for opencode
// executors to prevent WAL contention with the operator's interactive sessions.
// Returns empty string for non-opencode executors.
func prepareOpencodeDataDir(executor, envelopeID string) (string, error) {
	if executor != "opencode" {
		return "", nil
	}
	dataDir, err := os.MkdirTemp("", "forge-oc-"+envelopeID+"-*")
	if err != nil {
		return "", fmt.Errorf("mk xdg data dir: %w", err)
	}
	if err := seedOpencodeAuth(dataDir); err != nil {
		_ = os.RemoveAll(dataDir)
		return "", fmt.Errorf("seed opencode auth: %w", err)
	}
	return dataDir, nil
}

// cleanupOnError cleans up resources after a launch error.
func cleanupOnError(p *runner.Process, cfgPath, dataDir string) {
	if cfgPath != "" {
		_ = os.Remove(cfgPath)
	}
	if dataDir != "" {
		_ = os.RemoveAll(dataDir)
	}
	_ = p.Stop()
}

// newProcess creates a runner.Process for the given executor.
// Returns the process and an optional MCP config path to clean up.
// Fails fast with ErrUnknownExecutor for executors we cannot launch
// locally (e.g. "kitt" — runs in VPS OpenClaw, not on Mac).
//
// sessionID + resume together decide continuation semantics:
//   - resume=true, sessionID set  → claude --resume / opencode --session
//     (continuing a previously captured conversation)
//   - resume=false, sessionID set → claude --session-id (pinning a
//     fresh session to a known uuid so Forge can register it with
//     Hermes before the executor exits)
//   - sessionID empty             → no session flag (one-shot run)
func newProcess(executor, envelopeID, mcpBin, hermesURL, hermesKey, sessionID string, resume bool) (*runner.Process, string, error) {
	switch executor {
	case "claude":
		cfgPath, err := writeMCPConfig(mcpBin, hermesURL, hermesKey, envelopeID)
		if err != nil {
			return nil, "", fmt.Errorf("write mcp config: %w", err)
		}
		args := []string{
			"--dangerously-skip-permissions",
			"--mcp-config", cfgPath,
			"--strict-mcp-config",
			"--print",
		}
		switch {
		case resume && sessionID != "":
			args = append(args, "--resume", sessionID)
		case !resume && sessionID != "":
			args = append(args, "--session-id", sessionID)
		}
		args = append(args, "-p", buildExecutorPrompt(envelopeID))
		return runner.New("claude", args...), cfgPath, nil
	case "opencode":
		// opencode `run` is the non-interactive one-shot mode: spawn,
		// execute the prompt, then exit. It reuses ~/.config/opencode
		// for MCP servers (including `hermes` via run-hermes-mcp.sh),
		// so task tools are wired without a per-spawn tmp config.
		//
		// IMPORTANT: `opencode run` takes the prompt as a POSITIONAL
		// argument (array called `message`), not via `--prompt`. An
		// earlier version of this code passed `--prompt ...` and
		// opencode silently printed help and exited in ~1s, leaving
		// every envelope stuck in `delivered`. Positional arg must
		// come AFTER all flags.
		//
		// --format json streams events so the driver can later parse
		// sub-agent spawns for the dashboard tree. --title tags the
		// session with the envelope id for easy lookup in
		// `opencode session list`. For resume we pass --session; fresh
		// starts let opencode assign the session id (Forge discovers
		// it via its /session endpoint and pushes it back to Hermes).
		//
		// If the operator's global opencode config does not include
		// the hermes MCP server, the prompt still runs but the
		// executor will flag that it cannot update status.
		args := []string{"run",
			"--dangerously-skip-permissions",
			"--format", "json",
			"--port", "4096",
			"--title", envelopeID,
		}
		if resume && sessionID != "" {
			args = append(args, "--session", sessionID)
		}
		// Positional prompt — must be last so flag parsing doesn't
		// accidentally consume it as a value.
		args = append(args, buildExecutorPrompt(envelopeID))
		return runner.New("opencode", args...), "", nil
	case "hermes-agent":
		// Hermes Agent (Nous Research) as executor. One-shot via `-z`, yolo
		// mode so tools execute without confirmation prompts. Hermes Agent
		// has its own tool registry and does NOT understand the Claude/OpenCode
		// `hermes_*` MCP tools — it needs plain HTTP PATCH instructions via
		// curl or the hermes_status_update.sh wrapper.
		//
		// SECURITY: hermesURL and hermesKey are passed via environment variables
		// (HERMES_URL, HERMES_KEY) rather than embedded in argv/prompt text to
		// avoid secret exposure in process listings. The prompt instructs the
		// agent to read these env vars when constructing curl commands.
		hermesBin := os.Getenv("HERMES_AGENT_BIN")
		if hermesBin == "" {
			hermesBin = "hermes"
		}
		p := runner.New(hermesBin, "-z", buildHermesAgentPrompt(envelopeID), "--yolo")
		// Inject credentials via env so they don't appear in process argv.
		env := []string{
			"HERMES_URL=" + hermesURL,
			"HERMES_KEY=" + hermesKey,
		}
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "HERMES_URL=") && !strings.HasPrefix(v, "HERMES_KEY=") {
				env = append(env, v)
			}
		}
		p.SetEnv(env)
		return p, "", nil
	default:
		return nil, "", fmt.Errorf("%w: %q", httpapi.ErrUnknownExecutor, executor)
	}
}

// buildHermesAgentPrompt is the system prompt for the Hermes Agent (Nous Research)
// executor. Unlike Claude/OpenCode, Hermes Agent does NOT have the hermes_*
// MCP tools — it reports envelope status by calling curl directly via its
// terminal tool.
//
// SECURITY: hermesURL and hermesKey are NOT embedded in the prompt text.
// They are injected as HERMES_URL and HERMES_KEY environment variables by
// the caller so they never appear in process argv or logs.
func buildHermesAgentPrompt(envelopeID string) string {
	return fmt.Sprintf(`You were dispatched by Hermes transport system to complete envelope %s.

The Hermes API credentials are available as environment variables:
  HERMES_URL  — base URL of the Hermes API
  HERMES_KEY  — authentication key (X-Hermes-Key header)

STEP 1 — Read the envelope to understand the task:
    curl -sf -H "X-Hermes-Key: $HERMES_KEY" $HERMES_URL/envelopes/%s
  The response JSON contains task_goal, task_steps, success_criteria.

STEP 2 — Mark in_progress:
    curl -sf -X PATCH -H "X-Hermes-Key: $HERMES_KEY" -H 'Content-Type: application/json' \
      $HERMES_URL/envelopes/%s/status -d '{"status":"in_progress"}'

STEP 3 — Execute the task using your terminal/file/ssh tools.

STEP 4 — Report final status. Hermes transport will auto-notify the user.
  Success:
    curl -sf -X PATCH -H "X-Hermes-Key: $HERMES_KEY" -H 'Content-Type: application/json' \
      $HERMES_URL/envelopes/%s/status \
      -d '{"status":"done","note":"<1-sentence outcome>","proof":{"summary":"<what you did>"}}'
  Failure:
    curl -sf -X PATCH -H "X-Hermes-Key: $HERMES_KEY" -H 'Content-Type: application/json' \
      $HERMES_URL/envelopes/%s/status -d '{"status":"failed","note":"<reason>"}'
  Blocked (need user input):
    curl -sf -X PATCH -H "X-Hermes-Key: $HERMES_KEY" -H 'Content-Type: application/json' \
      $HERMES_URL/envelopes/%s/status -d '{"status":"blocked","note":"<what you need>"}'

Do NOT finish without a final PATCH — your work will look lost.
Do NOT use Telegram APIs directly — Hermes transport notifies the user for you.`,
		envelopeID,
		envelopeID,
		envelopeID,
		envelopeID,
		envelopeID,
		envelopeID,
	)
}

// buildExecutorPrompt is the shared system prompt handed to the executor
// (Claude or OpenCode) by Forge. The prompt pins the Hermes contract so
// the executor always reports status through hermes_* MCP tools rather
// than silently exiting.
func buildExecutorPrompt(envelopeID string) string {
	return fmt.Sprintf(`You were launched by Hermes transport system to complete a task.
Envelope ID: %s

CRITICAL RULES — you MUST follow these:
1. Call hermes_get_envelope(id="%s") FIRST to read the task goal and steps.
2. Call hermes_update_status(id="%s", status="in_progress") as soon as you start.
3. Use hermes_log_decision to record important choices (approach selection, rejected alternatives).
4. When DONE: call hermes_update_status(id="%s", status="done", proof={"summary":"what you did"}).
5. When BLOCKED or need input: call hermes_update_status(id="%s", status="blocked", note="what you need").
6. KITT and the user are notified automatically when you update status. This is your ONLY way to communicate back.
7. Do NOT finish without calling hermes_update_status — your work will appear lost.

Read the project files (CLAUDE.md, VISION.md, etc.) to understand context before starting.`,
		envelopeID, envelopeID, envelopeID, envelopeID, envelopeID)
}

// cleanupMCPConfig removes the temp MCP config file when the process exits.
func cleanupMCPConfig(p *runner.Process, cfgPath string) {
	if cfgPath == "" {
		return
	}
	go func() {
		<-p.Done()
		_ = os.Remove(cfgPath)
	}()
}

// cleanupDataDir recursively removes the per-spawn XDG data dir when
// the process exits. No-op if dir is empty.
func cleanupDataDir(p *runner.Process, dir string) {
	if dir == "" {
		return
	}
	go func() {
		<-p.Done()
		_ = os.RemoveAll(dir)
	}()
}

// seedOpencodeAuth copies the operator's opencode auth.json into the
// isolated XDG data dir so the spawned `opencode run` has the same
// model credentials without sharing the operator's SQLite store.
// Without this file, opencode silently blocks waiting for credentials
// and never progresses past migrations.
//
// The source path is ~/.local/share/opencode/auth.json by convention;
// if it doesn't exist (fresh operator box), we leave the dir empty and
// let opencode handle the "no creds" case itself.
func seedOpencodeAuth(xdgDataDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	src := home + "/.local/share/opencode/auth.json"
	raw, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	destDir := xdgDataDir + "/opencode"
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(destDir+"/auth.json", raw, 0o600)
}

// writeMCPConfig creates a temp JSON file with Hermes MCP server config
// suitable for `claude --mcp-config <path>`.
func writeMCPConfig(mcpBin, hermesURL, hermesKey, envelopeID string) (string, error) {
	env := map[string]string{
		"HERMES_URL":  hermesURL,
		"ENVELOPE_ID": envelopeID,
	}
	if hermesKey != "" {
		env["HERMES_KEY"] = hermesKey
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"hermes": map[string]any{
				"command": mcpBin,
				"env":     env,
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "hermes-mcp-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// agentHost returns the Host label this Forge reports as. Defaults
// to "mac-forge" so the dashboard can group by host without the
// operator having to configure anything; override with AGENT_HOST.
func agentHost() string {
	if v := os.Getenv("AGENT_HOST"); v != "" {
		return v
	}
	return "mac-forge"
}

// loadProjectMap queries Hermes /registry/projects once at startup
// and returns a working_dir → project-name lookup. Reporters use the
// longest-prefix match so processes spawned anywhere inside a
// registered project root get tagged correctly in the dashboard.
//
// If Hermes is unreachable we return an empty map — agents still get
// reported, just without a project label.
func loadProjectMap(ctx context.Context, hermesURL, hermesKey string, logger *slog.Logger) map[string]string {
	out := map[string]string{}
	if hermesURL == "" {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hermesURL+"/registry/projects", nil)
	if err != nil {
		logger.Warn("project map load: new request", "err", err)
		return out
	}
	if hermesKey != "" {
		req.Header.Set("X-Hermes-Key", hermesKey)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("project map load failed", "err", err)
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("project map load non-200", "status", resp.StatusCode)
		return out
	}
	body, _ := io.ReadAll(resp.Body)
	var projects []struct {
		Project    string `json:"project"`
		WorkingDir string `json:"working_dir"`
	}
	if err := json.Unmarshal(body, &projects); err != nil {
		logger.Warn("project map parse", "err", err)
		return out
	}
	for _, p := range projects {
		if p.Project != "" && p.WorkingDir != "" {
			out[p.WorkingDir] = p.Project
		}
	}
	logger.Info("agent reporter project map loaded", "count", len(out))
	return out
}
