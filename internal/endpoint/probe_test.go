package endpoint

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/netproxy"
)

// startConnectProxy runs a minimal HTTP CONNECT proxy that records the
// targets it was asked for and completes the handshake.
func startConnectProxy(t *testing.T) (addr string, asked chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	asked = make(chan string, 8)
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
				fields := splitFields(line)
				if len(fields) >= 2 {
					asked <- fields[1]
				}
				// Drain the remaining headers.
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
	return ln.Addr().String(), asked
}

func splitFields(line string) []string {
	var out []string
	start := -1
	for i, r := range line {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			if start >= 0 {
				out = append(out, line[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, line[start:])
	}
	return out
}

// TestProbeWithDirectIsTCPProbe asserts the default path is untouched: a
// direct policy returns the same function, so no existing behaviour moves.
func TestProbeWithDirectIsTCPProbe(t *testing.T) {
	got := ProbeWith(netproxy.Direct())
	if got == nil {
		t.Fatal("ProbeWith(direct) returned nil")
	}
	// Function values cannot be compared directly; assert via behaviour on
	// an unreachable target, which both must report as unreachable.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if got(ctx, "http://127.0.0.1:1/", 300*time.Millisecond) {
		t.Fatal("a closed port must probe as unreachable")
	}
}

// TestProbeWithProxyRoutesThroughProxy is the regression test for the
// probe/login split: with a proxy configured, the probe must travel
// through the proxy rather than dialling the bastion directly.
func TestProbeWithProxyRoutesThroughProxy(t *testing.T) {
	proxyAddr, asked := startConnectProxy(t)

	px, err := netproxy.Parse("http://" + proxyAddr)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	probe := ProbeWith(px)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The target is deliberately unroutable: only a probe that goes through
	// the proxy can succeed, so success proves the routing.
	reachable := probe(ctx, "http://bastion.invalid:2280/", 2*time.Second)
	if !reachable {
		t.Fatal("probe through the proxy reported unreachable")
	}

	select {
	case got := <-asked:
		if got != "bastion.invalid:2280" {
			t.Fatalf("proxy was asked for %q, want bastion.invalid:2280", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the proxy was never contacted; the probe dialled directly")
	}
}

// TestTCPProbeDialsDirectly asserts the legacy function keeps its direct
// behaviour even when the environment asks for a proxy.
func TestTCPProbeDialsDirectly(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !TCPProbe(ctx, "http://"+ln.Addr().String()+"/", time.Second) {
		t.Fatal("TCPProbe failed against a listening socket")
	}
}

// TestProbeWithProxyReportsUnreachableWhenProxyRefuses asserts a proxy
// that rejects CONNECT yields "unreachable" rather than a false positive.
func TestProbeWithProxyReportsUnreachableWhenProxyRefuses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					h, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if h == "\r\n" || h == "\n" {
						break
					}
				}
				_, _ = c.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
				_, _ = io.Copy(io.Discard, c)
			}(conn)
		}
	}()

	px, err := netproxy.Parse("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ProbeWith(px)(ctx, "http://bastion.invalid:2280/", 2*time.Second) {
		t.Fatal("a refused CONNECT must probe as unreachable")
	}
}
