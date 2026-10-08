package cli

import (
	"bufio"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/config"
)

// recordingProxy is a minimal HTTP CONNECT proxy that records every target
// it is asked to tunnel to.
type recordingProxy struct {
	addr string

	mu      sync.Mutex
	targets []string
}

func startRecordingProxy(t *testing.T) *recordingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	p := &recordingProxy{addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					p.mu.Lock()
					p.targets = append(p.targets, fields[1])
					p.mu.Unlock()
				}
				for {
					h, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if h == "\r\n" || h == "\n" {
						break
					}
				}
				_, _ = c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
			}(conn)
		}
	}()
	return p
}

func (p *recordingProxy) saw() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// writeGlobalProxy adds a global proxy to the configuration file.
func (e *cliEnv) writeGlobalProxy(value string) {
	e.t.Helper()
	cfg, err := loadOrNewConfig(nil, Deps{ConfigPath: e.cfgPath})
	if err != nil {
		e.t.Fatalf("load config: %v", err)
	}
	cfg.Proxy = value
	if _, err := saveConfigFor(nil, Deps{ConfigPath: e.cfgPath}, cfg); err != nil {
		e.t.Fatalf("save config: %v", err)
	}
}

// TestProbeFollowsConfiguredProxy is the CLI-level regression test for the
// probe/login split.
//
// The probe seam is deliberately left nil so the command resolves its own
// default, which must be the proxy-aware one. The bastion host does not
// exist, so the only way `jms ls` can get past endpoint selection is by
// sending the probe through the configured proxy.
func TestProbeFollowsConfiguredProxy(t *testing.T) {
	proxy := startRecordingProxy(t)

	env := newCLIEnv(t)
	env.seedServer("bastion", "http://bastion.invalid:2280/", "", "testuser")
	env.writeGlobalProxy("http://" + proxy.addr)
	if err := env.creds.Set("bastion", config.CredPassword, "pw"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	// The login seam answers with a session pointed at a local asset server,
	// so the command can finish once the proxy probe succeeds.
	env.clientURL = newAssetServer(t, nil, nil).URL

	// Leave Deps.Probe nil: the command must pick ProbeWith itself.
	deps := env.deps()
	deps.Probe = nil
	env.ctxOverride = nil

	code := executeArgsContext(t.Context(), []string{"ls", "bastion"}, deps)
	if code != 0 {
		t.Fatalf("jms ls exited %d; stderr:\n%s", code, env.stderr())
	}

	targets := proxy.saw()
	if len(targets) == 0 {
		t.Fatal("the configured proxy was never contacted; the probe dialled directly")
	}
	if targets[0] != "bastion.invalid:2280" {
		t.Fatalf("proxy was asked for %q, want bastion.invalid:2280", targets[0])
	}
}

// TestProbeStaysDirectWithoutProxy asserts the default is unchanged: with
// no proxy configured, the probe dials the address itself.
func TestProbeStaysDirectWithoutProxy(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://127.0.0.1:1/", "", "testuser")
	if err := env.creds.Set("bastion", config.CredPassword, "pw"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	// The probe fails on a closed port, and the 「端点故障转移」 fallback then spends a
	// full login on the same candidate, so the login seam has to fail too.
	env.loginErr = errors.New("dial tcp 127.0.0.1:1: connect: connection refused")

	deps := env.deps()
	deps.Probe = nil

	// A closed port means the probe fails, every candidate is exhausted and
	// the command reports the address as unreachable — the point is that it
	// dialled directly rather than through anything.
	code := executeArgsContext(t.Context(), []string{"ls", "bastion"}, deps)
	if code == 0 {
		t.Fatal("expected the unreachable address to fail the command")
	}
	stderr := env.stderr()
	if !strings.Contains(stderr, "no reachable endpoint") {
		t.Fatalf("stderr = %q, want it to report no reachable endpoint", stderr)
	}
	if !strings.Contains(stderr, "127.0.0.1:1") {
		t.Fatalf("stderr = %q, want it to name the dialled address", stderr)
	}
}

// TestConfigTestProbesThroughProxy asserts `jms config test` — the command
// whose whole purpose is reporting which address works — also probes
// through the configured proxy.
func TestConfigTestProbesThroughProxy(t *testing.T) {
	proxy := startRecordingProxy(t)

	env := newCLIEnv(t)
	env.seedServer("bastion", "http://bastion.invalid:2280/", "", "testuser")
	env.writeGlobalProxy("http://" + proxy.addr)
	if err := env.creds.Set("bastion", config.CredPassword, "pw"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	env.clientURL = "http://127.0.0.1:1"

	deps := env.deps()
	deps.Probe = nil

	_ = executeArgsContext(t.Context(), []string{"config", "test", "bastion"}, deps)

	targets := proxy.saw()
	if len(targets) == 0 {
		t.Fatal("`jms config test` never contacted the configured proxy")
	}
	if targets[0] != "bastion.invalid:2280" {
		t.Fatalf("proxy was asked for %q, want bastion.invalid:2280", targets[0])
	}
}

// TestInvalidProxyInConfigIsReported asserts a malformed proxy fails the
// command with a clear message instead of being ignored.
func TestInvalidProxyInConfigIsReported(t *testing.T) {
	env := newCLIEnv(t)
	env.seedServer("bastion", "http://127.0.0.1:1/", "", "testuser")

	// Write the bad value straight to disk: config.Save validates, so a
	// corrupt file has to be produced the way a user would.
	raw := "version = 2\ndefault_server = \"bastion\"\nproxy = \"ftp://nope:21\"\n\n" +
		"[servers.bastion]\ninternal = \"http://127.0.0.1:1/\"\nusername = \"testuser\"\n"
	if err := writeFile(env.cfgPath, raw); err != nil {
		t.Fatalf("write config: %v", err)
	}

	deps := env.deps()
	deps.Probe = nil
	code := executeArgsContext(t.Context(), []string{"ls", "bastion"}, deps)
	if code == 0 {
		t.Fatal("an invalid proxy should fail the command")
	}
	if !strings.Contains(env.stderr(), "proxy") {
		t.Fatalf("stderr = %q, want it to name the proxy", env.stderr())
	}
}

// TestSelectEndpointUsesProxyForProbeAndLogin asserts the two halves of
// endpoint selection agree on one policy, which is the property 「端点故障转移」
// depends on.
func TestSelectEndpointUsesProxyForProbeAndLogin(t *testing.T) {
	proxy := startRecordingProxy(t)

	env := newCLIEnv(t)
	env.seedServer("bastion", "http://bastion.invalid:2280/", "", "testuser")
	env.writeGlobalProxy("http://" + proxy.addr)

	cfg, err := loadOrNewConfig(nil, Deps{ConfigPath: env.cfgPath})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	srv, err := cfg.Get("bastion")
	if err != nil {
		t.Fatalf("get server: %v", err)
	}

	deps := env.deps()
	deps.Probe = nil // force the proxy-aware default

	_, sel, err := selectEndpoint(t.Context(), deps, newPrompter(deps), cfg, srv, "", "pw", "")
	if err != nil {
		t.Fatalf("selectEndpoint: %v", err)
	}
	if sel.Candidate.URL != "http://bastion.invalid:2280/" {
		t.Fatalf("selected %q, want the internal address", sel.Candidate.URL)
	}
	if len(proxy.saw()) == 0 {
		t.Fatal("selectEndpoint probed directly instead of through the proxy")
	}
	if len(env.loggedIn) != 1 {
		t.Fatalf("login attempts = %d, want 1", len(env.loggedIn))
	}
}

// writeFile is a tiny helper so the test above can plant a corrupt config.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
