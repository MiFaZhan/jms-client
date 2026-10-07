package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// keyringService is the service name used for every credential entry
// (DESIGN.md §7.1).
const keyringService = "jms-client"

// envPrefixPassword and envPrefixOTP prefix the environment-variable
// credential fallback (DESIGN.md §7.1).
const (
	envPrefixPassword = "JMS_PASSWORD_"
	envPrefixOTP      = "JMS_OTP_"
)

// CredentialStore abstracts credential persistence so the CLI can be
// tested without touching the real OS credential store.
//
// Implementations must return ErrNotFound (wrapped or bare) for absent
// credentials and must never return a credential together with a
// non-nil error.
type CredentialStore interface {
	// Get returns the credential of kind ("password" or "otp") for alias.
	Get(alias, kind string) (string, error)
	// Set stores the credential of kind for alias.
	Set(alias, kind, value string) error
	// Delete removes every credential held for alias.
	Delete(alias string) error
}

// credentialSuffix maps a credential kind onto the account-name suffix
// used in the OS credential store. It reports false for unknown kinds so
// a typo cannot silently overwrite the password entry.
func credentialSuffix(kind string) (string, bool) {
	switch kind {
	case CredPassword:
		return "password", true
	case CredOTP:
		return "otp_secret", true
	default:
		return "", false
	}
}

// Chain returns a CredentialStore that consults stores in order and
// returns the first hit. Writes go to the first store that accepts them.
//
// The conventional chain is Chain(NewEnvStore(), NewKeyringStore()):
// environment variables win over the OS credential store, and because
// the environment store is read-only a Set falls through to the keyring
// (DESIGN.md §7.1).
func Chain(stores ...CredentialStore) CredentialStore {
	return &chainStore{stores: stores}
}

type chainStore struct {
	stores []CredentialStore
}

func (c *chainStore) Get(alias, kind string) (string, error) {
	var firstErr error
	for _, s := range c.stores {
		if s == nil {
			continue
		}
		v, err := s.Get(alias, kind)
		if err == nil {
			return v, nil
		}
		// A backend that is merely missing the entry must not mask a real
		// failure (for example a locked credential store) reported later.
		if !errors.Is(err, ErrNotFound) && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", fmt.Errorf("%w: %s/%s", ErrNotFound, alias, kind)
}

func (c *chainStore) Set(alias, kind, value string) error {
	var readOnlyErr error
	for _, s := range c.stores {
		if s == nil {
			continue
		}
		if err := s.Set(alias, kind, value); err != nil {
			if errors.Is(err, ErrReadOnly) {
				readOnlyErr = err
				continue
			}
			return err
		}
		return nil
	}
	if readOnlyErr != nil {
		return readOnlyErr
	}
	return errors.New("no writable credential store configured")
}

func (c *chainStore) Delete(alias string) error {
	var lastErr error
	for _, s := range c.stores {
		if s == nil {
			continue
		}
		// Read-only backends cannot hold credentials, so their refusal is
		// expected and not reported.
		if err := s.Delete(alias); err != nil && !errors.Is(err, ErrReadOnly) {
			lastErr = err
		}
	}
	return lastErr
}

// NewKeyringStore returns a CredentialStore backed by the OS credential
// store (Windows Credential Manager / macOS Keychain / Linux Secret
// Service).
//
// service is "jms-client"; the account is "<alias>/password" or
// "<alias>/otp_secret", matching DESIGN.md §7.1.
func NewKeyringStore() CredentialStore {
	return &keyringStore{}
}

type keyringStore struct{}

// keyringAccount renders the credential-store account name for an alias
// and credential kind.
func keyringAccount(alias, kind string) (string, error) {
	suffix, ok := credentialSuffix(kind)
	if !ok {
		return "", fmt.Errorf("unknown credential kind %q", kind)
	}
	return alias + "/" + suffix, nil
}

func (k *keyringStore) Get(alias, kind string) (string, error) {
	account, err := keyringAccount(alias, kind)
	if err != nil {
		return "", err
	}
	v, err := keyring.Get(keyringService, account)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, account)
		}
		return "", fmt.Errorf("credential store read failed: %w", err)
	}
	return v, nil
}

