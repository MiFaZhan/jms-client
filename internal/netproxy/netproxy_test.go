package netproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		direct  bool
		scheme  string
	}{
		{name: "empty is direct", raw: "", direct: true},
		{name: "whitespace is direct", raw: "   ", direct: true},
		{name: "direct word", raw: "direct", direct: true},
		{name: "none word", raw: "none", direct: true},
		{name: "off word", raw: "off", direct: true},
		{name: "uppercase DIRECT", raw: "DIRECT", direct: true},
		{name: "http proxy", raw: "http://127.0.0.1:7897", scheme: "http"},
		{name: "https proxy", raw: "https://proxy.example:8443", scheme: "https"},
		{name: "socks5 proxy", raw: "socks5://127.0.0.1:1080", scheme: "socks5"},
		{name: "socks5h proxy", raw: "socks5h://127.0.0.1:1080", scheme: "socks5h"},
		{name: "with credentials", raw: "http://user:pass@proxy:8080", scheme: "http"},
		{name: "missing scheme", raw: "127.0.0.1:7897", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://proxy:21", wantErr: true},
		{name: "missing host", raw: "http://", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			px, err := Parse(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want an error", tc.raw, px)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) returned an unexpected error: %v", tc.raw, err)
			}
			if px.IsDirect() != tc.direct {
				t.Fatalf("Parse(%q).IsDirect() = %v, want %v", tc.raw, px.IsDirect(), tc.direct)
			}
			if tc.scheme != "" && px.Scheme() != tc.scheme {
				t.Fatalf("Parse(%q).Scheme() = %q, want %q", tc.raw, px.Scheme(), tc.scheme)
			}
		})
	}
}

// TestDirectPolicyIgnoresEnvironment is the regression test for the bug
// this package exists to prevent.
//
// A direct policy must produce a transport whose Proxy func is nil, and a
// nil Proxy func is what makes net/http skip the environment entirely.
// Before this, api.New left Transport unset, so the REST client inherited
// http.DefaultTransport and honoured HTTP_PROXY — while the TCP probe,
// SSH, SFTP and WebSocket dialled directly.
func TestDirectPolicyIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	transport := Direct().Transport()
	if transport.Proxy != nil {
		t.Fatal("direct transport has a non-nil Proxy func; it would consult the environment")
	}

	// And prove it against the real DefaultTransport, which is what the
	// old code inherited.
	req, err := http.NewRequest(http.MethodGet, "http://bastion.invalid/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	inherited, err := http.DefaultTransport.(*http.Transport).Proxy(req)
	if err != nil {
		t.Fatalf("DefaultTransport.Proxy: %v", err)
	}
	if inherited == nil {
		t.Fatal("test is not meaningful: DefaultTransport did not pick up HTTP_PROXY")
	}
}

// TestExplicitPolicyIgnoresEnvironment asserts the other direction: a
// configured proxy wins over whatever the environment says, including a
// NO_PROXY entry that would otherwise bypass it.
func TestExplicitPolicyIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.invalid:9999")
	t.Setenv("NO_PROXY", "*")

	px, err := Parse("http://configured.invalid:3128")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	transport := px.Transport()
	if transport.Proxy == nil {
		t.Fatal("configured proxy produced a nil Proxy func")
	}
	req, err := http.NewRequest(http.MethodGet, "http://bastion.invalid/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("transport.Proxy: %v", err)
	}
	if got == nil || got.Host != "configured.invalid:3128" {
		t.Fatalf("transport.Proxy = %v, want the configured proxy", got)
	}
}

// TestHTTPConnectDialContext drives a real CONNECT handshake against a
// minimal proxy that records the target it was asked for.
func TestHTTPConnectDialContext(t *testing.T) {
	var asked atomic.Value
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "want CONNECT", http.StatusMethodNotAllowed)
			return
		}
		asked.Store(r.Host)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
		_ = buf.Flush()
		// Echo one byte so the caller can prove the tunnel is live.
		_, _ = conn.Write([]byte("K"))
	}))
	defer proxySrv.Close()

	proxyURL, err := url.Parse(proxySrv.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	px, err := Parse(proxyURL.String())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := px.DialContext(3*time.Second)(ctx, "tcp", "bastion.internal:2280")
	if err != nil {
		t.Fatalf("DialContext through proxy: %v", err)
	}
	defer conn.Close()

	got, _ := asked.Load().(string)
	if got != "bastion.internal:2280" {
		t.Fatalf("proxy was asked for %q, want bastion.internal:2280", got)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if reply[0] != 'K' {
		t.Fatalf("tunnel payload = %q, want %q", reply[0], 'K')
	}
}

// TestHTTPConnectRefused asserts a non-2xx CONNECT reply is an error, not
// a silently usable connection.
func TestHTTPConnectRefused(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer proxySrv.Close()

	px, err := Parse(proxySrv.URL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := px.DialContext(3*time.Second)(ctx, "tcp", "bastion.internal:2280")
	if err == nil {
		conn.Close()
		t.Fatal("a refused CONNECT must return an error")
	}
	if !strings.Contains(err.Error(), "refused CONNECT") {
		t.Fatalf("error = %v, want it to name the refusal", err)
	}
}

// TestHTTPConnectSendsBasicAuth asserts proxy credentials reach the
// proxy as a Proxy-Authorization header.
func TestHTTPConnectSendsBasicAuth(t *testing.T) {
	var gotAuth atomic.Value
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Proxy-Authorization"))
		hijacker := w.(http.Hijacker)
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n\r\n")
		_ = buf.Flush()
	}))
	defer proxySrv.Close()

	proxyURL, _ := url.Parse(proxySrv.URL)
	proxyURL.User = url.UserPassword("alice", "s3cret")
	px, err := Parse(proxyURL.String())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := px.DialContext(3*time.Second)(ctx, "tcp", "bastion.internal:2280")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	conn.Close()

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if got, _ := gotAuth.Load().(string); got != want {
		t.Fatalf("Proxy-Authorization = %q, want %q", got, want)
	}
}

