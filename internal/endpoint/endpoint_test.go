package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
)

// server builds a ServerConfig for the ordering tests.
func server(name, internal, external, prefer string) *config.ServerConfig {
	return &config.ServerConfig{
		Name:     name,
		Internal: internal,
		External: external,
		Username: "alice",
		Prefer:   prefer,
	}
}

// kinds lists the kind of every candidate, for readable assertions.
func kinds(cands []Candidate) []Kind {
	out := make([]Kind, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Kind)
	}
	return out
}

func TestCandidatesOrdering(t *testing.T) {
	cases := []struct {
		name string
		srv  *config.ServerConfig
		st   State
		want []Kind
	}{
		{
			name: "default prefer puts internal first",
			srv:  server("s", "https://in.example.com", "https://out.example.com", ""),
			want: []Kind{KindInternal, KindExternal},
		},
		{
			name: "explicit external prefer puts external first",
			srv:  server("s", "https://in.example.com", "https://out.example.com", config.PreferExternal),
			want: []Kind{KindExternal, KindInternal},
		},
		{
			name: "external-only server yields one candidate",
			srv:  server("s", "", "https://out.example.com", ""),
			want: []Kind{KindExternal},
		},
		{
			name: "internal-only server yields one candidate",
			srv:  server("s", "https://in.example.com", "", ""),
			want: []Kind{KindInternal},
		},
		{
			name: "no address yields no candidates",
			srv:  server("s", "", "", ""),
			want: []Kind{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Candidates(tc.srv, "", tc.st)
			if diff := strings.Join(kindStrings(kinds(got)), ","); diff != strings.Join(kindStrings(tc.want), ",") {
				t.Fatalf("Candidates = %v, want %v", kinds(got), tc.want)
			}
		})
	}
}

func TestCandidatesLastGoodFresh(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")

	fresh := State{LastGood: KindExternal, At: time.Now()}
	got := kinds(Candidates(srv, "", fresh))
	if len(got) != 2 || got[0] != KindExternal {
		t.Fatalf("fresh last-good ordering = %v, want external first", got)
	}

	stale := State{LastGood: KindExternal, At: time.Now().Add(-11 * time.Minute)}
	got = kinds(Candidates(srv, "", stale))
	if len(got) != 2 || got[0] != KindInternal {
		t.Fatalf("stale last-good ordering = %v, want internal first", got)
	}
}

func TestCandidatesForceNeverFallsBack(t *testing.T) {
	both := server("s", "https://in.example.com", "https://out.example.com", "")
	got := Candidates(both, KindExternal, State{})
	if len(got) != 1 || got[0].Kind != KindExternal {
		t.Fatalf("force external = %v, want exactly one external candidate", got)
	}

	externalOnly := server("s", "", "https://out.example.com", "")
	if got := Candidates(externalOnly, KindInternal, State{}); len(got) != 0 {
		t.Fatalf("force internal on external-only server = %v, want empty", got)
	}
}

func TestCandidatesDeduplicatesKind(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")
	st := State{LastGood: KindInternal, At: time.Now()}
	got := Candidates(srv, "", st)
	if len(got) != 2 {
		t.Fatalf("Candidates = %v, want exactly 2 (no duplicate kind)", got)
	}
}

func kindStrings(kk []Kind) []string {
	out := make([]string, 0, len(kk))
	for _, k := range kk {
		out = append(out, string(k))
	}
	return out
}

// recordingProbe returns a ProbeFunc that records every probed URL and
// reports reachability from the supplied set.
func recordingProbe(reachable map[string]bool, probed *[]string) ProbeFunc {
	return func(_ context.Context, url string, _ time.Duration) bool {
		if probed != nil {
			*probed = append(*probed, url)
		}
		return reachable[url]
	}
}

func TestSelectAndLoginProbeFailureFailsOver(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")
	store := NewMemoryStateStore()

	var probed []string
	probe := recordingProbe(map[string]bool{"https://out.example.com": true}, &probed)

	var logged []Kind
	login := func(_ context.Context, cand Candidate) error {
		logged = append(logged, cand.Kind)
		return nil
	}

	sel, err := SelectAndLogin(context.Background(), srv, "", probe, login, store)
	if err != nil {
		t.Fatalf("SelectAndLogin: %v", err)
	}
	if sel.Candidate.Kind != KindExternal {
		t.Fatalf("selected %v, want external", sel.Candidate.Kind)
	}
	if len(logged) != 1 || logged[0] != KindExternal {
		t.Fatalf("login attempts = %v, want only external", logged)
	}
	if len(probed) != 2 {
		t.Fatalf("probed %v, want both candidates", probed)
	}
	if sel.Reason == "" {
		t.Fatal("Selection.Reason is empty")
	}

	st, ok := store.Get(srv.Name)
	if !ok {
		t.Fatal("state store has no record after a successful login")
	}
	if st.LastGood != KindExternal {
		t.Fatalf("recorded last-good = %v, want external", st.LastGood)
	}
	if time.Since(st.At) > time.Minute {
		t.Fatalf("recorded timestamp %v is not fresh", st.At)
	}
}

