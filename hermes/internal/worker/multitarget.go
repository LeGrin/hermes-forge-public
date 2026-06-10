// Package worker — multitarget dispatch.
//
// MultiTargetClient routes envelopes to different executor gateways based
// on envelope.target_node. This keeps the rest of the worker blissfully
// unaware of where an envelope ends up running.
//
// Current targets:
//
//   - "mac-forge" (default)   → Forge on the Mac (Claude / OpenCode executor)
//   - "marshal-vps"           → marshal shim on example-vps (hermes-agent executor)
//   - "marshal-mac"           → marshal shim on the Mac     (hermes-agent executor)
//
// All targets speak the same /deliver wire protocol (delivery_id + envelope
// + working_dir → {session_id, acked_at}), so the dispatch is a pure URL
// swap per envelope. Unknown target_node falls through to the default.
package worker

import (
	"context"
	"fmt"

	"github.com/legrin-tech/hermes/envelope"
)

// MultiTargetClient routes Deliver calls by envelope.target_node.
//
// Construct it with NewMultiTarget: provide a default client (existing
// Forge on Mac) and a map of named targets. Nil map = always-default.
type MultiTargetClient struct {
	Default ForgeClient
	Targets map[string]ForgeClient
}

// NewMultiTarget wraps a default client with per-target_node routing.
func NewMultiTarget(def ForgeClient, targets map[string]ForgeClient) *MultiTargetClient {
	return &MultiTargetClient{Default: def, Targets: targets}
}

// Deliver picks the client whose name matches envelope.target_node, or
// falls back to Default when the node is unknown or unset.
func (m *MultiTargetClient) Deliver(ctx context.Context, deliveryID string, e *envelope.Envelope, workingDir string) (string, error) {
	if m == nil || m.Default == nil {
		return "", fmt.Errorf("multitarget: no default client")
	}
	if e == nil {
		return m.Default.Deliver(ctx, deliveryID, e, workingDir)
	}
	if client, ok := m.Targets[e.TargetNode]; ok && client != nil {
		return client.Deliver(ctx, deliveryID, e, workingDir)
	}
	return m.Default.Deliver(ctx, deliveryID, e, workingDir)
}
