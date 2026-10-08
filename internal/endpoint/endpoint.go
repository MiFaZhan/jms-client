// Package endpoint resolves which of a server's addresses to use and
// performs the internal/external failover described in DESIGN.md「端点故障转移」.
//
// The policy in one paragraph: order the configured addresses (an
// explicit force wins, then a recent last-good address, then the
// configured preference, then the rest), probe each with a short TCP
// timeout, and attempt a login on the first reachable one. Network-class
// failures move to the next candidate; an authentication failure is
// terminal, because a credential problem is not fixed by another address.
// When every probe fails, the first candidate still gets one full login
// attempt — the probe is an accelerator, not a verdict.
//
// Dependency direction: endpoint depends on api and config.
package endpoint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/config"
	"github.com/MiFaZhan/jms-client/internal/netproxy"
)

// Kind identifies which configured address a candidate came from.
type Kind string

// Address kinds. These values are also written to the audit stream and
// rendered by `jms tail` / `jms attach`.
const (
	KindInternal Kind = config.KindInternal
	KindExternal Kind = config.KindExternal
)

// ProbeTimeout is the TCP dial timeout for a candidate (DESIGN.md「数值基线」).
// It is deliberately short: a DROP-style firewall would otherwise stall
// for 75s.
const ProbeTimeout = 1500 * time.Millisecond

// LastGoodTTL is how long a successful address keeps its priority boost.
const LastGoodTTL = 10 * time.Minute

// Candidate is one address worth trying.
type Candidate struct {
	Kind    Kind
	URL     string
	SSHPort int
}

// String renders the candidate as "internal http://host:port".
func (c Candidate) String() string {
	return fmt.Sprintf("%s %s", c.Kind, c.URL)
}

// Selection records which candidate was chosen and why.
type Selection struct {
	Candidate Candidate
	ProbeMS   int64  // measured probe time in milliseconds; 0 when not probed
	Reason    string // human-readable decision reason, for logs and audit
}

// ProbeFunc reports whether an address is reachable. It must honour the
// context and must not return an error: unreachability is a false result.
type ProbeFunc func(ctx context.Context, url string, timeout time.Duration) bool

// LoginFunc attempts a login against one candidate.
//
// It must return an error matching api.AuthError for credential
// problems; any other error is treated as a network-class failure and
// triggers failover.
type LoginFunc func(ctx context.Context, cand Candidate) error

// State is the persisted cross-process memory of the last successful
// address and backend.
type State struct {
	LastGood Kind            `json:"last_good,omitempty"`
	At       time.Time       `json:"at,omitempty"`
	Backend  map[Kind]string `json:"backend,omitempty"`
}

// LastGoodFresh reports whether the recorded address is still within TTL.
func (s State) LastGoodFresh(now time.Time) bool {
	if s.LastGood == "" || s.At.IsZero() {
		return false
	}
	return now.Sub(s.At) < LastGoodTTL
}

// StateStore persists State between processes.
type StateStore interface {
	// Get returns the recorded state for a server alias. The bool is
	// false when nothing is recorded.
	Get(server string) (State, bool)
	// Set records the state for a server alias.
	Set(server string, st State) error
}

// ErrAllUnreachable reports that every candidate failed.
var ErrAllUnreachable = errors.New("no reachable endpoint")

