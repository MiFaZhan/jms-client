//go:build live

// Live probe against a real JumpServer. Not part of the default test run:
//
//	go test -tags live ./internal/webxfer/ -run Live -v
//
// It reuses the developer's jms configuration (config file plus credential
// store, exactly like the CLI), opens the file-manager WebSocket, uploads
// a small probe file to JMS_PROBE_DIR (default /tmp), downloads it back,
// compares the bytes and removes the probe file through a terminal.
//
// Environment:
//
//	JMS_PROBE_ASSET  asset name to use; the first authorized asset when unset
//	JMS_PROBE_DIR    remote directory for the probe file (default /tmp)
package webxfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/netproxy"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// liveSession logs in with the stored credentials, trying every configured
// address the way a cold CLI start would.
func liveSession(t *testing.T) *auth.Session {
	t.Helper()
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	srv, err := cfg.DefaultServer()
	if err != nil {
		t.Fatalf("default server: %v", err)
	}

	creds := config.Chain(config.NewEnvStore(), config.NewKeyringStore())
	password, err := creds.Get(srv.Name, config.CredPassword)
	if err != nil {
		t.Fatalf("password for %q: %v", srv.Name, err)
	}
	secret, _ := creds.Get(srv.Name, config.CredOTP)

	px, err := netproxy.Parse(cfg.ProxyFor(srv))
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}

	cands := []string{srv.Preferred()}
	for _, kind := range []string{config.KindInternal, config.KindExternal} {
		if u, ok := srv.URLFor(kind); ok {
			cands = append(cands, u)
		}
	}
	seen := map[string]bool{}
	var lastErr error
	for _, u := range cands {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		sess, err := auth.LoginWithProxy(ctx, srv, u,
			auth.Credentials{Password: password, OTPSecret: secret},
			func() (string, error) { return "", fmt.Errorf("live probe cannot prompt for MFA") },
			px)
		cancel()
		if err == nil {
			t.Logf("logged in via %s", u)
			return sess
		}
		lastErr = err
		t.Logf("login via %s failed: %v", u, err)
	}
	t.Fatalf("no address answered: last error: %v", lastErr)
	return nil
}

// TestLiveUploadDownload is the end-to-end protocol probe.
func TestLiveUploadDownload(t *testing.T) {
	sess := liveSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	name := os.Getenv("JMS_PROBE_ASSET")
	if name == "" {
		list, err := assets.List(ctx, sess.Client, 1)
		if err != nil || len(list) == 0 {
			t.Fatalf("no asset to probe: %v", err)
		}
		name = list[0].Name
	}
	t.Logf("probe asset: %s", name)

	info, err := assets.Resolve(ctx, sess.Client, name, "", "ssh")
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}

	c, err := Dial(ctx, sess, info)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	remoteDir := os.Getenv("JMS_PROBE_DIR")
	if remoteDir == "" {
		remoteDir = "/tmp"
	}
	stamp := time.Now().UnixNano()
	remote := fmt.Sprintf("%s/jms-webxfer-probe-%d.bin", remoteDir, stamp)

	payload := make([]byte, 3<<20) // 3 MiB: forces multiple 1 MiB chunks
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	local := filepath.Join(t.TempDir(), "probe.bin")
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	start := time.Now()
	sent, err := c.Upload(ctx, local, remote, DefaultChunk)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Logf("uploaded %d bytes in %s (%.1f KiB/s)", sent, time.Since(start).Round(time.Millisecond),
		float64(sent)/1024/time.Since(start).Seconds())
	if sent != int64(len(payload)) {
		t.Fatalf("uploaded %d bytes, wanted %d", sent, len(payload))
	}

	back := filepath.Join(t.TempDir(), "probe-back.bin")
	start = time.Now()
	got, err := c.Download(ctx, remote, back)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	t.Logf("downloaded %d bytes in %s (%.1f KiB/s)", got, time.Since(start).Round(time.Millisecond),
		float64(got)/1024/time.Since(start).Seconds())

	gotBytes, err := os.ReadFile(back)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	wantSum := sha256.Sum256(payload)
	gotSum := sha256.Sum256(gotBytes)
	if !bytes.Equal(payload, gotBytes) {
		t.Fatalf("round-trip mismatch: want sha256 %s, got %s",
			hex.EncodeToString(wantSum[:8]), hex.EncodeToString(gotSum[:8]))
	}
	t.Logf("round-trip verified (sha256 %s)", hex.EncodeToString(wantSum[:8]))

	// Cleanup through a terminal, the pre-existing exec path.
	term, err := transport.Connect(ctx, sess, info, transport.ConnectOptions{})
	if err != nil {
		t.Logf("cleanup skipped: terminal connect failed: %v", err)
		return
	}
	defer func() { _ = term.Close() }()
	if _, err := term.Execute(ctx, "rm -f "+remote, time.Minute); err != nil {
		t.Logf("cleanup rm %s failed: %v", remote, err)
	} else {
		t.Logf("cleaned up %s", remote)
	}
}
