package xfer

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
)

// webRecording captures how the web seam was used by one Run.
type webRecording struct {
	called int
	result Result
	err    error
}

// newFallbackEnv returns an engine wired for fallback tests: the SFTP dial
// seam fails with dialErr, the web seam records its calls and answers
// res/werr.
func newFallbackEnv(t *testing.T, dialErr error, res Result, werr error) (*Engine, *webRecording) {
	t.Helper()
	rec := &webRecording{result: res, err: werr}
	e := newTestEnv(t).engine
	e.withOpen(func(context.Context, *Engine, assets.Info) (remoteFS, error) {
		return nil, dialErr
	})
	e.web = func(context.Context, *auth.Session, assets.Info, Task) (Result, error) {
		rec.called++
		return rec.result, rec.err
	}
	return e, rec
}

// networkDialError is the shape a web-port-only deployment produces: the
// TCP dial to the KoKo SSH port is refused.
func networkDialError() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

func fallbackTask() Task {
	return Task{Direction: Upload, Local: "src", Remote: "/tmp/dst"}
}

func TestAutoFallsBackToWebWhenSFTPDialFails(t *testing.T) {
	e, rec := newFallbackEnv(t, networkDialError(), Result{Bytes: 42, Transport: TransportWeb}, nil)
	res, err := e.Run(context.Background(), fallbackTask())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.called != 1 {
		t.Fatalf("web seam called %d times, want 1", rec.called)
	}
	if res.Transport != TransportWeb || res.Bytes != 42 {
		t.Fatalf("result = %+v, want the web seam's result", res)
	}
}

func TestForcedWebSkipsTheSFTPDialEntirely(t *testing.T) {
	e, rec := newFallbackEnv(t, errors.New("dial must not happen"), Result{Bytes: 7, Transport: TransportWeb}, nil)
	task := fallbackTask()
	task.Transport = TransportWeb
	res, err := e.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.called != 1 {
		t.Fatalf("web seam called %d times, want 1", rec.called)
	}
	if res.Transport != TransportWeb {
		t.Fatalf("transport = %q, want web", res.Transport)
	}
}

func TestForcedSSHDoesNotFallBack(t *testing.T) {
	e, rec := newFallbackEnv(t, networkDialError(), Result{Bytes: 42, Transport: TransportWeb}, nil)
	task := fallbackTask()
	task.Transport = TransportSSH
	_, err := e.Run(context.Background(), task)
	if err == nil {
		t.Fatal("forced ssh succeeded despite the dial failure")
	}
	if rec.called != 0 {
		t.Fatalf("web seam called %d times on a forced-ssh task", rec.called)
	}
}

func TestAuthFailureDoesNotFallBack(t *testing.T) {
	dialErr := &api.AuthError{StatusCode: 403, Body: "permission denied"}
	e, rec := newFallbackEnv(t, dialErr, Result{Bytes: 42, Transport: TransportWeb}, nil)
	_, err := e.Run(context.Background(), fallbackTask())
	if err == nil {
		t.Fatal("run succeeded despite an auth failure")
	}
	if rec.called != 0 {
		t.Fatalf("web seam called %d times after an auth failure", rec.called)
	}
}

func TestBothChannelsFailReportsBothErrors(t *testing.T) {
	webErr := errors.New("web dial failed")
	e, _ := newFallbackEnv(t, networkDialError(), Result{}, webErr)
	_, err := e.Run(context.Background(), fallbackTask())
	if err == nil {
		t.Fatal("run succeeded despite both channels failing")
	}
	if !errors.Is(err, webErr) {
		t.Fatalf("error %v does not wrap the web error", err)
	}
}

func TestRelayRejectsTheWebTransport(t *testing.T) {
	src := newTestEnv(t).engine
	dst := secondEnv(t).engine
	_, err := Relay(context.Background(), src, dst, "/a", "/b", Task{Transport: TransportWeb})
	if err == nil || !strings.Contains(err.Error(), "relay over the web channel") {
		t.Fatalf("relay error = %v, want the web-transport rejection", err)
	}
}

func TestParseTransportVocabulary(t *testing.T) {
	cases := []struct {
		raw  string
		want Transport
		ok   bool
	}{
		{"", TransportAuto, true},
		{"auto", TransportAuto, true},
		{"ssh", TransportSSH, true},
		{"sftp", TransportSSH, true},
		{"web", TransportWeb, true},
		{"WEB", TransportWeb, true},
		{"carrier-pigeon", "", false},
	}
	for _, tc := range cases {
		got, err := ParseTransport(tc.raw)
		if tc.ok && err != nil {
			t.Fatalf("ParseTransport(%q): %v", tc.raw, err)
		}
		if !tc.ok {
			if err == nil {
				t.Fatalf("ParseTransport(%q) accepted an invalid value", tc.raw)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("ParseTransport(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