// Candidates orders the addresses to try, given a server, an optional
// forced kind, and the recorded state.
//
// Order: a forced kind returns exactly that candidate (or nothing when
// the server has no such address — forcing must never silently fall back);
// otherwise the fresh last-good kind, then the configured preference,
// then any remaining address. Each kind appears at most once.
func Candidates(srv *config.ServerConfig, force Kind, st State) []Candidate {
	if srv == nil {
		return nil
	}

	all := make([]Candidate, 0, 2)
	for _, kind := range []Kind{KindInternal, KindExternal} {
		raw, ok := srv.URLFor(string(kind))
		if !ok {
			continue
		}
		all = append(all, Candidate{Kind: kind, URL: raw, SSHPort: srv.Port()})
	}
	if len(all) == 0 {
		return nil
	}

	// A pinned server is restricted to one address by configuration, which is
	// the user saying "I know which network I am on": no probing of the other,
	// no failover (DESIGN.md「端点故障转移」第 7 点, persisted).
	if pin := strings.TrimSpace(srv.Pin); pin != "" {
		pinned := Kind(strings.ToLower(pin))
		for _, cand := range all {
			if cand.Kind == pinned {
				return []Candidate{cand}
			}
		}
		return nil
	}

	if force != "" {
		for _, cand := range all {
			if cand.Kind == force {
				return []Candidate{cand}
			}
		}
		return nil
	}

	ordered := make([]Candidate, 0, len(all))
	taken := make(map[Kind]bool, len(all))
	appendKind := func(kind Kind) {
		if taken[kind] {
			return
		}
		for _, cand := range all {
			if cand.Kind == kind {
				ordered = append(ordered, cand)
				taken[kind] = true
				return
			}
		}
	}

	if st.LastGoodFresh(time.Now()) {
		appendKind(st.LastGood)
	}
	appendKind(Kind(srv.Preferred()))
	for _, cand := range all {
		appendKind(cand.Kind)
	}
	return ordered
}

// TCPProbe is the default ProbeFunc: it dials the candidate's host:port
// and closes the connection immediately.
//
// It dials DIRECTLY. When a proxy is configured, use ProbeWith instead,
// or the probe and the login that follows it would measure two different
// networks (DESIGN.md「代理策略」).
func TCPProbe(ctx context.Context, rawURL string, timeout time.Duration) bool {
	return dialProbe(ctx, rawURL, timeout, netproxy.Direct())
}

// ProbeWith returns a ProbeFunc that reaches the candidate through the
// given proxy policy.
//
// A direct policy returns TCPProbe itself, so the default path is
// unchanged. This is what keeps the 「端点故障转移」 policy honest: the
// probe is an accelerator for the login, and it can only accelerate the
// right thing if it travels the same route.
func ProbeWith(px netproxy.Proxy) ProbeFunc {
	if px.IsDirect() {
		return TCPProbe
	}
	return func(ctx context.Context, rawURL string, timeout time.Duration) bool {
		return dialProbe(ctx, rawURL, timeout, px)
	}
}

