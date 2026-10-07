package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/config"
)

// fakeServer drives the dual login with a scripted handler and records what
// the client actually sent, so tests can assert on protocol shape.
type fakeServer struct {
	t *testing.T

	mu       sync.Mutex
	calls    []call
	handler  func(fs *fakeServer, w http.ResponseWriter, r *http.Request) bool
	srv      *httptest.Server
	failFast bool
}

type call struct {
	Method string
	Path   string
	Form   url.Values
	Body   string
	Cookie string
}

func newFakeServer(t *testing.T) *fakeServer {
	fs := &fakeServer{t: t}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{Method: r.Method, Path: r.URL.Path}
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			c.Form = r.PostForm
			c.Body = readAll(r)
		}
		for _, ck := range r.Cookies() {
			if ck.Name == CookieCSRF {
				c.Cookie = ck.Value
			}
		}
		fs.mu.Lock()
		fs.calls = append(fs.calls, c)
		h := fs.handler
		fs.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if h != nil && h(fs, w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeServer) setHandler(h func(*fakeServer, http.ResponseWriter, *http.Request) bool) {
	fs.mu.Lock()
	fs.handler = h
	fs.mu.Unlock()
}

func (fs *fakeServer) URL() string { return fs.srv.URL }

func (fs *fakeServer) n() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.calls)
}

// path returns the path of the nth call (0-based).
func (fs *fakeServer) path(n int) string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if n >= len(fs.calls) {
		return ""
	}
	return fs.calls[n].Path
}

// form returns the parsed form body of the nth call.
func (fs *fakeServer) form(n int) url.Values {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if n >= len(fs.calls) {
		return url.Values{}
	}
	return fs.calls[n].Form
}

func (fs *fakeServer) body(n int) string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if n >= len(fs.calls) {
		return ""
	}
	return fs.calls[n].Body
}

func (fs *fakeServer) csrfAt(n int) string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if n >= len(fs.calls) {
		return ""
	}
	return fs.calls[n].Cookie
}

// seedCSRFCookie mimics Django: the csrf cookie is issued on the login-page
// GET, before the form POST.
func seedCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieCSRF, Value: "csrf-abc", Path: "/"})
}

// grantSessionCookie mimics a successful form login: the session cookie is
// issued on the POST response.
func grantSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieSession, Value: "sess-123", Path: "/"})
}

// writeJSON is a small helper for scripted bodies.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func readAll(r *http.Request) string {
	buf := new(strings.Builder)
	if r.Body == nil {
		return ""
	}
	b := make([]byte, 4096)
	for {
		n, err := r.Body.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	return buf.String()
}

// newSrv is the config.ServerConfig every test uses.
func newSrv() *config.ServerConfig {
	return &config.ServerConfig{Name: "bastion", Username: "testuser"}
}

func TestLoginSucceedsWithBothTokens(t *testing.T) {
	fs := newFakeServer(t)
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case PathAPILogin:
			writeJSON(w, http.StatusOK, `{"token":"bearer-1"}`)
			return true
		case PathFormLogin:
			// GET seeds csrf, POST grants the session cookie.
			if r.Method == http.MethodGet {
				seedCSRFCookie(w)
			} else {
				grantSessionCookie(w)
			}
			return true
		}
		return false
	})

	sess, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw"}, nil)
	if err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}
	if sess == nil {
		t.Fatal("Login() = nil session")
	}
	if got := sess.Bearer(); got != "bearer-1" {
		t.Fatalf("Bearer() = %q, want %q", got, "bearer-1")
	}
	if got := sess.SessionID(); got != "sess-123" {
		t.Fatalf("SessionID() = %q, want %q", got, "sess-123")
	}
	if got := sess.CSRF(); got != "csrf-abc" {
		t.Fatalf("CSRF() = %q, want %q", got, "csrf-abc")
	}
	if fs.n() != 3 {
		t.Fatalf("calls = %d, want 3 (api login + form GET + form POST)", fs.n())
	}
}

