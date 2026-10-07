package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/xfer"
)

func TestSSHPipeWritesOnlyBridgeStdoutToStdout(t *testing.T) {
	env, fakes := newRuntimeEnv(t)
	// The fake bridge writes its own protocol bytes to stdout and its own
	// diagnostics to stderr, exactly as the real relay would.
	fakes.bridgeFn = func(_ context.Context, _ []string, b xfer.Bridge) (int, error) {
		if _, err := b.Stdout.Write([]byte("rsync-protocol-bytes\n")); err != nil {
			return 1, err
		}
		if _, err := b.Stderr.Write([]byte("diagnostic: should stay off stdout\n")); err != nil {
			return 1, err
		}
		return 0, nil
	}

	code := fakes.runWithRuntime(env, "ssh-pipe", "-l", "web1", "bastion", "rsync", "--server", "--sender", ".")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if env.stdout() != "rsync-protocol-bytes\n" {
		t.Fatalf("stdout = %q, want only the bridge's protocol bytes", env.stdout())
	}
	if !strings.Contains(env.stderr(), "should stay off stdout") {
		t.Fatalf("stderr = %q, want the bridge's diagnostics", env.stderr())
	}
	if len(fakes.bridgeArgs) != 1 || fakes.bridgeArgs[0][0] != "-l" {
		t.Fatalf("bridge args = %v, want the raw rsync argv forwarded", fakes.bridgeArgs)
	}
}

func TestSSHPipeMapsBridgeExitCode(t *testing.T) {
	t.Run("non-zero remote exit becomes the process code", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)
		fakes.bridgeRes = 23

		code := fakes.runWithRuntime(env, "ssh-pipe", "web1", "rsync", "--server", ".")
		if code != 23 {
			t.Fatalf("exit code = %d, want the bridge's 23", code)
		}
	})

	t.Run("bridge failure keeps its rendered message and non-zero code", func(t *testing.T) {
		env, fakes := newRuntimeEnv(t)
		fakes.bridgeRes = 1
		fakes.bridgeErr = errors.New("unrecognized option \"--bogus\"")

		code := fakes.runWithRuntime(env, "ssh-pipe", "--bogus", "web1", "cmd")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if strings.Contains(env.stderr(), "Error:") {
			t.Fatalf("stderr = %q, want the bridge's own rendering without a second Error: line", env.stderr())
		}
	})
}

// TestSSHPipeStdoutCarriesProtocolBytesUnderFailure pins the invariant the
// whole command exists for: even a failing bridge must not add CLI chatter
// to stdout, because rsync parses that stream.
func TestSSHPipeStdoutCarriesProtocolBytesUnderFailure(t *testing.T) {
	env, fakes := newRuntimeEnv(t)
	fakes.bridgeRes = 1
	fakes.bridgeErr = errors.New("dial failed")

	code := fakes.runWithRuntime(env, "ssh-pipe", "web1", "cmd")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if env.stdout() != "" {
		t.Fatalf("stdout = %q, want empty under failure", env.stdout())
	}
}
