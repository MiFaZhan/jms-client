package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/connpool"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// execOptions carries the parsed `jms exec` flags.
type execOptions struct {
	account  string
	protocol string
	backend  string
	endpoint string
	timeout  int
	timing   bool
}

// newExecCommand builds `jms exec <target> <command...>`.
//
// Target syntax is <asset>[@<server>]. The command is passed through
// unprocessed so flags meant for the remote shell are not parsed locally.
func newExecCommand(deps *Deps) *cobra.Command {
	var opts execOptions
	cmd := &cobra.Command{
		Use:   "exec <target> <command...>",
		Short: "Run one command on an asset",
		Long: `Run one command on an asset and print its output.

The connection is taken from the pool, so a second command on the same asset
reuses the live connection instead of logging in again.

The remote command's arguments are passed through unprocessed, so quoting
is yours to do: jms exec host 'echo hello world'.

Credentials come from JMS_PASSWORD_<ALIAS> / JMS_OTP_<ALIAS> or from the OS
credential store. On a headless machine the credential store cannot be
unlocked, so set the environment variable instead.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExec(cmd, *deps, args[0], args[1:], opts)
		},
	}
	cmd.Flags().StringVar(&opts.account, "account", "", "Account override")
	cmd.Flags().StringVar(&opts.protocol, "protocol", "", "Protocol override")
	cmd.Flags().StringVar(&opts.backend, "backend", "", "Backend: ssh|ws|auto")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().IntVarP(&opts.timeout, "timeout", "t", 0, "Command timeout in seconds")
	cmd.Flags().BoolVar(&opts.timing, "timing", false, "Print exec stage timings")
	return cmd
}

// runExec resolves the target, runs the command through the pooled
// terminal and returns the remote exit code through exitError.
//
// stdout carries only the command output: the endpoint line and the
// non-zero-exit status line go to stderr, so
// `out=$(jms exec host cmd)` stays clean.
func runExec(cmd *cobra.Command, deps Deps, target string, command []string, opts execOptions) error {
	t, err := parseCommandTarget(target)
	if err != nil {
		return err
	}
	force, err := parseEndpointKind(opts.endpoint)
	if err != nil {
		return err
	}
	backend := transport.BackendWS
	if opts.backend != "" {
		backend, err = parseBackend(opts.backend)
		if err != nil {
			return err
		}
	}
	// The credential read happens inside resolveCommandTarget and is the
	// stage that can block on a locked keyring, so it gets its own
	// timing line: a run that stalls must be attributable to a stage.
	// The line is printed on the failure path too, because a run that
	// dies in this stage is exactly the run whose timing matters.
	credStarted := time.Now()
	conn, err := resolveCommandTarget(cmd, deps, t)
	if opts.timing {
		fmt.Fprintf(deps.Err, "Timing credentials: %s\n",
			time.Since(credStarted).Round(time.Millisecond))
	}
	if err != nil {
		return err
	}

	ctx := commandContext(cmd)

	// The stage timings exist because a stuck command must be
	// attributable: without them, a run that hangs in the credential
	// store looks exactly like one that hangs in the transport. The
	// credential read is timed separately because it is the stage that
	// can block on a locked keyring (DESIGN.md「配置与凭据分层」).
	started := time.Now()
	session, kind, err := conn.connect(ctx, force)
	if err != nil {
		return err
	}
	if opts.timing {
		fmt.Fprintf(deps.Err, "Timing endpoint/auth: %s\n",
			time.Since(started).Round(time.Millisecond))
	}

	stageStarted := time.Now()
	// Automated execution defaults to the WebSocket transport, matching
	// The asset is resolved inside the pool's dial: the name, account and
	// protocol are part of the terminal key, so a second exec with a
	// different account cannot land on the wrong pooled connection.
	res, err := deps.Runtime.Exec(ctx, conn.srv, session, t.Asset, strings.Join(command, " "),
		connpool.ExecOptions{
			Account:  opts.account,
			Protocol: opts.protocol,
			Backend:  transport.BackendType(backend),
			Timeout:  execTimeout(opts.timeout),
			OnBackend: func(b transport.BackendType) {
				conn.rememberBackend(kind, b)
			},
		})
	if opts.timing {
		fmt.Fprintf(deps.Err, "Timing command: %s\n",
			time.Since(stageStarted).Round(time.Millisecond))
	}
	if err != nil {
		return err
	}

	if res.Output != "" {
		fmt.Fprint(deps.Out, res.Output)
	}
	if res.ExitCode != 0 {
		// The status line is a diagnostic, not output: stdout stays pure
		// command output for the caller's $(...) capture.
		fmt.Fprintf(deps.Err, "Command exited with status %d\n", res.ExitCode)
		return &exitError{code: res.ExitCode}
	}
	return nil
}

// exitError reports a remote command's exit status as the process exit
// code. It is also reused by ssh-pipe.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("remote command exited with status %d", e.code)
}

// Unwrap keeps errors.Is working through the exit-code wrapper.
func (e *exitError) Unwrap() error { return e.err }

// ExitCode is what ExecuteArgs turns into the process exit code.
func (e *exitError) ExitCode() int { return e.code }