func TestSelectAndLoginInternalPreferredSucceeds(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")
	store := NewMemoryStateStore()

	probe := func(_ context.Context, _ string, _ time.Duration) bool { return true }
	var logged []Kind
	login := func(_ context.Context, cand Candidate) error {
		logged = append(logged, cand.Kind)
		return nil
	}

	sel, err := SelectAndLogin(context.Background(), srv, "", probe, login, store)
	if err != nil {
		t.Fatalf("SelectAndLogin: %v", err)
	}
	if sel.Candidate.Kind != KindInternal {
		t.Fatalf("selected %v, want internal", sel.Candidate.Kind)
	}
	if len(logged) != 1 || logged[0] != KindInternal {
		t.Fatalf("login attempts = %v, want only internal", logged)
	}
	if st, _ := store.Get(srv.Name); st.LastGood != KindInternal {
		t.Fatalf("recorded last-good = %v, want internal", st.LastGood)
	}
}

// TestSelectAndLoginFallbackWhenAllProbesFail covers DESIGN.md「端点故障转移」:
// the probe is an accelerator, not a verdict.
func TestSelectAndLoginFallbackWhenAllProbesFail(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")
	store := NewMemoryStateStore()

	var probed []string
	probe := recordingProbe(nil, &probed)

	var logged []Kind
	login := func(_ context.Context, cand Candidate) error {
		logged = append(logged, cand.Kind)
		return nil
	}

	sel, err := SelectAndLogin(context.Background(), srv, "", probe, login, store)
	if err != nil {
		t.Fatalf("SelectAndLogin: %v", err)
	}
	if sel.Candidate.Kind != KindInternal {
		t.Fatalf("selected %v, want the first candidate internal", sel.Candidate.Kind)
	}
	if len(logged) != 1 || logged[0] != KindInternal {
		t.Fatalf("login attempts = %v, want one fallback attempt on internal", logged)
	}
	if st, _ := store.Get(srv.Name); st.LastGood != KindInternal {
		t.Fatalf("recorded last-good = %v, want internal", st.LastGood)
	}
}

func TestSelectAndLoginAllFailReturnsAllUnreachable(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")

	probe := func(_ context.Context, _ string, _ time.Duration) bool { return false }
	login := func(_ context.Context, _ Candidate) error { return errors.New("dial tcp: refused") }

	_, err := SelectAndLogin(context.Background(), srv, "", probe, login, NewMemoryStateStore())
	if !errors.Is(err, ErrAllUnreachable) {
		t.Fatalf("error = %v, want ErrAllUnreachable", err)
	}
	if !strings.Contains(err.Error(), "internal") || !strings.Contains(err.Error(), "external") {
		t.Fatalf("error %q does not list every candidate tried", err)
	}
}

func TestSelectAndLoginAuthErrorIsTerminal(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")

	probes := 0
	probe := func(_ context.Context, _ string, _ time.Duration) bool {
		probes++
		return true
	}
	logins := 0
	authErr := &api.AuthError{StatusCode: 401, Body: "bad password"}
	login := func(_ context.Context, _ Candidate) error {
		logins++
		return authErr
	}

	_, err := SelectAndLogin(context.Background(), srv, "", probe, login, NewMemoryStateStore())
	if !errors.Is(err, authErr) {
		t.Fatalf("error = %v, want the AuthError returned as-is", err)
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1: the second candidate must not be probed", probes)
	}
	if logins != 1 {
		t.Fatalf("logins = %d, want 1: the second candidate must not be logged into", logins)
	}
}

