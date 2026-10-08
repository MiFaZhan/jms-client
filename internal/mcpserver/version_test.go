package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestImplementationVersionPrefersConfigured pins the fix for a version
// that was hardcoded to "1.0.0" regardless of the actual build: every MCP
// client was told the server was 1.0.0 while `jms version` reported
// something else entirely.
func TestImplementationVersionPrefersConfigured(t *testing.T) {
	got := implementationVersion(Options{Version: "v9.9.9"})
	if got != "v9.9.9" {
		t.Fatalf("implementationVersion() = %q, want the configured version", got)
	}
}

// TestImplementationVersionFallsBack asserts an empty option yields the
// package default rather than an empty string, which some clients reject.
func TestImplementationVersionFallsBack(t *testing.T) {
	for _, in := range []string{"", "   "} {
		got := implementationVersion(Options{Version: in})
		if got != ServerVersion {
			t.Fatalf("implementationVersion(%q) = %q, want %q", in, got, ServerVersion)
		}
		if strings.TrimSpace(got) == "" {
			t.Fatal("implementationVersion returned an empty version")
		}
	}
}

// TestServerVersionIsNotTheHardcodedOne guards against the literal coming
// back: the old value was "1.0.0", which never matched a real release.
func TestServerVersionIsNotTheHardcodedOne(t *testing.T) {
	if ServerVersion == "1.0.0" {
		t.Fatal(`ServerVersion is back to the old hardcoded "1.0.0"`)
	}
}

// TestInitializeHandshakeReportsVersion drives a real initialize handshake
// over the SDK's in-memory transport and reads back what a client actually
// receives.
//
// Asserting the wire result rather than an internal field is deliberate:
// the bug being fixed was visible only to clients, and the field that held
// it is unexported.
func TestInitializeHandshakeReportsVersion(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	s := New(Options{Version: "v1.2.3"})
	srv := s.build()

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	info := session.InitializeResult().ServerInfo
	if info == nil {
		t.Fatal("initialize result carried no server info")
	}
	if info.Name != ServerName {
		t.Errorf("server name = %q, want %q", info.Name, ServerName)
	}
	if info.Version != "v1.2.3" {
		t.Fatalf("server version = %q, want v1.2.3 (this is what MCP clients see)", info.Version)
	}
}

// TestInitializeHandshakeDefaultsVersion asserts a server built without an
// explicit version still advertises a non-empty one.
func TestInitializeHandshakeDefaultsVersion(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	s := New(Options{})
	srv := s.build()

	go func() { _ = srv.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	info := session.InitializeResult().ServerInfo
	if info == nil || strings.TrimSpace(info.Version) == "" {
		t.Fatalf("server version is empty: %+v", info)
	}
	if info.Version != ServerVersion {
		t.Fatalf("server version = %q, want the default %q", info.Version, ServerVersion)
	}
}