// startSOCKS5Proxy runs a minimal SOCKS5 server that accepts the CONNECT
// command and then echoes a byte, recording the requested target.
func startSOCKS5Proxy(t *testing.T, requireAuth bool, wantUser, wantPass string) (string, *atomic.Value) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var asked atomic.Value
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				// Greeting: version, nmethods, methods.
				head := make([]byte, 2)
				if _, err := io.ReadFull(r, head); err != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				if _, err := io.ReadFull(r, methods); err != nil {
					return
				}
				if requireAuth {
					if _, err := c.Write([]byte{socksVersion, socksAuthUserPwd}); err != nil {
						return
					}
					// RFC 1929: version, ulen, user, plen, pass.
					authHead := make([]byte, 2)
					if _, err := io.ReadFull(r, authHead); err != nil {
						return
					}
					user := make([]byte, int(authHead[1]))
					if _, err := io.ReadFull(r, user); err != nil {
						return
					}
					plen := make([]byte, 1)
					if _, err := io.ReadFull(r, plen); err != nil {
						return
					}
					pass := make([]byte, int(plen[0]))
					if _, err := io.ReadFull(r, pass); err != nil {
						return
					}
					if string(user) != wantUser || string(pass) != wantPass {
						_, _ = c.Write([]byte{0x01, 0x01})
						return
					}
					if _, err := c.Write([]byte{0x01, socksSucceeded}); err != nil {
						return
					}
				} else if _, err := c.Write([]byte{socksVersion, socksAuthNone}); err != nil {
					return
				}

				// Request: version, cmd, rsv, atyp, addr, port.
				req := make([]byte, 4)
				if _, err := io.ReadFull(r, req); err != nil {
					return
				}
				var host string
				switch req[3] {
				case socksAddrDomain:
					l := make([]byte, 1)
					if _, err := io.ReadFull(r, l); err != nil {
						return
					}
					name := make([]byte, int(l[0]))
					if _, err := io.ReadFull(r, name); err != nil {
						return
					}
					host = string(name)
				case socksAddrIPv4:
					ip := make([]byte, 4)
					if _, err := io.ReadFull(r, ip); err != nil {
						return
					}
					host = net.IP(ip).String()
				default:
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(r, port); err != nil {
					return
				}
				asked.Store(host)
				// Success reply with an IPv4 bound address.
				_, _ = c.Write([]byte{socksVersion, socksSucceeded, 0x00, socksAddrIPv4, 0, 0, 0, 0, 0, 0})
				_, _ = c.Write([]byte("K"))
			}(conn)
		}
	}()
	return ln.Addr().String(), &asked
}

func TestSOCKS5DialContext(t *testing.T) {
	addr, asked := startSOCKS5Proxy(t, false, "", "")
	px, err := Parse("socks5://" + addr)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !px.IsSOCKS() {
		t.Fatal("socks5 policy should report IsSOCKS")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := px.DialContext(3*time.Second)(ctx, "tcp", "bastion.internal:2280")
	if err != nil {
		t.Fatalf("DialContext through socks5: %v", err)
	}
	defer conn.Close()

	if got, _ := asked.Load().(string); got != "bastion.internal" {
		t.Fatalf("socks5 proxy was asked for %q, want bastion.internal", got)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if reply[0] != 'K' {
		t.Fatalf("tunnel payload = %q, want %q", reply[0], 'K')
	}
}

func TestSOCKS5DialContextWithAuth(t *testing.T) {
	addr, _ := startSOCKS5Proxy(t, true, "alice", "s3cret")
	px, err := Parse("socks5://alice:s3cret@" + addr)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := px.DialContext(3*time.Second)(ctx, "tcp", "bastion.internal:2280")
	if err != nil {
		t.Fatalf("DialContext with socks5 auth: %v", err)
	}
	conn.Close()
}

// TestSOCKS5AuthRejected asserts a bad credential is an error rather than
// a connection that silently carries no payload.
func TestSOCKS5AuthRejected(t *testing.T) {
	addr, _ := startSOCKS5Proxy(t, true, "alice", "s3cret")
	px, err := Parse("socks5://alice:wrong@" + addr)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := px.DialContext(3*time.Second)(ctx, "tcp", "bastion.internal:2280")
	if err == nil {
		conn.Close()
		t.Fatal("a rejected SOCKS5 credential must return an error")
	}
}

// TestDialContextDirectMatchesNetDialer asserts the direct policy returns
// a dialer whose behaviour is the plain net.Dialer: no proxy in the path.
func TestDialContextDirectMatchesNetDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Direct().DialContext(3*time.Second)(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("direct dial: %v", err)
	}
	conn.Close()
}
