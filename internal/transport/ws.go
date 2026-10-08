package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/transport/console"
)

// PathWSTerminal is the KoKo WebSocket terminal endpoint.
//
// /koko/ws/token/ returns 404 on some versions, so the terminal path is the
// only one used (DESIGN.md §3.2).
const PathWSTerminal = "/koko/ws/terminal/"

// WSSubprotocol is the subprotocol KoKo requires on the handshake.
const WSSubprotocol = "JMS-KOKO"

// wsConnectTimeout bounds the WebSocket handshake (DESIGN.md §4.5).
const wsConnectTimeout = DialTimeout

// InteractiveReadTimeout is retained for compatibility with callers that
// reference the transport package. Interactive reads no longer use a read
// deadline: Gorilla WebSocket documents every read error as permanent, so a
// deadline timeout cannot be treated as an idle-loop signal.
const InteractiveReadTimeout = 30 * time.Second

// wsDialer is indirected so tests can replace the dialer with one aimed at
// an httptest server.
var wsDialer = dialWS

// connectWS opens the KoKo WebSocket backend.
//
// The handshake must carry the jms_sessionid cookie from the form login — a
// bearer token is rejected by KoKo — and the JMS-KOKO subprotocol. Output
// arrives as binary frames; input is text-frame JSON. Keepalive is an
// application-level text-frame PING every Keepalive, because Nginx is a
// transparent TCP tunnel and never delivers a WebSocket-level ping
// (DESIGN.md §3.2).
//
// The endpoint is bound to the session (DESIGN.md §4.7 point 1): the host is
// parsed from sess.BaseURL, never re-resolved.
func connectWS(ctx context.Context, sess *auth.Session, asset assets.Info, opts ConnectOptions) (Terminal, error) {
	host := kokoHostPort(sess.BaseURL)
	if host == "" {
		return nil, fmt.Errorf("websocket backend: session base URL %q has no host", sess.BaseURL)
	}

	var lastErr error
	// One retry with a fresh token, matching the SSH backend: a stale token
	// is the most common cause of a rejected handshake.
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		term, err := dialWSOnce(ctx, sess, asset, opts, host)
		if err == nil {
			return term, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("websocket connect to %s: %w", host, lastErr)
}

// dialWSOnce performs one token-request-plus-handshake attempt.
func dialWSOnce(ctx context.Context, sess *auth.Session, asset assets.Info,
	opts ConnectOptions, host string) (*wsTerminal, error) {

	token, err := newConnectionToken(ctx, sess, asset, opts.Protocol, opts.ConnectMethod)
	if err != nil {
		return nil, fmt.Errorf("connection token: %w", err)
	}

	wsURL := wsTerminalURL(sess.BaseURL, host, token.ID)
	conn, resp, err := wsDialer(ctx, sess, wsURL)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("handshake: %s returned HTTP %d", wsURL, resp.StatusCode)
		}
		return nil, fmt.Errorf("handshake: %w", err)
	}

	term := &wsTerminal{
		conn:  conn,
		done:  make(chan struct{}),
		cols:  opts.Cols,
		rows:  opts.Rows,
		proto: opts.Protocol,
	}
	interval := opts.Keepalive
	if interval <= 0 {
		interval = Keepalive
	}
	if err := term.initialise(); err != nil {
		conn.Close()
		return nil, err
	}
	term.startKeepalive(interval)
	return term, nil
}

// dialWS performs the actual upgrade, carrying the session cookie and the
// JMS-KOKO subprotocol.
//
// The dialer carries the session's proxy policy explicitly. Leaving
// gorilla's Proxy field unset means "direct", which is what jms wants by
// default — but stating it makes the policy visible and, more
// importantly, keeps the WebSocket on the same path as the REST login
// that produced the session (DESIGN.md §6.1).
func dialWS(ctx context.Context, sess *auth.Session, wsURL string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	if sid := sess.SessionID(); sid != "" {
		header.Set("Cookie", auth.CookieSession+"="+sid)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: wsConnectTimeout,
		Subprotocols:     []string{WSSubprotocol},
		Proxy:            sess.NetProxy().HTTPProxyFunc(),
	}
	return dialer.DialContext(ctx, wsURL, header)
}

