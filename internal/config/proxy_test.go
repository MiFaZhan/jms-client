package config

import (
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/netproxy"
)

// TestProxyForInheritsGlobal is the core precedence rule: a server with no
// proxy of its own uses the global setting.
func TestProxyForInheritsGlobal(t *testing.T) {
	cfg := &AppConfig{
		Proxy: "http://global.invalid:3128",
		Servers: map[string]*ServerConfig{
			"inherit": {Name: "inherit", Username: "u", Internal: "http://a:2280/"},
			"own":     {Name: "own", Username: "u", Internal: "http://b:2280/", Proxy: "http://own.invalid:3128"},
			"off":     {Name: "off", Username: "u", Internal: "http://c:2280/", Proxy: "direct"},
		},
	}

	tests := []struct {
		server string
		want   string
	}{
		{server: "inherit", want: "http://global.invalid:3128"},
		{server: "own", want: "http://own.invalid:3128"},
		{server: "off", want: "direct"},
	}
	for _, tc := range tests {
		got := cfg.ProxyFor(cfg.Servers[tc.server])
		if got != tc.want {
			t.Errorf("ProxyFor(%s) = %q, want %q", tc.server, got, tc.want)
		}
	}
}

// TestProxyForNilSafety asserts the safe default when there is no
// configuration at all: direct.
func TestProxyForNilSafety(t *testing.T) {
	var nilCfg *AppConfig
	if got := nilCfg.ProxyFor(&ServerConfig{Name: "s"}); got != "" {
		t.Errorf("nil config ProxyFor = %q, want the empty (direct) value", got)
	}
	cfg := &AppConfig{Proxy: "http://global.invalid:3128"}
	if got := cfg.ProxyFor(nil); got != "http://global.invalid:3128" {
		t.Errorf("ProxyFor(nil server) = %q, want the global value", got)
	}
}

// TestValidateProxy pins the accepted grammar for the configuration file.
func TestValidateProxy(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{raw: "", wantErr: false},
		{raw: "direct", wantErr: false},
		{raw: "none", wantErr: false},
		{raw: "off", wantErr: false},
		{raw: "DIRECT", wantErr: false},
		{raw: "http://127.0.0.1:7897", wantErr: false},
		{raw: "https://proxy.example:8443", wantErr: false},
		{raw: "socks5://127.0.0.1:1080", wantErr: false},
		{raw: "socks5h://user:pass@127.0.0.1:1080", wantErr: false},
		{raw: "127.0.0.1:7897", wantErr: true},
		{raw: "ftp://proxy:21", wantErr: true},
		{raw: "http://", wantErr: true},
	}
	for _, tc := range tests {
		err := validateProxy(tc.raw)
		if tc.wantErr && err == nil {
			t.Errorf("validateProxy(%q) = nil, want an error", tc.raw)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateProxy(%q) = %v, want nil", tc.raw, err)
		}
	}
}

// TestProxyGrammarMatchesNetproxy is the guard that keeps config's
// dependency-free validator in sync with the package that actually uses
// the value. config must not import netproxy (it is a leaf package), so
// the two grammars are duplicated; this test is what stops them drifting.
func TestProxyGrammarMatchesNetproxy(t *testing.T) {
	values := []string{
		"", "   ", "direct", "none", "off", "DIRECT",
		"http://127.0.0.1:7897", "https://proxy.example:8443",
		"socks5://127.0.0.1:1080", "socks5h://user:pass@127.0.0.1:1080",
		"127.0.0.1:7897", "ftp://proxy:21", "http://", "://nope",
	}
	for _, v := range values {
		cfgErr := validateProxy(v)
		_, netErr := netproxy.Parse(v)
		if (cfgErr == nil) != (netErr == nil) {
			t.Errorf("grammars disagree on %q: config=%v netproxy=%v", v, cfgErr, netErr)
		}
	}
}

// TestValidateRejectsBadServerProxy asserts a bad per-server proxy is
// caught when the server entry is validated, not silently ignored.
func TestValidateRejectsBadServerProxy(t *testing.T) {
	srv := &ServerConfig{
		Name:     "bad",
		Username: "u",
		Internal: "http://host:2280/",
		Proxy:    "not-a-url",
	}
	err := Validate(srv)
	if err == nil {
		t.Fatal("Validate accepted an invalid proxy")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("error = %v, want it to name the proxy", err)
	}
}

// TestProxyRoundTripsThroughTOML asserts the proxy settings survive a
// save/load cycle, both global and per-server.
func TestProxyRoundTripsThroughTOML(t *testing.T) {
	cfg := &AppConfig{
		Version: ConfigVersion,
		Default: "main",
		Proxy:   "socks5://127.0.0.1:1080",
		Servers: map[string]*ServerConfig{
			"main": {
				Name:     "main",
				Username: "u",
				Internal: "http://host:2280/",
				Proxy:    "http://own.invalid:3128",
			},
			"other": {
				Name:     "other",
				Username: "u",
				External: "https://jump.example.com/",
				Proxy:    "direct",
			},
		},
	}

	data, err := marshalConfig(cfg)
	if err != nil {
		t.Fatalf("marshalConfig: %v", err)
	}
	if !strings.Contains(string(data), `proxy = "socks5://127.0.0.1:1080"`) {
		t.Fatalf("rendered config is missing the global proxy:\n%s", data)
	}

	parsed, err := parseConfig("test.toml", data)
	if err != nil {
		t.Fatalf("parseConfig: %v\nrendered:\n%s", err, data)
	}
	if parsed.Proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("global proxy = %q, want the original", parsed.Proxy)
	}
	if got := parsed.Servers["main"].Proxy; got != "http://own.invalid:3128" {
		t.Errorf("main proxy = %q, want the original", got)
	}
	if got := parsed.Servers["other"].Proxy; got != "direct" {
		t.Errorf("other proxy = %q, want %q", got, "direct")
	}
}

// TestProxyOmittedWhenUnset asserts a config with no proxy renders no
// proxy lines, so existing files are not rewritten with noise.
func TestProxyOmittedWhenUnset(t *testing.T) {
	cfg := &AppConfig{
		Version: ConfigVersion,
		Default: "main",
		Servers: map[string]*ServerConfig{
			"main": {Name: "main", Username: "u", Internal: "http://host:2280/"},
		},
	}
	data, err := marshalConfig(cfg)
	if err != nil {
		t.Fatalf("marshalConfig: %v", err)
	}
	if strings.Contains(string(data), "proxy") {
		t.Fatalf("unset proxy should not be rendered:\n%s", data)
	}
}

// TestParseConfigRejectsBadGlobalProxy asserts a malformed global proxy
// fails the load rather than being ignored.
func TestParseConfigRejectsBadGlobalProxy(t *testing.T) {
	_, err := parseConfig("test.toml", []byte(`
version = 2
default_server = "main"
proxy = "ftp://nope:21"

[servers.main]
internal = "http://host:2280/"
username = "u"
`))
	if err == nil {
		t.Fatal("parseConfig accepted an invalid global proxy")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("error = %v, want it to name the proxy", err)
	}
}