// TestSelectAndLoginServerErrorsFailOver pins DESIGN 4.7 pt 2 / 11.9: a
// network-class login failure (5xx or transport) must move on to the next
// candidate, not abort the selection.
func TestSelectAndLoginServerErrorsFailOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "502 from login", err: &api.AuthError{StatusCode: 502, Body: "bad gateway"}},
		{name: "transport failure", err: &api.APIError{StatusCode: 0, Body: "connection reset"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := server("s", "https://in.example.com", "https://out.example.com", "")
			probes := 0
			probe := func(_ context.Context, _ string, _ time.Duration) bool {
				probes++
				return true
			}
			var loggedInto []Kind
			login := func(_ context.Context, cand Candidate) error {
				loggedInto = append(loggedInto, cand.Kind)
				return tc.err
			}

			_, err := SelectAndLogin(context.Background(), srv, "", probe, login, NewMemoryStateStore())
			if !IsNetworkError(err) {
				t.Fatalf("error = %v, want it classified as network-class", err)
			}
			if len(loggedInto) != 2 {
				t.Fatalf("login attempts = %v, want both candidates tried", loggedInto)
			}
			if probes != 2 {
				t.Fatalf("probes = %d, want 2", probes)
			}
		})
	}
}

// TestSelectAndLoginMFASentinelIsTerminal pins that the login-flow sentinels
// do NOT fail over: the endpoint answered, so the problem is the account or
// its configuration, and a second doomed login would only prompt the user
// twice.
func TestSelectAndLoginMFASentinelIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "MFA not provided", err: auth.ErrMFANotProvided},
		{name: "missing session cookie", err: auth.ErrMissingSessionCookie},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := server("s", "https://in.example.com", "https://out.example.com", "")
			probes, logins := 0, 0
			probe := func(_ context.Context, _ string, _ time.Duration) bool { probes++; return true }
			login := func(_ context.Context, _ Candidate) error { logins++; return tc.err }

			_, err := SelectAndLogin(context.Background(), srv, "", probe, login, NewMemoryStateStore())
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want the sentinel returned as-is", err)
			}
			if probes != 1 || logins != 1 {
				t.Fatalf("probes = %d, logins = %d, want exactly one of each (terminal)", probes, logins)
			}
		})
	}
}

func TestSelectAndLoginNetworkErrorFailsOver(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")

	probe := func(_ context.Context, _ string, _ time.Duration) bool { return true }
	var logged []Kind
	login := func(_ context.Context, cand Candidate) error {
		logged = append(logged, cand.Kind)
		if cand.Kind == KindInternal {
			return errors.New("connection reset by peer")
		}
		return nil
	}

	sel, err := SelectAndLogin(context.Background(), srv, "", probe, login, NewMemoryStateStore())
	if err != nil {
		t.Fatalf("SelectAndLogin: %v", err)
	}
	if sel.Candidate.Kind != KindExternal {
		t.Fatalf("selected %v, want external after a network-class failure", sel.Candidate.Kind)
	}
	if len(logged) != 2 {
		t.Fatalf("login attempts = %v, want internal then external", logged)
	}
}

func TestSelectAndLoginNilProbeAssumesReachable(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")

	sel, err := SelectAndLogin(context.Background(), srv, "", nil,
		func(_ context.Context, _ Candidate) error { return nil }, NewMemoryStateStore())
	if err != nil {
		t.Fatalf("SelectAndLogin: %v", err)
	}
	if sel.Candidate.Kind != KindInternal {
		t.Fatalf("selected %v, want internal", sel.Candidate.Kind)
	}
	if sel.ProbeMS != 0 {
		t.Fatalf("ProbeMS = %d, want 0 when not probed", sel.ProbeMS)
	}
}

func TestSelectAndLoginNilLoginReportsReachability(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")

	probe := func(_ context.Context, _ string, _ time.Duration) bool { return true }
	sel, err := SelectAndLogin(context.Background(), srv, "", probe, nil, NewMemoryStateStore())
	if err != nil {
		t.Fatalf("SelectAndLogin: %v", err)
	}
	if sel.Candidate.Kind != KindInternal {
		t.Fatalf("selected %v, want the first reachable candidate", sel.Candidate.Kind)
	}
	if sel.Reason == "" {
		t.Fatal("Selection.Reason is empty for the reachability-only path")
	}
}

func TestSelectAndLoginForceWithoutAddressErrors(t *testing.T) {
	srv := server("s", "", "https://out.example.com", "")
	_, err := SelectAndLogin(context.Background(), srv, KindInternal, nil,
		func(_ context.Context, _ Candidate) error { return nil }, NewMemoryStateStore())
	if !errors.Is(err, ErrAllUnreachable) {
		t.Fatalf("error = %v, want ErrAllUnreachable", err)
	}
}

