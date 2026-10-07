package connpool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
)

// loginServer is a scripted JumpServer that answers the dual login, so the
// pool can be driven through the real auth.Login rather than a stub of it.
// It counts the API logins, which is the observable cost the pool exists to
// remove.
type loginServer struct {
	*httptest.Server

	mu        sync.Mutex
	apiLogins int
	// reject, when non-zero, is the status the API login answers with.
	reject int

	// entered is signalled once per API login the server handles, and gate
	// holds each of them open until the test closes it. Together they let a
	// test observe a login that is in flight.
	entered chan struct{}
	gate    chan struct{}
}

func newLoginServer(t *testing.T) *loginServer {
	t.Helper()
	ls := &loginServer{entered: make(chan struct{}, 8)}
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case auth.PathAPILogin:
			ls.mu.Lock()
			ls.apiLogins++
			reject := ls.reject
			gate := ls.gate
			ls.mu.Unlock()
			select {
			case ls.entered <- struct{}{}:
			default:
			}
			if gate != nil {
				<-gate
			}
			if reject != 0 {
				w.WriteHeader(reject)
				_, _ = w.Write([]byte(`{"detail":"invalid credentials"}`))
				return
			}
			_, _ = w.Write([]byte(`{"token":"bearer-token"}`))
		case auth.PathFormLogin:
			if r.Method == http.MethodGet {
				http.SetCookie(w, &http.Cookie{Name: auth.CookieCSRF, Value: "csrf-1", Path: "/"})
			} else {
				http.SetCookie(w, &http.Cookie{Name: auth.CookieSession, Value: "sess-1", Path: "/"})
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ls.Close)
	return ls
}

func (ls *loginServer) logins() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.apiLogins
}

func (ls *loginServer) setReject(status int) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.reject = status
}

// hold opens a gate that keeps every API login in flight until the returned
// function is called, so a test can observe a login that has not finished.
func (ls *loginServer) hold() func() {
	gate := make(chan struct{})
	ls.mu.Lock()
	ls.gate = gate
	ls.mu.Unlock()
	return func() {
		ls.mu.Lock()
		ls.gate = nil
		ls.mu.Unlock()
		close(gate)
	}
}

// waitEntered blocks until the server has started one API login.
func (ls *loginServer) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-ls.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the login server to be reached")
	}
}

// creds is the credential the session tests present. It is obviously fake
// and the pool must never echo it back in an error.
func creds() auth.Credentials { return auth.Credentials{Password: "pw-not-real"} }

// TestSessionPoolGetLogsInOnceForOneAlias drives two Gets through the real
// auth.Login and asserts the server saw one API login: the second call is
// answered from the pool.
func TestSessionPoolGetLogsInOnceForOneAlias(t *testing.T) {
	ls := newLoginServer(t)
	p := NewSessionPool()
	t.Cleanup(func() { _ = p.Close() })
	srv := testServer()

	first, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
	if err != nil {
		t.Fatalf("first Get() = %v, want nil", err)
	}
	second, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
	if err != nil {
		t.Fatalf("second Get() = %v, want nil", err)
	}

	if first != second {
		t.Fatal("second Get() returned a different session; the pool did not reuse the first")
	}
	if got := ls.logins(); got != 1 {
		t.Fatalf("API logins = %d, want 1", got)
	}
	if got := first.Bearer(); got != "bearer-token" {
		t.Fatalf("Bearer() = %q, want the token the login server issued", got)
	}
}

