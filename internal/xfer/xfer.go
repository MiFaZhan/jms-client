// Package xfer implements file transfer: the SFTP engine, md5 verification,
// relay between two assets, and the stdio bridge rsync uses via
// `rsync -e`.
//
// SFTP rides on its own connection token. The token must be created with
// protocol="sftp" and connect_method="web_sftp": an ssh/web_cli token's SFTP
// subsystem lands on the KoKo virtual root and every path fails with
// "please select one of the assets" (DESIGN.md §3.1).
package xfer

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/endpoint"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// Defaults.
const (
	// DefaultChunk is the transfer chunk size.
	DefaultChunk = 1 << 20
	// DefaultParallel is the number of concurrent chunk streams.
	DefaultParallel = 4
)

// sftpProtocol and sftpConnectMethod are the token pair SFTP requires
// (DESIGN.md §3.1). They are constants rather than literals at the call
// site because getting the pair wrong is the single failure mode this
// package exists to avoid.
const (
	sftpProtocol      = "sftp"
	sftpConnectMethod = "web_sftp"
)

// copyBuffer is the per-read size inside one chunk. The Python
// implementation raised paramiko's 8 KiB default to 64 KiB; the same value
// keeps the SFTP request count low without large allocations per worker.
const copyBuffer = 64 * 1024

// md5Timeout bounds one remote md5sum. Multi-GB files take minutes, so the
// ceiling is generous; it exists only so a hung remote cannot block a
// transfer forever.
const md5Timeout = 30 * time.Minute

// Direction is which way a transfer moves.
type Direction string

// Directions.
const (
	Upload   Direction = "upload"
	Download Direction = "download"
)

// Task is one file transfer.
type Task struct {
	Direction Direction
	// Local is the local path; for a relay both sides are remote.
	Local string
	// Remote is the path inside the asset.
	Remote string
	// Verify asks for an md5 comparison after the transfer.
	Verify bool
	// Chunk and Parallel override the defaults when non-zero.
	Chunk    int
	Parallel int
}

// Result is the outcome of one Task.
type Result struct {
	Bytes    int64
	Duration time.Duration
	// MD5Local and MD5Remote are set when Verify was requested. For a
	// relay both sides are remote: MD5Local is the source digest and
	// MD5Remote the destination digest.
	MD5Local  string
	MD5Remote string
	Verified  bool
}

// ErrVerifyFailed reports an md5 mismatch.
var ErrVerifyFailed = errors.New("md5 verification failed")

// Engine performs transfers against one asset.
type Engine struct {
	Session *auth.Session
	Asset   assets.Info
	// Cols and Rows size any PTY the remote side needs.
	Cols, Rows int

	// AssetName and Account let a caller that has not resolved the asset
	// yet run a transfer anyway: Run resolves it on demand. They are
	// consulted only when Asset.ID is empty, so a caller holding a
	// resolved asset (the usual case) can leave them unset.
	AssetName string
	Account   string
	// Protocol is the asset protocol to resolve with; empty means "ssh",
	// because SFTP rides on the asset's ssh protocol even though its
	// connection token uses protocol="sftp".
	Protocol string

	// open is the SFTP dial seam. Nil means dialKoKoSFTP; tests inject a
	// fake filesystem here.
	open func(ctx context.Context, e *Engine, asset assets.Info) (remoteFS, error)
	// terminal is the SSH-exec seam RemoteHasher uses. Nil means the real
	// transport.
	terminal func(ctx context.Context, e *Engine) (transport.Terminal, error)
}

