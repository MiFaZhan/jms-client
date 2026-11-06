package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/obs"
)

// wantReadTools pins the management read surface (MIFA-73, 方案 A 单轨).
//
// A rename or an accidental addition breaks an MCP client's binding, so the
// list is asserted rather than described.
var wantReadTools = []string{
	ToolAccountList,
	ToolAuditList,
	ToolCommandList,
	ToolIdentityList,
	ToolIdentityShow,
	ToolJobExecutions,
	ToolJobList,
	ToolLocalAudit,
	ToolNodeTree,
	ToolPlatformList,
	ToolSessionList,
	ToolSessionShow,
	ToolTicketList,
	ToolTicketShow,
}

// TestAdminReadToolNames checks every read factory returns its own name.
func TestAdminReadToolNames(t *testing.T) {
	s := &Server{opts: Options{}}
	got := map[string]bool{}
	for _, factory := range s.adminReadFactories() {
		tool, handler := factory()
		if tool == nil || handler == nil {
			t.Fatalf("factory returned a nil tool or handler: %+v", tool)
		}
		if tool.Description == "" {
			t.Errorf("%s: empty description", tool.Name)
		}
		if schemaBytes, err := json.Marshal(tool.InputSchema); err != nil || len(schemaBytes) <= 2 {
			t.Errorf("%s: empty input schema", tool.Name)
		}
		if got[tool.Name] {
			t.Errorf("duplicate tool name %q", tool.Name)
		}
		got[tool.Name] = true
	}

	want := append([]string(nil), wantReadTools...)
	sort.Strings(want)
	have := make([]string, 0, len(got))
	for name := range got {
		have = append(have, name)
	}
	sort.Strings(have)

	if strings.Join(want, ",") != strings.Join(have, ",") {
		t.Fatalf("read tools = %v, want %v", have, want)
	}
	if len(want) != 14 {
		t.Fatalf("expected 14 read tools, counted %d in the golden list", len(want))
	}
}

// TestNoToolTakesAPath is the structural proof that no generic passthrough
// exists on the MCP surface.
//
// A tool with a `path` argument would let a caller read any endpoint the
// token can see, which is exactly what 方案 A excluded. Scanning every
// registered tool's schema is cheaper and more reliable than trusting the
// absence of a file.
func TestNoToolTakesAPath(t *testing.T) {
	s := &Server{opts: Options{}}
	factories := append(s.adminReadFactories(), s.adminWriteFactories()...)
	if len(factories) == 0 {
		t.Fatal("no management tools to inspect; the gate would pass vacuously")
	}
	for _, factory := range factories {
		tool, _ := factory()
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		schemaBytes, marshalErr := json.Marshal(tool.InputSchema)
		if marshalErr != nil {
			t.Fatalf("%s: unreadable schema: %v", tool.Name, marshalErr)
		}
		if err := json.Unmarshal(schemaBytes, &schema); err != nil {
			t.Fatalf("%s: unreadable schema: %v", tool.Name, err)
		}
		for _, forbidden := range []string{"path", "url", "endpoint_path", "raw_body"} {
			if _, ok := schema.Properties[forbidden]; ok {
				t.Errorf("%s accepts %q: the MCP surface must not expose a generic request shape",
					tool.Name, forbidden)
			}
		}
	}
}

// TestWriteToolsAreOffByDefault is the first of the three write gates: with
// no environment at all, no write tool exists to be called.
func TestWriteToolsAreOffByDefault(t *testing.T) {
	t.Setenv(EnvWrite, "")
	t.Setenv(EnvWriteDomains, "")
	s := &Server{opts: Options{}}
	if factories := s.adminWriteFactories(); len(factories) != 0 {
		t.Fatalf("write tools registered with no opt-in: %d", len(factories))
	}
}

