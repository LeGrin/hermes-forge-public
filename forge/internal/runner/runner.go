// Package runner spawns and manages executor subprocesses.
//
// Worldview:
//   - W-F3: launch OpenCode / Claude sessions
//   - W-F4: read from sessions (stdout+stderr capture)
//   - W-F5: push messages into sessions (stdin write)
//   - W-F6: a real process handle exists before any ack is returned
package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

var (
	ErrNotRunning = errors.New("runner: process not running")
	ErrAlreadyRun = errors.New("runner: process already started")
)

// Process wraps an os/exec.Cmd with guarded stdin/stdout access.
type Process struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	outR  *os.File // read end of combined stdout+stderr pipe
	outW  *os.File // write end — closed after Start

	mu      sync.Mutex
	started bool
	done    chan struct{} // closed when process exits AND output is fully drained
	exitErr error

	// output collects all stdout+stderr bytes for polling reads.
	outMu  sync.Mutex
	outBuf bytes.Buffer
	drainW sync.WaitGroup // tracks the drain goroutine
}

// New creates a Process for the given command. Call Start to launch it.
func New(name string, args ...string) *Process {
	return &Process{
		cmd:  exec.Command(name, args...),
		done: make(chan struct{}),
	}
}

// SetDir sets the working directory for the process. Must be called
// before Start. If dir is empty, the parent's CWD is inherited.
func (p *Process) SetDir(dir string) {
	p.cmd.Dir = dir
}

// SetEnv overrides the environment passed to the subprocess. Must be
// called before Start. Pass a full "KEY=VALUE" slice; typically callers
// start from os.Environ() and append overrides so inherited config
// (PATH, HOME, credentials) is preserved.
func (p *Process) SetEnv(env []string) {
	p.cmd.Env = env
}

// Env returns the configured environment slice for the process, or
// nil if it was never overridden (parent env is inherited). Intended
// for diagnostics and tests — do not mutate.
func (p *Process) Env() []string {
	return p.cmd.Env
}

// Args returns the configured argv for the process (including program
// name at [0]). Intended for diagnostics and tests — do not mutate.
func (p *Process) Args() []string {
	return p.cmd.Args
}

// Start launches the subprocess. Returns ErrAlreadyRun on double-start.
func (p *Process) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return ErrAlreadyRun
	}

	if err := p.setupPipes(); err != nil {
		return err
	}

	if err := p.cmd.Start(); err != nil {
		p.cleanupPipesOnError()
		return fmt.Errorf("runner: start: %w", err)
	}

	_ = p.outW.Close()
	p.started = true
	p.startDrain()
	p.startWait()

	return nil
}

func (p *Process) setupPipes() error {
	var err error
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("runner: stdin pipe: %w", err)
	}

	p.outR, p.outW, err = os.Pipe()
	if err != nil {
		_ = p.stdin.Close()
		return fmt.Errorf("runner: output pipe: %w", err)
	}
	p.cmd.Stdout = p.outW
	p.cmd.Stderr = p.outW
	return nil
}

func (p *Process) cleanupPipesOnError() {
	_ = p.stdin.Close()
	_ = p.outR.Close()
	_ = p.outW.Close()
}

const maxOutBuf = 100 * 1024 * 1024 // 100 MiB

func (p *Process) startDrain() {
	p.drainW.Add(1)
	go func() {
		defer p.drainW.Done()
		buf := make([]byte, 4096)
		for {
			n, err := p.outR.Read(buf)
			if n > 0 {
				p.outMu.Lock()
				if p.outBuf.Len() < maxOutBuf {
					p.outBuf.Write(buf[:n])
				}
				p.outMu.Unlock()
			}
			if err != nil {
				break
			}
		}
	}()
}

func (p *Process) startWait() {
	go func() {
		p.exitErr = p.cmd.Wait()
		p.drainW.Wait()
		close(p.done)
	}()
}

// CloseStdin closes the process's stdin pipe. Some executors
// (notably `opencode run`) block indefinitely before bootstrap
// when stdin is an open pipe with no data — they wait for EOF
// even when the prompt was passed via argv. Callers that only
// push work via argv/flags should invoke this right after Start.
//
// Safe to call before Start (no-op); idempotent on repeat calls.
func (p *Process) CloseStdin() error {
	p.mu.Lock()
	started := p.started
	stdin := p.stdin
	p.mu.Unlock()
	if !started || stdin == nil {
		return nil
	}
	return stdin.Close()
}

// Write sends data to the process stdin. W-F5.
// Returns ErrNotRunning if the process hasn't started or has already exited.
func (p *Process) Write(data []byte) (int, error) {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()

	if !started {
		return 0, ErrNotRunning
	}

	// Check if process already exited (review feedback).
	select {
	case <-p.done:
		return 0, ErrNotRunning
	default:
	}

	return p.stdin.Write(data)
}

// ReadOutput returns all stdout+stderr captured so far and clears the buffer.
// W-F4.
func (p *Process) ReadOutput() []byte {
	p.outMu.Lock()
	defer p.outMu.Unlock()

	if p.outBuf.Len() == 0 {
		return nil
	}
	out := make([]byte, p.outBuf.Len())
	copy(out, p.outBuf.Bytes())
	p.outBuf.Reset()
	return out
}

// ReadOutputTail returns the last n lines from the output buffer without clearing it.
// Used by GET /sessions/{id}/output?tail=n for non-destructive log preview (CON-003).
func (p *Process) ReadOutputTail(n int) []byte {
	p.outMu.Lock()
	defer p.outMu.Unlock()

	if p.outBuf.Len() == 0 || n <= 0 {
		return nil
	}

	all := p.outBuf.Bytes()
	// Scan backwards to find the position after the nth newline from the end.
	// We break when lineCount > n so that start lands after the nth newline,
	// giving us n lines total (not n-1).
	lineCount := 0
	start := len(all)
	for i := len(all) - 1; i >= 0; i-- {
		if all[i] == '\n' {
			lineCount++
			if lineCount > n {
				start = i + 1
				break
			}
		}
	}

	out := make([]byte, len(all)-start)
	copy(out, all[start:])
	return out
}

// PID returns the OS process ID, or -1 if not started.
func (p *Process) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.started || p.cmd.Process == nil {
		return -1
	}
	return p.cmd.Process.Pid
}

// Running reports whether the process is still alive.
func (p *Process) Running() bool {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()

	select {
	case <-p.done:
		return false
	default:
		return started
	}
}

// Stop closes stdin and waits for the process to exit.
// If the process does not exit within 3 seconds, it is killed (review feedback).
func (p *Process) Stop() error {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()

	if !started {
		return ErrNotRunning
	}

	// Close stdin so the child sees EOF.
	_ = p.stdin.Close()

	// Wait for exit with a timeout fallback (review feedback).
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-p.done
	}
	return p.exitErr
}

// Done returns a channel that closes when the process exits and all
// output has been drained.
func (p *Process) Done() <-chan struct{} {
	return p.done
}
