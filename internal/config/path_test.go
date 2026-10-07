package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isWindows reports whether the POSIX-only assertions can run.
func isWindows() bool {
	return runtime.GOOS == "windows"
}

func TestDefaultPathUsesUserConfigDir(t *testing.T) {
	t.Setenv(EnvConfig, "")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() = %v, want nil", err)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() = %v", err)
	}
	want := filepath.Join(root, "jms", "config.toml")
	if got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathHonoursEnvConfig(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "elsewhere", "jms.toml")
	t.Setenv(EnvConfig, custom)
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() = %v, want nil", err)
	}
	if got != custom {
		t.Fatalf("DefaultPath() = %q, want %q", got, custom)
	}
}

func TestDefaultPathIgnoresBlankEnvConfig(t *testing.T) {
	t.Setenv(EnvConfig, "   ")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() = %v, want nil", err)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() = %v", err)
	}
	if want := filepath.Join(root, "jms", "config.toml"); got != want {
		t.Fatalf("DefaultPath() = %q, want the platform default %q", got, want)
	}
}

func TestConfigDirFollowsEnvConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfig, filepath.Join(dir, "relocated", "config.toml"))
	got, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() = %v, want nil", err)
	}
	if want := filepath.Join(dir, "relocated"); got != want {
		t.Fatalf("ConfigDir() = %q, want %q", got, want)
	}
}

func TestConfigDirIsParentOfDefaultPath(t *testing.T) {
	t.Setenv(EnvConfig, "")
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() = %v, want nil", err)
	}
	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() = %v, want nil", err)
	}
	if want := filepath.Dir(path); dir != want {
		t.Fatalf("ConfigDir() = %q, want %q", dir, want)
	}
	if base := filepath.Base(dir); base != "jms" {
		t.Fatalf("ConfigDir() base = %q, want jms", base)
	}
}

func TestLegacyConfigNextToIgnoresDirectory(t *testing.T) {
	// A *directory* named config.yaml must not be reported as a legacy file.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config.yaml"), 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	_, err := Load(filepath.Join(dir, "config.toml"))
	if !strings.Contains(err.Error(), "config file not found") {
		t.Fatalf("Load() = %v, want ErrConfigNotFound", err)
	}
	if strings.Contains(err.Error(), "legacy") {
		t.Fatalf("Load() = %v, want no legacy detection for a directory", err)
	}
}
