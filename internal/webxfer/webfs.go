// Package webxfer moves files through the KoKo web file-manager
// WebSocket, /koko/ws/sftp/, which is the same channel the browser's file
// manager uses. A deployment that exposes only the web port cannot reach
// the KoKo SSH port (default 2222) the SFTP engine rides on; this channel
// needs nothing beyond the web URL, the jms_sessionid cookie and a
// sftp/web_sftp connection token.
//
// The wire protocol (KoKo v4.x pkg/httpd/websftp.go) is JSON text frames:
//
//	{"id": "1", "type": "SFTP_DATA", "cmd": "upload",
//	 "data": "{\"path\":\"/tmp/f\",\"chunk\":true,\"offset\":0}",
//	 "raw": "<base64 chunk>"}
//
// Responses come back with the same id. Uploads are chunk-then-merge: every
// chunk of one file shares one integer chunk id (the server keeps one file
// handle per id), and a final merge command closes the handle. Downloads
// stream any number of SFTP_BINARY frames followed by one terminating
// SFTP_DATA frame whose data field carries the file name.
//
// The protocol has no stat and no seek on downloads: the remote size is
// unknown until the stream ends, and the remote destination must name the
// file itself (no cp-style directory expansion).
package webxfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/transport"
)

// Wire constants. The sftp token pair is the same one the SFTP engine
// opens: protocol "sftp", connect_method "web_sftp" (DESIGN.md「KoKo 协议」).
const (
	pathWSSftp = "/koko/ws/sftp/"

	// The token pair the file-manager WebSocket expects.
	sftpProtocol      = "sftp"
	sftpConnectMethod = "web_sftp"

	typeSFTPData   = "SFTP_DATA"
	typeSFTPBinary = "SFTP_BINARY"
	typeConnect    = "CONNECT"
	typeError      = "ERROR"
	typeClose      = "CLOSE"
	typePing       = "PING"
	typePong       = "PONG"

	cmdUpload   = "upload"
	cmdDownload = "download"

	// wsConnectTimeout bounds the WebSocket handshake.
	wsConnectTimeout = 15 * time.Second

	// readTimeout bounds one idle read on an open connection. Active
	// transfers always answer faster than this.
	readTimeout = 10 * time.Minute
)

// MaxChunk is the largest raw payload one upload frame may carry. KoKo
// reads each frame into a 2 MiB buffer (transferChunkMaxSize); the base64
// body inside the JSON is larger than the raw bytes, so staying at 2 MiB
// raw keeps the decoded payload within the server's buffer.
const MaxChunk = 2 << 20

// DefaultChunk is the upload chunk size: 1 MiB raw, ~1.33 MiB as base64.
const DefaultChunk = 1 << 20

// ErrProtocol reports a server answer that does not match the protocol
// this client speaks (an unexpected frame shape or order).
var ErrProtocol = errors.New("webxfer: protocol error")

// message mirrors KoKo's httpd.Message as it appears on the sftp
// WebSocket. Raw travels base64-encoded inside the JSON text frame.
type message struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Data        string `json:"data,omitempty"`
	Raw         []byte `json:"raw,omitempty"`
	Cmd         string `json:"cmd,omitempty"`
	CurrentPath string `json:"current_path,omitempty"`
	Err         string `json:"err,omitempty"`
}

// request is the JSON payload of the data field.
type request struct {
	Path   string `json:"path"`
	Chunk  bool   `json:"chunk,omitempty"`
	Merge  bool   `json:"merge,omitempty"`
	OffSet int64  `json:"offset,omitempty"`
	Size   int64  `json:"size,omitempty"`
	IsDir  bool   `json:"is_dir,omitempty"`
}

// Client is one file-manager WebSocket connection.
//
// Commands are round-trips keyed by the frame id: call registers a waiter,
// the read pump delivers every frame carrying that id, and the caller
// consumes until the terminating SFTP_DATA frame. Upload chunks of one
// file share a single id by protocol (the server keeps one handle per id),
// so chunks are sent strictly one at a time.
type Client struct {
	conn *websocket.Conn

	writeMu sync.Mutex // serialises writes to conn

	mu      sync.Mutex
	pending map[string]chan *message
	ready   chan *message // first CONNECT or ERROR frame
	closed  bool

	nextID int64

	closeOnce sync.Once
	doneOnce  sync.Once
	done      chan struct{}
}

