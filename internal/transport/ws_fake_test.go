package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
)

// fakeKoKo is an in-process stand-in for KoKo: one HTTP server serving both
// the connection-token endpoint and the WebSocket terminal upgrade.
//
// It upgrades only when the jms_sessionid cookie and the JMS-KOKO
// subprotocol are present — the two handshake requirements DESIGN.md §3.2
// states — records every frame it receives, answers a client PING with a
// PONG, and lets a test script frames to send.
type fakeKoKo struct {
	t *testing.T

	mu               sync.Mutex
	received         []wsMessage
	upgraded         bool
	sawCookie        bool
	sawProto         bool
	rawURL           string
	tokenRequestBody string

	connectID string
	onFrame   func(fs *fakeKoKo, msg wsMessage)
	srv       *httptest.Server
	conn      *websocket.Conn
}

func newFakeKoKo(t *testing.T) *fakeKoKo {
	f := &fakeKoKo{t: t, connectID: "3f7b1c2e-0000-4a11-9c33-abcdef012345"}
	mux := http.NewServeMux()

	// The connection-token endpoint records the body the client posted, so
	// the protocol and connect_method can be asserted.
	mux.HandleFunc(PathConnectionToken, func(w http.ResponseWriter, r *http.Request) {
		// The client posts JSON (matching the Python reference), so decode
		// the body rather than reading form values.
		var body struct {
			Asset         string `json:"asset"`
			Account       string `json:"account"`
			Protocol      string `json:"protocol"`
			ConnectMethod string `json:"connect_method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.tokenRequestBody = strings.Join([]string{
			body.Asset, body.Account, body.Protocol, body.ConnectMethod,
		}, "|")
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: auth.CookieSession, Value: "sess-test", Path: "/"})
		_, _ = w.Write([]byte(`{"id":"tok-42","value":"tok-value"}`))
	})
	// Any other path seeds the session cookie, as the form login does.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: auth.CookieSession, Value: "sess-test", Path: "/"})
		_, _ = w.Write([]byte(`{}`))
	})
	// The WebSocket terminal upgrade, on the frozen path.
	mux.HandleFunc(PathWSTerminal, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.rawURL = r.URL.String()
		f.sawCookie = strings.Contains(r.Header.Get("Cookie"), auth.CookieSession+"=")
		f.sawProto = strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), WSSubprotocol)
		upgrade := f.sawCookie && f.sawProto
		if !upgrade {
			f.mu.Unlock()
			http.Error(w, "missing session cookie or subprotocol", http.StatusUnauthorized)
			return
		}
		conn, err := (&websocket.Upgrader{Subprotocols: []string{WSSubprotocol}}).Upgrade(w, r, nil)
		if err != nil {
			f.mu.Unlock()
			return
		}
		f.conn = conn
		f.upgraded = true
		connectID := f.connectID
		f.mu.Unlock()
		defer conn.Close()

		// KoKo pushes a CONNECT frame immediately after the upgrade and the
		// client waits for it before sending TERMINAL_INIT. The frame is a
		// bare {"id": "<uuid>"} — no type field — which is what the Python
		// reference reads, and it may arrive as a binary frame, so this fake
		// sends it as binary to keep the client honest about not requiring
		// text.
		connect, _ := json.Marshal(map[string]string{"id": connectID})
		if err := conn.WriteMessage(websocket.TextMessage, connect); err != nil {
			return
		}

		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg wsMessage
			_ = json.Unmarshal(payload, &msg)
			f.mu.Lock()
			f.received = append(f.received, msg)
			handler := f.onFrame
			f.mu.Unlock()

			if handler != nil {
				handler(f, msg)
			}
			if msg.Type == msgPing {
				pong, _ := json.Marshal(wsMessage{Type: msgPong})
				_ = conn.WriteMessage(websocket.TextMessage, pong)
			}
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKoKo) requests() []wsMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wsMessage(nil), f.received...)
}

func (f *fakeKoKo) lastURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rawURL
}

func (f *fakeKoKo) handshakeOK() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sawCookie && f.sawProto
}

// tokenBody returns the pipe-joined body of the connection-token request:
// asset|account|protocol|connect_method.
func (f *fakeKoKo) tokenBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenRequestBody
}

// onFrames installs a handler invoked for every received frame.
func (f *fakeKoKo) onFrames(fn func(fs *fakeKoKo, msg wsMessage)) {
	f.mu.Lock()
	f.onFrame = fn
	f.mu.Unlock()
}

// echoCommand mimics the remote shell: it echoes the wrapped command line
// (which contains the done marker once), then writes the command's output,
// then prints the marker again followed by the exit status. The marker
// appearing twice is what delimits the output, exactly as a real PTY
// produces it and as the Python reference parses it.
func (f *fakeKoKo) echoCommand(output string, exitCode int) {
	f.onFrames(func(fs *fakeKoKo, msg wsMessage) {
		if msg.Type != msgTerminalData {
			return
		}
		// The client wrapped the command; recover the marker from the echo.
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
		echo := msg.Data + "\r\n"
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(echo))
		if output != "" {
			_ = conn.WriteMessage(websocket.BinaryMessage, []byte(output+"\r\n"))
		}
		tail := fmt.Sprintf("%s\r\n__JMSRC:%d__\r\n", marker, exitCode)
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte(tail))
	})
}

// extractMarker recovers the done marker from a wrapped command line.
func extractMarker(cmd string) string {
	const prefix = "__JMSDONE_"
	i := strings.Index(cmd, prefix)
	if i < 0 {
		return ""
	}
	rest := cmd[i:]
	// The marker is __JMSDONE_<digits>__: skip the name, then take the
	// digits and the trailing double underscore.
	body := rest[len(prefix):]
	if j := strings.Index(body, "__"); j >= 0 {
		return prefix + body[:j+2]
	}
	return rest
}

// session builds a real auth.Session whose client's cookie jar already
// carries the jms_sessionid cookie, the way auth.Login leaves it, with the
// base URL pointing at the fake so both the token POST and the handshake
// land here.
func (f *fakeKoKo) session(t *testing.T) *auth.Session {
	t.Helper()
	sess := &auth.Session{
		Server:  &config.ServerConfig{Name: "bastion", Username: "tester"},
		BaseURL: f.srv.URL,
	}
	sess.Client = api.New(f.srv.URL)
	// Seed the session cookie the way auth.Login's form step would.
	if err := sess.Client.Get(context.Background(), "/", nil, nil); err != nil {
		t.Fatalf("seed session cookie: %v", err)
	}
	return sess
}

// testAsset is the resolved asset every ws test connects with.
func testAsset() assets.Info {
	return assets.Info{
		ID: "asset-uuid", Name: "web-01", Address: "10.0.0.1",
		Account: "@USER", Protocol: "ssh",
	}
}

// testServerConfig is the server entry the ws tests use.
func testServerConfig() *config.ServerConfig {
	return &config.ServerConfig{Name: "bastion", Username: "tester"}
}

// wsConnect connects a real terminal to the fake.
func wsConnect(t *testing.T, f *fakeKoKo, opts ConnectOptions) *wsTerminal {
	t.Helper()
	term, err := connectWS(context.Background(), f.session(t), testAsset(), opts)
	if err != nil {
		t.Fatalf("connectWS: %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })
	ws, ok := term.(*wsTerminal)
	if !ok {
		t.Fatalf("connectWS returned %T, want *wsTerminal", term)
	}
	return ws
}
