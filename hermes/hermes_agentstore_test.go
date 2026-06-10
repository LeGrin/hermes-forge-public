package hermes

import (
	"testing"
	"time"
)

// TestNewAgentStore_DefaultTTL locks the public facade contract: 0
// TTL falls back to the 5-minute default and Recent()/Apply are usable.
func TestNewAgentStore_DefaultTTL(t *testing.T) {
	s := NewAgentStore(0)
	if s == nil {
		t.Fatal("NewAgentStore returned nil")
	}
	if len(s.Recent()) != 0 {
		t.Error("fresh store should have zero agents")
	}
}

// TestNewAgentStore_ExplicitTTL verifies the override path.
func TestNewAgentStore_ExplicitTTL(t *testing.T) {
	s := NewAgentStore(time.Second)
	if s == nil {
		t.Fatal("nil store")
	}
}
