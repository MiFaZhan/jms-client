package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/config"
)

// withAddPrompts installs the prompt hooks for one `config add` run.
func (e *cliEnv) withAddPrompts(internal, external, username, password, confirmation, secret string) {
	e.promptLine = lineQueue(internal, external, username)
	e.promptSecret = lineQueue(password, confirmation, secret)
}

// withConfirmAnswer installs a confirmation hook that always answers yes
// or no, without reading Stdin.
func (e *cliEnv) withConfirmAnswer(answer bool) {
	e.confirmSet = true
	e.confirmAnswer = answer
}

func TestConfigAddStoresMetadataAndPasswordWithoutSecrets(t *testing.T) {
	const (
		password = "s3cr3t-pass"
		secret   = "JBSWY3DPEHPK3PXP"
	)
	env := newCLIEnv(t)
	env.withAddPrompts("http://internal.example:2280/", "http://external.example:2280/",
		"testuser", password, password, secret)

	if code := env.run("config", "add", "bastion"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}

	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	srv, err := cfg.Get("bastion")
	if err != nil {
		t.Fatalf("get saved server: %v", err)
	}
	if srv.Internal != "http://internal.example:2280/" || srv.External != "http://external.example:2280/" {
		t.Fatalf("saved addresses = %q / %q", srv.Internal, srv.External)
	}
	if srv.Username != "testuser" {
		t.Fatalf("saved username = %q", srv.Username)
	}

	got, err := env.creds.Get("bastion", config.CredPassword)
	if err != nil {
		t.Fatalf("password was not stored: %v", err)
	}
	if got != password {
		t.Fatalf("stored password = %q, want %q", got, password)
	}
	otp, err := env.creds.Get("bastion", config.CredOTP)
	if err != nil {
		t.Fatalf("TOTP secret was not stored: %v", err)
	}
	if otp != secret {
		t.Fatalf("stored TOTP secret = %q, want %q", otp, secret)
	}

	raw, err := os.ReadFile(env.cfgPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	file := string(raw)
	for _, forbidden := range []string{password, secret, "password", "otp_secret"} {
		if strings.Contains(file, forbidden) {
			t.Errorf("config file leaks %q:\n%s", forbidden, file)
		}
	}
	if err := config.CheckCredentialsRefused(cfg); err != nil {
		t.Errorf("config.CheckCredentialsRefused: %v", err)
	}
}

func TestConfigAddOmitsOTPEntryWhenNoSecretEntered(t *testing.T) {
	env := newCLIEnv(t)
	env.withAddPrompts("http://internal.example:2280/", "", "alice", "pw", "pw", "")

	if code := env.run("config", "add", "solo"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if _, err := env.creds.Get("solo", config.CredOTP); err == nil {
		t.Fatalf("an OTP credential was stored although no secret was entered")
	}
	if _, err := env.creds.Get("solo", config.CredPassword); err != nil {
		t.Fatalf("password was not stored: %v", err)
	}
}

func TestConfigAddRejectsInvalidInputBeforeWritingAnything(t *testing.T) {
	cases := []struct {
		name     string
		internal string
		external string
		username string
		wantErr  string
	}{
		{
			name:     "both addresses empty",
			internal: "",
			external: "",
			username: "alice",
			wantErr:  "at least one of internal/external URL is required",
		},
		{
			name:     "username empty",
			internal: "http://internal.example:2280/",
			external: "",
			username: "",
			wantErr:  "username is required",
		},
		{
			name:     "address is not http",
			internal: "ftp://internal.example:2280/",
			external: "",
			username: "alice",
			wantErr:  "must use http:// or https://",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCLIEnv(t)
			env.withAddPrompts(tc.internal, tc.external, tc.username, "pw", "pw", "")

			code := env.run("config", "add", "bad")
			if code == 0 {
				t.Fatalf("exit code = 0, want non-zero (stdout: %s)", env.stdout())
			}
			if !strings.Contains(env.stderr(), tc.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", env.stderr(), tc.wantErr)
			}
			env.requireNoFile()
			env.requireNoCredential("bad")
			if len(env.probed) != 0 {
				t.Fatalf("probed %v, want no probe before local validation passes", env.probed)
			}
			if len(env.loggedIn) != 0 {
				t.Fatalf("logged in to %v, want no login before local validation passes", env.loggedIn)
			}
		})
	}
}

func TestConfigAddFirstServerBecomesDefault(t *testing.T) {
	env := newCLIEnv(t)
	env.withAddPrompts("http://first.example:2280/", "", "alice", "pw", "pw", "")

	if code := env.run("config", "add", "first"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Default != "first" {
		t.Fatalf("default = %q, want %q", cfg.Default, "first")
	}
}

func TestConfigAddSetDefaultFlag(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("first", "http://first.example:2280/", "", "alice")
	env.setDefaultServer("first")

	env.withAddPrompts("http://second.example:2280/", "", "bob", "pw", "pw", "")
	if code := env.run("config", "add", "second", "--set-default"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}

	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Default != "second" {
		t.Fatalf("default = %q, want %q", cfg.Default, "second")
	}
}

func TestConfigAddRollsBackCredentialWhenMetadataWriteFails(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("existing", "http://existing.example:2280/", "", "alice")
	if err := env.creds.Set("existing", config.CredPassword, "previous"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	makeMetadataWriteFail(t, env.cfgPath)

	env.withAddPrompts("http://new.example:2280/", "", "bob", "pw", "pw", "")
	code := env.run("config", "add", "fresh")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when the metadata write fails")
	}

	env.requireNoCredential("fresh")
	if !strings.Contains(env.stderr(), "rolled back") {
		t.Fatalf("stderr = %q, want it to report the credential rollback", env.stderr())
	}
	// The pre-existing server's credential must survive untouched.
	kept, err := env.creds.Get("existing", config.CredPassword)
	if err != nil {
		t.Fatalf("rollback disturbed the pre-existing alias: %v", err)
	}
	if kept != "previous" {
		t.Fatalf("pre-existing password = %q, want %q", kept, "previous")
	}
	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := cfg.Get("existing"); err != nil {
		t.Fatalf("pre-existing server disappeared: %v", err)
	}
}

// makeMetadataWriteFail makes config.Save fail in a way that works on
// both Unix and Windows: the directory is made read-only (which blocks
// the temp file on Unix) and the file read-only (which blocks the rename
// on Windows).
//
// It skips the test when the environment cannot induce the failure (for
// example when the suite runs as root), because then the rollback path
// simply cannot be reached.
func makeMetadataWriteFail(t *testing.T, cfgPath string) {
	t.Helper()
	dir := filepath.Dir(cfgPath)
	restore := func() {
		_ = os.Chmod(dir, 0o700)
		_ = os.Chmod(cfgPath, 0o600)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make %s read-only: %v", dir, err)
	}
	if err := os.Chmod(cfgPath, 0o400); err != nil {
		restore()
		t.Skipf("cannot make %s read-only: %v", cfgPath, err)
	}
	t.Cleanup(restore)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		restore()
		t.Skipf("cannot reload %s while probing the failure: %v", cfgPath, err)
	}
	if err := config.Save(cfgPath, cfg); err == nil {
		restore()
		t.Skip("the environment still permits the metadata write (running as root?); rollback path unreachable")
	}
}

func TestConfigListRendersTableAndWarnsAboutMissingCredential(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://192.168.1.10:2280/", "http://bastion.example.com:2280/", "testuser")
	env.seedServer("nopass", "http://nopass.example:2280/", "", "bob")
	env.setDefaultServer("bastion")
	if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if code := env.run("config", "list"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}

	stdout := env.stdout()
	for _, want := range []string{
		"Alias", "Internal", "External", "Username", "Creds", "Default",
		"bastion", "http://192.168.1.10:2280/", "http://bastion.example.com:2280/", "testuser",
		"nopass", "keyring", "missing", "Total: 2 server(s)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	bastion := rowByFirstField(t, stdout, "bastion")
	if !strings.HasSuffix(bastion, "*") {
		t.Errorf("default marker missing on the default row: %q", bastion)
	}
	nopass := rowByFirstField(t, stdout, "nopass")
	if strings.HasSuffix(nopass, "*") {
		t.Errorf("non-default row is marked as default: %q", nopass)
	}
	if !strings.Contains(nopass, "missing") {
		t.Errorf("missing credential not reported on the row: %q", nopass)
	}
	if !strings.Contains(env.stderr(), "no stored password") {
		t.Errorf("stderr = %q, want a warning about the missing credential", env.stderr())
	}
}

func TestConfigListDistinguishesEnvFromKeyring(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("from-keyring", "http://keyring.example:2280/", "", "alice")
	env.seedServer("from-env", "http://env.example:2280/", "", "bob")
	if err := env.creds.Set("from-keyring", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	t.Setenv(mustEnvVarName(t, "from-env", config.CredPassword), "from-the-environment")

	if code := env.run("config", "list"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}

	keyringRow := rowByFirstField(t, env.stdout(), "from-keyring")
	if !strings.Contains(keyringRow, "keyring") {
		t.Errorf("keyring row = %q, want the keyring source marker", keyringRow)
	}
	envRow := rowByFirstField(t, env.stdout(), "from-env")
	if !strings.Contains(envRow, "env") {
		t.Errorf("env row = %q, want the env source marker", envRow)
	}
	if strings.Contains(envRow, "keyring") {
		t.Errorf("env row = %q, want the environment to outrank the keyring", envRow)
	}
}

func TestConfigListWithNoServersFailsClearly(t *testing.T) {
	env := newCLIEnv(t)
	code := env.run("config", "list")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when no config exists")
	}
	if !strings.Contains(env.stderr(), "config file not found") {
		t.Fatalf("stderr = %q, want a clear not-found message", env.stderr())
	}
}

func TestConfigRemoveDeletesMetadataAndCredential(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("keep", "http://keep.example:2280/", "", "alice")
	env.seedServer("drop", "http://drop.example:2280/", "", "bob")
	env.setDefaultServer("keep")
	if err := env.creds.Set("drop", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if code := env.run("config", "remove", "drop", "-y"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}

	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := cfg.Get("drop"); err == nil {
		t.Fatalf("server %q is still configured", "drop")
	}
	if _, err := cfg.Get("keep"); err != nil {
		t.Fatalf("unrelated server disappeared: %v", err)
	}
	if _, err := env.creds.Get("drop", config.CredPassword); err == nil {
		t.Fatalf("credential for %q survived the removal", "drop")
	}
	if !strings.Contains(env.stdout(), "removed") {
		t.Fatalf("stdout = %q, want a removal confirmation", env.stdout())
	}
}

func TestConfigRemoveUnknownAliasFails(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("known", "http://known.example:2280/", "", "alice")

	code := env.run("config", "remove", "bogus", "-y")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unknown alias")
	}
	if !strings.Contains(env.stderr(), "server not found") {
		t.Fatalf("stderr = %q, want a not-found message", env.stderr())
	}
}

func TestConfigRemoveAsksForConfirmationWithoutYes(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("drop", "http://drop.example:2280/", "", "bob")

	env.withConfirmAnswer(false)
	if code := env.run("config", "remove", "drop"); code == 0 {
		t.Fatalf("exit code = 0, want non-zero when the user declines")
	}
	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := cfg.Get("drop"); err != nil {
		t.Fatalf("server was removed although the user declined: %v", err)
	}

	env.withConfirmAnswer(true)
	if code := env.run("config", "remove", "drop"); code != 0 {
		t.Fatalf("exit code = %d, want 0 when the user confirms (stderr: %s)", code, env.stderr())
	}
	if cfg, err = config.Load(env.cfgPath); err == nil {
		if _, err := cfg.Get("drop"); err == nil {
			t.Fatalf("server survived a confirmed removal")
		}
	}
}

// TestConfigRemoveLastServerDeletesTheFile pins the outcome of removing
// the only server: Save rejects a serverless config (Load would refuse to
// read it back), so the file is deleted and the credential is cleared -
// the user is back to the pre-`config add` state, not stranded.
func TestConfigRemoveLastServerDeletesTheFile(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("only", "http://only.example:2280/", "", "bob")
	if err := env.creds.Set("only", config.CredPassword, "pw"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if code := env.run("config", "remove", "only", "-y"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if _, err := os.Stat(env.cfgPath); !os.IsNotExist(err) {
		t.Fatalf("config file still exists after removing the last server (err = %v)", err)
	}
	if _, err := env.creds.Get("only", config.CredPassword); !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("credential survived the removal: %v", err)
	}
	// The user must be able to start over.
	env.withAddPrompts("http://again.example:2280/", "", "carol", "pw", "pw", "")
	if code := env.run("config", "add", "again"); code != 0 {
		t.Fatalf("config add after removing the last server = %d, want 0 (stderr: %s)",
			code, env.stderr())
	}
}

func TestConfigSetDefaultSwitchesTheDefault(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("prod", "http://prod.example:2280/", "", "alice")
	env.seedServer("dev", "http://dev.example:2280/", "", "bob")
	env.setDefaultServer("prod")

	if code := env.run("config", "set-default", "dev"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	cfg, err := config.Load(env.cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Default != "dev" {
		t.Fatalf("default = %q, want %q", cfg.Default, "dev")
	}
}

func TestConfigSetDefaultUnknownAliasFails(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("prod", "http://prod.example:2280/", "", "alice")

	code := env.run("config", "set-default", "bogus")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unknown alias")
	}
	if !strings.Contains(env.stderr(), "server not found") {
		t.Fatalf("stderr = %q, want a not-found message", env.stderr())
	}
}

func TestConfigTestReportsUnreachableInternalAndReachableExternal(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://internal.example:2280/", "http://external.example:2280/", "testuser")
	if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	env.probeFn = func(_ context.Context, url string, _ time.Duration) bool {
		env.probed = append(env.probed, url)
		return strings.Contains(url, "external")
	}

	if code := env.run("config", "test"); code != 0 {
		t.Fatalf("exit code = %d, want 0 when one address is reachable (stderr: %s)", code, env.stderr())
	}

	internal := rowByFirstField(t, env.stdout(), "internal")
	if !strings.Contains(internal, "no") {
		t.Errorf("internal row = %q, want it reported unreachable", internal)
	}
	external := rowByFirstField(t, env.stdout(), "external")
	for _, want := range []string{"yes", "ok"} {
		if !strings.Contains(external, want) {
			t.Errorf("external row = %q, want %q", external, want)
		}
	}
	if len(env.loggedIn) != 1 || !strings.Contains(env.loggedIn[0], "external") {
		t.Fatalf("logged in to %v, want exactly the reachable external address", env.loggedIn)
	}
	if !strings.Contains(env.stdout(), "Credentials: keyring") {
		t.Errorf("stdout = %q, want the credential source line", env.stdout())
	}
}

func TestConfigTestFailsWhenNothingIsReachable(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://internal.example:2280/", "http://external.example:2280/", "testuser")
	if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	env.probeFn = func(_ context.Context, url string, _ time.Duration) bool {
		env.probed = append(env.probed, url)
		return false
	}

	code := env.run("config", "test")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when no address is reachable")
	}
	if !strings.Contains(env.stderr(), "unreachable") {
		t.Fatalf("stderr = %q, want an unreachable report", env.stderr())
	}
	if len(env.loggedIn) != 0 {
		t.Fatalf("logged in to %v, want no login attempt when nothing is reachable", env.loggedIn)
	}
}

func TestConfigTestDistinguishesAuthFailureFromUnreachable(t *testing.T) {
	t.Run("auth failure is reported as a credential problem", func(t *testing.T) {
		env := newCLIEnv(t)
		env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")
		if err := env.creds.Set("bastion", config.CredPassword, "wrong"); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		env.loginErr = &api.AuthError{StatusCode: 401, Body: "invalid credentials"}

		code := env.run("config", "test", "bastion")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero for a rejected credential")
		}
		stderr := env.stderr()
		if !strings.Contains(stderr, "rejected the stored credentials") {
			t.Fatalf("stderr = %q, want it to name the credential problem", stderr)
		}
		if strings.Contains(stderr, "unreachable") {
			t.Fatalf("stderr = %q, want a reachable-but-rejected report, not an unreachable one", stderr)
		}
		if !strings.Contains(env.stdout(), "auth failed") {
			t.Errorf("stdout = %q, want the login cell to say auth failed", env.stdout())
		}
	})

	t.Run("unreachable is not reported as a credential problem", func(t *testing.T) {
		env := newCLIEnv(t)
		env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")
		if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		env.probeFn = func(_ context.Context, url string, _ time.Duration) bool {
			env.probed = append(env.probed, url)
			return false
		}

		code := env.run("config", "test", "bastion")
		if code == 0 {
			t.Fatalf("exit code = 0, want non-zero when nothing is reachable")
		}
		stderr := env.stderr()
		if !strings.Contains(stderr, "unreachable") {
			t.Fatalf("stderr = %q, want an unreachable report", stderr)
		}
		if strings.Contains(stderr, "rejected the stored credentials") {
			t.Fatalf("stderr = %q, want no credential blame for a network failure", stderr)
		}
	})
}

func TestConfigTestUnknownAliasFails(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")

	code := env.run("config", "test", "bogus")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for an unknown alias")
	}
	if !strings.Contains(env.stderr(), "server not found") {
		t.Fatalf("stderr = %q, want a not-found message", env.stderr())
	}
}

// rowByFirstField returns the rendered table row whose first whitespace
// separated field equals name.
func rowByFirstField(t *testing.T, table, name string) string {
	t.Helper()
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return strings.TrimRight(line, " ")
		}
	}
	t.Fatalf("no row starting with %q in:\n%s", name, table)
	return ""
}

// mustEnvVarName wraps the two-value EnvVarName for tests that know the
// kind is valid.
func mustEnvVarName(t *testing.T, alias, kind string) string {
	t.Helper()
	name, err := config.EnvVarName(alias, kind)
	if err != nil {
		t.Fatalf("EnvVarName(%q, %q) = %v", alias, kind, err)
	}
	return name
}
