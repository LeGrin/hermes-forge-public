// Package main provides the Hermes VPS transport/status authority.
//
// CON-013: Agent registry prune policy — TTL-based cleanup of stale agent
// entries from the in-memory agentstore, with operator-triggered drain.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/legrin-tech/hermes/internal/agentstore"
)

// agentConfig holds the parsed TTL and prune interval for agents.
type agentConfig struct {
	TTL      time.Duration
	Interval time.Duration
}

// warnFunc is a logging adapter for parse errors.
type warnFunc func(msg string, args ...any)

// ParseAgentConfig parses HERMES_AGENT_TTL and HERMES_AGENT_PRUNE_INTERVAL
// from the environment with safe fallbacks. Exposed for testing.
func ParseAgentConfig(getenv func(string) string, warn warnFunc) agentConfig {
	const defaultTTL = 15 * time.Minute
	const defaultInterval = time.Minute

	cfg := agentConfig{TTL: defaultTTL, Interval: defaultInterval}

	if v := getenv("HERMES_AGENT_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.TTL = d
		} else {
			warn("invalid HERMES_AGENT_TTL, using default", "value", v, "err", err, "default", defaultTTL.String())
		}
	}
	if v := getenv("HERMES_AGENT_PRUNE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Interval = d
		} else {
			warn("invalid HERMES_AGENT_PRUNE_INTERVAL, using default", "value", v, "err", err, "default", defaultInterval.String())
		}
	}
	return cfg
}

// StartPruneTicker launches a background goroutine that prunes stale agents
// every interval. It stops cleanly when ctx is cancelled. Exposed for testing.
func StartPruneTicker(agents *agentstore.Store, interval time.Duration, logger *slog.Logger, ctx context.Context) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				if n := agents.Prune(); n > 0 {
					logger.Info("agent prune", "pruned", n)
				}
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
}
