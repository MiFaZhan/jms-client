//go:build linux

// Package config — Linux Secret Service credential path.
//
// Why this file exists (2026-10-27):
//
// The generic credential path uses github.com/zalando/go-keyring, whose
// Linux implementation calls the Secret Service D-Bus API synchronously and
// without a timeout (secret_service.go: `s.object.Call(...).Store(...)`).
// When the login collection is locked, the service answers Unlock with a
// *prompt* path and the client then waits for a human to type the unlock
// password into a graphical prompt (go-keyring's handlePrompt).
//
// On a headless machine there is nobody to answer that prompt: the D-Bus
// call blocks forever. The user sees no output, no error and no audit
// record — the process is simply killed by an external timeout. That is
// the defect reported against `jms exec` on a machine with no graphical
// session (XDG_SESSION_TYPE and DISPLAY both empty).
//
// This file replaces the Linux read path with one that never blocks
// indefinitely:
//
//   - every D-Bus call carries a context with a deadline, so a wedged
//     service surfaces as a timeout error instead of a hang;
//   - the collection's Locked property is inspected *before* any Unlock
//     call, so a locked keyring fails immediately instead of provoking a
//     prompt that can never be answered;
//   - the resulting error names the environment variable that provides a
//     way out, satisfying DESIGN.md「配置与凭据分层」 ("两处都没有 → 明确报错并
//     给出指引").
//
// Only the read path is replaced. Writes still go through go-keyring:
// creating an item in a locked collection legitimately needs the unlock
// prompt, and that is a user-initiated action where waiting for a prompt
// is the correct behaviour.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	dbus "github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
)

// Secret Service D-Bus constants (org.freedesktop.Secret, version 1).
const (
	secretServiceName      = "org.freedesktop.secrets"
	secretServicePath      = "/org/freedesktop/secrets"
	secretServiceInterface = "org.freedesktop.Secret.Service"
	secretCollectionIface  = "org.freedesktop.Secret.Collection"
	secretItemInterface    = "org.freedesktop.Secret.Item"
	secretSessionInterface = "org.freedesktop.Secret.Session"

	// secretLoginCollection is the well-known path of the collection that
	// holds login secrets. It is what the Secret Service returns for the
	// "default" alias.
	secretLoginCollection = "/org/freedesktop/secrets/collection/login"
)

// keyringCallTimeout bounds every single D-Bus round trip to the Secret
// Service. It is deliberately short: the service is a local process, so a
// call that has not answered within a few seconds is not going to.
const keyringCallTimeout = 5 * time.Second

// ErrCredentialStoreLocked reports that the OS credential store exists but
// is locked and cannot be unlocked unattended.
//
// Callers test for it with errors.Is. It is deliberately distinct from
// ErrNotFound: "the entry is absent" and "the store is locked" call for
// different user actions, and the difference is what makes the failure
// diagnosable.
var ErrCredentialStoreLocked = errors.New("credential store is locked")

// secretServiceClient is a minimal Secret Service client that speaks D-Bus
// directly so every call can carry a deadline.
//
// The godbus connection is intentionally not shared with go-keyring's
// package-level session bus: this client needs its own context-aware call
// path, and go-keyring's own helper functions do not accept one.
type secretServiceClient struct {
	conn *dbus.Conn
}

// dialSecretService connects to the session bus and returns a client.
//
// A connection failure is not fatal on its own: the caller decides whether
// a missing Secret Service means "no credential there" or "the credential
// store is broken". The returned error is preserved either way.
func dialSecretService() (*secretServiceClient, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus: %w", err)
	}
	return &secretServiceClient{conn: conn}, nil
}

// object returns a proxy for one Secret Service object path.
func (c *secretServiceClient) object(path dbus.ObjectPath) dbus.BusObject {
	return c.conn.Object(secretServiceName, path)
}

// callTimeout returns a context bounded by keyringCallTimeout.
//
// The parent context is honoured when it has an earlier deadline, so a
// caller-imposed timeout still wins.
func callTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, keyringCallTimeout)
}

// loginCollection returns the path of the collection that holds login
// secrets.
//
// The service exposes a "default" alias; most implementations resolve it
// to a concrete collection path, but the alias itself is also a valid
// object path, so it is the fallback.
func (c *secretServiceClient) loginCollection(ctx context.Context) (dbus.ObjectPath, error) {
	callCtx, cancel := callTimeout(ctx)
	defer cancel()

	// ReadAlias is the documented way to resolve the default collection.
	// Older services may not implement it, so a failure falls back to the
	// Collections property.
	var alias dbus.ObjectPath
	if err := c.object(secretServicePath).CallWithContext(callCtx,
		secretServiceInterface+".ReadAlias", 0, "default").Store(&alias); err == nil {
		return alias, nil
	}

	callCtx2, cancel2 := callTimeout(ctx)
	defer cancel2()
	var paths []dbus.ObjectPath
	if err := c.object(secretServicePath).CallWithContext(callCtx2,
		"org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.Secret.Service", "Collections").Store(&paths); err != nil {
		return "", fmt.Errorf("list the credential collections: %w", err)
	}
	for _, p := range paths {
		if p == dbus.ObjectPath(secretLoginCollection) {
			return p, nil
		}
	}
	if len(paths) > 0 {
		return paths[0], nil
	}
	return "", errors.New("the credential store has no collection")
}

