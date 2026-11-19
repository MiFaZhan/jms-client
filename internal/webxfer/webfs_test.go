package webxfer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
)

// fakeKoKo is a hermetic stand-in for KoKo's /koko/ws/sftp/ endpoint. It
// answers the connection-token API (setting the session cookie the way a
// real login would) and speaks the v4.x file-manager frame protocol.
type fakeKoKo struct {
	*httptest.Server

	mu      sync.Mutex
	files   map[string][]byte // uploaded content, by remote path
	uploadN int               // chunk frames received
	cookie  string            // the jms_sessionid the client presented
	token   string            // the token query parameter
	subs    []string          // subprotocols the client offered

	// failChunk makes every chunk upload answer with an error frame.
	failChunk string
	// dropAfterChunks closes the connection after N chunk frames.
	dropAfterChunks int
	// failDownload sends an error frame instead of the file.
	failDownload string
}

func newFakeKoKo(t *testing.T) *fakeKoKo {
	t.Helper()
	f := &fakeKoKo{files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/authentication/connection-token/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The real login leaves jms_sessionid in the jar; the client must
		// present it on the WebSocket handshake.
		http.SetCookie(w, &http.Cookie{Name: auth.CookieSession, Value: "sid-test", Path: "/"})
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "tok-1", "value": "secret"})
	})
	mux.HandleFunc(pathWSSftp, func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie(auth.CookieSession); err == nil {
			f.cookie = ck.Value
		}
		f.token = r.URL.Query().Get("token")
		f.subs = websocket.Subprotocols(r)

		up := websocket.Upgrader{Subprotocols: []string{"JMS-KOKO"}}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		f.serve(conn)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// serve runs the frame protocol until the connection dies.
func (f *fakeKoKo) serve(conn *websocket.Conn) {
	_ = conn.WriteJSON(message{ID: "ws-uuid", Type: typeConnect, Data: "{}"})
	for {
		var msg message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case typePing:
			_ = conn.WriteJSON(message{ID: msg.ID, Type: typePong})
			continue
		case typeSFTPData:
			// fall through to the command dispatch
		default:
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(msg.Data), &req); err != nil {
			_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPData, Err: "bad request"})
			continue
		}
		switch msg.Cmd {
		case cmdUpload:
			if req.Merge {
				f.mu.Lock()
				f.uploadN++
				n := f.uploadN
				f.mu.Unlock()
				if f.dropAfterChunks > 0 && n >= f.dropAfterChunks {
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(4000, "boom"), time.Now().Add(time.Second))
					return
				}
				_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPData, Data: "ok"})
				continue
			}
			if f.failChunk != "" {
				_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPData, Err: f.failChunk})
				continue
			}
			f.mu.Lock()
			buf := f.files[req.Path]
			if int(req.OffSet)+len(msg.Raw) > len(buf) {
				grown := make([]byte, int(req.OffSet)+len(msg.Raw))
				copy(grown, buf)
				buf = grown
			}
			copy(buf[int(req.OffSet):], msg.Raw)
			f.files[req.Path] = buf
			f.uploadN++
			n := f.uploadN
			f.mu.Unlock()
			if f.dropAfterChunks > 0 && n >= f.dropAfterChunks {
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(4000, "boom"), time.Now().Add(time.Second))
				return
			}
			_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPData, Data: req.Path})
		case cmdDownload:
			if f.failDownload != "" {
				_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPData, Err: f.failDownload})
				continue
			}
			f.mu.Lock()
			buf := append([]byte(nil), f.files[req.Path]...)
			f.mu.Unlock()
			for off := 0; off < len(buf); off += MaxChunk {
				end := off + MaxChunk
				if end > len(buf) {
					end = len(buf)
				}
				_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPBinary, Raw: buf[off:end]})
			}
			_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPData,
				Data: filepath.Base(req.Path)})
		default:
			_ = conn.WriteJSON(message{ID: msg.ID, Cmd: msg.Cmd, Type: typeSFTPData, Err: "Unknown command"})
		}
	}
}

func (f *fakeKoKo) uploaded(path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.files[path]...)
}

// testSession builds a session bound to the fake server, primed with the
// session cookie the token endpoint sets.
func testSession(t *testing.T, f *fakeKoKo) *auth.Session {
	t.Helper()
	// One throwaway request primes the client's cookie jar, exactly the
	// way the real login flow leaves jms_sessionid behind.
	c := api.New(f.URL)
	sess := &auth.Session{
		Server:  &config.ServerConfig{Name: "test", Internal: f.URL, Username: "tester"},
		Client:  c,
		BaseURL: f.URL,
	}
	return sess
}

var testAsset = assets.Info{ID: "asset-1", Name: "web-01", Account: "@USER", Protocol: "ssh"}

func mustDial(t *testing.T, f *fakeKoKo) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, testSession(t, f), testAsset)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDialSendsCookieSubprotocolAndToken(t *testing.T) {
	f := newFakeKoKo(t)
	c := mustDial(t, f)
	_ = c

	if f.cookie != "sid-test" {
		t.Fatalf("client did not present jms_sessionid: %q", f.cookie)
	}
	if f.token != "tok-1" {
		t.Fatalf("token query = %q, want tok-1", f.token)
	}
	sawKoko := false
	for _, s := range f.subs {
		if s == "JMS-KOKO" {
			sawKoko = true
		}
	}
	if !sawKoko {
		t.Fatalf("client did not offer the JMS-KOKO subprotocol: %v", f.subs)
	}
}

