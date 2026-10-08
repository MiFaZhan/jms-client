package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/MiFaZhan/jms-client/internal/atomicfile"
)

// EnvConfig overrides the configuration file path.
const EnvConfig = "JMS_CONFIG"

// appDirName is the directory under the OS config root.
const appDirName = "jms"

// configFileName is the configuration file name inside ConfigDir.
const configFileName = "config.toml"

// legacyConfigFileName is the pre-rewrite (Python jms-cli) file name.
const legacyConfigFileName = "config.yaml"

// DefaultPath returns the configuration file path.
//
// Resolution order:
//  1. the JMS_CONFIG environment variable, when set and non-empty
//  2. <os.UserConfigDir()>/jms/config.toml
//
// On Windows os.UserConfigDir is %AppData% (Roaming), on Unix it is
// $XDG_CONFIG_HOME or ~/.config.
func DefaultPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvConfig)); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine the user config directory: %w", err)
	}
	return filepath.Join(dir, appDirName, configFileName), nil
}

// ConfigDir returns the directory that holds config.toml, state.json and
// the audit directory. It is the directory of DefaultPath, so JMS_CONFIG
// relocates the whole directory.
func ConfigDir() (string, error) {
	p, err := DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// Load reads and validates the configuration file.
//
// An empty path means DefaultPath. A missing file yields
// ErrConfigNotFound, unless a pre-rewrite config.yaml is found next to it,
// in which case ErrLegacyConfig is returned (the rewrite deliberately does
// not import the legacy format; DESIGN.md「配置与凭据分层」).
func Load(path string) (*AppConfig, error) {
	if strings.TrimSpace(path) == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if legacy, ok := legacyConfigNextTo(path); ok {
				return nil, fmt.Errorf(
					"%w — unsupported by design; re-add with jms config add (found %s)",
					ErrLegacyConfig, legacy)
			}
			return nil, fmt.Errorf("%w: %s — run `jms config add <alias>`", ErrConfigNotFound, path)
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return parseConfig(path, data)
}

// Save writes the configuration file atomically (temp file in the same
// directory + fsync + rename) with permissions 0600 on Unix. The parent
// directory is created when missing.
//
// Every server is validated before anything is written, so a rejected
// entry never truncates or corrupts an existing file.
func Save(path string, cfg *AppConfig) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	if strings.TrimSpace(path) == "" {
		p, err := DefaultPath()
		if err != nil {
			return err
		}
		path = p
	}
	if len(cfg.Servers) == 0 {
		// Save and Load must be inverses: Load rejects a serverless file, so
		// writing one would strand the user - after removing the last server,
		// every later `jms config add` would fail to parse its own config.
		return fmt.Errorf("%w: refusing to write a configuration with no servers", ErrNoServers)
	}
	if cfg.Default != "" {
		if _, ok := cfg.Servers[cfg.Default]; !ok {
			return fmt.Errorf("default_server %q does not exist (available: %s)",
				cfg.Default, strings.Join(cfg.Names(), ", "))
		}
	}
	for _, name := range cfg.Names() {
		if err := Validate(cfg.Servers[name]); err != nil {
			return err
		}
	}
	data, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return atomicfile.WriteFile(path, data, 0o600)
}

// legacyConfigNextTo reports whether a pre-rewrite config.yaml file exists
// in the same directory as path. A directory of that name does not count.
func legacyConfigNextTo(path string) (string, bool) {
	candidate := filepath.Join(filepath.Dir(path), legacyConfigFileName)
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return "", false
	}
	return candidate, true
}
