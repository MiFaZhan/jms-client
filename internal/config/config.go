// Package config manages jms metadata (TOML) and credential storage.
//
// Design contract (DESIGN.md §7): the configuration file holds metadata
// only — alias, internal/external URL, username, prefer, ssh_port. It
// never holds a password or a TOTP secret; those live in the OS
// credential store (see credentials.go). The file is therefore safe to
// back up, sync, or commit.
//
// Dependency direction: config depends on nothing else in this module.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ConfigVersion is the schema version written to new configuration files.
const ConfigVersion float64 = 2

// Address kinds. These strings are also the values accepted by
// ServerConfig.Prefer and by the CLI --endpoint flag.
const (
	KindInternal = "internal"
	KindExternal = "external"
)

// Preference values accepted by ServerConfig.Prefer. An empty Prefer
// means PreferInternal.
const (
	PreferInternal = KindInternal
	PreferExternal = KindExternal
)

// DefaultSSHPort is the KoKo SSH port used when ServerConfig.SSHPort is unset.
const DefaultSSHPort = 2222

// Credential kinds accepted by CredentialStore.
const (
	CredPassword = "password"
	CredOTP      = "otp"
)

// Sentinel errors. Callers should test them with errors.Is.
var (
	// ErrNotFound is returned by CredentialStore.Get when a credential
	// is absent from every backing store.
	ErrNotFound = errors.New("credential not found")

	// ErrReadOnly is returned by CredentialStore.Set/Delete for stores
	// that cannot be written (the environment-variable store).
	ErrReadOnly = errors.New("credential store is read-only")

	// ErrConfigNotFound is returned by Load when the configuration file
	// does not exist.
	ErrConfigNotFound = errors.New("config file not found")

	// ErrNoServers is returned when no server is configured at all.
	ErrNoServers = errors.New("no servers configured")

	// ErrServerNotFound is returned when an alias does not exist.
	ErrServerNotFound = errors.New("server not found")

	// ErrLegacyConfig is returned when a pre-rewrite jms-cli config.yaml
	// is detected. The rewrite deliberately does not import it
	// (DESIGN.md §7.3).
	ErrLegacyConfig = errors.New("detected legacy jms-cli config")
)

// ServerConfig is one JumpServer entry.
//
// A server may define an internal address, an external address, or both;
// at least one is required. Credentials are NOT stored here — see
// CredentialStore.
type ServerConfig struct {
	Name     string
	Internal string // internal URL, may be empty
	External string // external URL, may be empty
	Username string
	Prefer   string // "internal" (default) | "external"
	// Pin, when set to a known address kind, restricts the server to that one
	// address: no probing of the other, no failover. For a user who knows
	// which network they are always on, it removes all guessing.
	Pin     string
	SSHPort int // KoKo SSH port; 0 means DefaultSSHPort
}

// URLFor returns the configured URL for kind ("internal" or "external").
//
// The returned URL is exactly as configured (no normalization); api.New
// normalizes it. ok is false when the address is empty, whitespace-only,
// or kind is not a known address kind.
func (s *ServerConfig) URLFor(kind string) (string, bool) {
	if s == nil {
		return "", false
	}
	var raw string
	switch kind {
	case KindInternal:
		raw = s.Internal
	case KindExternal:
		raw = s.External
	default:
		return "", false
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	return raw, true
}

// Preferred reports which address kind to try first: Prefer when it is
// valid, otherwise internal. The result is always KindInternal or
// KindExternal.
func (s *ServerConfig) Preferred() string {
	if s != nil && strings.EqualFold(strings.TrimSpace(s.Prefer), PreferExternal) {
		return KindExternal
	}
	return KindInternal
}

// Port returns SSHPort, or DefaultSSHPort when it is unset or out of range.
func (s *ServerConfig) Port() int {
	if s == nil || s.SSHPort < 1 || s.SSHPort > 65535 {
		return DefaultSSHPort
	}
	return s.SSHPort
}

// Validate checks a server entry.
//
// Rules: Name and Username non-empty; at least one of Internal/External
// set; every non-empty address is an absolute http(s) URL; Prefer is
// empty or a known kind; SSHPort is 0 or in 1..65535.
func Validate(s *ServerConfig) error {
	if s == nil {
		return errors.New("server config is nil")
	}
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("server alias must not be empty")
	}
	if strings.TrimSpace(s.Username) == "" {
		return fmt.Errorf("server %q: username is required", s.Name)
	}
	internal := strings.TrimSpace(s.Internal)
	external := strings.TrimSpace(s.External)
	if internal == "" && external == "" {
		return fmt.Errorf("server %q: at least one of internal/external URL is required", s.Name)
	}
	if internal != "" {
		if err := validateURL(KindInternal, internal); err != nil {
			return fmt.Errorf("server %q: %w", s.Name, err)
		}
	}
	if external != "" {
		if err := validateURL(KindExternal, external); err != nil {
			return fmt.Errorf("server %q: %w", s.Name, err)
		}
	}
	if p := strings.TrimSpace(s.Prefer); p != "" {
		if !strings.EqualFold(p, PreferInternal) && !strings.EqualFold(p, PreferExternal) {
			return fmt.Errorf("server %q: prefer must be %q or %q, got %q",
				s.Name, PreferInternal, PreferExternal, p)
		}
	}
	if s.SSHPort != 0 && (s.SSHPort < 1 || s.SSHPort > 65535) {
		return fmt.Errorf("server %q: ssh_port must be between 1 and 65535, got %d",
			s.Name, s.SSHPort)
	}
	if p := strings.TrimSpace(s.Pin); p != "" {
		if !strings.EqualFold(p, KindInternal) && !strings.EqualFold(p, KindExternal) {
			return fmt.Errorf("server %q: pin must be %q or %q, got %q",
				s.Name, KindInternal, KindExternal, p)
		}
	}
	return nil
}