// Dial opens the file-manager WebSocket for asset.
//
// The connection token is created with the same sftp/web_sftp pair the
// SFTP engine uses; the WebSocket itself authenticates with the
// jms_sessionid cookie and the JMS-KOKO subprotocol, exactly like the
// terminal WebSocket (DESIGN.md「WebSocket 终端」). Dial blocks until the
// server's CONNECT frame arrives, so a returned client is ready for its
// first command.
func Dial(ctx context.Context, sess *auth.Session, asset assets.Info) (*Client, error) {
	if sess == nil || sess.Client == nil {
		return nil, errors.New("webxfer: session has no API client")
	}
	token, err := transport.NewConnectionToken(ctx, sess, asset, sftpProtocol, sftpConnectMethod)
	if err != nil {
		return nil, fmt.Errorf("webxfer: sftp connection token for %s: %w", asset.Name, err)
	}

	header := http.Header{}
	if sid := sess.SessionID(); sid != "" {
		header.Set("Cookie", auth.CookieSession+"="+sid)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: wsConnectTimeout,
		Subprotocols:     []string{transport.WSSubprotocol},
		Proxy:            sess.NetProxy().HTTPProxyFunc(),
	}
	wsURL := wsSftpURL(sess.BaseURL, token.ID)
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("webxfer: dial %s: %w", wsURL, err)
	}

	c := &Client{
		conn:    conn,
		pending: make(map[string]chan *message),
		ready:   make(chan *message, 1),
		done:    make(chan struct{}),
	}
	go c.readPump()

	select {
	case msg := <-c.ready:
		if msg == nil || msg.Type == typeError || msg.Type == typeClose {
			_ = c.Close()
			reason := "connection failed"
			if msg != nil && msg.Err != "" {
				reason = msg.Err
			}
			return nil, fmt.Errorf("webxfer: connect to %s rejected: %s", asset.Name, reason)
		}
	case <-ctx.Done():
		_ = c.Close()
		return nil, fmt.Errorf("webxfer: waiting for connect frame: %w", ctx.Err())
	case <-c.done:
		return nil, fmt.Errorf("webxfer: connection to %s closed before the connect frame", asset.Name)
	}
	return c, nil
}

// wsSftpURL builds the file-manager WebSocket URL from the session's bound
// base URL: KoKo is proxied at /koko/ on the web host, so the host is the
// base URL's host and the scheme follows http → ws, https → wss.
func wsSftpURL(baseURL, tokenID string) string {
	scheme := "ws"
	if strings.HasPrefix(strings.TrimSpace(baseURL), "https://") {
		scheme = "wss"
	}
	host := hostPort(baseURL)
	return (&url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   pathWSSftp,
		RawQuery: url.Values{
			"token": {tokenID},
			"_":     {strconv.FormatInt(time.Now().UnixMilli(), 10)},
		}.Encode(),
	}).String()
}

// hostPort extracts host:port from a base URL, tolerating a missing
// scheme. It mirrors transport.kokoHostPort, which is unexported there.
func hostPort(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if u, err = url.Parse("https://" + raw); err != nil {
			return ""
		}
	}
	return u.Host
}

// Upload copies the local file to the remote path, chunk by chunk.
//
// remote must name the file itself: the web protocol cannot stat the
// destination, so cp-style directory expansion is impossible. Chunks are
// sequential (the server writes one handle per chunk id) and each chunk is
// acknowledged before the next is sent, which also bounds memory. The
// final merge command closes the server-side handle.
func (c *Client) Upload(ctx context.Context, local, remote string, chunkSize int) (int64, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunk
	}
	if chunkSize > MaxChunk {
		return 0, fmt.Errorf("webxfer: chunk size %d exceeds the %d-byte protocol maximum",
			chunkSize, MaxChunk)
	}
	if strings.TrimSpace(remote) == "" {
		return 0, errors.New("webxfer: remote path is empty")
	}

	f, err := os.Open(local)
	if err != nil {
		return 0, fmt.Errorf("webxfer: open %s: %w", local, err)
	}
	defer func() { _ = f.Close() }()

	// One chunk id for the whole file: the server opens the destination
	// on the first chunk and keeps the handle until merge.
	const cid = "1"
	var sent int64
	buf := make([]byte, chunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return sent, err
		}
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			req := request{Path: remote, Chunk: true, OffSet: sent}
			if err := c.roundTripAck(ctx, cid, cmdUpload, req, buf[:n]); err != nil {
				return sent, err
			}
			sent += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return sent, fmt.Errorf("webxfer: read %s: %w", local, rerr)
		}
	}

	// merge closes the server-side handle; the ack's data is "ok".
	if err := c.roundTripAck(ctx, cid, cmdUpload, request{Path: remote, Merge: true}, nil); err != nil {
		return sent, err
	}
	return sent, nil
}

// Download streams the remote file to the local path.
//
// The remote size is not announced: the result reports the bytes actually
// received. SFTP_BINARY frames are appended as they arrive and the
// terminating SFTP_DATA frame (whose data is the remote file name) commits
// the file.
func (c *Client) Download(ctx context.Context, remote, local string) (int64, error) {
	if strings.TrimSpace(remote) == "" {
		return 0, errors.New("webxfer: remote path is empty")
	}
	if strings.TrimSpace(local) == "" {
		return 0, errors.New("webxfer: local path is empty")
	}
	if dir := filepath.Dir(local); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return 0, fmt.Errorf("webxfer: create %s: %w", dir, err)
		}
	}

	id := c.freshID()
	frames, err := c.call(ctx, id, cmdDownload, request{Path: remote}, nil)
	if err != nil {
		return 0, err
	}

	f, err := os.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		c.drop(id)
		return 0, fmt.Errorf("webxfer: open %s: %w", local, err)
	}
	defer func() { _ = f.Close() }()

	var received int64
	for msg := range frames {
		if msg.Err != "" {
			return received, fmt.Errorf("webxfer: download %s: %s", remote, msg.Err)
		}
		switch msg.Type {
		case typeSFTPBinary:
			if len(msg.Raw) > 0 {
				if _, err := f.Write(msg.Raw); err != nil {
					return received, fmt.Errorf("webxfer: write %s: %w", local, err)
				}
				received += int64(len(msg.Raw))
			}
		case typeSFTPData:
			// Terminating frame; data carries the remote file name.
			return received, nil
		case typeClose:
			return received, fmt.Errorf("webxfer: download %s: session closed", remote)
		default:
			return received, fmt.Errorf("%w: frame type %q during download of %s",
				ErrProtocol, msg.Type, remote)
		}
	}
	return received, fmt.Errorf("%w: download %s ended without a terminating frame", ErrProtocol, remote)
}

