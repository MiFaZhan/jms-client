// Package netproxy resolves how jms reaches the network: directly, or
// through an explicit HTTP CONNECT / SOCKS5 proxy.
//
// Design contract (DESIGN.md §6.1): jms defaults to a DIRECT connection
// and never consults HTTP_PROXY / HTTPS_PROXY / NO_PROXY. The
// environment-variable convention is the wrong default for a bastion
// client: a developer machine running a transparent-proxy tool (Clash,
// Surge, corporate MITM) exports those variables globally, and the
// tool's own DIRECT rules then apply to the bastion address. The result
// is a request that leaves the process through a proxy nobody asked for,
// with the proxy's own error (typically an empty 502) standing in for
// the real network result.
//
// The failure this prevents is subtle and worth stating plainly: because
// only the REST client used to inherit http.DefaultTransport, the TCP
// probe dialled directly while the login went through the proxy. Probe
// and login measured two different networks, so the failover policy in
// §6 reasoned about the wrong one. Every transport now shares one
// policy, resolved here.
//
// Dependency direction: netproxy depends on nothing else in this module,
// and adds no third-party dependency: the SOCKS5 and HTTP CONNECT
// handshakes are small enough to own outright.
package netproxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Proxy is a resolved proxy policy: either direct, or one explicit proxy
// URL. The zero value is direct, which is the default.
type Proxy struct {
	url *url.URL
}

// Direct returns the direct-connection policy (the default).
func Direct() Proxy {
	return Proxy{}
}

// Parse resolves a configured proxy string into a policy.
//
// An empty (or whitespace-only) string is the direct policy — the same
// value as Direct(). The words "direct", "none" and "off" are also
// accepted so a per-server entry can explicitly opt out of a global
// proxy: "inherit the global default" and "force a direct connection"
// are different intentions and must not collapse into the same syntax.
//
// Accepted schemes are http, https and socks5; anything else is rejected
// rather than silently ignored, because a typo that silently disables
// the proxy is exactly the bug this package exists to prevent.
func Parse(raw string) (Proxy, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Direct(), nil
	}
	switch strings.ToLower(trimmed) {
	case "direct", "none", "off":
		return Direct(), nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return Proxy{}, fmt.Errorf("proxy %q is not a valid URL: %w", trimmed, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	case "":
		return Proxy{}, fmt.Errorf("proxy %q is missing a scheme (use http:// or socks5://)", trimmed)
	default:
		return Proxy{}, fmt.Errorf("proxy %q has unsupported scheme %q (use http, https or socks5)",
			trimmed, u.Scheme)
	}
	if u.Host == "" {
		return Proxy{}, fmt.Errorf("proxy %q is missing a host", trimmed)
	}
	return Proxy{url: u}, nil
}

// String renders the policy for logs and audit events: "direct" or the
// proxy URL.
func (p Proxy) String() string {
	if p.IsDirect() {
		return "direct"
	}
	return p.url.String()
}

// IsDirect reports whether the policy connects directly.
func (p Proxy) IsDirect() bool {
	return p.url == nil
}

// URL returns the proxy URL, or nil when the policy is direct.
func (p Proxy) URL() *url.URL {
	if p.IsDirect() {
		return nil
	}
	return p.url
}

// Scheme returns the proxy scheme, or "" when the policy is direct.
func (p Proxy) Scheme() string {
	if p.IsDirect() {
		return ""
	}
	return strings.ToLower(p.url.Scheme)
}

// IsSOCKS reports whether the proxy speaks SOCKS5.
func (p Proxy) IsSOCKS() bool {
	switch p.Scheme() {
	case "socks5", "socks5h":
		return true
	default:
		return false
	}
}

// httpProxyFunc adapts the policy to the func(*http.Request)(*url.URL,error)
// shape that net/http and gorilla/websocket expect. A nil return means
// "connect directly", which is how a direct policy is expressed.
func (p Proxy) httpProxyFunc() func(*http.Request) (*url.URL, error) {
	if p.IsDirect() {
		return nil
	}
	target := p.url
	return func(*http.Request) (*url.URL, error) { return target, nil }
}

// Transport returns an *http.Transport that honours the policy.
//
// It is built from a clone of http.DefaultTransport so the tuned
// defaults (connection pooling, HTTP/2, sane timeouts) are kept, then the
// Proxy field is set EXPLICITLY: nil for a direct policy. That explicit
// nil is the whole point — leaving the field unset is what made the REST
// client inherit ProxyFromEnvironment in the first place.
func (p Proxy) Transport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Not reachable with the standard library, but a clone of a
		// hand-built transport is the honest fallback.
		return &http.Transport{Proxy: p.httpProxyFunc()}
	}
	transport := base.Clone()
	transport.Proxy = p.httpProxyFunc()
	return transport
}

