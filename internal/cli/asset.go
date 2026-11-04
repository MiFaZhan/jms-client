package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assetsmanage"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/permits"
)

// newAssetCommand builds `jms asset`, the management side of the
// asset collection: create, list, show, update and delete.
//
// It sits next to `jms ls` on purpose. `jms ls` answers "which assets may
// I connect to"; `jms asset list` answers "which assets exist on the
// server". The two views differ whenever the caller holds fewer
// permissions than the administrator, which is exactly the case that
// makes a difference while provisioning.
func newAssetCommand(deps *Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "asset",
		Short: "Manage bastion assets",
		Long: `Create, inspect, update and delete assets on the bastion.

These are write operations against the bastion, so they need an account
with the corresponding permission. "jms ls" stays the read-only view of
the assets you may connect to; "jms asset list" shows every asset that
exists.`,
	}
	cmd.AddCommand(newAssetListCommand(deps))
	cmd.AddCommand(newAssetShowCommand(deps))
	cmd.AddCommand(newAssetCreateCommand(deps))
	cmd.AddCommand(newAssetUpdateCommand(deps))
	cmd.AddCommand(newAssetDeleteCommand(deps))
	cmd.AddCommand(newAssetGrantCommand(deps))
	cmd.AddCommand(newAssetPermissionsCommand(deps))
	cmd.AddCommand(newAssetRevokeCommand(deps))
	return cmd
}

// assetSession resolves the server, loads the credentials and logs in,
// returning the session client and the endpoint that was used.
//
// Every asset subcommand needs this, and every one of them must report
// the endpoint on stderr so the operator knows which address was
// touched — the same rule `jms ls` follows.
func assetSession(cmd *cobra.Command, deps Deps, server, endpointKind string) (*api.Client, error) {
	force, err := parseEndpointKind(endpointKind)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfigFor(cmd, deps)
	if err != nil {
		return nil, err
	}
	srv, err := resolveServer(cfg, server)
	if err != nil {
		return nil, err
	}
	password, secret, err := loadCredentials(deps, srv.Name)
	if err != nil {
		return nil, err
	}
	ctx := commandContext(cmd)
	session, sel, err := selectEndpoint(ctx, deps, newPrompter(deps), cfg, srv, force, password, secret)
	if err != nil {
		if force != "" {
			if _, ok := srv.URLFor(string(force)); !ok {
				return nil, fmt.Errorf("server %q has no %s address configured; refusing to fall back to the other address",
					srv.Name, force)
			}
		}
		return nil, err
	}
	fmt.Fprintf(deps.Err, "Using %s endpoint %s\n", sel.Candidate.Kind, sel.Candidate.URL)
	return session.Client, nil
}

