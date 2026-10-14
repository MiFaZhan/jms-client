package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/connpool"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
	"github.com/MiFaZhan/jms-client/internal/transport"
	"github.com/MiFaZhan/jms-client/internal/xfer"
)

// stateStore is an in-memory endpoint.StateStore for the resolver tests.
type stateStore struct {
	states map[string]endpoint.State
	sets   int
}

func newStateStore(states map[string]endpoint.State) *stateStore {
	if states == nil {
		states = map[string]endpoint.State{}
	}
	return &stateStore{states: states}
}

func (s *stateStore) Get(server string) (endpoint.State, bool) {
	st, ok := s.states[server]
	return st, ok
}

func (s *stateStore) Set(server string, st endpoint.State) error {
	s.sets++
	s.states[server] = st
	return nil
}

// credStore answers a fixed credential, so a test never touches the real OS
// credential store.
type credStore struct{ password string }

func (c credStore) Get(alias, kind string) (string, error) {
	if kind == config.CredPassword {
		return c.password, nil
	}
	return "", config.ErrNotFound
}

func (c credStore) Set(alias, kind, value string) error { return nil }
func (c credStore) Delete(alias string) error           { return nil }

// mcpLoginServer is a scripted JumpServer answering the dual login, plus a
// configuration file that points a server entry at it. Together they let a
// test drive the real sessionFor path — config load, credential read, login —
// rather than stubbing it out.
type mcpLoginServer struct {
	URL string

	mu     sync.Mutex
	logins int
}

func newLoginServerForMCP(t *testing.T) *mcpLoginServer {
	t.Helper()
	ls := &mcpLoginServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case auth.PathAPILogin:
			ls.mu.Lock()
			ls.logins++
			ls.mu.Unlock()
			_, _ = w.Write([]byte(`{"token":"bearer-token"}`))
		case auth.PathFormLogin:
			if r.Method == http.MethodGet {
				http.SetCookie(w, &http.Cookie{Name: auth.CookieCSRF, Value: "csrf-1", Path: "/"})
			} else {
				http.SetCookie(w, &http.Cookie{Name: auth.CookieSession, Value: "sess-1", Path: "/"})
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ls.URL = srv.URL
	return ls
}

func (ls *mcpLoginServer) logins2() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.logins
}