func TestLoginFormPostCarriesCSRFAndCredentials(t *testing.T) {
	fs := newFakeServer(t)
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == PathAPILogin {
			writeJSON(w, http.StatusOK, `{"token":"t"}`)
			return true
		}
		if r.URL.Path == PathFormLogin {
			// Django: GET seeds csrf, POST grants the session.
			if r.Method == http.MethodGet {
				seedCSRFCookie(w)
			} else {
				grantSessionCookie(w)
			}
			return true
		}
		return false
	})

	if _, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "hunter2"}, nil); err != nil {
		t.Fatalf("Login() = %v", err)
	}

	// calls: 0 = api login, 1 = form GET, 2 = form POST
	if got := fs.path(0); got != PathAPILogin {
		t.Fatalf("call 0 = %q, want %q", got, PathAPILogin)
	}
	if got := fs.path(1); got != PathFormLogin {
		t.Fatalf("call 1 = %q, want the form GET %q", got, PathFormLogin)
	}
	if got := fs.path(2); got != PathFormLogin {
		t.Fatalf("call 2 = %q, want the form POST %q", got, PathFormLogin)
	}
	form := fs.form(2)
	if got := form.Get("username"); got != "testuser" {
		t.Fatalf("form username = %q, want %q", got, "testuser")
	}
	if got := form.Get("password"); got != "hunter2" {
		t.Fatalf("form password = %q, want the credential that was passed in", got)
	}
	if got := form.Get("csrfmiddlewaretoken"); got != "csrf-abc" {
		t.Fatalf("csrfmiddlewaretoken = %q, want the seeded %q", got, "csrf-abc")
	}
	if got := fs.csrfAt(2); got != "csrf-abc" {
		t.Fatalf("cookie header on POST = %q, want the seeded csrf cookie", got)
	}
}

func TestLoginMFACodeMatchesTOTPSecret(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP" // classic test secret, valid base32

	fs := newFakeServer(t)
	mfaDone := false
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case PathAPILogin:
			if !mfaDone {
				writeJSON(w, http.StatusOK, `{"code":"mfa_required"}`)
				return true
			}
			writeJSON(w, http.StatusOK, `{"token":"after-mfa"}`)
			return true
		case PathFormLogin:
			if r.Method == http.MethodGet {
				seedCSRFCookie(w)
			} else {
				grantSessionCookie(w)
			}
			return true
		case PathMFAChallenge:
			mfaDone = true
			var posted map[string]string
			if err := jsonUnmarshal([]byte(fs.body(fs.n()-1)), &posted); err != nil {
				t.Errorf("challenge body: %v", err)
				return true
			}
			if posted["type"] != "otp" {
				t.Errorf("challenge type = %q, want \"otp\"", posted["type"])
			}
			want, _ := GenerateCode(secret)
			if posted["code"] != want {
				t.Errorf("challenge code = %q, want the current TOTP %q", posted["code"], want)
			}
			writeJSON(w, http.StatusOK, `{}`)
			return true
		}
		return false
	})

	sess, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw", OTPSecret: secret}, nil)
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if got := sess.Bearer(); got != "after-mfa" {
		t.Fatalf("Bearer() = %q, want the post-MFA token", got)
	}
	if fs.n() != 5 {
		t.Fatalf("calls = %d, want 5 (login, challenge, login retry, form GET, form POST)", fs.n())
	}
}

func TestLoginMFADetectedViaErrorFieldName(t *testing.T) {
	fs := newFakeServer(t)
	mfaDone := false
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case PathAPILogin:
			if !mfaDone {
				// The other field name some JumpServer versions use.
				writeJSON(w, http.StatusOK, `{"error":"mfa_required"}`)
				return true
			}
			writeJSON(w, http.StatusOK, `{"token":"ok"}`)
			return true
		case PathFormLogin:
			if r.Method == http.MethodGet {
				seedCSRFCookie(w)
			} else {
				grantSessionCookie(w)
			}
			return true
		case PathMFAChallenge:
			mfaDone = true
			writeJSON(w, http.StatusOK, `{}`)
			return true
		}
		return false
	})

	sess, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw", OTPSecret: "JBSWY3DPEHPK3PXP"}, nil)
	if err != nil {
		t.Fatalf("Login() = %v (the `error` field must also trigger MFA)", err)
	}
	if got := sess.Bearer(); got != "ok" {
		t.Fatalf("Bearer() = %q", got)
	}
}

