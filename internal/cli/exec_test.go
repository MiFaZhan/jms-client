package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/transport"
)

func TestExecRunsInjectedSeamAndPrintsOutputToStdout(t *testing.T) {
	env, fakes := newRuntimeEnv(t)
	fakes.execRes = transport.Result{Output: "hello from remote\n", ExitCode: 0}

	code := fakes.runWithRuntime(env, "exec", "web1@bastion", "echo", "hello", "world")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if env.stdout() != "hello from remote\n" {
		t.Fatalf("stdout = %q, want the command output only", env.stdout())
	}

	if len(fakes.execCalls) != 1 {
		t.Fatalf("Exec called %d times, want 1", len(fakes.execCalls))
	}
	call := fakes.execCalls[0]
	if call.asset != "web1" {
		t.Errorf("asset = %q, want %q", call.asset, "web1")
	}
	if call.cmd != "echo hello world" {
		t.Errorf("cmd = %q, want %q", call.cmd, "echo hello world")
	}
	if call.server == nil || call.server.Name != "bastion" {
		t.Errorf("server = %+v, want bastion", call.server)
	}
	if call.sess == nil {
		t.Errorf("session = nil, want the session the login seam produced")
	}
}

func TestExecPropagatesRemoteExitCodeAsProcessExitCode(t *testing.T) {
	env, fakes := newRuntimeEnv(t)
	fakes.execRes = transport.Result{Output: "partial output\n", ExitCode: 7}

	code := fakes.runWithRuntime(env, "exec", "web1", "false")
	if code != 7 {
		t.Fatalf("exit code = %d, want the remote status 7", code)
	}
	// stdout stays pure command output; the status note is a diagnostic.
	if env.stdout() != "partial output\n" {
		t.Fatalf("stdout = %q, want only the command output", env.stdout())
	}
	if !strings.Contains(env.stderr(), "status 7") {
		t.Fatalf("stderr = %q, want a status note for the caller", env.stderr())
	}
	// ExecuteArgs stays silent for ExitCoder errors: no double rendering.
	if strings.Contains(env.stderr(), "Error:") {
		t.Fatalf("stderr = %q, want no generic Error: line", env.stderr())
	}
}

func TestExecDefaultsToWSBackend(t *testing.T) {
	env, fakes := newRuntimeEnv(t)

	code := fakes.runWithRuntime(env, "exec", "web1", "uptime")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if got := fakes.execCalls[0].opts.Backend; got != transport.BackendWS {
		t.Fatalf("Backend = %q, want %q", got, transport.BackendWS)
	}
}

func TestExecPassesAccountProtocolBackendThrough(t *testing.T) {
	env, fakes := newRuntimeEnv(t)

	code := fakes.runWithRuntime(env, "exec", "--account", "deploy", "--protocol", "ssh",
		"--backend", "ws", "web1", "uptime")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	opts := fakes.execCalls[0].opts
	if opts.Account != "deploy" {
		t.Errorf("Account = %q, want deploy", opts.Account)
	}
	if opts.Protocol != "ssh" {
		t.Errorf("Protocol = %q, want ssh", opts.Protocol)
	}
	if opts.Backend != transport.BackendWS {
		t.Errorf("Backend = %q, want ws", opts.Backend)
	}
}

func TestExecTimeoutFlagReachesSeam(t *testing.T) {
	env, fakes := newRuntimeEnv(t)

	code := fakes.runWithRuntime(env, "exec", "-t", "90", "web1", "longjob")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if got := fakes.execCalls[0].opts.Timeout; got != 90*time.Second {
		t.Errorf("Timeout = %s, want 90s", got)
	}
}

func TestExecUnknownAssetFailsWithActionableMessage(t *testing.T) {
	env, _ := newRuntimeEnv(t)
	env.clientURL = newAssetServer(t, []map[string]any{}, nil).URL

	// The default Runtime seam is the real terminal pool, so the asset
	// lookup runs against the httptest server: an empty listing must fail
	// the command before any terminal is built.
	code := ExecuteArgs([]string{"exec", "no-such-asset", "whoami"}, env.deps())
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	msg := env.stderr()
	if !strings.Contains(msg, "no-such-asset") {
		t.Fatalf("stderr = %q, want it to name the missing asset", msg)
	}
	if !strings.Contains(msg, "no asset matches") {
		t.Fatalf("stderr = %q, want the wrapped library detail", msg)
	}
}

func TestExecUnknownServerFailsWithGuidance(t *testing.T) {
	env, _ := newRuntimeEnv(t)

	code := ExecuteArgs([]string{"exec", "web1@nosuch", "whoami"}, env.deps())
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(env.stderr(), "nosuch") {
		t.Fatalf("stderr = %q, want it to name the unknown server", env.stderr())
	}
}

func TestExecBackendFlagRejectsGarbage(t *testing.T) {
	env, fakes := newRuntimeEnv(t)

	code := fakes.runWithRuntime(env, "exec", "--backend", "telnet", "web1", "whoami")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(env.stderr(), "--backend") {
		t.Fatalf("stderr = %q, want it to blame --backend", env.stderr())
	}
}