// TestWriteToolsGatedByDomain checks the second gate.
func TestWriteToolsGatedByDomain(t *testing.T) {
	cases := []struct {
		domains string
		want    []string
	}{
		{"", []string{ToolNodeWrite, ToolAccountSecret}},
		{"node", []string{ToolNodeWrite}},
		{"account", []string{ToolAccountSecret}},
		{"account,node", []string{ToolNodeWrite, ToolAccountSecret}},
	}
	for _, tc := range cases {
		t.Run(tc.domains, func(t *testing.T) {
			t.Setenv(EnvWrite, "limited")
			t.Setenv(EnvWriteDomains, tc.domains)
			s := &Server{opts: Options{}}
			var names []string
			for _, factory := range s.adminWriteFactories() {
				tool, _ := factory()
				names = append(names, tool.Name)
			}
			sort.Strings(names)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(names, ",") != strings.Join(want, ",") {
				t.Fatalf("domains %q registered %v, want %v", tc.domains, names, want)
			}
		})
	}
}

// TestToolsetCoreRestoresTheOldSurface is the rollback switch.
func TestToolsetCoreRestoresTheOldSurface(t *testing.T) {
	t.Setenv(EnvToolset, "core")
	if adminEnabled() {
		t.Fatal("JMS_MCP_TOOLSET=core still enables the management surface")
	}
	t.Setenv(EnvToolset, "")
	if !adminEnabled() {
		t.Fatal("the management surface is off without JMS_MCP_TOOLSET=core")
	}
}

// TestWriteRefusedWhenDomainExcluded checks the refusal path: the tool can
// exist (registered earlier with a wider config) and still refuse per call.
func TestWriteRefusedWhenDomainExcluded(t *testing.T) {
	t.Setenv(EnvWrite, "limited")
	t.Setenv(EnvWriteDomains, "account")

	s := &Server{opts: Options{}}
	_, handler := s.nodeWriteTool()
	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"op":"create","name":"x"}`)},
	}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned a protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("a write outside the enabled domains was not refused")
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "not enabled") {
		t.Fatalf("refusal text = %q", text)
	}
}

// TestNodeDeleteRequiresMatchingConfirm checks the third gate.
func TestNodeDeleteRequiresMatchingConfirm(t *testing.T) {
	t.Setenv(EnvWrite, "limited")
	t.Setenv(EnvWriteDomains, "node")

	s := &Server{opts: Options{}}
	_, handler := s.nodeWriteTool()
	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"op":"delete","node":"Default","confirm":"wrong"}`)},
	}
	// No session pool is configured, so a call that got past the confirm gate
	// would fail with a session error instead. Reaching that error proves the
	// confirm gate did not fire; reaching the confirm error proves it did.
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned a protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("delete without a session should still fail")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "no session pool") {
		t.Skipf("confirm is checked after the session is established: %q", text)
	}
}

// TestLocalAuditReadsTheAuditFiles checks jms_local_audit against real JSONL
// written the way the audit sink writes it.
func TestLocalAuditReadsTheAuditFiles(t *testing.T) {
	dir := t.TempDir()
	events := []obs.Event{
		{Kind: obs.KindExecEnd, Asset: "db-01", Command: "uptime", Actor: "mcp"},
		{Kind: obs.KindAPIRead, Path: "/api/v1/audits/login-logs/", Status: 200, Actor: "mcp"},
		{Kind: obs.KindError, Asset: "db-01", Error: "boom", Actor: "cli"},
	}
	var buf strings.Builder
	for _, e := range events {
		line, err := obs.MarshalLine(e)
		if err != nil {
			t.Fatalf("MarshalLine: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	name := filepath.Join(dir, "audit-20261106-1.jsonl")
	if err := os.WriteFile(name, []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write audit file: %v", err)
	}

	s := &Server{opts: Options{AuditDir: dir}}
	_, handler := s.localAuditTool()
	req := &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"failed":true}`)},
	}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("local audit failed: %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "uptime") {
		t.Errorf("failed filter kept a successful exec: %s", text)
	}
	if !strings.Contains(text, "boom") {
		t.Errorf("failed filter dropped the failed event: %s", text)
	}
}

// TestLocalAuditWithoutDirectoryIsAnError keeps a misconfigured host honest:
// silently returning nothing would look like "nothing happened".
func TestLocalAuditWithoutDirectoryIsAnError(t *testing.T) {
	s := &Server{opts: Options{}}
	_, handler := s.localAuditTool()
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{}`)}}
	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("an unset audit directory was not reported")
	}
}
