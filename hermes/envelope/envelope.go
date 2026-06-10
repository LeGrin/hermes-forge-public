// Package envelope defines the canonical Envelope type and its status
// state machine. This package is imported by hermes/internal/* packages;
// it must not import them (W-H12: structural separation).
//
// See ENVELOPE-SPEC.md for the field-level spec and WORLDVIEW.md for the
// invariants enforced here.
package envelope

import (
	"errors"
	"fmt"
	"time"
)

// Status is the envelope lifecycle state.
type Status string

const (
	StatusCreated         Status = "created"
	StatusDelivered       Status = "delivered"
	StatusRead            Status = "read"
	StatusInProgress      Status = "in_progress"
	StatusPaused          Status = "paused"
	StatusBlocked         Status = "blocked"
	StatusAwaitingConfirm Status = "awaiting_confirm"
	StatusDone            Status = "done"
	StatusFailed          Status = "failed"
	StatusLost            Status = "lost"
)

// IsTerminal reports whether s is a terminal state. Terminal envelopes
// never transition to another status (W-H17: ownership is never forgotten;
// terminal rows stay in the store).
func (s Status) IsTerminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusLost:
		return true
	}
	return false
}

// Known reports whether s is a recognised envelope status.
func (s Status) Known() bool {
	switch s {
	case StatusCreated, StatusDelivered, StatusRead, StatusInProgress,
		StatusPaused, StatusBlocked, StatusAwaitingConfirm,
		StatusDone, StatusFailed, StatusLost:
		return true
	}
	return false
}

// Delivery captures delivery/read acknowledgement facts.
type Delivery struct {
	Delivered   bool       `json:"delivered"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	Read        bool       `json:"read"`
	ReadAt      *time.Time `json:"read_at,omitempty"`
}

// Metrics holds lifecycle timestamps and retry counter.
type Metrics struct {
	StartedAt   *time.Time `json:"started_at,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	RetryCount  int        `json:"retry_count"`
}

// Message represents a single message in a thread conversation.
// Used for v2 two-way communication between KITT, OpenCode, and Claude.
type Message struct {
	ID      string    `json:"id"`
	From    string    `json:"from"` // "kitt" | "opencode" | "claude"
	Kind    string    `json:"kind"` // "decision" | "steer" | "reply"
	Text    string    `json:"text"`
	ReplyTo string    `json:"reply_to,omitempty"`
	At      time.Time `json:"at"`
}

// Envelope is the transportable unit of work. Field shape matches
// ENVELOPE-SPEC.md §Required Fields.
type Envelope struct {
	ID                 string            `json:"id"`
	CreatedAt          time.Time         `json:"created_at"`
	CreatedBy          string            `json:"created_by"`
	Title              string            `json:"title"`
	Domain             string            `json:"domain"`
	Project            string            `json:"project"`
	TargetNode         string            `json:"target_node"`
	TargetExecutor     string            `json:"target_executor"`
	TaskTitle          string            `json:"task_title"`
	TaskGoal           string            `json:"task_goal"`
	TaskSteps          []string          `json:"task_steps"`
	SuccessCriteria    []string          `json:"success_criteria"`
	EscalationCriteria []string          `json:"escalation_criteria"`
	ProofRequired      []string          `json:"proof_required"`
	Status             Status            `json:"status"`
	Delivery           Delivery          `json:"delivery"`
	CapabilityHints    []string          `json:"capability_hints"`
	SessionBinding     *string           `json:"session_binding,omitempty"`
	ExecutorSessionID  string            `json:"executor_session_id"`
	Thread             []Message         `json:"thread"`
	Metrics            Metrics           `json:"metrics"`
	History            []string          `json:"history"`
	Proof              map[string]string `json:"proof,omitempty"`
}

// Transition and validation errors.
var (
	ErrTerminalTransition = errors.New("envelope: cannot transition from terminal state")
	ErrDoneWithoutProof   = errors.New("envelope: cannot transition to done without required proof")
	ErrUnknownStatus      = errors.New("envelope: unknown status")
)

// Validate returns nil if e has the minimum structural shape required
// for insertion. Full semantic validation (W-H11 "unclear message"
// rejection) is explicitly deferred — see WORLDVIEW-CHECKLIST.md.
func (e *Envelope) Validate() error {
	if e.ID == "" {
		return errors.New("envelope: id is required")
	}
	if e.Title == "" {
		return errors.New("envelope: title is required")
	}
	if e.TaskTitle == "" {
		return errors.New("envelope: task_title is required")
	}
	if e.TargetExecutor == "" {
		return errors.New("envelope: target_executor is required")
	}
	if !e.Status.Known() {
		return fmt.Errorf("%w: %q", ErrUnknownStatus, e.Status)
	}
	return nil
}

// ValidateProofValues checks that no proof value is an empty string.
// Called during CanTransition to done — proof keys must have real values.
func ValidateProofValues(proof map[string]string) error {
	for k, v := range proof {
		if v == "" {
			return fmt.Errorf("envelope: proof key %q has empty value", k)
		}
	}
	return nil
}

// CanTransition reports whether moving from e.Status to next is permitted.
//
// Invariants enforced:
//   - W-H17: terminal statuses never transition anywhere.
//   - W-H15: transitioning to Done requires that e.Proof contain every key
//     listed in e.ProofRequired — no "done" without proof.
func (e *Envelope) CanTransition(next Status) error {
	if e.Status.IsTerminal() {
		return ErrTerminalTransition
	}
	if !next.Known() {
		return fmt.Errorf("%w: %q", ErrUnknownStatus, next)
	}
	if next == StatusDone {
		for _, key := range e.ProofRequired {
			if _, ok := e.Proof[key]; !ok {
				return fmt.Errorf("%w: missing %q", ErrDoneWithoutProof, key)
			}
		}
		if err := ValidateProofValues(e.Proof); err != nil {
			return fmt.Errorf("%w: %v", ErrDoneWithoutProof, err)
		}
	}
	return nil
}
