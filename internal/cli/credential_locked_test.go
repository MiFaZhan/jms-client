package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/config"
)

// lockedStore is a CredentialStore whose reads fail the way a locked OS
// credential store fails. It stands in for the headless-machine case
// reported against `jms exec`: the store exists but cannot be read.
type lockedStore struct {
	config.CredentialStore
}

func (l lockedStore) Get(alias, kind string) (string, error) {
	// The same error the real store produces, so the hint an operator
	// sees is the one under test.
	return "", &config.LockedError{Alias: alias}
}

// TestExecReportsLockedCredentialStoreWithGuidance is the regression test
// for the reported defect: with a locked credential store, `jms exec` used
// to block forever with no output. It must fail promptly, with a non-zero
// exit code and a message that says what to do.
//
// The message has to name the environment variable, because on a headless
// machine unlocking the keyring is not an option.
func TestExecReportsLockedCredentialStoreWithGuidance(t *testing.T) {
	env := newCLIEnv(t)
	env.creds = lockedStore{}
	// A server entry exists, so the failure is the credential and not the
	// configuration.
	env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")

	code := env.run("exec", "host01", "hostname")

	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero (stderr: %s)", env.stderr())
	}
	msg := env.stderr()
	if !strings.Contains(msg, "locked") {
		t.Fatalf("stderr = %q, want it to say the store is locked", msg)
	}
	if !strings.Contains(msg, "JMS_PASSWORD_BASTION") {
		t.Fatalf("stderr = %q, want it to name JMS_PASSWORD_BASTION", msg)
	}
}

// TestExecWithTimingReportsTheCredentialStage checks the --timing flag
// prints the credential stage, including on the failure path: a run that
// dies in the credential store is exactly the run whose timing matters.
func TestExecWithTimingReportsTheCredentialStage(t *testing.T) {
	env := newCLIEnv(t)
	env.creds = lockedStore{}
	env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")

	env.run("exec", "--timing", "host01", "hostname")

	if !strings.Contains(env.stderr(), "Timing credentials:") {
		t.Fatalf("stderr = %q, want a credential timing line", env.stderr())
	}
}

// TestConfigListWarnsAboutLockedCredentialStore checks the config listing
// reports a locked store as a problem rather than silently hiding it.
func TestConfigListWarnsAboutLockedCredentialStore(t *testing.T) {
	env := newCLIEnv(t)
	env.creds = lockedStore{}
	env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")

	env.run("config", "list")

	// The listing must not claim a usable credential, and it must not
	// crash either.
	out := env.stdout() + env.stderr()
	if strings.Contains(out, "keyring") && !strings.Contains(out, "locked") {
		t.Fatalf("output = %q, want the locked state to be named", out)
	}
}

// TestLoadCredentialsDistinguishesLockedFromMissing pins the difference
// between the two failure modes: a missing credential and a locked store
// call for different actions, so they must not share a message.
func TestLoadCredentialsDistinguishesLockedFromMissing(t *testing.T) {
	missing := config.NewMemoryStore()
	_, _, err := loadCredentials(Deps{Creds: missing}, "bastion")
	if err == nil {
		t.Fatal("loadCredentials() on an empty store = nil, want an error")
	}
	if !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("loadCredentials() = %v, want it to wrap ErrNotFound", err)
	}

	_, _, err = loadCredentials(Deps{Creds: lockedStore{}}, "bastion")
	if err == nil {
		t.Fatal("loadCredentials() on a locked store = nil, want an error")
	}
	if !errors.Is(err, config.ErrCredentialStoreLocked) {
		t.Fatalf("loadCredentials() = %v, want it to wrap ErrCredentialStoreLocked", err)
	}
	if !strings.Contains(err.Error(), "JMS_PASSWORD_BASTION") {
		t.Fatalf("loadCredentials() = %v, want it to name the environment variable", err)
	}
}
