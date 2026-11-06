package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/mgmt"
	"github.com/MiFaZhan/jms-client/internal/obs"
)

// Tool names for the gated write surface.
//
// Two operations, not fifteen. Node create/update/delete keeps the asset tree
// maintainable between console visits; account secret rotation is the one
// routine operation that is both frequent and easy to get wrong by hand.
// Everything else (user changes, session kill, job execution, ticket
// approval) waits for a real need — a write tool is a production change that
// no longer needs a console.
const (
	ToolNodeWrite     = "jms_node_write"
	ToolAccountSecret = "jms_account_secret"
)

// Write domains, matching EnvWriteDomains values.
const (
	DomainNode    = "node"
	DomainAccount = "account"
)

// registerAdminWrites registers the write tools when they are enabled.
//
// Registration is the first of three gates, and the only one a client can
// see: an unregistered tool does not appear in tools/list at all, so a model
// cannot be talked into calling it. The second gate is the domain list, the
// third is the confirm argument.
func (s *Server) registerAdminWrites(srv *mcp.Server) {
	for _, factory := range s.adminWriteFactories() {
		register(srv, factory)
	}
}

// adminWriteFactories returns the write tools the current configuration
// allows, in registration order.
//
// Both the registration path and the tests read this slice, so "0 write tools
// by default" is asserted against the code that registers them.
func (s *Server) adminWriteFactories() []func() (*mcp.Tool, mcp.ToolHandler) {
	enabled, allowed := writeDomains()
	if !enabled {
		return nil
	}
	var out []func() (*mcp.Tool, mcp.ToolHandler)
	if domainAllowed(allowed, DomainNode) {
		out = append(out, s.nodeWriteTool)
	}
	if domainAllowed(allowed, DomainAccount) {
		out = append(out, s.accountSecretTool)
	}
	return out
}

// domainAllowed reports whether a domain passed the domain list.
func domainAllowed(allowed map[string]bool, domain string) bool {
	if allowed == nil {
		return true
	}
	return allowed[domain]
}

// nodeWriteTool builds jms_node_write.
func (s *Server) nodeWriteTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolNodeWrite,
		"Create, rename or delete one JumpServer asset node. Destructive operations require confirm set to the exact "+
			"current node name. Only registered when JMS_MCP_WRITE=limited.",
		mergeProps(map[string]map[string]any{
			"op":        strProp("create | update (rename) | delete."),
			"name":      strProp("New node name (create/update)."),
			"parent_id": strProp("Parent node id or exact name for create; empty means the root."),
			"node":      strProp("Target node id or exact name (update/delete)."),
			"confirm":   strProp("For delete: the exact current node name. Not needed for create."),
		}),
		[]string{"op"},
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			op := strings.ToLower(strArg(args, "op"))
			name := strArg(args, "name")
			node := strArg(args, "node")
			confirm := strArg(args, "confirm")

			switch op {
			case "create":
				if domainMissing(DomainNode) {
					return s.refuse(ToolNodeWrite, "write domain \"node\" is not enabled")
				}
				return s.adminWrite(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolNodeWrite, mgmt.PathNodes, name,
					func(c *api.Client) (any, string, error) {
						created, err := mgmt.CreateNode(ctx, c, strArg(args, "parent_id"), name)
						if err != nil {
							return nil, "", err
						}
						return created, fmt.Sprintf("created node %q (id %s)", created.Name, created.ID), nil
					})
			case "update", "rename":
				if domainMissing(DomainNode) {
					return s.refuse(ToolNodeWrite, "write domain \"node\" is not enabled")
				}
				if name == "" {
					return s.refuse(ToolNodeWrite, "name is required for update")
				}
				return s.adminWrite(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolNodeWrite, mgmt.PathNodes, node,
					func(c *api.Client) (any, string, error) {
						target, err := mgmt.ResolveNode(ctx, c, node)
						if err != nil {
							return nil, "", err
						}
						updated, err := mgmt.RenameNode(ctx, c, node, name)
						if err != nil {
							return nil, "", err
						}
						return updated, fmt.Sprintf("renamed node %q to %q", target.Name, updated.Name), nil
					})
			case "delete":
				if domainMissing(DomainNode) {
					return s.refuse(ToolNodeWrite, "write domain \"node\" is not enabled")
				}
				if node == "" {
					return s.refuse(ToolNodeWrite, "node is required for delete")
				}
				return s.adminWrite(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolNodeWrite, mgmt.PathNodes, node,
					func(c *api.Client) (any, string, error) {
						target, err := mgmt.ResolveNode(ctx, c, node)
						if err != nil {
							return nil, "", err
						}
						if confirm != target.Name && confirm != target.ID {
							return nil, "", fmt.Errorf("%w: confirm must repeat the node name %q", errConfirm, target.Name)
						}
						deleted, err := mgmt.DeleteNode(ctx, c, node)
						if err != nil {
							return nil, "", err
						}
						return deleted, fmt.Sprintf("deleted node %q (id %s)", deleted.Name, deleted.ID), nil
					})
			default:
				return s.refuse(ToolNodeWrite, fmt.Sprintf("unknown op %q: use create | update | delete", op))
			}
		})
}

