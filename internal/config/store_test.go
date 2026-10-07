package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPopulatedConfig returns a config with two servers, the second one
// exercising every optional field.
func newPopulatedConfig(t *testing.T) *AppConfig {
	t.Helper()
	cfg := &AppConfig{}
	if err := cfg.Put(&ServerConfig{
		Name:     "bastion",
		Internal: "http://192.168.1.10:2280/",
		External: "http://bastion.example.com:2280/",
		Username: "testuser",
	}); err != nil {
		t.Fatalf("Put(bastion) = %v", err)
	}
	if err := cfg.Put(&ServerConfig{
		Name:     "alpha",
		Internal: "https://alpha.example.com:2280/",
		Username: "ops",
		Prefer:   PreferExternal,
		SSHPort:  2200,
	}); err != nil {
		t.Fatalf("Put(alpha) = %v", err)
	}
	return cfg
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := newPopulatedConfig(t)

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() = %v, want nil", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if got.Version != ConfigVersion {
		t.Errorf("Version = %v, want %v", got.Version, ConfigVersion)
	}
	if got.Default != "bastion" {
		t.Errorf("Default = %q, want bastion", got.Default)
	}
	if names := got.Names(); len(names) != 2 || names[0] != "alpha" || names[1] != "bastion" {
		t.Errorf("Names() = %v, want [alpha bastion]", names)
	}

	bastion, err := got.Get("bastion")
	if err != nil {
		t.Fatalf("Get(bastion) = %v", err)
	}
	if bastion.Internal != "http://192.168.1.10:2280/" {
		t.Errorf("bastion.Internal = %q", bastion.Internal)
	}
	if bastion.External != "http://bastion.example.com:2280/" {
		t.Errorf("bastion.External = %q", bastion.External)
	}
	if bastion.Username != "testuser" {
		t.Errorf("bastion.Username = %q", bastion.Username)
	}
	if bastion.Prefer != "" {
		t.Errorf("bastion.Prefer = %q, want empty", bastion.Prefer)
	}
	if bastion.SSHPort != 0 {
		t.Errorf("bastion.SSHPort = %d, want 0", bastion.SSHPort)
	}

	alpha, err := got.Get("alpha")
	if err != nil {
		t.Fatalf("Get(alpha) = %v", err)
	}
	if alpha.Prefer != PreferExternal {
		t.Errorf("alpha.Prefer = %q, want %q", alpha.Prefer, PreferExternal)
	}
	if alpha.SSHPort != 2200 {
		t.Errorf("alpha.SSHPort = %d, want 2200", alpha.SSHPort)
	}
}

func TestSaveRendersDocumentedShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := &AppConfig{}
	if err := cfg.Put(&ServerConfig{
		Name:     "bastion",
		Internal: "http://192.168.1.10:2280/",
		External: "http://bastion.example.com:2280/",
		Username: "testuser",
	}); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	want := "version = 2\n" +
		"default_server = \"bastion\"\n" +
		"\n" +
		"[servers.bastion]\n" +
		"internal = \"http://192.168.1.10:2280/\"\n" +
		"external = \"http://bastion.example.com:2280/\"\n" +
		"username = \"testuser\"\n"
	if string(data) != want {
		t.Fatalf("rendered file:\n%s\nwant:\n%s", data, want)
	}
}

func TestSaveOmitsUnsetOptionalFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := newPopulatedConfig(t)
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}
	rendered := string(data)
	for _, forbidden := range []string{"password", "secret", "otp_secret", "enc:v1:", "ssh_port = 0", `prefer = ""`} {
		if strings.Contains(strings.ToLower(rendered), forbidden) {
			t.Errorf("rendered config contains %q:\n%s", forbidden, rendered)
		}
	}
	// The explicitly configured port must still be present.
	if !strings.Contains(rendered, "ssh_port = 2200") {
		t.Errorf("rendered config lost the explicit ssh_port:\n%s", rendered)
	}
}

func TestLoadToleratesExtraneousCredentialFields(t *testing.T) {
	// The loader reads a fixed set of fields; a hand-written `password` line
	// is not a schema field and is ignored rather than being interpreted.
	// CheckCredentialsRefused is the guard that keeps the *writer* from ever
	// producing such a line (see TestCheckCredentialsRefusedIsACanary).
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "version = 2\n" +
		"[servers.a]\n" +
		"internal = \"https://a/\"\n" +
		"username = \"u\"\n" +
		"password = \"hunter2\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	srv, err := cfg.Get("a")
	if err != nil {
		t.Fatalf("Get(a) = %v", err)
	}
	if srv.Internal != "https://a/" || srv.Username != "u" {
		t.Fatalf("loaded server = %+v, want the metadata fields only", srv)
	}
}