func TestLoginMFAWithoutSecretOrPromptFails(t *testing.T) {
	fs := newFakeServer(t)
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusOK, `{"code":"mfa_required"}`)
		return true
	})

	_, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw"}, nil)
	if !errors.Is(err, ErrMFANotProvided) {
		t.Fatalf("Login() = %v, want ErrMFANotProvided", err)
	}
}

func TestLoginMFAUsesPromptWhenNoSecret(t *testing.T) {
	fs := newFakeServer(t)
	var gotCode string
	mfaDone := false
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case PathAPILogin:
			if !mfaDone {
				writeJSON(w, http.StatusOK, `{"code":"mfa_required"}`)
				return true
			}
			writeJSON(w, http.StatusOK, `{"token":"ok"}`)
			return true
		case PathFormLogin:
			if r.Method == http.MethodGet {
				seedCSRFCookie(w)
			} else {
				grantSessionCookie(w)
			}
			return true
		case PathMFAChallenge:
			mfaDone = true
			gotCode = fieldFrom(fs.body(fs.n()-1), "code")
			writeJSON(w, http.StatusOK, `{}`)
			return true
		}
		return false
	})

	prompt := func() (string, error) { return "123456", nil }
	sess, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw"}, prompt)
	if err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if gotCode != "123456" {
		t.Fatalf("challenge code = %q, want the prompted \"123456\"", gotCode)
	}
	if got := sess.Bearer(); got != "ok" {
		t.Fatalf("Bearer() = %q", got)
	}
}

func TestLoginInvalidSecretFallsBackToPrompt(t *testing.T) {
	fs := newFakeServer(t)
	var gotCode string
	mfaDone := false
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case PathAPILogin:
			if !mfaDone {
				writeJSON(w, http.StatusOK, `{"code":"mfa_required"}`)
				return true
			}
			writeJSON(w, http.StatusOK, `{"token":"ok"}`)
			return true
		case PathFormLogin:
			if r.Method == http.MethodGet {
				seedCSRFCookie(w)
			} else {
				grantSessionCookie(w)
			}
			return true
		case PathMFAChallenge:
			mfaDone = true
			gotCode = fieldFrom(fs.body(fs.n()-1), "code")
			writeJSON(w, http.StatusOK, `{}`)
			return true
		}
		return false
	})

	prompt := func() (string, error) { return "654321", nil }
	// "not-base32!" is not valid base32, so the secret is unusable and the
	// prompt must take over instead of the login failing.
	sess, err := Login(context.Background(), newSrv(), fs.URL(),
		Credentials{Password: "pw", OTPSecret: "not-base32!!"}, prompt)
	if err != nil {
		t.Fatalf("Login() = %v (an unusable secret must fall back to the prompt)", err)
	}
	if gotCode != "654321" {
		t.Fatalf("challenge code = %q, want the prompted value", gotCode)
	}
	_ = sess
}

func TestLoginBadCredentialsIsAuthErrorAndSkipsFormLogin(t *testing.T) {
	fs := newFakeServer(t)
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusUnauthorized, `{"detail":"Invalid username or password."}`)
		return true
	})

	_, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "wrong"}, nil)
	authErr, ok := api.AsAuthError(err)
	if !ok {
		t.Fatalf("Login() = %T(%v), want *api.AuthError", err, err)
	}
	if authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", authErr.StatusCode)
	}
	if !strings.Contains(authErr.Body, "Invalid username or password") {
		t.Fatalf("Body = %q, want the server's detail", authErr.Body)
	}
	if fs.n() != 1 {
		t.Fatalf("calls = %d, want 1 (a rejected credential must not reach the form login)", fs.n())
	}
}