// newAssetListCommand builds `jms asset list`.
func newAssetListCommand(deps *Deps) *cobra.Command {
	var (
		server   string
		endpoint string
	)
	cmd := &cobra.Command{
		Use:   "list [server]",
		Short: "List every asset that exists on the server",
		Long: `List every asset that exists on the bastion.

Unlike "jms ls", which shows only the assets your account may connect to,
this lists the whole collection and therefore shows assets that are not
yet granted to you.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				server = args[0]
			}
			client, err := assetSession(cmd, *deps, server, endpoint)
			if err != nil {
				return err
			}
			listed, err := assetsmanage.List(commandContext(cmd), client)
			if err != nil {
				return err
			}
			if len(listed) == 0 {
				fmt.Fprintln(deps.Out, "No assets found.")
				return nil
			}
			printManagedAssetTable(deps.Out, listed)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	return cmd
}

// printManagedAssetTable renders the full asset table.
func printManagedAssetTable(out io.Writer, listed []assetsmanage.Asset) {
	table := newTable(out)
	table.setHeader("Name", "Address", "Platform", "Comment")
	for _, a := range listed {
		comment := a.Comment
		if comment == "" {
			comment = "-"
		}
		table.addRow(a.Name, a.Address, a.Platform.Name, comment)
	}
	table.setTrailer(fmt.Sprintf("\nTotal: %d asset(s)", len(listed)))
	_ = table.render()
}

// newAssetShowCommand builds `jms asset show`.
func newAssetShowCommand(deps *Deps) *cobra.Command {
	var (
		server   string
		endpoint string
	)
	cmd := &cobra.Command{
		Use:   "show <name-or-id> [server]",
		Short: "Show one asset in detail",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			if len(args) == 2 {
				server = args[1]
			}
			client, err := assetSession(cmd, *deps, server, endpoint)
			if err != nil {
				return err
			}
			ctx := commandContext(cmd)
			asset, err := lookupAsset(ctx, client, target)
			if err != nil {
				return err
			}
			printAssetDetail(deps.Out, asset)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	return cmd
}

// lookupAsset finds an asset by id, falling back to an exact name match.
func lookupAsset(ctx context.Context, client *api.Client, target string) (assetsmanage.Asset, error) {
	if asset, err := assetsmanage.Get(ctx, client, target); err == nil {
		return asset, nil
	}
	asset, err := assetsmanage.FindByName(ctx, client, target)
	if err != nil {
		return assetsmanage.Asset{}, err
	}
	return asset, nil
}

// printAssetDetail renders one asset's detail block.
func printAssetDetail(out io.Writer, a assetsmanage.Asset) {
	fmt.Fprintf(out, "ID:       %s\n", a.ID)
	fmt.Fprintf(out, "Name:     %s\n", a.Name)
	fmt.Fprintf(out, "Address:  %s\n", a.Address)
	fmt.Fprintf(out, "Platform: %s\n", a.Platform.Name)
	fmt.Fprintf(out, "Comment:  %s\n", orDash(a.Comment))
	nodes := make([]string, 0, len(a.Nodes))
	for _, n := range a.Nodes {
		nodes = append(nodes, n.Name)
	}
	fmt.Fprintf(out, "Nodes:    %s\n", orDash(strings.Join(nodes, ", ")))
	accounts := make([]string, 0, len(a.Accounts))
	for _, acc := range a.Accounts {
		accounts = append(accounts, fmt.Sprintf("%s(%s)", acc.Name, acc.Username))
	}
	fmt.Fprintf(out, "Accounts: %s\n", orDash(strings.Join(accounts, ", ")))
}

// newAssetCreateCommand builds `jms asset create`.
func newAssetCreateCommand(deps *Deps) *cobra.Command {
	var (
		server    string
		endpoint  string
		account   string
		username  string
		password  string
		nodeID    string
		platform  int
		comment   string
		protocols []string
		grantMe   bool
	)
	cmd := &cobra.Command{
		Use:   "create <name> <address> [server]",
		Short: "Create a host asset",
		Long: `Create a host asset on the bastion.

An asset with an account can be connected to; an asset without one can
only be listed. The account password is sent in the create request and is
never written to a local file.

Example:
  jms asset create "主机-云则预演01" 192.168.100.252 \
      --account yunze --username yunze --password '...'`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 3 {
				server = args[2]
			}
			client, err := assetSession(cmd, *deps, server, endpoint)
			if err != nil {
				return err
			}
			spec := assetsmanage.Spec{
				Name:       args[0],
				Address:    args[1],
				PlatformID: platform,
				NodeID:     nodeID,
				Comment:    comment,
			}
			if account != "" {
				spec.Accounts = []assetsmanage.AccountSpec{{
					Name:     account,
					Username: firstNonEmpty(username, account),
					Password: password,
				}}
			}
			if len(protocols) > 0 {
				spec.Protocols = protocols
			}
			created, err := assetsmanage.Create(commandContext(cmd), client, spec)
			if err != nil {
				return err
			}
			fmt.Fprintf(deps.Out, "Created %s (id %s)\n", created.Name, created.ID)

			if !grantMe {
				fmt.Fprintf(deps.Err, "Note: a new asset is not connectable until it is granted to your account;\n")
				fmt.Fprintf(deps.Err, "      pass --grant-me, or run `jms asset grant <name> --self`.\n")
				return nil
			}
			uid, _, err := permits.CurrentUser(commandContext(cmd), client)
			if err != nil {
				return fmt.Errorf("asset created but the grant failed: %w", err)
			}
			granted, err := permits.Grant(commandContext(cmd), client, permits.GrantSpec{
				Name:     "grant-" + created.Name,
				UserIDs:  []string{uid},
				AssetIDs: []string{created.ID},
			})
			if err != nil {
				return fmt.Errorf("asset created (id %s) but the grant failed: %w", created.ID, err)
			}
			fmt.Fprintf(deps.Out, "Granted to the current user via permission %s\n", granted.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().StringVar(&account, "account", "", "Account display name to attach")
	cmd.Flags().StringVar(&username, "username", "", "Login name on the target host (defaults to --account)")
	cmd.Flags().StringVar(&password, "password", "", "Account password (sent in the request, not stored)")
	cmd.Flags().StringVar(&nodeID, "node", "", "Asset node id (defaults to the default node)")
	cmd.Flags().IntVar(&platform, "platform", 0, "Platform id (0 = Linux)")
	cmd.Flags().StringVar(&comment, "comment", "", "Free-text note")
	cmd.Flags().StringSliceVar(&protocols, "protocol", nil, "Protocols to enable (default ssh,sftp)")
	cmd.Flags().BoolVar(&grantMe, "grant-me", false,
		"Also grant the current user access, so the asset becomes reachable immediately")
	return cmd
}

// newAssetUpdateCommand builds `jms asset update`.
func newAssetUpdateCommand(deps *Deps) *cobra.Command {
	var (
		server   string
		endpoint string
		newName  string
		comment  string
		address  string
	)
	cmd := &cobra.Command{
		Use:   "update <name-or-id> [server]",
		Short: "Update an asset's name, comment or address",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				server = args[1]
			}
			client, err := assetSession(cmd, *deps, server, endpoint)
			if err != nil {
				return err
			}
			ctx := commandContext(cmd)
			asset, err := lookupAsset(ctx, client, args[0])
			if err != nil {
				return err
			}
			fields := assetsmanage.UpdateFields{}
			if cmd.Flags().Changed("name") {
				fields.Name = &newName
			}
			if cmd.Flags().Changed("comment") {
				fields.Comment = &comment
			}
			if cmd.Flags().Changed("address") {
				fields.Address = &address
			}
			updated, err := assetsmanage.Update(ctx, client, asset.ID, fields)
			if err != nil {
				return err
			}
			fmt.Fprintf(deps.Out, "Updated %s (id %s)\n", updated.Name, updated.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().StringVar(&newName, "name", "", "New asset name")
	cmd.Flags().StringVar(&comment, "comment", "", "New comment")
	cmd.Flags().StringVar(&address, "address", "", "New address")
	return cmd
}

// newAssetDeleteCommand builds `jms asset delete`.
func newAssetDeleteCommand(deps *Deps) *cobra.Command {
	var (
		server   string
		endpoint string
		yes      bool
	)
	cmd := &cobra.Command{
		Use:   "delete <name-or-id> [server]",
		Short: "Delete an asset",
		Long: `Delete an asset from the bastion.

This cannot be undone. The command asks for confirmation unless --yes is
given, and it deletes only the asset itself — accounts, nodes and other
objects are left alone.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				server = args[1]
			}
			client, err := assetSession(cmd, *deps, server, endpoint)
			if err != nil {
				return err
			}
			ctx := commandContext(cmd)
			asset, err := lookupAsset(ctx, client, args[0])
			if err != nil {
				return err
			}
			if !yes {
				fmt.Fprintf(deps.Out, "About to delete asset %q (id %s, address %s).\n",
					asset.Name, asset.ID, asset.Address)
				prompter := newPrompter(deps.WithDefaults())
				ok, err := prompter.confirm(fmt.Sprintf("Delete %q?", asset.Name))
				if err != nil {
					return fmt.Errorf("confirmation failed: %w", err)
				}
				if !ok {
					return errors.New("not confirmed; nothing was deleted")
				}
			}
			if err := assetsmanage.Delete(ctx, client, asset.ID); err != nil {
				return err
			}
			fmt.Fprintf(deps.Out, "Deleted %s (id %s)\n", asset.Name, asset.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Do not ask for confirmation")
	return cmd
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// unusedConfigGuard keeps the config import referenced when the file is
// edited; the session helper uses config.ServerConfig indirectly.
var _ = config.CredPassword