// HTTPProxyFunc exposes the policy as an http.ProxyFromEnvironment-shaped
// function, for callers that must set the field themselves (the gorilla
// WebSocket dialer).
func (p Proxy) HTTPProxyFunc() func(*http.Request) (*url.URL, error) {
	return p.httpProxyFunc()
}

// DialContext returns a dial function bound to the policy.
//
// For a direct policy it is net.Dialer.DialContext, so behaviour is
// byte-for-byte what it was before this package existed. For a SOCKS5
// proxy it routes through the proxy; for an HTTP proxy it issues a
// CONNECT and returns the tunnelled connection.
//
// The returned function never performs an environment lookup: the policy
// is captured at construction time.
func (p Proxy) DialContext(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if p.IsDirect() {
		return dialer.DialContext
	}
	if p.IsSOCKS() {
		return p.socksDialContext(dialer)
	}
	return p.httpConnectDialContext(dialer)
}

// proxyAddr returns the proxy's own host:port, defaulting the port from
// the scheme.
func (p Proxy) proxyAddr() string {
	if p.url.Port() != "" {
		return p.url.Host
	}
	port := "80"
	if p.Scheme() == "https" {
		port = "443"
	}
	return net.JoinHostPort(p.url.Hostname(), port)
}

// httpConnectDialContext dials through an HTTP CONNECT proxy.
//
// The handshake is bounded by the caller's timeout: a proxy that accepts
// the TCP connection and then stalls must not hang the caller, which is
// the same DROP-firewall concern DESIGN.md §4.5 raises for direct dials.
func (p Proxy) httpConnectDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	target := p.proxyAddr()
	return func(ctx context.Context, network, hostport string) (net.Conn, error) {
		conn, err := base.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, fmt.Errorf("dial proxy %s: %w", target, err)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		if err := httpConnectHandshake(conn, p.url, hostport); err != nil {
			_ = conn.Close()
			return nil, err
		}
		// Clear the handshake deadline: the caller owns the connection
		// lifetime from here.
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// httpConnectHandshake writes a CONNECT request and verifies the reply.
//
// It is hand-rolled rather than routed through http.Transport because the
// caller needs the raw tunnelled net.Conn (for SSH, or for a WebSocket
// upgrade the standard transport cannot express).
func httpConnectHandshake(conn net.Conn, proxyURL *url.URL, target string) error {
	var request strings.Builder
	request.WriteString("CONNECT " + target + " HTTP/1.1\r\n")
	request.WriteString("Host: " + target + "\r\n")
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		request.WriteString("Proxy-Authorization: Basic " +
			basicAuth(proxyURL.User.Username(), password) + "\r\n")
	}
	request.WriteString("\r\n")

	if _, err := conn.Write([]byte(request.String())); err != nil {
		return fmt.Errorf("write CONNECT to proxy: %w", err)
	}
	status, err := readCONNECTStatus(conn)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("proxy refused CONNECT to %s: HTTP %d", target, status)
	}
	return nil
}

// maxConnectResponse bounds the CONNECT reply headers, so a hostile or
// broken proxy cannot make the client read unbounded data.
const maxConnectResponse = 8192

// readCONNECTStatus reads the proxy's reply headers and returns the
// status code.
func readCONNECTStatus(conn net.Conn) (int, error) {
	buf := make([]byte, 0, 512)
	one := make([]byte, 1)
	for {
		if len(buf) >= maxConnectResponse {
			return 0, errors.New("proxy CONNECT response is too large")
		}
		if _, err := io.ReadFull(conn, one); err != nil {
			return 0, fmt.Errorf("read CONNECT response from proxy: %w", err)
		}
		buf = append(buf, one[0])
		if len(buf) >= 4 && string(buf[len(buf)-4:]) == "\r\n\r\n" {
			break
		}
	}

	statusLine, _, _ := strings.Cut(string(buf), "\r\n")
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed proxy CONNECT response: %q", statusLine)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("malformed proxy CONNECT status %q: %w", fields[1], err)
	}
	return status, nil
}

// SOCKS5 protocol constants (RFC 1928).
const (
	socksVersion     = 0x05
	socksAuthNone    = 0x00
	socksAuthUserPwd = 0x02
	socksAuthFailed  = 0xFF
	socksCmdConnect  = 0x01
	socksAddrIPv4    = 0x01
	socksAddrDomain  = 0x03
	socksAddrIPv6    = 0x04
	socksSucceeded   = 0x00
)

