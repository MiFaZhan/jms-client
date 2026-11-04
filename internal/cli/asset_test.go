package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/config"
)

// managedServer is a fake JumpServer that answers the asset-management
// endpoints the CLI uses, recording what was asked for.
type managedServer struct {
	items    []map[string]any
	requests []string
	bodies   []map[string]any
	created  map[string]any
}

func (m *managedServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requests = append(m.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/assets/hosts/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": len(m.items), "results": m.items,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/assets/nodes/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 1,
				"results": []map[string]any{
					{"id": "node-1", "key": "1", "value": "DEFAULT"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/assets/hosts/":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.bodies = append(m.bodies, body)
			created := map[string]any{
				"id":      "new-id",
				"name":    body["name"],
				"address": body["address"],
				"platform": map[string]any{
					"id": 1, "name": "Linux",
				},
			}
			m.items = append(m.items, created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "a1", "name": "asset-a", "address": "10.0.0.1",
				"platform": map[string]any{"id": 1, "name": "Linux"},
				"comment":  "a note",
			})
		case r.Method == http.MethodPut:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.bodies = append(m.bodies, body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "a1", "name": body["name"], "address": "10.0.0.1",
				"platform": map[string]any{"id": 1, "name": "Linux"},
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Not found"})
		}
	})
}

// seedManagedFixture configures one server and points the fake session at
// the managed asset server.
func seedManagedFixture(t *testing.T, env *cliEnv, m *managedServer) {
	t.Helper()
	env.seedServer("bastion", "http://internal.example:2280/", "", "testuser")
	if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	server := httptest.NewServer(m.handler(t))
	t.Cleanup(server.Close)
	env.clientURL = server.URL
}

func TestAssetListPrintsEveryAssetNotOnlyReachableOnes(t *testing.T) {
	env := newCLIEnv(t)
	m := &managedServer{items: []map[string]any{
		assetRow("id-1", "主机-云则预演01", "192.168.100.252", "Linux"),
		assetRow("id-2", "主机-云则预演02", "192.168.100.191", "Linux"),
	}}
	seedManagedFixture(t, env, m)

	if code := env.run("asset", "list"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	stdout := env.stdout()
	for _, want := range []string{"Name", "Address", "Platform", "主机-云则预演01", "192.168.100.252", "Total: 2 asset(s)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(env.stderr(), "internal") {
		t.Errorf("stderr = %q, want the chosen endpoint reported", env.stderr())
	}
}

func TestAssetCreateSendsTheAccountAndTheAddress(t *testing.T) {
	env := newCLIEnv(t)
	m := &managedServer{}
	seedManagedFixture(t, env, m)

	code := env.run("asset", "create", "主机-云则开发", "192.168.101.205",
		"--account", "yunze", "--username", "yunze", "--password", "secret",
		"--comment", "开发环境")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if len(m.bodies) != 1 {
		t.Fatalf("bodies = %d, want 1 create request", len(m.bodies))
	}
	body := m.bodies[0]
	if body["name"] != "主机-云则开发" {
		t.Errorf("name = %v", body["name"])
	}
	if body["address"] != "192.168.101.205" {
		t.Errorf("address = %v", body["address"])
	}
	accounts, _ := body["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v, want one entry", body["accounts"])
	}
	acc, _ := accounts[0].(map[string]any)
	if acc["username"] != "yunze" {
		t.Errorf("username = %v", acc["username"])
	}
	if !strings.Contains(env.stdout(), "Created") {
		t.Errorf("stdout = %q, want a creation confirmation", env.stdout())
	}
}

func TestAssetShowPrintsDetail(t *testing.T) {
	env := newCLIEnv(t)
	m := &managedServer{items: []map[string]any{
		assetRow("a1", "asset-a", "10.0.0.1", "Linux"),
	}}
	seedManagedFixture(t, env, m)

	if code := env.run("asset", "show", "asset-a"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	stdout := env.stdout()
	for _, want := range []string{"ID:", "asset-a", "10.0.0.1", "Linux"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestAssetUpdateSendsOnlyTheChangedField(t *testing.T) {
	env := newCLIEnv(t)
	m := &managedServer{items: []map[string]any{
		assetRow("a1", "asset-a", "10.0.0.1", "Linux"),
	}}
	seedManagedFixture(t, env, m)

	if code := env.run("asset", "update", "asset-a", "--name", "renamed"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	if len(m.bodies) != 1 {
		t.Fatalf("bodies = %d, want 1 update request", len(m.bodies))
	}
	body := m.bodies[0]
	if body["name"] != "renamed" {
		t.Errorf("name = %v, want renamed", body["name"])
	}
	// The server replaces the whole object, so the required fields travel
	// with it and the untouched ones keep their value.
	for _, key := range []string{"name", "address", "platform", "protocols"} {
		if _, ok := body[key]; !ok {
			t.Errorf("body missing the required field %q: %v", key, body)
		}
	}
	if body["address"] != "10.0.0.1" {
		t.Errorf("address = %v, want the current address", body["address"])
	}
}

func TestAssetDeleteUsesTheDeleteMethod(t *testing.T) {
	env := newCLIEnv(t)
	m := &managedServer{items: []map[string]any{
		assetRow("a1", "asset-a", "10.0.0.1", "Linux"),
	}}
	seedManagedFixture(t, env, m)

	if code := env.run("asset", "delete", "asset-a", "--yes"); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, env.stderr())
	}
	sawDelete := false
	for _, req := range m.requests {
		if req == "DELETE /api/v1/assets/hosts/a1/" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Errorf("requests = %v, want a DELETE on the asset resource", m.requests)
	}
	if !strings.Contains(env.stdout(), "Deleted") {
		t.Errorf("stdout = %q, want a deletion confirmation", env.stdout())
	}
}

func TestAssetDeleteWithoutConfirmationDeletesNothing(t *testing.T) {
	env := newCLIEnv(t)
	m := &managedServer{items: []map[string]any{
		assetRow("a1", "asset-a", "10.0.0.1", "Linux"),
	}}
	seedManagedFixture(t, env, m)
	// The injected confirmation answers no.
	env.confirmAnswer = false
	env.confirmSet = true

	if code := env.run("asset", "delete", "asset-a"); code == 0 {
		t.Fatalf("exit code = 0, want a non-zero exit when the deletion is not confirmed")
	}
	for _, req := range m.requests {
		if strings.HasPrefix(req, "DELETE") {
			t.Errorf("requests = %v, want no DELETE when the confirmation was declined", m.requests)
		}
	}
}
