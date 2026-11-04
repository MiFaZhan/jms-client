package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assetsmanage"
)

// Tool names for the asset-management tools.
const (
	ToolAssetCreate = "jms_asset_create"
	ToolAssetList   = "jms_asset_list"
	ToolAssetShow   = "jms_asset_show"
	ToolAssetUpdate = "jms_asset_update"
	ToolAssetDelete = "jms_asset_delete"
)

// assetCreateArgs are the jms_asset_create arguments.
type assetCreateArgs struct {
	Server     string `json:"server"`
	ConfigPath string `json:"config_path"`
	Name       string `json:"name"`
	Address    string `json:"address"`
	Account    string `json:"account"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Comment    string `json:"comment"`
	NodeID     string `json:"node_id"`
	PlatformID int    `json:"platform_id"`
}

// assetTargetArgs are the arguments of the tools that act on one asset.
type assetTargetArgs struct {
	Server     string `json:"server"`
	ConfigPath string `json:"config_path"`
	Target     string `json:"target"`
	NewName    string `json:"new_name"`
	Comment    string `json:"comment"`
	Address    string `json:"address"`
}

// assetCreateTool builds the jms_asset_create tool.
func (s *Server) assetCreateTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolAssetCreate,
		Description: "Create a host asset on a JumpServer.",
		InputSchema: objectSchema([]string{"name", "address"}, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"name":        strProp("Asset name; must be unique on the server."),
			"address":     strProp("Reachable address of the target host."),
			"account":     strProp("Account display name to attach; without one the asset cannot be connected to."),
			"username":    strProp("Login name on the target host; defaults to the account name."),
			"password":    strProp("Account password; sent in the request and never stored locally."),
			"comment":     strProp("Free-text note stored with the asset."),
			"node_id":     strProp("Asset node id; the default node is used when omitted."),
			"platform_id": intProp("Platform id; 0 means Linux."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetCreateArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		if strings.TrimSpace(args.Name) == "" || strings.TrimSpace(args.Address) == "" {
			return errorResult(errors.New("name and address are required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset create "+args.Name, err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		spec := assetsmanage.Spec{
			Name:       args.Name,
			Address:    args.Address,
			PlatformID: args.PlatformID,
			NodeID:     args.NodeID,
			Comment:    args.Comment,
		}
		if strings.TrimSpace(args.Account) != "" {
			username := args.Username
			if strings.TrimSpace(username) == "" {
				username = args.Account
			}
			spec.Accounts = []assetsmanage.AccountSpec{{
				Name:     args.Account,
				Username: username,
				Password: args.Password,
			}}
		}

		created, err := assetsmanage.Create(ctx, sess.Client, spec)
		if err != nil {
			s.failOp(args.Server, "", "asset create "+args.Name, err)
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, "", "", "asset create "+args.Name)
		text := fmt.Sprintf("Created %s (id %s, address %s).", created.Name, created.ID, created.Address)
		text += "\nThe asset is not connectable until it is granted to your account."
		s.endOp(started, srv.Name, "", "", "asset create "+args.Name, transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// assetListTool builds the jms_asset_list tool.
func (s *Server) assetListTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolAssetList,
		Description: "List every asset that exists on a JumpServer, not only the ones you may connect to.",
		InputSchema: objectSchema(nil, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetTargetArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset list", err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		listed, err := assetsmanage.List(ctx, sess.Client)
		if err != nil {
			s.failOp(args.Server, "", "asset list", err)
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, "", "", "asset list")
		text := renderManagedAssetList(listed)
		s.endOp(started, srv.Name, "", "", "asset list", transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// renderManagedAssetList renders the managed asset list.
func renderManagedAssetList(listed []assetsmanage.Asset) string {
	if len(listed) == 0 {
		return "No assets found."
	}
	lines := make([]string, 0, len(listed))
	for _, a := range listed {
		comment := a.Comment
		if comment == "" {
			comment = "-"
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s", a.Name, a.Address, a.Platform.Name, comment))
	}
	return strings.Join(lines, "\n")
}

// assetShowTool builds the jms_asset_show tool.
func (s *Server) assetShowTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolAssetShow,
		Description: "Show one JumpServer asset in detail, by name or by id.",
		InputSchema: objectSchema([]string{"target"}, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"target":      strProp("Asset name or id."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetTargetArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		if strings.TrimSpace(args.Target) == "" {
			return errorResult(errors.New("target is required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset show "+args.Target, err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		asset, err := s.lookupManagedAsset(ctx, sess.Client, args.Target)
		if err != nil {
			s.failOp(args.Server, "", "asset show "+args.Target, err)
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, "", "", "asset show "+args.Target)
		text := renderManagedAsset(asset)
		s.endOp(started, srv.Name, "", "", "asset show "+args.Target, transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// lookupManagedAsset finds an asset by id, falling back to an exact name
// match.
func (s *Server) lookupManagedAsset(ctx context.Context, client *api.Client, target string) (assetsmanage.Asset, error) {
	if asset, err := assetsmanage.Get(ctx, client, target); err == nil {
		return asset, nil
	}
	return assetsmanage.FindByName(ctx, client, target)
}

// renderManagedAsset renders one asset's detail block.
func renderManagedAsset(a assetsmanage.Asset) string {
	nodes := make([]string, 0, len(a.Nodes))
	for _, n := range a.Nodes {
		nodes = append(nodes, n.Name)
	}
	accounts := make([]string, 0, len(a.Accounts))
	for _, acc := range a.Accounts {
		accounts = append(accounts, fmt.Sprintf("%s(%s)", acc.Name, acc.Username))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ID:       %s\n", a.ID)
	fmt.Fprintf(&b, "Name:     %s\n", a.Name)
	fmt.Fprintf(&b, "Address:  %s\n", a.Address)
	fmt.Fprintf(&b, "Platform: %s\n", a.Platform.Name)
	fmt.Fprintf(&b, "Comment:  %s\n", dashIfEmpty(a.Comment))
	fmt.Fprintf(&b, "Nodes:    %s\n", dashIfEmpty(strings.Join(nodes, ", ")))
	fmt.Fprintf(&b, "Accounts: %s\n", dashIfEmpty(strings.Join(accounts, ", ")))
	return b.String()
}

// dashIfEmpty renders an empty string as a dash.
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// assetUpdateTool builds the jms_asset_update tool.
func (s *Server) assetUpdateTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolAssetUpdate,
		Description: "Update a JumpServer asset's name, comment or address.",
		InputSchema: objectSchema([]string{"target"}, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"target":      strProp("Asset name or id."),
			"new_name":    strProp("New asset name."),
			"comment":     strProp("New comment."),
			"address":     strProp("New address."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetTargetArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		if strings.TrimSpace(args.Target) == "" {
			return errorResult(errors.New("target is required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset update "+args.Target, err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		asset, err := s.lookupManagedAsset(ctx, sess.Client, args.Target)
		if err != nil {
			s.failOp(args.Server, "", "asset update "+args.Target, err)
			return errorResult(err), nil
		}

		fields := assetsmanage.UpdateFields{}
		if args.NewName != "" {
			fields.Name = &args.NewName
		}
		if args.Comment != "" {
			fields.Comment = &args.Comment
		}
		if args.Address != "" {
			fields.Address = &args.Address
		}
		updated, err := assetsmanage.Update(ctx, sess.Client, asset.ID, fields)
		if err != nil {
			s.failOp(args.Server, "", "asset update "+args.Target, err)
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, "", "", "asset update "+args.Target)
		text := fmt.Sprintf("Updated %s (id %s).", updated.Name, updated.ID)
		s.endOp(started, srv.Name, "", "", "asset update "+args.Target, transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}

// assetDeleteTool builds the jms_asset_delete tool.
func (s *Server) assetDeleteTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        ToolAssetDelete,
		Description: "Delete a JumpServer asset. This cannot be undone.",
		InputSchema: objectSchema([]string{"target"}, map[string]map[string]any{
			"server":      strProp("Server alias; the default server when omitted."),
			"config_path": strProp("Configuration file path override."),
			"target":      strProp("Asset name or id."),
		}),
	}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args assetTargetArgs
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		if strings.TrimSpace(args.Target) == "" {
			return errorResult(errors.New("target is required")), nil
		}

		sess, srv, err := s.sessionFor(ctx, args.Server, args.ConfigPath)
		if err != nil {
			s.failOp(args.Server, "", "asset delete "+args.Target, err)
			return errorResult(err), nil
		}
		if sess == nil || sess.Client == nil {
			return errorResult(errors.New("session has no API client")), nil
		}

		asset, err := s.lookupManagedAsset(ctx, sess.Client, args.Target)
		if err != nil {
			s.failOp(args.Server, "", "asset delete "+args.Target, err)
			return errorResult(err), nil
		}
		if err := assetsmanage.Delete(ctx, sess.Client, asset.ID); err != nil {
			s.failOp(args.Server, "", "asset delete "+args.Target, err)
			return errorResult(err), nil
		}

		started := s.startOp(srv.Name, "", "", "asset delete "+args.Target)
		text := fmt.Sprintf("Deleted %s (id %s).", asset.Name, asset.ID)
		s.endOp(started, srv.Name, "", "", "asset delete "+args.Target, transferOutcome{output: text, outputBytes: len(text)})
		return textResult(text), nil
	}
}
