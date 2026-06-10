package httpapi

import (
	"log/slog"
	"sync"

	"github.com/legrin-tech/forge/internal/runner"
)

// SpawnGate serializes spawn decisions across all handlers (deliver + resume)
// on a per-envelope basis. This prevents the race where concurrent /deliver
// and /sessions/{id}/resume for the same dead envelope both spawn a process.
type SpawnGate struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newSpawnGate() *SpawnGate {
	return &SpawnGate{locks: make(map[string]*sync.Mutex)}
}

// Lock acquires the per-envelope spawn lock. Caller must call Unlock.
func (g *SpawnGate) Lock(envelopeID string) {
	g.mu.Lock()
	m, ok := g.locks[envelopeID]
	if !ok {
		m = &sync.Mutex{}
		g.locks[envelopeID] = m
	}
	g.mu.Unlock()
	m.Lock()
}

// Unlock releases the per-envelope spawn lock.
func (g *SpawnGate) Unlock(envelopeID string) {
	g.mu.Lock()
	m := g.locks[envelopeID]
	g.mu.Unlock()
	if m != nil {
		m.Unlock()
	}
}

// Launcher spawns an executor process.
//
// Arguments:
//   - executor: which binary to launch (e.g. "opencode", "claude").
//   - envelopeID: the envelope being delivered.
//   - workingDir: directory to run in (resolved from the Hermes project
//     registry), empty to inherit the parent's CWD.
//   - sessionID: the executor's native session id. When resume=true the
//     launcher should continue that session (Claude --resume / OpenCode
//     --session); when resume=false and sessionID is non-empty, the
//     launcher creates a new session with that fixed id (Claude
//     --session-id). Empty string means "do not pass a session flag".
//   - resume: true for continuation, false for fresh.
//
// Implementations must return a started process — Start() already called.
type Launcher func(executor, envelopeID, workingDir, sessionID string, resume bool) (*runner.Process, error)

// ProcessRegistry tracks running executor processes by session_id.
// Shared between deliverHandler (writes) and sessionHandler (reads).
// Extracted from deliverHandler per review feedback on PR #8.
type ProcessRegistry struct {
	logger    *slog.Logger
	SpawnGate *SpawnGate // shared spawn serializer for deliver + resume

	mu        sync.Mutex
	processes map[string]*runner.Process
}

func newProcessRegistry(logger *slog.Logger) *ProcessRegistry {
	return &ProcessRegistry{
		logger:    logger,
		SpawnGate: newSpawnGate(),
		processes: make(map[string]*runner.Process),
	}
}

// Register stores a process and starts a cleanup goroutine that removes
// it when the process exits.
func (r *ProcessRegistry) Register(sessionID string, proc *runner.Process) {
	r.mu.Lock()
	r.processes[sessionID] = proc
	r.mu.Unlock()

	go func() {
		<-proc.Done()
		r.mu.Lock()
		defer r.mu.Unlock()
		// Only delete if this is still the same process — a re-register
		// with the same sessionID should not be clobbered (review feedback).
		if r.processes[sessionID] == proc {
			delete(r.processes, sessionID)
			r.logger.Info("process exited, removed from registry", "session_id", sessionID)
		}
	}()
}

// Get returns the running process for a session, or nil.
func (r *ProcessRegistry) Get(sessionID string) *runner.Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.processes[sessionID]
}

// Running reports whether a process exists and is still alive.
func (r *ProcessRegistry) Running(sessionID string) bool {
	r.mu.Lock()
	proc, ok := r.processes[sessionID]
	r.mu.Unlock()
	return ok && proc.Running()
}
