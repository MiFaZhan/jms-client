package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetSleep restores the real sleep function after a test replaces it.
func resetSleep(t *testing.T, fn func(context.Context, time.Duration)) {
	t.Helper()
	prev := sleepFn
	sleepFn = fn
	t.Cleanup(func() { sleepFn = prev })
}

// countingServer answers with a scripted list of responses, recording every
// request. It is the backbone of the retry tests.
type countingServer struct {
	mu       sync.Mutex
	requests int
	statuses []int
	authSeen []string
	paths    []string
	queries  []url.Values
}

func newCountingServer(t *testing.T, statuses []int) (*countingServer, string) {
	cs := &countingServer{statuses: statuses}
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		n := cs.requests
		cs.requests++
		cs.authSeen = append(cs.authSeen, r.Header.Get("Authorization"))
		cs.paths = append(cs.paths, r.URL.Path)
		if r.URL.RawQuery != "" {
			q := r.URL.Query()
			cs.queries = append(cs.queries, q)
		}
		cs.mu.Unlock()

		status := http.StatusOK
		cs.mu.Lock()
		if n < len(cs.statuses) {
			status = cs.statuses[n]
		}
		cs.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return cs, srv.URL
}

func (cs *countingServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.requests
}

func (cs *countingServer) bearerAt(n int) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if n >= len(cs.authSeen) {
		return ""
	}
	return cs.authSeen[n]
}

func TestRetrySucceedsAfterTwoBadGateways(t *testing.T) {
	resetSleep(t, func(context.Context, time.Duration) {}) // no real sleeping

	cs, u := newCountingServer(t, []int{502, 503, http.StatusOK})
	c := New(u)

	var out map[string]any
	if err := c.Get(context.Background(), "/api/v1/x/", nil, &out); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	if got := cs.count(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if out["ok"] != true {
		t.Fatalf("decoded body = %v, want {\"ok\":true}", out)
	}
}

func TestRetryExhaustsAndReportsLastStatus(t *testing.T) {
	resetSleep(t, func(context.Context, time.Duration) {})

	// 1 initial + 3 retries = 4 attempts, every one a 503.
	cs, u := newCountingServer(t, []int{503, 503, 503, 503, 503})
	c := New(u)

	err := c.Get(context.Background(), "/api/v1/x/", nil, nil)
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("Get() = %T(%v), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode = %d, want 503", apiErr.StatusCode)
	}
	if got := cs.count(); got != maxRetries+1 {
		t.Fatalf("attempts = %d, want %d", got, maxRetries+1)
	}
}

func TestUnauthorizedIsNotRetried(t *testing.T) {
	resetSleep(t, func(context.Context, time.Duration) {})

	cs, u := newCountingServer(t, []int{http.StatusUnauthorized})
	c := New(u)

	err := c.Get(context.Background(), "/api/v1/x/", nil, nil)
	authErr, ok := AsAuthError(err)
	if !ok {
		t.Fatalf("Get() = %T(%v), want *AuthError", err, err)
	}
	if authErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", authErr.StatusCode)
	}
	if got := cs.count(); got != 1 {
		t.Fatalf("attempts = %d, want exactly 1 (401 must never be retried)", got)
	}
}

func TestTransportFailureHasZeroStatus(t *testing.T) {
	resetSleep(t, func(context.Context, time.Duration) {})

	c := New("http://127.0.0.1:1/") // nothing listens there

	err := c.Get(context.Background(), "/api/v1/x/", nil, nil)
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("Get() = %T(%v), want *APIError", err, err)
	}
	if apiErr.StatusCode != 0 {
		t.Fatalf("StatusCode = %d, want 0 for a transport failure", apiErr.StatusCode)
	}
}

