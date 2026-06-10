// Package agentreport scans the local host for AI-executor processes
// (Claude, OpenCode, and Forge itself) and POSTs a compact snapshot
// to Hermes every minute so the Constellation dashboard has live data.
//
// The scanner is intentionally best-effort: it parses `ps`, walks
// parent chains to classify where a process came from (spawned by
// Forge vs. by a terminal vs. orphan adopted by init), reads CWD via
// lsof, and tags the Hermes project by matching CWD against the
// known project roots list. None of this is authoritative — it is
// telemetry for the operator dashboard.
package agentreport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ParentIDForSession returns the parent agent ID for a given child session
// ID, set by forge.go from the httpapi agent link store. This lets the
// reporter augment agent snapshots with parent_id discovered via
// OpenCode's session.created event without a circular import.
var ParentIDForSession func(sessionID string) string

// Agent mirrors agentstore.Agent on the Hermes side. Defined locally
// so Forge does not import the hermes module — keeps the dependency
// arrow single-direction (Hermes ← Forge, never the other way).
type Agent struct {
	ID         string    `json:"id"`
	Host       string    `json:"host"`
	Executor   string    `json:"executor"`
	PID        int       `json:"pid"`
	CWD        string    `json:"cwd,omitempty"`
	Project    string    `json:"project,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Title      string    `json:"title,omitempty"`
	State      string    `json:"state"`
	ParentKind string    `json:"parent_kind,omitempty"`
	ParentID   string    `json:"parent_id,omitempty"`
	CPUPercent float64   `json:"cpu_percent,omitempty"`
	MemPercent float64   `json:"mem_percent,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at,omitempty"`
	ReportedBy string    `json:"reported_by,omitempty"`
}

// Snapshot is the POST body Forge sends to Hermes.
type Snapshot struct {
	Host    string    `json:"host"`
	TakenAt time.Time `json:"taken_at"`
	Agents  []Agent   `json:"agents"`
}

// Reporter runs the background scan loop and pushes snapshots to
// Hermes.
type Reporter struct {
	Host       string        // "mac-forge"
	HermesURL  string        // http://127.0.0.1:8081
	HermesKey  string        // X-Hermes-Key
	ForgePID   int           // used to classify Forge-spawned children
	Interval   time.Duration // default 60s
	Logger     *slog.Logger
	HTTP       *http.Client
	ProjectMap map[string]string // cwd-prefix → project name (from Hermes registry)

	// projectMapMu guards hot-reload of the project map — not critical
	// for correctness but keeps Data Race detector quiet.
	projectMapMu sync.RWMutex

	// PSStub is a test-only seam: when non-empty, runPS parses this
	// string instead of forking `ps`. Production wiring never sets it.
	PSStub string
}

// Run blocks until ctx is done. Fires one snapshot immediately, then
// every Interval. Errors are logged; the loop never panics.
func (r *Reporter) Run(ctx context.Context) {
	if r.Interval == 0 {
		r.Interval = 60 * time.Second
	}
	if r.HTTP == nil {
		r.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	tick := time.NewTicker(r.Interval)
	defer tick.Stop()

	r.fire(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.fire(ctx)
		}
	}
}

func (r *Reporter) fire(ctx context.Context) {
	agents := r.Scan(ctx)
	if len(agents) == 0 {
		return
	}
	snap := Snapshot{
		Host:    r.Host,
		TakenAt: time.Now().UTC(),
		Agents:  agents,
	}
	if err := r.post(ctx, snap); err != nil {
		r.Logger.Warn("agent snapshot post failed", "err", err)
	}
}

func (r *Reporter) post(ctx context.Context, snap Snapshot) error {
	body, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.HermesURL+"/agents/snapshot", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.HermesKey != "" {
		req.Header.Set("X-Hermes-Key", r.HermesKey)
	}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hermes /agents/snapshot returned %d", resp.StatusCode)
	}
	return nil
}

// psRow is the minimal set of ps columns we need. Exported for tests.
type psRow struct {
	PID     int
	PPID    int
	CPU     float64
	Mem     float64
	Started time.Time
	Command string // full command line
}

// Scan walks `ps` on the local host, classifies each AI process, and
// returns an Agent slice ready to POST. Never returns nil — returns
// empty slice if nothing matched.
func (r *Reporter) Scan(ctx context.Context) []Agent {
	rows, err := r.runPS(ctx)
	if err != nil {
		r.Logger.Warn("ps scan failed", "err", err)
		return []Agent{}
	}

	byPID := indexByPID(rows)
	agents, sessionToAgent := r.buildAgentsFromRows(ctx, rows, byPID)
	resolveParentAgents(agents, sessionToAgent)
	return agents
}

// indexByPID builds a PID→psRow lookup map.
func indexByPID(rows []psRow) map[int]psRow {
	m := make(map[int]psRow, len(rows))
	for _, row := range rows {
		m[row.PID] = row
	}
	return m
}

// buildAgentsFromRows is the first pass: classifies AI processes,
// builds Agent records, and returns them together with a session→agentID
// map for parent resolution.
func (r *Reporter) buildAgentsFromRows(ctx context.Context, rows []psRow, byPID map[int]psRow) ([]Agent, map[string]string) {
	agents := make([]Agent, 0, 8)
	sessionToAgent := make(map[string]string, 8)

	for _, row := range rows {
		a := r.maybeBuildAgent(ctx, row, byPID, sessionToAgent)
		if a != nil {
			agents = append(agents, *a)
		}
	}
	return agents, sessionToAgent
}

// maybeBuildAgent returns an Agent for row if it is a top-level AI
// process, or nil if it should be filtered (child language-server).
// When returning non-nil, also populates sessionToAgent for sessions
// discovered in this row.
func (r *Reporter) maybeBuildAgent(ctx context.Context, row psRow, byPID map[int]psRow, sessionToAgent map[string]string) *Agent {
	exec := classifyExecutor(row.Command)
	if exec == "" {
		return nil
	}
	if isLanguageServerChild(row, byPID) {
		return nil
	}
	sessionID := parseSessionID(exec, row.Command)
	id := buildAgentID(r.Host, exec, sessionID, row.PID)
	a := Agent{
		ID:         id,
		Host:       r.Host,
		Executor:   exec,
		PID:        row.PID,
		CPUPercent: row.CPU,
		MemPercent: row.Mem,
		StartedAt:  row.Started,
		State:      deriveState(exec, row),
		ParentKind: r.classifyParent(row.PPID, byPID),
		SessionID:  sessionID,
	}
	a.CWD = r.readCWD(ctx, row.PID)
	a.Project = r.matchProject(a.CWD)
	a.Title = deriveTitle(exec, row.Command, sessionID)
	if sessionID != "" {
		sessionToAgent[sessionID] = id
	}
	return &a
}

// isLanguageServerChild returns true if row's parent is a claude or
// opencode process — those are language-server children we skip.
func isLanguageServerChild(row psRow, byPID map[int]psRow) bool {
	par, ok := byPID[row.PPID]
	if !ok {
		return false
	}
	p := classifyExecutor(par.Command)
	return p == "claude" || p == "opencode"
}

// buildAgentID constructs the canonical agent ID from its components.
// Uses sessionID when available, otherwise falls back to PID.
func buildAgentID(host, exec, sessionID string, pid int) string {
	if sessionID != "" {
		return fmt.Sprintf("%s:%s:%s", host, exec, sessionID)
	}
	return fmt.Sprintf("%s:%s:%d", host, exec, pid)
}

// resolveParentAgents is the second pass: resolves parentSessionID →
// parentAgentID using the link store and sessionToAgent map.
func resolveParentAgents(agents []Agent, sessionToAgent map[string]string) {
	if ParentIDForSession == nil {
		return
	}
	for i := range agents {
		if agents[i].SessionID == "" {
			continue
		}
		parentSessionID := ParentIDForSession(agents[i].SessionID)
		if parentSessionID == "" {
			continue
		}
		if parentAgentID, ok := sessionToAgent[parentSessionID]; ok {
			agents[i].ParentID = parentAgentID
		}
	}
}

// runPS is a thin wrapper around `ps` that parses cpu, mem, start
// time, and the full command line. macOS/Linux compatible —
// intentionally omits etime/etimes because BSD `ps` silently drops
// unknown keywords and shifts the remaining columns, breaking parse.
// Uptime is derived from StartedAt instead.
// When r.PSStub is set the stub output is used instead of forking
// `ps` — gives tests a deterministic seam without shelling out.
func (r *Reporter) runPS(ctx context.Context) ([]psRow, error) {
	var raw []byte
	if r.PSStub != "" {
		raw = []byte(r.PSStub)
	} else {
		cmd := exec.CommandContext(ctx, "ps", "-eo", "pid,ppid,%cpu,%mem,lstart,command")
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		raw = out
	}
	lines := strings.Split(string(raw), "\n")
	rows := make([]psRow, 0, len(lines))
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		row, ok := parsePSLine(line)
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parsePSLine(line string) (psRow, bool) {
	// Columns: pid ppid %cpu %mem lstart(5 tokens) command(rest)
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return psRow{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return psRow{}, false
	}
	ppid, _ := strconv.Atoi(fields[1])
	cpu, _ := strconv.ParseFloat(fields[2], 64)
	mem, _ := strconv.ParseFloat(fields[3], 64)
	// lstart: `Thu Apr 17 01:55:41 2026` — 5 fields
	lstart := strings.Join(fields[4:9], " ")
	started, _ := time.Parse("Mon Jan _2 15:04:05 2006", lstart)
	command := strings.Join(fields[9:], " ")
	return psRow{
		PID:     pid,
		PPID:    ppid,
		CPU:     cpu,
		Mem:     mem,
		Started: started.UTC(),
		Command: command,
	}, true
}

// classifyExecutor maps a command line to the canonical executor name
// or "" if it is not an AI process we care about. Handles both
// /path/to/claude and `claude` in PATH, both `opencode` and
// `opencode-darwin-arm64`, the node-wrapped forms, and Forge itself.
func classifyExecutor(cmd string) string {
	// The wrapper node process runs `node …/opencode serve` etc. — detect
	// that pattern before the plain binary match so we catch the
	// operator's top-level opencode (user-tty) reliably.
	if strings.Contains(cmd, "/opencode") || strings.Contains(cmd, "opencode-darwin-arm64") || strings.HasPrefix(cmd, "opencode") {
		return "opencode"
	}
	// `claude` on its own line, or `claude-hook …`, or `/path/to/claude`.
	bin := firstToken(cmd)
	base := filepath.Base(bin)
	switch base {
	case "claude":
		return "claude"
	case "forge":
		return "forge"
	}
	return ""
}

func firstToken(s string) string {
	for i, r := range s {
		if r == ' ' || r == '\t' {
			return s[:i]
		}
	}
	return s
}

// classifyParent walks up to 6 levels of the parent chain and
// returns the first recognised ancestor kind. Unknown or purely-shell
// ancestors fall through to the default "user-tty".
func (r *Reporter) classifyParent(ppid int, byPID map[int]psRow) string {
	pid := ppid
	for i := 0; i < 6 && pid > 1; i++ {
		row, ok := byPID[pid]
		if !ok {
			break
		}
		if kind := r.parentKindOf(row); kind != "" {
			return kind
		}
		pid = row.PPID
	}
	if ppid == 1 {
		return "init"
	}
	return "user-tty"
}

// parentKindOf inspects a single psRow and returns a kind label if it
// matches a known ancestor signature, or empty string to keep walking.
// Separated out so classifyParent stays a simple linear loop.
func (r *Reporter) parentKindOf(row psRow) string {
	if r.ForgePID > 0 && row.PID == r.ForgePID {
		return "forge"
	}
	if strings.Contains(row.Command, "osm-daemon.py") {
		return "osm-daemon"
	}
	if strings.Contains(row.Command, "/cmux.app/") || strings.Contains(row.Command, "/cmux ") {
		return "cmux"
	}
	return ""
}

// deriveState returns "active"/"idle"/"exited" based on coarse signals.
func deriveState(_ string, row psRow) string {
	if row.CPU >= 1.0 {
		return "active"
	}
	return "idle"
}

// readCWD shells out to `lsof -a -p <pid> -d cwd`. Returns empty
// string on any error (process gone, no permission, etc.).
func (r *Reporter) readCWD(ctx context.Context, pid int) string {
	cmd := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// lsof -Fn prints lines starting with 'n' for the name field.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

// matchProject finds the longest prefix of cwd that equals a known
// project root. Reporter uses the map passed in at construction; the
// caller may refresh it periodically from Hermes /registry/projects.
func (r *Reporter) matchProject(cwd string) string {
	if cwd == "" {
		return ""
	}
	r.projectMapMu.RLock()
	defer r.projectMapMu.RUnlock()
	best, bestName := 0, ""
	for prefix, name := range r.ProjectMap {
		if prefix == "" || !strings.HasPrefix(cwd, prefix) {
			continue
		}
		if len(prefix) > best {
			best = len(prefix)
			bestName = name
		}
	}
	return bestName
}

// SetProjectMap replaces the cwd → project-name lookup atomically.
func (r *Reporter) SetProjectMap(m map[string]string) {
	r.projectMapMu.Lock()
	defer r.projectMapMu.Unlock()
	r.ProjectMap = m
}

// parseSessionID tries to recover the executor's native session id
// from its command line. Forge-spawned Claude carries it via
// --session-id or --resume; OpenCode's -s flag carries `ses_…`.
func parseSessionID(exec, cmd string) string {
	switch exec {
	case "claude":
		for _, flag := range []string{"--session-id", "--resume"} {
			if id := extractFlagValue(cmd, flag); id != "" {
				return id
			}
		}
	case "opencode":
		for _, flag := range []string{"--session", "-s"} {
			if id := extractFlagValue(cmd, flag); id != "" {
				return id
			}
		}
	}
	return ""
}

// extractFlagValue returns the next whitespace-separated token after
// the given flag. Supports both `--flag value` and `--flag=value`.
func extractFlagValue(cmd, flag string) string {
	// `--flag=value` form
	if i := strings.Index(cmd, flag+"="); i >= 0 {
		rest := cmd[i+len(flag)+1:]
		return firstToken(rest)
	}
	// `--flag value` form
	i := strings.Index(cmd, flag+" ")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(cmd[i+len(flag):])
	return firstToken(rest)
}

// deriveTitle picks a short human label:
//   - For Forge-spawned executors we store the envelope id in --title
//     (opencode) or encode it in the prompt (claude). Use session id
//     if we have it; otherwise fall back to a trimmed command tail.
func deriveTitle(_, cmd, sid string) string {
	if id := extractFlagValue(cmd, "--title"); id != "" {
		return id
	}
	if sid != "" {
		return sid[:minInt(len(sid), 20)]
	}
	// Trim down long command lines to the tail after the binary path.
	trimmed := strings.TrimSpace(cmd)
	const maxLen = 80
	if len(trimmed) > maxLen {
		return trimmed[:maxLen] + "…"
	}
	return trimmed
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
