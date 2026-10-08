package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
)

// newLSCommand builds `jms ls`.
func newLSCommand(deps *Deps) *cobra.Command {
	var (
		keyword  string
		limit    int
		endpoint string
	)
	cmd := &cobra.Command{
		Use:   "ls [server]",
		Short: "List authorized assets",
		Long: `List the assets you are authorized to reach.

The endpoint is chosen with the internal/external failover policy, or
forced with --endpoint. The chosen endpoint is reported on stderr so that
stdout stays a clean table.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server := ""
			if len(args) == 1 {
				server = args[0]
			}
			return runLS(cmd, *deps, lsOptions{
				server:   server,
				keyword:  keyword,
				limit:    limit,
				endpoint: endpoint,
			})
		},
	}
	cmd.Flags().StringVarP(&keyword, "search", "q", "", "Search keyword")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "Maximum number of results")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	return cmd
}

// lsOptions carries the parsed `jms ls` flags.
type lsOptions struct {
	server   string
	keyword  string
	limit    int
	endpoint string
}

// runLS resolves the server and the endpoint, lists or searches the
// authorized assets, and prints the table.
//
// The endpoint resolution and the listing share one session: the address
// the login actually used is the address the API calls go to, which is
// what DESIGN.md §4.7 means by binding the endpoint to the session.
func runLS(cmd *cobra.Command, deps Deps, opts lsOptions) error {
	force, err := parseEndpointKind(opts.endpoint)
	if err != nil {
		return err
	}

	cfg, err := loadConfigFor(cmd, deps)
	if err != nil {
		return err
	}
	srv, err := resolveServer(cfg, opts.server)
	if err != nil {
		return err
	}

	password, secret, err := loadCredentials(deps, srv.Name)
	if err != nil {
		return err
	}

	ctx := commandContext(cmd)
	session, sel, err := selectEndpoint(ctx, deps, newPrompter(deps), cfg, srv, force, password, secret)
	if err != nil {
		return describeEndpointFailure(srv, force, err)
	}

	fmt.Fprintf(deps.Err, "Using %s endpoint %s\n", sel.Candidate.Kind, sel.Candidate.URL)

	listed, err := listAssets(ctx, session.Client, opts)
	if err != nil {
		return err
	}
	if len(listed) == 0 {
		fmt.Fprintln(deps.Out, "No assets found.")
		return nil
	}
	printAssetTable(deps.Out, listed)
	return nil
}

// describeEndpointFailure turns a selection failure into an actionable
// message.
//
// A forced endpoint that is not configured is a user mistake, not a
// network problem, and the wording must make clear that no fallback
// happened — silently using the other address would hide a broken
// internal route.
func describeEndpointFailure(srv *config.ServerConfig, force endpoint.Kind, err error) error {
	if force != "" {
		if _, ok := srv.URLFor(string(force)); !ok {
			return fmt.Errorf("server %q has no %s address configured; refusing to fall back to the other address",
				srv.Name, force)
		}
	}
	return err
}

// listAssets performs the search or the limited listing.
func listAssets(ctx context.Context, client *api.Client, opts lsOptions) ([]assets.Asset, error) {
	if opts.keyword != "" {
		return assets.Search(ctx, client, opts.keyword)
	}
	return assets.List(ctx, client, opts.limit)
}

// printAssetTable renders the asset table.
func printAssetTable(out io.Writer, listed []assets.Asset) {
	table := newTable(out)
	table.setHeader("Name", "Address", "Platform", "Type")
	for _, a := range listed {
		table.addRow(a.Name, a.Address, a.Platform, a.Type)
	}
	table.setTrailer(fmt.Sprintf("\nTotal: %d asset(s)", len(listed)))
	_ = table.render()
}
