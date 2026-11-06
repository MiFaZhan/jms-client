package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/mgmt"
	"github.com/MiFaZhan/jms-client/internal/obs"
)

// auditListTool builds jms_audit_list, which covers all five audit views.
//
// One tool with a kind enum rather than five tools: they share a permission,
// a record shape (after normalization) and an audit semantic, which is the
// only condition under which this codebase aggregates.
func (s *Server) auditListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolAuditList,
		"Read JumpServer audit logs: login, operate, password-change, file-transfer (ftp) and job records. "+
			"since/until are applied by the server; user/asset/action are matched on the fetched page.",
		mergeProps(map[string]map[string]any{
			"kind":   strProp("Audit kind: login | operate | password | ftp | job (default login)."),
			"since":  strProp("Only records on or after this date, e.g. 2026-11-01 (server-side)."),
			"until":  strProp("Only records on or before this date (server-side)."),
			"user":   strProp("Filter by user name on the fetched page."),
			"asset":  strProp("Filter by object/asset on the fetched page."),
			"action": strProp("Filter by action or result on the fetched page."),
			"limit":  intProp("Page size, default 20, max 100."),
			"offset": intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.AuditQuery{
				Kind:   strArg(args, "kind"),
				Since:  strArg(args, "since"),
				Until:  strArg(args, "until"),
				User:   strArg(args, "user"),
				Asset:  strArg(args, "asset"),
				Action: strArg(args, "action"),
				Limit:  intArg(args, "limit"),
				Offset: intArg(args, "offset"),
			}
			if q.Kind == "" {
				q.Kind = mgmt.AuditLogin
			}
			path := mgmt.AuditPath(q.Kind)
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolAuditList, path,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListAudit(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					note := ""
					if q.User != "" || q.Asset != "" || q.Action != "" {
						note = windowNote(mgmt.Window(q.Limit, true))
					}
					return pageOf(page, note), nil
				})
		})
}

// sessionListTool builds jms_session_list.
func (s *Server) sessionListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolSessionList,
		"List JumpServer terminal sessions: who connected to which asset, when, and whether it is still open.",
		mergeProps(map[string]map[string]any{
			"since":       strProp("Only sessions starting on or after this date (server-side)."),
			"until":       strProp("Only sessions starting on or before this date (server-side)."),
			"user":        strProp("Filter by user on the fetched page."),
			"asset":       strProp("Filter by asset on the fetched page."),
			"active_only": boolProp("Only sessions that are still open."),
			"limit":       intProp("Page size, default 20, max 100."),
			"offset":      intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.SessionQuery{
				Since:  strArg(args, "since"),
				Until:  strArg(args, "until"),
				User:   strArg(args, "user"),
				Asset:  strArg(args, "asset"),
				Active: boolArg(args, "active_only"),
				Limit:  intArg(args, "limit"),
				Offset: intArg(args, "offset"),
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolSessionList, mgmt.PathSessions,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListSessions(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					note := ""
					if q.User != "" || q.Asset != "" || q.Active {
						note = windowNote(mgmt.Window(q.Limit, true))
					}
					return pageOf(page, note), nil
				})
		})
}

// sessionShowTool builds jms_session_show.
func (s *Server) sessionShowTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolSessionShow,
		"Show one terminal session: participants, timing, replay availability and whether it can be terminated.",
		mergeProps(map[string]map[string]any{
			"session_id": strProp("Session id (UUID) from jms_session_list."),
		}),
		[]string{"session_id"},
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			id := strArg(args, "session_id")
			if id == "" {
				return errorResult(errors.New("session_id is required")), nil
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolSessionShow, mgmt.PathSessions+id+"/",
				func(c *api.Client) (adminPage, error) {
					one, err := mgmt.GetSession(ctx, c, id)
					if err != nil {
						return adminPage{}, err
					}
					return adminPage{Count: 1, Returned: 1, Items: one}, nil
				})
		})
}

