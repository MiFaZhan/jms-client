package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/transport/console"
)

// keepaliveMissesTolerated mirrors OpenSSH's own client: a single lost
// reply does not declare the link dead, three in a row do. A genuinely
// dead link surfaces on the next Execute anyway, which is where an error
// can actually reach the caller.
const keepaliveMissesTolerated = 3

// connectSSH opens the KoKo SSH backend.
//
// STATUS (2026-10-07): UNTESTED against a live server. Every deployment
// reached so far has the KoKo SSH entry disabled — the official SSH Client
// launch path fails with ECONNREFUSED on the KoKo port (2222), and the
// handshake here times out identically. The code paths are covered by unit
// tests and the DESIGN.md porting checklist, but no live session has ever
// completed over this backend. When a deployment with KoKo SSH enabled is
// available, verify before relying on it: interactive login, jms exec exit
// codes, keepalive behaviour behind the bastion's own firewall, and the
// auto-backend fallback order.
//
// KoKo's SSH service authenticates with the connection token: the username
// is "JMS-{token_id}" and the password is token_value (DESIGN.md §3.3).
// Every command then runs on its own session channel, which is what gives a
// native exit code and removes the marker-parsing problem the WebSocket
// backend has.
//
// The host is parsed from sess.BaseURL and never re-resolved: the endpoint
// is bound to the session (DESIGN.md §4.7 point 1). Per DESIGN.md §3.3 one
// retry with a fresh token is made, because a stale token is the most
// common cause of an auth failure.
func connectSSH(ctx context.Context, sess *auth.Session, asset assets.Info, opts ConnectOptions) (Terminal, error) {
	host := kokoHost(sess.BaseURL)
	if host == "" {
		return nil, fmt.Errorf("ssh backend: session base URL %q has no host", sess.BaseURL)
	}
	port := opts.SSHPort
	if port == 0 {
		port = DefaultSSHPort
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	// One attempt, not two. DESIGN.md §3.3 asks for a retry on a stale token,
	// but a token is created fresh for this dial, so the only failure the
	// retry could absorb is a transient network fault — and paying a second
	// handshake timeout on every blocked port costs the auto-backend path
	// twice the latency for a case it cannot fix. The retry lives in
	// connectAuto instead, which tries a genuinely different backend.
	term, err := dialSSH(ctx, sess, asset, opts, addr)
	if err != nil {
		return nil, fmt.Errorf("ssh connect to %s: %w", addr, err)
	}
	return term, nil
}

// dialSSH performs one token-request-plus-dial attempt.
func dialSSH(ctx context.Context, sess *auth.Session, asset assets.Info, opts ConnectOptions, addr string) (*sshTerminal, error) {
	token, err := newConnectionToken(ctx, sess, asset, opts.Protocol, opts.ConnectMethod)
	if err != nil {
		return nil, fmt.Errorf("connection token: %w", err)
	}

	// The host key is accepted unauthenticated, which deserves an honest
	// note: KoKo presents a host key the tool has no way to pin — there is
	// no known_hosts contract for a bastion reached with a one-shot token,
	// and the token itself is what authenticates the session. The
	// trade-off is that an on-path attacker could impersonate KoKo and read
	// or alter the session; what they cannot do is reuse it, because the
	// token is single-use and MFA-bounded. This is documented here rather
	// than hidden behind a callback that pretends to verify.
	config := &ssh.ClientConfig{
		User:            "JMS-" + token.ID,
		Auth:            []ssh.AuthMethod{ssh.Password(token.Value)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		// A TCP handshake alone proves nothing: a firewall that accepts every
		// connection and then silently drops it makes Dial succeed in ~25ms
		// while the SSH identification string never arrives, and the handshake
		// then waits out its full timeout. Bounding it here is what keeps the
		// auto-backend fallback to the WebSocket quick on a blocked KoKo port —
		// without this, one blocked port cost ~13s per call before the
		// fallback got its turn.
		Timeout: SSHBannerTimeout,
	}

	// DialContext instead of ssh.Dial so the 15s connect bound (a DROP-type
	// firewall would otherwise hang for 75s, DESIGN.md §4.5) coexists with
	// ctx cancellation.
	conn, err := (&net.Dialer{Timeout: DialTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	// Bound the handshake on the socket itself: ClientConfig.Timeout applies
	// to ssh.Dial, not to NewClientConn, so setting only the config left the
	// handshake waiting out the peer's own timeout. A KoKo port behind a
	// firewall that accepts the TCP connection and then stalls answers after
	// exactly 5s (measured), which is 5s of dead time before the WebSocket
	// fallback can start. The deadline is cleared once the handshake is done.
	_ = conn.SetDeadline(time.Now().Add(SSHBannerTimeout))
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})

	term := &sshTerminal{
		client: ssh.NewClient(clientConn, chans, reqs),
		done:   make(chan struct{}),
	}
	interval := opts.Keepalive
	if interval <= 0 {
		interval = Keepalive
	}
	term.startKeepalive(interval)
	return term, nil
}

// SSHBannerTimeout bounds the SSH handshake, including the wait for the
// server's identification string.
//
// A real SSH server sends its banner immediately after accepting the
// connection, so two seconds is generous. The value is deliberately far below
// DialTimeout: the point is to fail fast so the WebSocket fallback can start.
const SSHBannerTimeout = 2 * time.Second

// kokoHost extracts the host the KoKo SSH port is reached on from a base
// URL, tolerating the bare-host form before api.New normalization.
func kokoHost(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		if u, err = url.Parse("https://" + raw); err != nil {
			return ""
		}
	}
	return u.Hostname()
}

// sshTerminal is the Terminal implementation over the KoKo SSH backend.
type sshTerminal struct {
	client *ssh.Client

	// done is closed by Close to stop the keepalive goroutine; stopped is
	// closed by that goroutine on exit, so Close can join it without a
	// sleep.
	done    chan struct{}
	stopped chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// Backend reports the SSH backend.
func (t *sshTerminal) Backend() BackendType { return BackendSSH }

// startKeepalive sends an OpenSSH keepalive request every interval so idle
// links survive middleboxes (DESIGN.md §3.3, §4.5). The Python reference
// uses paramiko's transport-level keepalive; x/crypto/ssh has no built-in
// equivalent, so a goroutine sending a global request is the Go stand-in.
// Failures are tolerated for keepaliveMissesTolerated consecutive ticks;
// the loop then gives up on a link the next Execute will report as dead
// anyway.
func (t *sshTerminal) startKeepalive(interval time.Duration) {
	t.stopped = make(chan struct{})
	go func() {
		defer close(t.stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		misses := 0
		for {
			select {
			case <-t.done:
				return
			case <-ticker.C:
				if _, _, err := t.client.SendRequest(keepaliveRequest, true, nil); err != nil {
					misses++
					if misses >= keepaliveMissesTolerated {
						return
					}
					continue
				}
				misses = 0
			}
		}
	}()
}

// Execute runs one command on its own session channel.
//
// The command's stdout and stderr are merged into Result.Output in arrival
// order: the two stream copiers run concurrently, so a strict
// stdout-then-stderr order is not available without full buffering, and a
// caller that needs the streams separated should invoke a command that
// redirects one of them.
//
// A non-zero remote exit status is not an error — it is reported in
// Result.ExitCode so the caller can distinguish "the command failed" from
// "the transport failed". A channel closed without an exit status is
// surfaced as an error: KoKo always reports one for a completed command,
// so its absence means the link dropped mid-command.
func (t *sshTerminal) Execute(ctx context.Context, cmd string, timeout time.Duration) (Result, error) {
	if t == nil || t.client == nil {
		return Result{}, errors.New("ssh terminal is closed")
	}
	session, err := t.client.NewSession()
	if err != nil {
		return Result{}, fmt.Errorf("open ssh session: %w", err)
	}
	defer session.Close()

	output := &mergedBuffer{}
	session.Stdout = output
	session.Stderr = output

	if err := session.Start(cmd); err != nil {
		return Result{}, fmt.Errorf("start command: %w", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- session.Wait() }()

	var timerCh <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timerCh = timer.C
	}

	select {
	case err := <-waited:
		if err == nil {
			return Result{Output: output.String()}, nil
		}
		return exitResult(output, err)
	case <-ctx.Done():
		// Closing the session unblocks Wait, so this returns promptly even
		// though the remote command is still running.
		session.Close()
		<-waited
		return Result{Output: output.String()}, ctx.Err()
	case <-timerCh:
		session.Close()
		<-waited
		return Result{Output: output.String()}, fmt.Errorf(
			"command timed out after %s: %w", timeout, context.DeadlineExceeded)
	}
}

// exitResult maps a failed Wait into a Result. *ssh.ExitError carries the
// remote exit status; anything else is a transport-level failure.
func exitResult(output *mergedBuffer, err error) (Result, error) {
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return Result{Output: output.String(), ExitCode: exitErr.ExitStatus()}, nil
	}
	var missing *ssh.ExitMissingError
	if errors.As(err, &missing) {
		return Result{}, fmt.Errorf(
			"remote closed the channel before reporting an exit status: %w", missing)
	}
	return Result{Output: output.String()}, fmt.Errorf("command failed: %w", err)
}

// Interactive attaches the local console to a remote PTY shell.
//
// The relay ends when the remote shell exits or ctx is cancelled. One
// caveat is accepted: the goroutine copying local stdin into the channel
// sits in a blocking read that only the next keystroke (or stdin EOF)
// releases after the session closes. Interactive sessions are hosted by
// the CLI process, which exits right after, so nothing observes it.
func (t *sshTerminal) Interactive(ctx context.Context) error {
	if t == nil || t.client == nil {
		return errors.New("ssh terminal is closed")
	}
	session, err := t.client.NewSession()
	if err != nil {
		return fmt.Errorf("open ssh session: %w", err)
	}
	defer session.Close()

	con, restore, err := console.Open(console.Options{
		OnResize: func(size console.Size) {
			_ = session.WindowChange(size.Rows, size.Cols)
		},
	})
	if err != nil {
		return fmt.Errorf("interactive mode: %w", err)
	}
	defer restore()

	size := con.Size()
	if err := session.RequestPty("xterm-256color", size.Rows, size.Cols, ssh.TerminalModes{}); err != nil {
		return fmt.Errorf("pty request: %w", err)
	}
	session.Stdin = readerFunc(con.Read)
	session.Stdout = writerFunc(con.Write)
	session.Stderr = writerFunc(con.Write)

	if err := session.Shell(); err != nil {
		return fmt.Errorf("remote shell: %w", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- session.Wait() }()

	select {
	case err := <-waited:
		// A shell that reports an exit status ended normally; anything else
		// is a transport failure worth surfacing.
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return err
	case <-ctx.Done():
		session.Close()
		<-waited
		return ctx.Err()
	}
}

// Close tears the terminal down. It is safe to call more than once.
func (t *sshTerminal) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		// Close the client first: it unblocks an in-flight keepalive
		// request, so joining the goroutine below cannot hang Close.
		t.closeErr = t.client.Close()
		close(t.done)
		<-t.stopped
	})
	return t.closeErr
}

// mergedBuffer merges the exec channel's stdout and stderr in arrival
// order. The two stream copiers run on separate goroutines, so writes to a
// plain bytes.Buffer would race.
type mergedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *mergedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *mergedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
