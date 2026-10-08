package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSHandshakeCarriesCookieAndSubprotocol(t *testing.T) {
	f := newFakeKoKo(t)
	ws := wsConnect(t, f, ConnectOptions{})
	_ = ws
	if !f.handshakeOK() {
		t.Fatal("the upgrade lacked the session cookie or the JMS-KOKO subprotocol")
	}
}

func TestWSURLCarriesTokenAndQuery(t *testing.T) {
	f := newFakeKoKo(t)
	ws := wsConnect(t, f, ConnectOptions{})
	_ = ws

	url := f.lastURL()
	for _, want := range []string{"disableautohash=false", "token=tok-42", "_="} {
		if !strings.Contains(url, want) {
			t.Errorf("upgrade URL %q lacks %q", url, want)
		}
	}
	if path := strings.Split(url, "?")[0]; !strings.HasSuffix(path, PathWSTerminal) {
		t.Errorf("upgrade URL %q does not target %s", url, PathWSTerminal)
	}
}

func TestWSTokenRequestCarriesAssetAndAccount(t *testing.T) {
	f := newFakeKoKo(t)
	ws := wsConnect(t, f, ConnectOptions{})
	_ = ws

	// asset-uuid|@USER|ssh|web_cli — the frozen defaults.
	want := strings.Join([]string{"asset-uuid", "@USER", "ssh", "web_cli"}, "|")
	if got := f.tokenBody(); got != want {
		t.Fatalf("token body = %q, want %q", got, want)
	}
}

