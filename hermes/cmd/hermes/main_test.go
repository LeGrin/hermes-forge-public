package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/legrin-tech/hermes/internal/agentstore"
)

func TestParseAgentConfig_Defaults(t *testing.T) {
	getenv := func(string) string { return "" }
	var warns []string
	warn := func(msg string, args ...any) {
		warns = append(warns, msg)
	}

	cfg := ParseAgentConfig(getenv, warn)

	if cfg.TTL != 15*time.Minute {
		t.Errorf("default TTL: got %v, want 15m", cfg.TTL)
	}
	if cfg.Interval != time.Minute {
		t.Errorf("default interval: got %v, want 1m", cfg.Interval)
	}
	if len(warns) != 0 {
		t.Errorf("no warns expected, got %d", len(warns))
	}
}

func TestParseAgentConfig_ValidTTL(t *testing.T) {
	getenv := func(k string) string {
		if k == "HERMES_AGENT_TTL" {
			return "30m"
		}
		return ""
	}
	var warns []string
	warn := func(msg string, args ...any) {
		warns = append(warns, msg)
	}

	cfg := ParseAgentConfig(getenv, warn)

	if cfg.TTL != 30*time.Minute {
		t.Errorf("TTL: got %v, want 30m", cfg.TTL)
	}
	if len(warns) != 0 {
		t.Errorf("no warns expected, got %d", len(warns))
	}
}

func TestParseAgentConfig_ValidInterval(t *testing.T) {
	getenv := func(k string) string {
		if k == "HERMES_AGENT_PRUNE_INTERVAL" {
			return "2m"
		}
		return ""
	}
	var warns []string
	warn := func(msg string, args ...any) {
		warns = append(warns, msg)
	}

	cfg := ParseAgentConfig(getenv, warn)

	if cfg.Interval != 2*time.Minute {
		t.Errorf("Interval: got %v, want 2m", cfg.Interval)
	}
	if len(warns) != 0 {
		t.Errorf("no warns expected, got %d", len(warns))
	}
}

func TestParseAgentConfig_InvalidTTL_WarnsAndDefaults(t *testing.T) {
	getenv := func(k string) string {
		if k == "HERMES_AGENT_TTL" {
			return "not-a-duration"
		}
		return ""
	}
	var warns []string
	warn := func(msg string, args ...any) {
		warns = append(warns, msg)
	}

	cfg := ParseAgentConfig(getenv, warn)

	if cfg.TTL != 15*time.Minute {
		t.Errorf("TTL should fall back to default: got %v", cfg.TTL)
	}
	if len(warns) != 1 || warns[0] != "invalid HERMES_AGENT_TTL, using default" {
		t.Errorf("expected warn about invalid TTL, got %v", warns)
	}
}

func TestParseAgentConfig_InvalidInterval_WarnsAndDefaults(t *testing.T) {
	getenv := func(k string) string {
		if k == "HERMES_AGENT_PRUNE_INTERVAL" {
			return "bad"
		}
		return ""
	}
	var warns []string
	warn := func(msg string, args ...any) {
		warns = append(warns, msg)
	}

	cfg := ParseAgentConfig(getenv, warn)

	if cfg.Interval != time.Minute {
		t.Errorf("Interval should fall back to default: got %v", cfg.Interval)
	}
	if len(warns) != 1 || warns[0] != "invalid HERMES_AGENT_PRUNE_INTERVAL, using default" {
		t.Errorf("expected warn about invalid interval, got %v", warns)
	}
}

func TestParseAgentConfig_ZeroTTL_UsesDefault(t *testing.T) {
	getenv := func(k string) string {
		if k == "HERMES_AGENT_TTL" {
			return "0s"
		}
		return ""
	}
	var warns []string
	warn := func(msg string, args ...any) {
		warns = append(warns, msg)
	}

	cfg := ParseAgentConfig(getenv, warn)

	if cfg.TTL != 15*time.Minute {
		t.Errorf("zero TTL should use default: got %v", cfg.TTL)
	}
	if len(warns) != 1 {
		t.Errorf("expected warn for zero TTL, got %d", len(warns))
	}
}

func TestStartPruneTicker_ZeroInterval_DoesNotStart(t *testing.T) {
	store := agentstore.New(time.Hour)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Should not panic and should return immediately
	StartPruneTicker(store, 0, logger, ctx)
}

func TestStartPruneTicker_NegativeInterval_DoesNotStart(t *testing.T) {
	store := agentstore.New(time.Hour)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should not panic
	StartPruneTicker(store, -1, logger, ctx)
}

func TestStartPruneTicker_CtxCancel_StopsCleanly(t *testing.T) {
	store := agentstore.New(50 * time.Millisecond)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		StartPruneTicker(store, 10*time.Millisecond, logger, ctx)
	}()

	// Wait a bit for ticker to start
	time.Sleep(50 * time.Millisecond)

	// Cancel context - ticker should stop
	cancel()

	// If no panic within 100ms, ticker stopped cleanly
	wg.Wait()
}

func TestStartPruneTicker_PrunesStaleAgents(t *testing.T) {
	store := agentstore.New(50 * time.Millisecond)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Add a stale agent (TTL is 50ms, this agent is 1 hour old)
	store.Apply(agentstore.Snapshot{
		Host:    "test-host",
		TakenAt: time.Now().Add(-time.Hour),
		Agents: []agentstore.Agent{
			{ID: "stale-agent", Executor: "claude", State: "exited", StartedAt: time.Now()},
		},
	})

	// Start ticker with very short interval
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		StartPruneTicker(store, 20*time.Millisecond, logger, ctx)
	}()

	// Wait long enough for at least one prune tick
	time.Sleep(100 * time.Millisecond)

	cancel()
	wg.Wait()

	// Stale agent should have been pruned
	if n := len(store.Recent()); n != 0 {
		t.Errorf("expected 0 agents after prune, got %d", n)
	}
}
