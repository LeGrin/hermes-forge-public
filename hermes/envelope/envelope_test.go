package envelope

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newValid(t *testing.T) *Envelope {
	t.Helper()
	return &Envelope{
		ID:             "env-1",
		CreatedAt:      time.Now(),
		CreatedBy:      "kitt",
		Title:          "test envelope",
		TaskTitle:      "run smoke",
		TargetExecutor: "opencode",
		Status:         StatusCreated,
		ProofRequired:  []string{"commit", "test_report"},
	}
}

func TestValidate_OK(t *testing.T) {
	e := newValid(t)
	if err := e.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

// v2-001: Title is a new required field.
func TestValidate_Title_Required(t *testing.T) {
	e := newValid(t)
	if err := e.Validate(); err != nil {
		t.Fatalf("expected valid with title set, got %v", err)
	}
	e.Title = ""
	err := e.Validate()
	if err == nil {
		t.Fatal("expected error when title is empty")
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("expected 'title is required' error, got %v", err)
	}
}

func TestValidate_Rejects_MissingFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Envelope)
	}{
		{"no id", func(e *Envelope) { e.ID = "" }},
		{"no title", func(e *Envelope) { e.TaskTitle = "" }},
		{"no executor", func(e *Envelope) { e.TargetExecutor = "" }},
		{"unknown status", func(e *Envelope) { e.Status = "bogus" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newValid(t)
			tc.mutate(e)
			if err := e.Validate(); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// W-H15: an envelope with unmet proof_required cannot transition to done.
func TestCanTransition_Done_RequiresAllProof(t *testing.T) {
	e := newValid(t)
	e.Status = StatusAwaitingConfirm
	e.Proof = map[string]string{"commit": "abc123"} // missing test_report

	err := e.CanTransition(StatusDone)
	if !errors.Is(err, ErrDoneWithoutProof) {
		t.Fatalf("expected ErrDoneWithoutProof, got %v", err)
	}

	e.Proof["test_report"] = "ok"
	if err := e.CanTransition(StatusDone); err != nil {
		t.Fatalf("expected transition to done with full proof, got %v", err)
	}
}

// W-H17: terminal envelopes never transition anywhere.
func TestCanTransition_Terminal_Rejected(t *testing.T) {
	for _, s := range []Status{StatusDone, StatusFailed, StatusLost} {
		t.Run(string(s), func(t *testing.T) {
			e := newValid(t)
			e.Status = s
			if err := e.CanTransition(StatusInProgress); !errors.Is(err, ErrTerminalTransition) {
				t.Fatalf("expected ErrTerminalTransition from %s, got %v", s, err)
			}
		})
	}
}

func TestCanTransition_UnknownNext_Rejected(t *testing.T) {
	e := newValid(t)
	if err := e.CanTransition("made_up"); !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("expected ErrUnknownStatus, got %v", err)
	}
}

func TestCanTransition_DoneWithEmptyProofValue(t *testing.T) {
	e := newValid(t)
	e.ProofRequired = []string{"commit"}
	e.Proof = map[string]string{"commit": ""}
	if err := e.CanTransition(StatusDone); !errors.Is(err, ErrDoneWithoutProof) {
		t.Fatalf("expected ErrDoneWithoutProof for empty value, got %v", err)
	}
}

func TestValidateProofValues(t *testing.T) {
	if err := ValidateProofValues(map[string]string{"k": "v"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateProofValues(map[string]string{"k": ""}); err == nil {
		t.Fatal("expected error for empty value")
	}
	if err := ValidateProofValues(nil); err != nil {
		t.Fatalf("nil map should be ok: %v", err)
	}
}
