package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/permits"
)

// TestRenderPermissionListShowsGroupsAndNodes pins the read-side fix.
//
// Before this, a rule was rendered as users= and assets= only, so a
// group-only rule (云平台测试) and a node-only rule (DTU开发) both came
// out as blank lines that looked like broken data.
func TestRenderPermissionListShowsGroupsAndNodes(t *testing.T) {
	listed := []permits.Permission{
		{
			Name:       "云平台测试",
			UserGroups: []permits.Relation{{ID: "g1", Name: "云平台运维"}},
			Assets:     []permits.Relation{{ID: "a1", Name: "预演01"}},
			Protocols:  []string{"all"},
			Actions:    []permits.Action{{Value: "connect"}},
		},
		{
			Name:      "DTU开发",
			Assets:    []permits.Relation{},
			Nodes:     []permits.Relation{{ID: "n1", Name: "DTU节点"}},
			Protocols: []string{"all"},
			Actions:   []permits.Action{{Value: "connect"}},
		},
	}

	got := renderPermissionList(listed)
	for _, want := range []string{
		"groups=云平台运维",
		"nodes=DTU节点",
		"users=",
		"assets=预演01",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// TestAssetGrantSchemaDeclaresUserGroups keeps the schema honest: the
// handlers decode []string, so a "string" declaration would make a
// spec-following client send a value that fails to decode. The old
// "users" property had exactly that bug.
func TestAssetGrantSchemaDeclaresUserGroups(t *testing.T) {
	s := &Server{opts: Options{}}
	tool, _ := s.assetGrantTool()

	var schema struct {
		Properties map[string]struct {
			Type  string `json:"type"`
			Items struct {
				Type string `json:"type"`
			} `json:"items"`
		} `json:"properties"`
	}
	raw, ok := tool.InputSchema.(json.RawMessage)
	if !ok {
		t.Fatalf("schema is %T, want json.RawMessage", tool.InputSchema)
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	for _, name := range []string{"users", "user_groups", "accounts", "protocols", "actions"} {
		prop, ok := schema.Properties[name]
		if !ok {
			t.Errorf("property %q is missing", name)
			continue
		}
		if prop.Type != "array" || prop.Items.Type != "string" {
			t.Errorf("%s type = %q items = %q, want array of string", name, prop.Type, prop.Items.Type)
		}
	}
}

// grantHTTPFake answers the calls jms_asset_grant makes, recording the
// permission body. The asset is resolved by name through the list.
type grantHTTPFake struct {
	permBody map[string]any
	posts    int
	profiles int
	groups   string
}

func (g *grantHTTPFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/assets/hosts/":
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":"a1","name":"asset-a","address":"10.0.0.1",
				 "platform":{"id":1,"name":"Linux"}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/groups/":
			if g.groups != "" {
				_, _ = w.Write([]byte(g.groups))
				return
			}
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":"g1","name":"云平台运维","users_amount":2}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/profile/":
			g.profiles++
			_, _ = w.Write([]byte(`{"id":"self","username":"mifazhan"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/perms/asset-permissions/":
			g.posts++
			_ = json.NewDecoder(r.Body).Decode(&g.permBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"p1","name":"grant-a"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
		}
	})
}

// newGrantServer wires the fake into the MCP server's session seam.
func newGrantServer(t *testing.T, fake *grantHTTPFake) *Server {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return New(Options{
		Session: func(context.Context, string) (*auth.Session, *config.ServerConfig, error) {
			sc := &config.ServerConfig{Name: "bastion"}
			return &auth.Session{Server: sc, Client: api.New(srv.URL), BaseURL: srv.URL}, sc, nil
		},
	})
}

func callTool(t *testing.T, s *Server, name string, args string) *mcp.CallToolResult {
	t.Helper()
	factories := map[string]func() (*mcp.Tool, mcp.ToolHandler){
		ToolAssetGrant: s.assetGrantTool,
	}
	factory, ok := factories[name]
	if !ok {
		t.Fatalf("no factory for %s", name)
	}
	_, handler := factory()
	res, err := handler(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(args)},
	})
	if err != nil {
		t.Fatalf("%s handler: %v", name, err)
	}
	return res
}

// TestAssetGrantHandlerResolvesGroups is the handler-level witness: the
// argument is decoded, the name resolved to an id, and user_groups sent.
func TestAssetGrantHandlerResolvesGroups(t *testing.T) {
	fake := &grantHTTPFake{}
	s := newGrantServer(t, fake)

	res := callTool(t, s, ToolAssetGrant,
		`{"target":"asset-a","name":"grant-a","user_groups":["云平台运维"]}`)
	if res == nil || res.IsError {
		t.Fatalf("handler returned an error: %+v", res)
	}
	groups, _ := fake.permBody["user_groups"].([]any)
	if len(groups) != 1 || groups[0] != "g1" {
		t.Errorf("user_groups = %v, want [g1]", fake.permBody["user_groups"])
	}
	if _, present := fake.permBody["users"]; present {
		t.Errorf("users = %v, want the field omitted", fake.permBody["users"])
	}
	if fake.profiles != 0 {
		t.Errorf("profile reads = %d; a group-only grant must not add the caller", fake.profiles)
	}
}

// TestAssetGrantHandlerFailsOnUnknownGroup closes the "grant silently
// succeeds with no principal" hole at the tool boundary.
func TestAssetGrantHandlerFailsOnUnknownGroup(t *testing.T) {
	fake := &grantHTTPFake{groups: `{"count":0,"results":[]}`}
	s := newGrantServer(t, fake)

	res := callTool(t, s, ToolAssetGrant,
		`{"target":"asset-a","name":"grant-a","user_groups":["不存在组"]}`)
	if res == nil || !res.IsError {
		t.Fatalf("handler accepted an unknown group: %+v", res)
	}
	if fake.posts != 0 {
		t.Errorf("POSTed %d permission(s) before resolving the group", fake.posts)
	}
}
