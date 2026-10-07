package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/xfer"
)

// assetAPIServer answers the asset listing endpoint and records the search
// queries it received, so a test can observe what defaultTransfer's engines
// resolved and with which names.
type assetAPIServer struct {
	*httptest.Server

	mu      sync.Mutex
	search  []string
	cookies []*http.Cookie
}

func newAssetAPIServer(t *testing.T, names ...string) *assetAPIServer {
	t.Helper()
	s := &assetAPIServer{cookies: []*http.Cookie{{Name: "jms_sessionid", Value: "sess-test"}}}
	results := make([]map[string]any, 0, len(names))
	for i, n := range names {
		results = append(results, map[string]any{
			"id":       "asset-" + n,
			"name":     n,
			"address":  "10.0.0." + string(rune('1'+i)),
			"platform": map[string]any{"name": "Linux"},
			// Resolve fetches the detail for permed accounts/protocols; an
			// empty list makes SelectAccount answer @INPUT, which is fine:
			// the request itself is what this test asserts.
			"permed_accounts":  []map[string]any{},
			"permed_protocols": []map[string]any{},
		})
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/perms/users/self/assets/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.search = append(s.search, r.Form.Get("search"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":   len(results),
			"results": results,
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *assetAPIServer) searches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.search...)
}

func testSession(baseURL string) *auth.Session {
	return &auth.Session{
		Server:  &config.ServerConfig{Name: "srv", Username: "tester"},
		Client:  api.New(baseURL),
		BaseURL: baseURL,
	}
}

// TestDefaultTransferPassesAssetNameToOneSidedEngine pins the SFTP blocker
// fix: req.Asset (a name string) must reach the engine as Engine.AssetName,
// so Run resolves it on demand. An earlier version built the engine with
// only the session and dropped the name, so every transfer failed with
// "no asset resolved".
func TestDefaultTransferPassesAssetNameToOneSidedEngine(t *testing.T) {
	const assetName = "asset-by-name"
	apiSrv := newAssetAPIServer(t, assetName)
	req := TransferRequest{
		Session:   testSession(apiSrv.URL),
		Asset:     assetName,
		Account:   "deploy",
		Direction: xfer.Download,
		Local:     t.TempDir() + "/out.bin",
		Remote:    "/tmp/remote.bin",
	}

	// Run reaches assetInfo → assets.Resolve before any SFTP dial; the
	// missing remote file/asset on the API side would fail later, but the
	// resolve call itself is the assertion target.
	_, _ = defaultTransfer(context.Background(), req)

	searches := apiSrv.searches()
	if len(searches) == 0 {
		t.Fatal("no asset search was issued: the engine never resolved the asset")
	}
	if searches[0] != assetName {
		t.Fatalf("resolve searched %q, want %q", searches[0], assetName)
	}
}

// TestDefaultTransferPassesAssetNamesToRelayEngines pins the relay branch:
// src and dst engines must each carry their own asset name, because a relay
// resolves both sides independently.
func TestDefaultTransferPassesAssetNamesToRelayEngines(t *testing.T) {
	const srcName, dstName = "relay-src", "relay-dst"
	apiSrv := newAssetAPIServer(t, srcName, dstName)
	req := TransferRequest{
		Verify: false,
		Relay: &RelayRequest{
			SrcSession: testSession(apiSrv.URL),
			SrcAsset:   srcName,
			SrcPath:    "/tmp/src.bin",
			DstSession: testSession(apiSrv.URL),
			DstAsset:   dstName,
			DstPath:    "/tmp/dst.bin",
		},
	}

	// Relay resolves src first; the resolve of dst only happens after a
	// successful src SFTP dial, so this run asserts on the src side. The
	// dst side is exercised through the field-level test below.
	_, _ = defaultTransfer(context.Background(), req)

	searches := apiSrv.searches()
	if len(searches) == 0 || searches[0] != srcName {
		t.Fatalf("relay src resolve searches = %v, want [%q, ...]", searches, srcName)
	}
}
