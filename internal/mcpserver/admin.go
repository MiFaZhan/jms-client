package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/mgmt"
	"github.com/MiFaZhan/jms-client/internal/obs"
)

// Tool names for the management read surface (MIFA-73, 方案 A 单轨).
//
// There is intentionally no generic "call any path" tool here, not even a
// read-only one: the caller of an MCP tool is a model, so a `path` argument
// would put "read anything the token can see" one sentence away, and the
// boundary that matters (which fields are secrets, whose session log this
// is) cannot be expressed as a path prefix. Each of these tools is a
// verb with a fixed shape instead.
const (
	ToolAuditList     = "jms_audit_list"
	ToolSessionList   = "jms_session_list"
	ToolSessionShow   = "jms_session_show"
	ToolCommandList   = "jms_command_list"
	ToolNodeTree      = "jms_node_tree"
	ToolPlatformList  = "jms_platform_list"
	ToolAccountList   = "jms_account_list"
	ToolIdentityList  = "jms_identity_list"
	ToolIdentityShow  = "jms_identity_show"
	ToolJobList       = "jms_job_list"
	ToolJobExecutions = "jms_job_execution_list"
	ToolTicketList    = "jms_ticket_list"
	ToolTicketShow    = "jms_ticket_show"
	ToolLocalAudit    = "jms_local_audit"
)

// Environment switches for the management surface.
//
// All of them are read at tool-registration time, so a restart is enough to
// change the surface and no configuration file has to change. That matters
// for rollback: unsetting a variable restores the previous surface without
// touching anything on disk.
const (
	// EnvToolset set to "core" registers only the pre-MIFA-73 tools.
	EnvToolset = "JMS_MCP_TOOLSET"
	// EnvWrite set to "limited" registers the write tools at all.
	EnvWrite = "JMS_MCP_WRITE"
	// EnvWriteDomains is a comma list narrowing EnvWrite: "node,account".
	EnvWriteDomains = "JMS_MCP_WRITE_DOMAINS"
)

// maxResultBytes bounds one tool result.
//
// A management collection can be huge (the reference instance held 166,995
// login records), and a tool result is charged to the caller's context
// window. Truncation is reported explicitly so a caller never mistakes a cut
// page for a complete answer.
const maxResultBytes = 32 * 1024

// adminEnabled reports whether the management read tools are registered.
func adminEnabled() bool {
	return strings.ToLower(strings.TrimSpace(os.Getenv(EnvToolset))) != "core"
}

// writeDomains reports whether writing is enabled and, if a domain list is
// configured, which domains are allowed.
//
// A nil map with true means "all registered write domains are allowed".
func writeDomains() (bool, map[string]bool) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(EnvWrite)), "limited") {
		return false, nil
	}
	raw := strings.TrimSpace(os.Getenv(EnvWriteDomains))
	if raw == "" {
		return true, nil
	}
	allowed := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if d := strings.ToLower(strings.TrimSpace(part)); d != "" {
			allowed[d] = true
		}
	}
	return true, allowed
}

// adminReadFactories returns the read tools in registration order.
//
// The registration path and the tests both iterate this slice, so "the tool
// list snapshot" is an assertion about the code that actually registers
// them, not a parallel list that can drift.
func (s *Server) adminReadFactories() []func() (*mcp.Tool, mcp.ToolHandler) {
	return []func() (*mcp.Tool, mcp.ToolHandler){
		s.auditListTool,
		s.sessionListTool,
		s.sessionShowTool,
		s.commandListTool,
		s.nodeTreeTool,
		s.platformListTool,
		s.accountListTool,
		s.identityListTool,
		s.identityShowTool,
		s.jobListTool,
		s.jobExecutionsTool,
		s.ticketListTool,
		s.ticketShowTool,
		s.localAuditTool,
	}
}

// registerAdmin registers the management read tools, and the write tools
// when they are enabled.
func (s *Server) registerAdmin(srv *mcp.Server) {
	if !adminEnabled() {
		return
	}
	for _, factory := range s.adminReadFactories() {
		register(srv, factory)
	}
	s.registerAdminWrites(srv)
}

