//go:build !linux

// Package config — credential read path for platforms other than Linux.
//
// Linux has its own implementation (secret_service_linux.go) because the
// Secret Service can block forever on a locked keyring when no graphical
// session can answer the unlock prompt.
//
// Elsewhere (Windows Credential Manager, macOS Keychain) the OS API either
// returns promptly or blocks on the user by design, so go-keyring is used
// directly. A bounded wait is still applied so a wedged OS service cannot
// hang the command forever: the goroutine is abandoned on timeout rather
// than killed, because the underlying call cannot be interrupted safely.
package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zalando/go-keyring"
)

// keyringCallTimeout bounds a single credential-store read.
const keyringCallTimeout = 10 * time.Second

// ErrCredentialStoreLocked reports that the OS credential store is locked
// and cannot be read unattended. See secret_service_linux.go for the
// Linux-specific handling.
var ErrCredentialStoreLocked = errors.New("credential store is locked")

// readCredential reads one credential from the OS credential store.
//
// service and account name the entry, matching go-keyring's addressing.
func readCredential(service, account string) (string, error) {
	type result struct {
		value string
		err   error
	}
	done := make(chan result, 1)

	go func() {
		v, err := keyring.Get(service, account)
		done <- result{value: v, err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), keyringCallTimeout)
	defer cancel()

	select {
	case r := <-done:
		if r.err != nil {
			if errors.Is(r.err, keyring.ErrNotFound) {
				return "", fmt.Errorf("%w: %s", ErrNotFound, account)
			}
			return "", r.err
		}
		return r.value, nil
	case <-ctx.Done():
		return "", fmt.Errorf("%w: reading %s from the credential store timed out after %s",
			ErrCredentialStoreLocked, account, keyringCallTimeout)
	}
}

// writeCredential stores one credential in the OS credential store.
//
// The Windows and macOS credential APIs return promptly, so the write
// goes straight to go-keyring, bounded so a wedged OS service cannot hang
// the command forever.
func writeCredential(service, account, value string) error {
	done := make(chan error, 1)
	go func() {
		done <- keyring.Set(service, account, value)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), keyringCallTimeout)
	defer cancel()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: storing %s in the credential store timed out after %s",
			ErrCredentialStoreLocked, account, keyringCallTimeout)
	}
}

// deleteCredential removes one credential from the OS credential store.
func deleteCredential(service, account string) error {
	done := make(chan error, 1)
	go func() {
		done <- keyring.Delete(service, account)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), keyringCallTimeout)
	defer cancel()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: deleting %s from the credential store timed out after %s",
			ErrCredentialStoreLocked, account, keyringCallTimeout)
	}
}