func (k *keyringStore) Set(alias, kind, value string) error {
	account, err := keyringAccount(alias, kind)
	if err != nil {
		return err
	}
	if err := keyring.Set(keyringService, account, value); err != nil {
		return fmt.Errorf("credential store write failed: %w", err)
	}
	return nil
}

// Delete removes both credential kinds for alias. An entry that is
// already absent is not an error, so a partially provisioned server can
// still be cleaned up.
func (k *keyringStore) Delete(alias string) error {
	var firstErr error
	for _, kind := range []string{CredPassword, CredOTP} {
		account, err := keyringAccount(alias, kind)
		if err != nil {
			continue
		}
		err = keyring.Delete(keyringService, account)
		if err == nil || errors.Is(err, keyring.ErrNotFound) {
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("credential store delete failed: %w", err)
		}
	}
	return firstErr
}

// NewEnvStore returns a read-only CredentialStore backed by environment
// variables, for headless Linux, CI and WSL.
//
// JMS_PASSWORD_<ALIAS> and JMS_OTP_<ALIAS>, with the alias uppercased and
// every '-' replaced by '_' (DESIGN.md §7.1).
func NewEnvStore() CredentialStore {
	return &envStore{}
}

type envStore struct{}

// EnvVarName renders the environment variable that holds alias's
// credential of the given kind. An unknown kind falls back to the
// password variable, matching the CredentialStore contract where kind is
// always one of CredPassword/CredOTP.
// EnvVarName renders the environment variable that holds alias's
// credential of kind. Unknown kinds must fail rather than silently alias
// to the password variable - the keyring path rejects them, and a
// mistyped kind would otherwise hand a TOTP code where a password is
// expected (or the reverse).
func EnvVarName(alias, kind string) (string, error) {
	prefix := ""
	switch kind {
	case CredPassword:
		prefix = envPrefixPassword
	case CredOTP:
		prefix = envPrefixOTP
	default:
		return "", fmt.Errorf("%w: unknown credential kind %q", ErrNotFound, kind)
	}
	normalized := strings.ToUpper(strings.ReplaceAll(alias, "-", "_"))
	return prefix + normalized, nil
}

func (e *envStore) Get(alias, kind string) (string, error) {
	name, err := EnvVarName(alias, kind)
	if err != nil {
		return "", err
	}
	v, ok := os.LookupEnv(name)
	// An exported-but-empty variable means "not configured": treating it
	// as a credential would send an empty password to the server.
	if !ok || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return v, nil
}

func (e *envStore) Set(alias, kind, value string) error {
	name, err := EnvVarName(alias, kind)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is set by the process environment",
		ErrReadOnly, name)
}

// Delete is a no-op: a process cannot unset its own parent's environment
// in a way that survives, and Chain relies on this being tolerated.
func (e *envStore) Delete(alias string) error {
	return nil
}

// NewMemoryStore returns an in-memory CredentialStore for tests and for
// cross-package fakes. It is safe for concurrent use.
func NewMemoryStore() CredentialStore {
	return &memoryStore{values: map[string]string{}}
}

type memoryStore struct {
	mu     sync.RWMutex
	values map[string]string
}

// memoryKey namespaces alias and kind without reusing the keyring account
// format, so a memory store never depends on keyring naming rules.
func memoryKey(alias, kind string) string {
	return alias + "\x00" + kind
}

func (m *memoryStore) Get(alias, kind string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.values[memoryKey(alias, kind)]
	if !ok {
		return "", fmt.Errorf("%w: %s/%s", ErrNotFound, alias, kind)
	}
	return v, nil
}

func (m *memoryStore) Set(alias, kind, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[memoryKey(alias, kind)] = value
	return nil
}

// Delete removes every credential kind held for alias. Deleting an
// unknown alias succeeds: `jms config remove` must be idempotent.
func (m *memoryStore) Delete(alias string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, kind := range []string{CredPassword, CredOTP} {
		delete(m.values, memoryKey(alias, kind))
	}
	return nil
}
