package permits

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
)

func newTestClient(t *testing.T, h http.Handler) *api.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return api.New(srv.URL)
}

func TestGrantPostsToAssetPermissionsWithDefaults(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "p1", "name": gotBody["name"],
		})
	}))

	created, err := Grant(context.Background(), client, GrantSpec{
		Name:     "grant-asset-a",
		UserIDs:  []string{"u1"},
		AssetIDs: []string{"a1"},
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// The authorization endpoint is the AssetPermission collection; the
	// read-only per-user view (/api/v1/perms/users/<id>/assets/) implements
	// no write method at all.
	//
	// The expected path is written out literally on purpose: comparing
	// against PathPermissions would still pass if the constant itself were
	// changed to a wrong value.
	if gotPath != "/api/v1/perms/asset-permissions/" {
		t.Errorf("path = %s, want /api/v1/perms/asset-permissions/", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if created.ID != "p1" {
		t.Errorf("id = %q", created.ID)
	}
	// Empty selectors must widen to "everything", not narrow to "nothing".
	if accounts, _ := gotBody["accounts"].([]any); len(accounts) != 1 || accounts[0] != AccountAll {
		t.Errorf("accounts = %v, want [%s]", gotBody["accounts"], AccountAll)
	}
	if protocols, _ := gotBody["protocols"].([]any); len(protocols) != 1 || protocols[0] != ProtocolAll {
		t.Errorf("protocols = %v, want [%s]", gotBody["protocols"], ProtocolAll)
	}
	if gotBody["is_active"] != true {
		t.Errorf("is_active = %v, want true", gotBody["is_active"])
	}
}

func TestGrantRequiresUsersAndAssets(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the server must not be called for an invalid spec")
	}))

	if _, err := Grant(context.Background(), client, GrantSpec{Name: "x", AssetIDs: []string{"a1"}}); err == nil {
		t.Error("expected an error when no user is given")
	}
	if _, err := Grant(context.Background(), client, GrantSpec{Name: "x", UserIDs: []string{"u1"}}); err == nil {
		t.Error("expected an error when no asset is given")
	}
	if _, err := Grant(context.Background(), client, GrantSpec{UserIDs: []string{"u1"}, AssetIDs: []string{"a1"}}); err == nil {
		t.Error("expected an error when no name is given")
	}
}

func TestPermissionParsesActionsAsObjects(t *testing.T) {
	// The server returns actions as labelled objects, not bare strings;
	// a []string field fails to unmarshal and breaks every read.
	raw := []byte(`{
		"id": "p1",
		"name": "grant-a",
		"actions": [{"label": "连接 (所有协议)", "value": "connect"}],
		"protocols": ["all"],
		"is_active": true
	}`)
	var p Permission
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Actions) != 1 {
		t.Fatalf("actions = %v, want one entry", p.Actions)
	}
	if p.Actions[0].Value != "connect" {
		t.Errorf("action value = %q, want connect", p.Actions[0].Value)
	}
}

func TestActionMarshalsAsBareString(t *testing.T) {
	// A write request wants the bare value, not the labelled object.
	b, err := json.Marshal(Action{Label: "连接", Value: "connect"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"connect"` {
		t.Errorf("marshaled = %s, want \"connect\"", b)
	}
}

func TestPermissionParsesNullRelations(t *testing.T) {
	// The list endpoint sends users and assets as null; a struct that
	// cannot take null would fail the whole listing.
	raw := []byte(`{"id":"p1","name":"x","users":null,"assets":null,"actions":[]}`)
	var p Permission
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal with null relations: %v", err)
	}
	if p.ID != "p1" {
		t.Errorf("id = %q", p.ID)
	}
}

func TestFindByAssetReadsDetailsBecauseTheListOmitsRelations(t *testing.T) {
	// The list reports only assets_amount; the relations come back null
	// and are expanded only by the detail endpoint.
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/perms/asset-permissions/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 2,
				"results": []map[string]any{
					{"id": "p1", "name": "grant-a", "assets_amount": 1, "assets": nil},
					{"id": "p2", "name": "grant-b", "assets_amount": 1, "assets": nil},
				},
			})
		case "/api/v1/perms/asset-permissions/p1/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "p1", "name": "grant-a",
				"assets": []map[string]any{{"id": "a1", "name": "asset-a"}},
			})
		case "/api/v1/perms/asset-permissions/p2/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "p2", "name": "grant-b",
				"assets": []map[string]any{{"id": "a2", "name": "asset-b"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	found, err := FindByAsset(context.Background(), client, "a2")
	if err != nil {
		t.Fatalf("FindByAsset: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found = %d permissions, want 1", len(found))
	}
	if found[0].Name != "grant-b" {
		t.Errorf("name = %q, want grant-b", found[0].Name)
	}
}

func TestRevokeDeletesThePermission(t *testing.T) {
	var gotMethod, gotPath string
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := Revoke(context.Background(), client, "p1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/perms/asset-permissions/p1/" {
		t.Errorf("path = %s, want /api/v1/perms/asset-permissions/p1/", gotPath)
	}
}

func TestCurrentUserReadsTheProfile(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/profile/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "u1", "username": "mifazhan", "name": "米法展",
		})
	}))

	id, name, err := CurrentUser(context.Background(), client)
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if id != "u1" {
		t.Errorf("id = %q", id)
	}
	if name != "米法展" {
		t.Errorf("name = %q, want the display name", name)
	}
}

func TestCurrentUserFallsBackToUsername(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "username": "mifazhan"})
	}))

	_, name, err := CurrentUser(context.Background(), client)
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if name != "mifazhan" {
		t.Errorf("name = %q, want the username when no display name is set", name)
	}
}