func TestWSSendsTerminalInitAfterConnect(t *testing.T) {
	f := newFakeKoKo(t)
	ws := wsConnect(t, f, ConnectOptions{Cols: 120, Rows: 40})
	_ = ws

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range f.requests() {
			if msg.Type == msgTerminalInit && strings.Contains(msg.Data, `"cols":120`) &&
				strings.Contains(msg.Data, `"rows":40`) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no TERMINAL_INIT with cols=120/rows=40 among %d frames", len(f.requests()))
}

func TestWSMarkerParsingProducesOutputAndExitCode(t *testing.T) {
	f := newFakeKoKo(t)
	f.echoCommand("hello world", 0)
	ws := wsConnect(t, f, ConnectOptions{})

	res, err := ws.Execute(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Execute = %v", err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Fatalf("output = %q, want it to contain hello", res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestWSNonZeroExitIsNotAnError(t *testing.T) {
	f := newFakeKoKo(t)
	f.echoCommand("", 7)
	ws := wsConnect(t, f, ConnectOptions{})

	res, err := ws.Execute(context.Background(), "false", 5*time.Second)
	if err != nil {
		t.Fatalf("Execute = %v, want nil error for a remote non-zero exit", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestWSTimeoutOnMissingMarker(t *testing.T) {
	f := newFakeKoKo(t) // no scripted frames: the marker never arrives
	ws := wsConnect(t, f, ConnectOptions{})

	start := time.Now()
	_, err := ws.Execute(context.Background(), "sleep 999", 200*time.Millisecond)
	if err == nil {
		t.Fatal("Execute = nil error, want a timeout error when the marker never arrives")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Execute took %s, want it to honour the timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "unbalanced quotes") {
		t.Fatalf("error = %v, want it to reference the swallowed-marker pitfall", err)
	}
}

// TestWSKeepaliveSendsApplicationLevelPing pins the heartbeat: it must be an
// application-level text frame, because a WebSocket-level ping never reaches
// KoKo through Nginx (DESIGN.md「WebSocket 终端」).
func TestWSKeepaliveSendsApplicationLevelPing(t *testing.T) {
	pingSeen := make(chan struct{}, 1)
	f := newFakeKoKo(t)
	f.onFrames(func(_ *fakeKoKo, msg wsMessage) {
		if msg.Type == msgPing {
			select {
			case pingSeen <- struct{}{}:
			default:
			}
		}
	})
	ws := wsConnect(t, f, ConnectOptions{Keepalive: 50 * time.Millisecond})
	defer ws.Close()

	select {
	case <-pingSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("no application-level PING frame was sent")
	}
}

// TestWSServerPingIsAnsweredDuringExecute covers answering a server PING
// while a command is in flight.
func TestWSServerPingIsAnsweredDuringExecute(t *testing.T) {
	f := newFakeKoKo(t)
	var sentPing bool
	f.onFrames(func(fs *fakeKoKo, msg wsMessage) {
		if msg.Type != msgTerminalData {
			return
		}
		fs.mu.Lock()
		conn := fs.conn
		first := !sentPing
		sentPing = true
		fs.mu.Unlock()
		if conn == nil {
			return
		}
		marker := extractMarker(msg.Data)
		if first {
			ping, _ := json.Marshal(wsMessage{Type: msgPing})
			_ = conn.WriteMessage(websocket.TextMessage, ping)
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(msg.Data+"\r\n"))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("OUT\r\n"))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(marker+"\r\n__JMSRC:0__\r\n"))
	})
	ws := wsConnect(t, f, ConnectOptions{})

	res, err := ws.Execute(context.Background(), "x", 5*time.Second)
	if err != nil {
		t.Fatalf("Execute = %v (a mid-command PING must be answered, not fatal)", err)
	}
	if !strings.Contains(res.Output, "OUT") {
		t.Fatalf("output = %q, want it to contain OUT", res.Output)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range f.requests() {
			if msg.Type == msgPong {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no PONG was sent for the server's PING")
}

// TestWSOutputIsDelimitedByTheMarkerNotByDraining pins the design choice:
// output is taken from between the marker's two occurrences, so a previous
// command's tail cannot leak into the next result even though nothing drains
// the connection (this is what the Python reference does).
func TestWSOutputIsDelimitedByTheMarkerNotByDraining(t *testing.T) {
	f := newFakeKoKo(t)

	// The fake answers every command: the first one normally, subsequent ones
	// with a stale tail prepended before their own echo. Output must still be
	// taken from between the marker's two occurrences, so the stale tail is
	// discarded even though nothing drains the connection.
	var calls int
	f.onFrames(func(fs *fakeKoKo, msg wsMessage) {
		if msg.Type != msgTerminalData {
			return
		}
		fs.mu.Lock()
		conn := fs.conn
		calls++
		first := calls == 1
		fs.mu.Unlock()
		if conn == nil {
			return
		}
		marker := extractMarker(msg.Data)
		if !first {
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte("STALE-TAIL\r\n"))
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(msg.Data+"\r\n"))
		output := "FIRST-OUTPUT"
		if !first {
			output = "SECOND-OUTPUT"
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(output+"\r\n"))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(marker+"\r\n__JMSRC:0__\r\n"))
	})

	ws := wsConnect(t, f, ConnectOptions{})
	if _, err := ws.Execute(context.Background(), "first", 5*time.Second); err != nil {
		t.Fatalf("first Execute = %v", err)
	}

	res, err := ws.Execute(context.Background(), "second", 5*time.Second)
	if err != nil {
		t.Fatalf("second Execute = %v", err)
	}
	if strings.Contains(res.Output, "STALE-TAIL") {
		t.Fatalf("a previous command's tail leaked into the result: %q", res.Output)
	}
	if !strings.Contains(res.Output, "SECOND-OUTPUT") {
		t.Fatalf("the real output is missing: %q", res.Output)
	}
}

func TestWSCloseIsIdempotent(t *testing.T) {
	f := newFakeKoKo(t)
	ws := wsConnect(t, f, ConnectOptions{})
	if err := ws.Close(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := ws.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}
	if _, err := ws.Execute(context.Background(), "x", time.Second); err == nil {
		t.Fatal("Execute after Close = nil error, want one")
	}
}

// TestWSTripleEchoMarkerParsing pins the slow-server fix: KoKo can echo
// the command line once before the PTY is ready and again after, so the
// stream holds three marker copies. Output must come from between the LAST
// two, not the first two.
func TestWSTripleEchoMarkerParsing(t *testing.T) {
	f := newFakeKoKo(t)
	var echoed bool
	f.onFrames(func(fs *fakeKoKo, msg wsMessage) {
		if msg.Type != msgTerminalData {
			return
		}
		marker := extractMarker(msg.Data)
		if marker == "" {
			return
		}
		fs.mu.Lock()
		conn := fs.conn
		fs.mu.Unlock()
		if conn == nil {
			return
		}
		line := msg.Data + "\r\n"
		if !echoed {
			// Pre-PTY input echo: the whole wrapped line, no output.
			echoed = true
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte(line))
		}
		// PTY echo + output + end marker + exit status.
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(line))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte("REAL-OUTPUT\r\n"))
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(marker+"\r\n__JMSRC:0__\r\n"))
	})
	ws := wsConnect(t, f, ConnectOptions{})

	res, err := ws.Execute(context.Background(), "echo real", 5*time.Second)
	if err != nil {
		t.Fatalf("Execute = %v", err)
	}
	if strings.Contains(res.Output, "__rc") || strings.Contains(res.Output, "echo") || strings.Contains(res.Output, "__JMSDONE") {
		t.Fatalf("output = %q, want it free of echo fragments", res.Output)
	}
	if !strings.Contains(res.Output, "REAL-OUTPUT") {
		t.Fatalf("output = %q, want the real output", res.Output)
	}
}

