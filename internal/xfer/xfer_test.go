package xfer

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// sftpPath renders an absolute host path the way the pkg/sftp server in the
// harness expects to receive it: slash-separated with a leading "/".
// On Windows "C:\a\b" becomes "/C:/a/b"; on Unix the path is unchanged.
// Remote paths are always POSIX paths, even on a Windows client.
func sftpPath(p string) string {
	s := filepath.ToSlash(p)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return s
}

// writeLocal writes name into a fresh temp directory and returns its path.
func writeLocal(t *testing.T, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// tempPath returns a not-yet-existing file path inside a fresh directory.
func tempPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// pattern returns n bytes whose value depends on the offset, so that a
// chunk written to the wrong offset is detectable rather than accidentally
// equal.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// requireBytes fails when got differs from want, naming the side under test.
func requireBytes(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: content differs (got %d bytes, want %d)", what, len(got), len(want))
	}
}

// ---------------------------------------------------------------------------
// 1. the token rule that roots SFTP at the asset (DESIGN.md §3.1)
// ---------------------------------------------------------------------------

func TestSFTPTokenRequestUsesSFTPProtocol(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("token rule\n")
	local := writeLocal(t, "src.bin", content)
	remote := tempPath(t, "dst.bin")

	if _, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    sftpPath(remote),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	reqs := env.tokens.recorded()
	if len(reqs) == 0 {
		t.Fatal("no connection token was requested during an SFTP transfer")
	}
	req := reqs[0]
	if req.Path != transport.PathConnectionToken {
		t.Errorf("token path = %q, want %q", req.Path, transport.PathConnectionToken)
	}
	// The account must be the alias, and the protocol/connect_method pair
	// must be sftp/web_sftp: an ssh/web_cli token's SFTP subsystem lands on
	// the KoKo virtual root and every path fails.
	want := map[string]string{
		"protocol":       "sftp",
		"connect_method": "web_sftp",
		"asset":          "asset-1",
		"account":        "@USER",
	}
	for key, value := range want {
		if got := req.Body[key]; got != value {
			t.Errorf("token body %s = %q, want %q", key, got, value)
		}
	}
	// The token body carries identity and routing only: a credential must
	// never be posted from this layer.
	if len(req.Body) != len(want) {
		t.Errorf("token body has %d fields, want exactly %d: %v", len(req.Body), len(want), req.Body)
	}
}

// ---------------------------------------------------------------------------
// 2-5. byte copies, chunking and parallelism
// ---------------------------------------------------------------------------

func TestUploadCopiesBytes(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("upload me, verbatim\n")
	local := writeLocal(t, "src.bin", content)
	remote := tempPath(t, "dst.bin")

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    sftpPath(remote),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(content))
	}
	requireBytes(t, "uploaded file", readFile(t, remote), content)
}

func TestDownloadCopiesBytes(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("download me, verbatim\n")
	remote := writeLocal(t, "src.bin", content)
	local := tempPath(t, "out.bin")

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Download,
		Local:     local,
		Remote:    sftpPath(remote),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(content))
	}
	requireBytes(t, "downloaded file", readFile(t, local), content)
}

func TestUploadTransfersMultipleChunks(t *testing.T) {
	env := newTestEnv(t)
	// One byte past a default chunk boundary, so the second chunk is short.
	content := pattern(DefaultChunk + 4096)
	local := writeLocal(t, "src.bin", content)
	remote := tempPath(t, "dst.bin")

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    sftpPath(remote),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(content))
	}
	requireBytes(t, "chunked upload", readFile(t, remote), content)
}

func TestUploadTruncatesAStaleLargerDestination(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("short")
	local := writeLocal(t, "src.bin", content)
	// A previous, larger file at the destination: a chunked overwrite that
	// does not truncate would leave its tail behind.
	remote := writeLocal(t, "dst.bin", pattern(4096))
	remotePath := sftpPath(remote)

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    remotePath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(content))
	}
	got := readFile(t, remote)
	if len(got) != len(content) {
		t.Fatalf("destination is %d bytes, want %d: stale tail was not truncated", len(got), len(content))
	}
	requireBytes(t, "overwritten destination", got, content)
}