// wsTerminalURL builds the KoKo terminal URL.
//
// /koko/ws/token/ returns 404 on some versions, so the terminal path is the
// only one used (DESIGN.md §3.2). The scheme follows the base URL: http →
// ws, https → wss.
func wsTerminalURL(baseURL, host, tokenID string) string {
	scheme := "ws"
	if strings.HasPrefix(strings.TrimSpace(baseURL), "https://") {
		scheme = "wss"
	}
	ts := time.Now().UnixMilli()
	return (&url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   PathWSTerminal,
		RawQuery: url.Values{
			"disableautohash": {"false"},
			"token":           {tokenID},
			"_":               {strconv.FormatInt(ts, 10)},
		}.Encode(),
	}).String()
}

// wsMessage is one JSON frame sent to KoKo.
//
// ID is the session id from the CONNECT frame, which is a UUID string.
type wsMessage struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

// Frame types on the KoKo WebSocket terminal.
const (
	msgConnect        = "CONNECT"
	msgTerminalInit   = "TERMINAL_INIT"
	msgTerminalData   = "TERMINAL_DATA"
	msgTerminalResize = "TERMINAL_RESIZE"
	msgPing           = "PING"
	msgPong           = "PONG"
	msgClose          = "CLOSE"
)

// wsTerminal is the Terminal implementation over the KoKo WebSocket
// backend. It drives one shared PTY, so exactly one Execute runs at a time
// (the pool serializes callers) and the keepalive shares the connection
// under a write mutex.
type wsTerminal struct {
	conn *websocket.Conn

	writeMu sync.Mutex // serialises writes to conn
	idMu    sync.Mutex // guards wsID
	wsID    string

	cols, rows int
	proto      string

	done     chan struct{}
	stopped  chan struct{}
	once     sync.Once
	closeErr error
}

// Backend reports the WebSocket backend.
func (t *wsTerminal) Backend() BackendType { return BackendWS }

// initialise waits for the CONNECT message, records the connection id and
// answers with TERMINAL_INIT carrying the PTY size.
func (t *wsTerminal) initialise() error {
	if t.cols <= 0 {
		t.cols = DefaultCols
	}
	if t.rows <= 0 {
		t.rows = DefaultRows
	}
	// Read one frame and take the session id from it, mirroring the Python
	// reference exactly (ws.recv_data() -> json.loads -> msg["id"]). Two
	// earlier mistakes here cost a real diagnostic cycle:
	//
	//   - requiring a text frame: KoKo may deliver the CONNECT frame as a
	//     binary frame, and insisting on text made the handshake time out
	//     against the real server while passing against a fake that sent text.
	//   - requiring type == "CONNECT" and an integer id: the id is a session
	//     UUID string, and the frame carries no type field to match on.
	//
	// So the frame type is ignored and the id is decoded as a string.
	remaining := time.Until(time.Now().Add(wsConnectTimeout))
	if remaining <= 0 {
		return fmt.Errorf("websocket: no CONNECT message within %s", wsConnectTimeout)
	}
	// A read deadline is required: without it ReadMessage blocks indefinitely
	// on a server that never speaks, so the timeout would never be noticed.
	_ = t.conn.SetReadDeadline(time.Now().Add(remaining))
	_, payload, err := t.readMessage()
	if err != nil {
		return fmt.Errorf("websocket: waiting for CONNECT: %w", err)
	}

	var connectFrame struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(payload, &connectFrame); err != nil || len(connectFrame.ID) == 0 {
		return fmt.Errorf("websocket: the first frame is not a CONNECT message: %s",
			truncateForError(payload))
	}
	sessionID := decodeFrameID(connectFrame.ID)
	if sessionID == "" {
		return fmt.Errorf("websocket: the CONNECT frame carried no usable id: %s",
			truncateForError(payload))
	}
	t.idMu.Lock()
	t.wsID = sessionID
	t.idMu.Unlock()

	init, _ := json.Marshal(wsMessage{
		ID:   sessionID,
		Type: msgTerminalInit,
		Data: fmt.Sprintf(`{"cols":%d,"rows":%d}`, t.cols, t.rows),
	})
	if err := t.writeText(init); err != nil {
		return fmt.Errorf("websocket: TERMINAL_INIT: %w", err)
	}
	// Clear the deadline so Execute starts from a clean read state.
	_ = t.conn.SetReadDeadline(time.Time{})
	return nil
}

// decodeFrameID renders a JSON id that may be a string or a number as the
// string the terminal messages must echo back.
func decodeFrameID(raw json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		return asNumber.String()
	}
	return ""
}