// TestSessionPoolConcurrentGetSharesOneLogin proves single-flight: callers
// that pile up on an in-flight login wait for it instead of starting their
// own. The login is held open on the server side, so "in flight" is a fact
// rather than a timing assumption.
func TestSessionPoolConcurrentGetSharesOneLogin(t *testing.T) {
	ls := newLoginServer(t)
	release := ls.hold()

	p := NewSessionPool()
	t.Cleanup(func() { _ = p.Close() })
	srv := testServer()

	type result struct {
		sess *auth.Session
		err  error
	}
	firstDone := make(chan result, 1)
	go func() {
		sess, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
		firstDone <- result{sess, err}
	}()
	ls.waitEntered(t)

	secondDone := make(chan result, 1)
	go func() {
		sess, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
		secondDone <- result{sess, err}
	}()

	// The first login is still open, so the second Get must neither start a
	// login of its own nor return a session. Both checks can only fail if the
	// pool really did race.
	select {
	case <-ls.entered:
		t.Fatal("a second login reached the server while the first was in flight")
	case got := <-secondDone:
		t.Fatalf("second Get() returned (%v) before the in-flight login finished", got.err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	first := <-firstDone
	second := <-secondDone

	if first.err != nil || second.err != nil {
		t.Fatalf("Get() errors = %v / %v, want nil", first.err, second.err)
	}
	if first.sess != second.sess {
		t.Fatal("concurrent Gets returned different sessions")
	}
	if got := ls.logins(); got != 1 {
		t.Fatalf("API logins = %d, want 1", got)
	}
}

// TestSessionPoolConcurrentGetDoesNotRace is the same property under the real
// login path, with enough callers to make a missing lock show up as either a
// second login or a torn map access.
func TestSessionPoolConcurrentGetDoesNotRace(t *testing.T) {
	ls := newLoginServer(t)
	p := NewSessionPool()
	t.Cleanup(func() { _ = p.Close() })
	srv := testServer()

	const n = 16
	sessions := make([]*auth.Session, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessions[i], errs[i] = p.Get(context.Background(), srv, ls.URL, creds(), nil)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Get() #%d = %v, want nil", i, errs[i])
		}
		if sessions[i] != sessions[0] {
			t.Fatalf("Get() #%d returned a different session", i)
		}
	}
	if got := ls.logins(); got != 1 {
		t.Fatalf("API logins = %d, want 1", got)
	}
}

// TestSessionPoolInvalidateForcesFreshLogin is the pool half of the
// retry-once policy: after Invalidate the next Get must pay for a login
// again instead of handing back the session that just failed.
func TestSessionPoolInvalidateForcesFreshLogin(t *testing.T) {
	ls := newLoginServer(t)
	p := NewSessionPool()
	t.Cleanup(func() { _ = p.Close() })
	srv := testServer()

	first, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
	if err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	p.Invalidate(srv.Name)

	second, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
	if err != nil {
		t.Fatalf("Get() after Invalidate = %v, want nil", err)
	}
	if first == second {
		t.Fatal("Get() after Invalidate returned the invalidated session")
	}
	if got := ls.logins(); got != 2 {
		t.Fatalf("API logins = %d, want 2", got)
	}
}

// TestSessionPoolGetAfterCloseReturnsErrClosed covers the teardown
// contract: a closed pool refuses new sessions instead of silently logging
// in again.
func TestSessionPoolGetAfterCloseReturnsErrClosed(t *testing.T) {
	ls := newLoginServer(t)
	p := NewSessionPool()
	srv := testServer()

	if _, err := p.Get(context.Background(), srv, ls.URL, creds(), nil); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if _, err := p.Get(context.Background(), srv, ls.URL, creds(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get() after Close = %v, want ErrClosed", err)
	}
	if got := ls.logins(); got != 1 {
		t.Fatalf("API logins = %d, want 1 (no login after Close)", got)
	}
}

// TestSessionPoolCloseIsIdempotent keeps the "safe to call twice" contract
// the CLI relies on in deferred teardown.
func TestSessionPoolCloseIsIdempotent(t *testing.T) {
	p := NewSessionPool()
	if err := p.Close(); err != nil {
		t.Fatalf("first Close() = %v, want nil", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
}

// TestSessionPoolInvalidateUnknownAliasIsSafe covers the retry path where the
// alias was never pooled: dropping it must be a no-op for the entries that do
// exist.
func TestSessionPoolInvalidateUnknownAliasIsSafe(t *testing.T) {
	ls := newLoginServer(t)
	p := NewSessionPool()
	t.Cleanup(func() { _ = p.Close() })
	srv := testServer()

	cached, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
	if err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	p.Invalidate("never-seen")

	again, err := p.Get(context.Background(), srv, ls.URL, creds(), nil)
	if err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	if again != cached {
		t.Fatal("Invalidate of an unrelated alias dropped a live session")
	}
	if got := ls.logins(); got != 1 {
		t.Fatalf("API logins = %d, want 1", got)
	}
}

// TestSessionPoolLoginErrorPropagatesWithoutCredential asserts the error
// class reaches the caller unchanged and that no credential material is
// interpolated into it.
func TestSessionPoolLoginErrorPropagatesWithoutCredential(t *testing.T) {
	ls := newLoginServer(t)
	ls.setReject(http.StatusUnauthorized)
	p := NewSessionPool()
	t.Cleanup(func() { _ = p.Close() })

	_, err := p.Get(context.Background(), testServer(), ls.URL, creds(), nil)
	if err == nil {
		t.Fatal("Get() = nil error, want the login failure")
	}
	if _, ok := api.AsAuthError(err); !ok {
		t.Fatalf("Get() error = %v (%T), want an *api.AuthError to reach the caller", err, err)
	}
	if strings.Contains(err.Error(), creds().Password) {
		t.Fatalf("login error leaked the password: %v", err)
	}
}

// TestSessionPoolGetNeedsServer keeps a programming mistake from becoming a
// nil-map panic or an anonymous login.
func TestSessionPoolGetNeedsServer(t *testing.T) {
	p := NewSessionPool()
	if _, err := p.Get(context.Background(), nil, "https://jms.example.com", creds(), nil); err == nil {
		t.Fatal("Get(nil server) = nil error, want an error")
	}
}

// TestSessionPoolNilReceiverIsSafe covers the nil-receiver behaviour the
// frozen signatures allow: a nil pool is a closed pool.
func TestSessionPoolNilReceiverIsSafe(t *testing.T) {
	var p *SessionPool
	if _, err := p.Get(context.Background(), testServer(), "https://jms.example.com", creds(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil pool Get() = %v, want ErrClosed", err)
	}
	p.Invalidate("anything")
	if err := p.Close(); err != nil {
		t.Fatalf("nil pool Close() = %v, want nil", err)
	}
}
