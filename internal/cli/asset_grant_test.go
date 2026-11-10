package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/permits"
)

// grantServer is a fake bastion for `jms asset grant`: it resolves one
// asset, one user and one group, and records the permission body.
type grantServer struct {
	requests []string
	permBody map[string]any
	profiles int
	posts    int
	groups   string // JSON results for the group search
}

func (g *grantServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests = append(g.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/assets/hosts/":
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":"a1","name":"asset-a","address":"10.0.0.1",
				 "platform":{"id":1,"name":"Linux"}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/users/":
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":"u9","username":"alice","name":"Alice"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/groups/":
			if g.groups != "" {
				_, _ = w.Write([]byte(g.groups))
				return
			}
			_, _ = w.Write([]byte(`{"count":1,"results":[
				{"id":"g1","name":"云平台运维","users_amount":2}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users/profile/":
			g.profiles++
			_, _ = w.Write([]byte(`{"id":"self","username":"mifazhan","name":"米法展"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/perms/asset-permissions/":
			g.posts++
			_ = json.NewDecoder(r.Body).Decode(&g.permBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"p1","name":"grant-asset-a"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
		}
	})
}

func seedGrantFixture(t *testing.T, env *cliEnv, g *grantServer) {
	t.Helper()
	seedFakeServer(t, env, g.handler(t))
}

// seedFakeServer points the CLI's session at an httptest server.
func seedFakeServer(t *testing.T, env *cliEnv, handler http.Handler) {
	t.Helper()
	env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")
	if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	env.clientURL = srv.URL
}

func TestAssetGrantWithGroupOnlyDoesNotFallBackToSelf(t *testing.T) {
	env := newCLIEnv(t)
	g := &grantServer{}
	seedGrantFixture(t, env, g)

	if code := env.run("asset", "grant", "asset-a", "--group", "云平台运维", "--name", "grant-a"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	groups, _ := g.permBody["user_groups"].([]any)
	if len(groups) != 1 || groups[0] != "g1" {
		t.Errorf("user_groups = %v, want [g1]", g.permBody["user_groups"])
	}
	if _, present := g.permBody["users"]; present {
		t.Errorf("users = %v, want the field omitted for a group-only grant", g.permBody["users"])
	}
	// A group-only grant must not silently add the caller as well.
	if g.profiles != 0 {
		t.Errorf("profile was read %d time(s); the grant should not add the current user", g.profiles)
	}
}

func TestAssetGrantWithUserAndGroupSendsBoth(t *testing.T) {
	env := newCLIEnv(t)
	g := &grantServer{}
	seedGrantFixture(t, env, g)

	if code := env.run("asset", "grant", "asset-a",
		"--user", "alice", "--group", "云平台运维", "--name", "grant-a"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	users, _ := g.permBody["users"].([]any)
	if len(users) != 1 || users[0] != "u9" {
		t.Errorf("users = %v, want [u9] (resolved from the username)", g.permBody["users"])
	}
	groups, _ := g.permBody["user_groups"].([]any)
	if len(groups) != 1 || groups[0] != "g1" {
		t.Errorf("user_groups = %v, want [g1]", g.permBody["user_groups"])
	}
	if g.profiles != 0 {
		t.Errorf("profile was read %d time(s); explicit principals must not add the current user", g.profiles)
	}
}

// TestPrintPermissionTableShowsGroupsAndNodes pins the columns the
// read-only live check relies on.
func TestPrintPermissionTableShowsGroupsAndNodes(t *testing.T) {
	var out bytes.Buffer
	printPermissionTable(&out, []permits.Permission{
		{
			Name:       "云平台测试",
			UserGroups: []permits.Relation{{ID: "g1", Name: "云平台运维"}},
			Nodes:      []permits.Relation{{ID: "n1", Name: "DTU节点"}},
			Protocols:  []string{"all"},
			Actions:    []permits.Action{{Value: "connect"}},
		},
	})

	got := out.String()
	for _, want := range []string{"Groups", "Nodes", "云平台运维", "DTU节点"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
}

// TestAssetGrantSelfStillGrantsTheCurrentUser covers the documented
// default that the group work must not have broken: no principal flag
// means --self.
func TestAssetGrantSelfStillGrantsTheCurrentUser(t *testing.T) {
	env := newCLIEnv(t)
	g := &grantServer{}
	seedGrantFixture(t, env, g)

	if code := env.run("asset", "grant", "asset-a", "--self", "--name", "grant-a"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	users, _ := g.permBody["users"].([]any)
	if len(users) != 1 || users[0] != "self" {
		t.Errorf("users = %v, want [self]", g.permBody["users"])
	}
	if _, present := g.permBody["user_groups"]; present {
		t.Errorf("user_groups = %v, want the field omitted", g.permBody["user_groups"])
	}
	if g.profiles != 1 {
		t.Errorf("profile reads = %d, want 1", g.profiles)
	}
}

// TestAssetGrantNoFlagsDefaultsToSelf covers the implicit form of the
// same default.
func TestAssetGrantNoFlagsDefaultsToSelf(t *testing.T) {
	env := newCLIEnv(t)
	g := &grantServer{}
	seedGrantFixture(t, env, g)

	if code := env.run("asset", "grant", "asset-a", "--name", "grant-a"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	users, _ := g.permBody["users"].([]any)
	if len(users) != 1 || users[0] != "self" {
		t.Errorf("users = %v, want [self]", g.permBody["users"])
	}
}

// TestAssetGrantUnknownGroupFailsBeforePosting keeps a mistyped group from
// creating a permission with no principal.
func TestAssetGrantUnknownGroupFailsBeforePosting(t *testing.T) {
	env := newCLIEnv(t)
	g := &grantServer{groups: `{"count":0,"results":[]}`}
	seedGrantFixture(t, env, g)

	if code := env.run("asset", "grant", "asset-a", "--group", "不存在组", "--name", "grant-a"); code == 0 {
		t.Fatal("exit code = 0, want a failure for an unknown group")
	}
	if g.posts != 0 {
		t.Errorf("POSTed %d permission(s); an unresolved principal must fail first", g.posts)
	}
	if !strings.Contains(env.stderr(), "不存在组") {
		t.Errorf("stderr = %q, want the unresolved name", env.stderr())
	}
}

// permissionServer serves the list summary and the expanded detail, so
// the expansion wiring is exercised end to end.
type permissionServer struct {
	details []string
}

func (p *permissionServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/perms/asset-permissions/":
			_, _ = w.Write([]byte(`{"count":2,"results":[
				{"id":"p1","name":"云平台测试","users_amount":0,"user_groups_amount":1,
				 "assets_amount":0,"nodes_amount":1,"users":null,"user_groups":null,
				 "assets":null,"nodes":null,"protocols":["all"]},
				{"id":"p2","name":"空规则","users_amount":0,"user_groups_amount":0,
				 "assets_amount":0,"nodes_amount":0,"users":null,"user_groups":null,
				 "assets":null,"nodes":null,"protocols":["all"]}
			]}`))
		case "/api/v1/perms/asset-permissions/p1/":
			p.details = append(p.details, "p1")
			_, _ = w.Write([]byte(`{"id":"p1","name":"云平台测试",
				"users":[],"user_groups":[{"id":"g1","name":"云平台测试组"}],
				"assets":[],"nodes":[{"id":"n1","name":"云则公司环境"}],
				"protocols":["all"],"actions":[{"value":"connect"}]}`))
		case "/api/v1/perms/asset-permissions/p2/":
			p.details = append(p.details, "p2")
			_, _ = w.Write([]byte(`{"id":"p2","name":"空规则","users":[],"user_groups":[],
				"assets":[],"nodes":[],"protocols":["all"],"actions":[{"value":"connect"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not found"}`))
		}
	})
}

// TestAssetPermissionsExpandsGroupAndNodeOnlyRules is the end-to-end
// witness for the read fix: the list carries only counts, and a
// group-only/node-only rule used to be skipped because it has neither
// users nor assets.
func TestAssetPermissionsExpandsGroupAndNodeOnlyRules(t *testing.T) {
	env := newCLIEnv(t)
	p := &permissionServer{}
	seedFakeServer(t, env, p.handler(t))

	if code := env.run("asset", "permissions"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	stdout := env.stdout()
	for _, want := range []string{"云平台测试组", "云则公司环境", "Groups", "Nodes"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if len(p.details) != 1 || p.details[0] != "p1" {
		t.Errorf("details read = %v, want only p1 (the rule with relations)", p.details)
	}
}