// truncateForError bounds a server payload so an error message stays
// readable and cannot dump a whole frame into the terminal.
func truncateForError(payload []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(payload))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// startKeepalive sends an application-level text-frame PING every interval.
//
// Nginx is a transparent TCP tunnel: a WebSocket-level ping never reaches
// KoKo, so the heartbeat lives above the protocol (DESIGN.md §3.2). A server
// PING is answered with a PONG. The loop exits when Close closes done, or
// when the connection dies — either way the next Execute reports the failure.
func (t *wsTerminal) startKeepalive(interval time.Duration) {
	t.stopped = make(chan struct{})
	go func() {
		defer close(t.stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-t.done:
				return
			case <-ticker.C:
				ping, _ := json.Marshal(wsMessage{Type: msgPing})
				if err := t.writeText(ping); err != nil {
					return
				}
			}
		}
	}()
}

// Execute runs one command on the shared PTY.
//
// The command is wrapped so the shell echoes a unique done marker around its
// output and a second line carrying the exit status. Output is delimited by
// the marker appearing TWICE — the first occurrence is the shell echoing the
// command you sent, the second marks the end of its output — which is what
// the Python reference relies on and what removes the need to drain: nothing
// before the first marker or after the second belongs to this command.
//
// A non-zero remote exit is not an error — it is reported in Result.ExitCode.
//
// Warning (DESIGN.md §11.1): a command ending in `#` or `\`, or with
// unbalanced quotes or parentheses, swallows the appended rc-capture chain
// and the marker never arrives. The timeout is the only defence — always
// pass one for commands that are not known to terminate.
func (t *wsTerminal) Execute(ctx context.Context, cmd string, timeout time.Duration) (Result, error) {
	if t == nil || t.conn == nil {
		return Result{}, errors.New("websocket terminal is closed")
	}

	marker := fmt.Sprintf("__JMSDONE_%d__", time.Now().UnixNano())
	wrapped := fmt.Sprintf("%s; __rc=$?; echo %s; echo __JMSRC:${__rc}__", cmd, marker)
	payload, _ := json.Marshal(wsMessage{ID: t.currentID(), Type: msgTerminalData, Data: wrapped + "\r"})
	if err := t.writeText(payload); err != nil {
		return Result{}, fmt.Errorf("websocket send: %w", err)
	}

	return t.readUntilMarkers(ctx, marker, timeout)
}

// ansiPattern matches the escape sequences a PTY emits around command echo.
//
// It mirrors the reference implementation's ANSI_RE. Without stripping, a real
// KoKo session leaks bracketed-paste toggles such as ESC[?2004l into the
// command output, which a caller capturing the result would see as garbage.
// ansiPattern matches the escape sequences a PTY emits around command echo.
//
// The parameter class includes ? and > so DEC private modes are covered
// (ESC[?2004h / ESC[?2004l bracketed-paste toggles, which a real KoKo session
// emits on every command). The reference implementation's ANSI_RE omits them,
// which is why its own output can still carry those sequences.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?>=]*[a-zA-Z]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)|\x08.")

// stripANSI removes terminal control sequences from captured output.
func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// normalizeNewlines folds the CRLF and bare-CR line endings a PTY produces
// into plain newlines, so the result is the same text regardless of backend.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// rcPattern extracts the exit status from the __JMSRC:N__ marker.
var rcPattern = regexp.MustCompile(`__JMSRC:(\d+)__`)

