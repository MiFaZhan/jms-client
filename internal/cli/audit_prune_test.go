package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAuditFile puts one audit record in dir under the given dated name.
func writeAuditFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{"kind":"exec.end"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// writeRetentionConfig writes a minimal config carrying audit_keep_days.
func writeRetentionConfig(t *testing.T, env *cliEnv, days string) {
	t.Helper()
	body := "version = 2\ndefault_server = \"bastion\"\naudit_keep_days = " + days + "\n\n[servers.bastion]\ninternal = \"https://internal.example:2280\"\nusername = \"svc\"\n"
	if err := os.WriteFile(env.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestLogPruneDryRunReportsAndKeeps(t *testing.T) {
	env := newCLIEnv(t)
	dir := t.TempDir()
	env.auditDirOverride = dir
	t.Setenv("JMS_AUDIT_KEEP_DAYS", "")
	old := writeAuditFile(t, dir, "audit-20200101-1.jsonl")

	if code := env.run("log", "prune", "--keep", "90", "--dry-run"); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, env.stderr())
	}
	if !strings.Contains(env.stdout(), "would delete") {
		t.Fatalf("dry run did not report the deletion: %q", env.stdout())
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("dry run removed the file: %v", err)
	}
}

func TestLogPruneDeletesOldFiles(t *testing.T) {
	env := newCLIEnv(t)
	dir := t.TempDir()
	env.auditDirOverride = dir
	t.Setenv("JMS_AUDIT_KEEP_DAYS", "")
	old := writeAuditFile(t, dir, "audit-20200101-1.jsonl")
	recent := writeAuditFile(t, dir, "audit-20991231-2.jsonl")

	if code := env.run("log", "prune", "--keep", "90"); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, env.stderr())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("the old file survived a prune: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("a file inside the window was deleted: %v", err)
	}
}

func TestLogPruneWithoutWindowRefuses(t *testing.T) {
	env := newCLIEnv(t)
	env.auditDirOverride = t.TempDir()
	t.Setenv("JMS_AUDIT_KEEP_DAYS", "")
	old := writeAuditFile(t, env.auditDirOverride, "audit-20200101-1.jsonl")

	if code := env.run("log", "prune"); code == 0 {
		t.Fatalf("prune without a configured window succeeded; stderr: %s", env.stderr())
	}
	if !strings.Contains(env.stderr(), "no retention window configured") {
		t.Fatalf("stderr = %q, want an explanation", env.stderr())
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("a refused prune deleted a file: %v", err)
	}
}

// TestRetentionRunsOnStartup is the whole point of making this a
// configuration item: no timer, no cron — any jms process enforces the window.
func TestRetentionRunsOnStartup(t *testing.T) {
	env := newCLIEnv(t)
	dir := t.TempDir()
	env.auditDirOverride = dir
	t.Setenv("JMS_AUDIT_KEEP_DAYS", "")
	old := writeAuditFile(t, dir, "audit-20200101-1.jsonl")
	writeRetentionConfig(t, env, "90")

	if code := env.run("version"); code != 0 {
		t.Fatalf("version failed: %s", env.stderr())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("startup prune did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".prune-stamp")); err != nil {
		t.Errorf("the prune stamp was not written, so every command would rescan: %v", err)
	}
}

// TestRetentionOffByDefault guards the invariant that silence means "keep
// everything": an unconfigured machine must not lose audit records.
func TestRetentionOffByDefault(t *testing.T) {
	env := newCLIEnv(t)
	dir := t.TempDir()
	env.auditDirOverride = dir
	t.Setenv("JMS_AUDIT_KEEP_DAYS", "")
	old := writeAuditFile(t, dir, "audit-20200101-1.jsonl")

	if code := env.run("version"); code != 0 {
		t.Fatalf("version failed: %s", env.stderr())
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("an unconfigured machine deleted audit records: %v", err)
	}
}

// TestRetentionEnvOverridesConfig checks the precedence the help text
// promises.
func TestRetentionEnvOverridesConfig(t *testing.T) {
	env := newCLIEnv(t)
	dir := t.TempDir()
	env.auditDirOverride = dir
	writeRetentionConfig(t, env, "90")
	old := writeAuditFile(t, dir, "audit-20200101-1.jsonl")

	// A window of 36500 days keeps a 2020 file; the config's 90 would delete
	// it, so this distinguishes the two sources.
	t.Setenv("JMS_AUDIT_KEEP_DAYS", "36500")
	if code := env.run("version"); code != 0 {
		t.Fatalf("version failed: %s", env.stderr())
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("the environment override was not honoured: %v", err)
	}
}
