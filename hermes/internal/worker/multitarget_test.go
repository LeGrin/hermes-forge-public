package worker

import (
	"context"
	"testing"

	"github.com/legrin-tech/hermes/envelope"
)

// routingClient records which deliveries it received.
type routingClient struct {
	calls []string
}

func (r *routingClient) Deliver(_ context.Context, deliveryID string, _ *envelope.Envelope, _ string) (string, error) {
	r.calls = append(r.calls, deliveryID)
	return "sess-" + deliveryID, nil
}

func TestMultiTarget_RoutesToNamedTarget(t *testing.T) {
	def := &routingClient{}
	named := &routingClient{}

	m := NewMultiTarget(def, map[string]ForgeClient{
		"marshal-vps": named,
	})

	e := &envelope.Envelope{TargetNode: "marshal-vps"}
	if _, err := m.Deliver(context.Background(), "d1", e, ""); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if len(named.calls) != 1 || named.calls[0] != "d1" {
		t.Errorf("named target calls = %v, want [d1]", named.calls)
	}
	if len(def.calls) != 0 {
		t.Errorf("default should not be called, got %v", def.calls)
	}
}

func TestMultiTarget_FallsBackToDefault(t *testing.T) {
	def := &routingClient{}
	named := &routingClient{}

	m := NewMultiTarget(def, map[string]ForgeClient{
		"marshal-vps": named,
	})

	// Unknown target_node → default.
	e := &envelope.Envelope{TargetNode: "mac-forge"}
	if _, err := m.Deliver(context.Background(), "d2", e, ""); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if len(def.calls) != 1 || def.calls[0] != "d2" {
		t.Errorf("default calls = %v, want [d2]", def.calls)
	}
	if len(named.calls) != 0 {
		t.Errorf("named target should not be called, got %v", named.calls)
	}
}

func TestMultiTarget_NilEnvelope_UsesDefault(t *testing.T) {
	def := &routingClient{}
	m := NewMultiTarget(def, nil)

	if _, err := m.Deliver(context.Background(), "d3", nil, ""); err != nil {
		t.Fatalf("deliver nil envelope: %v", err)
	}
	if len(def.calls) != 1 {
		t.Errorf("default calls = %v, want [d3]", def.calls)
	}
}
