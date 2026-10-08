package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// flagConfig is the global configuration-file override.
const flagConfig = "config"

// Version is the release version. It is normally injected at build time
// with
//
//	go build -ldflags "-X github.com/MiFaZhan/jms-client/internal/cli.Version=1.2.3"
//
// but `go install github.com/MiFaZhan/jms-client/cmd/jms@latest` cannot
// pass ldflags, so the version falls back to the module version Go stamps
// into the binary (see effectiveVersion). The literal below is only the
// last resort, for a build that has neither.
var Version = "0.1.0-dev"

// effectiveVersion reports the version to display.
//
// Precedence: an injected Version (a release build or a local ldflags
// build), then the module version `go install` records in the build info,
// then the compiled-in default. Without the middle step every `go install`
// user would see "0.1.0-dev" and be unable to tell which release they
// actually have.
func effectiveVersion() string {
	if Version != "" && Version != devVersion {
		return Version
	}
	if v := moduleVersion(); v != "" {
		return v
	}
	return Version
}

// devVersion marks a build with no injected version.
const devVersion = "0.1.0-dev"

// moduleVersion reads the module version recorded by the Go toolchain.
//
// It returns "" for a plain `go build` from a working copy, where the
// toolchain records "(devel)" rather than a version.
func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return ""
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		return ""
	}
	return v
}

// Execute runs the jms command line and returns the process exit code.
//
// It is the single entry point used by cmd/jms. Errors are rendered as a
// single line on stderr; a non-zero exit code is returned for any
// failure.
func Execute() int {
	return ExecuteArgs(nil, Deps{})
}

// ExitCoder is an error that carries the process exit code the command
// wants.
//
// It exists because a remote command's exit status is meaningful to the
// caller: `jms exec host 'test -f /x' && ...` must see the remote status,
// not a generic 1. ExecuteArgs honours this interface before falling back
// to the generic failure code.
type ExitCoder interface {
	ExitCode() int
}

// ExecuteArgs runs the command line with an explicit argument list (nil
// means os.Args[1:]) and an explicit dependency set. It exists so tests
// can drive the full command surface with injected fakes.
//
// Cobra is put in silent mode so that this function owns the single place
// where a failure becomes "Error: <message>" on stderr plus a non-zero
// exit code: a library error must never surface as a usage dump, a panic
// or a raw traceback.
func ExecuteArgs(args []string, deps Deps) int {
	// A cancellable context tied to the interrupt signal, so the commands that
	// block by design (`attach`, `tail -f`) stop on Ctrl-C rather than
	// requiring the process to be killed. Cobra's default context is
	// context.Background(), which can never be cancelled.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return executeArgsContext(ctx, args, deps)
}

// executeArgsContext is ExecuteArgs with an explicit context, so tests can
// release a blocking command the way an interrupt does.
func executeArgsContext(ctx context.Context, args []string, deps Deps) int {
	deps = deps.WithDefaults()
	if args == nil {
		args = commandLineArgs()
	}
	root := NewRootCommand(deps)
	root.SetArgs(args)
	root.SetContext(ctx)

	code := runRoot(ctx, root, deps)

	// The audit log is flushed for every command, not just the long-lived
	// ones: delivery is asynchronous, so a one-shot `jms exec` would
	// otherwise exit before its own record reached the file.
	if err := deps.Runtime.Close(); err != nil {
		fmt.Fprintf(deps.Err, "Warning: audit log: %s\n", err)
	}
	return code
}

// runRoot executes the command tree and maps the result to an exit code.
func runRoot(ctx context.Context, root *cobra.Command, deps Deps) int {
	if err := root.ExecuteContext(ctx); err != nil {
		// A command that knows its exit status (a remote command's status,
		// say) owns its own diagnostics and reports the status directly.
		var coder ExitCoder
		if errors.As(err, &coder) {
			return coder.ExitCode()
		}
		fmt.Fprintf(deps.Err, "Error: %s\n", err)
		return 1
	}
	return 0
}

// NewRootCommand builds the cobra command tree.
//
// The full DESIGN.md「总体架构」 command surface is registered and implemented. An
// unrecognised subcommand fails with an explicit error rather than printing
// help and exiting 0, so a script chaining on one cannot mistake a typo for
// success.
func NewRootCommand(deps Deps) *cobra.Command {
	deps = deps.WithDefaults()

	root := &cobra.Command{
		Use:           "jms",
		Short:         "JumpServer v4 bastion asset access",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		// A root command with no Run is "not runnable", so cobra treats any
		// unrecognised subcommand as a help request: it prints the help and
		// returns nil. That made `jms login host` exit 0 without doing
		// anything, so `jms login host && next` would silently continue.
		// Naming the unknown command and failing is the honest outcome for a
		// subcommand this milestone does not implement yet.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return fmt.Errorf("unknown command %q for %q: run `jms --help` for the M1 command set",
				args[0], cmd.CommandPath())
		},
	}

	root.AddCommand(newVersionCommand())
	root.AddCommand(newConfigCommand(&deps))
	root.AddCommand(newLSCommand(&deps))
	root.AddCommand(newExecCommand(&deps))
	root.AddCommand(newLoginCommand(&deps))
	root.AddCommand(newSFTPCommand(&deps))
	root.AddCommand(newSSHPipeCommand(&deps))
	root.AddCommand(newTailCommand(&deps))
	root.AddCommand(newAttachCommand(&deps))
	root.AddCommand(newLogCommand(&deps))
	root.AddCommand(newMCPCommand(&deps))

	root.PersistentFlags().String(flagConfig, "",
		"Configuration file (overrides JMS_CONFIG; overridden by Deps.ConfigPath)")

	root.SetOut(deps.Out)
	root.SetErr(deps.Err)
	return root
}

const rootLong = `jms — JumpServer v4 bastion access.

Credentials are never written to the configuration file: they live in the
OS credential store (or in JMS_PASSWORD_<ALIAS> / JMS_OTP_<ALIAS>).

Target syntax:
    <asset>@<server>    asset on a named server
    <asset>             asset on the default server`

// newVersionCommand reports the build version.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "jms %s (%s/%s)\n",
				effectiveVersion(), runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}

// commandLineArgs returns the process arguments without the program name.
func commandLineArgs() []string {
	args := make([]string, 0, 8)
	for _, a := range processArgs() {
		if strings.TrimSpace(a) != "" {
			args = append(args, a)
		}
	}
	return args
}