// socksDialContext dials through a SOCKS5 proxy (RFC 1928), with
// username/password authentication (RFC 1929) when the proxy URL carries
// credentials.
func (p Proxy) socksDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	target := p.proxyAddr()
	username := ""
	password := ""
	if p.url.User != nil {
		username = p.url.User.Username()
		password, _ = p.url.User.Password()
	}
	return func(ctx context.Context, network, hostport string) (net.Conn, error) {
		conn, err := base.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, fmt.Errorf("dial socks5 proxy %s: %w", target, err)
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		if err := socksHandshake(conn, username, password, hostport); err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// socksHandshake performs the SOCKS5 greeting, optional authentication
// and CONNECT request.
func socksHandshake(conn net.Conn, username, password, target string) error {
	methods := []byte{socksAuthNone}
	if username != "" {
		methods = []byte{socksAuthUserPwd, socksAuthNone}
	}
	greeting := append([]byte{socksVersion, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 greeting reply: %w", err)
	}
	if reply[0] != socksVersion {
		return fmt.Errorf("socks5 proxy %s speaks version %d, want 5", conn.RemoteAddr(), reply[0])
	}
	switch reply[1] {
	case socksAuthNone:
	case socksAuthUserPwd:
		if username == "" {
			return errors.New("socks5 proxy requires username/password authentication but none is configured")
		}
		if err := socksUserPassAuth(conn, username, password); err != nil {
			return err
		}
	case socksAuthFailed:
		return errors.New("socks5 proxy rejected every offered authentication method")
	default:
		return fmt.Errorf("socks5 proxy selected unsupported auth method %#x", reply[1])
	}

	if err := socksConnect(conn, target); err != nil {
		return err
	}
	return nil
}

// socksUserPassAuth performs RFC 1929 username/password authentication.
func socksUserPassAuth(conn net.Conn, username, password string) error {
	if len(username) > 255 || len(password) > 255 {
		return errors.New("socks5 username/password must each be at most 255 bytes")
	}
	request := make([]byte, 0, 3+len(username)+len(password))
	request = append(request, 0x01, byte(len(username)))
	request = append(request, username...)
	request = append(request, byte(len(password)))
	request = append(request, password...)
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("socks5 auth request: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 auth reply: %w", err)
	}
	if reply[1] != socksSucceeded {
		return errors.New("socks5 proxy rejected the username/password")
	}
	return nil
}

// socksConnect sends the CONNECT request and reads the bound address
// reply.
func socksConnect(conn net.Conn, target string) error {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("socks5 target %q: %w", target, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("socks5 target port %q: %w", portText, err)
	}

	request := []byte{socksVersion, socksCmdConnect, 0x00}
	switch {
	case net.ParseIP(host) == nil:
		if len(host) > 255 {
			return fmt.Errorf("socks5 target hostname %q is too long", host)
		}
		request = append(request, socksAddrDomain, byte(len(host)))
		request = append(request, host...)
	default:
		ip := net.ParseIP(host)
		if v4 := ip.To4(); v4 != nil {
			request = append(request, socksAddrIPv4)
			request = append(request, v4...)
		} else {
			request = append(request, socksAddrIPv6)
			request = append(request, ip.To16()...)
		}
	}
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("socks5 connect request: %w", err)
	}

	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 connect reply: %w", err)
	}
	if reply[0] != socksVersion {
		return fmt.Errorf("socks5 connect reply version %d, want 5", reply[0])
	}
	if reply[1] != socksSucceeded {
		return fmt.Errorf("socks5 proxy refused CONNECT to %s: %s", target, socksReplyMessage(reply[1]))
	}
	// Drain the bound address so the stream is positioned at the payload.
	if err := discardSocksAddr(conn, reply[3]); err != nil {
		return err
	}
	return nil
}

// discardSocksAddr consumes the BND.ADDR/BND.PORT fields of a reply.
func discardSocksAddr(conn net.Conn, atyp byte) error {
	var length int
	switch atyp {
	case socksAddrIPv4:
		length = 4
	case socksAddrIPv6:
		length = 16
	case socksAddrDomain:
		head := make([]byte, 1)
		if _, err := io.ReadFull(conn, head); err != nil {
			return fmt.Errorf("socks5 bound address: %w", err)
		}
		length = int(head[0])
	default:
		return fmt.Errorf("socks5 reply has unknown address type %#x", atyp)
	}
	// Address plus the two-byte port.
	if _, err := io.ReadFull(conn, make([]byte, length+2)); err != nil {
		return fmt.Errorf("socks5 bound address: %w", err)
	}
	return nil
}

// socksReplyMessage renders a SOCKS5 reply code for an error message.
func socksReplyMessage(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("reply code %#x", code)
	}
}

// basicAuth encodes a username/password pair for a Proxy-Authorization
// header.
func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}