// Run performs one task.
//
// Destination directories are expanded like `cp`: when the destination is
// an existing directory the source basename is appended.
func (e *Engine) Run(ctx context.Context, t Task) (Result, error) {
	start := time.Now()
	if err := e.validate(); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	switch t.Direction {
	case Upload, Download:
	default:
		return Result{}, fmt.Errorf("xfer: unknown direction %q", t.Direction)
	}

	asset, err := e.assetInfo(ctx)
	if err != nil {
		return Result{}, err
	}
	conn, err := e.dialSFTP(ctx, asset)
	if err != nil {
		return Result{}, err
	}
	// Cancelling ctx tears the connection down, which is what unblocks a
	// worker parked in a read or write on a stalled link.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()

	remote := remoteSide{fs: conn}
	var (
		src, dst         side
		srcPath, dstPath string
		localPath        string
		remotePath       string
	)
	if t.Direction == Upload {
		src, srcPath = localSide{}, t.Local
		dst, dstPath = remote, t.Remote
		localPath = t.Local
	} else {
		src, srcPath = remote, t.Remote
		dst, dstPath = localSide{}, t.Local
		remotePath = t.Remote
	}

	res := Result{}
	dstPath, err = expandDestination(dst, srcPath, dstPath)
	if err != nil {
		return res, err
	}
	// Verify the file that was actually written, not the directory the
	// caller named: expandDestination may have appended the basename.
	if t.Direction == Upload {
		remotePath = dstPath
	} else {
		localPath = dstPath
	}
	size, err := src.stat(srcPath)
	if err != nil {
		return res, fmt.Errorf("xfer: source %s: %w", srcPath, err)
	}

	bytes, err := transfer(ctx, src, srcPath, dst, dstPath, size, chunkSize(t), parallelWidth(t))
	res.Bytes = bytes
	res.Duration = time.Since(start)
	if err != nil {
		return res, ctxOr(err, ctx)
	}

	if !t.Verify {
		return res, nil
	}
	res.MD5Local, res.MD5Remote, err = digests(ctx, e, localPath, remotePath)
	res.Duration = time.Since(start)
	if err != nil {
		return res, err
	}
	if res.MD5Local == "" || res.MD5Local != res.MD5Remote {
		return res, fmt.Errorf("%w: local %s, remote %s",
			ErrVerifyFailed, digestOrUnknown(res.MD5Local), digestOrUnknown(res.MD5Remote))
	}
	res.Verified = true
	return res, nil
}

// Relay copies between two assets, streaming through this process.
//
// remoteSrc and remoteDst are paths inside src.Asset and dst.Asset
// respectively; neither side is local. t.Chunk, t.Parallel and t.Verify
// apply; t.Direction and t.Local are ignored.
func Relay(ctx context.Context, src, dst *Engine, remoteSrc, remoteDst string, t Task) (Result, error) {
	start := time.Now()
	if err := src.validate(); err != nil {
		return Result{}, fmt.Errorf("xfer: relay source: %w", err)
	}
	if err := dst.validate(); err != nil {
		return Result{}, fmt.Errorf("xfer: relay destination: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	srcAsset, err := src.assetInfo(ctx)
	if err != nil {
		return Result{}, err
	}
	dstAsset, err := dst.assetInfo(ctx)
	if err != nil {
		return Result{}, err
	}
	srcConn, err := src.dialSFTP(ctx, srcAsset)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = srcConn.Close() }()
	dstConn, err := dst.dialSFTP(ctx, dstAsset)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = dstConn.Close() }()

	stop := context.AfterFunc(ctx, func() {
		_ = srcConn.Close()
		_ = dstConn.Close()
	})
	defer stop()

	srcSide, dstSide := remoteSide{fs: srcConn}, remoteSide{fs: dstConn}
	res := Result{}
	dstPath, err := expandDestination(dstSide, remoteSrc, remoteDst)
	if err != nil {
		return res, err
	}
	size, err := srcSide.stat(remoteSrc)
	if err != nil {
		return res, fmt.Errorf("xfer: relay source %s: %w", remoteSrc, err)
	}

	bytes, err := transfer(ctx, srcSide, remoteSrc, dstSide, dstPath, size,
		chunkSize(t), parallelWidth(t))
	res.Bytes = bytes
	res.Duration = time.Since(start)
	if err != nil {
		return res, ctxOr(err, ctx)
	}

	if !t.Verify {
		return res, nil
	}
	srcDigest, err := (&RemoteHasher{Engine: src}).MD5(ctx, remoteSrc)
	if err != nil {
		return res, err
	}
	dstDigest, err := (&RemoteHasher{Engine: dst}).MD5(ctx, dstPath)
	res.MD5Local, res.MD5Remote = srcDigest, dstDigest
	res.Duration = time.Since(start)
	if err != nil {
		return res, err
	}
	if srcDigest == "" || srcDigest != dstDigest {
		return res, fmt.Errorf("%w: source %s, destination %s",
			ErrVerifyFailed, digestOrUnknown(srcDigest), digestOrUnknown(dstDigest))
	}
	res.Verified = true
	return res, nil
}