// accountSecretTool builds jms_account_secret.
func (s *Server) accountSecretTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolAccountSecret,
		"Rotate one asset account's password via JumpServer (the server generates the new secret; it is never passed in "+
			"or returned). Requires confirm set to \"<asset>/<username>\". Only registered when JMS_MCP_WRITE=limited.",
		mergeProps(map[string]map[string]any{
			"asset":    strProp("Asset name the account belongs to; it disambiguates a repeated username."),
			"username": strProp("Account username to rotate."),
			"confirm":  strProp("Must equal \"<asset>/<username>\"."),
		}),
		[]string{"username", "confirm"},
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			if domainMissing(DomainAccount) {
				return s.refuse(ToolAccountSecret, "write domain \"account\" is not enabled")
			}
			asset := strArg(args, "asset")
			username := strArg(args, "username")
			want := asset + "/" + username
			if got := strArg(args, "confirm"); got != want {
				return s.refuse(ToolAccountSecret, fmt.Sprintf("confirm must be %q", want))
			}
			return s.adminWrite(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolAccountSecret, mgmt.PathAccounts, want,
				func(c *api.Client) (any, string, error) {
					id, err := mgmt.FindAccountID(ctx, c, asset, username)
					if err != nil {
						return nil, "", err
					}
					if err := mgmt.ChangeAccountSecret(ctx, c, []string{id}); err != nil {
						return nil, "", err
					}
					return map[string]string{"account_id": id, "asset": asset, "username": username},
						fmt.Sprintf("requested a secret rotation for %s on %s", username, asset), nil
				})
		})
}

// errConfirm marks a wrong confirm argument, so the audit line can say the
// call was refused locally rather than by the server.
var errConfirm = errors.New("confirm does not match")

// domainMissing reports whether a write domain was excluded by
// JMS_MCP_WRITE_DOMAINS.
//
// It is checked per call as well as at registration: a long-lived host that
// started with the domain allowed must still obey a narrower configuration on
// the next process, and a test can exercise the refusal without restarting.
func domainMissing(domain string) bool {
	enabled, allowed := writeDomains()
	if !enabled {
		return true
	}
	return !domainAllowed(allowed, domain)
}

// adminWrite runs one write, records it, and renders the outcome.
//
// Failures are classified once here so every write tool reports the same
// way: a locally refused confirm, a permission 403 and a transport failure
// look identical to a caller only if this function lets them.
func (s *Server) adminWrite(ctx context.Context, server, configPath, op, path, target string,
	run func(*api.Client) (any, string, error)) (*mcp.CallToolResult, error) {
	sess, srv, err := s.sessionFor(ctx, server, configPath)
	if err != nil {
		s.failOp(server, target, op, err)
		return errorResult(err), nil
	}
	if sess == nil || sess.Client == nil {
		err := errors.New("session has no API client")
		s.failOp(server, target, op, err)
		return errorResult(err), nil
	}

	event := obs.Event{
		Kind:   obs.KindAPIWrite,
		Server: srv.Name,
		Path:   path,
		Asset:  target,
		Status: 200,
	}
	result, message, err := run(sess.Client)
	if err != nil {
		event.Error = err.Error()
		event.Kind = obs.KindError
		if errors.Is(err, errConfirm) {
			event.Kind = obs.KindAPIDenied
			event.Reason = err.Error()
		}
		if e, ok := api.AsAPIError(err); ok {
			event.Status = e.StatusCode
		}
		if e, ok := api.AsAuthError(err); ok {
			event.Status = e.StatusCode
		}
		s.publish(event)
		return errorResult(err), nil
	}
	s.publish(event)

	body, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		return textResult(message), nil
	}
	return textResult(message + "\n" + string(body)), nil
}

// refuse reports a call refused before any request was made.
//
// The refusal is recorded as api.denied: an attempt to use a write path that
// is switched off is exactly the event an operator wants to see later, and
// silence would make it invisible.
func (s *Server) refuse(op, reason string) (*mcp.CallToolResult, error) {
	s.publish(obs.Event{
		Kind:    obs.KindAPIDenied,
		Command: op,
		Reason:  reason,
		PID:     os.Getpid(),
	})
	return errorResult(fmt.Errorf("%s: %s", op, reason)), nil
}