// readUntilMarkers reads frames until the done marker has appeared twice and
// an exit status follows it, then returns the text between the two markers.
//
// Delimiting on the marker's second occurrence (rather than draining first)
// is what the Python reference does: the first occurrence is the echoed
// command, so anything before it — including a previous command's tail — is
// discarded, and the output is everything between the two.
func (t *wsTerminal) readUntilMarkers(ctx context.Context, marker string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)

	// A context watcher interrupts a blocking read, so cancellation is
	// observed promptly instead of waiting out the read deadline. Without
	// it, cancelling a call with no timeout would block until the default
	// 30s expiry.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = t.conn.SetReadDeadline(time.Now())
		case <-watchDone:
		}
	}()

	var stream strings.Builder
	wsDump := os.Getenv("JMS_WS_DUMP") != ""
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Result{Output: extractBetween(stream.String(), marker)},
				fmt.Errorf("websocket: command did not complete within %s — a command ending in '#' or '\\', or with unbalanced quotes, swallows the status marker (DESIGN.md §11.1)", timeout)
		}
		_ = t.conn.SetReadDeadline(time.Now().Add(remaining))

		msgType, payload, err := t.readMessage()
		if err != nil {
			if ctx.Err() != nil {
				return Result{Output: extractBetween(stream.String(), marker)}, ctx.Err()
			}
			// A read deadline fires as a net timeout; report it as the DESIGN
			// pitfall rather than an opaque transport error, because that is
			// the overwhelmingly common cause.
			if isTimeout(err) {
				return Result{Output: extractBetween(stream.String(), marker)},
					fmt.Errorf("websocket: command did not complete within %s — a command ending in '#' or '\\', or with unbalanced quotes, swallows the status marker (DESIGN.md §11.1)", timeout)
			}
			return Result{Output: extractBetween(stream.String(), marker)},
				fmt.Errorf("websocket read: %w", err)
		}

		// A server PING must be answered with a PONG on the same channel.
		if msgType == websocket.TextMessage && strings.Contains(string(payload), `"type":"PING"`) {
			pong, _ := json.Marshal(wsMessage{Type: msgPong})
			if err := t.writeText(pong); err != nil {
				return Result{Output: extractBetween(stream.String(), marker)}, err
			}
			continue
		}
		if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
			continue
		}

		stream.Write(payload)
		if wsDump {
			fmt.Fprintf(os.Stderr, "[wsdump] +%d bytes: %q\n", len(payload), payload)
		}
		if code, ok := parseMarkers(stream.String(), marker); ok {
			// Clear the read deadline so the next Execute starts clean.
			_ = t.conn.SetReadDeadline(time.Time{})
			return Result{Output: extractBetween(stream.String(), marker), ExitCode: code}, nil
		}
		if ctx.Err() != nil {
			return Result{Output: extractBetween(stream.String(), marker)}, ctx.Err()
		}
	}
}