// RemoteHasher computes an md5 on the remote side through the terminal.
//
// The SFTP subsystem has no hashing primitive, so the digest is produced
// by running md5sum over an SSH exec channel, exactly as the Python
// implementation did. The terminal is opened per call and closed again:
// a transfer verifies once, and holding a second connection open across a
// multi-hour copy would only add an idle link for a middlebox to drop.
type RemoteHasher struct {
	Engine *Engine
}

// MD5 returns the remote file's md5.
func (h *RemoteHasher) MD5(ctx context.Context, path string) (string, error) {
	if h == nil || h.Engine == nil {
		return "", errors.New("xfer: RemoteHasher needs an Engine")
	}
	term, err := h.Engine.dialTerminal(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = term.Close() }()

	res, err := term.Execute(ctx, "md5sum "+shellQuote(path), md5Timeout)
	if err != nil {
		return "", fmt.Errorf("xfer: remote md5 of %s: %w", path, err)
	}
	digest := parseMD5(res.Output)
	if digest == "" {
		return "", fmt.Errorf("xfer: remote md5 of %s: no digest in %q",
			path, truncate(res.Output, 200))
	}
	return digest, nil
}

// validate reports whether the engine can do any work at all.
func (e *Engine) validate() error {
	if e == nil {
		return errors.New("xfer: nil engine")
	}
	if e.Session == nil || e.Session.Client == nil {
		return errors.New("xfer: engine has no session: set Engine.Session")
	}
	return nil
}

// assetInfo returns the resolved asset, resolving by name when the caller
// supplied only a session.
func (e *Engine) assetInfo(ctx context.Context) (assets.Info, error) {
	if e.Asset.ID != "" {
		return e.Asset, nil
	}
	name := strings.TrimSpace(e.AssetName)
	if name == "" {
		return assets.Info{}, errors.New(
			"xfer: no asset resolved: set Engine.Asset (assets.Info) or Engine.AssetName")
	}
	protocol := strings.TrimSpace(e.Protocol)
	if protocol == "" {
		protocol = "ssh"
	}
	info, err := assets.Resolve(ctx, e.Session.Client, name, e.Account, protocol)
	if err != nil {
		return assets.Info{}, fmt.Errorf("xfer: resolve asset %q: %w", name, err)
	}
	return info, nil
}

// dialSFTP opens the SFTP session for one asset.
func (e *Engine) dialSFTP(ctx context.Context, asset assets.Info) (remoteFS, error) {
	if e.open != nil {
		return e.open(ctx, e, asset)
	}
	return dialKoKoSFTP(ctx, e.Session, asset)
}

// dialTerminal opens the SSH exec terminal RemoteHasher runs md5sum on.
func (e *Engine) dialTerminal(ctx context.Context) (transport.Terminal, error) {
	if e.terminal != nil {
		return e.terminal(ctx, e)
	}
	asset, err := e.assetInfo(ctx)
	if err != nil {
		return nil, err
	}
	return transport.Connect(ctx, e.Session, asset, transport.ConnectOptions{
		Backend: transport.BackendSSH,
	})
}

// sftpToken creates the connection token SFTP requires.
//
// It exists as its own function so the protocol/connect_method pair has one
// definition and one test, rather than being repeated at every call site.
func sftpToken(ctx context.Context, sess *auth.Session, asset assets.Info) (transport.Token, error) {
	return transport.NewConnectionToken(ctx, sess, asset, sftpProtocol, sftpConnectMethod)
}

// dialKoKoSFTP opens an SFTP session over a KoKo connection token.
func dialKoKoSFTP(ctx context.Context, sess *auth.Session, asset assets.Info) (remoteFS, error) {
	token, err := sftpToken(ctx, sess, asset)
	if err != nil {
		return nil, fmt.Errorf("xfer: sftp connection token for %s: %w", asset.Name, err)
	}
	client, err := dialSSH(ctx, sess, token)
	if err != nil {
		return nil, err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("xfer: sftp subsystem for %s: %w", asset.Name, err)
	}
	return &koKoSFTP{client: sc, ssh: client}, nil
}

