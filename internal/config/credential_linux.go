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