func TestUploadMultiChunkRoundTrip(t *testing.T) {
	f := newFakeKoKo(t)
	c := mustDial(t, f)

	// 2.5 MiB forces three 1 MiB chunks plus a partial one.
	payload := make([]byte, 2*DefaultChunk+DefaultChunk/2)
	for i := range payload {
		payload[i] = byte(i)
	}
	local := filepath.Join(t.TempDir(), "up.bin")
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sent, err := c.Upload(ctx, local, "/tmp/up.bin", DefaultChunk)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if sent != int64(len(payload)) {
		t.Fatalf("sent %d, want %d", sent, len(payload))
	}
	got := f.uploaded("/tmp/up.bin")
	if string(got) != string(payload) {
		t.Fatalf("server stored %d bytes that differ from the payload", len(got))
	}
}

func TestUploadEmptyFileSendsMergeOnly(t *testing.T) {
	f := newFakeKoKo(t)
	c := mustDial(t, f)

	local := filepath.Join(t.TempDir(), "empty.bin")
	if err := os.WriteFile(local, nil, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Upload(ctx, local, "/tmp/empty.bin", DefaultChunk); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := f.uploaded("/tmp/empty.bin"); len(got) != 0 {
		t.Fatalf("stored %d bytes for an empty file", len(got))
	}
}

func TestUploadServerErrorFailsTheTransfer(t *testing.T) {
	f := newFakeKoKo(t)
	f.failChunk = "permission denied"
	c := mustDial(t, f)

	local := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(local, []byte("data"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Upload(ctx, local, "/tmp/x.bin", DefaultChunk)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("upload error = %v, want the server's error text", err)
	}
}

func TestUploadConnectionDropFailsTheTransfer(t *testing.T) {
	f := newFakeKoKo(t)
	f.dropAfterChunks = 2 // die on the second chunk
	c := mustDial(t, f)

	payload := make([]byte, 2*DefaultChunk)
	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.Upload(ctx, local, "/tmp/big.bin", DefaultChunk)
	if err == nil {
		t.Fatal("upload succeeded despite the dropped connection")
	}
}

func TestDownloadStreamsFramesToFile(t *testing.T) {
	f := newFakeKoKo(t)
	f.files["/tmp/down.bin"] = []byte(strings.Repeat("0123456789", 500_000)) // ~4.8 MiB
	c := mustDial(t, f)

	dst := filepath.Join(t.TempDir(), "nested", "down.bin")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := c.Download(ctx, "/tmp/down.bin", dst)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if want := int64(len(f.files["/tmp/down.bin"])); got != want {
		t.Fatalf("downloaded %d bytes, want %d", got, want)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != string(f.files["/tmp/down.bin"]) {
		t.Fatal("downloaded content differs")
	}
}

func TestDownloadServerErrorFailsTheTransfer(t *testing.T) {
	f := newFakeKoKo(t)
	f.failDownload = "sftp_path_not_found"
	c := mustDial(t, f)

	dst := filepath.Join(t.TempDir(), "out.bin")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Download(ctx, "/tmp/missing.bin", dst)
	if err == nil || !strings.Contains(err.Error(), "sftp_path_not_found") {
		t.Fatalf("download error = %v, want the server's error text", err)
	}
}

func TestUploadRejectsOversizedChunkConfig(t *testing.T) {
	f := newFakeKoKo(t)
	c := mustDial(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Upload(ctx, "whatever-local", "/tmp/x", MaxChunk+1)
	if err == nil || !strings.Contains(err.Error(), "protocol maximum") {
		t.Fatalf("error = %v, want the protocol-maximum rejection", err)
	}
}

func TestWsSftpURLSchemeAndQuery(t *testing.T) {
	cases := []struct {
		base, want string
	}{
		{"https://jms.example.com", "wss://jms.example.com/koko/ws/sftp/?"},
		{"http://10.0.0.1:8080", "ws://10.0.0.1:8080/koko/ws/sftp/?"},
	}
	for _, tc := range cases {
		got := wsSftpURL(tc.base, "tok-9")
		if !strings.HasPrefix(got, tc.want) {
			t.Fatalf("wsSftpURL(%q) = %q, want prefix %q", tc.base, got, tc.want)
		}
		if !strings.Contains(got, "token=tok-9") {
			t.Fatalf("wsSftpURL(%q) missing the token parameter: %q", tc.base, got)
		}
	}
}

func TestHostPortToleratesSchemelessBaseURL(t *testing.T) {
	if got := hostPort("jms.example.com"); got != "jms.example.com" {
		t.Fatalf("hostPort schemeless = %q", got)
	}
	if got := hostPort("https://h:443/x"); got != "h:443" {
		t.Fatalf("hostPort full URL = %q", got)
	}
	if got := hostPort("not a url"); got != "" {
		t.Fatalf("hostPort garbage = %q, want empty", got)
	}
}

func TestChunkConstantsStayWithinTheProtocolCeiling(t *testing.T) {
	if DefaultChunk > MaxChunk {
		t.Fatalf("DefaultChunk %d must not exceed MaxChunk %d", DefaultChunk, MaxChunk)
	}
}