// commandListTool builds jms_command_list.
func (s *Server) commandListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolCommandList,
		"List recorded commands from JumpServer session command logs.",
		mergeProps(map[string]map[string]any{
			"since":  strProp("Only records on or after this date (server-side)."),
			"until":  strProp("Only records on or before this date (server-side)."),
			"user":   strProp("Filter by user on the fetched page."),
			"asset":  strProp("Filter by asset on the fetched page."),
			"input":  strProp("Filter by command text on the fetched page."),
			"limit":  intProp("Page size, default 20, max 100."),
			"offset": intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.CommandQuery{
				Since:  strArg(args, "since"),
				Until:  strArg(args, "until"),
				User:   strArg(args, "user"),
				Asset:  strArg(args, "asset"),
				Input:  strArg(args, "input"),
				Limit:  intArg(args, "limit"),
				Offset: intArg(args, "offset"),
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolCommandList, mgmt.PathCommands,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListCommands(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					note := ""
					if q.User != "" || q.Asset != "" || q.Input != "" {
						note = windowNote(mgmt.Window(q.Limit, true))
					}
					if page.Count == 0 {
						note = joinNote(note, "the server reports no command records; command logging may be off on this instance")
					}
					return pageOf(page, note), nil
				})
		})
}

// nodeTreeTool builds jms_node_tree.
func (s *Server) nodeTreeTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolNodeTree,
		"Show the JumpServer asset node tree (the tree assets are filed under).",
		mergeProps(nil),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolNodeTree, mgmt.PathNodes,
				func(c *api.Client) (adminPage, error) {
					nodes, err := mgmt.ListNodes(ctx, c)
					if err != nil {
						return adminPage{}, err
					}
					return adminPage{Count: len(nodes), Returned: len(nodes), Items: nodes}, nil
				})
		})
}

// platformListTool builds jms_platform_list.
func (s *Server) platformListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolPlatformList,
		"List JumpServer asset platform templates (the platform id used when creating an asset).",
		mergeProps(map[string]map[string]any{
			"keyword": strProp("Match platform name or type."),
			"limit":   intProp("Page size, default 20, max 100."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			keyword := strArg(args, "keyword")
			limit := intArg(args, "limit")
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolPlatformList, mgmt.PathPlatforms,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListPlatforms(ctx, c, keyword, limit)
					if err != nil {
						return adminPage{}, err
					}
					note := ""
					if keyword != "" {
						note = windowNote(mgmt.MaxLimit)
					}
					return pageOf(page, note), nil
				})
		})
}

// accountListTool builds jms_account_list.
func (s *Server) accountListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolAccountList,
		"List asset accounts (never their secrets): which login exists on which asset, and whether it is privileged.",
		mergeProps(map[string]map[string]any{
			"asset":      strProp("Match asset name or id on the fetched page."),
			"username":   strProp("Match account username or name on the fetched page."),
			"privileged": boolProp("Only privileged accounts."),
			"limit":      intProp("Page size, default 20, max 100."),
			"offset":     intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.AccountQuery{
				Asset:      strArg(args, "asset"),
				Username:   strArg(args, "username"),
				Privileged: boolArg(args, "privileged"),
				Limit:      intArg(args, "limit"),
				Offset:     intArg(args, "offset"),
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolAccountList, mgmt.PathAccounts,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListAccounts(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					note := ""
					if q.Asset != "" || q.Username != "" || q.Privileged {
						note = windowNote(mgmt.Window(q.Limit, true))
					}
					return pageOf(page, joinNote(note, "secret values are never returned")), nil
				})
		})
}

// identityListTool builds jms_identity_list.
func (s *Server) identityListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolIdentityList,
		"List JumpServer users, groups, roles, organizations, or the members of one organization.",
		mergeProps(map[string]map[string]any{
			"kind":   strProp("users | groups | roles | orgs | org_members (default users)."),
			"org":    strProp("Organization id; narrows users/org_members to one organization."),
			"limit":  intProp("Page size, default 20, max 100."),
			"offset": intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.IdentityQuery{
				Kind:   strArg(args, "kind"),
				Org:    strArg(args, "org"),
				Limit:  intArg(args, "limit"),
				Offset: intArg(args, "offset"),
			}
			if q.Kind == "" {
				q.Kind = mgmt.IdentUsers
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolIdentityList, mgmt.PathUsers,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListIdentity(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					return adminPage{Count: page.Count, Returned: len(page.Results), Items: page.Results}, nil
				})
		})
}