func TestLoginTransportFailureStaysNetworkClass(t *testing.T) {
	// Nothing listens on this port. A transport failure during login must
	// stay network-class (api.APIError with StatusCode 0) so the endpoint
	// policy can fail over to the next address - DESIGN.md 4.7 pt 2 and
	// 11.9 (TCP reachable, service behind it broken). Only a credential
	// rejection is terminal.
	_, err := Login(context.Background(), newSrv(), "http://127.0.0.1:1/", Credentials{Password: "pw"}, nil)
	apiErr, ok := api.AsAPIError(err)
	if !ok {
		t.Fatalf("transport failure during login = %T(%v), want *api.APIError", err, err)
	}
	if apiErr.StatusCode != 0 {
		t.Fatalf("StatusCode = %d, want 0 for a transport failure", apiErr.StatusCode)
	}
	if _, isAuth := api.AsAuthError(err); isAuth {
		t.Fatal("a transport failure must NOT be classified as a credential rejection")
	}
}

func TestLoginServerErrorStaysNetworkClass(t *testing.T) {
	// A 502 from the login endpoint is the server being broken, not the
	// credential being wrong: it must stay network-class so failover can
	// try the other address.
	fs := newFakeServer(t)
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusBadGateway, `<html>502 Bad Gateway</html>`)
		return true
	})
	_, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw"}, nil)
	apiErr, ok := api.AsAPIError(err)
	if !ok {
		t.Fatalf("502 during login = %T(%v), want *api.APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d, want 502", apiErr.StatusCode)
	}
}

func TestLoginForbiddenIsCredentialRejection(t *testing.T) {
	// 403 is the server refusing the credential: terminal, and the form
	// login must not be attempted.
	fs := newFakeServer(t)
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusForbidden, `{"detail":"forbidden"}`)
		return true
	})
	_, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw"}, nil)
	authErr, ok := api.AsAuthError(err)
	if !ok {
		t.Fatalf("403 during login = %T(%v), want *api.AuthError", err, err)
	}
	if authErr.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d, want 403", authErr.StatusCode)
	}
}

func TestLoginMissingSessionCookie(t *testing.T) {
	fs := newFakeServer(t)
	fs.setHandler(func(_ *fakeServer, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == PathAPILogin {
			writeJSON(w, http.StatusOK, `{"token":"t"}`)
			return true
		}
		// Form login answers 200 but never sets jms_sessionid.
		return true
	})

	_, err := Login(context.Background(), newSrv(), fs.URL(), Credentials{Password: "pw"}, nil)
	if !errors.Is(err, ErrMissingSessionCookie) {
		t.Fatalf("Login() = %v, want ErrMissingSessionCookie", err)
	}
}

func TestValidateSecretAcceptsBase32AndRejectsGarbage(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{name: "classic 16 chars", secret: "JBSWY3DPEHPK3PXP"},
		{name: "with padding", secret: "JBSWY3DP"},
		{name: "lowercase", secret: "jbswy3dpehpk3pxp"},
		{name: "empty", secret: "", wantErr: true},
		{name: "one char", secret: "A", wantErr: true},
		{name: "not base32", secret: "not-base32!!", wantErr: true},
		{name: "ambiguous chars", secret: "0O1I", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSecret(tt.secret)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("ValidateSecret(%q) = %v, wantErr %v", tt.secret, err, tt.wantErr)
			}
		})
	}
}

func TestGenerateCodeIsSixDigits(t *testing.T) {
	code, err := GenerateCode("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatalf("GenerateCode() = %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("code = %q, want 6 digits", code)
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			t.Fatalf("code = %q, want digits only", code)
		}
	}
}

func TestSessionNilReceiversAreSafe(t *testing.T) {
	var s *Session
	if s.SessionID() != "" {
		t.Fatal("nil SessionID() != \"\"")
	}
	if s.CSRF() != "" {
		t.Fatal("nil CSRF() != \"\"")
	}
}

// transportLogin drives a real Login against a dead port so the test can
// assert on the error class.
func transportLogin(t *testing.T) error {
	t.Helper()
	_, err := Login(context.Background(), newSrv(), "http://127.0.0.1:1/", Credentials{Password: "pw"}, nil)
	return err
}

// fieldFrom pulls one key out of a flat JSON object body.
func fieldFrom(body, key string) string {
	var m map[string]string
	if err := jsonUnmarshal([]byte(body), &m); err != nil {
		return ""
	}
	return m[key]
}

// jsonUnmarshal is a thin wrapper so test helpers stay one indirection away
// from encoding/json.
func jsonUnmarshal(data []byte, out any) error {
	return json.Unmarshal(data, out)
}