// configPath writes a configuration file whose server entry points at the
// test server, and returns its path.
func (ls *mcpLoginServer) configPath(t *testing.T, srv *config.ServerConfig) string {
	t.Helper()
	cfg := &config.AppConfig{
		Version: 2,
		Default: srv.Name,
		Servers: map[string]*config.ServerConfig{
			srv.Name: {Name: srv.Name, Internal: ls.URL, Username: "tester"},
		},
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

// testSrv is a server entry with both addresses configured.
func testSrv() *config.ServerConfig {
	return &config.ServerConfig{
		Name:     "yunze",
		Internal: "http://192.168.37.60:2280/",
		External: "http://yznw.xiujie.cn:2280/",
		Username: "tester",
	}
}

// sessionAt builds a session bound to one of testSrv's addresses.
func sessionAt(url string) *auth.Session {
	return &auth.Session{Server: testSrv(), BaseURL: url, Client: api.New(url)}
}

// TestBackendForPrefersRememberedBackend pins the fix for the reported bug:
// the MCP host used to hardcode ws, so a state.json recording ssh for the
// selected endpoint was ignored and every call kept retrying a backend that
// no longer worked while the CLI succeeded.
func TestBackendForPrefersRememberedBackend(t *testing.T) {
	store := newStateStore(map[string]endpoint.State{
		"yunze": {LastGood: endpoint.KindInternal, At: time.Now(),
			Backend: map[endpoint.Kind]string{endpoint.KindInternal: "ssh"}},
	})
	s := New(Options{state: store})

	got := s.backendFor(sessionAt("http://192.168.37.60:2280"), testSrv(), nil)
	if got != transport.BackendSSH {
		t.Fatalf("backendFor() = %q, want %q (the remembered backend)", got, transport.BackendSSH)
	}
}

// TestBackendForUsesTheSessionsEndpoint asserts the memory is keyed by the
// address the session is actually bound to, not by the configured
// preference: a session on the external address must read the external
// entry.
func TestBackendForUsesTheSessionsEndpoint(t *testing.T) {
	store := newStateStore(map[string]endpoint.State{
		"yunze": {
			LastGood: endpoint.KindInternal, At: time.Now(),
			Backend: map[endpoint.Kind]string{
				endpoint.KindInternal: "ssh",
				endpoint.KindExternal: "ws",
			},
		},
	})
	s := New(Options{state: store})

	if got := s.backendFor(sessionAt("http://yznw.xiujie.cn:2280"), testSrv(), nil); got != transport.BackendWS {
		t.Fatalf("external session backend = %q, want ws", got)
	}
	if got := s.backendFor(sessionAt("http://192.168.37.60:2280"), testSrv(), nil); got != transport.BackendSSH {
		t.Fatalf("internal session backend = %q, want ssh", got)
	}
}

// TestBackendForDefaultsToWSWithoutMemory keeps the documented default: a
// server that has never recorded a backend still gets the WebSocket
// transport, which is the one that works wherever the web UI does.
func TestBackendForDefaultsToWSWithoutMemory(t *testing.T) {
	s := New(Options{state: newStateStore(nil)})
	if got := s.backendFor(sessionAt("http://192.168.37.60:2280"), testSrv(), nil); got != transport.BackendWS {
		t.Fatalf("backendFor() = %q, want the ws default", got)
	}

	// A session bound to an address the server does not configure cannot be
	// keyed, so the default applies rather than a guess.
	other := New(Options{state: newStateStore(map[string]endpoint.State{
		"yunze": {Backend: map[endpoint.Kind]string{endpoint.KindInternal: "ssh"}},
	})})
	if got := other.backendFor(sessionAt("http://unknown.example.com:2280"), testSrv(), nil); got != transport.BackendWS {
		t.Fatalf("unkeyable session backend = %q, want the ws default", got)
	}
}

// TestBackendForSkipsABackendThatAlreadyFailed covers the one-shot recovery:
// after ws failed, the retry must not be handed ws again just because
// state.json remembers it.
func TestBackendForSkipsABackendThatAlreadyFailed(t *testing.T) {
	store := newStateStore(map[string]endpoint.State{
		"yunze": {Backend: map[endpoint.Kind]string{endpoint.KindInternal: "ws"}},
	})
	s := New(Options{state: store})

	tried := map[transport.BackendType]bool{transport.BackendWS: true}
	if got := s.backendFor(sessionAt("http://192.168.37.60:2280"), testSrv(), tried); got == transport.BackendWS {
		t.Fatalf("backendFor() = %q, want a backend other than the one that just failed", got)
	}
}

// TestBackendForEnvOverrideWins pins the manual escape hatch. Without it the
// only way to steer the transport from the MCP side was to edit state.json
// and restart the host.
func TestBackendForEnvOverrideWins(t *testing.T) {
	store := newStateStore(map[string]endpoint.State{
		"yunze": {Backend: map[endpoint.Kind]string{endpoint.KindInternal: "ssh"}},
	})
	s := New(Options{state: store})

	t.Setenv(EnvBackend, "auto")
	if got := s.backendFor(sessionAt("http://192.168.37.60:2280"), testSrv(), nil); got != transport.BackendAuto {
		t.Fatalf("backendFor() with %s=auto = %q, want auto", EnvBackend, got)
	}

	// The override also beats the "already failed" exclusion: it is an
	// explicit instruction, not a memory to be second-guessed.
	tried := map[transport.BackendType]bool{transport.BackendAuto: true}
	if got := s.backendFor(sessionAt("http://192.168.37.60:2280"), testSrv(), tried); got != transport.BackendAuto {
		t.Fatalf("backendFor() with an explicit override = %q, want auto", got)
	}
}

// TestBackendOverrideRejectsGarbage keeps a typo from being silently
// swallowed: an unparseable value must be reported, not ignored.
func TestBackendOverrideRejectsGarbage(t *testing.T) {
	t.Setenv(EnvBackend, "websocket")
	if _, _, err := backendOverride(); err == nil {
		t.Fatalf("%s=websocket was accepted", EnvBackend)
	} else if !strings.Contains(err.Error(), EnvBackend) {
		t.Fatalf("error %v does not name the variable", err)
	}

	t.Setenv(EnvBackend, "")
	if _, ok, err := backendOverride(); ok || err != nil {
		t.Fatalf("an empty %s must mean no override, got ok=%v err=%v", EnvBackend, ok, err)
	}
}

// TestEndpointOverride pins the second escape hatch: an operator can force
// the address kind from the MCP side, which the tool arguments do not expose.
func TestEndpointOverride(t *testing.T) {
	t.Setenv(EnvEndpoint, "external")
	got, err := endpointOverride()
	if err != nil || got != endpoint.KindExternal {
		t.Fatalf("endpointOverride() = (%q, %v), want (external, nil)", got, err)
	}

	t.Setenv(EnvEndpoint, "INTERNAL")
	if got, err := endpointOverride(); err != nil || got != endpoint.KindInternal {
		t.Fatalf("endpointOverride() = (%q, %v), want (internal, nil) — the value is case-insensitive", got, err)
	}

	t.Setenv(EnvEndpoint, "")
	if got, err := endpointOverride(); err != nil || got != "" {
		t.Fatalf("an empty %s must mean no override, got (%q, %v)", EnvEndpoint, got, err)
	}

	t.Setenv(EnvEndpoint, "lan")
	if _, err := endpointOverride(); err == nil {
		t.Fatalf("%s=lan was accepted", EnvEndpoint)
	}
}

// TestExecTimeoutOverride pins the third escape hatch and, more importantly,
// the default it replaces: an agent that omits `timeout` must not be capped
// at the Python tool's 30s, which cut off builds and deploys a human would
// have waited out.
func TestExecTimeoutOverride(t *testing.T) {
	t.Setenv(EnvExecTimeout, "")
	if got, err := execTimeout(); err != nil || got != DefaultExecTimeout {
		t.Fatalf("execTimeout() = (%v, %v), want (%v, nil) with no override", got, err, DefaultExecTimeout)
	}
	if DefaultExecTimeout < time.Minute {
		t.Fatalf("DefaultExecTimeout = %v, want at least a minute", DefaultExecTimeout)
	}

	t.Setenv(EnvExecTimeout, "90s")
	if got, err := execTimeout(); err != nil || got != 90*time.Second {
		t.Fatalf("execTimeout() = (%v, %v), want (1m30s, nil)", got, err)
	}

	// A bare number is seconds, matching the CLI's -t flag.
	t.Setenv(EnvExecTimeout, "120")
	if got, err := execTimeout(); err != nil || got != 2*time.Minute {
		t.Fatalf("execTimeout() = (%v, %v), want (2m, nil)", got, err)
	}

	// A malformed or non-positive value is reported, never silently ignored:
	// falling back to the default would leave the operator believing a
	// ceiling they never got.
	for _, bad := range []string{"soon", "0", "-30s"} {
		t.Setenv(EnvExecTimeout, bad)
		if _, err := execTimeout(); err == nil {
			t.Errorf("%s=%q was accepted", EnvExecTimeout, bad)
		} else if !strings.Contains(err.Error(), EnvExecTimeout) {
			t.Errorf("error %v does not name the variable", err)
		}
	}
}

// TestRememberBackendRoundTrips records what worked so the next cold start —
// in this process or any CLI invocation — goes straight to it.
func TestRememberBackendRoundTrips(t *testing.T) {
	store := newStateStore(nil)
	s := New(Options{state: store})

	s.rememberBackend(testSrv(), endpoint.KindInternal, transport.BackendSSH)

	st, ok := store.Get("yunze")
	if !ok {
		t.Fatal("rememberBackend did not write any state")
	}
	if got := st.BackendFor(endpoint.KindInternal); got != "ssh" {
		t.Fatalf("remembered backend = %q, want ssh", got)
	}
	if store.sets != 1 {
		t.Fatalf("state writes = %d, want 1", store.sets)
	}

	// It must also survive a later selection: the resolver reads it back.
	if got := s.backendFor(sessionAt("http://192.168.37.60:2280"), testSrv(), nil); got != transport.BackendSSH {
		t.Fatalf("backendFor() after remembering = %q, want ssh", got)
	}
}

// TestRememberBackendKeepsLastGood guards the two memories from clobbering
// each other: the backend write must preserve the address memory.
func TestRememberBackendKeepsLastGood(t *testing.T) {
	store := newStateStore(map[string]endpoint.State{
		"yunze": {LastGood: endpoint.KindExternal, At: time.Now()},
	})
	s := New(Options{state: store})

	s.rememberBackend(testSrv(), endpoint.KindExternal, transport.BackendWS)

	st, _ := store.Get("yunze")
	if st.LastGood != endpoint.KindExternal {
		t.Fatalf("LastGood = %q, want external (the backend write clobbered it)", st.LastGood)
	}
	if got := st.BackendFor(endpoint.KindExternal); got != "ws" {
		t.Fatalf("backend = %q, want ws", got)
	}
}

// TestRecoverableClassifiesFailures pins what is worth one fresh login. The
// important negatives are the terminal ones: retrying a credential rejection
// would only prompt for MFA twice.
func TestRecoverableClassifiesFailures(t *testing.T) {
	ctx := context.Background()

	terminal := []error{
		&api.AuthError{StatusCode: http.StatusUnauthorized},
		&api.AuthError{StatusCode: http.StatusForbidden},
		&api.AuthError{StatusCode: http.StatusBadRequest},
		&api.AuthError{StatusCode: http.StatusUnprocessableEntity},
		auth.ErrMFANotProvided,
		auth.ErrMissingSessionCookie,
		context.Canceled,
		context.DeadlineExceeded,
		transport.ErrUnsupported,
		// A command that outran its timeout already reached the remote host;
		// replaying it would run it twice (DESIGN.md「TerminalPool」第 5 点).
		transport.ErrCommandTimeout,
		fmt.Errorf("command timed out after 5m0s: %w", transport.ErrCommandTimeout),
	}
	for _, err := range terminal {
		if recoverable(ctx, err) {
			t.Errorf("recoverable(%v) = true, want false", err)
		}
	}

	transient := []error{
		errors.New("websocket connect to 192.168.37.60:2280: handshake: returned HTTP 302"),
		&api.AuthError{StatusCode: http.StatusBadGateway},
		&api.AuthError{StatusCode: http.StatusInternalServerError},
		errors.New("dial tcp: i/o timeout"),
	}
	for _, err := range transient {
		if !recoverable(ctx, err) {
			t.Errorf("recoverable(%v) = false, want true", err)
		}
	}

	if recoverable(ctx, nil) {
		t.Error("recoverable(nil) = true, want false")
	}

	// A cancelled context means the client asked to stop, whatever the error.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if recoverable(cancelled, errors.New("boom")) {
		t.Error("recoverable() with a cancelled context = true, want false")
	}
}

// TestTransferNotStarted gates the transfer retry on the byte counter: a run
// that moved data is never replayed, and neither is a verification failure.
func TestTransferNotStarted(t *testing.T) {
	if !transferNotStarted(xfer.Result{}, errors.New("dial failed")) {
		t.Error("a transfer that moved no bytes must be retryable")
	}
	if transferNotStarted(xfer.Result{Bytes: 4096}, errors.New("write failed")) {
		t.Error("a transfer that moved bytes must not be replayed")
	}
	if transferNotStarted(xfer.Result{}, xfer.ErrVerifyFailed) {
		t.Error("a verification failure must not be replayed: the file was written")
	}
}

// TestSessionForFindsTheMatchingSide distinguishes a relay's two sides so a
// recovery replaces the session that was actually refreshed.
func TestSessionForFindsTheMatchingSide(t *testing.T) {
	srv := testSrv()
	sess := sessionAt("http://192.168.37.60:2280")

	oneSided := &TransferRequest{Session: sess}
	if got := oneSided.sessionFor(srv); got != sess {
		t.Fatal("sessionFor did not return the one-sided session")
	}

	srcSrv := &config.ServerConfig{Name: "src", Internal: "http://10.0.0.1:2280/"}
	dstSrv := &config.ServerConfig{Name: "dst", Internal: "http://10.0.0.2:2280/"}
	srcSess := &auth.Session{Server: srcSrv, BaseURL: "http://10.0.0.1:2280/"}
	dstSess := &auth.Session{Server: dstSrv, BaseURL: "http://10.0.0.2:2280/"}
	relay := &TransferRequest{Relay: &RelayRequest{SrcSession: srcSess, DstSession: dstSess}}

	if got := relay.sessionFor(srcSrv); got != srcSess {
		t.Fatal("sessionFor(src) did not return the source session")
	}
	if got := relay.sessionFor(dstSrv); got != dstSess {
		t.Fatal("sessionFor(dst) did not return the destination session")
	}
	if got := relay.sessionFor(srv); got != nil {
		t.Fatalf("sessionFor(unknown) = %v, want nil", got)
	}
	if got := relay.sessionFor(nil); got != nil {
		t.Fatalf("sessionFor(nil) = %v, want nil", got)
	}
}

// TestSessionEndpointReportsTheBoundKind keeps the audit record honest: the
// endpoint reported for a call is the one the session actually uses.
func TestSessionEndpointReportsTheBoundKind(t *testing.T) {
	if got := sessionEndpoint(testSrv(), sessionAt("http://192.168.37.60:2280")); got != endpoint.KindInternal {
		t.Fatalf("sessionEndpoint() = %q, want internal", got)
	}
	if got := sessionEndpoint(testSrv(), sessionAt("http://yznw.xiujie.cn:2280")); got != endpoint.KindExternal {
		t.Fatalf("sessionEndpoint() = %q, want external", got)
	}
	if got := sessionEndpoint(testSrv(), sessionAt("http://unknown.example.com:2280")); got != "" {
		t.Fatalf("sessionEndpoint() for an unknown address = %q, want empty", got)
	}
}

// TestRetryWithFreshSessionDropsTheCachedSession pins the recovery mechanism
// itself: the pooled session must be dropped so the next selection re-reads
// state.json and re-logs in, instead of handing back the session that just
// failed.
func TestRetryWithFreshSessionDropsTheCachedSession(t *testing.T) {
	ls := newLoginServerForMCP(t)
	pool := connpool.NewSessionPool()
	t.Cleanup(func() { _ = pool.Close() })
	srv := testSrv()

	first, err := pool.Get(context.Background(), srv, ls.URL, auth.Credentials{Password: "pw"}, nil)
	if err != nil {
		t.Fatalf("pool.Get() = %v, want nil", err)
	}
	if got := ls.logins2(); got != 1 {
		t.Fatalf("logins = %d, want 1", got)
	}

	configPath := ls.configPath(t, srv)
	s := New(Options{
		Sessions: pool,
		state:    newStateStore(nil),
		Creds:    credStore{password: "pw"},
	})

	sess, _, err := s.retryWithFreshSession(context.Background(), srv, srv.Name, configPath)
	if err != nil {
		t.Fatalf("retryWithFreshSession() = %v, want nil", err)
	}
	if sess == first {
		t.Fatal("retryWithFreshSession returned the session that had just failed")
	}
	if got := ls.logins2(); got != 2 {
		t.Fatalf("logins = %d, want 2 (the recovery must re-login)", got)
	}
}

// TestExecUsesTheRememberedBackendFirst is the reported bug in one test.
//
// state.json records ssh for the internal address, written by an earlier
// `jms exec --endpoint internal --backend ssh`. Before the fix the MCP host
// hardcoded ws and ignored that memory, so every call went out over WebSocket
// and the internal endpoint answered 302 — forever, while the CLI (a fresh
// process every time, so it re-read the file) kept working.
//
// With the memory honoured, the very first attempt uses ssh and succeeds; no
// failure, no retry, no restart.
func TestExecUsesTheRememberedBackendFirst(t *testing.T) {
	srv := testSrv()
	sess := sessionAt("http://192.168.37.60:2280")
	store := newStateStore(map[string]endpoint.State{
		srv.Name: {
			LastGood: endpoint.KindInternal, At: time.Now(),
			Backend: map[endpoint.Kind]string{endpoint.KindInternal: "ssh"},
		},
	})

	var mu sync.Mutex
	var backends []transport.BackendType
	s := New(Options{
		Session: func(context.Context, string) (*auth.Session, *config.ServerConfig, error) {
			return sess, srv, nil
		},
		Exec: func(_ context.Context, _ *config.ServerConfig, _ *auth.Session,
			_, _ string, opts connpool.ExecOptions) (transport.Result, error) {
			mu.Lock()
			backends = append(backends, opts.Backend)
			mu.Unlock()
			return transport.Result{Output: "EXEC_OK\n"}, nil
		},
		state: store,
	})

	text, isErr := callExecTool(t, s, "192.168.37.60", "echo EXEC_OK")
	if isErr {
		t.Fatalf("jms_exec reported an error: %s", text)
	}
	if !strings.Contains(text, "EXEC_OK") {
		t.Fatalf("jms_exec output = %q, want the command output", text)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(backends) != 1 {
		t.Fatalf("attempts = %v, want exactly one: the remembered backend must be used first", backends)
	}
	if backends[0] != transport.BackendSSH {
		t.Fatalf("attempt backend = %q, want ssh (what state.json remembers)", backends[0])
	}
}

// TestExecRecoversBySwitchingBackend covers the case the memory cannot: no
// backend has been recorded for this endpoint yet, the default WebSocket
// attempt hits the 302, and the call must recover on its own instead of
// failing until the AI client is restarted.
func TestExecRecoversBySwitchingBackend(t *testing.T) {
	srv := testSrv()
	sess := sessionAt("http://192.168.37.60:2280")

	var mu sync.Mutex
	var backends []transport.BackendType
	exec := func(_ context.Context, _ *config.ServerConfig, _ *auth.Session,
		_, _ string, opts connpool.ExecOptions) (transport.Result, error) {

		mu.Lock()
		backends = append(backends, opts.Backend)
		n := len(backends)
		mu.Unlock()

		if opts.Backend == transport.BackendWS {
			// The failure from the report, verbatim in shape.
			return transport.Result{}, errors.New(
				"websocket connect to 192.168.37.60:2280: handshake: " +
					"ws://192.168.37.60:2280/koko/ws/terminal/?token=... returned HTTP 302")
		}
		if n == 0 {
			t.Error("the recovery ran no attempt")
		}
		return transport.Result{Output: "EXEC_OK\n"}, nil
	}

	store := newStateStore(nil)
	s := New(Options{
		Session: func(context.Context, string) (*auth.Session, *config.ServerConfig, error) {
			return sess, srv, nil
		},
		Exec:  exec,
		state: store,
	})

	text, isErr := callExecTool(t, s, "192.168.37.60", "echo EXEC_OK")
	if isErr {
		t.Fatalf("jms_exec reported an error: %s", text)
	}
	if !strings.Contains(text, "EXEC_OK") {
		t.Fatalf("jms_exec output = %q, want the command output", text)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(backends) != 2 {
		t.Fatalf("attempts = %v, want 2: one failure plus one recovery", backends)
	}
	if backends[0] != transport.BackendWS {
		t.Fatalf("first attempt backend = %q, want the ws default", backends[0])
	}
	if backends[1] != transport.BackendSSH {
		t.Fatalf("retry backend = %q, want ssh (the other backend)", backends[1])
	}

	// The successful backend must be remembered, so the next call starts from
	// it instead of paying the failed attempt again.
	st, _ := store.Get(srv.Name)
	if got := st.BackendFor(endpoint.KindInternal); got != "ssh" {
		t.Fatalf("remembered backend = %q, want ssh", got)
	}
}

// TestExecDoesNotRetryWhenNothingCanChange guards the cost of the recovery: a
// failure that a fresh login cannot fix — a credential rejection — must be
// reported immediately, and a call that produced output must never be
// replayed.
func TestExecDoesNotRetryWhenNothingCanChange(t *testing.T) {
	srv := testSrv()
	sess := sessionAt("http://192.168.37.60:2280")

	t.Run("credential rejection is terminal", func(t *testing.T) {
		var attempts int
		s := New(Options{
			Session: func(context.Context, string) (*auth.Session, *config.ServerConfig, error) {
				return sess, srv, nil
			},
			Exec: func(context.Context, *config.ServerConfig, *auth.Session, string, string,
				connpool.ExecOptions) (transport.Result, error) {
				attempts++
				return transport.Result{}, &api.AuthError{StatusCode: http.StatusUnauthorized}
			},
			state: newStateStore(nil),
		})

		if _, isErr := callExecTool(t, s, "web-01", "uptime"); !isErr {
			t.Fatal("a credential rejection was reported as success")
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1: a rejection is not fixed by a fresh login", attempts)
		}
	})

	t.Run("output means the command already ran", func(t *testing.T) {
		var attempts int
		s := New(Options{
			Session: func(context.Context, string) (*auth.Session, *config.ServerConfig, error) {
				return sess, srv, nil
			},
			Exec: func(context.Context, *config.ServerConfig, *auth.Session, string, string,
				connpool.ExecOptions) (transport.Result, error) {
				attempts++
				return transport.Result{Output: "half a line"}, errors.New("connection reset")
			},
			state: newStateStore(nil),
		})

		if _, isErr := callExecTool(t, s, "web-01", "rm -rf /srv"); !isErr {
			t.Fatal("a failed command was reported as success")
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1: a command that produced output must not be replayed", attempts)
		}
	})
}

// callExecTool drives jms_exec through the SDK's in-memory transport and
// returns the text the client receives, so a test asserts on what an AI client
// actually sees rather than on an internal field.
func callExecTool(t *testing.T, s *Server, asset, command string) (string, bool) {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	go func() { _ = s.build().Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      ToolExec,
		Arguments: map[string]any{"asset": asset, "command": command},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("tool result carried no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text, res.IsError
}

// TestTransferWithRecoveryRefreshesBothRelaySides pins the relay case: a
// relay has two sides on possibly different servers, and the caller cannot tell
// from the error which one was stale, so both are refreshed — and each side
// keeps its own server's session.
func TestTransferWithRecoveryRefreshesBothRelaySides(t *testing.T) {
	srcSrv := &config.ServerConfig{Name: "src", Internal: "http://10.0.0.1:2280/", Username: "u"}
	dstSrv := &config.ServerConfig{Name: "dst", Internal: "http://10.0.0.2:2280/", Username: "u"}
	staleSrc := &auth.Session{Server: srcSrv, BaseURL: "http://10.0.0.1:2280/"}
	staleDst := &auth.Session{Server: dstSrv, BaseURL: "http://10.0.0.2:2280/"}
	freshSrc := &auth.Session{Server: srcSrv, BaseURL: "http://10.0.0.1:2280/"}
	freshDst := &auth.Session{Server: dstSrv, BaseURL: "http://10.0.0.2:2280/"}

	var mu sync.Mutex
	var seen [][2]*auth.Session
	s := New(Options{
		Transfer: func(_ context.Context, req TransferRequest) (xfer.Result, error) {
			mu.Lock()
			seen = append(seen, [2]*auth.Session{req.Relay.SrcSession, req.Relay.DstSession})
			n := len(seen)
			mu.Unlock()
			if n == 1 {
				return xfer.Result{}, errors.New("xfer: dial 10.0.0.1:2222: connection refused")
			}
			return xfer.Result{Bytes: 42}, nil
		},
	})

	// The recovery re-selects both servers; the returned sessions are the
	// fresh pair.
	refreshed := map[string]*auth.Session{"src": freshSrc, "dst": freshDst}
	s.opts.Session = func(_ context.Context, name string) (*auth.Session, *config.ServerConfig, error) {
		if name == "dst" {
			return refreshed["dst"], dstSrv, nil
		}
		return refreshed["src"], srcSrv, nil
	}

	req := TransferRequest{Relay: &RelayRequest{
		SrcSession: staleSrc, SrcAsset: "a", SrcPath: "/x",
		DstSession: staleDst, DstAsset: "b", DstPath: "/y",
	}}
	res, err := s.transferWithRecovery(context.Background(), "",
		req,
		transferSide{srv: srcSrv, current: staleSrc},
		transferSide{srv: dstSrv, current: staleDst},
	)
	if err != nil {
		t.Fatalf("transferWithRecovery() = %v, want nil", err)
	}
	if res.Bytes != 42 {
		t.Fatalf("bytes = %d, want 42", res.Bytes)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("attempts = %d, want 2", len(seen))
	}
	if seen[0][0] != staleSrc || seen[0][1] != staleDst {
		t.Fatal("the first attempt did not use the sessions the request carried")
	}
	if seen[1][0] != freshSrc {
		t.Fatal("the retry did not refresh the source session")
	}
	if seen[1][1] != freshDst {
		t.Fatal("the retry did not refresh the destination session")
	}
}

// TestTransferWithRecoverySkipsRetryWhenNothingChanged covers the safety
// bound: with an unchanged session (an injected seam, or a pool that handed
// back the same session) the transfer must not be replayed.
func TestTransferWithRecoverySkipsRetryWhenNothingChanged(t *testing.T) {
	srv := testSrv()
	sess := sessionAt("http://192.168.37.60:2280")

	var attempts int
	s := New(Options{
		Transfer: func(context.Context, TransferRequest) (xfer.Result, error) {
			attempts++
			return xfer.Result{}, errors.New("xfer: dial failed")
		},
		Session: func(context.Context, string) (*auth.Session, *config.ServerConfig, error) {
			return sess, srv, nil
		},
	})

	_, err := s.transferWithRecovery(context.Background(), "", TransferRequest{
		Session: sess, Asset: "web-01", Local: "/tmp/a", Remote: "/tmp/b",
		Direction: xfer.Upload,
	}, transferSide{srv: srv, current: sess})
	if err == nil {
		t.Fatal("transferWithRecovery() = nil, want the dial failure")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1: an unchanged session means nothing to retry", attempts)
	}
}

// TestTransferWithRecoveryNeverReplaysMovedBytes guards the other safety
// bound: a transfer that already wrote data reports its failure instead of
// running again.
func TestTransferWithRecoveryNeverReplaysMovedBytes(t *testing.T) {
	srv := testSrv()
	stale := sessionAt("http://192.168.37.60:2280")
	fresh := sessionAt("http://yznw.xiujie.cn:2280")

	var attempts int
	s := New(Options{
		Transfer: func(context.Context, TransferRequest) (xfer.Result, error) {
			attempts++
			return xfer.Result{Bytes: 8192}, errors.New("xfer: write failed mid-transfer")
		},
		Session: func(context.Context, string) (*auth.Session, *config.ServerConfig, error) {
			return fresh, srv, nil
		},
	})

	_, err := s.transferWithRecovery(context.Background(), "", TransferRequest{
		Session: stale, Asset: "web-01", Local: "/tmp/a", Remote: "/tmp/b",
		Direction: xfer.Upload,
	}, transferSide{srv: srv, current: stale})
	if err == nil {
		t.Fatal("transferWithRecovery() = nil, want the write failure")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1: a transfer that moved bytes must not be replayed", attempts)
	}
}