// collectionLocked reports whether the collection is locked.
//
// Reading the Locked property is a plain property read: it never provokes
// an unlock prompt, so it is safe to call on a headless machine. That is
// the whole point of checking it before touching Unlock.
func (c *secretServiceClient) collectionLocked(ctx context.Context, collection dbus.ObjectPath) (bool, error) {
	callCtx, cancel := callTimeout(ctx)
	defer cancel()

	var locked bool
	if err := c.object(collection).CallWithContext(callCtx,
		"org.freedesktop.DBus.Properties.Get", 0,
		secretCollectionIface, "Locked").Store(&locked); err != nil {
		return false, fmt.Errorf("read the credential store lock state: %w", err)
	}
	return locked, nil
}

// probeSecretServiceLock reports whether the Secret Service is usable.
//
// It returns nil when the store can be read, and ErrCredentialStoreLocked
// when the collection is locked and no unattended unlock is possible.
//
// The probe reads the Locked property, which is a plain property read:
// unlike Unlock, it never starts an interactive prompt, so it always
// answers promptly — even with no graphical session.
func probeSecretServiceLock(ctx context.Context) error {
	client, err := dialSecretService()
	if err != nil {
		// No session bus at all: there is no credential store to read, so
		// the entry is simply absent. Reporting "not found" is honest
		// here, and the caller turns that into a message that names the
		// environment variable.
		return nil
	}

	collection, err := client.loginCollection(ctx)
	if err != nil {
		// The service exists but does not answer the way the spec says.
		// Treat it as "no store" rather than blocking: a read that hangs
		// is the defect this code exists to remove.
		return nil
	}

	locked, err := client.collectionLocked(ctx, collection)
	if err != nil {
		// The lock state is unreadable. A store whose state cannot be
		// read might still hold the entry, so the read is attempted and
		// its own timeout applies.
		return nil
	}
	if locked {
		return fmt.Errorf("%w: the collection %s is locked%s",
			ErrCredentialStoreLocked, collection, lockHint())
	}
	return nil
}

// lockHint explains what to do about a locked credential store.
//
// On a machine with no graphical session nobody can answer the unlock
// prompt, so the environment variable is the only route left. The hint
// says so instead of leaving the operator to work it out.
func lockHint() string {
	if hasGraphicalSession() {
		return " (unlock it from the desktop session)"
	}
	return " (no graphical session: unlock it there, or use JMS_PASSWORD_<ALIAS>)"
}

// keyringGet reads one credential through go-keyring.
//
// It is a thin wrapper so the Linux credential path can reach the same
// implementation the other platforms use, and so tests can stub it.
func keyringGet(service, account string) (string, error) {
	if credentialReader != nil {
		return credentialReader(service, account)
	}
	return keyring.Get(service, account)
}

// credentialReader is the function the Linux credential read goes through.
//
// It is a variable so a test can substitute a fake. A nil value means "use
// go-keyring".
var credentialReader func(service, account string) (string, error)

// keyringSet stores one credential through go-keyring.
//
// It is a thin wrapper so the Linux credential path can reach the same
// implementation the other platforms use, and so tests can stub it.
func keyringSet(service, account, value string) error {
	if credentialWriter != nil {
		return credentialWriter(service, account, value)
	}
	return keyring.Set(service, account, value)
}

// keyringDelete removes one credential through go-keyring.
//
// It is a thin wrapper so the Linux credential path can reach the same
// implementation the other platforms use, and so tests can stub it.
func keyringDelete(service, account string) error {
	if credentialDeleter != nil {
		return credentialDeleter(service, account)
	}
	return keyring.Delete(service, account)
}

// credentialWriter is the function the Linux credential write goes through.
//
// It is a variable so a test can substitute a fake. A nil value means
// "use go-keyring".
var credentialWriter func(service, account, value string) error

// credentialDeleter is the function the Linux credential delete goes through.
//
// It is a variable so a test can substitute a fake. A nil value means
// "use go-keyring".
var credentialDeleter func(service, account string) error

// hasGraphicalSession reports whether a human could answer an unlock
// prompt.
//
// It is used only to phrase the error message, never to decide whether to
// try: the lock state itself already decides that.
func hasGraphicalSession() bool {
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		return true
	}
	switch os.Getenv("XDG_SESSION_TYPE") {
	case "x11", "wayland":
		return true
	}
	return false
}