func TestParallelTransferIsByteIdentical(t *testing.T) {
	env := newTestEnv(t)
	content := pattern(300 * 1024)
	local := writeLocal(t, "src.bin", content)
	remote := tempPath(t, "dst.bin")

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    sftpPath(remote),
		Chunk:     4096, // 75 chunks across 4 workers
		Parallel:  4,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(content))
	}
	requireBytes(t, "parallel upload", readFile(t, remote), content)
}

func TestUploadIntoDirectoryKeepsBasename(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("cp semantics\n")
	local := writeLocal(t, "report.txt", content)
	remoteDir := t.TempDir()

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    sftpPath(remoteDir),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(content))
	}
	requireBytes(t, "upload into directory",
		readFile(t, filepath.Join(remoteDir, "report.txt")), content)
}

func TestUploadEmptyFileCreatesDestination(t *testing.T) {
	env := newTestEnv(t)
	local := writeLocal(t, "empty.bin", nil)
	remote := tempPath(t, "dst.bin")

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    sftpPath(remote),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0", res.Bytes)
	}
	fi, err := os.Stat(remote)
	if err != nil {
		t.Fatalf("stat destination: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("destination size = %d, want 0", fi.Size())
	}
}

// ---------------------------------------------------------------------------
// 6-7. md5 verification
// ---------------------------------------------------------------------------

func TestVerifySetsVerified(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("content that verifies\n")
	local := writeLocal(t, "src.bin", content)
	remote := tempPath(t, "dst.bin")
	remotePath := sftpPath(remote)

	digest := md5hex(content)
	term := &fakeTerminal{output: digest + "  " + remotePath + "\n"}
	env.engine.withTerminal(term)

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    remotePath,
		Verify:    true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Verified {
		t.Error("Verified = false, want true")
	}
	if res.MD5Local != digest {
		t.Errorf("MD5Local = %q, want %q", res.MD5Local, digest)
	}
	if res.MD5Remote != digest {
		t.Errorf("MD5Remote = %q, want %q", res.MD5Remote, digest)
	}
	if cmd := term.lastCommand(); !strings.HasPrefix(cmd, "md5sum ") || !strings.Contains(cmd, shellQuote(remotePath)) {
		t.Errorf("remote md5 command = %q, want md5sum of %s", cmd, remotePath)
	}
	requireBytes(t, "verified upload", readFile(t, remote), content)
}

func TestDownloadVerifySetsVerified(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("downloaded content that verifies\n")
	remote := writeLocal(t, "src.bin", content)
	remotePath := sftpPath(remote)
	local := tempPath(t, "out.bin")

	digest := md5hex(content)
	env.engine.withTerminal(&fakeTerminal{output: digest + "  " + remotePath + "\n"})

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Download,
		Local:     local,
		Remote:    remotePath,
		Verify:    true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Verified {
		t.Error("Verified = false, want true")
	}
	if res.MD5Local != digest || res.MD5Remote != digest {
		t.Errorf("digests = (%q, %q), want (%q, %q)",
			res.MD5Local, res.MD5Remote, digest, digest)
	}
	requireBytes(t, "verified download", readFile(t, local), content)
}

func TestVerifyMismatchReturnsErrVerifyFailed(t *testing.T) {
	env := newTestEnv(t)
	content := []byte("content that does NOT match\n")
	local := writeLocal(t, "src.bin", content)
	remote := tempPath(t, "dst.bin")

	wantLocal := md5hex(content)
	remoteDigest := md5hex([]byte("something else entirely"))
	env.engine.withTerminal(&fakeTerminal{output: remoteDigest + "  " + sftpPath(remote)})

	res, err := env.engine.Run(context.Background(), Task{
		Direction: Upload,
		Local:     local,
		Remote:    sftpPath(remote),
		Verify:    true,
	})
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("err = %v, want ErrVerifyFailed", err)
	}
	if res.Verified {
		t.Error("Verified = true, want false on a mismatch")
	}
	// Both digests must survive the failure so a caller can report them.
	if res.MD5Local != wantLocal {
		t.Errorf("MD5Local = %q, want %q", res.MD5Local, wantLocal)
	}
	if res.MD5Remote != remoteDigest {
		t.Errorf("MD5Remote = %q, want %q", res.MD5Remote, remoteDigest)
	}
	if !strings.Contains(err.Error(), wantLocal) || !strings.Contains(err.Error(), remoteDigest) {
		t.Errorf("error %q does not name both digests", err)
	}
}

