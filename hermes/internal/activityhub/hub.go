// Package activityhub provides an in-memory, ephemeral event stream
// for live executor activity. Events are stored in a fixed-size ring
// buffer and fanned out to SSE subscribers in real time.
//
// Nothing here touches the database. A Hermes restart clears all
// activity — by design, these are "what is happening right now" signals,
// not persistent truth.
package activityhub

import (
	"sync"
	"time"
)

// Event is a single activity signal from an executor or system component.
type Event struct {
	APIKey     string    `json:"api_key"`
	EnvelopeID string    `json:"envelope_id"`
	Kind       string    `json:"kind"`    // e.g. "tool_use", "status", "heartbeat"
	Summary    string    `json:"summary"` // human-readable one-liner
	Timestamp  time.Time `json:"timestamp"`
}

const ringSize = 1024

// Hub is the central activity event bus. It stores the last ringSize
// events and fans out new events to live subscribers.
type Hub struct {
	mu   sync.RWMutex
	ring [ringSize]Event
	head int // next write position
	len  int // how many slots are filled (max ringSize)

	subsMu sync.RWMutex
	subs   map[*subscriber]struct{}
}

type subscriber struct {
	key string // API key filter ("" = all, for admin)
	ch  chan Event
}

// New creates a ready-to-use Hub.
func New() *Hub {
	return &Hub{
		subs: make(map[*subscriber]struct{}),
	}
}

// Publish writes an event to the ring buffer and fans it out to all
// matching subscribers. Non-blocking: if a subscriber's channel is full,
// the event is dropped for that subscriber (slow consumer).
func (h *Hub) Publish(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}

	h.mu.Lock()
	h.ring[h.head] = e
	h.head = (h.head + 1) % ringSize
	if h.len < ringSize {
		h.len++
	}
	h.mu.Unlock()

	h.subsMu.RLock()
	defer h.subsMu.RUnlock()
	for sub := range h.subs {
		// Public events (APIKey=="") are operational telemetry — fan
		// out to every authenticated subscriber. Otherwise scope by key.
		if sub.key != "" && e.APIKey != "" && sub.key != e.APIKey {
			continue
		}
		select {
		case sub.ch <- e:
		default: // slow consumer, drop
		}
	}
}

// Recent returns up to n most recent events matching the given API key.
// Pass "" for key to get all events (admin view).
//
// Events published with APIKey="" are "public" — operational telemetry
// (envelope status transitions etc.) that every authenticated operator
// should see. They bypass the per-key filter.
func (h *Hub) Recent(key string, n int) []Event {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if n > h.len {
		n = h.len
	}
	result := make([]Event, 0, n)
	for i := 0; i < h.len && len(result) < n; i++ {
		idx := (h.head - 1 - i + ringSize) % ringSize
		e := h.ring[idx]
		if key != "" && e.APIKey != "" && e.APIKey != key {
			continue
		}
		result = append(result, e)
	}
	return result
}

// Subscribe returns a channel that receives events matching the given
// API key. Pass "" for all events (admin). Call Subscription.Close to clean up.
func (h *Hub) Subscribe(key string) *Subscription {
	sub := &subscriber{
		key: key,
		ch:  make(chan Event, 64),
	}
	h.subsMu.Lock()
	h.subs[sub] = struct{}{}
	h.subsMu.Unlock()
	return &Subscription{hub: h, sub: sub}
}

// Subscription is a handle to an active event subscription.
type Subscription struct {
	hub *Hub
	sub *subscriber
}

// Events returns the channel to read events from.
func (s *Subscription) Events() <-chan Event { return s.sub.ch }

// Close removes the subscription and closes the channel.
func (s *Subscription) Close() {
	s.hub.subsMu.Lock()
	delete(s.hub.subs, s.sub)
	s.hub.subsMu.Unlock()
	close(s.sub.ch)
}
