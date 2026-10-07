package cli

import (
	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/xfer"
)

// newSSHPipeCommand builds `jms ssh-pipe`, the stdio bridge rsync uses as
// `rsync -e "jms ssh-pipe"`.
//
// It forwards its arguments and stdio, so the caller's protocol bytes are
// never mixed with diagnostics: stdout carries the remote side of the
// rsync protocol, every diagnostic goes to stderr.
func newSSHPipeCommand(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:                "ssh-pipe <target> [command...]",
		Short:              "Bridge stdio to an asset (for rsync -e)",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSHPipe(cmd, *deps, args)
		},
	}
	return cmd
}

// runSSHPipe wires the process stdio into the bridge and maps the remote
// exit code through exitError.
//
// It writes nothing of its own to stdout: rsync speaks its protocol there,
// and one stray diagnostic line would corrupt the transfer (DESIGN.md §6,
// ssh-pipe bridge rule). The bridge itself reports its failures on stderr.
func runSSHPipe(cmd *cobra.Command, deps Deps, args []string) error {
	ctx := commandContext(cmd)
	code, err := deps.Runtime.Bridge(ctx, args, xfer.Bridge{
		Stdin:  deps.Stdin,
		Stdout: deps.Out,
		Stderr: deps.Err,
	})
	if err != nil {
		// RunBridge already rendered the failure on stderr; keep the
		// non-zero code the caller (rsync) expects without a second
		// "Error:" line.
		return &exitError{code: code, err: err}
	}
	if code != 0 {
		// A non-zero remote exit is the relay's normal outcome, not a
		// transport failure: surface it as the process exit code only.
		return &exitError{code: code}
	}
	return nil
}