func TestSelectAndLoginNilStateStore(t *testing.T) {
	srv := server("s", "https://in.example.com", "", "")
	sel, err := SelectAndLogin(context.Background(), srv, "", nil,
		func(_ context.Context, _ Candidate) error { return nil }, nil)
	if err != nil {
		t.Fatalf("SelectAndLogin with a nil StateStore: %v", err)
	}
	if sel.Candidate.Kind != KindInternal {
		t.Fatalf("selected %v, want internal", sel.Candidate.Kind)
	}
}

func TestSelectAndLoginCancelledContext(t *testing.T) {
	srv := server("s", "https://in.example.com", "https://out.example.com", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := SelectAndLogin(ctx, srv, "", func(_ context.Context, _ string, _ time.Duration) bool {
		t.Error("probe must not run under a cancelled context")
		return true
	}, func(_ context.Context, _ Candidate) error { return nil }, NewMemoryStateStore())

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestIsNetworkError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		// Credential rejections are terminal, whatever wrapper carries them.
		{name: "401", err: &api.AuthError{StatusCode: 401}, want: false},
		{name: "403", err: &api.AuthError{StatusCode: 403}, want: false},
		{name: "400", err: &api.AuthError{StatusCode: 400}, want: false},
		{name: "422", err: &api.AuthError{StatusCode: 422}, want: false},
		// Network-class failures must fail over: DESIGN 4.7 pt 2 / 11.9.
		{name: "502 from login", err: &api.AuthError{StatusCode: 502}, want: true},
		{name: "500 from login", err: &api.AuthError{StatusCode: 500}, want: true},
		{name: "transport failure via AuthError", err: &api.AuthError{StatusCode: 0}, want: true},
		{name: "generic error", err: errors.New("connection refused"), want: true},
		{name: "api transport failure", err: &api.APIError{StatusCode: 0, Body: "boom"}, want: true},
		// The login-flow sentinels mean the endpoint answered and the account
		// or its configuration is the problem: terminal.
		{name: "MFA not provided", err: auth.ErrMFANotProvided, want: false},
		{name: "wrapped MFA not provided", err: fmt.Errorf("login: %w", auth.ErrMFANotProvided), want: false},
		{name: "missing session cookie", err: auth.ErrMissingSessionCookie, want: false},
		// Asking to stop is never a reason to try elsewhere.
		{name: "context canceled", err: context.Canceled, want: false},
		{name: "context deadline", err: context.DeadlineExceeded, want: false},
		{name: "context canceled", err: context.Canceled, want: false},
		{name: "context deadline", err: context.DeadlineExceeded, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNetworkError(tc.err); got != tc.want {
				t.Fatalf("IsNetworkError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestTCPProbe(t *testing.T) {
	t.Run("live listener is reachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer srv.Close()
		if !TCPProbe(context.Background(), srv.URL, ProbeTimeout) {
			t.Fatalf("TCPProbe(%s) = false, want true", srv.URL)
		}
	})

	t.Run("closed port is unreachable", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		ln.Close()
		if TCPProbe(context.Background(), "http://"+addr, ProbeTimeout) {
			t.Fatalf("TCPProbe(%s) = true, want false for a closed port", addr)
		}
	})

	t.Run("URL without a host is unreachable", func(t *testing.T) {
		if TCPProbe(context.Background(), "http:///missing-host", ProbeTimeout) {
			t.Fatal("TCPProbe on a hostless URL = true, want false")
		}
		if TCPProbe(context.Background(), "not a url", ProbeTimeout) {
			t.Fatal("TCPProbe on garbage = true, want false")
		}
	})

	t.Run("honours the context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// The dial must fail promptly rather than wait for the timeout.
		start := time.Now()
		if TCPProbe(ctx, "http://192.0.2.1:80", 5*time.Second) {
			t.Fatal("TCPProbe under a cancelled context = true, want false")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("TCPProbe blocked %v under a cancelled context", elapsed)
		}
	})
}

func TestFileStateStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	store := NewFileStateStore(path)

	if fs, ok := store.(*FileStateStore); !ok || fs.Path() != path {
		t.Fatalf("NewFileStateStore returned %T with Path() = %q", store, store.(*FileStateStore).Path())
	}

	if _, ok := store.Get("s"); ok {
		t.Fatal("Get on a missing file reported found")
	}

	want := State{LastGood: KindExternal, At: time.Now().Truncate(time.Second)}
	if err := store.Set("s", want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// A fresh store reads the persisted document.
	reopened := NewFileStateStore(path)
	got, ok := reopened.Get("s")
	if !ok {
		t.Fatal("Get after Set reported not found")
	}
	if got.LastGood != want.LastGood || !got.At.Equal(want.At) {
		t.Fatalf("round-trip state = %+v, want %+v", got, want)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat state file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("state file mode = %o, want 600", perm)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temp residue left behind: %s", entry.Name())
		}
	}
}

func TestFileStateStoreCorruptFileReportsNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	store := NewFileStateStore(path)

	if _, ok := store.Get("s"); ok {
		t.Fatal("Get on a corrupt file reported found")
	}
	// A corrupt file must be recoverable, not fatal.
	if err := store.Set("s", State{LastGood: KindInternal, At: time.Now()}); err != nil {
		t.Fatalf("Set over a corrupt file: %v", err)
	}
	if _, ok := store.Get("s"); !ok {
		t.Fatal("Set did not repair the corrupt file")
	}
}

func TestFileStateStoreCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", StateFileName)
	store := NewFileStateStore(path)
	if err := store.Set("s", State{LastGood: KindInternal, At: time.Now()}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}
}

func TestStateBackendRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	store := NewFileStateStore(path)

	want := State{
		LastGood: KindExternal,
		At:       time.Now().Truncate(time.Second),
		Backend:  map[Kind]string{KindExternal: "ws", KindInternal: "ssh"},
	}
	if err := store.Set("s", want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok := NewFileStateStore(path).Get("s")
	if !ok {
		t.Fatal("Get reported not found")
	}
	if got.BackendFor(KindExternal) != "ws" {
		t.Fatalf("BackendFor(external) = %q, want ws", got.BackendFor(KindExternal))
	}
	if got.BackendFor(KindInternal) != "ssh" {
		t.Fatalf("BackendFor(internal) = %q, want ssh", got.BackendFor(KindInternal))
	}
	if got.BackendFor(Kind("bogus")) != "" {
		t.Fatalf("BackendFor(unknown) = %q, want empty", got.BackendFor(Kind("bogus")))
	}

	var zero State
	if zero.BackendFor(KindInternal) != "" {
		t.Fatalf("BackendFor on a zero State = %q, want empty", zero.BackendFor(KindInternal))
	}
}

func TestStateTouchAndFreshness(t *testing.T) {
	now := time.Now()
	touched := State{}.Touch(KindExternal, now)
	if touched.LastGood != KindExternal || !touched.At.Equal(now) {
		t.Fatalf("Touch = %+v, want external at %v", touched, now)
	}
	if !touched.LastGoodFresh(now.Add(9 * time.Minute)) {
		t.Fatal("state 9 minutes old should still be fresh")
	}
	if touched.LastGoodFresh(now.Add(10 * time.Minute)) {
		t.Fatal("state exactly 10 minutes old should be stale")
	}
	if (State{}).LastGoodFresh(now) {
		t.Fatal("zero state reported fresh")
	}
}

func TestMemoryStateStoreRoundTrip(t *testing.T) {
	store := NewMemoryStateStore()
	if _, ok := store.Get("s"); ok {
		t.Fatal("empty memory store reported found")
	}
	want := State{LastGood: KindInternal, At: time.Now()}
	if err := store.Set("s", want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := store.Get("s")
	if !ok || got.LastGood != want.LastGood {
		t.Fatalf("Get = %+v, %v; want %+v, true", got, ok, want)
	}
}

func TestFileStateStorePersistsMultipleServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	store := NewFileStateStore(path)

	if err := store.Set("a", State{LastGood: KindInternal, At: time.Now()}); err != nil {
		t.Fatalf("Set a: %v", err)
	}
	if err := store.Set("b", State{LastGood: KindExternal, At: time.Now()}); err != nil {
		t.Fatalf("Set b: %v", err)
	}

	// The document shape is {"servers": {...}}.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var doc struct {
		Servers map[string]json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("state file is not {\"servers\": {...}}: %v", err)
	}
	if len(doc.Servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(doc.Servers))
	}

	reopened := NewFileStateStore(path)
	if st, _ := reopened.Get("a"); st.LastGood != KindInternal {
		t.Fatalf("server a = %v, want internal", st.LastGood)
	}
	if st, _ := reopened.Get("b"); st.LastGood != KindExternal {
		t.Fatalf("server b = %v, want external", st.LastGood)
	}
}
