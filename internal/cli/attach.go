package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/ipc"
	"github.com/MiFaZhan/jms-client/internal/obs"
)

// newAttachCommand builds `jms attach`: join a running host.
func newAttachCommand(deps *Deps) *cobra.Command {
	var (
		asset    string
		readonly bool
		last     int
		attach   bool
	)
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Attach to a running host: watch events and run commands",
		Long: `Watch a running host's event stream, optionally running commands yourself.

Observation is the default: a plain ` + "`jms attach`" + ` never accepts input, so
keeping a window open cannot turn an accidental keystroke into a command.
Pass --attach to enable the operator prompt.

Operator commands share the host's terminal pool, so a human command and an
AI command serialize on the same connection instead of racing for it
(DESIGN.md §12.4, §12.5).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttach(cmd, *deps, attachOptions{
				asset:    asset,
				readonly: readonly,
				last:     last,
				attach:   attach,
			})
		},
	}
	cmd.Flags().StringVar(&asset, "asset", "", "Only show this asset")
	cmd.Flags().BoolVar(&readonly, "readonly", false, "Observe only")
	cmd.Flags().IntVar(&last, "last", 0, "Replay the last N events first")
	cmd.Flags().BoolVar(&attach, "attach", false,
		"Enable the operator prompt (off by default: observation only)")
	return cmd
}

// attachOptions carries the parsed `jms attach` flags.
type attachOptions struct {
	asset    string
	readonly bool
	last     int
	attach   bool
}

// attachRole decides the role the client asks the host for.
//
// Observation is the default, and --readonly wins over --attach so that a
// contradictory pair cannot grant input by accident.
func attachRole(opts attachOptions) ipc.Role {
	if opts.attach && !opts.readonly {
		return ipc.RoleOperator
	}
	return ipc.RoleObserver
}

// runAttach connects to the host and renders the events it pushes.
func runAttach(cmd *cobra.Command, deps Deps, opts attachOptions) error {
	dir, err := stateDir(deps)
	if err != nil {
		return err
	}
	addr := ipc.EndpointName(dir)

	client := &ipc.Client{
		Role:        attachRole(opts),
		AssetFilter: opts.asset,
		Last:        opts.last,
		OnEvent: func(raw json.RawMessage) {
			out := deps.Out
			var e obs.Event
			if err := json.Unmarshal(raw, &e); err != nil {
				// An unreadable event is shown as-is rather than dropped:
				// the operator can still see that something happened.
				fmt.Fprintf(out, "%s\n", strings.TrimSpace(string(raw)))
				return
			}
			fmt.Fprintln(out, renderAuditEvent(e))
		},
	}

	ctx := commandContext(cmd)
	conn, err := deps.Runtime.Attach(ctx, addr, client)
	if err != nil {
		return fmt.Errorf("attach to %s: %w", addr, err)
	}
	defer conn.Close()

	// The session runs until the caller interrupts it with Ctrl-C, which
	// ExecuteArgs turns into context cancellation.
	<-ctx.Done()
	if err := ctx.Err(); errors.Is(err, context.Canceled) {
		// Ctrl-C is how an operator leaves an attach session; it is not a
		// failure and must not print an error.
		return nil
	}
	return ctx.Err()
}
