package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// tomlDoc mirrors the on-disk configuration file (DESIGN.md §7.2).
//
// It exists so that field names and rendering order are explicit and
// stable, and so the in-memory AppConfig stays free of encoding tags.
type tomlDoc struct {
	Version       float64              `toml:"version"`
	DefaultServer string               `toml:"default_server"`
	Proxy         string               `toml:"proxy,omitempty"`
	Servers       map[string]tomlEntry `toml:"servers"`
}

// tomlEntry is one [servers.<alias>] table. Only metadata lives here:
// there is no password or otp_secret field, by design.
type tomlEntry struct {
	Internal string `toml:"internal,omitempty"`
	External string `toml:"external,omitempty"`
	Username string `toml:"username"`
	Prefer   string `toml:"prefer,omitempty"`
	Pin      string `toml:"pin,omitempty"`
	Proxy    string `toml:"proxy,omitempty"`
	SSHPort  *int   `toml:"ssh_port,omitempty"`
}

// serversParentHeader is the table header the encoder emits above a map
// of tables. It is stripped so the rendered file matches DESIGN.md §7.2,
// which shows [servers.<alias>] tables directly under the root keys.
const serversParentHeader = "[servers]\n"

// optionalPort renders ssh_port so that an unset port is omitted: the
// zero value keeps meaningless `ssh_port = 0` lines out of the file.
func optionalPort(port int) *int {
	if port == 0 {
		return nil
	}
	return &port
}

// marshalConfig renders cfg as TOML in the shape documented in
// DESIGN.md §7.2. Servers are emitted in alias order so the file is
// stable and diff-friendly.
func marshalConfig(cfg *AppConfig) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	version := cfg.Version
	if version == 0 {
		version = ConfigVersion
	}

	servers := make(map[string]tomlEntry, len(cfg.Servers))
	for name, srv := range cfg.Servers {
		if srv == nil {
			continue
		}
		servers[name] = tomlEntry{
			Internal: srv.Internal,
			External: srv.External,
			Username: srv.Username,
			Prefer:   srv.Prefer,
			Pin:      srv.Pin,
			Proxy:    srv.Proxy,
			SSHPort:  optionalPort(srv.SSHPort),
		}
	}

	var buf bytes.Buffer
	// "2", not "2.0": the documented file uses an integer literal, and a
	// float encoding would render "2.0" here.
	buf.WriteString("version = " + strconv.FormatFloat(version, 'g', -1, 64) + "\n")

	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(struct {
		DefaultServer string               `toml:"default_server"`
		Proxy         string               `toml:"proxy,omitempty"`
		Servers       map[string]tomlEntry `toml:"servers"`
	}{DefaultServer: cfg.Default, Proxy: strings.TrimSpace(cfg.Proxy), Servers: servers}); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}

	text := buf.String()
	// The encoder emits a bare [servers] header before the sub-tables;
	// DESIGN.md §7.2 shows only the [servers.<alias>] headers, so drop it.
	text = strings.Replace(text, serversParentHeader, "", 1)
	return []byte(text), nil
}

// parseConfig parses and validates TOML configuration bytes.
func parseConfig(path string, data []byte) (*AppConfig, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("config file is empty: %s", path)
	}
	var doc tomlDoc
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	cfg := &AppConfig{
		Version: doc.Version,
		Default: strings.TrimSpace(doc.DefaultServer),
		Proxy:   strings.TrimSpace(doc.Proxy),
		Servers: make(map[string]*ServerConfig, len(doc.Servers)),
	}
	if cfg.Version == 0 {
		cfg.Version = ConfigVersion
	}
	if cfg.Proxy != "" {
		if err := validateProxy(cfg.Proxy); err != nil {
			return nil, fmt.Errorf("invalid config %s: %w", path, err)
		}
	}

	names := make([]string, 0, len(doc.Servers))
	for name := range doc.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := doc.Servers[name]
		port := 0
		if entry.SSHPort != nil {
			port = *entry.SSHPort
		}
		srv := &ServerConfig{
			Name:     name,
			Internal: strings.TrimSpace(entry.Internal),
			External: strings.TrimSpace(entry.External),
			Username: strings.TrimSpace(entry.Username),
			Prefer:   strings.ToLower(strings.TrimSpace(entry.Prefer)),
			Pin:      strings.ToLower(strings.TrimSpace(entry.Pin)),
			Proxy:    strings.TrimSpace(entry.Proxy),
			SSHPort:  port,
		}
		if err := Validate(srv); err != nil {
			return nil, fmt.Errorf("invalid config %s: %w", path, err)
		}
		cfg.Servers[name] = srv
	}

	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("%w: %s contains no servers — run `jms config add <alias>`",
			ErrNoServers, path)
	}
	if cfg.Default != "" {
		if _, ok := cfg.Servers[cfg.Default]; !ok {
			return nil, fmt.Errorf("invalid config %s: default_server %q does not exist (available: %s)",
				path, cfg.Default, strings.Join(cfg.Names(), ", "))
		}
	}
	return cfg, nil
}

// CheckCredentialsRefused is a guard used by tests and by reviewers: it
// reports an error when the rendered configuration contains any of the
// forbidden credential markers. The configuration file must never carry
// a secret, encrypted or not (DESIGN.md §7.4).
// credentialFieldRe matches a credential-bearing schema field at key
// position. A bare substring match would raise a false alarm for a
// legitimate alias or username that merely contains the word - the canary
// must train reviewers to trust it.
var credentialFieldRe = regexp.MustCompile(`(?m)^\s*(password|otp_secret|secret)\s*=`)

func CheckCredentialsRefused(cfg *AppConfig) error {
	data, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	if credentialFieldRe.Match(data) {
		return errors.New("rendered config declares a credential field")
	}
	if bytes.Contains(data, []byte("enc:v1:")) {
		return errors.New("rendered config contains an encrypted-credential marker")
	}
	return nil
}
