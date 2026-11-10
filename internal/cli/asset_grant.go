package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/MiFaZhan/jms-client/internal/mgmt"
	"github.com/MiFaZhan/jms-client/internal/permits"
)

// newAssetGrantCommand builds `jms asset grant`.
func newAssetGrantCommand(deps *Deps) *cobra.Command {
	var (
		server    string
		endpoint  string
		userNames []string
		groups    []string
		accounts  []string
		protocols []string
		actions   []string
		name      string
		comment   string
		self      bool
	)
	cmd := &cobra.Command{
		Use:   "grant <asset-name-or-id> [server]",
		Short: "Grant users or user groups access to an asset",
		Long: `Grant access to an asset by creating an AssetPermission.

A freshly created asset is not reachable: JumpServer only exposes it to a
user through a permission record. Until this command (or the console) has
granted it, the asset shows up in "jms asset list" but not in "jms ls".

Principals are users and/or user groups; at least one is required.
--user takes a user id or an exact username, --group takes a group id or
an exact group name, and both may be repeated or comma-separated.
--self grants the authenticated user, which is the common case while
provisioning and also the default when no --user or --group is given.`,
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

			userIDs, err := mgmt.ResolveUserIDs(ctx, client, userNames)
			if err != nil {
				return err
			}
			groupIDs, err := mgmt.ResolveGroupIDs(ctx, client, groups)
			if err != nil {
				return err
			}
			// --self is additive; with neither users nor groups the
			// authenticated user is the principal, as before.
			if self || (len(userIDs) == 0 && len(groupIDs) == 0) {
				uid, _, err := permits.CurrentUser(ctx, client)
				if err != nil {
					return fmt.Errorf("resolve the current user: %w", err)
				}
				if !containsString(userIDs, uid) {
					userIDs = append(userIDs, uid)
				}
			}

			permName := name
			if permName == "" {
				permName = "grant-" + asset.Name
			}

			granted, err := permits.Grant(ctx, client, permits.GrantSpec{
				Name:      permName,
				UserIDs:   userIDs,
				GroupIDs:  groupIDs,
				AssetIDs:  []string{asset.ID},
				Accounts:  accounts,
				Protocols: protocols,
				Actions:   actions,
				Comment:   comment,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(deps.Out, "Granted %s to %d user(s) and %d user group(s) via permission %s (id %s)\n",
				asset.Name, len(userIDs), len(groupIDs), granted.Name, granted.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().BoolVar(&self, "self", false, "Also grant the authenticated user")
	cmd.Flags().StringSliceVar(&userNames, "user", nil, "User id or exact username to grant (repeatable)")
	cmd.Flags().StringSliceVar(&groups, "group", nil, "User group id or exact name to grant (repeatable)")
	cmd.Flags().StringSliceVar(&accounts, "account", nil, "Accounts to allow (default @ALL)")
	cmd.Flags().StringSliceVar(&protocols, "protocol", nil, "Protocols to allow (default all)")
	cmd.Flags().StringSliceVar(&actions, "action", nil, "Actions to allow (default connect)")
	cmd.Flags().StringVar(&name, "name", "", "Permission name (default grant-<asset>)")
	cmd.Flags().StringVar(&comment, "comment", "", "Free-text note")
	return cmd
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// newAssetPermissionsCommand builds `jms asset permissions`.
func newAssetPermissionsCommand(deps *Deps) *cobra.Command {
	var (
		server   string
		endpoint string
		assetArg string
	)
	cmd := &cobra.Command{
		Use:   "permissions [asset-name-or-id] [server]",
		Short: "List asset permissions, optionally for one asset",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) >= 1 {
				assetArg = args[0]
			}
			if len(args) == 2 {
				server = args[1]
			}
			client, err := assetSession(cmd, *deps, server, endpoint)
			if err != nil {
				return err
			}
			ctx := commandContext(cmd)

			var listed []permits.Permission
			if assetArg != "" {
				asset, err := lookupAsset(ctx, client, assetArg)
				if err != nil {
					return err
				}
				listed, err = permits.FindByAsset(ctx, client, asset.ID)
				if err != nil {
					return err
				}
			} else {
				listed, err = permits.List(ctx, client)
				if err != nil {
					return err
				}
				// The list endpoint carries counts but no relations, so
				// expand every row that still needs it; a group-only or
				// node-only rule has no users and no assets, and used to
				// be skipped entirely. The asset-scoped branch above
				// already returns expanded details, so this runs once.
				expanded := make([]permits.Permission, 0, len(listed))
				for _, p := range listed {
					if p.NeedsDetail() {
						detail, err := permits.Get(ctx, client, p.ID)
						if err != nil {
							return fmt.Errorf("read permission %q: %w", p.Name, err)
						}
						p = detail
					}
					expanded = append(expanded, p)
				}
				listed = expanded
			}
			if len(listed) == 0 {
				fmt.Fprintln(deps.Out, "No permissions found.")
				return nil
			}
			printPermissionTable(deps.Out, listed)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	return cmd
}

// printPermissionTable renders the permission table.
func printPermissionTable(out io.Writer, listed []permits.Permission) {
	table := newTable(out)
	table.setHeader("Name", "Users", "Groups", "Assets", "Nodes", "Protocols", "Actions")
	for _, p := range listed {
		actions := make([]string, 0, len(p.Actions))
		for _, a := range p.Actions {
			actions = append(actions, a.Value)
		}
		table.addRow(
			p.Name,
			orDash(strings.Join(permits.RelationNames(p.Users), ",")),
			orDash(strings.Join(permits.RelationNames(p.UserGroups), ",")),
			orDash(strings.Join(permits.RelationNames(p.Assets), ",")),
			orDash(strings.Join(permits.RelationNames(p.Nodes), ",")),
			orDash(strings.Join(p.Protocols, ",")),
			orDash(strings.Join(actions, ",")),
		)
	}
	table.setTrailer(fmt.Sprintf("\nTotal: %d permission(s)", len(listed)))
	_ = table.render()
}

// newAssetRevokeCommand builds `jms asset revoke`.
func newAssetRevokeCommand(deps *Deps) *cobra.Command {
	var (
		server   string
		endpoint string
		yes      bool
	)
	cmd := &cobra.Command{
		Use:   "revoke <permission-name-or-id> [server]",
		Short: "Revoke an asset permission",
		Long: `Delete an AssetPermission, which removes the access it granted.

The asset itself is left in place; only the authorization goes away.`,
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

			target := args[0]
			listed, err := permits.List(ctx, client)
			if err != nil {
				return err
			}
			var found *permits.Permission
			for i := range listed {
				if listed[i].ID == target || listed[i].Name == target {
					found = &listed[i]
					break
				}
			}
			if found == nil {
				return fmt.Errorf("no permission matches %q", target)
			}

			if !yes {
				prompter := newPrompter(deps.WithDefaults())
				ok, err := prompter.confirm(fmt.Sprintf("Revoke permission %q?", found.Name))
				if err != nil {
					return fmt.Errorf("confirmation failed: %w", err)
				}
				if !ok {
					return fmt.Errorf("not confirmed; nothing was revoked")
				}
			}
			if err := permits.Revoke(ctx, client, found.ID); err != nil {
				return err
			}
			fmt.Fprintf(deps.Out, "Revoked %s (id %s)\n", found.Name, found.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Force an address: internal|external")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Do not ask for confirmation")
	return cmd
}