func TestBackoffScheduleIsHalfSecondOneSecondTwoSeconds(t *testing.T) {
	var got []time.Duration
	resetSleep(t, func(_ context.Context, d time.Duration) { got = append(got, d) })

	_, u := newCountingServer(t, []int{502, 502, 502, 502})
	c := New(u)
	_ = c.Get(context.Background(), "/api/v1/x/", nil, nil)

	want := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	if len(got) != len(want) {
		t.Fatalf("backoff entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestGetAllSpansTwoPagesWithExactOffsets(t *testing.T) {
	var offsets []string
	var limits []string
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limits = append(limits, q.Get("limit"))
		offsets = append(offsets, q.Get("offset"))
		switch q.Get("offset") {
		case "0":
			_, _ = w.Write([]byte(`{"count":3,"results":[{"id":"a"},{"id":"b"}]}`))
		default:
			_, _ = w.Write([]byte(`{"count":3,"results":[{"id":"c"}]}`))
		}
	}))
	c := New(srv.URL)

	items, err := c.GetAll(context.Background(), PathSelfAssetsLike, nil)
	if err != nil {
		t.Fatalf("GetAll() = %v, want nil", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if got := strings.Join(offsets, ","); got != "0,2" {
		t.Fatalf("offsets = %q, want \"0,2\"", got)
	}
	if got := strings.Join(limits, ","); got != fmt.Sprintf("%d,%d", DefaultPageSize, DefaultPageSize) {
		t.Fatalf("limits = %q, want two %d calls", got, DefaultPageSize)
	}
}

func TestGetAllAcceptsBareListAndStopsAfterOneRequest(t *testing.T) {
	requests := 0
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`[{"id":"a"},{"id":"b"}]`))
	}))
	c := New(srv.URL)

	items, err := c.GetAll(context.Background(), PathSelfAssetsLike, nil)
	if err != nil {
		t.Fatalf("GetAll() = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestGetAllStopsWhenResultsAreEmpty(t *testing.T) {
	requests := 0
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"count":5,"results":[]}`))
	}))
	c := New(srv.URL)

	items, err := c.GetAll(context.Background(), PathSelfAssetsLike, nil)
	if err != nil {
		t.Fatalf("GetAll() = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1 (no endless loop on an empty page)", requests)
	}
}

func TestGetAllBoundsARogueServerThatNeverAdvances(t *testing.T) {
	// A server that always answers the same page must not loop forever; the
	// bound is an error, not a silently truncated result.
	requests := 0
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"count":999999,"results":[{"id":"a"}]}`))
	}))
	c := New(srv.URL)

	if _, err := c.GetAll(context.Background(), PathSelfAssetsLike, nil); err == nil {
		t.Fatal("GetAll() = nil error, want one when pagination never terminates")
	}
	if requests != maxPages {
		t.Fatalf("requests = %d, want the maxPages bound of %d", requests, maxPages)
	}
}

func TestNonJSONBodyOnTwoHundredIsAnAPIError(t *testing.T) {
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	c := New(srv.URL)

	var out map[string]any
	err := c.Get(context.Background(), "/api/v1/x/", nil, &out)
	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("Get() = %T(%v), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", apiErr.StatusCode)
	}
}

func TestNoContentLeavesOutUntouched(t *testing.T) {
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	c := New(srv.URL)

	out := map[string]any{"sentinel": "untouched"}
	if err := c.Get(context.Background(), "/api/v1/x/", nil, &out); err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if out["sentinel"] != "untouched" {
		t.Fatalf("204 overwrote out: %v", out)
	}
}

func TestEmptyBodyLeavesOutUntouched(t *testing.T) {
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(""))
	}))
	c := New(srv.URL)

	out := map[string]any{"sentinel": "untouched"}
	if err := c.Get(context.Background(), "/api/v1/x/", nil, &out); err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if out["sentinel"] != "untouched" {
		t.Fatalf("empty body overwrote out: %v", out)
	}
}

func TestCookieJarPersistsServerSetCookie(t *testing.T) {
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "jms_sessionid", Value: "s3cret", Path: "/"})
	}))
	c := New(srv.URL)

	if err := c.Get(context.Background(), "/core/auth/login/", nil, nil); err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if got := c.Cookie("jms_sessionid"); got != "s3cret" {
		t.Fatalf("Cookie() = %q, want \"s3cret\"", got)
	}
	if got := c.Cookie("absent"); got != "" {
		t.Fatalf("Cookie(absent) = %q, want \"\"", got)
	}
}

func TestNewNormalizesBaseURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://h:2280/", "http://h:2280"},
		{"http://h:2280//", "http://h:2280"},
		{"  http://h:2280/  ", "http://h:2280"},
		{"jump.example.com", "https://jump.example.com"},
		{"https://h", "https://h"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := New(tt.in).BaseURL(); got != tt.want {
			t.Errorf("New(%q).BaseURL() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBearerAndCSRFReachTheWire(t *testing.T) {
	resetSleep(t, func(context.Context, time.Duration) {})
	cs, u := newCountingServer(t, []int{http.StatusOK})
	c := New(u)
	c.SetBearer("tok-1")
	c.SetCSRF("csrf-1")

	if err := c.Get(context.Background(), "/api/v1/x/", nil, nil); err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if got := cs.bearerAt(0); got != "Bearer tok-1" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok-1")
	}
}

func TestGetAllTreatsAnObjectWithoutResultsAsEmpty(t *testing.T) {
	// {"detail": ...} decodes to zero results, which the client reports as
	// an empty listing rather than a protocol error — matching the Python
	// reference, where data.get("results", []) simply stops the iterator.
	requests := 0
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"detail":"no results"}`))
	}))
	c := New(srv.URL)

	items, err := c.GetAll(context.Background(), PathSelfAssetsLike, nil)
	if err != nil {
		t.Fatalf("GetAll() = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

// TestCancelledContextDoesNotBurnRetries guards the fix for the retry loop
// burning the budget after the caller gave up.
func TestCancelledContextDoesNotBurnRetries(t *testing.T) {
	var inFlight sync.WaitGroup
	inFlight.Add(1)
	release := make(chan struct{})
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inFlight.Done()
		<-release // hold the request open until the caller has cancelled
	}))
	c := New(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Get(ctx, "/api/v1/x/", nil, nil) }()

	inFlight.Wait()
	cancel()

	// The handler is deliberately left blocked while Get runs, so the only
	// way Get can return is by observing the cancellation. Releasing it here
	// would race the abort against the response: if the 200 arrived first,
	// Get would succeed and the assertions below would fail intermittently
	// under load. Unblock it only after Get has returned.
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("Get did not return after the context was cancelled")
	}
	close(release)

	apiErr, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("Get() = %T(%v), want *APIError", err, err)
	}
	// The cancellation must surface, not a misleading "no attempts made".
	if apiErr.StatusCode != 0 {
		t.Fatalf("StatusCode = %d, want 0", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "context canceled") {
		t.Fatalf("Body = %q, want it to mention the cancellation", apiErr.Body)
	}
}

// TestSetHTTPClientReplacesTransport verifies the seam tests and the form
// login rely on.
func TestSetHTTPClientReplacesTransport(t *testing.T) {
	c := New("https://example.invalid/")
	if c.HTTP() == nil {
		t.Fatal("HTTP() = nil, want a default client")
	}
	jar, _ := cookiejar.New(nil)
	replacement := &http.Client{Jar: jar, Timeout: time.Second}
	c.SetHTTPClient(replacement)
	if c.HTTP() != replacement {
		t.Fatal("SetHTTPClient did not replace the transport")
	}
	c.SetHTTPClient(nil)
	if c.HTTP() != replacement {
		t.Fatal("SetHTTPClient(nil) must be a no-op")
	}
}

// TestNilClientIsSafe pins the nil-receiver contract the CLI relies on.
func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	if c.BaseURL() != "" {
		t.Fatal("nil BaseURL() != \"\"")
	}
	if c.HTTP() != nil {
		t.Fatal("nil HTTP() != nil")
	}
	if err := c.Get(context.Background(), "/x/", nil, nil); err == nil {
		t.Fatal("nil client Get() = nil error, want one")
	}
}
