package xfer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// tokenRequest records one connection-token POST as the server saw it.
type tokenRequest struct {
	Path string
	Body map[string]string
}

// tokenServer is a hermetic stand-in for JumpServer's REST API. It answers
// the connection-token endpoint and records the body, so a test can assert
// the protocol/connect_method pair on the wire.
type tokenServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []tokenRequest
	// id and value are handed out as the token. value doubles as the
	// password the in-process SSH server accepts, so the token actually
	// authenticates the subsequent SFTP session.
	id    string
	value string
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	ts := &tokenServer{id: "tok-1", value: testSecret(t)}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		ts.mu.Lock()
		ts.requests = append(ts.requests, tokenRequest{Path: r.URL.Path, Body: body})
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": ts.id, "value": ts.value})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tokenServer) recorded() []tokenRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]tokenRequest, len(ts.requests))
	copy(out, ts.requests)
	return out
}

// testSecret returns a random hex string. Tests need a non-empty password,
// and a random one keeps the fixture from looking like a real credential.
func testSecret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// sftpServer is an in-process SSH server serving the SFTP subsystem.
//
// It listens on 127.0.0.1:0 and accepts exactly one password, so the
// engine's real dial path (connection token -> SSH handshake -> SFTP
// subsystem) is exercised without a network or a JumpServer.
type sftpServer struct {
	ln       net.Listener
	password string
	// root is the SFTP start directory; the pkg/sftp server serves the
	// host filesystem rooted at "/", so tests use absolute temp paths.
	done chan struct{}
}

func newSFTPServer(t *testing.T, password string) *sftpServer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if string(pass) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &sftpServer{ln: ln, password: password, done: make(chan struct{})}

	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				serveSSHConn(conn, cfg)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
		close(srv.done)
	})
	return srv
}

// port returns the listener's TCP port.
func (s *sftpServer) port(t *testing.T) int {
	t.Helper()
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %T is not TCP", s.ln.Addr())
	}
	return addr.Port
}

// serveSSHConn runs the SSH handshake and serves one SFTP subsystem per
// session channel.
func serveSSHConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)

	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chreqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go serveSessionChannel(ch, chreqs)
	}
}

// serveSessionChannel answers the "subsystem" request with an SFTP server.
func serveSessionChannel(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type != "subsystem" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Name string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil || payload.Name != "sftp" {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)
		srv, err := sftp.NewServer(ch)
		if err != nil {
			_ = ch.Close()
			return
		}
		_ = srv.Serve()
		_ = srv.Close()
		return
	}
	_ = ch.Close()
}

// testEnv bundles everything one engine test needs: a token API, an
// in-process SFTP server and a session bound to both.
type testEnv struct {
	tokens *tokenServer
	ssh    *sftpServer
	engine *Engine
	sess   *auth.Session
}

// newTestEnv wires a token API and an SFTP server together and returns an
// engine that resolves its asset from the token server.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tokens := newTokenServer(t)
	sshSrv := newSFTPServer(t, tokens.value)

	srv := &config.ServerConfig{
		Name:     "test",
		Internal: tokens.URL,
		Username: "tester",
		SSHPort:  sshSrv.port(t),
	}
	sess := &auth.Session{
		Server:  srv,
		Client:  api.New(tokens.URL),
		BaseURL: tokens.URL,
	}
	engine := &Engine{
		Session:   sess,
		Asset:     assets.Info{ID: "asset-1", Name: "web-01", Account: "@USER", Protocol: "ssh"},
		AssetName: "web-01",
	}
	return &testEnv{tokens: tokens, ssh: sshSrv, engine: engine, sess: sess}
}

// secondEnv returns a second, fully independent environment, used by the
// relay test where both sides must be distinct assets.
func secondEnv(t *testing.T) *testEnv {
	t.Helper()
	tokens := newTokenServer(t)
	sshSrv := newSFTPServer(t, tokens.value)
	srv := &config.ServerConfig{
		Name:     "test-2",
		Internal: tokens.URL,
		Username: "tester",
		SSHPort:  sshSrv.port(t),
	}
	sess := &auth.Session{
		Server:  srv,
		Client:  api.New(tokens.URL),
		BaseURL: tokens.URL,
	}
	return &testEnv{
		tokens: tokens,
		ssh:    sshSrv,
		sess:   sess,
		engine: &Engine{
			Session:   sess,
			Asset:     assets.Info{ID: "asset-2", Name: "db-01", Account: "@USER", Protocol: "ssh"},
			AssetName: "db-01",
		},
	}
}

// fakeTerminal is an injected transport.Terminal. It records the command it
// was asked to run and replays a canned stdout.
type fakeTerminal struct {
	mu       sync.Mutex
	commands []string
	output   string
	err      error
	closed   int
}

func (f *fakeTerminal) Execute(_ context.Context, cmd string, _ time.Duration) (transport.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
	if f.err != nil {
		return transport.Result{}, f.err
	}
	return transport.Result{Output: f.output}, nil
}

func (f *fakeTerminal) Backend() transport.BackendType    { return transport.BackendSSH }
func (f *fakeTerminal) Interactive(context.Context) error { return nil }
func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeTerminal) lastCommand() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) == 0 {
		return ""
	}
	return f.commands[len(f.commands)-1]
}

// withTerminal injects a fake SSH-exec terminal into the engine.
func (e *Engine) withTerminal(term transport.Terminal) {
	e.terminal = func(context.Context, *Engine) (transport.Terminal, error) { return term, nil }
}

// withOpen injects an SFTP dialer into the engine.
func (e *Engine) withOpen(fn func(ctx context.Context, e *Engine, asset assets.Info) (remoteFS, error)) {
	e.open = fn
}
