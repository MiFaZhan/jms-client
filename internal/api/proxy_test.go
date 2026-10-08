package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/netproxy"
)

// TestNewDoesNotInheritEnvironmentProxy is the regression test for the
// defect that motivated internal/netproxy.
//
// api.New used to leave http.Client.Transport unset, so the REST client
// silently inherited http.DefaultTransport and therefore
// http.ProxyFromEnvironment. On a machine with HTTP_PROXY set (a
// transparent-proxy tool like Clash exports it globally), the login went
// through the proxy while the TCP probe, SSH, SFTP and WebSocket all
// dialled directly. The probe and the login then measured different
// networks, and the 「端点故障转移」 policy acted on the wrong one.
func TestNewDoesNotInheritEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	client := New("http://bastion.invalid:2280/")
	transport, ok := client.HTTP().Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("New left the transport unset (%T); it would inherit the environment",
			client.HTTP().Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("New produced a transport with a non-nil Proxy func; it would consult the environment")
	}
}

// TestNewWithProxyRoutesThroughTheProxy asserts the configured policy is
// actually applied, so a user who wants a proxy gets one.
func TestNewWithProxyRoutesThroughTheProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.invalid:9999")
	t.Setenv("NO_PROXY", "*")

	px, err := netproxy.Parse("http://configured.invalid:3128")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	client := NewWithProxy("http://bastion.invalid:2280/", px)
	transport, ok := client.HTTP().Transport.(*http.Transport)
	if !ok || transport == nil || transport.Proxy == nil {
		t.Fatal("NewWithProxy did not install a Proxy func")
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

// TestClientReachesServerDirectlyWhileProxyEnvIsHostile is the end-to-end
// half of the regression: with a proxy configured in the environment that
// refuses every request, a direct client must still reach a real server.
func TestClientReachesServerDirectlyWhileProxyEnvIsHostile(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// A proxy that would fail the request if it were ever consulted.
	t.Setenv("HTTP_PROXY", srv.URL)
	t.Setenv("HTTPS_PROXY", srv.URL)
	t.Setenv("NO_PROXY", "")

	client := New(srv.URL)
	var out struct {
		OK bool `json:"ok"`
	}
	if err := client.Get(t.Context(), "/", nil, &out); err != nil {
		t.Fatalf("direct request failed: %v", err)
	}
	if !out.OK {
		t.Fatal("server response was not decoded")
	}
	if hits.Load() != 1 {
		t.Fatalf("server saw %d requests, want 1", hits.Load())
	}
}

// TestClientUsesConfiguredProxy asserts the positive path: with a proxy
// policy configured, the request really does go to the proxy.
func TestClientUsesConfiguredProxy(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if !r.URL.IsAbs() {
			t.Errorf("proxy received a non-absolute request URL: %q", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxy.Close()

	px, err := netproxy.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	client := NewWithProxy("http://bastion.invalid:2280/", px)

	var out struct {
		OK bool `json:"ok"`
	}
	if err := client.Get(t.Context(), "/api/v1/", nil, &out); err != nil {
		t.Fatalf("proxied request failed: %v", err)
	}
	if !out.OK {
		t.Fatal("server response was not decoded")
	}
	if proxied.Load() != 1 {
		t.Fatalf("proxy saw %d requests, want 1", proxied.Load())
	}
}

// TestNormalizeBaseURLUnaffected is a small guard that the proxy work did
// not disturb URL handling.
func TestNormalizeBaseURLUnaffected(t *testing.T) {
	if got := normalizeBaseURL("  http://host:2280/  "); got != "http://host:2280" {
		t.Fatalf("normalizeBaseURL = %q", got)
	}
	if got := normalizeBaseURL("host:2280"); !strings.HasPrefix(got, "https://") {
		t.Fatalf("normalizeBaseURL = %q, want an https:// prefix", got)
	}
}