// Close tears the WebSocket down. It is safe to call more than once and
// races safely with the read pump shutting itself down on error.
func (c *Client) Close() error {
	c.closeOnce.Do(c.teardown)
	return nil
}

// teardown wakes every waiter and stops the client exactly once, whether
// the read pump died on its own (failAll) or the caller closed the client.
func (c *Client) teardown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	waiters := c.pending
	c.pending = make(map[string]chan *message)
	c.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	c.doneOnce.Do(func() {
		_ = c.conn.Close()
		close(c.done)
	})
}

// Done reports the connection's lifetime channel; it closes when the
// connection dies.
func (c *Client) Done() <-chan struct{} { return c.done }

// freshID allocates a request id for commands that are not upload chunks.
func (c *Client) freshID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return strconv.FormatInt(c.nextID, 10)
}

// call sends one command and returns the channel every response frame for
// its id will arrive on. The channel closes when the connection dies; the
// caller drops it with drop once finished.
func (c *Client) call(ctx context.Context, id, cmd string, req request, raw []byte) (<-chan *message, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("webxfer: marshal request: %w", err)
	}
	ch := make(chan *message, 8)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("webxfer: connection is closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(ctx, message{ID: id, Type: typeSFTPData, Cmd: cmd, Data: string(payload), Raw: raw}); err != nil {
		c.drop(id)
		return nil, err
	}
	return ch, nil
}

// drop removes a pending waiter, closing its channel.
func (c *Client) drop(id string) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ok {
		close(ch)
	}
}

// roundTripAck sends one command and waits for its single SFTP_DATA
// acknowledgement, failing on any error field.
func (c *Client) roundTripAck(ctx context.Context, id, cmd string, req request, raw []byte) error {
	frames, err := c.call(ctx, id, cmd, req, raw)
	if err != nil {
		return err
	}
	defer c.drop(id)
	for msg := range frames {
		if msg.Err != "" {
			return fmt.Errorf("webxfer: %s %s: %s", cmd, req.Path, msg.Err)
		}
		if msg.Type == typeSFTPData {
			return nil
		}
		if msg.Type == typeClose {
			return fmt.Errorf("webxfer: %s %s: session closed", cmd, req.Path)
		}
		return fmt.Errorf("%w: frame type %q while awaiting %s ack", ErrProtocol, msg.Type, cmd)
	}
	return fmt.Errorf("%w: %s ack never arrived", ErrProtocol, cmd)
}

// send writes one JSON text frame.
func (c *Client) send(ctx context.Context, msg message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("webxfer: marshal frame: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(readTimeout))
	if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("webxfer: write frame: %w", err)
	}
	return nil
}

// readPump receives frames until the connection dies, dispatching by id
// and handling the control frames itself.
func (c *Client) readPump() {
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			c.failAll(fmt.Errorf("webxfer: read: %w", err))
			return
		}
		var msg message
		if err := json.Unmarshal(payload, &msg); err != nil {
			// Not JSON: this client only speaks the JSON protocol.
			c.failAll(fmt.Errorf("%w: undecodable frame %q", ErrProtocol, truncate(payload, 120)))
			return
		}
		switch msg.Type {
		case typePing:
			// Keep the server's liveness bookkeeping fed.
			_ = c.send(context.Background(), message{ID: msg.ID, Type: typePong})
			continue
		case typeConnect:
			select {
			case c.ready <- &msg:
			default:
			}
			continue
		case typeError, typeClose:
			select {
			case c.ready <- &msg:
			default:
			}
			c.failAll(errors.New(msg.Err))
			return
		}
		if msg.ID == "" {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[msg.ID]
		c.mu.Unlock()
		if !ok {
			// A late frame for a finished request; nothing waits for it.
			continue
		}
		select {
		case ch <- &msg:
		case <-c.done:
			return
		}
	}
}

// failAll wakes every waiter and shuts the client down; it runs when the
// read pump dies. A Dial still parked on the ready gate observes the death
// through the nil pushed there.
func (c *Client) failAll(cause error) {
	_ = cause // the waiters see the closed channel; cause is for logging call sites
	c.teardown()
	select {
	case c.ready <- nil:
	default:
	}
}

// truncate keeps error text bounded.
func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
