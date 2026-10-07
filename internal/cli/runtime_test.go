package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/connpool"
	"github.com/MiFaZhan/jms-client/internal/transport"
	"github.com/MiFaZhan/jms-client/internal/xfer"
)

// runtimeFakes records every call the pool-backed command seams receive.
//
// The bridge seam is a plain function field so a test can replace the
// recording default and write through the xfer.Bridge it is handed.
type runtimeFakes struct {
	execCalls []fakeExecCall
	execRes   transport.Result
	execErr   error

	transferCalls []TransferRequest
	transferRes   xfer.Result
	transferErr   error

	bridgeFn   func(ctx context.Context, args []string, b xfer.Bridge) (int, error)
	bridgeArgs [][]string
	bridgeRes  int
	bridgeErr  error
}

type fakeExecCall struct {
	server *config.ServerConfig
	sess   *auth.Session
	asset  string
	cmd    string
	opts   connpool.ExecOptions
}

// depsRuntime clones env.deps and installs the fakes into Runtime's seams.
// A command that reaches for the real pool or the real transport instead
// of these seams fails loudly: no seam here dials anywhere.
func (f *runtimeFakes) depsRuntime(env *cliEnv) Deps {
	d := env.deps().WithDefaults()
	d.Runtime = &Runtime{
		Sessions:  d.Runtime.Sessions,
		Terminals: d.Runtime.Terminals,
		AuditDir:  d.Runtime.AuditDir,
		Bus:       d.Runtime.Bus,
		Exec: func(_ context.Context, srv *config.ServerConfig, sess *auth.Session,
			asset, cmd string, opts connpool.ExecOptions) (transport.Result, error) {
			f.execCalls = append(f.execCalls, fakeExecCall{
				server: srv, sess: sess, asset: asset, cmd: cmd, opts: opts,
			})
			return f.execRes, f.execErr
		},
		Transfer: func(_ context.Context, req TransferRequest) (xfer.Result, error) {
			f.transferCalls = append(f.transferCalls, req)
			return f.transferRes, f.transferErr
		},
		Bridge: f.bridge,
	}
	return d
}

// bridge records the invocation by default, or runs the test-supplied
// bridgeFn when one is installed.
func (f *runtimeFakes) bridge(ctx context.Context, args []string, b xfer.Bridge) (int, error) {
	f.bridgeArgs = append(f.bridgeArgs, append([]string(nil), args...))
	if f.bridgeFn != nil {
		return f.bridgeFn(ctx, args, b)
	}
	return f.bridgeRes, f.bridgeErr
}

// runWithRuntime drives ExecuteArgs with the fakes installed and the
// capture buffers reset, mirroring cliEnv.run.
func (f *runtimeFakes) runWithRuntime(env *cliEnv, args ...string) int {
	env.t.Helper()
	env.out.Reset()
	env.errOut.Reset()
	return ExecuteArgs(args, f.depsRuntime(env))
}

// newRuntimeEnv seeds one server with a stored password, the minimal
// setup every pool-backed command needs before it can reach its seam.
func newRuntimeEnv(t *testing.T) (*cliEnv, *runtimeFakes) {
	t.Helper()
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://int.example:2280/", "http://ext.example:2280/", "ops")
	if err := env.creds.Set("bastion", config.CredPassword, "stored"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	return env, &runtimeFakes{}
}

// requireExecCalls asserts exactly one exec reached the seam.
func (f *runtimeFakes) requireExecCalls(t *testing.T) fakeExecCall {
	t.Helper()
	if len(f.execCalls) != 1 {
		t.Fatalf("Exec called %d times, want 1", len(f.execCalls))
	}
	return f.execCalls[0]
}

// TestDefaultTransferPassesAssetNameToEngine pins the SFTP blocker fix at
// the CLI seam: req.Asset (an asset NAME) must reach the engine as
// Engine.AssetName so Run resolves it on demand. The earlier version built
// the engine with only the session, so every `jms sftp` failed with
// "xfer: no asset resolved".
func TestDefaultTransferPassesAssetNameToEngine(t *testing.T) {
	const assetName = "transfer-target"
	apiSrv := newAssetAPIServerForTransfer(t, assetName)
	req := TransferRequest{
		Session: &auth.Session{
			Server:  &config.ServerConfig{Name: "srv", Username: "tester"},
			Client:  api.New(apiSrv.URL),
			BaseURL: apiSrv.URL,
		},
		Asset:    assetName,
		Account:  "deploy",
		Task:     xfer.Task{Direction: xfer.Download, Local: t.TempDir() + "/out.bin", Remote: "/tmp/remote.bin"},
		Endpoint: "external",
	}

	// Run resolves the asset before any SFTP dial; the transfer itself
	// cannot complete against an HTTP fake, but the resolve call is the
	// assertion target: it proves the name reached the engine.
	_, _ = defaultTransfer(context.Background(), req)

	if got := apiSrv.searches(); len(got) == 0 || got[0] != assetName {
		t.Fatalf("asset resolve searches = %v, want [%q, ...]", got, assetName)
	}
}

// transferAPIServer is a minimal asset-listing fake for transfer-seam
// tests: it records the search parameter of every request.
type transferAPIServer struct {
	*httptest.Server
	mu     sync.Mutex
	search []string
}

func newAssetAPIServerForTransfer(t *testing.T, name string) *transferAPIServer {
	t.Helper()
	s := &transferAPIServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.search = append(s.search, r.Form.Get("search"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 1,
			"results": []map[string]any{{
				"id": "asset-1", "name": name, "address": "10.0.0.1",
				"platform":         map[string]any{"name": "Linux"},
				"permed_accounts":  []map[string]any{},
				"permed_protocols": []map[string]any{},
			}},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *transferAPIServer) searches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.search...)
}
