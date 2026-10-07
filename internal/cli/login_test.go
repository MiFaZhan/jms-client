package cli

import (
	"strings"
	"testing"
)

func TestLoginUnknownAssetFailsBeforeInteractive(t *testing.T) {
	env, _ := newRuntimeEnv(t)
	env.clientURL = newAssetServer(t, []map[string]any{}, nil).URL

	// The interactive terminal is opened only after the asset resolves; an
	// empty listing must fail the command without ever touching a terminal.
	code := ExecuteArgs([]string{"login", "no-such-asset"}, env.deps())
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(env.stderr(), "no-such-asset") {
		t.Fatalf("stderr = %q, want it to name the missing asset", env.stderr())
	}
}

func TestLoginUnknownServerFailsWithGuidance(t *testing.T) {
	env, _ := newRuntimeEnv(t)

	code := ExecuteArgs([]string{"login", "web1@nosuch"}, env.deps())
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(env.stderr(), "nosuch") {
		t.Fatalf("stderr = %q, want it to name the unknown server", env.stderr())
	}
}

func TestLoginBackendFlagRejectsGarbage(t *testing.T) {
	env, _ := newRuntimeEnv(t)

	code := ExecuteArgs([]string{"login", "--backend", "telnet", "web1"}, env.deps())
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero")
	}
	if !strings.Contains(env.stderr(), "--backend") {
		t.Fatalf("stderr = %q, want it to blame --backend", env.stderr())
	}
}
