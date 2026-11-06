package mgmt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// newTestClient returns a client bound to an httptest server running handler.
func newTestClient(t *testing.T, handler http.HandlerFunc) *api.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return api.New(srv.URL)
}

// jsonHandler answers every request with body.
func jsonHandler(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func TestListAuditNormalizesLogin(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, jsonHandler(t, `{
		"count": 109,
		"results": [
			{"id":"a1","username":"alice","ip":"10.0.0.8","city":"济南","status":"success",
			 "type":"W","backend_display":"数据库","reason_display":"","datetime":"2026-11-06T09:00:00+08:00"}
		]
	}`))

	page, err := ListAudit(context.Background(), client, AuditQuery{Kind: AuditLogin, Limit: 5})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if page.Count != 109 || len(page.Results) != 1 {
		t.Fatalf("page = %+v, want count 109 with one row", page)
	}
	got := page.Results[0]
	if got.User != "alice" || got.SourceIP != "10.0.0.8" || got.Action != "login" || got.Result != "success" {
		t.Errorf("normalized row = %+v", got)
	}
	if got.Object != "W/数据库" {
		t.Errorf("object = %q, want %q", got.Object, "W/数据库")
	}
	if got.Time != "2026-11-06T09:00:00+08:00" {
		t.Errorf("time was rewritten: %q", got.Time)
	}
}

func TestListAuditSendsDateFilters(t *testing.T) {
	t.Parallel()

	var gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"count":0,"results":[]}`)
	})

	if _, err := ListAudit(context.Background(), client, AuditQuery{
		Kind: AuditOperate, Since: "2026-11-01", Until: "2026-11-06", Limit: 50,
	}); err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	for _, want := range []string{"date_from=2026-11-01", "date_to=2026-11-06", "limit=50"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}
}

func TestListAuditRejectsUnknownKind(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused kind must not reach the network, got %s", r.URL.Path)
	})
	if _, err := ListAudit(context.Background(), client, AuditQuery{Kind: "everything"}); err == nil {
		t.Fatal("unknown kind was accepted")
	}
}

func TestListSessionsFiltersActiveAndUser(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, jsonHandler(t, `{"count":2,"results":[
		{"id":"s1","user":"alice","asset":"db-01","protocol":"ssh","is_finished":false,"can_terminate":true,"has_replay":false},
		{"id":"s2","user":"bob","asset":"db-02","protocol":"ssh","is_finished":true,"has_replay":true,"replay_size":1024}
	]}`))

	page, err := ListSessions(context.Background(), client, SessionQuery{Active: true, Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(page.Results) != 1 || page.Results[0].ID != "s1" {
		t.Fatalf("active filter returned %+v", page.Results)
	}
	if page.Count != 2 {
		t.Errorf("count = %d, want the server total 2", page.Count)
	}

	page, err = ListSessions(context.Background(), client, SessionQuery{User: "bob", Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(page.Results) != 1 || page.Results[0].ID != "s2" {
		t.Fatalf("user filter returned %+v", page.Results)
	}
}

func TestListNodesBuildsTree(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, jsonHandler(t, `{"count":2,"results":[
		{"id":"n1","value":"Default","full_value":"Default"},
		{"id":"n2","value":"生产","full_value":"Default/生产"}
	]}`))

	nodes, err := ListNodes(context.Background(), client)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 || len(nodes[0].Children) != 1 {
		t.Fatalf("tree = %+v, want Default -> 生产", nodes)
	}
}

func TestListAccountsNeverReturnsASecret(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, jsonHandler(t, `{"count":1,"results":[{
		"id":"acc1","name":"root","username":"root","privileged":true,"is_active":true,
		"secret_type":{"value":"password","label":"密码"},
		"change_secret_status":{"value":"ok","label":"正常"},
		"asset":{"id":"a1","name":"db-01"},
		"secret":"hunter2","password":"hunter2"
	}]}`))

	page, err := ListAccounts(context.Background(), client, AccountQuery{Limit: 5})
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Results))
	}
	if string(page.Results[0].SecretType) != "password" {
		t.Errorf("secret_type = %q, want the object's value", page.Results[0].SecretType)
	}
	encoded, err := json.Marshal(page.Results)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "hunter2") {
		t.Fatalf("a secret reached the tool output: %s", encoded)
	}
}

func TestListJobsSurfacesDisabledFeature(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"detail":"命令执行已禁用"}`)
	})

	_, err := ListJobs(context.Background(), client, JobQuery{Limit: 5})
	if err == nil {
		t.Fatal("403 was swallowed")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "命令执行已禁用") {
		t.Fatalf("error = %v, want the server's 403 detail", err)
	}
}

