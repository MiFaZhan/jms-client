package assetsmanage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
)

// newTestClient starts a test server and returns an API client for it.
func newTestClient(t *testing.T, h http.Handler) (*api.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return api.New(srv.URL), srv
}

// decodeBody decodes the request body into a map.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func TestCreatePostsToHostsEndpointWithFullPayload(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody = decodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "asset-1",
			"name":    "主机-云则预演01",
			"address": "192.168.100.252",
		})
	}))

	created, err := Create(context.Background(), client, Spec{
		Name:      "主机-云则预演01",
		Address:   "192.168.100.252",
		NodeID:    "node-1",
		Comment:   "预演01",
		Accounts:  []AccountSpec{{Name: "yunze", Username: "yunze", Password: "secret"}},
		Protocols: []string{"ssh"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	// The plural /assets/ view refuses POST; the hosts view is the one
	// that implements creation. The expected path is literal so a wrong
	// constant cannot make this assertion pass.
	if gotPath != "/api/v1/assets/hosts/" {
		t.Errorf("path = %s, want /api/v1/assets/hosts/", gotPath)
	}
	if created.ID != "asset-1" {
		t.Errorf("id = %q, want asset-1", created.ID)
	}
	if gotBody["name"] != "主机-云则预演01" {
		t.Errorf("name = %v", gotBody["name"])
	}
	if gotBody["address"] != "192.168.100.252" {
		t.Errorf("address = %v", gotBody["address"])
	}
	if gotBody["platform"] != float64(PlatformIDLinux) {
		t.Errorf("platform = %v, want %d", gotBody["platform"], PlatformIDLinux)
	}
	nodes, _ := gotBody["nodes"].([]any)
	if len(nodes) != 1 || nodes[0] != "node-1" {
		t.Errorf("nodes = %v, want [node-1]", gotBody["nodes"])
	}
	if gotBody["comment"] != "预演01" {
		t.Errorf("comment = %v", gotBody["comment"])
	}
	accounts, _ := gotBody["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v, want 1 entry", gotBody["accounts"])
	}
	acc, _ := accounts[0].(map[string]any)
	if acc["username"] != "yunze" {
		t.Errorf("account username = %v", acc["username"])
	}
	// The secret field is "secret". Naming it "password" is silently
	// ignored by the server and yields an account that cannot
	// authenticate, which surfaces as a misleading "认证失败".
	if acc["secret"] != "secret" {
		t.Errorf("account secret = %v, want the password under \"secret\"", acc["secret"])
	}
	if _, wrong := acc["password"]; wrong {
		t.Error("account must not carry a \"password\" field; the server ignores it")
	}
	if acc["secret_type"] != "password" {
		t.Errorf("account secret_type = %v, want password", acc["secret_type"])
	}
}

func TestCreateRejectsDuplicateNameAsConflict(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": []string{"字段必须唯一"},
		})
	}))

	// The node is given explicitly so the only request is the create
	// itself; otherwise the default-node lookup would consume the
	// scripted response first.
	_, err := Create(context.Background(), client, Spec{
		Name: "dup", Address: "1.2.3.4", NodeID: "node-1",
	})
	if err == nil {
		t.Fatal("expected an error for a duplicate name")
	}
	if !strings.Contains(err.Error(), ErrConflict.Error()) {
		t.Errorf("error = %v, want it to report ErrConflict", err)
	}
}

func TestCreateRequiresNameAndAddress(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the server must not be called for an invalid spec")
	}))

	if _, err := Create(context.Background(), client, Spec{Address: "1.2.3.4"}); err == nil {
		t.Error("expected an error for a missing name")
	}
	if _, err := Create(context.Background(), client, Spec{Name: "x"}); err == nil {
		t.Error("expected an error for a missing address")
	}
}

