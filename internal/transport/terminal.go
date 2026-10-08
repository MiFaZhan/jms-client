// Package transport opens KoKo terminal sessions against a resolved asset.
//
// Two backends exist and both authenticate with a connection token rather
// than the user's password (DESIGN.md「KoKo 协议」的「认证」):
//
//   - SSH: connect to the KoKo port (default 2222) as JMS-{token_id} with
//     token_value as the password. Each command runs on its own channel, so
//     it gets a native exit code and no marker parsing.
//   - WebSocket: /koko/ws/terminal/ with the jms_sessionid cookie and the
//     JMS-KOKO subprotocol. The shell is a shared PTY, so output is
//     delimited by the __JMSDONE__ / __JMSRC__ marker protocol.
//
// BackendAuto tries SSH first and falls back to WebSocket, because an
// external endpoint often permits the WebSocket path while the KoKo SSH
// port is blocked (DESIGN.md「端点故障转移」第 4 点).
package transport

import (
	"context"
	"errors"
	"time"

	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
)

// BackendType selects a transport backend.
type BackendType string

// Backend kinds. BackendAuto is the default and tries SSH then WebSocket.
const (
	BackendSSH  BackendType = "ssh"
	BackendWS   BackendType = "ws"
	BackendAuto BackendType = "auto"
)

// Numeric defaults from DESIGN.md「数值基线」
const (
	// DefaultSSHPort is the KoKo SSH port.
	DefaultSSHPort = 2222
	// DialTimeout bounds the TCP connect. A DROP-style firewall would
	// otherwise hang for 75s.
	DialTimeout = 15 * time.Second
	// Keepalive is the SSH keepalive interval and the WebSocket
	// application-level ping interval.
	Keepalive = 30 * time.Second
	// DefaultCols and DefaultRows size the PTY.
	DefaultCols = 200
	DefaultRows = 50
)

// ErrUnsupported reports a backend name that is not one of the three above.
var ErrUnsupported = errors.New("unsupported backend")

// Result is the outcome of one non-interactive command.
type Result struct {
	Output   string
	ExitCode int
}

// ConnectOptions tunes one connection attempt.
type ConnectOptions struct {
	// Backend selects SSH, WebSocket, or auto. The empty value means auto.
	Backend BackendType
	// SSHPort is the KoKo SSH port; 0 means DefaultSSHPort.
	SSHPort int
	// Cols and Rows size the PTY; 0 means the defaults.
	Cols, Rows int
	// Keepalive overrides the keepalive interval; 0 means Keepalive.
	Keepalive time.Duration
	// Protocol is the connection-token protocol ("ssh" or "sftp"); the
	// empty value means "ssh".
	Protocol string
	// ConnectMethod is the token's connect_method; the empty value means
	// "web_cli", which SFTP must override with "web_sftp".
	ConnectMethod string
}

// Terminal is one connected KoKo session.
//
// Execute is serialized by the caller (connpool holds a per-terminal lock):
// a shared PTY cannot interleave two commands, and even the SSH backend's
// per-channel execution is ordered to keep output attribution simple.
type Terminal interface {
	// Execute runs one command and returns its output and exit code.
	//
	// A non-zero remote exit is NOT an error: it is reported in
	// Result.ExitCode so the caller can distinguish "the command failed"
	// from "the transport failed".
	Execute(ctx context.Context, cmd string, timeout time.Duration) (Result, error)
	// Backend reports which backend is live.
	Backend() BackendType
	// Interactive attaches the local console to the remote shell. It
	// returns when the remote shell exits.
	Interactive(ctx context.Context) error
	// Close tears the terminal down. It is safe to call more than once.
	Close() error
}

// Connect opens a terminal for asset on sess, using the backend selected by
// opts.
//
// The endpoint is already bound to sess (DESIGN.md「端点故障转移」第 1 点): the KoKo
// address is derived from sess.BaseURL and opts.SSHPort, never re-resolved
// here.
func Connect(ctx context.Context, sess *auth.Session, asset assets.Info, opts ConnectOptions) (Terminal, error) {
	return connect(ctx, sess, asset, opts)
}

// NewConnectionToken creates a connection token for asset.
//
// It is exported because SFTP opens its own token with a different
// protocol/connect_method pair, and because tests need to observe the
// request shape.
func NewConnectionToken(ctx context.Context, sess *auth.Session, asset assets.Info, protocol, connectMethod string) (Token, error) {
	return newConnectionToken(ctx, sess, asset, protocol, connectMethod)
}

// Token is a connection token as returned by
// POST /api/v1/authentication/connection-token/.
type Token struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}