// dialProbe dials a candidate URL through the given policy and reports
// reachability.
func dialProbe(ctx context.Context, rawURL string, timeout time.Duration, px netproxy.Proxy) bool {
	host, port, err := hostPort(rawURL)
	if err != nil {
		return false
	}
	dial := px.DialContext(timeout)
	conn, err := dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// hostPort extracts the dial target from an http(s) URL, defaulting the
// port from the scheme (443 for https, 80 for http).
func hostPort(rawURL string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", err
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("URL %q has no host", rawURL)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("URL %q has no host", rawURL)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port, nil
}

// SelectAndLogin applies the failover policy and returns the chosen
// candidate.
//
// probe may be nil, in which case every candidate is assumed reachable.
// login may be nil, in which case the first candidate is returned without
// attempting a login (used by `jms config test` to report reachability
// only).
//
// On success the chosen address is recorded in st; a nil st is tolerated.
// Authentication failures are returned immediately and never trigger
// failover (DESIGN.md「端点故障转移」第 2 点).
func SelectAndLogin(ctx context.Context, srv *config.ServerConfig, force Kind,
	probe ProbeFunc, login LoginFunc, st StateStore) (Selection, error) {

	candidates := Candidates(srv, force, stateOf(st, srv))
	if len(candidates) == 0 {
		if force != "" {
			return Selection{}, fmt.Errorf("%w: server %q has no %s address",
				ErrAllUnreachable, serverName(srv), force)
		}
		return Selection{}, fmt.Errorf("%w: server %q has no address configured",
			ErrAllUnreachable, serverName(srv))
	}

	var failures []string
	var firstProbeMS int64
	// firstSkipped records that candidate 0 never reached the login stage
	// because its probe failed - the only situation in which the 4.7
	// fallback is allowed to spend a full login on it.
	firstSkipped := false
	for i, cand := range candidates {
		if err := ctx.Err(); err != nil {
			return Selection{}, err
		}

		probeMS := int64(0)
		reachable := true
		if probe != nil {
			started := time.Now()
			reachable = probe(ctx, cand.URL, ProbeTimeout)
			probeMS = time.Since(started).Milliseconds()
		}
		if i == 0 {
			firstProbeMS = probeMS
		}
		if !reachable {
			failures = append(failures, fmt.Sprintf("%s: probe failed", cand.Kind))
			if i == 0 {
				firstSkipped = true
			}
			continue
		}
		if login == nil {
			return Selection{
				Candidate: cand,
				ProbeMS:   probeMS,
				Reason:    "reachable (no login requested)",
			}, nil
		}

		err := login(ctx, cand)
		if err == nil {
			recordLastGood(st, srv, cand.Kind)
			return Selection{Candidate: cand, ProbeMS: probeMS, Reason: "login succeeded"}, nil
		}
		if !IsNetworkError(err) {
			// Auth error (terminal) or a cancelled context: do not move on.
			return Selection{}, err
		}
		failures = append(failures, fmt.Sprintf("%s: %v", cand.Kind, err))
	}

	// Every probe failed: the probe is an accelerator, not a verdict, so the
	// first candidate gets one full login attempt (DESIGN.md「端点故障转移」). A
	// first candidate that was already login-attempted is not retried - its
	// network-class failure is already recorded above, and a second identical
	// attempt would only double the round-trips (and prompt for MFA twice).
	first := candidates[0]
	if login == nil || !firstSkipped {
		return Selection{}, fmt.Errorf("%w: server %q: %s",
			ErrAllUnreachable, serverName(srv), strings.Join(failures, "; "))
	}
	if err := ctx.Err(); err != nil {
		return Selection{}, err
	}
	err := login(ctx, first)
	if err == nil {
		recordLastGood(st, srv, first.Kind)
		return Selection{
			Candidate: first,
			// The first candidate was probed earlier in the loop; report
			// that measurement rather than a misleading zero.
			ProbeMS: firstProbeMS,
			Reason:  "fallback login succeeded after every probe failed",
		}, nil
	}
	if !IsNetworkError(err) {
		return Selection{}, err
	}
	failures = append(failures, fmt.Sprintf("%s: %v", first.Kind, err))
	return Selection{}, fmt.Errorf("%w: server %q: %s",
		ErrAllUnreachable, serverName(srv), strings.Join(failures, "; "))
}

// terminalStatuses are the credential-rejection statuses: the server saw
// the credential and refused it. Switching to another address cannot fix
// them, so they are terminal for endpoint selection (DESIGN.md「端点故障转移」第 2 点).
var terminalStatuses = map[int]bool{
	http.StatusBadRequest:          true,
	http.StatusUnauthorized:        true,
	http.StatusForbidden:           true,
	http.StatusUnprocessableEntity: true,
}

// IsNetworkError reports whether err should trigger failover to the next
// candidate (DESIGN.md「端点故障转移」第 2 点).
//
// Terminal - never a reason to try another address:
//   - a credential rejection: *api.AuthError carrying a 400/401/403/422, and
//     auth.ErrMFANotProvided / auth.ErrMissingSessionCookie (both mean the
//     endpoint answered and the problem is the account or its configuration)
//   - context cancellation or deadline expiry: the caller asked to stop
//
// Network-class - fail over: a transport failure (api.APIError with
// StatusCode 0, i.e. TCP reachable but the service behind it broken - the
// 「端点故障转移」 VPN-half-connected case) and 5xx responses.
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, auth.ErrMFANotProvided) || errors.Is(err, auth.ErrMissingSessionCookie) {
		return false
	}
	if authErr, ok := api.AsAuthError(err); ok {
		return !terminalStatuses[authErr.StatusCode]
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func stateOf(st StateStore, srv *config.ServerConfig) State {
	if st == nil {
		return State{}
	}
	state, _ := st.Get(serverName(srv))
	return state
}

// recordLastGood stores the kind and timestamp of a successful selection,
// preserving the backend memory written by other callers. Failures to
// persist are ignored: the state file is an optimization, and a
// successful login must not be reported as failed because of it.
func recordLastGood(st StateStore, srv *config.ServerConfig, kind Kind) {
	if st == nil {
		return
	}
	name := serverName(srv)
	state, _ := st.Get(name)
	state.LastGood = kind
	state.At = time.Now()
	_ = st.Set(name, state)
}

func serverName(srv *config.ServerConfig) string {
	if srv == nil {
		return ""
	}
	return srv.Name
}