func TestCheckCredentialsRefusedIsACanary(t *testing.T) {
	// CheckCredentialsRefused renders the config and scans for credential
	// markers. It must return nil today, and it fails the moment someone adds
	// a password/otp_secret field to the on-disk representation.
	cfg := newPopulatedConfig(t)
	if err := CheckCredentialsRefused(cfg); err != nil {
		t.Fatalf("CheckCredentialsRefused() = %v, want nil", err)
	}
	// It must also accept a zero config rather than panicking.
	if err := CheckCredentialsRefused(&AppConfig{}); err != nil {
		t.Fatalf("CheckCredentialsRefused(zero) = %v, want nil", err)
	}
}

func TestLoadRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "empty file", body: ""},
		{name: "whitespace only", body: "\n\n   \n"},
		{name: "invalid TOML", body: "version = = 2\n"},
		{name: "no servers at all", body: "version = 2\ndefault_server = \"a\"\n", wantErr: ErrNoServers},
		{name: "empty servers table", body: "version = 2\n[servers]\n", wantErr: ErrNoServers},
		{
			name: "neither address",
			body: "version = 2\n[servers.a]\nusername = \"u\"\n",
		},
		{
			name: "missing username",
			body: "version = 2\n[servers.a]\ninternal = \"https://a/\"\n",
		},
		{
			name: "unknown default_server",
			body: "version = 2\ndefault_server = \"ghost\"\n[servers.a]\ninternal = \"https://a/\"\nusername = \"u\"\n",
		},
		{
			name: "bad prefer value",
			body: "version = 2\n[servers.a]\ninternal = \"https://a/\"\nusername = \"u\"\nprefer = \"both\"\n",
		},
		{
			name: "non-http URL",
			body: "version = 2\n[servers.a]\ninternal = \"ftp://a/\"\nusername = \"u\"\n",
		},
		{
			name: "URL without a host",
			body: "version = 2\n[servers.a]\ninternal = \"http://\"\nusername = \"u\"\n",
		},
		{
			name: "ssh_port out of range",
			body: "version = 2\n[servers.a]\ninternal = \"https://a/\"\nusername = \"u\"\nssh_port = 65536\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("WriteFile() = %v", err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() = nil, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Load() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSaveAtomicNoTempResidue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(path, newPopulatedConfig(t)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveFailureLeavesPreviousFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(path, newPopulatedConfig(t)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}

	// A config holding an invalid server must be rejected before any write.
	broken := newPopulatedConfig(t)
	broken.Servers["broken"] = &ServerConfig{Name: "broken", Username: "u"}
	if err := Save(path, broken); err == nil {
		t.Fatal("Save() = nil, want a validation error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() after failed Save = %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("failed Save changed the file:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	assertNoTempFiles(t, dir)
}

func TestSaveNilConfig(t *testing.T) {
	dir := t.TempDir()
	if err := Save(filepath.Join(dir, "config.toml"), nil); err == nil {
		t.Fatal("Save(nil) = nil, want an error")
	}
}

func TestSaveCreatesMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "config.toml")
	if err := Save(path, newPopulatedConfig(t)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() = %v", err)
	}
}

func TestSaveModeIs0600(t *testing.T) {
	if isWindows() {
		// Windows has no POSIX permission bits; ACLs govern access there.
		t.Skip("POSIX permission bits are not available on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(path, newPopulatedConfig(t)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestLoadMissingReturnsErrConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "config.toml"))
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Load() = %v, want ErrConfigNotFound", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "jms config add") {
		t.Errorf("error message %q does not tell the user what to run", msg)
	}
}

func TestLoadDetectsLegacyYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("servers: {}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	_, err := Load(filepath.Join(dir, "config.toml"))
	if !errors.Is(err, ErrLegacyConfig) {
		t.Fatalf("Load() = %v, want ErrLegacyConfig", err)
	}
	msg := err.Error()
	for _, want := range []string{"legacy", "config add"} {
		if !strings.Contains(msg, want) {
			t.Errorf("legacy error %q does not mention %q", msg, want)
		}
	}
}

func TestLoadIgnoresLegacyYAMLWhenTOMLExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("servers: {}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	if err := Save(filepath.Join(dir, "config.toml"), newPopulatedConfig(t)); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if _, err := Load(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("Load() = %v, want the TOML file to win", err)
	}
}

func TestLoadEmptyPathUsesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv(EnvConfig, path)
	if err := Save("", newPopulatedConfig(t)); err != nil {
		t.Fatalf("Save(\"\") = %v", err)
	}
	got, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") = %v", err)
	}
	if got.Default != "bastion" {
		t.Fatalf("Default = %q, want bastion", got.Default)
	}
}

// assertNoTempFiles fails when dir holds a leftover atomic-write temp file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) = %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.Contains(name, ".tmp") {
			t.Errorf("temp file %q left behind in %s", name, dir)
		}
	}
}
