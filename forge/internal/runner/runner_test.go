package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/legrin-tech/forge/internal/runner"
)

// TestStart_Cat launches cat, writes, reads, and stops.
// Composite proof for W-F3 (launch), W-F4 (read), W-F5 (write).
func TestStart_Cat(t *testing.T) {
	p := runner.New("cat")
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	if pid := p.PID(); pid <= 0 {
		t.Fatalf("expected positive PID, got %d", pid)
	}
	if !p.Running() {
		t.Fatal("expected process to be running")
	}

	// W-F5: write to stdin.
	msg := []byte("hello from forge\n")
	n, err := p.Write(msg)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("expected %d bytes written, got %d", len(msg), n)
	}

	// Give cat time to echo back.
	time.Sleep(50 * time.Millisecond)

	// W-F4: read from stdout.
	out := p.ReadOutput()
	if string(out) != "hello from forge\n" {
		t.Fatalf("expected echoed output, got %q", string(out))
	}

	// ReadOutput clears the buffer.
	out2 := p.ReadOutput()
	if out2 != nil {
		t.Fatalf("expected nil after drain, got %q", string(out2))
	}

	// Stop closes stdin → cat exits.
	if err := p.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if p.Running() {
		t.Fatal("expected process to be stopped")
	}
}

// TestDoubleStart returns ErrAlreadyRun.
func TestDoubleStart(t *testing.T) {
	p := runner.New("cat")
	if err := p.Start(); err != nil {
		t.Fatalf("first start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	if err := p.Start(); err != runner.ErrAlreadyRun {
		t.Fatalf("expected ErrAlreadyRun, got %v", err)
	}
}

// TestWriteBeforeStart returns ErrNotRunning.
func TestWriteBeforeStart(t *testing.T) {
	p := runner.New("cat")
	_, err := p.Write([]byte("nope"))
	if err != runner.ErrNotRunning {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}

// TestStopBeforeStart returns ErrNotRunning.
func TestStopBeforeStart(t *testing.T) {
	p := runner.New("cat")
	if err := p.Stop(); err != runner.ErrNotRunning {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}

// TestProcessExitNaturally — process exits when it finishes; Done channel closes.
func TestProcessExitNaturally(t *testing.T) {
	p := runner.New("echo", "done")
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit within 2s")
	}

	if p.Running() {
		t.Fatal("expected process to have exited")
	}

	// Done() now guarantees drain is complete — no sleep needed (review fix).
	out := p.ReadOutput()
	if string(out) != "done\n" {
		t.Fatalf("expected 'done\\n', got %q", string(out))
	}
}

// TestWriteAfterExit returns ErrNotRunning (review feedback: check done channel).
func TestWriteAfterExit(t *testing.T) {
	p := runner.New("echo", "bye")
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	<-p.Done()

	_, err := p.Write([]byte("should fail"))
	if err != runner.ErrNotRunning {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}

// TestStderr_Captured verifies stderr is merged into output (review feedback).
func TestStderr_Captured(t *testing.T) {
	// "sh -c" writes to stderr via >&2.
	p := runner.New("sh", "-c", "echo err_output >&2")
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	<-p.Done()

	out := p.ReadOutput()
	if string(out) != "err_output\n" {
		t.Fatalf("expected stderr captured, got %q", string(out))
	}
}

// TestPID_BeforeStart returns -1.
func TestPID_BeforeStart(t *testing.T) {
	p := runner.New("cat")
	if pid := p.PID(); pid != -1 {
		t.Fatalf("expected -1, got %d", pid)
	}
}

// TestSetDir sets working directory before start.
func TestSetDir(t *testing.T) {
	tmp := t.TempDir()
	p := runner.New("pwd")
	p.SetDir(tmp)
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	<-p.Done()
	out := strings.TrimSpace(string(p.ReadOutput()))
	if out == "" {
		t.Fatal("expected output from pwd")
	}
	if out != tmp {
		t.Fatalf("expected dir %q, got %q", tmp, out)
	}
}

// TestCloseStdin — explicit stdin close makes cat see EOF and exit
// without a Stop() call. Mirrors the `opencode run` EOF-before-bootstrap
// requirement that motivated CloseStdin.
func TestCloseStdin(t *testing.T) {
	p := runner.New("cat")
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := p.CloseStdin(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}

	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit within 2s after stdin close")
	}

	if p.Running() {
		t.Fatal("expected process to have exited")
	}
}

// TestCloseStdinBeforeStart — no-op before Start, no panic.
func TestCloseStdinBeforeStart(t *testing.T) {
	p := runner.New("cat")
	if err := p.CloseStdin(); err != nil {
		t.Fatalf("expected nil before start, got %v", err)
	}
}

// TestReadOutputTail_NonDestructive verifies ReadOutputTail returns last N lines
// without clearing the buffer (CON-003).
func TestReadOutputTail_NonDestructive(t *testing.T) {
	// Use sh -c to emit all lines at once so the buffer is deterministic.
	p := runner.New("sh", "-c", `printf 'AAAA\nBBBB\nCCCC\nDDDD\nEEEE\nFFFF\nGGGG\n'`)
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	<-p.Done() // wait for sh to finish and drain

	got := string(p.ReadOutputTail(5))
	// Must contain last 5 lines.
	for _, want := range []string{"CCCC", "DDDD", "EEEE", "FFFF", "GGGG"} {
		if !strings.Contains(got, want) {
			t.Errorf("tail=5: missing %q in output %q", want, got)
		}
	}
	// Must NOT contain earlier lines.
	for _, unwanted := range []string{"AAAA", "BBBB"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("tail=5: should not contain %q", unwanted)
		}
	}
}

// TestReadOutputTail_EmptyBuffer returns nil when buffer is empty (CON-003).
func TestReadOutputTail_EmptyBuffer(t *testing.T) {
	p := runner.New("cat")
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = p.ReadOutput() // drain empty buffer

	got := p.ReadOutputTail(5)
	if got != nil {
		t.Errorf("expected nil for empty buffer, got %q", string(got))
	}
	_ = p.Stop()
}

// TestReadOutputTail_ZeroAndNegative treat n<=0 as "return nil" (CON-003).
func TestReadOutputTail_ZeroAndNegative(t *testing.T) {
	p := runner.New("cat")
	if err := p.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	p.Write([]byte("hello\n"))
	time.Sleep(50 * time.Millisecond)

	if got := p.ReadOutputTail(0); got != nil {
		t.Errorf("tail=0: expected nil, got %q", string(got))
	}
	if got := p.ReadOutputTail(-3); got != nil {
		t.Errorf("tail=-3: expected nil, got %q", string(got))
	}

	_ = p.Stop()
}
