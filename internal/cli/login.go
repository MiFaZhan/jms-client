package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// loginOptions carries the parsed `jms login` flags.
type loginOptions struct {
	account  string
	protocol string
	backend  string
	endpoint string
	timing   bool
}

// newLoginCommand builds `jms login <target>`, the interactive PTY.
//
// The interactive terminal is deliberately not pooled (DESIGN.md「总体架构」): the
// process lifetime is the session lifetime, so there is nothing to reuse.
func newLoginCommand(deps *Deps) *cobra.Command {
	var opts loginOptions
	cmd := &cobra.Command{
		Use:   "login <target>",
		Short: "Open an interactive shell on an asset",
		Long: `Open an interactive shell on an asset and attach the local console.

Blocks until the remote shell exits (Ctrl+] by default). The session is
bound to this process and is not pooled.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(cmd, *deps, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.account, "account", "", "Account override")
	cmd.Flags().StringVar(&opts.protocol, "protocol", "", "Protocol override")
	cmd.Flags().StringVar(&opts.backend, "backend", "", "Backend: ssh|ws|auto")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().BoolVar(&opts.timing, "timing", false, "Print login stage timings")
	return cmd
}

// runLogin connects to the target and hands the console to
// Terminal.Interactive, which blocks until the remote shell exits.
func runLogin(cmd *cobra.Command, deps Deps, target string, opts loginOptions) error {
	t, err := parseCommandTarget(target)
	if err != nil {
		return err
	}
	endpointValue := opts.endpoint
	if endpointValue == "" {
		// Interactive login follows the proven WebSocket path by default.
		// Automated commands keep their own backend policy below.
		endpointValue = string(endpoint.KindExternal)
	}
	force, err := parseEndpointKind(endpointValue)
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
	conn, err := resolveCommandTarget(cmd, deps, t)
	if err != nil {
		return err
	}

	ctx := commandContext(cmd)
	started := time.Now()

	stageStarted := time.Now()
	session, kind, err := conn.connect(ctx, force)
	if err != nil {
		return err
	}
	if opts.timing {
		fmt.Fprintf(deps.Err, "Timing endpoint/auth: %s\n", time.Since(stageStarted).Round(time.Millisecond))
	}

	stageStarted = time.Now()
	info, err := assets.Resolve(ctx, session.Client, t.Asset, opts.account, opts.protocol)
	if err != nil {
		return err
	}
	if opts.timing {
		fmt.Fprintf(deps.Err, "Timing asset resolve: %s\n", time.Since(stageStarted).Round(time.Millisecond))
	}

	if backend == transport.BackendAuto {
		if preferred := conn.preferredBackend(kind); preferred != "" {
			backend = preferred
		}
	}

	stageStarted = time.Now()
	term, err := transport.Connect(ctx, session, info, transport.ConnectOptions{
		Backend:  backend,
		SSHPort:  conn.srv.Port(),
		Protocol: opts.protocol,
	})
	if opts.timing {
		fmt.Fprintf(deps.Err, "Timing %s connect: %s\n", backend, time.Since(stageStarted).Round(time.Millisecond))
	}
	if err != nil {
		return err
	}
	conn.rememberBackend(kind, term.Backend())
	defer func() { _ = term.Close() }()

	fmt.Fprintf(deps.Err, "Connected to %s (%s backend). Press Ctrl+] to exit.\n",
		info.Name, term.Backend())
	if opts.timing {
		fmt.Fprintf(deps.Err, "Timing to interactive: %s\n", time.Since(started).Round(time.Millisecond))
	}
	return term.Interactive(ctx)
}
