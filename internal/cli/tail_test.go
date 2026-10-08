package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/ipc"
	"github.com/MiFaZhan/jms-client/internal/mcpserver"
	"github.com/MiFaZhan/jms-client/internal/obs"
)

// seedAudit writes audit events into the environment's audit directory, in
// the per-pid file naming obs.AuditWriter uses.
func (e *cliEnv) seedAudit(t *testing.T, events ...obs.Event) string {
	t.Helper()
	dir := e.auditDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create audit dir: %v", err)
	}
	name := filepath.Join(dir, "audit-20260101-4242.jsonl")
	f, err := os.Create(name)
	if err != nil {
		t.Fatalf("create audit file: %v", err)
	}
	defer f.Close()
	for _, ev := range events {
		line, err := obs.MarshalLine(ev)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatalf("write event: %v", err)
		}
	}
	return dir
}

// auditDir is the audit directory the CLI under test will read.
func (e *cliEnv) auditDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(e.cfgPath), "audit")
}

// withAuditDir points the runtime's audit directory at the test's location.
func (e *cliEnv) withAuditDir(t *testing.T) {
	t.Helper()
	e.auditDirOverride = e.auditDir(t)
}

func auditEvent(ts time.Time, kind obs.Kind, asset, command string, exit int) obs.Event {
	code := exit
	return obs.Event{
		TS: ts, PID: 4242, Kind: kind, Server: "bastion", Asset: asset,
		Command: command, ExitCode: &code, DurationMS: 12, Actor: "mcp:pi",
	}
}