func TestWSNormalCloseIsNotAnInteractiveError(t *testing.T) {
	for _, code := range []int{
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	} {
		if !isNormalWSClose(&websocket.CloseError{Code: code}) {
			t.Errorf("close code %d was treated as an interactive error", code)
		}
	}
	if isNormalWSClose(errors.New("connection reset by peer")) {
		t.Fatal("an unrelated transport error was treated as a normal close")
	}
}

func TestWSBackendIsWS(t *testing.T) {
	f := newFakeKoKo(t)
	ws := wsConnect(t, f, ConnectOptions{})
	if ws.Backend() != BackendWS {
		t.Fatalf("Backend = %v, want ws", ws.Backend())
	}
}

func TestWSTerminalURLSchemeAndToken(t *testing.T) {
	if got := wsTerminalURL("https://jms.example.com", "jms.example.com", "tok"); !strings.HasPrefix(got, "wss://") {
		t.Errorf("https base must yield wss, got %q", got)
	}
	if got := wsTerminalURL("http://jms.example.com", "jms.example.com", "tok"); !strings.HasPrefix(got, "ws://") {
		t.Errorf("http base must yield ws, got %q", got)
	}
	if got := wsTerminalURL("http://jms.example.com", "jms.example.com", "tok"); !strings.Contains(got, "token=tok") {
		t.Errorf("URL lacks the token: %q", got)
	}
}

// TestWSContextCancellationIsPrompt guards the fix that makes cancellation
// interrupt a blocking read instead of waiting out the read deadline.
func TestWSContextCancellationIsPrompt(t *testing.T) {
	f := newFakeKoKo(t) // no frames: Execute can only exit via ctx
	ws := wsConnect(t, f, ConnectOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := ws.Execute(ctx, "slow", 0) // no timeout: ctx is the only exit
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Execute = nil error, want the cancellation to surface")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Execute took %s, want a prompt return on cancellation", elapsed)
	}
}

// TestWSDispatchThroughFrozenConnect proves the frozen connect() now
// dispatches to a real WebSocket backend rather than the placeholder.
func TestWSDispatchThroughFrozenConnect(t *testing.T) {
	f := newFakeKoKo(t)
	sess := f.session(t)
	term, err := connect(context.Background(), sess, testAsset(), ConnectOptions{Backend: BackendWS})
	if err != nil {
		t.Fatalf("connect(BackendWS) = %v", err)
	}
	if term.Backend() != BackendWS {
		t.Fatalf("Backend = %v", term.Backend())
	}
	_ = term.Close()
}
