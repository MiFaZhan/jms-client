//go:build linux

package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// restoreLinuxCredentialHooks puts the Linux credential hooks back after a
// test replaced them, so one test cannot leak a fake into another.
func restoreLinuxCredentialHooks(t *testing.T) {
	t.Helper()
	oldReader := credentialReader
	oldProbe := credentialStoreStateProbe
	t.Cleanup(func() {
		credentialReader = oldReader
		credentialStoreStateProbe = oldProbe
	})
}

// TestReadCredentialFailsFastWhenStoreIsLocked is the regression test for
// the reported defect: on a machine with no graphical session, a locked
// credential store made `jms exec` block forever with no output at all.
//
// The lock state is detected by a probe, so the read must fail promptly
// and say what to do about it — never wait for a prompt that no human can
// answer.
func TestReadCredentialFailsFastWhenStoreIsLocked(t *testing.T) {
	restoreLinuxCredentialHooks(t)

	credentialStoreStateProbe = func(ctx context.Context) error {
		return errors.New("locked: the collection is locked")
	}
	// The read blocks for a long time, exactly like the real one does on
	// a locked keyring. If the implementation waited for it, this test
	// would fail on the elapsed-time check rather than hang forever.
	credentialReader = func(service, account string) (string, error) {
		time.Sleep(30 * time.Second)
		return "", errors.New("the read should not have been waited for")
	}

	start := time.Now()
	_, err := readCredential("jms-client", "bastion/password")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("readCredential() = nil error, want a locked-store failure")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("readCredential() took %s, want a prompt failure", elapsed)
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("readCredential() error = %q, want it to mention the lock", err)
	}
}

// TestReadCredentialReturnsValueWhenStoreIsReadable checks the normal
// path still works: an unlocked store must not change the outcome.
func TestReadCredentialReturnsValueWhenStoreIsReadable(t *testing.T) {
	restoreLinuxCredentialHooks(t)

	credentialStoreStateProbe = func(ctx context.Context) error { return nil }
	credentialReader = func(service, account string) (string, error) {
		return "s3cret", nil
	}

	got, err := readCredential("jms-client", "bastion/password")
	if err != nil {
		t.Fatalf("readCredential() = %v, want nil", err)
	}
	if got != "s3cret" {
		t.Fatalf("readCredential() = %q, want %q", got, "s3cret")
	}
}

// TestReadCredentialPropagatesReadError checks that a real read failure
// reaches the caller instead of being masked by the probe.
func TestReadCredentialPropagatesReadError(t *testing.T) {
	restoreLinuxCredentialHooks(t)

	credentialStoreStateProbe = func(ctx context.Context) error { return nil }
	credentialReader = func(service, account string) (string, error) {
		return "", errors.New("boom")
	}

	if _, err := readCredential("jms-client", "bastion/password"); err == nil {
		t.Fatal("readCredential() = nil error, want the read failure")
	}
}

// TestProbeSecretServiceLockReportsUnlockableStore is a no-op on a machine
// without a Secret Service, but it pins the contract: the probe must never
// report a locked store when it cannot reach one.
func TestProbeSecretServiceLockReportsUnlockableStore(t *testing.T) {
	if err := probeSecretServiceLock(context.Background()); err != nil {
		// A locked store is a legitimate outcome on a headless machine
		// with a real keyring, so it is accepted here. Any other failure
		// would mean the probe is not the "is it locked" check it claims
		// to be.
		if !errors.Is(err, ErrCredentialStoreLocked) {
			t.Fatalf("probeSecretServiceLock() = %v, want nil or ErrCredentialStoreLocked", err)
		}
	}
}

// TestCredentialHintNamesEnvironmentVariable checks the hint tells the
// operator how to get out of a locked store without a graphical session.
func TestCredentialHintNamesEnvironmentVariable(t *testing.T) {
	hint := CredentialHint("bastion")
	if !strings.Contains(hint, "JMS_PASSWORD_BASTION") {
		t.Fatalf("CredentialHint() = %q, want it to name JMS_PASSWORD_BASTION", hint)
	}
	if strings.Contains(hint, "s3cret") {
		t.Fatal("CredentialHint() leaked a credential value")
	}
}
