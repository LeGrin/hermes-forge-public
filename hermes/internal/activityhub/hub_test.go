package activityhub

import (
	"testing"
	"time"
)

func TestPublishAndRecent(t *testing.T) {
	h := New()
	h.Publish(Event{APIKey: "dev-key-a", EnvelopeID: "env-1", Kind: "tool_use", Summary: "Edit foo.go"})
	h.Publish(Event{APIKey: "dev-key-a", EnvelopeID: "env-1", Kind: "tool_use", Summary: "Bash go test"})
	h.Publish(Event{APIKey: "dev-key-b", EnvelopeID: "env-2", Kind: "status", Summary: "in_progress"})

	// All events.
	all := h.Recent("", 10)
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}
	// Most recent first.
	if all[0].Summary != "in_progress" {
		t.Fatalf("expected most recent first, got %q", all[0].Summary)
	}

	// Key-scoped.
	a := h.Recent("dev-key-a", 10)
	if len(a) != 2 {
		t.Fatalf("expected 2 events for dev-key-a, got %d", len(a))
	}

	b := h.Recent("dev-key-b", 10)
	if len(b) != 1 {
		t.Fatalf("expected 1 event for dev-key-b, got %d", len(b))
	}
}

func TestRecentLimit(t *testing.T) {
	h := New()
	for i := 0; i < 20; i++ {
		h.Publish(Event{APIKey: "dev-key-a", Kind: "tick"})
	}
	got := h.Recent("dev-key-a", 5)
	if len(got) != 5 {
		t.Fatalf("expected 5, got %d", len(got))
	}
}

func TestRingOverwrite(t *testing.T) {
	h := New()
	// Fill beyond ring capacity.
	for i := 0; i < ringSize+100; i++ {
		h.Publish(Event{APIKey: "dev-key-a", Kind: "tick"})
	}
	all := h.Recent("", ringSize+100)
	if len(all) != ringSize {
		t.Fatalf("expected %d (ring cap), got %d", ringSize, len(all))
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	h := New()
	sub := h.Subscribe("dev-key-a")
	defer sub.Close()

	h.Publish(Event{APIKey: "dev-key-a", Kind: "tool_use", Summary: "Edit"})
	h.Publish(Event{APIKey: "dev-key-b", Kind: "tool_use", Summary: "Other"})

	select {
	case e := <-sub.Events():
		if e.Summary != "Edit" {
			t.Fatalf("expected Edit, got %q", e.Summary)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for event")
	}

	// dev-key-b event should NOT arrive.
	select {
	case e := <-sub.Events():
		t.Fatalf("unexpected event: %+v", e)
	case <-time.After(20 * time.Millisecond):
		// ok
	}
}

func TestSubscribeAll(t *testing.T) {
	h := New()
	sub := h.Subscribe("") // admin: all events
	defer sub.Close()

	h.Publish(Event{APIKey: "dev-key-a", Summary: "one"})
	h.Publish(Event{APIKey: "dev-key-b", Summary: "two"})

	count := 0
	for i := 0; i < 2; i++ {
		select {
		case <-sub.Events():
			count++
		case <-time.After(100 * time.Millisecond):
			t.Fatal("timeout")
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 events, got %d", count)
	}
}

func TestUnsubscribe(t *testing.T) {
	h := New()
	sub := h.Subscribe("dev-key-a")
	sub.Close()

	h.Publish(Event{APIKey: "dev-key-a", Summary: "after close"})
	// Channel is closed — reading should return zero value immediately.
	_, ok := <-sub.Events()
	if ok {
		t.Fatal("expected closed channel")
	}
}

func TestTimestampAutoSet(t *testing.T) {
	h := New()
	h.Publish(Event{APIKey: "dev-key-a", Kind: "test"})
	events := h.Recent("", 1)
	if events[0].Timestamp.IsZero() {
		t.Fatal("expected auto-set timestamp")
	}
}

func TestRecentEmpty(t *testing.T) {
	h := New()
	got := h.Recent("dev-key-a", 10)
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}