// adminPage is what a management read tool reports.
type adminPage struct {
	// Count is the server's total for the query.
	Count int `json:"count"`
	// Returned is how many records are in this answer.
	Returned int `json:"returned"`
	// Items is the page.
	Items any `json:"items"`
	// Note explains a narrowed answer: a truncated page, or a filter that
	// only saw the fetched window.
	Note string `json:"note,omitempty"`
}

// adminRead runs one management read, records it, and renders the page.
//
// Centralizing this is what keeps the fourteen read tools from each growing
// their own audit and truncation behaviour — the failure mode this codebase
// already paid for once, when a failed call left no trace at all.
func (s *Server) adminRead(ctx context.Context, server, configPath, op, path string, run func(*api.Client) (adminPage, error)) (*mcp.CallToolResult, error) {
	sess, srv, err := s.sessionFor(ctx, server, configPath)
	if err != nil {
		s.failOp(server, "", op, err)
		return errorResult(err), nil
	}
	if sess == nil || sess.Client == nil {
		err := errors.New("session has no API client")
		s.failOp(server, "", op, err)
		return errorResult(err), nil
	}

	started := time.Now()
	page, err := run(sess.Client)
	event := obs.Event{
		Kind:       obs.KindAPIRead,
		Server:     srv.Name,
		Path:       path,
		Status:     200,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if err != nil {
		event.Error = err.Error()
		event.Kind = obs.KindError
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
	return textResult(renderPage(page)), nil
}

// renderPage renders one page as JSON, truncated to maxResultBytes.
func renderPage(page adminPage) string {
	body, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		return fmt.Sprintf("count=%d returned=%d (rendering failed: %v)", page.Count, page.Returned, err)
	}
	text := string(body)
	if len(text) <= maxResultBytes {
		return text
	}
	return text[:maxResultBytes] + "\n… truncated at " + fmt.Sprint(maxResultBytes) +
		" bytes; narrow the filters or lower limit\n"
}

// pageOf builds a report from a value page.
func pageOf[T any](page mgmt.Page[T], note string) adminPage {
	return adminPage{Count: page.Count, Returned: len(page.Results), Items: page.Results, Note: note}
}

// windowNote explains an answer that filtered only the fetched window.
func windowNote(scanned int) string {
	return fmt.Sprintf("filter applied to the %d most recent records only, not to the whole collection", scanned)
}

// requireSession is the shared prologue of every read handler.
func (s *Server) adminTool(name, description string, props map[string]map[string]any, required []string,
	fn func(ctx context.Context, args map[string]json.RawMessage) (*mcp.CallToolResult, error)) (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{Name: name, Description: description, InputSchema: objectSchema(required, props)}
	return tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]json.RawMessage
		if err := decodeArgs(req, &args); err != nil {
			return errorResult(err), nil
		}
		return fn(ctx, args)
	}
}

// strArg reads a string argument.
func strArg(args map[string]json.RawMessage, key string) string {
	raw, ok := args[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// intArg reads an integer argument.
func intArg(args map[string]json.RawMessage, key string) int {
	raw, ok := args[key]
	if !ok || len(raw) == 0 {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

// boolArg reads a boolean argument.
func boolArg(args map[string]json.RawMessage, key string) bool {
	raw, ok := args[key]
	if !ok || len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false
	}
	return b
}

// adminCommonProps are the two arguments every management tool takes.
func adminCommonProps() map[string]map[string]any {
	return map[string]map[string]any{
		"server":      strProp("Server alias; the default server when omitted."),
		"config_path": strProp("Configuration file path override."),
	}
}

// mergeProps merges the per-tool properties over the common ones.
func mergeProps(extra map[string]map[string]any) map[string]map[string]any {
	out := adminCommonProps()
	for k, v := range extra {
		out[k] = v
	}
	return out
}
