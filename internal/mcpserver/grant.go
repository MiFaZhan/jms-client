package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/permits"
)

// Tool names for the permission tools.
const (
	ToolAssetGrant       = "jms_asset_grant"
	ToolAssetPermissions = "jms_asset_permissions"
	ToolAssetRevoke      = "jms_asset_revoke"
)

// assetGrantArgs are the jms_asset_grant arguments.
type assetGrantArgs struct {
	Server     string   `json:"server"`
	ConfigPath string   `json:"config_path"`
	Target     string   `json:"target"`
	Name       string   `json:"name"`
	Users      []string `json:"users"`
	Accounts   []string `json:"accounts"`
	Protocols  []string `json:"protocols"`
	Actions    []string `json:"actions"`
	Comment    string   `json:"comment"`
}

// assetRevokeArgs are the jms_asset_revoke arguments.
type assetRevokeArgs struct {
	Server     string `json:"server"`
	ConfigPath string `json:"config_path"`
	Target     string `json:"target"`
}

// assetGrantTool builds the jms_asset_grant tool.
func (s *Server) assetGrantTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name: ToolAssetGrant,
		Description: "Grant users access to a JumpServer asset by creating an AssetPermission. " +
			"A newly created asset is not reachable until it has been granted.",
		InputSchema: objectSchema([]string{"target"}, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"target":      strProp("Asset name or id to grant."),
			"name":        strProp("Permission name; defaults to grant-<asset>."),
			"users":       strProp("User ids to grant; the authenticated user when omitted."),
			"accounts":    strProp("Accounts to allow; @ALL when omitted."),
			"protocols":   strProp("Protocols to allow; all when omitted."),
			"actions":     strProp("Actions to allow; connect when omitted."),
			"comment":     strProp("Free-text note."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetGrantArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		if strings.TrimSpace(args.Target) == "" {
			return errorResult(errors.New("target is required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset grant "+args.Target, err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		asset, err := s.lookupManagedAsset(ctx, sess.Client, args.Target)
		if err != nil {
			s.failOp(args.Server, "", "asset grant "+args.Target, err)
			return errorResult(err), nil
		}

		userIDs := args.Users
		if len(userIDs) == 0 {
			uid, _, err := permits.CurrentUser(ctx, sess.Client)
			if err != nil {
				s.failOp(args.Server, "", "asset grant "+args.Target, err)
				return errorResult(err), nil
			}
			userIDs = []string{uid}
		}

		permName := args.Name
		if permName == "" {
			permName = "grant-" + asset.Name
		}

		granted, err := permits.Grant(ctx, sess.Client, permits.GrantSpec{
			Name:      permName,
			UserIDs:   userIDs,
			AssetIDs:  []string{asset.ID},
			Accounts:  args.Accounts,
			Protocols: args.Protocols,
			Actions:   args.Actions,
			Comment:   args.Comment,
		})
		if err != nil {
			s.failOp(args.Server, "", "asset grant "+args.Target, err)
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, "", "", "asset grant "+args.Target)
		text := fmt.Sprintf("Granted %s to %d user(s) via permission %s (id %s).",
			asset.Name, len(userIDs), granted.Name, granted.ID)
		s.endOp(started, srv.Name, "", "", "asset grant "+args.Target,
			transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// assetPermissionsTool builds the jms_asset_permissions tool.
func (s *Server) assetPermissionsTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolAssetPermissions,
		Description: "List JumpServer asset permissions, optionally restricted to one asset.",
		InputSchema: objectSchema(nil, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"target":      strProp("Asset name or id; lists every permission when omitted."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetRevokeArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset permissions", err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		var listed []permits.Permission
		if strings.TrimSpace(args.Target) != "" {
			asset, err := s.lookupManagedAsset(ctx, sess.Client, args.Target)
			if err != nil {
				s.failOp(args.Server, "", "asset permissions "+args.Target, err)
				return errorResult(err), nil
			}
			listed, err = permits.FindByAsset(ctx, sess.Client, asset.ID)
			if err != nil {
				s.failOp(args.Server, "", "asset permissions "+args.Target, err)
				return errorResult(err), nil
			}
		} else {
			listed, err = permits.List(ctx, sess.Client)
			if err != nil {
				s.failOp(args.Server, "", "asset permissions", err)
				return errorResult(err), nil
			}
			// The list endpoint omits the relations, so expand each row.
			for i := range listed {
				if len(listed[i].Assets) == 0 && len(listed[i].Users) == 0 {
					if detail, err := permits.Get(ctx, sess.Client, listed[i].ID); err == nil {
						listed[i] = detail
					}
				}
			}
		}

		started := s.startOp(srv.Name, "", "", "asset permissions")
		text := renderPermissionList(listed)
		s.endOp(started, srv.Name, "", "", "asset permissions",
			transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// renderPermissionList renders one permission per line.
func renderPermissionList(listed []permits.Permission) string {
	if len(listed) == 0 {
		return "No permissions found."
	}
	lines := make([]string, 0, len(listed))
	for _, p := range listed {
		users := make([]string, 0, len(p.Users))
		for _, u := range p.Users {
			users = append(users, u.Name)
		}
		assets := make([]string, 0, len(p.Assets))
		for _, a := range p.Assets {
			assets = append(assets, a.Name)
		}
		actions := make([]string, 0, len(p.Actions))
		for _, a := range p.Actions {
			actions = append(actions, a.Value)
		}
		lines = append(lines, fmt.Sprintf("%s  users=%s  assets=%s  protocols=%s  actions=%s",
			p.Name,
			strings.Join(users, ","),
			strings.Join(assets, ","),
			strings.Join(p.Protocols, ","),
			strings.Join(actions, ",")))
	}
	return strings.Join(lines, "\n")
}

// assetRevokeTool builds the jms_asset_revoke tool.
func (s *Server) assetRevokeTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolAssetRevoke,
		Description: "Revoke a JumpServer asset permission. The asset stays; only the access it granted is removed.",
		InputSchema: objectSchema([]string{"target"}, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"target":      strProp("Permission name or id."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetRevokeArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		if strings.TrimSpace(args.Target) == "" {
			return errorResult(errors.New("target is required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset revoke "+args.Target, err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		listed, err := permits.List(ctx, sess.Client)
		if err != nil {
			s.failOp(args.Server, "", "asset revoke "+args.Target, err)
			return errorResult(err), nil
		}
		var found *permits.Permission
		for i := range listed {
			if listed[i].ID == args.Target || listed[i].Name == args.Target {
				found = &listed[i]
				break
			}
		}
		if found == nil {
			return errorResult(fmt.Errorf("no permission matches %q", args.Target)), nil
		}

		if err := permits.Revoke(ctx, sess.Client, found.ID); err != nil {
			s.failOp(args.Server, "", "asset revoke "+args.Target, err)
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, "", "", "asset revoke "+args.Target)
		text := fmt.Sprintf("Revoked %s (id %s).", found.Name, found.ID)
		s.endOp(started, srv.Name, "", "", "asset revoke "+args.Target,
			transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}