// identityShowTool builds jms_identity_show.
func (s *Server) identityShowTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolIdentityShow,
		"Show one user, group, role or organization by id or exact name.",
		mergeProps(map[string]map[string]any{
			"kind":       strProp("users | groups | roles | orgs (default users)."),
			"id_or_name": strProp("Record id (UUID) or exact name."),
		}),
		[]string{"id_or_name"},
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			kind := strArg(args, "kind")
			if kind == "" {
				kind = mgmt.IdentUsers
			}
			slug := strArg(args, "id_or_name")
			if slug == "" {
				return errorResult(errors.New("id_or_name is required")), nil
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolIdentityShow, mgmt.PathUsers+slug+"/",
				func(c *api.Client) (adminPage, error) {
					one, err := mgmt.GetIdentity(ctx, c, kind, slug)
					if err != nil {
						return adminPage{}, err
					}
					return adminPage{Count: 1, Returned: 1, Items: one}, nil
				})
		})
}

// jobListTool builds jms_job_list.
func (s *Server) jobListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolJobList,
		"List JumpServer jobs (the ops job definitions). A 403 usually means job execution is disabled on the instance, "+
			"not that the path is wrong — jms_audit_list with kind=job still answers.",
		mergeProps(map[string]map[string]any{
			"keyword": strProp("Match job name, type or creator."),
			"since":   strProp("Only jobs created on or after this date (server-side)."),
			"until":   strProp("Only jobs created on or before this date (server-side)."),
			"limit":   intProp("Page size, default 20, max 100."),
			"offset":  intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.JobQuery{
				Keyword: strArg(args, "keyword"),
				Since:   strArg(args, "since"),
				Until:   strArg(args, "until"),
				Limit:   intArg(args, "limit"),
				Offset:  intArg(args, "offset"),
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolJobList, mgmt.PathJobs,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListJobs(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					return pageOf(page, ""), nil
				})
		})
}

// jobExecutionsTool builds jms_job_execution_list.
func (s *Server) jobExecutionsTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolJobExecutions,
		"List JumpServer job executions (what ran, when, and whether it succeeded).",
		mergeProps(map[string]map[string]any{
			"keyword": strProp("Match job material, type or creator."),
			"since":   strProp("Only executions starting on or after this date (server-side)."),
			"until":   strProp("Only executions starting on or before this date (server-side)."),
			"limit":   intProp("Page size, default 20, max 100."),
			"offset":  intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.JobQuery{
				Keyword: strArg(args, "keyword"),
				Since:   strArg(args, "since"),
				Until:   strArg(args, "until"),
				Limit:   intArg(args, "limit"),
				Offset:  intArg(args, "offset"),
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolJobExecutions, mgmt.PathJobExecutions,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListJobExecutions(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					return pageOf(page, ""), nil
				})
		})
}

// ticketListTool builds jms_ticket_list.
func (s *Server) ticketListTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolTicketList,
		"List JumpServer work orders (tickets) with status and applicant.",
		mergeProps(map[string]map[string]any{
			"status":    strProp("Match status on the fetched page."),
			"type":      strProp("Match ticket type on the fetched page."),
			"applicant": strProp("Match applicant or assignee on the fetched page."),
			"limit":     intProp("Page size, default 20, max 100."),
			"offset":    intProp("Skip this many records."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			q := mgmt.TicketQuery{
				Status:    strArg(args, "status"),
				Type:      strArg(args, "type"),
				Applicant: strArg(args, "applicant"),
				Limit:     intArg(args, "limit"),
				Offset:    intArg(args, "offset"),
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolTicketList, mgmt.PathTickets,
				func(c *api.Client) (adminPage, error) {
					page, err := mgmt.ListTickets(ctx, c, q)
					if err != nil {
						return adminPage{}, err
					}
					note := ""
					if q.Status != "" || q.Type != "" || q.Applicant != "" {
						note = windowNote(mgmt.Window(q.Limit, true))
					}
					return pageOf(page, note), nil
				})
		})
}