func TestTailLastNPrintsOldestFirst(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	env.seedAudit(t,
		auditEvent(base, obs.KindExecEnd, "a", "first", 0),
		auditEvent(base.Add(time.Second), obs.KindExecEnd, "b", "second", 0),
		auditEvent(base.Add(2*time.Second), obs.KindExecEnd, "c", "third", 0),
	)

	if code := env.run("tail", "--last", "2"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	out := env.stdout()
	if strings.Contains(out, "first") {
		t.Errorf("--last 2 included the oldest event:\n%s", out)
	}
	second := strings.Index(out, "second")
	third := strings.Index(out, "third")
	if second < 0 || third < 0 {
		t.Fatalf("missing events:\n%s", out)
	}
	if second > third {
		t.Errorf("events are not oldest-first:\n%s", out)
	}
}

func TestTailJSONEmitsParseableLines(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	env.seedAudit(t, auditEvent(base, obs.KindExecEnd, "web-01", "uptime", 0))

	if code := env.run("tail", "--json"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	for _, line := range strings.Split(strings.TrimSpace(env.stdout()), "\n") {
		if line == "" {
			continue
		}
		var e obs.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("--json emitted an unparseable line %q: %v", line, err)
		}
		if e.Asset != "web-01" {
			t.Fatalf("event asset = %q, want web-01", e.Asset)
		}
	}
}

func TestTailRendersEndpointAndPoolBadges(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	ev := auditEvent(base, obs.KindExecEnd, "web-01", "uptime", 0)
	ev.Endpoint = "external"
	ev.Pool = "cold"
	env.seedAudit(t, ev)

	if code := env.run("tail"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := env.stdout()
	// DESIGN.md「端点故障转移」第 6 点 and 「IPC 宿主」 ask for these markers.
	if !strings.Contains(out, "[外]") {
		t.Errorf("output lacks the external endpoint badge:\n%s", out)
	}
	if !strings.Contains(out, "⚡冷连") {
		t.Errorf("output lacks the cold-start marker:\n%s", out)
	}
}

func TestTailEmptyDirectoryIsNotAnError(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t) // directory does not exist yet
	if code := env.run("tail"); code != 0 {
		t.Fatalf("exit code = %d, want 0 when nothing has been recorded", code)
	}
}

func TestLogShowFailedFilters(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	env.seedAudit(t,
		auditEvent(base, obs.KindExecEnd, "good", "uptime", 0),
		auditEvent(base.Add(time.Second), obs.KindExecEnd, "bad", "false", 1),
	)

	if code := env.run("log", "show", "--failed"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	out := env.stdout()
	if !strings.Contains(out, "false") {
		t.Errorf("--failed dropped the failing command:\n%s", out)
	}
	if strings.Contains(out, "uptime") {
		t.Errorf("--failed kept the successful command:\n%s", out)
	}
}

func TestLogShowAssetFilters(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	env.seedAudit(t,
		auditEvent(base, obs.KindExecEnd, "web-01", "uptime", 0),
		auditEvent(base.Add(time.Second), obs.KindExecEnd, "db-01", "ls", 0),
	)

	if code := env.run("log", "show", "--asset", "web-01"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := env.stdout()
	if !strings.Contains(out, "uptime") || strings.Contains(out, "ls") {
		t.Errorf("--asset did not filter correctly:\n%s", out)
	}
}

func TestLogShowExportMarkdown(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	env.seedAudit(t, auditEvent(base, obs.KindExecEnd, "web-01", "uptime", 0))

	if code := env.run("log", "show", "--export", "md"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := env.stdout()
	if !strings.Contains(out, "| Time |") || !strings.Contains(out, "| --- |") {
		t.Errorf("--export md did not emit a Markdown table:\n%s", out)
	}
	if !strings.Contains(out, "uptime") {
		t.Errorf("Markdown table lacks the command:\n%s", out)
	}
}

func TestLogShowRejectsUnknownExport(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	if code := env.run("log", "show", "--export", "csv"); code == 0 {
		t.Fatal("exit code = 0, want a usage error naming the supported formats")
	}
	if !strings.Contains(env.stderr(), "md") {
		t.Errorf("stderr = %q, want it to name the supported format", env.stderr())
	}
}

func TestLogShowSinceFilters(t *testing.T) {
	env := newCLIEnv(t)
	env.withAuditDir(t)
	// The environment's clock is stepClock, which starts at a fixed instant in
	// 2024; --since is resolved against that clock, so the event must be dated
	// relative to the same base for the window to exclude it.
	old := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC).Add(-48 * time.Hour)
	env.seedAudit(t, auditEvent(old, obs.KindExecEnd, "ancient", "old-cmd", 0))

	if code := env.run("log", "show", "--since", "1h"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if strings.Contains(env.stdout(), "old-cmd") {
		t.Errorf("--since 1h kept a 48h-old event:\n%s", env.stdout())
	}
}

func TestAttachRendersEventsInDocumentedShape(t *testing.T) {
	env := newCLIEnv(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	ev := auditEvent(base, obs.KindExecEnd, "web-01", "systemctl status nginx", 0)
	ev.Endpoint = "external"
	ev.Pool = "hit"

	var gotRole ipc.Role
	env.attachFn = func(ctx context.Context, _ string, client *ipc.Client) (*ipc.Conn, error) {
		gotRole = client.Role
		raw, err := json.Marshal(ev)
		if err != nil {
			return nil, err
		}
		client.OnEvent(raw)
		// Cancel the way an operator's Ctrl-C would, so the blocking command
		// returns instead of waiting forever.
		if cancel := env.cancel; cancel != nil {
			cancel()
		}
		return &ipc.Conn{}, nil
	}
	env.withCancel()

	if code := env.run("attach"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	out := env.stdout()
	for _, want := range []string{"systemctl status nginx", "[外]", "♻命中", "web-01"} {
		if !strings.Contains(out, want) {
			t.Errorf("attach output lacks %q:\n%s", want, out)
		}
	}
	if gotRole != ipc.RoleObserver {
		t.Fatalf("plain attach asked for role %q, want observer", gotRole)
	}
}

func TestAttachAssetFilterIsPassedToTheHost(t *testing.T) {
	env := newCLIEnv(t)
	var gotFilter string
	env.attachFn = func(_ context.Context, _ string, client *ipc.Client) (*ipc.Conn, error) {
		gotFilter = client.AssetFilter
		if cancel := env.cancel; cancel != nil {
			cancel()
		}
		return &ipc.Conn{}, nil
	}
	env.withCancel()
	if code := env.run("attach", "--asset", "web-01"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotFilter != "web-01" {
		t.Fatalf("asset filter = %q, want web-01", gotFilter)
	}
}

// TestAttachRoleIsObservationByDefault pins the DESIGN.md「IPC 宿主」 safety property:
// a plain attach, or a contradictory pair, never grants input.
func TestAttachRoleIsObservationByDefault(t *testing.T) {
	cases := []struct {
		name     string
		opts     attachOptions
		wantRole ipc.Role
	}{
		{name: "default", opts: attachOptions{}, wantRole: ipc.RoleObserver},
		{name: "readonly", opts: attachOptions{readonly: true}, wantRole: ipc.RoleObserver},
		{name: "attach enables operator", opts: attachOptions{attach: true}, wantRole: ipc.RoleOperator},
		{name: "readonly beats attach", opts: attachOptions{attach: true, readonly: true}, wantRole: ipc.RoleObserver},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachRole(tc.opts); got != tc.wantRole {
				t.Fatalf("attachRole = %q, want %q", got, tc.wantRole)
			}
		})
	}
}

func TestAttachReportsAHostThatIsNotRunning(t *testing.T) {
	env := newCLIEnv(t)
	env.attachFn = func(context.Context, string, *ipc.Client) (*ipc.Conn, error) {
		return nil, os.ErrNotExist
	}
	if code := env.run("attach"); code == 0 {
		t.Fatal("exit code = 0, want non-zero when no host is listening")
	}
	if !strings.Contains(env.stderr(), "attach") {
		t.Errorf("stderr = %q, want it to name the failed operation", env.stderr())
	}
}

func TestMCPPrintConfigNamesTheCommandAndCarriesNoCredential(t *testing.T) {
	env := newCLIEnv(t)
	if code := env.run("mcp", "--print-config"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	out := env.stdout()
	if !strings.Contains(out, "jms") || !strings.Contains(out, "mcp") {
		t.Errorf("--print-config does not name the command:\n%s", out)
	}
	// DESIGN.md「MCP 客户端集成」: the entry is shareable, so it must carry nothing secret.
	for _, forbidden := range []string{"password", "secret", "token", "otp"} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("--print-config contains %q:\n%s", forbidden, out)
		}
	}
}

func TestMCPRunsTheInjectedServerSeam(t *testing.T) {
	env := newCLIEnv(t)
	var called bool
	env.serveMCPFn = func(_ context.Context, opts mcpserver.Options) error {
		called = true
		return nil
	}
	if code := env.run("mcp"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if !called {
		t.Fatal("the MCP server seam was not invoked")
	}
}

func TestParseSinceRejectsGarbage(t *testing.T) {
	if _, err := parseSince("last tuesday"); err == nil {
		t.Fatal("parseSince(garbage) = nil error, want one")
	}
	if d, err := parseSince("30m"); err != nil || d != 30*time.Minute {
		t.Fatalf("parseSince(30m) = (%v, %v)", d, err)
	}
}