// ---------------------------------------------------------------------------
// 8. cancellation
// ---------------------------------------------------------------------------

// blockingFS wraps a real remoteFS and parks the first read until Close,
// so cancellation can be observed deterministically without a sleep.
type blockingFS struct {
	remoteFS
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *blockingFS) Open(p string) (remoteFile, error) {
	f, err := b.remoteFS.Open(p)
	if err != nil {
		return nil, err
	}
	return &blockingFile{remoteFile: f, fs: b}, nil
}

func (b *blockingFS) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.remoteFS.Close()
}

type blockingFile struct {
	remoteFile
	fs *blockingFS
}

func (f *blockingFile) ReadAt([]byte, int64) (int, error) {
	select {
	case f.fs.started <- struct{}{}:
	default:
	}
	<-f.fs.closed
	return 0, errors.New("xfer test: read aborted by teardown")
}

func TestRunWithCanceledContextDoesNotDial(t *testing.T) {
	env := newTestEnv(t)
	dialed := false
	env.engine.withOpen(func(ctx context.Context, e *Engine, asset assets.Info) (remoteFS, error) {
		dialed = true
		return nil, errors.New("should not dial")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := env.engine.Run(ctx, Task{
		Direction: Upload,
		Local:     writeLocal(t, "src.bin", []byte("x")),
		Remote:    sftpPath(tempPath(t, "dst.bin")),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if dialed {
		t.Error("engine dialed despite an already-cancelled context")
	}
}

func TestCanceledContextAbortsTransfer(t *testing.T) {
	env := newTestEnv(t)
	content := pattern(4096)
	src := writeLocal(t, "src.bin", content)
	localDst := tempPath(t, "out.bin")

	blocked := &blockingFS{
		started: make(chan struct{}, 1),
		closed:  make(chan struct{}),
	}
	env.engine.withOpen(func(ctx context.Context, e *Engine, asset assets.Info) (remoteFS, error) {
		fs, err := dialKoKoSFTP(ctx, e.Session, asset)
		if err != nil {
			return nil, err
		}
		blocked.remoteFS = fs
		return blocked, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := env.engine.Run(ctx, Task{
			Direction: Download,
			Local:     localDst,
			Remote:    sftpPath(src),
		})
		done <- outcome{res, err}
	}()

	select {
	case <-blocked.started:
	case <-time.After(10 * time.Second):
		t.Fatal("transfer never began reading the remote file")
	}
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// ---------------------------------------------------------------------------
// 9. RemoteHasher
// ---------------------------------------------------------------------------

func TestRemoteHasherMD5ParsesOutput(t *testing.T) {
	const digest = "d41d8cd98f00b204e9800998ecf8427e"
	cases := []struct {
		name   string
		output string
		want   string
	}{
		{"hash and path", digest + "  /tmp/report.txt\n", digest},
		{"bare hash", digest + "\n", digest},
		{"upper case is normalized", strings.ToUpper(digest) + "  /tmp/report.txt\n", digest},
		{"noise before the digest", "warning: something\n" + digest + "  /tmp/report.txt\n", digest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			term := &fakeTerminal{output: tc.output}
			e := &Engine{}
			e.withTerminal(term)

			got, err := (&RemoteHasher{Engine: e}).MD5(context.Background(), "/tmp/report.txt")
			if err != nil {
				t.Fatalf("MD5: %v", err)
			}
			if got != tc.want {
				t.Errorf("MD5 = %q, want %q", got, tc.want)
			}
			if cmd := term.lastCommand(); !strings.Contains(cmd, shellQuote("/tmp/report.txt")) {
				t.Errorf("command = %q, want the quoted remote path", cmd)
			}
		})
	}
}

func TestRemoteHasherMD5RejectsDigestlessOutput(t *testing.T) {
	e := &Engine{}
	e.withTerminal(&fakeTerminal{output: "md5sum: /tmp/missing: No such file or directory\n"})

	got, err := (&RemoteHasher{Engine: e}).MD5(context.Background(), "/tmp/missing")
	if err == nil {
		t.Fatal("err = nil, want a failure for digestless output")
	}
	if got != "" {
		t.Errorf("digest = %q, want empty on failure", got)
	}
	if !strings.Contains(err.Error(), "/tmp/missing") {
		t.Errorf("error %q does not name the path", err)
	}
}

func TestRemoteHasherNeedsEngine(t *testing.T) {
	if _, err := (&RemoteHasher{}).MD5(context.Background(), "/tmp/x"); err == nil {
		t.Fatal("err = nil, want a failure for a hasher without an engine")
	}
}

// ---------------------------------------------------------------------------
// 10. relay
// ---------------------------------------------------------------------------

func TestRelayCopiesBetweenEngines(t *testing.T) {
	srcEnv := newTestEnv(t)
	dstEnv := secondEnv(t)

	content := pattern(200 * 1024)
	srcPath := sftpPath(writeLocal(t, "src.bin", content))
	dstFile := tempPath(t, "relayed.bin")

	res, err := Relay(context.Background(), srcEnv.engine, dstEnv.engine,
		srcPath, sftpPath(dstFile), Task{Chunk: 4096, Parallel: 4})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if res.Bytes != int64(len(content)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(content))
	}
	requireBytes(t, "relayed file", readFile(t, dstFile), content)

	// Both assets were resolved through their own token endpoint.
	if got := len(srcEnv.tokens.recorded()); got == 0 {
		t.Error("relay source never requested a connection token")
	}
	if got := len(dstEnv.tokens.recorded()); got == 0 {
		t.Error("relay destination never requested a connection token")
	}
}

func TestRelayVerifyUsesBothRemoteDigests(t *testing.T) {
	srcEnv := newTestEnv(t)
	dstEnv := secondEnv(t)

	content := []byte("relay verify payload\n")
	srcPath := sftpPath(writeLocal(t, "src.bin", content))
	dstFile := tempPath(t, "relayed.bin")

	digest := md5hex(content)
	srcTerm := &fakeTerminal{output: digest + "  " + srcPath}
	dstTerm := &fakeTerminal{output: digest + "  " + sftpPath(dstFile)}
	srcEnv.engine.withTerminal(srcTerm)
	dstEnv.engine.withTerminal(dstTerm)

	res, err := Relay(context.Background(), srcEnv.engine, dstEnv.engine,
		srcPath, sftpPath(dstFile), Task{Verify: true})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if !res.Verified {
		t.Error("Verified = false, want true")
	}
	if res.MD5Local != digest || res.MD5Remote != digest {
		t.Errorf("digests = (%q, %q), want (%q, %q)",
			res.MD5Local, res.MD5Remote, digest, digest)
	}
}

// ---------------------------------------------------------------------------
// 11-13. the rsync stdio bridge
// ---------------------------------------------------------------------------

// fakeBridgeExec is an injected bridgeExec. It relays stdin to stdout the
// way a real exec channel would, then appends the protocol bytes.
type fakeBridgeExec struct {
	protocol []byte
	exitCode int
	runErr   error
	relay    bool
	mu       sync.Mutex
	closed   int
}

func (f *fakeBridgeExec) Run(stdin io.Reader, stdout, _ io.Writer) (int, error) {
	if f.runErr != nil {
		return 0, f.runErr
	}
	if f.relay && stdin != nil {
		if _, err := io.Copy(stdout, stdin); err != nil {
			return 0, err
		}
	}
	if len(f.protocol) > 0 {
		if _, err := stdout.Write(f.protocol); err != nil {
			return 0, err
		}
	}
	return f.exitCode, nil
}

func (f *fakeBridgeExec) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeBridgeExec) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// bridgeArgs is the classic rsync form: -l <asset> <server> <command...>.
var bridgeArgs = []string{"-l", "web-01", "prod", "rsync", "--server", "."}

// fixedBridge returns an openBridge seam that records the request.
func fixedBridge(exec bridgeExec, err error, seen *bridgeRequest) func(context.Context, bridgeRequest) (bridgeExec, error) {
	return func(_ context.Context, req bridgeRequest) (bridgeExec, error) {
		if seen != nil {
			*seen = req
		}
		return exec, err
	}
}

func TestRunBridgeRelaysStdoutAndKeepsItClean(t *testing.T) {
	exec := &fakeBridgeExec{
		protocol: []byte("rsync\x00protocol"),
		relay:    true,
	}
	const stdinPayload = "payload from rsync"
	var req bridgeRequest
	var stdout, stderr bytes.Buffer

	code, err := RunBridge(context.Background(), bridgeArgs, Bridge{
		Stdin:      strings.NewReader(stdinPayload),
		Stdout:     &stdout,
		Stderr:     &stderr,
		openBridge: fixedBridge(exec, nil, &req),
	})
	if err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if req.Asset != "web-01" || req.Server != "prod" || req.Command != "rsync --server ." {
		t.Errorf("request = %+v, want asset web-01 on prod running %q", req, "rsync --server .")
	}
	if want := stdinPayload + string(exec.protocol); stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	// stdout is rsync's data channel: one diagnostic line there corrupts
	// the transfer.
	if strings.Contains(stdout.String(), "ssh-pipe") {
		t.Errorf("diagnostic text leaked to stdout: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on success", stderr.String())
	}
	if exec.closeCount() != 1 {
		t.Errorf("exec closed %d times, want 1", exec.closeCount())
	}
}

func TestRunBridgeReturnsRemoteExitCode(t *testing.T) {
	exec := &fakeBridgeExec{protocol: []byte("data"), exitCode: 7}
	var stdout, stderr bytes.Buffer

	code, err := RunBridge(context.Background(), bridgeArgs, Bridge{
		Stdout:     &stdout,
		Stderr:     &stderr,
		openBridge: fixedBridge(exec, nil, nil),
	})
	if err != nil {
		t.Fatalf("RunBridge: %v", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
	// A non-zero remote exit is the relay's normal outcome, not a
	// diagnostic.
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty for a non-zero exit code", stderr.String())
	}
	if stdout.String() != "data" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "data")
	}
}

func TestRunBridgeReportsDiagnosticsOnStderr(t *testing.T) {
	cases := []struct {
		name string
		args []string
		exec bridgeExec
		err  error
		want string
	}{
		{
			name: "bad arguments",
			args: []string{},
			want: "expected",
		},
		{
			name: "bridge cannot open",
			args: bridgeArgs,
			err:  errors.New("asset not found"),
			want: "asset not found",
		},
		{
			name: "remote command fails",
			args: bridgeArgs,
			exec: &fakeBridgeExec{runErr: errors.New("channel closed")},
			want: "channel closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := RunBridge(context.Background(), tc.args, Bridge{
				Stdout:     &stdout,
				Stderr:     &stderr,
				openBridge: fixedBridge(tc.exec, tc.err, nil),
			})
			if err == nil {
				t.Fatal("err = nil, want a failure")
			}
			if code == 0 {
				t.Errorf("code = 0, want non-zero")
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty: diagnostics must not touch stdout", stdout.String())
			}
			if !strings.Contains(stderr.String(), "jms ssh-pipe:") {
				t.Errorf("stderr = %q, want a jms ssh-pipe diagnostic", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestParseBridgeArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bridgeRequest
	}{
		{
			name: "classic rsync",
			args: []string{"-l", "web-01", "prod", "rsync", "--server", "-v"},
			want: bridgeRequest{Asset: "web-01", Server: "prod", Command: "rsync --server -v"},
		},
		{
			name: "openrsync",
			args: []string{"web-01@prod", "rsync", "--server", "."},
			want: bridgeRequest{Asset: "web-01", Server: "prod", Command: "rsync --server ."},
		},
		{
			name: "config flag",
			args: []string{"--config", "/etc/jms.toml", "-l", "web-01", "prod", "rsync", "."},
			want: bridgeRequest{Asset: "web-01", Server: "prod", Command: "rsync .", ConfigPath: "/etc/jms.toml"},
		},
		{
			name: "config flag with equals",
			args: []string{"--config=/etc/jms.toml", "web-01@prod", "rsync", "."},
			want: bridgeRequest{Asset: "web-01", Server: "prod", Command: "rsync .", ConfigPath: "/etc/jms.toml"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBridgeArgs(tc.args)
			if err != nil {
				t.Fatalf("parseBridgeArgs(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseBridgeArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseBridgeArgsRejectsIncompleteInvocations(t *testing.T) {
	cases := [][]string{
		{},
		{"-l"},
		{"-l", "web-01"},         // no server, no command
		{"-l", "web-01", "prod"}, // no remote command
		{"web-01@prod"},          // no remote command
		{"--config"},             // flag without a value
		{"--bogus", "-l", "web-01", "prod", "rsync"}, // unknown option
	}
	for _, args := range cases {
		if _, err := parseBridgeArgs(args); err == nil {
			t.Errorf("parseBridgeArgs(%q) = nil error, want a failure", args)
		}
	}
}

// ---------------------------------------------------------------------------
// 14. under-configured engines
// ---------------------------------------------------------------------------

func TestEngineValidateRejectsUnderConfigured(t *testing.T) {
	var nilEngine *Engine
	if err := nilEngine.validate(); err == nil {
		t.Error("nil engine validated, want an error")
	}

	noSession := &Engine{}
	if err := noSession.validate(); err == nil {
		t.Error("engine without a session validated, want an error")
	} else if !strings.Contains(err.Error(), "Session") {
		t.Errorf("error %q does not point at Engine.Session", err)
	}

	noClient := &Engine{Session: &auth.Session{}}
	if err := noClient.validate(); err == nil {
		t.Error("engine whose session has no client validated, want an error")
	}

	// Run must fail cleanly rather than dereference the missing session.
	if _, err := noSession.Run(context.Background(), Task{
		Direction: Upload, Local: "a", Remote: "b",
	}); err == nil {
		t.Error("Run on an under-configured engine returned no error")
	}
}

func TestRunRejectsUnknownDirection(t *testing.T) {
	env := newTestEnv(t)
	local := writeLocal(t, "src.bin", []byte("x"))

	_, err := env.engine.Run(context.Background(), Task{
		Direction: Direction("sideways"),
		Local:     local,
		Remote:    sftpPath(tempPath(t, "dst.bin")),
	})
	if err == nil {
		t.Fatal("Run = nil error, want a failure for an unknown direction")
	}
	if !strings.Contains(err.Error(), "sideways") {
		t.Errorf("error %q does not name the bad direction", err)
	}
}

func TestRunWithoutAssetFailsBeforeDialing(t *testing.T) {
	// A session whose BaseURL points nowhere: the test proves the engine
	// stops at asset resolution instead of attempting a connection. The
	// URL is never dialed because no asset name is available to resolve.
	sess := &auth.Session{Client: api.New("http://127.0.0.1:1"), BaseURL: "http://127.0.0.1:1"}
	e := &Engine{Session: sess}

	_, err := e.Run(context.Background(), Task{
		Direction: Upload, Local: "a", Remote: "b",
	})
	if err == nil {
		t.Fatal("Run = nil error, want a failure when no asset is resolved")
	}
	if !strings.Contains(err.Error(), "asset") {
		t.Errorf("error %q does not explain the missing asset", err)
	}
}