// ticketShowTool builds jms_ticket_show.
func (s *Server) ticketShowTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolTicketShow,
		"Show one work order (ticket) by id.",
		mergeProps(map[string]map[string]any{
			"ticket": strProp("Ticket id from jms_ticket_list."),
		}),
		[]string{"ticket"},
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			id := strArg(args, "ticket")
			if id == "" {
				return errorResult(errors.New("ticket is required")), nil
			}
			return s.adminRead(ctx, strArg(args, "server"), strArg(args, "config_path"), ToolTicketShow, mgmt.PathTickets+id+"/",
				func(c *api.Client) (adminPage, error) {
					one, err := mgmt.GetTicket(ctx, c, id)
					if err != nil {
						return adminPage{}, err
					}
					return adminPage{Count: 1, Returned: 1, Items: one}, nil
				})
		})
}

// localAuditTool builds jms_local_audit, the local exec/API audit stream.
//
// It reads the same JSONL files `jms log show` renders, so "what did the
// agents do on this machine" is answerable from inside the agent, without a
// terminal and without server permissions.
func (s *Server) localAuditTool() (*mcp.Tool, mcp.ToolHandler) {
	return s.adminTool(
		ToolLocalAudit,
		"Read the local jms audit stream (commands and management calls issued from this machine), "+
			"the same records `jms log show` prints. No server permission needed.",
		mergeProps(map[string]map[string]any{
			"since":  strProp("Only events newer than this Go duration, e.g. 24h or 30m."),
			"kind":   strProp("Only events of this kind, e.g. exec.end, api.read, error."),
			"asset":  strProp("Only events for this asset."),
			"failed": boolProp("Only failed events."),
			"limit":  intProp("Return at most this many newest events, default 50, max 500."),
		}),
		nil,
		func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error) {
			dir := s.opts.AuditDir
			if dir == "" {
				return errorResult(errors.New("no audit directory is configured for this host")), nil
			}
			events, err := obs.ReadEvents(dir)
			if err != nil {
				return errorResult(err), nil
			}
			events = filterLocalAudit(events, localAuditFilter{
				since:  strArg(args, "since"),
				kind:   strArg(args, "kind"),
				asset:  strArg(args, "asset"),
				failed: boolArg(args, "failed"),
			})
			limit := intArg(args, "limit")
			if limit <= 0 || limit > 500 {
				limit = 50
			}
			total := len(events)
			if total > limit {
				events = events[total-limit:]
			}
			return textResult(renderPage(adminPage{
				Count:    total,
				Returned: len(events),
				Items:    events,
				Note:     "newest last",
			})), nil
		})
}

// localAuditFilter is the parsed jms_local_audit filter set.
type localAuditFilter struct {
	since  string
	kind   string
	asset  string
	failed bool
}

// filterLocalAudit applies the local filters.
func filterLocalAudit(events []obs.Event, f localAuditFilter) []obs.Event {
	cutoff := time.Time{}
	if f.since != "" {
		if d, err := time.ParseDuration(f.since); err == nil {
			cutoff = time.Now().Add(-d)
		}
	}
	kind := f.kind
	out := make([]obs.Event, 0, len(events))
	for _, e := range events {
		if !cutoff.IsZero() && e.TS.Before(cutoff) {
			continue
		}
		if kind != "" && !strings.EqualFold(string(e.Kind), kind) {
			continue
		}
		if f.asset != "" && e.Asset != f.asset {
			continue
		}
		if f.failed && e.Error == "" && (e.ExitCode == nil || *e.ExitCode == 0) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// joinNote joins two notes, skipping the empty one.
func joinNote(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += "; "
		}
		out += p
	}
	return out
}
