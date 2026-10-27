package mcpserver

import (
	"errors"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/obs"
)

// TestFailOpRecordsAnEvent pins the behaviour the reported defect made
// impossible to rely on: a tool call that fails before it starts still
// leaves a trace in the audit stream.
//
// Before this, a failure to read the credential produced no audit record
// at all, so an operator could not tell a broken configuration from a
// server that was merely down.
func TestFailOpRecordsAnEvent(t *testing.T) {
	var events []obs.Event
	s := New(Options{
		Actor: "mcp:test",
		Publish: func(e obs.Event) {
			events = append(events, e)
		},
	})

	s.failOp("bastion", "host01", "hostname", errors.New("the credential store is locked"))

	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	e := events[0]
	if e.Kind != obs.KindError {
		t.Fatalf("event kind = %q, want %q", e.Kind, obs.KindError)
	}
	if e.Server != "bastion" {
		t.Fatalf("event server = %q, want %q", e.Server, "bastion")
	}
	if e.Asset != "host01" {
		t.Fatalf("event asset = %q, want %q", e.Asset, "host01")
	}
	if e.Command != "hostname" {
		t.Fatalf("event command = %q, want %q", e.Command, "hostname")
	}
	if !strings.Contains(e.Reason, "locked") {
		t.Fatalf("event reason = %q, want it to carry the cause", e.Reason)
	}
	if e.Actor != "mcp:test" {
		t.Fatalf("event actor = %q, want %q", e.Actor, "mcp:test")
	}
}

// TestFailOpIgnoresANilError checks a nil cause produces no event, so a
// success path cannot leave a phantom failure in the audit stream.
func TestFailOpIgnoresANilError(t *testing.T) {
	var events []obs.Event
	s := New(Options{
		Publish: func(e obs.Event) {
			events = append(events, e)
		},
	})

	s.failOp("bastion", "host01", "hostname", nil)

	if len(events) != 0 {
		t.Fatalf("published %d events, want 0", len(events))
	}
}

// TestFailOpReasonCarriesNoCredential guards the audit stream against
// leaking a secret: the event reason is the failure text, so a failure
// that happens to mention a password must not be recorded.
//
// The locked-store error names the environment variable to set, and that
// is the shape this checks: a variable name, never a value.
func TestFailOpReasonCarriesNoCredential(t *testing.T) {
	var events []obs.Event
	s := New(Options{
		Publish: func(e obs.Event) {
			events = append(events, e)
		},
	})

	s.failOp("bastion", "host01", "hostname",
		errors.New("the credential store is locked — unlock the keyring, or set JMS_PASSWORD_BASTION"))

	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	reason := events[0].Reason
	if !strings.Contains(reason, "JMS_PASSWORD_BASTION") {
		t.Fatalf("event reason = %q, want the hint to survive", reason)
	}
	// The reason must not look like it carries a value after the "=".
	if idx := strings.Index(reason, "JMS_PASSWORD_BASTION="); idx >= 0 {
		t.Fatalf("event reason = %q, want no value after the variable name", reason)
	}
}
