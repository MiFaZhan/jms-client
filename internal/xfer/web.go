// Web-channel transfer: the file-manager WebSocket alternative to SFTP.
//
// A deployment that exposes only the web port cannot reach the KoKo SSH
// port the SFTP engine rides on. The web channel (internal/webxfer)
// speaks the browser file manager's protocol over the same web URL the
// session already uses, so a transfer still has a path — at the cost of
// the protocol's limits documented in the webxfer package.
package xfer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MiFaZhan/jms-client/internal/api"
	"github.com/MiFaZhan/jms-client/internal/assets"
	"github.com/MiFaZhan/jms-client/internal/auth"
	"github.com/MiFaZhan/jms-client/internal/webxfer"
)

// runWeb performs one task over the web channel.
func (e *Engine) runWeb(ctx context.Context, t Task, asset assets.Info) (Result, error) {
	if e.web != nil {
		return e.web(ctx, e.Session, asset, t)
	}
	return webTransfer(ctx, e.Session, asset, t)
}

// webTransfer dials the file-manager WebSocket and moves the file.
//
// Differences from the SFTP path, all forced by the protocol:
//   - the remote destination of an upload must name the file itself
//     (no cp-style directory expansion: the channel cannot stat it);
//   - the remote parent directory must already exist (no mkdir);
//   - the remote size of a download is not announced, so Result.Bytes is
//     the count actually received.
func webTransfer(ctx context.Context, sess *auth.Session, asset assets.Info, t Task) (Result, error) {
	start := time.Now()
	res := Result{Transport: TransportWeb}

	c, err := webxfer.Dial(ctx, sess, asset)
	if err != nil {
		return res, err
	}
	defer func() { _ = c.Close() }()

	localPath, remotePath := t.Local, t.Remote
	switch t.Direction {
	case Upload:
		// A caller-tuned chunk may exceed the protocol maximum (the SFTP
		// side has no such ceiling); clamp rather than fail the transfer.
		chunk := int(chunkSize(t))
		if chunk > webxfer.MaxChunk {
			chunk = webxfer.MaxChunk
		}
		n, err := c.Upload(ctx, t.Local, t.Remote, chunk)
		res.Bytes = n
		res.Duration = time.Since(start)
		if err != nil {
			return res, ctxOr(err, ctx)
		}
	case Download:
		// The local side is stat-able, so cp-style expansion of an
		// existing destination directory still applies here.
		dst, err := expandDestination(localSide{}, t.Remote, t.Local)
		if err != nil {
			return res, err
		}
		n, err := c.Download(ctx, t.Remote, dst)
		res.Bytes = n
		res.Duration = time.Since(start)
		localPath = dst
		if err != nil {
			return res, ctxOr(err, ctx)
		}
	default:
		return res, fmt.Errorf("xfer: unknown direction %q", t.Direction)
	}

	if !t.Verify {
		return res, nil
	}
	res.MD5Local, res.MD5Remote, err = digests(ctx, &Engine{Session: sess, Asset: asset}, localPath, remotePath)
	res.Duration = time.Since(start)
	if err != nil {
		return res, err
	}
	if res.MD5Local == "" || res.MD5Local != res.MD5Remote {
		return res, fmt.Errorf("%w: local %s, remote %s",
			ErrVerifyFailed, digestOrUnknown(res.MD5Local), digestOrUnknown(res.MD5Remote))
	}
	res.Verified = true
	return res, nil
}

// webFallbackEligible reports whether an SFTP dial failure is worth
// retrying over the web channel.
//
// Network-class failures (dial refused, timeout, handshake) are: they are
// the signature of a web-port-only deployment. A per-dial timeout counts,
// even though net's *timeoutError satisfies errors.Is with
// context.DeadlineExceeded: the deadline that fired was the dialer's own
// (transport.DialTimeout), not the caller's. Whether the caller's context
// still has time left is the call site's ctx.Err() guard, so this
// function only rejects a cancelled context — which no retry can fix —
// and authentication failures, which the web channel would hit too: it
// authenticates the same user against the same permissions.
func webFallbackEligible(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		return false
	}
	return true
}