func TestListIdentitySendsOrgFilter(t *testing.T) {
	t.Parallel()

	var gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"count":9,"results":[{"id":"u1","username":"alice"}]}`)
	})

	page, err := ListIdentity(context.Background(), client, IdentityQuery{
		Kind: IdentOrgMembers, Org: "00000000-0000-0000-0000-000000000002", Limit: 5,
	})
	if err != nil {
		t.Fatalf("ListIdentity: %v", err)
	}
	if !strings.Contains(gotQuery, "org=00000000-0000-0000-0000-000000000002") {
		t.Errorf("query %q is missing the org filter", gotQuery)
	}
	if page.Count != 9 || len(page.Results) != 1 {
		t.Errorf("page = %+v", page)
	}
}

func TestCreateNodeSendsFullValue(t *testing.T) {
	t.Parallel()

	var (
		gotMethod string
		gotBody   map[string]any
	)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// ResolveNode looks the parent up by id first.
			_, _ = io.WriteString(w, `{"count":1,"results":[{"id":"n1","value":"Default","full_value":"Default"}]}`)
			return
		}
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"id":"n9","value":"机房A","full_value":"Default/机房A"}`)
	})

	node, err := CreateNode(context.Background(), client, "n1", "机房A")
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotBody["value"] != "机房A" {
		t.Errorf("body value = %v", gotBody["value"])
	}
	if gotBody["full_value"] != "Default/机房A" {
		t.Errorf("full_value = %v, want the parent path joined with the name", gotBody["full_value"])
	}
	if node.ID != "n9" {
		t.Errorf("node = %+v", node)
	}
}

func TestCreateNodeRefusesSlashInName(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused create must not reach the network")
	})
	if _, err := CreateNode(context.Background(), client, "", "a/b"); err == nil {
		t.Fatal("a slash in a node name was accepted")
	}
}

func TestDeleteNodePropagatesServerRefusal(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"count":1,"results":[{"id":"n1","value":"Default","full_value":"Default"}]}`)
		case http.MethodDelete:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"detail":"该节点下存在资产，无法删除"}`)
		}
	})

	_, err := DeleteNode(context.Background(), client, "Default")
	if err == nil || !strings.Contains(err.Error(), "无法删除") {
		t.Fatalf("error = %v, want the server's refusal", err)
	}
}

func TestChangeAccountSecretPostsAccountIDs(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotBody map[string]any
	)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	})

	if err := ChangeAccountSecret(context.Background(), client, []string{"acc1"}); err != nil {
		t.Fatalf("ChangeAccountSecret: %v", err)
	}
	if !strings.HasSuffix(gotPath, "change-secret/") {
		t.Errorf("path = %q, want the account secret-rotation view", gotPath)
	}
	ids, ok := gotBody["accounts"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "acc1" {
		t.Fatalf("body = %v, want accounts:[acc1]", gotBody)
	}
}

func TestGetIdentityFallsBackToExactName(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "search=") {
			t.Errorf("the name belongs in the query, not the path: %s", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/users/alice/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"Not found"}`)
			return
		}
		_, _ = io.WriteString(w, `{"count":2,"results":[
			{"id":"u1","username":"alice","name":"Alice"},
			{"id":"u2","username":"alice2","name":"Alice Two"}
		]}`)
	})

	one, err := GetIdentity(context.Background(), client, IdentUsers, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	user, ok := one.(User)
	if !ok || user.ID != "u1" {
		t.Fatalf("resolved %+v, want u1", one)
	}
}

func TestGetIdentityRefusesAmbiguousName(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathUsers {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"Not found"}`)
			return
		}
		_, _ = io.WriteString(w, `{"count":2,"results":[
			{"id":"g1","name":"ops","comment":"a"},
			{"id":"g2","name":"ops","comment":"b"}
		]}`)
	})

	if _, err := GetIdentity(context.Background(), client, IdentGroups, "ops"); err == nil {
		t.Fatal("an ambiguous name resolved silently")
	}
}

// TestListAuditDecodesLabeledFields pins the shapes the live instance
// actually returns.
//
// This is a regression test for a real failure: the first end-to-end call
// answered `invalid JSON response` because `type` and `status` arrive as
// {"value","label"} objects on a v4 login record, not as strings. A decoded
// struct that assumes strings turns a working endpoint into a broken tool.
func TestListAuditDecodesLabeledFields(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, jsonHandler(t, `{"count":1,"results":[{
		"id":"d1a607fd","username":"alice","type":{"value":"W","label":"Web"},
		"ip":"192.168.37.1","city":"LAN","user_agent":"Go-http-client/1.1",
		"mfa":{"value":1,"label":"启用"},
		"reason":"","reason_display":"","backend":"Password","backend_display":"密码",
		"status":{"value":true,"label":"成功"},
		"datetime":"2026/11/06 14:45:18 +0800"
	}]}`))

	page, err := ListAudit(context.Background(), client, AuditQuery{Kind: AuditLogin, Limit: 1})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	got := page.Results[0]
	if got.Result != "成功" {
		t.Errorf("result = %q, want the label of a boolean status", got.Result)
	}
	if got.Object != "W/密码" {
		t.Errorf("object = %q, want type/backend", got.Object)
	}
	if got.Time != "2026/11/06 14:45:18 +0800" {
		t.Errorf("time = %q, want the server's own spelling", got.Time)
	}
}

func TestFlexStringShapes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`"plain"`:                     "plain",
		`{"value":"password"}`:        "password",
		`{"value":true,"label":"成功"}`: "成功",
		`{"value":1,"label":"启用"}`:    "启用",
		`1`:                           "1",
		`false`:                       "false",
		`null`:                        "",
		`""`:                          "",
	}
	for raw, want := range cases {
		var got FlexString
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Errorf("unmarshal %s: %v", raw, err)
			continue
		}
		if string(got) != want {
			t.Errorf("unmarshal %s = %q, want %q", raw, got, want)
		}
	}
}