// parseMarkers reports the exit status once the done marker has appeared
// twice and an __JMSRC:N__ marker follows the second occurrence.
//
// The LAST two occurrences are used, not the first two: a slow server may
// echo the command line twice (KoKo echoes the input before the PTY is
// ready, then the PTY echoes it again), so the stream can hold three or
// more marker copies. The final pair is always the PTY echo and the
// end-of-output marker, which is what delimits the command output.
func parseMarkers(stream, marker string) (int, bool) {
	last := strings.LastIndex(stream, marker)
	if last < 0 {
		return 0, false
	}
	tail := stream[last+len(marker):]
	m := rcPattern.FindStringSubmatch(tail)
	if m == nil {
		return 0, false
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// extractBetween returns the command's output: the text after the
// second-to-last marker's newline and before the last marker, with terminal
// control sequences stripped and line endings normalised.
//
// The LAST two occurrences are used rather than the first two: a slow
// server may echo the command line twice (KoKo echoes the input while the
// PTY is still being attached, then the PTY echoes it again), so the stream
// can hold three or more copies. The final pair is always the PTY echo and
// the end-of-output marker, so slicing between them survives any number of
// earlier duplicates. Starting after the marker's NEWLINE matters: the
// shell echoes the wrapped command line, so what sits between a marker and
// its newline is the tail of the rc-capture chain, not output.
func extractBetween(stream, marker string) string {
	clean := normalizeNewlines(stripANSI(stream))
	last := strings.LastIndex(clean, marker)
	if last < 0 {
		return strings.TrimSpace(clean)
	}
	prev := strings.LastIndex(clean[:last], marker)
	if prev < 0 {
		// The command has not finished; there is no output to report yet.
		return ""
	}

	start := prev + len(marker)
	if nl := strings.Index(clean[start:], "\n"); nl >= 0 && start+nl < last {
		start = start + nl + 1
	}
	if start >= last {
		return ""
	}
	return strings.TrimSpace(clean[start:last])
}

// isTimeout reports whether err is a network deadline expiry.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isNormalWSClose reports WebSocket close conditions that mean the peer
// ended the interactive session normally.
func isNormalWSClose(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived)
}

// readMessage reads one frame, translating a keepalive-server PING so the
// caller's PONG logic sees it as an ordinary text frame.
func (t *wsTerminal) readMessage() (int, []byte, error) {
	msgType, payload, err := t.conn.ReadMessage()
	if err != nil {
		return 0, nil, err
	}
	// KoKo sends its heartbeat as a text frame carrying {"type":"PING"};
	// readUntilMarker answers it, so this wrapper only needs to pass frames
	// through.
	return msgType, payload, nil
}

func (t *wsTerminal) writeText(payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.WriteMessage(websocket.TextMessage, payload)
}

// currentID returns the connection id the CONNECT message carried.
func (t *wsTerminal) currentID() string {
	t.idMu.Lock()
	defer t.idMu.Unlock()
	return t.wsID
}

// Interactive attaches the local console to the shared PTY.
func (t *wsTerminal) Interactive(ctx context.Context) error {
	if t == nil || t.conn == nil {
		return errors.New("websocket terminal is closed")
	}

	con, restore, err := console.Open(console.Options{
		OnResize: func(size console.Size) {
			// KoKo resizes its PTY from a TERMINAL_RESIZE message.
			resize, _ := json.Marshal(wsMessage{
				ID:   t.currentID(),
				Type: msgTerminalResize,
				Data: fmt.Sprintf(`{"cols":%d,"rows":%d}`, size.Cols, size.Rows),
			})
			_ = t.writeText(resize)
		},
	})
	if err != nil {
		return fmt.Errorf("interactive mode: %w", err)
	}
	defer restore()

	// Push the local window size before the first prompt so the remote line
	// editing matches the terminal the user is actually looking at.
	size := con.Size()
	init, _ := json.Marshal(wsMessage{
		ID:   t.currentID(),
		Type: msgTerminalResize,
		Data: fmt.Sprintf(`{"cols":%d,"rows":%d}`, size.Cols, size.Rows),
	})
	_ = t.writeText(init)

	// A local Ctrl+] requests a clean detach. Closing the connection wakes
	// the single read loop, while closeRequested lets that expected read error
	// return nil instead of being reported as a transport failure.
	closeRequested := make(chan struct{})
	var requestClose sync.Once
	closeInteractive := func() {
		requestClose.Do(func() {
			close(closeRequested)
			closeFrame, _ := json.Marshal(wsMessage{Type: msgClose})
			_ = t.writeText(closeFrame)
			_ = t.conn.Close()
		})
	}

	// Local keystrokes become TERMINAL_DATA frames.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, readErr := con.Read(buf)
			if n > 0 {
				input := buf[:n]
				if i := bytes.IndexByte(input, 0x1d); i >= 0 {
					if i > 0 {
						frame, _ := json.Marshal(wsMessage{
							ID:   t.currentID(),
							Type: msgTerminalData,
							Data: string(input[:i]),
						})
						_ = t.writeText(frame)
					}
					closeInteractive()
					return
				}
				frame, _ := json.Marshal(wsMessage{
					ID:   t.currentID(),
					Type: msgTerminalData,
					Data: string(input),
				})
				if err := t.writeText(frame); err != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// Gorilla WebSocket requires the caller to stop reading after the first
	// read error, including a read-deadline timeout. Keep this read blocking
	// while the terminal is idle, and close the connection on cancellation so
	// the blocked read wakes up without a second read attempt.
	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = t.conn.Close()
			return
		case <-cancelDone:
		}
	}()
	defer close(cancelDone)

	// Remote output and control frames are written straight to the screen.
	for {
		msgType, payload, err := t.readMessage()
		if err != nil {
			if ctx.Err() != nil || isNormalWSClose(err) {
				return nil
			}
			select {
			case <-closeRequested:
				return nil
			default:
			}
			return fmt.Errorf("websocket interactive read: %w", err)
		}

		switch msgType {
		case websocket.BinaryMessage:
			_, _ = con.Write(payload)
		case websocket.TextMessage:
			// Control frames are never terminal output. PING gets a PONG;
			// CLOSE means the remote shell has ended and is a normal return.
			var ctrl struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(payload, &ctrl); err != nil {
				continue
			}
			switch {
			case strings.EqualFold(ctrl.Type, msgPing):
				pong, _ := json.Marshal(wsMessage{Type: msgPong})
				_ = t.writeText(pong)
			case strings.EqualFold(ctrl.Type, msgClose):
				closeInteractive()
				return nil
			}
		}

		if ctx.Err() != nil {
			return nil
		}
	}
}

// Close tears the terminal down. It is safe to call more than once.
func (t *wsTerminal) Close() error {
	if t == nil {
		return nil
	}
	t.once.Do(func() {
		close(t.done)
		<-t.stopped
		closeFrame, _ := json.Marshal(wsMessage{Type: msgClose})
		_ = t.writeText(closeFrame)
		t.closeErr = t.conn.Close()
	})
	return t.closeErr
}