// validateURL accepts only absolute http(s) URLs with a host. A bare
// "host:port" is rejected: the scheme must be explicit, because the
// internal/external choice drives the failover policy (DESIGN.md §4.7)
// and must not be guessed.
func validateURL(kind, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s URL %q is not a valid URL: %w", kind, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s URL %q must use http:// or https://", kind, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s URL %q is missing a host", kind, raw)
	}
	return nil
}

// AppConfig is the parsed configuration file.
//
// Servers is keyed by alias. Iteration order is not meaningful: use
// Names for a deterministic order.
type AppConfig struct {
	Version float64
	Default string
	Servers map[string]*ServerConfig
}

// Names returns every server alias in sorted order. It returns an empty
// slice (never nil) for a nil receiver, so callers can range over it.
func (c *AppConfig) Names() []string {
	if c == nil {
		return []string{}
	}
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DefaultServer returns the default server.
//
// When Default names an existing alias it wins; otherwise the
// alphabetically first alias is used, so the fallback is deterministic.
//
// Errors: ErrNoServers (wrapped) when nothing is configured.
func (c *AppConfig) DefaultServer() (*ServerConfig, error) {
	if c == nil || len(c.Servers) == 0 {
		return nil, fmt.Errorf("%w: run `jms config add <alias>`", ErrNoServers)
	}
	if c.Default != "" {
		if s, ok := c.Servers[c.Default]; ok && s != nil {
			return s, nil
		}
	}
	for _, name := range c.Names() {
		if s := c.Servers[name]; s != nil {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: run `jms config add <alias>`", ErrNoServers)
}

// Get returns the server with the given alias.
//
// Errors: ErrServerNotFound (wrapped) when the alias is unknown; the
// message lists the available aliases.
func (c *AppConfig) Get(name string) (*ServerConfig, error) {
	if c != nil {
		if s, ok := c.Servers[name]; ok && s != nil {
			return s, nil
		}
	}
	available := "(none)"
	if c != nil {
		if names := c.Names(); len(names) > 0 {
			available = strings.Join(names, ", ")
		}
	}
	return nil, fmt.Errorf("%w: %q (available: %s)", ErrServerNotFound, name, available)
}

// Put validates s and stores it under s.Name, creating the Servers map
// when needed. Name, Username and both addresses are trimmed and Prefer
// is lowercased before validation, so callers may pass raw input. The
// first server added becomes the default.
func (c *AppConfig) Put(s *ServerConfig) error {
	if c == nil {
		return errors.New("app config is nil")
	}
	if s == nil {
		return errors.New("server config is nil")
	}
	s.Name = strings.TrimSpace(s.Name)
	s.Username = strings.TrimSpace(s.Username)
	s.Internal = strings.TrimSpace(s.Internal)
	s.External = strings.TrimSpace(s.External)
	s.Prefer = strings.ToLower(strings.TrimSpace(s.Prefer))
	if err := Validate(s); err != nil {
		return err
	}
	if c.Version == 0 {
		c.Version = ConfigVersion
	}
	if c.Servers == nil {
		c.Servers = make(map[string]*ServerConfig)
	}
	c.Servers[s.Name] = s
	if c.Default == "" {
		c.Default = s.Name
	}
	return nil
}

// Delete removes the server with the given alias. When it was the
// default, the alphabetically first remaining alias becomes the default
// (or Default is cleared when none remain).
//
// Errors: ErrServerNotFound (wrapped) when the alias is unknown.
func (c *AppConfig) Delete(name string) error {
	if c == nil {
		return fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if _, ok := c.Servers[name]; !ok {
		return fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	delete(c.Servers, name)
	if c.Default == name {
		c.Default = ""
		if names := c.Names(); len(names) > 0 {
			c.Default = names[0]
		}
	}
	return nil
}

// SetDefault marks an existing alias as the default server.
//
// Errors: ErrServerNotFound (wrapped) when the alias is unknown.
func (c *AppConfig) SetDefault(name string) error {
	if _, err := c.Get(name); err != nil {
		return err
	}
	c.Default = name
	return nil
}
