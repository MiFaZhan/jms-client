//go:build linux

package config

import (
	"context"
	"fmt"
)

// credentialReadResult is the outcome of a credential read.
type credentialReadResult struct {
	value string
	err   error
}

// readCredential reads one credential from the Linux Secret Service.
//
// Two things can go wrong on a Linux credential store, and they need
// different answers:
//
//   - The store is locked. The Secret Service then answers Unlock with a
//     prompt path, and go-keyring waits for a human to type the unlock
//     password. On a headless machine nobody can answer, so the call
//     blocks forever: the user sees no output, no error and no audit
//     record, and the process is eventually killed by an external timeout.
//
//   - The store is fine but the entry is absent. That is an ordinary
//     "not found" and must keep saying so.
//
// The two are told apart by racing a lock probe against the real read.
//
// The probe reads the collection's Locked property. That is a plain
// property read, so it never provokes a prompt and always answers
// immediately — it is the only way to learn the lock state on a headless
// machine. The real read answers immediately too when the store is
// unlocked.
//
// The winner decides:
//
//   - the probe wins with "locked" → fail at once, naming the way out;
//   - the probe wins with "readable" → the read is not blocked, so wait
//     for it;
//   - the read wins → the credential was read; the probe's answer is no
//     longer needed.
//
// The read runs in its own goroutine because it cannot be interrupted: the
// D-Bus read has no context. It is abandoned when the probe settles the
// question, and the goroutine ends with the process.
//
// This keeps go-keyring as the reader, so its mock provider still works
// for tests and the other platforms keep the same behaviour.
func readCredential(service, account string) (string, error) {
	ctx := context.Background()

	type readResult = credentialReadResult
	readDone := make(chan readResult, 1)
	go func() {
		v, err := keyringGet(service, account)
		readDone <- readResult{value: v, err: err}
	}()

	probe := credentialStoreStateProbe
	if probe == nil {
		probe = probeSecretServiceLock
	}
	probeDone := make(chan error, 1)
	go func() {
		probeDone <- probe(ctx)
	}()

	select {
	case r := <-readDone:
		// The read settled first. It is authoritative: the credential
		// was found, or the store answered with a real error.
		return r.value, r.err
	case err := <-probeDone:
		if err == nil {
			// The store is readable, so the read is not stuck on a
			// prompt. Wait for it, with a bound in case it wedges for
			// another reason.
			return awaitRead(ctx, readDone, service, account)
		}
		// The probe found a problem: a locked store, most likely. Report
		// it instead of waiting for a read that will never finish.
		return "", err
	}
}

// awaitRead waits for the credential read to finish, bounded by
// keyringCallTimeout.
func awaitRead(ctx context.Context, done <-chan credentialReadResult, service, account string) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, keyringCallTimeout)
	defer cancel()

	select {
	case r := <-done:
		return r.value, r.err
	case <-waitCtx.Done():
		return "", fmt.Errorf("%w: reading %s from the credential store timed out after %s",
			ErrCredentialStoreLocked, account, keyringCallTimeout)
	}
}

// credentialStoreStateProbe reports the state of the OS credential store
// before a read is attempted.
//
// It returns ErrCredentialStoreLocked when the store is locked and cannot
// be unlocked unattended, and nil when the store is readable. It is a
// variable so a test can substitute a fake. A nil value means "use
// probeSecretServiceLock".
var credentialStoreStateProbe func(ctx context.Context) error

// writeCredential stores one credential in the Linux Secret Service.
//
// The write path has the same trap as the read path, and the same
// consequence. go-keyring's Set opens the collection with Unlock before
// creating the item, and Unlock blocks on a prompt that nobody can
// answer on a headless machine: the command would hang with no output
// and no audit record, exactly like `jms exec` used to.
//
// So the lock state is probed first. A locked store is reported at once,
// with the way out, instead of hanging. The write itself then runs
// bounded, because the underlying D-Bus call cannot be interrupted: a
// store that wedges for another reason must not hang the command
// either.
func writeCredential(service, account, value string) error {
	if err := probeCredentialStore(); err != nil {
		return fmt.Errorf("%w: the credential for %q cannot be stored%s",
			ErrCredentialStoreLocked, account, writeLockHint())
	}
	return runBoundedCredentialOp("storing "+account, func() error {
		return keyringSet(service, account, value)
	})
}

// deleteCredential removes one credential from the Linux Secret Service.
//
// go-keyring's Delete also opens the collection with Unlock, so the same
// lock probe and the same bound apply. A locked store means the entry is
// still there, and the error says so rather than pretending it is gone.
func deleteCredential(service, account string) error {
	if err := probeCredentialStore(); err != nil {
		return fmt.Errorf("%w: the credential for %q cannot be removed%s",
			ErrCredentialStoreLocked, account, writeLockHint())
	}
	return runBoundedCredentialOp("deleting "+account, func() error {
		return keyringDelete(service, account)
	})
}

// probeCredentialStore reports the lock state of the OS credential store.
func probeCredentialStore() error {
	probe := credentialStoreStateProbe
	if probe == nil {
		probe = probeSecretServiceLock
	}
	return probe(context.Background())
}

// runBoundedCredentialOp runs one credential-store operation with a bound.
//
// The operation cannot be cancelled, so on timeout it is abandoned and
// the goroutine ends with the process.
func runBoundedCredentialOp(what string, op func() error) error {
	done := make(chan error, 1)
	go func() { done <- op() }()

	waitCtx, cancel := context.WithTimeout(context.Background(), keyringCallTimeout)
	defer cancel()

	select {
	case err := <-done:
		return err
	case <-waitCtx.Done():
		return fmt.Errorf("%w: %s in the credential store timed out after %s",
			ErrCredentialStoreLocked, what, keyringCallTimeout)
	}
}

// writeLockHint explains what to do about a locked credential store on the
// write path.
//
// The environment variable that bypasses the store on the read path
// cannot help here: it is read-only, so it cannot receive a stored
// credential. Unlocking the keyring is the only route.
func writeLockHint() string {
	if hasGraphicalSession() {
		return " (unlock the keyring from the desktop session first)"
	}
	return " (unlock the keyring before storing credentials)"
}