func TestUpdateReadsThenSendsTheWholeObject(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	sawGet := false

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			sawGet = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "a1", "name": "old-name", "address": "1.2.3.4",
				"platform": map[string]any{"id": 1, "name": "Linux"},
				"comment":  "keep-me",
				"nodes":    []map[string]any{{"id": "node-1", "name": "DEFAULT"}},
			})
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody = decodeBody(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "a1", "name": "new-name"})
	}))

	name := "new-name"
	updated, err := Update(context.Background(), client, "a1", UpdateFields{Name: &name})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	// The server treats PUT as a full replacement, so the current asset
	// has to be read first.
	if !sawGet {
		t.Error("the asset must be read before the update is sent")
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if gotPath != "/api/v1/assets/hosts/a1/" {
		t.Errorf("path = %s, want /api/v1/assets/hosts/a1/", gotPath)
	}
	if updated.Name != "new-name" {
		t.Errorf("name = %q", updated.Name)
	}
	// The fields the server requires must be present ...
	for _, key := range []string{"name", "address", "platform", "protocols"} {
		if _, ok := gotBody[key]; !ok {
			t.Errorf("body missing the required field %q: %v", key, gotBody)
		}
	}
	// ... and the fields the caller did not mention must keep their value.
	if gotBody["name"] != "new-name" {
		t.Errorf("body name = %v, want new-name", gotBody["name"])
	}
	if gotBody["address"] != "1.2.3.4" {
		t.Errorf("body address = %v, want the current address", gotBody["address"])
	}
	if gotBody["comment"] != "keep-me" {
		t.Errorf("body comment = %v, want the current comment", gotBody["comment"])
	}
}

func TestUpdateWithoutFieldsReadsInstead(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "a1", "name": "unchanged"})
	}))

	asset, err := Update(context.Background(), client, "a1", UpdateFields{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if asset.Name != "unchanged" {
		t.Errorf("name = %q", asset.Name)
	}
}

func TestDeleteUsesDeleteMethodOnTheResource(t *testing.T) {
	var gotMethod, gotPath string
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := Delete(context.Background(), client, "a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// The asset view answers DELETE on the resource itself and rejects
	// POST with method_not_allowed, so a delete cannot be a POST.
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/assets/hosts/a1/" {
		t.Errorf("path = %s, want /api/v1/assets/hosts/a1/", gotPath)
	}
}

func TestFindByNameMatchesExactly(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"id": "a1", "name": "主机-云则预演01", "address": "192.168.100.252"},
				{"id": "a2", "name": "主机-云则预演02", "address": "192.168.100.191"},
			},
		})
	}))

	asset, err := FindByName(context.Background(), client, "主机-云则预演02")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if asset.ID != "a2" {
		t.Errorf("id = %q, want a2 (a prefix match would pick a1)", asset.ID)
	}

	if _, err := FindByName(context.Background(), client, "主机-云则预演"); err == nil {
		t.Error("a partial name must not match")
	}
}

func TestListSkipsMalformedRows(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"id": "a1", "name": "good", "address": "1.1.1.1"},
				{"id": "a2", "name": 42},
			},
		})
	}))

	listed, err := List(context.Background(), client)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// The malformed row is skipped; the well-formed one must survive it,
	// exactly as the read-side assets package does.
	if len(listed) != 1 {
		t.Fatalf("len = %d, want 1 (the malformed row is skipped)", len(listed))
	}
	if listed[0].Name != "good" {
		t.Errorf("name = %q, want good", listed[0].Name)
	}
}

func TestDefaultNodeIDPrefersTheDefaultNode(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"id": "n1", "key": "1:5", "value": "DTU开发环境"},
				{"id": "n2", "key": "1", "value": "DEFAULT"},
			},
		})
	}))

	id, err := DefaultNodeID(context.Background(), client)
	if err != nil {
		t.Fatalf("DefaultNodeID: %v", err)
	}
	if id != "n2" {
		t.Errorf("id = %q, want n2 (the DEFAULT node)", id)
	}
}