// dialSSH opens the KoKo SSH transport authenticated with a connection
// token: username JMS-{token_id}, password token_value (DESIGN.md §3.3).
//
// The host key is not pinned. KoKo presents an ephemeral key and the
// Python implementation did not verify one either, so pinning would be a
// behaviour change rather than a hardening. token.Value is used only as an
// authentication secret and never reaches an error, a log line or a
// returned value.
func dialSSH(ctx context.Context, sess *auth.Session, token transport.Token) (*ssh.Client, error) {
	host, err := sessionHost(sess)
	if err != nil {
		return nil, err
	}
	port := transport.DefaultSSHPort
	if sess.Server != nil {
		port = sess.Server.Port()
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	cfg := &ssh.ClientConfig{
		User:            "JMS-" + token.ID,
		Auth:            []ssh.AuthMethod{ssh.Password(token.Value)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // KoKo's key is ephemeral
		Timeout:         transport.DialTimeout,
	}
	dialer := net.Dialer{Timeout: transport.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("xfer: dial %s: %w", addr, err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("xfer: ssh handshake with %s: %w", addr, err)
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

// sessionHost extracts the host from the session's bound address.
//
// The endpoint is fixed for the session's lifetime (DESIGN.md §4.7), so
// every KoKo connection derives from this one address.
func sessionHost(sess *auth.Session) (string, error) {
	if sess == nil || strings.TrimSpace(sess.BaseURL) == "" {
		return "", errors.New("xfer: session has no base URL")
	}
	u, err := url.Parse(sess.BaseURL)
	if err != nil {
		return "", fmt.Errorf("xfer: session base URL %q: %w", sess.BaseURL, err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("xfer: session base URL %q has no host", sess.BaseURL)
	}
	return u.Hostname(), nil
}

// ---------------------------------------------------------------------------
// SFTP seam
// ---------------------------------------------------------------------------

// remoteFS is the slice of an SFTP connection the engine uses.
//
// The concrete implementation wraps *sftp.Client; tests substitute an
// in-memory filesystem so the engine can be driven without a server.
type remoteFS interface {
	Open(path string) (remoteFile, error)
	OpenFile(path string, flag int) (remoteFile, error)
	Stat(path string) (os.FileInfo, error)
	MkdirAll(path string) error
	Close() error
}

// remoteFile is one open SFTP file handle.
type remoteFile interface {
	io.Reader
	io.Writer
	io.ReaderAt
	io.WriterAt
	io.Seeker
	io.Closer
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
}

// koKoSFTP is one SFTP session over a KoKo connection token.
//
// Close is idempotent because both the ctx watchdog and the deferred
// cleanup can reach it.
type koKoSFTP struct {
	client *sftp.Client
	ssh    *ssh.Client
	once   sync.Once
	err    error
}

func (k *koKoSFTP) Open(p string) (remoteFile, error) {
	f, err := k.client.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (k *koKoSFTP) OpenFile(p string, flag int) (remoteFile, error) {
	f, err := k.client.OpenFile(p, flag)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (k *koKoSFTP) Stat(p string) (os.FileInfo, error) { return k.client.Stat(p) }

func (k *koKoSFTP) MkdirAll(p string) error { return k.client.MkdirAll(p) }

func (k *koKoSFTP) Close() error {
	k.once.Do(func() {
		k.err = k.client.Close()
		if cerr := k.ssh.Close(); cerr != nil && k.err == nil {
			k.err = cerr
		}
	})
	return k.err
}

// ---------------------------------------------------------------------------
// Transfer engine
// ---------------------------------------------------------------------------

// readerAtCloser is a readable, seekable-by-offset handle.
type readerAtCloser interface {
	io.ReaderAt
	io.Closer
}

// writerAtCloser is a writable, seekable-by-offset handle.
type writerAtCloser interface {
	io.WriterAt
	io.Closer
}

// side abstracts one end of a transfer: the local filesystem or one SFTP
// connection. Every handle is opened per worker, because neither an SFTP
// channel nor a plain file handle is documented as safe for concurrent
// offset I/O on the same object.
type side interface {
	openReader(path string) (readerAtCloser, error)
	openWriter(path string) (writerAtCloser, error)
	// prepare creates the parent directories and truncates path to zero
	// length, so a stale larger file cannot leave a tail behind.
	prepare(path string) error
	// truncate sets the final size.
	truncate(path string, size int64) error
	// stat returns the file size, rejecting a directory.
	stat(path string) (int64, error)
	// isDir reports whether path exists and is a directory.
	isDir(path string) (bool, error)
}

// localSide is the local filesystem.
type localSide struct{}

func (localSide) openReader(p string) (readerAtCloser, error) { return os.Open(p) }

func (localSide) openWriter(p string) (writerAtCloser, error) {
	return os.OpenFile(p, os.O_RDWR, 0)
}

func (localSide) prepare(p string) error {
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func (localSide) truncate(p string, size int64) error { return os.Truncate(p, size) }

func (localSide) stat(p string) (int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("%s is a directory", p)
	}
	return fi.Size(), nil
}

func (localSide) isDir(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

// remoteSide is one SFTP connection. Remote paths are POSIX paths even on
// a Windows client, so path.Dir is used rather than filepath.Dir.
type remoteSide struct{ fs remoteFS }

func (r remoteSide) openReader(p string) (readerAtCloser, error) {
	f, err := r.fs.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (r remoteSide) openWriter(p string) (writerAtCloser, error) {
	f, err := r.fs.OpenFile(p, os.O_RDWR)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (r remoteSide) prepare(p string) error {
	if dir := path.Dir(p); dir != "" && dir != "." && dir != "/" {
		if err := r.fs.MkdirAll(dir); err != nil {
			return err
		}
	}
	f, err := r.fs.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	return f.Close()
}

func (r remoteSide) truncate(p string, size int64) error {
	f, err := r.fs.OpenFile(p, os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Truncate(size)
}

func (r remoteSide) stat(p string) (int64, error) {
	fi, err := r.fs.Stat(p)
	if err != nil {
		return 0, err
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("%s is a directory", p)
	}
	return fi.Size(), nil
}

func (r remoteSide) isDir(p string) (bool, error) {
	fi, err := r.fs.Stat(p)
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

// expandDestination applies cp semantics: an existing destination
// directory receives the source basename.
//
// The separator is always "/", because a remote destination is a POSIX
// path even when the client runs on Windows.
func expandDestination(dst side, srcPath, dstPath string) (string, error) {
	if strings.TrimSpace(dstPath) == "" {
		return "", errors.New("xfer: empty destination path")
	}
	isDir, err := dst.isDir(dstPath)
	if err != nil || !isDir {
		// A missing or non-directory destination is a file path.
		return dstPath, nil
	}
	base := path.Base(strings.ReplaceAll(srcPath, `\`, "/"))
	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("xfer: cannot derive a file name from %q", srcPath)
	}
	return strings.TrimRight(dstPath, "/") + "/" + base, nil
}

// chunkSize and parallelWidth apply the task overrides.
func chunkSize(t Task) int64 {
	if t.Chunk > 0 {
		return int64(t.Chunk)
	}
	return DefaultChunk
}

func parallelWidth(t Task) int {
	if t.Parallel > 0 {
		return t.Parallel
	}
	return DefaultParallel
}

// chunkRanges splits size into [offset, length) pairs of at most chunk
// bytes each.
func chunkRanges(size, chunk int64) [][2]int64 {
	if chunk <= 0 {
		chunk = DefaultChunk
	}
	ranges := make([][2]int64, 0, size/chunk+1)
	for off := int64(0); off < size; off += chunk {
		length := chunk
		if off+length > size {
			length = size - off
		}
		ranges = append(ranges, [2]int64{off, length})
	}
	return ranges
}

// transfer copies size bytes from srcPath to dstPath in chunks.
//
// It returns the number of bytes written, which equals size on success.
func transfer(ctx context.Context, src side, srcPath string, dst side, dstPath string,
	size, chunk int64, parallel int) (int64, error) {

	if err := dst.prepare(dstPath); err != nil {
		return 0, fmt.Errorf("xfer: prepare %s: %w", dstPath, err)
	}
	if size <= 0 {
		return 0, nil
	}

	ranges := chunkRanges(size, chunk)
	if parallel > len(ranges) {
		parallel = len(ranges)
	}
	if parallel < 1 {
		parallel = 1
	}

	work := make(chan [2]int64)
	abort := make(chan struct{})
	var (
		wg       sync.WaitGroup
		written  atomic.Int64
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			close(abort)
		})
	}

	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := copyWorker(ctx, src, srcPath, dst, dstPath, work, abort, &written); err != nil {
				fail(err)
			}
		}()
	}

feed:
	for _, r := range ranges {
		select {
		case work <- r:
		case <-abort:
			break feed
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()

	if firstErr != nil {
		return written.Load(), firstErr
	}
	if err := ctx.Err(); err != nil {
		return written.Load(), err
	}
	if err := dst.truncate(dstPath, size); err != nil {
		return written.Load(), fmt.Errorf("xfer: finalize %s: %w", dstPath, err)
	}
	return written.Load(), nil
}

// copyWorker consumes chunk ranges, opening one handle pair per worker.
func copyWorker(ctx context.Context, src side, srcPath string, dst side, dstPath string,
	work <-chan [2]int64, abort <-chan struct{}, written *atomic.Int64) error {

	var (
		srcF readerAtCloser
		dstF writerAtCloser
	)
	defer func() {
		if srcF != nil {
			_ = srcF.Close()
		}
		if dstF != nil {
			_ = dstF.Close()
		}
	}()

	buf := make([]byte, copyBuffer)
	for r := range work {
		select {
		case <-abort:
			return nil
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if srcF == nil {
			var err error
			if srcF, err = src.openReader(srcPath); err != nil {
				return fmt.Errorf("xfer: open source %s: %w", srcPath, err)
			}
			if dstF, err = dst.openWriter(dstPath); err != nil {
				return fmt.Errorf("xfer: open destination %s: %w", dstPath, err)
			}
		}
		if err := copyRange(srcF, dstF, r[0], r[1], buf); err != nil {
			return fmt.Errorf("xfer: copy %s[%d:%d]: %w", dstPath, r[0], r[0]+r[1], err)
		}
		written.Add(r[1])
	}
	return nil
}

// copyRange copies exactly length bytes at offset off.
func copyRange(src io.ReaderAt, dst io.WriterAt, off, length int64, buf []byte) error {
	for length > 0 {
		want := int64(len(buf))
		if want > length {
			want = length
		}
		n, err := src.ReadAt(buf[:want], off)
		if n > 0 {
			if werr := writeAllAt(dst, buf[:n], off); werr != nil {
				return werr
			}
			off += int64(n)
			length -= int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && length == 0 {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

// writeAllAt writes all of p, honouring a short write.
func writeAllAt(w io.WriterAt, p []byte, off int64) error {
	for len(p) > 0 {
		n, err := w.WriteAt(p, off)
		if n > 0 {
			off += int64(n)
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// digests computes the local and remote md5 for one transferred file.
func digests(ctx context.Context, e *Engine, localPath, remotePath string) (string, string, error) {
	local, err := localMD5(localPath)
	if err != nil {
		return "", "", fmt.Errorf("xfer: local md5 of %s: %w", localPath, err)
	}
	remote, err := (&RemoteHasher{Engine: e}).MD5(ctx, remotePath)
	if err != nil {
		return "", "", err
	}
	return local, remote, nil
}

// localMD5 hashes a local file in 4 MiB reads.
func localMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ctxOr prefers the context error when the context ended, so a cancelled
// transfer reports context.Canceled rather than the incidental socket
// error closing the connection produced.
func ctxOr(err error, ctx context.Context) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// digestOrUnknown keeps an empty digest visible in a mismatch message
// instead of printing nothing.
func digestOrUnknown(digest string) string {
	if digest == "" {
		return "(none)"
	}
	return digest
}

// parseMD5 extracts the first 32-hex-digit digest from md5sum output.
//
// md5sum prints "<hash>  <path>", but busybox builds and some wrappers
// print the bare hash, so both shapes are accepted.
func parseMD5(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		head := strings.ToLower(fields[0])
		if isHex32(head) {
			return head
		}
	}
	return ""
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// shellQuote wraps s in single quotes for a POSIX shell, escaping embedded
// single quotes. Remote assets are Unix hosts, so POSIX quoting is the
// right dialect even when the client runs on Windows.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// truncate shortens s for an error message.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// rsync stdio bridge
// ---------------------------------------------------------------------------

// Bridge is the stdio relay behind `rsync -e`: it speaks the SSH protocol
// on stdin/stdout and forwards the remote command to the asset.
type Bridge struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// openBridge is the transport seam. It is unexported so that only this
	// package's tests can replace it: a caller outside the package always
	// reaches a real asset. Nil means openKoKoBridge.
	openBridge func(ctx context.Context, req bridgeRequest) (bridgeExec, error)
}

// bridgeRequest is a parsed ssh-pipe invocation.
type bridgeRequest struct {
	Asset      string
	Server     string
	Command    string
	ConfigPath string
}

// bridgeExec is the remote side of the stdio relay.
type bridgeExec interface {
	// Run wires the caller's streams to the remote command and returns
	// its exit code. A non-zero remote exit is a successful relay: it is
	// returned as the code with a nil error, because rsync reads the code
	// rather than a diagnostic. Only a transport failure is an error.
	Run(stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Close() error
}

// RunBridge parses the ssh-style arguments, opens the asset, and bridges
// stdio until the remote command exits. It returns the remote exit code.
//
// Two argument shapes are accepted, matching what rsync produces:
//
//	classic rsync: jms ssh-pipe -l <asset> <server> <remote command...>
//	openrsync:     jms ssh-pipe <asset>@<server> <remote command...>
//
// Nothing but the caller's protocol bytes may reach Stdout: stdout is
// rsync's data channel, so a single diagnostic line there corrupts the
// transfer. Every diagnostic goes to Stderr.
func RunBridge(ctx context.Context, args []string, b Bridge) (int, error) {
	if b.Stdout == nil {
		b.Stdout = io.Discard
	}
	if b.Stderr == nil {
		b.Stderr = io.Discard
	}
	if b.Stdin == nil {
		b.Stdin = strings.NewReader("")
	}

	req, err := parseBridgeArgs(args)
	if err != nil {
		return bridgeFail(b.Stderr, err)
	}

	open := b.openBridge
	if open == nil {
		open = openKoKoBridge
	}
	exec, err := open(ctx, req)
	if err != nil {
		return bridgeFail(b.Stderr, err)
	}
	defer func() { _ = exec.Close() }()

	code, err := exec.Run(b.Stdin, b.Stdout, b.Stderr)
	if err != nil {
		return bridgeFail(b.Stderr, err)
	}
	return code, nil
}

// bridgeFail reports a bridge failure on stderr and returns a non-zero
// exit code.
func bridgeFail(stderr io.Writer, err error) (int, error) {
	fmt.Fprintf(stderr, "jms ssh-pipe: %s\n", err)
	return 1, err
}

// parseBridgeArgs parses the ssh-style argv rsync builds.
func parseBridgeArgs(args []string) (bridgeRequest, error) {
	var (
		req   bridgeRequest
		asset string
		srv   string
		rest  []string
	)

	for i := 0; i < len(args); {
		switch a := args[i]; {
		case a == "-l":
			if i+1 >= len(args) {
				return req, errors.New("-l needs an asset name")
			}
			asset = args[i+1]
			i += 2
			if i < len(args) {
				srv = args[i]
				i++
			}
			rest = args[i:]
			i = len(args)
		case a == "--config":
			if i+1 >= len(args) {
				return req, errors.New("--config needs a path")
			}
			req.ConfigPath = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--config="):
			req.ConfigPath = strings.TrimPrefix(a, "--config=")
			i++
		default:
			if strings.HasPrefix(a, "-") {
				return req, fmt.Errorf("unrecognized option %q", a)
			}
			if at := strings.LastIndex(a, "@"); at > 0 {
				asset, srv = a[:at], a[at+1:]
			} else {
				asset = a
			}
			rest = args[i+1:]
			i = len(args)
		}
	}

	req.Asset = strings.TrimSpace(asset)
	req.Server = strings.TrimSpace(srv)
	if req.Asset == "" {
		return req, errors.New("expected '-l <asset> <server>' or '<asset>@<server>' from rsync")
	}
	if len(rest) == 0 {
		return req, errors.New("no remote command in arguments")
	}
	req.Command = strings.Join(rest, " ")
	return req, nil
}

// sshBridgeExec is one remote exec channel carrying rsync's protocol.
type sshBridgeExec struct {
	client *ssh.Client
	sess   *ssh.Session
	cmd    string
}

func (s *sshBridgeExec) Run(stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	s.sess.Stdin = stdin
	s.sess.Stdout = stdout
	s.sess.Stderr = stderr

	err := s.sess.Run(s.cmd)
	if err == nil {
		return 0, nil
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero remote exit is the relay's normal outcome.
		return exitErr.ExitStatus(), nil
	}
	var missing *ssh.ExitMissingError
	if errors.As(err, &missing) {
		return 1, fmt.Errorf("remote command %q exited without a status", s.cmd)
	}
	return 1, fmt.Errorf("remote command %q: %w", s.cmd, err)
}

func (s *sshBridgeExec) Close() error {
	err := s.sess.Close()
	if cerr := s.client.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// openKoKoBridge is the real bridge: load config, log in, resolve the
// asset, open an ssh/web_cli token and an exec channel for the remote
// command.
func openKoKoBridge(ctx context.Context, req bridgeRequest) (bridgeExec, error) {
	cfg, err := config.Load(req.ConfigPath)
	if err != nil {
		return nil, err
	}
	srv, err := bridgeServer(cfg, req.Server)
	if err != nil {
		return nil, err
	}
	creds, err := bridgeCredentials(srv.Name)
	if err != nil {
		return nil, err
	}
	sess, err := bridgeLogin(ctx, srv, creds)
	if err != nil {
		return nil, err
	}
	asset, err := assets.Resolve(ctx, sess.Client, req.Asset, "", "ssh")
	if err != nil {
		return nil, fmt.Errorf("resolve asset %q: %w", req.Asset, err)
	}
	token, err := transport.NewConnectionToken(ctx, sess, asset, "ssh", "web_cli")
	if err != nil {
		return nil, fmt.Errorf("connection token for %s: %w", asset.Name, err)
	}
	client, err := dialSSH(ctx, sess, token)
	if err != nil {
		return nil, err
	}
	sshSess, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ssh exec channel: %w", err)
	}
	return &sshBridgeExec{client: client, sess: sshSess, cmd: req.Command}, nil
}

// bridgeServer returns the named server, or the default one when the
// rsync invocation carried no alias.
func bridgeServer(cfg *config.AppConfig, name string) (*config.ServerConfig, error) {
	if strings.TrimSpace(name) == "" {
		return cfg.DefaultServer()
	}
	return cfg.Get(name)
}

// bridgeCredentials reads the password and the optional TOTP secret from
// the credential chain.
//
// The bridge is non-interactive (rsync owns the terminal), so a server
// that demands MFA with no stored secret fails here with the credential
// layer's guidance rather than blocking on a prompt.
func bridgeCredentials(alias string) (auth.Credentials, error) {
	store := config.Chain(config.NewEnvStore(), config.NewKeyringStore())
	password, err := store.Get(alias, config.CredPassword)
	if err != nil {
		return auth.Credentials{}, err
	}
	creds := auth.Credentials{Password: password}
	if secret, err := store.Get(alias, config.CredOTP); err == nil {
		creds.OTPSecret = secret
	}
	return creds, nil
}

// bridgeLogin authenticates against the server under the same failover
// policy the rest of the CLI uses (DESIGN.md §4.7).
func bridgeLogin(ctx context.Context, srv *config.ServerConfig, creds auth.Credentials) (*auth.Session, error) {
	var session *auth.Session
	login := func(ctx context.Context, cand endpoint.Candidate) error {
		s, err := auth.Login(ctx, srv, cand.URL, creds, nil)
		if err != nil {
			return err
		}
		session = s
		return nil
	}
	if _, err := endpoint.SelectAndLogin(ctx, srv, "", endpoint.TCPProbe, login, bridgeStateStore()); err != nil {
		return nil, err
	}
	if session == nil || session.Client == nil {
		return nil, fmt.Errorf("login for server %q returned no API client", srv.Name)
	}
	return session, nil
}

// bridgeStateStore places the endpoint memory beside the configuration
// file, so an injected JMS_CONFIG relocates it too.
func bridgeStateStore() endpoint.StateStore {
	p, err := config.DefaultPath()
	if err != nil {
		return endpoint.NewMemoryStateStore()
	}
	return endpoint.NewFileStateStore(filepath.Join(filepath.Dir(p), endpoint.StateFileName))
}
